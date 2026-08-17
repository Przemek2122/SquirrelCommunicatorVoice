package main

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// --- Deterministic WebM Cluster-aligned relay ---------------------------------
//
// MediaRecorder emits a live WebM stream where the init (EBML + Segment +
// Info + Tracks) and each Cluster are written with an UNKNOWN element size, and
// a Cluster may be split across several WebSocket messages at arbitrary byte
// offsets (so a receiver can get a chunk starting mid-element, e.g. in the
// middle of a Cluster ID).
//
// A bare init (a Segment with unknown size and no following Cluster) is FATAL
// for Chrome's MSE: appending it alone makes the SourceBuffer error with the
// MediaSource transitioned to "ended". A receiver must therefore never be
// handed an init without the first Cluster, and a late joiner must never be
// handed a chunk that starts mid-Cluster.
//
// webmRelay makes this deterministic: it accumulates a publisher's raw bytes
// and only emits
//   1. one keyframe = init + FIRST COMPLETE Cluster, then
//   2. subsequent COMPLETE Clusters.
// A receiver always starts with init+media and always continues at Cluster
// boundaries.

// clusterIDBytes is the WebM Cluster element ID (0x1F 0x43 0xB6 0x75).
var clusterIDBytes = []byte{0x1F, 0x43, 0xB6, 0x75}

// ebmlMagic is the WebM EBML header magic (0x1A 0x45 0xDF 0xA3). Every
// MediaRecorder WebM init segment begins with these four bytes.
var ebmlMagic = []byte{0x1A, 0x45, 0xDF, 0xA3}

// isEBMLHeader reports whether data starts with the WebM EBML header magic.
func isEBMLHeader(data []byte) bool {
	return len(data) >= 4 &&
		data[0] == ebmlMagic[0] &&
		data[1] == ebmlMagic[1] &&
		data[2] == ebmlMagic[2] &&
		data[3] == ebmlMagic[3]
}

const (
	// maxAudioPendingBytes bounds how much a single audio sender may buffer
	// while waiting for the next Cluster boundary. A healthy MediaRecorder
	// stream emits a Cluster every ~100 ms (~1 KB of Opus), so this only ever
	// triggers on a pathological stream and simply degrades to raw forwarding.
	maxAudioPendingBytes = 1 << 20

	// maxScreenPendingBytes is the same safety valve for the (much higher
	// bitrate) screen-share stream.
	maxScreenPendingBytes = 4 << 20
)

// findClusterOffset returns the offset of the next WebM Cluster element ID in
// data at or after start, or -1. MediaRecorder writes Clusters with unknown
// size, so scanning for the 4-byte Cluster ID is the only reliable boundary
// detector (the same technique the frontend uses).
func findClusterOffset(data []byte, start int) int {
	if start < 0 {
		start = 0
	}
	for i := start; i+len(clusterIDBytes) <= len(data); i++ {
		if data[i] == clusterIDBytes[0] &&
			data[i+1] == clusterIDBytes[1] &&
			data[i+2] == clusterIDBytes[2] &&
			data[i+3] == clusterIDBytes[3] {
			return i
		}
	}
	return -1
}

// webmRelay splits a single publisher's WebM byte stream into a keyframe
// (init + first complete Cluster) followed by complete Clusters.
type webmRelay struct {
	maxPending int    // safety-valve budget for pending bytes
	pending    []byte // raw publisher bytes not yet emitted
	ready      bool   // keyframe finalized
	keyframe   []byte // init + first complete Cluster (nil until ready)
}

// feed appends raw publisher bytes and returns the complete units that should
// now be relayed, in order. keyframeFinalized reports whether the keyframe was
// produced during this call (so the caller can refresh its cached init for
// late joiners). Each returned unit is an independent copy.
func (f *webmRelay) feed(data []byte) (units [][]byte, keyframeFinalized bool) {
	if f.maxPending <= 0 {
		f.maxPending = maxAudioPendingBytes
	}
	f.pending = append(f.pending, data...)

	// flush copies the first n pending bytes into a fresh unit and drops them
	// from the pending buffer (memmove-safe).
	flush := func(n int) []byte {
		unit := make([]byte, n)
		copy(unit, f.pending[:n])
		copy(f.pending, f.pending[n:])
		f.pending = f.pending[:len(f.pending)-n]
		return unit
	}

	if !f.ready {
		c1 := findClusterOffset(f.pending, 0)
		if c1 >= 0 {
			c2 := findClusterOffset(f.pending, c1+len(clusterIDBytes))
			if c2 >= 0 {
				f.keyframe = flush(c2)
				f.ready = true
				keyframeFinalized = true
				units = append(units, f.keyframe)
			}
		}

		// Safety valve: never hold the init forever. If we still cannot form a
		// keyframe after a generous budget, emit what we have (init + partial
		// Cluster) so receivers are not starved.
		if !f.ready && len(f.pending) > f.maxPending {
			f.keyframe = flush(len(f.pending))
			f.ready = true
			keyframeFinalized = true
			units = append(units, f.keyframe)
		}
	}

	if f.ready {
		// Emit complete Clusters. After the keyframe, pending always starts at
		// a Cluster boundary, so look for the NEXT boundary from index 4.
		for {
			c := findClusterOffset(f.pending, len(clusterIDBytes))
			if c < 0 {
				break
			}
			units = append(units, flush(c))
		}

		// Safety valve: a single Cluster larger than the budget (pathological)
		// is flushed raw rather than buffered forever.
		if len(f.pending) > f.maxPending {
			units = append(units, flush(len(f.pending)))
		}
	}

	return units, keyframeFinalized
}

// Room represents a single channel with its own isolated state and mutex
type Room struct {
	id    string
	token string
	mutex sync.Mutex

	/** Connected clients mapped to bool */
	clients map[*websocket.Conn]string

	/** First packages from each client */
	initSegments map[*websocket.Conn][]byte

	/** Per-sender WebM Cluster-aligned relay state */
	audioRelays map[*websocket.Conn]*webmRelay

	/** Timer for room delete when empty */
	idleTimer *time.Timer

	// --- Screen share fields ---
	screenPublisher   *websocket.Conn            // Only one publisher per room
	screenViewers     map[*websocket.Conn]string // Viewers receiving the screen stream
	screenInitSegment []byte                     // Cached VP8/VP9 init segment for late joiners
	screenInitBuffer  []byte                     // Accumulating publisher bytes until a COMPLETE init is found
	screenRelay       *webmRelay                 // publisher's WebM Cluster-aligned relay
	screenInitReady   bool                       // true once screenInitSegment is a complete init

	// screenInitPending tracks viewers whose init segment has NOT yet been
	// delivered. Media frames must never reach a viewer before its init, so
	// BroadcastScreen skips any viewer still marked here; the mark is cleared
	// once the init has actually been written to that viewer.
	screenInitPending map[*websocket.Conn]bool

	// audioWriteMu serializes ALL writes to audio client connections.
	//
	// Two participants talking at the same time means two goroutines (one per
	// sender's read loop) can both target the SAME receiver's *websocket.Conn in
	// Broadcast. gorilla/websocket panics with "concurrent write to websocket
	// connection" on concurrent writes, so every audio write must go through this
	// mutex (mirrors screenWriteMu on the screen-share path).
	audioWriteMu sync.Mutex

	// screenWriteMu serializes ALL writes to screen viewer connections.
	//
	// Without this, the one-time init write in handleScreenViewer can race a
	// media write from the publisher's BroadcastScreen goroutine on the SAME
	// *websocket.Conn. gorilla/websocket panics with
	// "concurrent write to websocket connection" when two goroutines write at
	// once, so every screen write must go through this mutex.
	screenWriteMu sync.Mutex
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
		id:                roomID,
		token:             token,
		clients:           make(map[*websocket.Conn]string),
		initSegments:      make(map[*websocket.Conn][]byte),
		audioRelays:       make(map[*websocket.Conn]*webmRelay),
		screenViewers:     make(map[*websocket.Conn]string),
		screenInitPending: make(map[*websocket.Conn]bool),
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
	room.audioRelays = make(map[*websocket.Conn]*webmRelay)
	room.screenPublisher = nil
	room.screenViewers = make(map[*websocket.Conn]string)
	room.screenInitSegment = nil
	room.screenInitBuffer = nil
	room.screenRelay = nil
	room.screenInitReady = false
	room.screenInitPending = make(map[*websocket.Conn]bool)
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

	// 1. Snapshot the cached init segments under the room lock WITHOUT mutating
	// room state. The client is NOT yet registered as a broadcast target, so a
	// concurrent Broadcast() cannot deliver a mid-stream media frame before this
	// init (the old ordering raced with Broadcast and could corrupt the
	// receiver's stream). We deliberately do NOT stop the idle timer here: if
	// the init write below fails and we bail out, the room must remain able to
	// be destroyed when empty (stopping it here would leak the room).
	room.mutex.Lock()
	var chunksToSend [][]byte
	for _, initChunk := range room.initSegments {
		chunksToSend = append(chunksToSend, initChunk)
	}
	room.mutex.Unlock()

	// 2. Send init segments BEFORE registering the client as a target. This
	// guarantees init-before-media for late joiners.
	for _, chunk := range chunksToSend {
		err := conn.WriteMessage(websocket.BinaryMessage, chunk)
		if err != nil {
			fmt.Printf("Error sending init segment: %v\n", err)
			return nil
		}
	}

	// 3. Now register the client as a broadcast target and stop the idle timer,
	// atomically under the room lock. Only at this point is the room considered
	// active again, so the timer is never left stopped on a failed join.
	room.mutex.Lock()
	room.idleTimer.Stop()
	room.clients[conn] = userId
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
	delete(room.initSegments, conn)
	delete(room.audioRelays, conn)
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

// Broadcast sends a binary audio message to all clients in the room except the
// sender. It prepends the sender's dynamic ID to the payload.
//
// Audio chunks are relayed through a per-sender webmRelay so every receiver
// (including late joiners) gets a complete "init + first Cluster" keyframe
// first, followed by complete Clusters — never a bare init (which Chrome's MSE
// rejects) and never a mid-Cluster fragment.
func (r *Room) Broadcast(sender *websocket.Conn, message []byte) {
	r.mutex.Lock()

	// 1. Retrieve the sender's ID and its byte-length prefix.
	senderID := r.clients[sender]
	idBytes := []byte(senderID)
	idLen := byte(len(idBytes))

	// 2. Get (or create) this sender's Cluster-aligned relay.
	relay := r.audioRelays[sender]

	// Drop duplicate / re-broadcast EBML init segments. The relay already
	// captured this sender's init in its keyframe; a second init arriving
	// mid-stream would be concatenated onto the current Cluster and emitted as
	// one corrupt unit that breaks the receiver's SourceBuffer.
	if relay != nil && isEBMLHeader(message) {
		r.mutex.Unlock()
		return
	}

	if relay == nil {
		relay = &webmRelay{maxPending: maxAudioPendingBytes}
		r.audioRelays[sender] = relay
	}

	// 3. Feed the raw chunk and extract complete units (keyframe first, then
	//    complete Clusters).
	units, keyframeFinalized := relay.feed(message)

	// 4. If the keyframe was just finalized, cache it (ID-prefixed) so late
	//    joiners receive init + first Cluster — never a bare init.
	if keyframeFinalized && relay.keyframe != nil {
		cached := make([]byte, 0, 1+len(idBytes)+len(relay.keyframe))
		cached = append(cached, idLen)
		cached = append(cached, idBytes...)
		cached = append(cached, relay.keyframe...)
		r.initSegments[sender] = cached
	}

	// 5. Snapshot target clients without holding the lock during I/O.
	targets := make([]*websocket.Conn, 0, len(r.clients))
	for client := range r.clients {
		if client != sender {
			targets = append(targets, client)
		}
	}
	r.mutex.Unlock()

	if len(units) == 0 {
		return
	}

	// 6. Transmit each unit (ID-prefixed) to all targets. All writes are
	//    serialized through audioWriteMu so two senders' Broadcast goroutines
	//    can never write to the same receiver's *websocket.Conn at once
	//    (gorilla/websocket panics on concurrent writes).
	r.audioWriteMu.Lock()
	defer r.audioWriteMu.Unlock()

	for _, client := range targets {
		for _, unit := range units {
			final := make([]byte, 0, 1+len(idBytes)+len(unit))
			final = append(final, idLen)
			final = append(final, idBytes...)
			final = append(final, unit...)
			if err := client.WriteMessage(websocket.BinaryMessage, final); err != nil {
				log.Printf("Error broadcasting to a client in room %s: %v", r.id, err)

				// Remove the dead connection immediately so we stop trying to
				// write to it on every broadcast. The caller's deferred
				// LeaveRoom will still fire, but the delete here is idempotent.
				r.mutex.Lock()
				delete(r.clients, client)
				delete(r.initSegments, client)
				delete(r.audioRelays, client)
				r.mutex.Unlock()
				client.Close()
				break
			}
		}
	}
}

// SendKeyframe re-sends the cached init segment for a given target user to the
// requesting connection. This supports the frontend's "request_keyframe" control
// message: when a viewer's SourceBuffer fails and is rebuilt, it requests a fresh
// copy of the sender's init so it can re-seed its MediaSource. The audio init is
// otherwise only sent once per session (on join), so without this a viewer that
// rejected the init has no recovery path.
//
// The write is serialized through audioWriteMu to avoid racing the sender's own
// Broadcast writes to the same connection.
func (r *Room) SendKeyframe(requester *websocket.Conn, targetUserID string) {
	r.mutex.Lock()
	var initChunk []byte
	for sender, initSeg := range r.initSegments {
		if r.clients[sender] == targetUserID {
			initChunk = initSeg
			break
		}
	}
	r.mutex.Unlock()

	if initChunk == nil {
		return
	}

	r.audioWriteMu.Lock()
	defer r.audioWriteMu.Unlock()
	if err := requester.WriteMessage(websocket.BinaryMessage, initChunk); err != nil {
		log.Printf("Error sending keyframe to client in room %s: %v", r.id, err)
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
	r.screenInitBuffer = nil
	r.screenRelay = nil
	r.screenInitReady = false
	r.screenInitPending = make(map[*websocket.Conn]bool)

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

// RegisterScreenViewer atomically adds a viewer and returns the cached init
// segment to send to it first (nil if there is no init yet). The viewer is
// marked as "init pending" so BroadcastScreen will NOT relay media to it until
// the init has actually been delivered (see MarkScreenInitDelivered). This
// guarantees a viewer never receives media before its init segment, which is
// what previously corrupted the WebM decoder on the frontend.
func (r *Room) RegisterScreenViewer(conn *websocket.Conn, userId string) []byte {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.screenViewers[conn] = userId
	r.screenInitPending[conn] = true // gate media until init is delivered first
	r.idleTimer.Stop()
	fmt.Printf("Screen viewer [%s] joined room [%s]. Total viewers: %d\n", userId, r.id, len(r.screenViewers))

	// No init cached yet — the publisher's first init frame will reach this
	// viewer via BroadcastScreen's first-init broadcast path.
	if r.screenInitSegment == nil {
		return nil
	}

	initSeg := make([]byte, len(r.screenInitSegment))
	copy(initSeg, r.screenInitSegment)
	return initSeg
}

// MarkScreenInitDelivered clears the "init pending" gate for a viewer, allowing
// BroadcastScreen to start relaying media frames to it. Must be called after
// the init segment has been successfully written to the connection.
func (r *Room) MarkScreenInitDelivered(conn *websocket.Conn) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	delete(r.screenInitPending, conn)
}

// RemoveScreenViewer removes a screen viewer connection.
func (r *Room) RemoveScreenViewer(conn *websocket.Conn) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	delete(r.screenViewers, conn)
	delete(r.screenInitPending, conn)
	fmt.Printf("Screen viewer left room [%s]. Remaining viewers: %d\n", r.id, len(r.screenViewers))
}

// BroadcastScreen sends a binary video message to all screen viewers (not the
// publisher).
//
// It relays the publisher's raw WebM through a per-room webmRelay, which
// deterministically produces "init + first complete Cluster" (the keyframe)
// then complete Clusters. The keyframe is cached for late joiners and delivered
// to every viewer (clearing its init-pending gate); media Clusters are
// delivered only to viewers whose init has already been delivered.
func (r *Room) BroadcastScreen(sender *websocket.Conn, message []byte) {
	r.mutex.Lock()

	relay := r.screenRelay

	// Drop the publisher's periodic re-broadcast init segments. The relay
	// captures the init once in its keyframe; feeding a second (bare) init
	// would concatenate it onto the current Cluster and emit one corrupt unit
	// that makes every viewer's SourceBuffer error out.
	if relay != nil && isEBMLHeader(message) {
		r.mutex.Unlock()
		return
	}

	if relay == nil {
		relay = &webmRelay{maxPending: maxScreenPendingBytes}
		r.screenRelay = relay
	}

	units, keyframeFinalized := relay.feed(message)

	// Cache the keyframe (init + first complete Cluster) for late joiners.
	if keyframeFinalized && relay.keyframe != nil {
		r.screenInitSegment = append([]byte(nil), relay.keyframe...)
		r.screenInitReady = true
		r.screenInitBuffer = nil
		log.Printf("screenshare init cached: room=%s bytes=%d", r.id, len(r.screenInitSegment))
	}

	// Build the ordered list of (connection, payload) writes under the room
	// lock. The keyframe is the FIRST unit and is delivered to every viewer
	// (clearing its init-pending gate); media Clusters go only to viewers whose
	// gate is already clear.
	type screenWrite struct {
		conn *websocket.Conn
		data []byte
	}
	var writes []screenWrite
	for i, unit := range units {
		isKeyframe := keyframeFinalized && i == 0
		for client := range r.screenViewers {
			if client == sender {
				continue
			}
			if isKeyframe {
				writes = append(writes, screenWrite{client, unit})
				delete(r.screenInitPending, client)
			} else if !r.screenInitPending[client] {
				writes = append(writes, screenWrite{client, unit})
			}
		}
	}

	r.mutex.Unlock()

	// Transmit. Writes are serialized through screenWriteMu so they can never
	// race the one-time init write in handleScreenViewer on the same connection
	// (gorilla/websocket panics on concurrent writes).
	r.screenWriteMu.Lock()
	defer r.screenWriteMu.Unlock()
	for _, w := range writes {
		if err := w.conn.WriteMessage(websocket.BinaryMessage, w.data); err != nil {
			log.Printf("Error broadcasting screen in room %s: %v", r.id, err)
			w.conn.Close()
		}
	}
}

// SendScreenKeyframe re-sends the cached screen keyframe (init + first complete
// Cluster) to the requesting viewer. This supports the viewer's recovery path:
// when a viewer's SourceBuffer fails and is rebuilt, it sends a
// {"type":"request_keyframe"} control message and we reply with a fresh
// keyframe so it can re-seed its MediaSource. Without this the viewer would
// wait forever, because the publisher only emits its init once at stream start.
//
// The write is serialized through screenWriteMu so it can never race the
// publisher's BroadcastScreen writes to the same connection.
func (r *Room) SendScreenKeyframe(requester *websocket.Conn) {
	r.mutex.Lock()
	keyframe := r.screenInitSegment
	r.mutex.Unlock()

	if keyframe == nil {
		return
	}

	r.screenWriteMu.Lock()
	defer r.screenWriteMu.Unlock()
	if err := requester.WriteMessage(websocket.BinaryMessage, keyframe); err != nil {
		log.Printf("Error sending screen keyframe in room %s: %v", r.id, err)
	}
}
