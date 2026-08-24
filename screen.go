package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

// handleScreenShare handles WebSocket connections for screen sharing.
// Two roles via query param ?role=publisher|viewer:
//   - "publisher": sends video frames which are broadcast to that publisher's viewers
//   - "viewer":    receives one specific publisher's screen stream
//
// Multiple publishers may share in the same voice channel at once (Discord-style).
// A viewer chooses which publisher to watch via the ?target=<publisher_userid>
// query param. Query params: room, userid, token, role, target.
func handleScreenShare(rm *RoomManager, w http.ResponseWriter, r *http.Request) {
	// --- Parse query params ---
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		log.Println("[screen] Missing room name")
		http.Error(w, "Missing room name", http.StatusBadRequest)
		return
	}

	userId := r.URL.Query().Get("userid")
	if userId == "" {
		log.Println("[screen] Missing userid")
		http.Error(w, "Missing userid", http.StatusBadRequest)
		return
	}

	token := r.URL.Query().Get("token")
	if token == "" {
		log.Println("[screen] Missing room token")
		http.Error(w, "Missing room token", http.StatusUnauthorized)
		return
	}

	role := r.URL.Query().Get("role")
	if role != "publisher" && role != "viewer" {
		log.Printf("[screen] Invalid role '%s' — must be 'publisher' or 'viewer'\n", role)
		http.Error(w, "Invalid role. Use ?role=publisher or ?role=viewer", http.StatusBadRequest)
		return
	}

	// --- Validate room access ---
	room := rm.GetRoom(roomID, token)
	if room == nil {
		http.Error(w, "Room not found or invalid token", http.StatusNotFound)
		return
	}

	// --- Upgrade to WebSocket ---
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[screen] Upgrade error:", err)
		return
	}
	defer func() { _ = conn.Close() }()

	// Keep the connection alive with periodic pings. Cloudflare (and other
	// proxies) drop idle WebSocket connections after ~100 seconds, so a 60s
	// ping prevents silent disconnects. WriteControl is used internally and is
	// safe to run concurrently with the read loop below.
	stopPing := startPingLoop(conn, pingInterval)
	defer stopPing()

	// Set a generous read limit for video frames (up to ~5MB)
	conn.SetReadLimit(5 << 20) // 5 MB

	if role == "publisher" {
		handleScreenPublisher(rm, room, conn, userId, roomID)
	} else {
		handleScreenViewer(rm, room, conn, userId, roomID, r)
	}
}

// handleScreenPublisher reads video frames from a publisher and broadcasts them
// to that publisher's viewers. Any number of publishers may share at once, each
// with its own relay / SFU / viewer set.
func handleScreenPublisher(rm *RoomManager, room *Room, conn *websocket.Conn, userId, roomID string) {
	pub := room.addScreenPublisher(conn, userId)
	if pub == nil {
		// Rejected by the per-room screen-share limit. Send a JSON error so the
		// frontend can surface it, then close the connection.
		resp, _ := json.Marshal(map[string]string{
			"error": fmt.Sprintf("Screen share limit reached (%d concurrent shares max)", maxScreenSharesPerRoom),
		})
		_ = conn.WriteMessage(websocket.TextMessage, resp)
		return
	}

	// Notify everyone else in the voice channel that a screen share started.
	room.BroadcastScreenShareState("screen_share_started", userId)

	// Cleanup on disconnect: clear this publisher + close all of its viewers.
	defer func() {
		viewers := room.ClearScreenPublisher(pub)
		room.BroadcastScreenShareState("screen_share_stopped", userId)
		for _, vConn := range viewers {
			_ = vConn.Close()
		}
		rm.resetIdleIfEmpty(roomID)
	}()

	// Read loop: broadcast binary video frames (MSE mode) and route WebRTC
	// signaling (offer/ICE) to the targeted viewer or this publisher's SFU.
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[screen] Publisher [%s] disconnected from room [%s]: %v\n", userId, roomID, err)
			break
		}

		if messageType == websocket.BinaryMessage {
			room.BroadcastScreen(pub, message)
			continue
		}

		if messageType == websocket.TextMessage {
			var ctrl struct {
				Type         string                  `json:"type"`
				Mode         string                  `json:"mode"`
				TargetUserID string                  `json:"target_userid"`
				SDP          string                  `json:"sdp"`
				Candidate    webrtc.ICECandidateInit `json:"candidate"`
			}
			if err := json.Unmarshal(message, &ctrl); err != nil {
				continue
			}
			switch ctrl.Type {
			case "hello":
				if ctrl.Mode != "" {
					room.setScreenPublisherMode(pub, ctrl.Mode)
					room.broadcastModeToViewers(pub, ctrl.Mode)
					if ctrl.Mode == "sfu" {
						room.ensureScreenSFU(pub)
						room.setupAllSFUViewers(pub)
					}
				}
			case "offer":
				if room.screenPublisherMode(pub) == "sfu" {
					log.Printf("[screen] publisher [%s] sent SFU offer in room [%s]", userId, roomID)
					room.ensureScreenSFU(pub)
					if sfu := room.getScreenSFU(pub); sfu != nil {
						sfu.HandlePublisherOffer(ctrl.SDP)
					}
				} else if ctrl.TargetUserID != "" {
					room.sendToViewerByUserID(pub, ctrl.TargetUserID, message)
				}
			case "ice":
				if room.screenPublisherMode(pub) == "sfu" {
					if sfu := room.getScreenSFU(pub); sfu != nil {
						sfu.HandlePublisherICE(ctrl.Candidate)
					}
				} else if ctrl.TargetUserID != "" {
					room.sendToViewerByUserID(pub, ctrl.TargetUserID, message)
				}
			}
			continue
		}
	}
}

// handleScreenViewer connects a viewer to a specific publisher (selected by the
// ?target= param) and relays that publisher's stream + signaling to it.
func handleScreenViewer(rm *RoomManager, room *Room, conn *websocket.Conn, userId, roomID string, r *http.Request) {
	targetUserID := r.URL.Query().Get("target")
	if targetUserID == "" {
		log.Printf("[screen] Viewer [%s] in room [%s] did not specify a target publisher\n", userId, roomID)
		resp, _ := json.Marshal(map[string]string{"error": "No target publisher specified"})
		_ = conn.WriteMessage(websocket.TextMessage, resp)
		return
	}

	pub := room.getScreenPublisherByUserID(targetUserID)
	if pub == nil {
		log.Printf("[screen] Viewer [%s] in room [%s] targeted unknown publisher [%s]\n", userId, roomID, targetUserID)
		resp, _ := json.Marshal(map[string]string{"error": "Target publisher not found"})
		_ = conn.WriteMessage(websocket.TextMessage, resp)
		return
	}

	// Atomically register the viewer with that publisher and get its init segment.
	initSeg := room.RegisterScreenViewer(pub, conn, userId)

	// Cleanup on disconnect
	defer func() {
		room.RemoveScreenViewer(pub, conn)
		rm.resetIdleIfEmpty(roomID)
	}()

	mode := room.screenPublisherMode(pub)
	log.Printf("[screen] viewer [%s] joined room [%s] watching publisher [%s] mode=%s", userId, roomID, targetUserID, mode)
	room.NotifyViewerMode(conn, mode)

	if mode == "sfu" {
		// SFU: the server relays this publisher's stream to this viewer via Pion.
		room.addSFUViewer(pub, conn, userId)
	} else if mode == "webrtc" {
		// WebRTC mesh: the publisher creates one RTCPeerConnection per viewer.
		pub.notifyViewerJoined(userId)
	} else if initSeg != nil {
		// MSE fallback: serve the cached init first. Serialized with
		// BroadcastScreen via room.screenWriteMu, and the init-pending gate
		// guarantees no media reaches this viewer before MarkScreenInitDelivered.
		log.Printf("screenshare init served: room=%s userid=%s bytes=%d", roomID, userId, len(initSeg))
		room.screenWriteMu.Lock()
		err := conn.WriteMessage(websocket.BinaryMessage, initSeg)
		room.screenWriteMu.Unlock()
		if err != nil {
			log.Printf("[screen] Error sending init segment to viewer [%s]: %v\n", userId, err)
			return
		}
		room.MarkScreenInitDelivered(pub, conn)
	}

	// Block on reads to detect disconnects. The viewer also sends WebRTC
	// signaling (answer/ICE) which we forward to its publisher, and an MSE
	// {"type":"request_keyframe"} control message after a MediaSource rebuild.
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[screen] Viewer [%s] disconnected from room [%s]: %v\n", userId, roomID, err)
			break
		}
		if messageType == websocket.TextMessage {
			var ctrl struct {
				Type      string                  `json:"type"`
				SDP       string                  `json:"sdp"`
				Candidate webrtc.ICECandidateInit `json:"candidate"`
			}
			if json.Unmarshal(message, &ctrl) != nil {
				continue
			}
			switch ctrl.Type {
			case "hello":
				// Viewer capability announcement — nothing to do here.
			case "answer":
				if room.screenPublisherMode(pub) == "sfu" {
					if sfu := room.getScreenSFU(pub); sfu != nil {
						sfu.HandleViewerAnswer(conn, ctrl.SDP)
					}
				} else {
					pub.forward(userId, message)
				}
			case "ice":
				if room.screenPublisherMode(pub) == "sfu" {
					if sfu := room.getScreenSFU(pub); sfu != nil {
						sfu.HandleViewerICE(conn, ctrl.Candidate)
					}
				} else {
					pub.forward(userId, message)
				}
			case "request_keyframe":
				room.SendScreenKeyframe(pub, conn)
			}
		}
	}
}
