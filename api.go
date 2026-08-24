package main

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

// maxAudioReadBytes bounds the size of a single message the server will accept
// on the audio WebSocket. Without an explicit limit gorilla allows unbounded
// frames, so a client could send a huge frame and force the server to allocate
// it all in memory before the relay's safety valve ever sees it.
const maxAudioReadBytes = 1 << 20

type CreateRoomRequest struct {
	RoomId string `json:"roomId"`
	Token  string `json:"token"`
}

type UpdateRoomTokenRequest struct {
	RoomId string `json:"roomId"`
	Token  string `json:"token"`
}

type RemoveRoomRequest struct {
	RoomId string `json:"roomId"`
}

// writeJSONError sends a JSON-formatted error response.
// All REST API errors use this for consistent Content-Type headers.
func writeJSONError(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func (rm *RoomManager) handleCreateRoomAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check token (Server should send auth)
	clientToken := r.Header.Get("X-API-Token")
	if !rm.isAuthorized(clientToken) {
		writeJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Decode JSON
	var req CreateRoomRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		writeJSONError(w, "Incorrect JSON", http.StatusBadRequest)
		return
	}

	// Check if JSON has RoomId
	if req.RoomId == "" {
		writeJSONError(w, "Missing roomId", http.StatusBadRequest)
		return
	}

	room := rm.CreateRoom(req.RoomId, req.Token)
	if room == nil {
		// CreateRoom returns nil when the room already exists with a different
		// token, meaning the caller did NOT actually create/claim it. Surface
		// that instead of falsely reporting "201 created".
		writeJSONError(w, "Room already exists with a different token", http.StatusConflict)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)

	jeer := json.NewEncoder(w).Encode(map[string]interface{}{
		"created": true,
		"roomId":  req.RoomId,
	})
	if jeer != nil {
		return
	}
}

func (rm *RoomManager) handleUpdateRoomTokenAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check auth
	clientToken := r.Header.Get("X-API-Token")
	if !rm.isAuthorized(clientToken) {
		writeJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Decode JSON
	var req UpdateRoomTokenRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		writeJSONError(w, "Incorrect JSON", http.StatusBadRequest)
		return
	}

	// Validate fields
	if req.RoomId == "" {
		writeJSONError(w, "Missing roomId", http.StatusBadRequest)
		return
	}
	if req.Token == "" {
		writeJSONError(w, "Missing token", http.StatusBadRequest)
		return
	}

	// Attempt to update the token
	updated := rm.UpdateRoomToken(req.RoomId, req.Token)
	if !updated {
		writeJSONError(w, "Room not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	jeer := json.NewEncoder(w).Encode(map[string]interface{}{
		"updated": true,
		"roomId":  req.RoomId,
	})
	if jeer != nil {
		return
	}
}

func (rm *RoomManager) handleRemoveRoomAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check auth
	clientToken := r.Header.Get("X-API-Token")
	if !rm.isAuthorized(clientToken) {
		writeJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Decode JSON
	var req RemoveRoomRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		writeJSONError(w, "Incorrect JSON", http.StatusBadRequest)
		return
	}

	// Validate fields
	if req.RoomId == "" {
		writeJSONError(w, "Missing roomId", http.StatusBadRequest)
		return
	}

	// Attempt to remove the room
	removed := rm.RemoveRoom(req.RoomId)
	if !removed {
		writeJSONError(w, "Room not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	jeer := json.NewEncoder(w).Encode(map[string]interface{}{
		"removed": true,
		"roomId":  req.RoomId,
	})
	if jeer != nil {
		return
	}
}

func (rm *RoomManager) handleCheckRoomAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check token (Server should send auth)
	clientToken := r.Header.Get("X-API-Token")
	if !rm.isAuthorized(clientToken) {
		writeJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		writeJSONError(w, "Missing room name", http.StatusBadRequest)
		return
	}

	exists := rm.DoesRoomExist(roomID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	err := json.NewEncoder(w).Encode(map[string]bool{"exists": exists})
	if err != nil {
		return
	}
}

func handleAudioStream(rm *RoomManager, w http.ResponseWriter, r *http.Request) {
	// Get room name
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		log.Println("Missing room name")
		writeJSONError(w, "Missing room name", http.StatusBadRequest)
		return
	}

	// Get user id
	userId := r.URL.Query().Get("userid")
	if userId == "" {
		log.Println("Missing room userid")
		writeJSONError(w, "Missing room userid", http.StatusBadRequest)
		return
	}

	// Get room token (password)
	token := r.URL.Query().Get("token")
	if token == "" {
		log.Println("Missing room token")
		writeJSONError(w, "Missing room token", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}
	defer func() { _ = conn.Close() }()

	// Bound the size of a single inbound message so a malicious client can't
	// force a huge allocation (see maxAudioReadBytes).
	conn.SetReadLimit(maxAudioReadBytes)

	// Keep the connection alive with periodic pings AND detect half-dead peers
	// (no data + no pong within pongWait) so they are torn down rather than
	// leaked. See startHeartbeat.
	stopHeartbeat := startHeartbeat(conn)
	defer stopHeartbeat()

	// 1. Client joins the requested room
	room := rm.JoinRoom(roomID, token, userId, conn)

	if room == nil {
		// Upgrade already happened — send close frame with meaningful message
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Room not found or invalid token"))
		return
	}

	// If a screen share is already in progress, tell this late-joining client so
	// it can show the LIVE badge and offer a one-click watch.
	for _, pubID := range room.CurrentScreenSharePublishers() {
		room.NotifyScreenShareState(conn, "screen_share_started", pubID)
	}

	// 2. Ensure the client is removed when they disconnect
	defer rm.LeaveRoom(roomID, conn)

	// 3. Infinite loop to listen for audio chunks / WebRTC signaling
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			break
		}

		// 4. Broadcast logic delegated to the Room struct (MSE fallback path).
		if messageType == websocket.BinaryMessage {
			room.Broadcast(conn, message)
			continue
		}

		// 5. Control + WebRTC signaling messages (JSON text).
		//
		//    MSE fallback: {"type":"request_keyframe","target_user_id":"N"}
		//    when a remote player's SourceBuffer fails and is rebuilt, asking us
		//    to re-send that user's cached init segment.
		//
		//    SFU (WebRTC): the frontend announces {"type":"hello","mode":"sfu"},
		//    sends its microphone as an upstream offer, answers downstream
		//    offers (one per other participant), and trickles ICE.
		if messageType == websocket.TextMessage {
			var ctrl struct {
				Type         string                  `json:"type"`
				Mode         string                  `json:"mode"`
				SDP          string                  `json:"sdp"`
				TargetUserID string                  `json:"target_userid"`
				Candidate    webrtc.ICECandidateInit `json:"candidate"`
			}
			if err := json.Unmarshal(message, &ctrl); err != nil {
				continue
			}
			switch ctrl.Type {
			case "hello":
				if ctrl.Mode == "sfu" {
					room.EnsureVoiceSFU()
				}
			case "offer":
				if ctrl.SDP != "" {
					room.EnsureVoiceSFU()
					room.VoiceSFUHandleOffer(conn, userId, ctrl.SDP)
				}
			case "answer":
				if ctrl.SDP != "" && ctrl.TargetUserID != "" {
					room.VoiceSFUHandleAnswer(conn, ctrl.TargetUserID, ctrl.SDP)
				}
			case "ice":
				if ctrl.TargetUserID != "" {
					room.VoiceSFUHandleSubscriberICE(conn, ctrl.TargetUserID, ctrl.Candidate)
				} else {
					room.VoiceSFUHandleUpstreamICE(conn, userId, ctrl.Candidate)
				}
			case "request_keyframe":
				if ctrl.TargetUserID != "" {
					room.SendKeyframe(conn, ctrl.TargetUserID)
				}
			}
		}
	}
}

// handleHealthCheck responds with a 200 OK to indicate the server is alive.
func handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	// We only want to allow GET requests here
	if r.Method != http.MethodGet {
		writeJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Explicitly set the 200 OK status
	w.WriteHeader(http.StatusOK)

	// Write a simple text response (or you could do JSON like `{"status":"ok"}`)
	_, _ = w.Write([]byte("OK"))
}
