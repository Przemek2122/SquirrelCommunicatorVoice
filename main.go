package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	// @TODO Temp allow anything
	CheckOrigin: func(r *http.Request) bool { return true },
}

// --- Main Application Entrypoint ---

func main() {
	// Initialize our microservice core
	manager := NewRoomManager()

	// Debug room
	manager.CreateRoom("test", "test")

	// Ensure we have APIKey or log
	manager.APIKey = GetAPIKey()
	if manager.APIKey == "" { // @TODO: Temporary allow empty key
		log.Printf("[WARNING] Server APIKey missing, we will allow anyone")
	}

	http.HandleFunc("/api/rooms/create", manager.handleCreateRoomAPI)
	http.HandleFunc("/api/rooms/check", manager.handleCheckRoomAPI)

	// Inject the manager into our HTTP handler using a closure
	http.HandleFunc("/api/rooms/stream", func(w http.ResponseWriter, r *http.Request) {
		handleAudioStream(manager, w, r)
	})

	port := GetPort()
	fmt.Printf("Go Microservice (Rooms) listening on port '%s'...", port)
	err := http.ListenAndServe(port, nil)
	if err != nil {
		log.Fatal("Server error: ", err)
	}
}
