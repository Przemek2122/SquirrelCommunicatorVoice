package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	// Allow connections from any origin — authentication is handled via room tokens
	// and API keys, so origin checking is unnecessary for this microservice.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// --- Main Application Entrypoint ---

func main() {
	// Initialize our microservice core
	manager := NewRoomManager()

	// Hard (but configurable) cap on concurrent screen shares per room.
	maxScreenSharesPerRoom = GetMaxScreenSharesPerRoom()
	fmt.Printf("Max concurrent screen shares per room: %d\n", maxScreenSharesPerRoom)

	// Debug room
	manager.CreateRoom("test", "test")

	// Ensure we have APIKey or log
	manager.APIKey = GetAPIKey()
	if manager.APIKey == "" { // @TODO: Temporary allow empty key
		log.Printf("[WARNING] Server APIKey missing, we will allow anyone")
	} else {
		fmt.Printf("Server has APIKey and will require it to connect\n")
	}

	// --- REST API ---
	http.HandleFunc("/api/rooms/create", manager.handleCreateRoomAPI)
	http.HandleFunc("/api/rooms/check", manager.handleCheckRoomAPI)
	http.HandleFunc("/api/rooms/update-token", manager.handleUpdateRoomTokenAPI)
	http.HandleFunc("/api/rooms/remove", manager.handleRemoveRoomAPI)

	// --- GIF proxy (KLIPY) ---
	http.HandleFunc("/api/gifs/search", manager.handleGifsSearch)
	http.HandleFunc("/api/gifs/trending", manager.handleGifsTrending)
	http.HandleFunc("/api/gifs/fetch", manager.handleGifsFetch)

	// --- File storage proxy (upload / download via the image service) ---
	http.HandleFunc("/api/files/upload", manager.handleFileUpload)
	http.HandleFunc("/api/files/", manager.handleFileDownload)

	// --- WebSocket endpoints ---
	http.HandleFunc("/api/rooms/stream", func(w http.ResponseWriter, r *http.Request) {
		handleAudioStream(manager, w, r)
	})
	http.HandleFunc("/api/rooms/screenshare", func(w http.ResponseWriter, r *http.Request) {
		handleScreenShare(manager, w, r)
	})

	http.HandleFunc("/health", handleHealthCheck)

	port := GetPort()
	fmt.Printf("Go Microservice (Rooms) listening on port '%s'...", port)
	err := http.ListenAndServe(port, nil)
	if err != nil {
		log.Fatal("Server error: ", err)
	}
}
