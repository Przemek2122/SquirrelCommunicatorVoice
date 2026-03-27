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
		return
	}

	var req CreateRoomRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "Incorrect JSON", http.StatusBadRequest)
		return
	}

	// Check token (Server should send auth)
	clientToken := r.Header.Get("X-API-Token")
	if clientToken != rm.APIKey {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	rm.CreateRoom(req.RoomId, req.Token)

	w.WriteHeader(http.StatusCreated)
}

func (rm *RoomManager) handleCheckRoomAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check token (Server should send auth)
	clientToken := r.Header.Get("X-API-Token")
	if clientToken != rm.APIKey {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		http.Error(w, "Missing room name", http.StatusBadRequest)
		return
	}

	exists := rm.DoesRoomExist(roomID)

	if exists {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusNotFound)
	}
}

func handleAudioStream(rm *RoomManager, w http.ResponseWriter, r *http.Request) {
	// Get room name
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		log.Println("Missing room name")
		http.Error(w, "Missing room name", http.StatusNotFound)
		return
	}

	// Get user id
	userId := r.URL.Query().Get("userid")
	if userId == "" {
		log.Println("Missing room userid")
		http.Error(w, "Missing room userid", http.StatusNotFound)
		return
	}

	// Get room token (password)
	token := r.URL.Query().Get("token")
	if token == "" {
		log.Println("Missing room token")
		http.Error(w, "Missing room token", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}
	defer conn.Close()

	// 1. Client joins the requested room
	room := rm.JoinRoom(roomID, token, userId, conn)

	if room == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// 2. Ensure the client is removed when they disconnect
	defer rm.LeaveRoom(roomID, conn)

	// 3. Infinite loop to listen for audio chunks
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			break
		}

		// 4. Broadcast logic delegated to the Room struct
		if messageType == websocket.BinaryMessage {
			room.Broadcast(conn, message)
		}
	}
}
