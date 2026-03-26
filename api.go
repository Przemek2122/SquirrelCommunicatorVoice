package main

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
)

type CreateRoomRequest struct {
	RoomId string `json:"roomId"`
	Token  string `json:"token"`
}

func (rm *RoomManager) handleCreateRoomAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}

	var req CreateRoomRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "Incorrect JSON", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusCreated)
}

func handleAudioStream(rm *RoomManager, w http.ResponseWriter, r *http.Request) {
	// Extract room ID from query parameter (e.g., ?room=test)
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		roomID = "general" // Default fallback
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}
	defer conn.Close()

	// 1. Client joins the requested room
	room := rm.JoinRoom(roomID, conn)

	// 2. Ensure the client is removed when they disconnect
	defer rm.LeaveRoom(roomID, conn)

	// 3. Infinite loop to listen for audio chunks
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("Client disconnected from room [%s]: %v\n", roomID, err)
			break
		}

		// 4. Broadcast logic delegated to the Room struct
		if messageType == websocket.BinaryMessage {
			room.Broadcast(conn, message)
		}
	}
}
