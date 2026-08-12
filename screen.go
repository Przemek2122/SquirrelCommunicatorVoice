package main

import (
	"log"
	"net/http"

	"github.com/gorilla/websocket"
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
		writeJSONError(w, "Missing room name", http.StatusBadRequest)
		return
	}

	userId := r.URL.Query().Get("userid")
	if userId == "" {
		log.Println("[screen] Missing userid")
		writeJSONError(w, "Missing userid", http.StatusBadRequest)
		return
	}

	token := r.URL.Query().Get("token")
	if token == "" {
		log.Println("[screen] Missing room token")
		writeJSONError(w, "Missing room token", http.StatusUnauthorized)
		return
	}

	role := r.URL.Query().Get("role")
	if role != "publisher" && role != "viewer" {
		log.Printf("[screen] Invalid role '%s' — must be 'publisher' or 'viewer'\n", role)
		writeJSONError(w, "Invalid role. Use ?role=publisher or ?role=viewer", http.StatusBadRequest)
		return
	}

	// --- Validate room access ---
	room := rm.GetRoom(roomID, token)
	if room == nil {
		writeJSONError(w, "Room not found or invalid token", http.StatusNotFound)
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

	// Read loop: broadcast each video frame to all viewers
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[screen] Publisher [%s] disconnected from room [%s]: %v\n", userId, roomID, err)
			break
		}

		if messageType == websocket.BinaryMessage {
			room.BroadcastScreen(conn, message)
		}
	}
}

// handleScreenViewer keeps the connection alive and receives screen frames pushed by the publisher.
// The read loop serves only as disconnect detection.
func handleScreenViewer(rm *RoomManager, room *Room, conn *websocket.Conn, userId, roomID string) {
	room.AddScreenViewer(conn, userId)

	// Cleanup on disconnect
	defer func() {
		room.RemoveScreenViewer(conn)
		rm.resetIdleIfEmpty(roomID)
	}()

	// Send cached init segment so the viewer can decode the ongoing stream immediately
	if initSeg := room.GetScreenInitSegment(); initSeg != nil {
		if err := conn.WriteMessage(websocket.BinaryMessage, initSeg); err != nil {
			log.Printf("[screen] Error sending init segment to viewer [%s]: %v\n", userId, err)
			return
		}
	}

	// Block on reads to detect disconnects (screen frames are pushed via WriteMessage by the publisher)
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[screen] Viewer [%s] disconnected from room [%s]: %v\n", userId, roomID, err)
			break
		}
	}
}
