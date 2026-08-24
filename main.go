package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	// Allow connections from any origin — authentication is handled via room
	// tokens and API keys, so origin checking is unnecessary for this
	// microservice (there are no ambient credentials like cookies to protect).
	CheckOrigin: func(r *http.Request) bool { return true },
}

// --- Main Application Entrypoint ---

func main() {
	// Initialize our microservice core
	manager := NewRoomManager()

	// Hard (but configurable) cap on concurrent screen shares per room.
	maxScreenSharesPerRoom = GetMaxScreenSharesPerRoom()
	fmt.Printf("Max concurrent screen shares per room: %d\n", maxScreenSharesPerRoom)

	// Require an API key before serving. With an empty key, every REST handler's
	// check (clientToken != rm.APIKey) would pass for an absent token, leaving
	// room create/remove/update open to anyone. Fail fast instead of running
	// with an open admin API.
	manager.APIKey = GetAPIKey()
	if manager.APIKey == "" {
		log.Fatal("[FATAL] SQRLL_VOICE_API_KEY is not set — refusing to start with an open admin API")
	}
	fmt.Printf("Server requires an API key to manage rooms\n")

	// --- REST API ---
	http.HandleFunc("/api/rooms/create", manager.handleCreateRoomAPI)
	http.HandleFunc("/api/rooms/check", manager.handleCheckRoomAPI)
	http.HandleFunc("/api/rooms/update-token", manager.handleUpdateRoomTokenAPI)
	http.HandleFunc("/api/rooms/remove", manager.handleRemoveRoomAPI)

	// --- WebSocket endpoints ---
	http.HandleFunc("/api/rooms/stream", func(w http.ResponseWriter, r *http.Request) {
		handleAudioStream(manager, w, r)
	})
	http.HandleFunc("/api/rooms/screenshare", func(w http.ResponseWriter, r *http.Request) {
		handleScreenShare(manager, w, r)
	})

	http.HandleFunc("/health", handleHealthCheck)

	addr := net.JoinHostPort(GetAddress(), GetPort())

	// Explicit server timeouts. ReadHeaderTimeout defeats slowloris-style
	// attacks on the HTTP handshake; IdleTimeout reaps idle keep-alive
	// connections. Read/WriteTimeout are deliberately left unset: WebSocket
	// connections are hijacked after the handshake and must never be subject to
	// a total request timeout.
	server := &http.Server{
		Addr:              addr,
		Handler:           nil, // DefaultServeMux
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	fmt.Printf("Go Microservice (Rooms) listening on '%s'...\n", addr)
	err := server.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		log.Fatal("Server error: ", err)
	}
}
