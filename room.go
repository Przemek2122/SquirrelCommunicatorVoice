package main

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Room represents a single channel with its own isolated state and mutex
type Room struct {
	id    string
	token string
	mutex sync.Mutex

	/** Connected clients mapped to bool */
	clients map[*websocket.Conn]string

	/** First packages from each client */
	initSegments map[*websocket.Conn][]byte

	/** Timer for room delete when empty */
	idleTimer *time.Timer
}

// RoomManager holds the state of the entire microservice
type RoomManager struct {
	rooms map[string]*Room
	mutex sync.RWMutex
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
		id:           roomID,
		token:        token,
		clients:      make(map[*websocket.Conn]string),
		initSegments: make(map[*websocket.Conn][]byte),
	}
	rm.rooms[roomID] = room

	// Destroy room when empty for 15 minutes
	room.idleTimer = time.AfterFunc(15*time.Minute, func() {
		rm.destroyRoom(roomID) // To nowa funkcja, zaraz ją omówimy
	})

	return room
}

// JoinRoom adds a client to a specific room, creating it if it doesn't exist
func (rm *RoomManager) JoinRoom(roomID string, token string, userId string, conn *websocket.Conn) *Room {
	rm.mutex.Lock()
	room, exists := rm.rooms[roomID]
	rm.mutex.Unlock()

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
	room.idleTimer.Stop()
	room.clients[conn] = userId

	// 2. Fast copy
	var chunksToSend [][]byte
	for _, initChunk := range room.initSegments {
		chunksToSend = append(chunksToSend, initChunk)
	}

	room.mutex.Unlock()

	fmt.Printf("Client joined room [%s]. Total clients in room: %d\n", roomID, len(room.clients))

	// 4. Send packages without mutex locked
	for _, chunk := range chunksToSend {
		err := conn.WriteMessage(websocket.BinaryMessage, chunk)
		if err != nil {
			fmt.Printf("Error sending init segment: %v\n", err)
			break
		}
	}

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
	delete(room.initSegments, conn)
	isEmpty := len(room.clients) == 0
	room.mutex.Unlock()

	fmt.Printf("Client left room [%s].\n", roomID)

	// Microservice cleanup: free up memory if no one is in the room
	if isEmpty {
		room.idleTimer.Reset(10 * time.Minute)
	}
}

func (rm *RoomManager) DoesRoomExist(roomID string) bool {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	_, exists := rm.rooms[roomID]
	return exists
}

func (rm *RoomManager) destroyRoom(roomID string) {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	room, exists := rm.rooms[roomID]

	if exists {
		room.mutex.Lock()
		isEmpty := len(room.clients) == 0
		room.mutex.Unlock()

		if isEmpty {
			delete(rm.rooms, roomID)
			fmt.Printf("Room [%s] is empty and was destroyed.\n", roomID)
		}
	}
}

// Broadcast sends a binary audio message to all clients in the room except the sender.
// It prepends the sender's dynamic ID to the payload and caches WebM initialization chunks.
func (r *Room) Broadcast(sender *websocket.Conn, message []byte) {
	r.mutex.Lock()

	// 1. Retrieve the sender's ID and calculate its byte length
	senderID := r.clients[sender]
	idBytes := []byte(senderID)
	idLen := byte(len(idBytes)) // Length stored as a single byte (0-255)

	// 2. Construct the final payload: [ID Length (1 byte)] + [ID Bytes] + [Audio Chunk]
	finalMessage := append([]byte{idLen}, idBytes...)
	finalMessage = append(finalMessage, message...)

	// 3. Detect WebM EBML magic bytes (0x1A 0x45 0xDF 0xA3) in the ORIGINAL message.
	// If it's an initialization chunk, cache the FINAL message (which includes the ID)
	// so new clients joining later can properly decode this user's stream.
	if len(message) >= 4 && message[0] == 0x1A && message[1] == 0x45 && message[2] == 0xDF && message[3] == 0xA3 {
		r.initSegments[sender] = finalMessage
	}

	// 4. Create a fast, local copy of target clients.
	// We do this to avoid holding the room mutex during slow network I/O operations.
	targets := make([]*websocket.Conn, 0, len(r.clients))
	for client := range r.clients {
		if client != sender {
			targets = append(targets, client)
		}
	}

	// Release the lock immediately after state copy is done
	r.mutex.Unlock()

	// 5. Safely transmit the constructed package to all targeted clients
	for _, client := range targets {
		err := client.WriteMessage(websocket.BinaryMessage, finalMessage)
		if err != nil {
			log.Printf("Error broadcasting to a client in room %s: %v", r.id, err)

			// If a connection is dead, we need to lock again just to clean it up
			r.mutex.Lock()
			client.Close()
			delete(r.clients, client)
			r.mutex.Unlock()
		}
	}
}
