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

	// Inject the manager into our HTTP handler using a closure
	http.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		handleAudioStream(manager, w, r)
	})

	http.HandleFunc("/api/rooms", manager.handleCreateRoomAPI)

	fmt.Println("Go Microservice (Rooms) listening on port 8080...")
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal("Server error: ", err)
	}
}
