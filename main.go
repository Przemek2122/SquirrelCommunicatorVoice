package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	// @TODO Temp allow anything
	CheckOrigin: func(r *http.Request) bool { return true },
}

func GetPort() string {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // Fallback
	}

	return fmt.Sprintf(":%s", port)
}

// --- Main Application Entrypoint ---

func main() {
	// Initialize our microservice core
	manager := NewRoomManager()

	// Debug room
	manager.CreateRoom("test", "test")

	http.HandleFunc("/api/rooms/create", manager.handleCreateRoomAPI)
	http.HandleFunc("/api/rooms/check", manager.handleCheckRoomAPI)

	// Inject the manager into our HTTP handler using a closure
	http.HandleFunc("/api/rooms/stream", func(w http.ResponseWriter, r *http.Request) {
		handleAudioStream(manager, w, r)
	})

	fmt.Println("Go Microservice (Rooms) listening on port 8080...")
	err := http.ListenAndServe(GetPort(), nil)
	if err != nil {
		log.Fatal("Server error: ", err)
	}
}
