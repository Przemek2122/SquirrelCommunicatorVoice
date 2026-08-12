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

	// --- Screen share fields ---
	screenPublisher   *websocket.Conn            // Only one publisher per room
	screenViewers     map[*websocket.Conn]string // Viewers receiving the screen stream
	screenInitSegment []byte                     // Cached VP8/VP9 init segment for late joiners
}

// RoomManager holds the state of the entire microservice
type RoomManager struct {
	rooms map[string]*Room

	/** Mutex for changing anything in rooms structure */
	mutex sync.RWMutex

	/** API Key - Password to allow editing sensitive fields called from other server. */
	APIKey string
}

// NewRoomManager is a constructor for our service
func NewRoomManager() *RoomManager {
	return &RoomManager{
		rooms: make(map[string]*Room),
	}
}

// CreateRoom Create room or get if exists and token matches
func (rm *RoomManager) CreateRoom(roomID string, token string) *Room {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	roomIfExists, exists := rm.rooms[roomID]
	if exists {
		if roomIfExists.token != token {
			fmt.Printf("Tried to join room with incorrect token: %s", roomID)
			return nil
		}
		return roomIfExists
	}

	room := &Room{
		id:            roomID,
		token:         token,
		clients:       make(map[*websocket.Conn]string),
		initSegments:  make(map[*websocket.Conn][]byte),
		screenViewers: make(map[*websocket.Conn]string),
	}
	rm.rooms[roomID] = room

	// Destroy room when empty for 10 minutes
	room.idleTimer = time.AfterFunc(10*time.Minute, func() {
		rm.destroyRoom(roomID)
	})

	return room
}

// UpdateRoomToken changes the access token for an existing room.
// Returns false if the room does not exist.
func (rm *RoomManager) UpdateRoomToken(roomID string, newToken string) bool {
	rm.mutex.RLock()
	room, exists := rm.rooms[roomID]
	rm.mutex.RUnlock()

	if !exists {
		fmt.Printf("Tried to update token for non-existent room: %s\n", roomID)
		return false
	}

	room.SetToken(newToken)
	fmt.Printf("Token updated for room [%s]\n", roomID)
	return true
}

// RemoveRoom forcefully removes a room and disconnects all participants.
// Returns false if the room does not exist.
func (rm *RoomManager) RemoveRoom(roomID string) bool {
	rm.mutex.Lock()
	room, exists := rm.rooms[roomID]
	if !exists {
		rm.mutex.Unlock()
		fmt.Printf("Tried to remove non-existent room: %s\n", roomID)
		return false
	}

	// Remove from map immediately so new joins can't find it
	delete(rm.rooms, roomID)
	rm.mutex.Unlock()

	// Stop the idle timer — no need for destroyRoom to fire on a removed room
	room.idleTimer.Stop()

	// Collect all connections under the room lock
	room.mutex.Lock()
	allConns := make([]*websocket.Conn, 0, len(room.clients)+len(room.screenViewers)+1)
	for conn := range room.clients {
		allConns = append(allConns, conn)
	}
	if room.screenPublisher != nil {
		allConns = append(allConns, room.screenPublisher)
	}
	for conn := range room.screenViewers {
		allConns = append(allConns, conn)
	}
	// Clear maps so deferred LeaveRoom / RemoveScreenViewer are harmless no-ops
	room.clients = make(map[*websocket.Conn]string)
	room.initSegments = make(map[*websocket.Conn][]byte)
	room.screenPublisher = nil
	room.screenViewers = make(map[*websocket.Conn]string)
	room.screenInitSegment = nil
	totalClients := len(allConns)
	room.mutex.Unlock()

	// Close all connections outside any lock — prevents deadlocks
	// with deferred LeaveRoom / ClearScreenPublisher running in other goroutines
	for _, conn := range allConns {
		conn.Close()
	}

	fmt.Printf("Room [%s] forcefully removed, %d client(s) disconnected\n", roomID, totalClients)
	return true
}

// SetToken updates the room's access token in a thread-safe manner.
func (r *Room) SetToken(newToken string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.token = newToken
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
	isEmpty := room.isFullyEmpty()
	room.mutex.Unlock()

	fmt.Printf("Client left room [%s].\n", roomID)

	// Microservice cleanup: free up memory if no one is in the room
	if isEmpty {
		room.idleTimer.Reset(10 * time.Minute)
	}
}

// GetRoom validates room existence and token WITHOUT adding to any client list.
// Used by screen share to validate access before adding to screen-specific maps.
func (rm *RoomManager) GetRoom(roomID string, token string) *Room {
	rm.mutex.RLock()
	room, exists := rm.rooms[roomID]
	rm.mutex.RUnlock()

	if !exists {
		fmt.Printf("Tried to connect to non-existent room: %s\n", roomID)
		return nil
	}

	if room.token != token {
		fmt.Printf("Tried to join room with incorrect token: %s\n", roomID)
		return nil
	}

	return room
}

func (rm *RoomManager) DoesRoomExist(roomID string) bool {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	_, exists := rm.rooms[roomID]
	return exists
}

// isFullyEmpty checks if the room has NO participants of any kind (audio + screen).
// Must be called with room.mutex held.
func (r *Room) isFullyEmpty() bool {
	return len(r.clients) == 0 && len(r.screenViewers) == 0 && r.screenPublisher == nil
}

// resetIdleIfEmpty starts the idle destruction timer if the room has zero participants.
func (rm *RoomManager) resetIdleIfEmpty(roomID string) {
	rm.mutex.RLock()
	room, exists := rm.rooms[roomID]
	rm.mutex.RUnlock()
	if !exists {
		return
	}

	room.mutex.Lock()
	defer room.mutex.Unlock()

	if room.isFullyEmpty() {
		room.idleTimer.Reset(10 * time.Minute)
		fmt.Printf("Room [%s] is now fully empty, idle timer started.\n", roomID)
	}
}

func (rm *RoomManager) destroyRoom(roomID string) {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	room, exists := rm.rooms[roomID]
	if !exists {
		return
	}

	room.mutex.Lock()
	isEmpty := room.isFullyEmpty()
	room.mutex.Unlock()

	if isEmpty {
		delete(rm.rooms, roomID)
		fmt.Printf("Room [%s] is empty and was destroyed.\n", roomID)
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

			// Close the dead connection; the caller's deferred LeaveRoom will
			// handle removing it from the room's client map and idle timer logic.
			client.Close()
		}
	}
}

// --- Screen Share Methods ---

// SetScreenPublisher attempts to register a connection as the screen publisher.
// Returns false if there's already an active publisher.
func (r *Room) SetScreenPublisher(conn *websocket.Conn, userId string) bool {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if r.screenPublisher != nil {
		return false
	}

	r.screenPublisher = conn
	r.idleTimer.Stop() // Room is active
	fmt.Printf("Screen publisher [%s] started in room [%s]\n", userId, r.id)
	return true
}

// ClearScreenPublisher removes the screen publisher and closes all viewer connections.
func (r *Room) ClearScreenPublisher(conn *websocket.Conn) []*websocket.Conn {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if r.screenPublisher != conn {
		return nil
	}

	r.screenPublisher = nil
	r.screenInitSegment = nil

	// Collect all viewer connections to close them outside the lock
	viewers := make([]*websocket.Conn, 0, len(r.screenViewers))
	for vConn := range r.screenViewers {
		viewers = append(viewers, vConn)
	}
	// Clear the viewer map
	r.screenViewers = make(map[*websocket.Conn]string)

	fmt.Printf("Screen publisher left room [%s], %d viewers disconnected\n", r.id, len(viewers))
	return viewers
}

// AddScreenViewer registers a connection as a screen viewer and stops the idle timer.
func (r *Room) AddScreenViewer(conn *websocket.Conn, userId string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.screenViewers[conn] = userId
	r.idleTimer.Stop()
	fmt.Printf("Screen viewer [%s] joined room [%s]. Total viewers: %d\n", userId, r.id, len(r.screenViewers))
}

// RemoveScreenViewer removes a screen viewer connection.
func (r *Room) RemoveScreenViewer(conn *websocket.Conn) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	delete(r.screenViewers, conn)
	fmt.Printf("Screen viewer left room [%s]. Remaining viewers: %d\n", r.id, len(r.screenViewers))
}

// GetScreenInitSegment returns the cached screen init segment (or nil).
// Safe to call without holding the mutex externally; we lock internally.
func (r *Room) GetScreenInitSegment() []byte {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if r.screenInitSegment == nil {
		return nil
	}
	// Return a copy to avoid data races
	seg := make([]byte, len(r.screenInitSegment))
	copy(seg, r.screenInitSegment)
	return seg
}

// BroadcastScreen sends a binary video message to all screen viewers (not the publisher).
// Caches WebM initialization chunks for late-joining viewers.
func (r *Room) BroadcastScreen(sender *websocket.Conn, message []byte) {
	r.mutex.Lock()

	// 1. Detect WebM EBML magic bytes — cache as init segment for late joiners
	if len(message) >= 4 && message[0] == 0x1A && message[1] == 0x45 && message[2] == 0xDF && message[3] == 0xA3 {
		r.screenInitSegment = make([]byte, len(message))
		copy(r.screenInitSegment, message)
	}

	// 2. Fast copy of target viewers (exclude sender)
	targets := make([]*websocket.Conn, 0, len(r.screenViewers))
	for client := range r.screenViewers {
		if client != sender {
			targets = append(targets, client)
		}
	}

	r.mutex.Unlock()

	// 3. Transmit to all viewers
	for _, client := range targets {
		err := client.WriteMessage(websocket.BinaryMessage, message)
		if err != nil {
			log.Printf("Error broadcasting screen in room %s: %v", r.id, err)

			// Close the dead connection; the caller's deferred RemoveScreenViewer will
			// handle removing it from the viewer map and idle timer logic.
			client.Close()
		}
	}
}
