package main

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

// handleScreenShare handles WebSocket connections for screen sharing.
// Two roles via query param ?role=publisher|viewer:
//   - "publisher": sends video frames which are broadcast to all viewers
//   - "viewer":   receives the screen stream (one-to-many from publisher)
//
// Query params: room, userid, token, role
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
	defer conn.Close()

	// Set a generous read limit for video frames (up to ~5MB)
	conn.SetReadLimit(5 << 20) // 5 MB

	switch role {
	case "publisher":
		handleScreenPublisher(rm, room, conn, userId, roomID)
	case "viewer":
		handleScreenViewer(rm, room, conn, userId, roomID)
	}
}

// handleScreenPublisher reads video frames from the publisher and broadcasts to all viewers.
func handleScreenPublisher(rm *RoomManager, room *Room, conn *websocket.Conn, userId, roomID string) {
	// Try to become the publisher; reject if one already exists
	if !room.SetScreenPublisher(conn, userId) {
		log.Printf("[screen] Room [%s] already has a publisher\n", roomID)
		conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"Room already has an active publisher"}`))
		return
	}

	// Cleanup on disconnect: clear publisher + close all viewers
	defer func() {
		viewers := room.ClearScreenPublisher(conn)
		// Close viewer connections outside any lock
		for _, vConn := range viewers {
			vConn.Close()
		}
		// Check if room is now fully empty
		rm.resetIdleIfEmpty(roomID)
	}()

	// Read loop: broadcast binary video frames (MSE mode) and route WebRTC
	// signaling (offer/ICE) to the targeted viewer.
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[screen] Publisher [%s] disconnected from room [%s]: %v\n", userId, roomID, err)
			break
		}

		if messageType == websocket.BinaryMessage {
			room.BroadcastScreen(conn, message)
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
					room.SetScreenPublisherMode(ctrl.Mode)
					room.BroadcastModeToViewers(ctrl.Mode)
					if ctrl.Mode == "sfu" {
						room.EnsureScreenSFU()
						room.SetupAllSFUViewers()
					}
				}
			case "offer":
				if room.ScreenPublisherMode() == "sfu" {
					log.Printf("[screen] publisher sent SFU offer in room [%s]", roomID)
					room.EnsureScreenSFU()
					room.HandleScreenSFUOffer(ctrl.SDP)
				} else if ctrl.TargetUserID != "" {
					room.SendToScreenViewerByUserID(ctrl.TargetUserID, message)
				}
			case "ice":
				if room.ScreenPublisherMode() == "sfu" {
					room.HandleScreenSFUPublisherICE(ctrl.Candidate)
				} else if ctrl.TargetUserID != "" {
					room.SendToScreenViewerByUserID(ctrl.TargetUserID, message)
				}
			}
			continue
		}
	}
}

// handleScreenViewer keeps the connection alive and receives screen frames pushed by the publisher.
// The read loop serves only as disconnect detection.
func handleScreenViewer(rm *RoomManager, room *Room, conn *websocket.Conn, userId, roomID string) {
	// Atomically register the viewer and obtain the init segment to send first.
	// Registration marks the viewer as "init pending", so BroadcastScreen will
	// not relay any media to it until we deliver the init below.
	initSeg := room.RegisterScreenViewer(conn, userId)

	// Cleanup on disconnect
	defer func() {
		room.RemoveScreenViewer(conn)
		rm.resetIdleIfEmpty(roomID)
	}()

	// Determine the publisher's signaling mode and tell both sides what to do.
	mode := room.ScreenPublisherMode()
	log.Printf("[screen] viewer joined room [%s], publisher mode=%s", roomID, mode)
	room.NotifyViewerMode(conn, mode)

	if mode == "sfu" {
		// SFU: the server relays the publisher's single upstream stream to this
		// viewer via Pion. The SFU sends an offer here; media flows
		// server -> viewer, so the publisher upload stays at 1x.
		room.AddScreenSFUViewer(conn, userId)
	} else if mode == "webrtc" {
		// WebRTC mesh: the publisher creates one RTCPeerConnection per viewer
		// and sends an offer through us. Media then flows peer-to-peer (no
		// relay), which removes the MediaRecorder/MSE latency entirely.
		room.NotifyViewerJoined(userId)
	} else if initSeg != nil {
		// MSE fallback: serve the cached init first (existing behavior). The
		// write is serialized with BroadcastScreen via room.screenWriteMu, and
		// the init-pending gate guarantees no media frame reaches this viewer
		// before MarkScreenInitDelivered clears it.
		log.Printf("screenshare init served: room=%s userid=%s bytes=%d", roomID, userId, len(initSeg))

		room.screenWriteMu.Lock()
		err := conn.WriteMessage(websocket.BinaryMessage, initSeg)
		room.screenWriteMu.Unlock()

		if err != nil {
			log.Printf("[screen] Error sending init segment to viewer [%s]: %v\n", userId, err)
			return
		}

		room.MarkScreenInitDelivered(conn)
	}

	// Block on reads to detect disconnects. The viewer also sends WebRTC
	// signaling (answer/ICE) which we forward to the publisher, and an MSE
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
				// Viewer capability announcement — the server already told the
				// viewer the publisher's mode; nothing to do here.
			case "answer":
				if room.ScreenPublisherMode() == "sfu" {
					room.HandleScreenSFUViewerAnswer(conn, ctrl.SDP)
				} else {
					room.ForwardToScreenPublisher(userId, message)
				}
			case "ice":
				if room.ScreenPublisherMode() == "sfu" {
					room.HandleScreenSFUViewerICE(conn, ctrl.Candidate)
				} else {
					room.ForwardToScreenPublisher(userId, message)
				}
			case "request_keyframe":
				room.SendScreenKeyframe(conn)
			}
		}
	}
}
