package main

import (
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	// @TODO Temp allow anything
	CheckOrigin: func(r *http.Request) bool { return true },
}

// --- Domain Models ---

// Room represents a single channel with its own isolated state and mutex
type Room struct {
	id      string
	token   string
	clients map[*websocket.Conn]bool
	mutex   sync.Mutex
}

// Broadcast sends a binary message to everyone in the room except the sender
func (r *Room) Broadcast(sender *websocket.Conn, message []byte) {
	r.mutex.Lock()
	defer r.mutex.Unlock() // Ensure unlock happens even if we return early

	for client := range r.clients {
		if client != sender {
			err := client.WriteMessage(websocket.BinaryMessage, message)
			if err != nil {
				log.Printf("Error broadcasting to a client in room %s: %v", r.id, err)
				client.Close()
				delete(r.clients, client)
			}
		}
	}
}

// RoomManager holds the state of the entire microservice
type RoomManager struct {
	rooms map[string]*Room
	mutex sync.Mutex
}

// NewRoomManager is a constructor for our service
func NewRoomManager() *RoomManager {
	return &RoomManager{
		rooms: make(map[string]*Room),
	}
}

func (rm *RoomManager) CreateRoom(roomID string, token string) *Room {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	room := &Room{
		id:      roomID,
		token:   token,
		clients: make(map[*websocket.Conn]bool),
	}
	rm.rooms[roomID] = room

	return room
}

// JoinRoom adds a client to a specific room, creating it if it doesn't exist
func (rm *RoomManager) JoinRoom(roomID string, conn *websocket.Conn) *Room {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	room, exists := rm.rooms[roomID]
	if !exists {
		// Create a new room if it's the first person joining
		room = &Room{
			id:      roomID,
			clients: make(map[*websocket.Conn]bool),
		}
		rm.rooms[roomID] = room
		fmt.Printf("Created new room: %s\n", roomID)
	}

	// Lock the specific room and add the client
	room.mutex.Lock()
	room.clients[conn] = true
	room.mutex.Unlock()

	fmt.Printf("Client joined room [%s]. Total clients in room: %d\n", roomID, len(room.clients))
	return room
}

// LeaveRoom removes a client and cleans up the room if it's empty
func (rm *RoomManager) LeaveRoom(roomID string, conn *websocket.Conn) {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	room, exists := rm.rooms[roomID]
	if !exists {
		return
	}

	room.mutex.Lock()
	delete(room.clients, conn)
	isEmpty := len(room.clients) == 0
	room.mutex.Unlock()

	fmt.Printf("Client left room [%s].\n", roomID)

	// Microservice cleanup: free up memory if no one is in the room
	if isEmpty {
		delete(rm.rooms, roomID)
		fmt.Printf("Room [%s] is empty and was destroyed.\n", roomID)
	}
}

// --- HTTP Transport / Handlers ---

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

// --- Main Application Entrypoint ---

func main() {
	// Initialize our microservice core
	manager := NewRoomManager()

	// Inject the manager into our HTTP handler using a closure
	http.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		handleAudioStream(manager, w, r)
	})

	fmt.Println("Go Microservice (Rooms) listening on port 8080...")
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal("Server error: ", err)
	}
}
