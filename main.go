package main

import (
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// Upgrader configuration
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for local testing
	},
}

// Global state to keep track of connected clients
// We use a mutex to prevent race conditions when multiple goroutines access the map
var clientsMutex sync.Mutex
var clients = make(map[*websocket.Conn]bool)

func handleAudioStream(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}
	// Ensure connection is closed when the function exits
	defer conn.Close()

	// Safely add the new client to our global map
	clientsMutex.Lock()
	clients[conn] = true
	clientsMutex.Unlock()

	fmt.Printf("New client connected! Total clients: %d\n", len(clients))

	// Cleanup block: remove client from map when they disconnect
	defer func() {
		clientsMutex.Lock()
		delete(clients, conn)
		clientsMutex.Unlock()
		fmt.Println("Client disconnected.")
	}()

	// Infinite loop to listen for incoming audio chunks from THIS client
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Println("Read error or client disconnected:", err)
			break
		}

		if messageType == websocket.BinaryMessage {
			// Broadcast the audio chunk to all OTHER connected clients
			clientsMutex.Lock()
			for client := range clients {
				if client != conn { // Don't send the audio back to the sender
					err := client.WriteMessage(websocket.BinaryMessage, message)
					if err != nil {
						log.Printf("Failed to send message to a client: %v", err)
						client.Close()
						delete(clients, client)
					}
				}
			}
			clientsMutex.Unlock()
		}
	}
}

func main() {
	http.HandleFunc("/stream", handleAudioStream)

	fmt.Println("Go audio broadcaster listening on port 8080...")

	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal("Server error: ", err)
	}
}
