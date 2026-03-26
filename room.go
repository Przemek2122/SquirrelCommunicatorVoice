package main

import (
	"fmt"
	"log"
	"sync"

	"github.com/gorilla/websocket"
)

// Room represents a single channel with its own isolated state and mutex
type Room struct {
	id      string
	token   string
	clients map[*websocket.Conn]bool
	mutex   sync.Mutex
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
func (rm *RoomManager) JoinRoom(roomID string, token string, conn *websocket.Conn) *Room {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	room, exists := rm.rooms[roomID]

	// Does room exists
	if !exists {
		fmt.Printf("Tried to connect to non-existent room: %s", roomID)
		return nil
	}

	// Is token correct
	if room.token != token {
		fmt.Printf("Tried to join room with incorrect token: %s", roomID)
		return nil
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

// Broadcast sends a binary message to everyone in the room except the sender
func (r *Room) Broadcast(sender *websocket.Conn, message []byte) {
	r.mutex.Lock()
	defer r.mutex.Unlock() // Ensure unlock happens even if we return early

	for client := range r.clients {
		if client != sender {
			err := client.WriteMessage(websocket.BinaryMessage, message)
			if err != nil {
				log.Printf("Error broadcasting to a client in room %s: %v", r.id, err)
				err := client.Close()
				if err != nil {
					log.Printf("Dead connection in room %s: %v", r.id, err)
				}
				delete(r.clients, client)
			}
		}
	}
}
