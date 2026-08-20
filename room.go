package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
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
	maxPending  int    // safety-valve budget for pending bytes
	pending     []byte // raw publisher bytes not yet emitted
	ready       bool   // keyframe finalized
	keyframe    []byte // init + first complete Cluster (nil until ready)
	initHeader  []byte // init alone (EBML+Segment+Info+Tracks, no Cluster)
	lastCluster []byte // most recent complete Cluster (nil until first emitted)
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
				f.initHeader = append([]byte(nil), f.pending[:c1]...)
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
			unit := flush(c)
			units = append(units, unit)
			f.lastCluster = append(f.lastCluster[:0], unit...)
		}

		// Safety valve: a single Cluster larger than the budget (pathological)
		// is flushed raw rather than buffered forever.
		if len(f.pending) > f.maxPending {
			unit := flush(len(f.pending))
			units = append(units, unit)
			f.lastCluster = append(f.lastCluster[:0], unit...)
		}
	}

	return units, keyframeFinalized
}

// maxScreenSharesPerRoom caps how many people can share their screen in a
// single room at once. It is a hard limit (enforced atomically in
// AddScreenPublisher) but configurable via the SQRLL_MAX_SCREENSHARES_PER_ROOM
// env var, read at startup in main().
var maxScreenSharesPerRoom = 5

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
	// screenPublishers maps each active screen publisher's connection to its
	// per-publisher state. Multiple people may share their screen at once
	// (Discord-style), each with its own relay / init cache / mode / SFU and
	// its own set of viewers (a viewer picks one publisher via ?target=).
	screenPublishers map[*websocket.Conn]*screenPublisher

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

	// voiceSFU is the Pion-based SFU for voice ("sfu" mode). It relays
	// every participant's microphone to every other participant. nil unless
	// at least one participant announced "sfu".
	voiceSFU *VoiceSFU
}

// RoomManager holds the state of the entire microservice
type RoomManager struct {
	rooms map[string]*Room

	/** Mutex for changing anything in rooms structure */
	mutex sync.RWMutex

	/** API Key - Password to allow editing sensitive fields called from other server. */
	APIKey string

	/** Per-IP rate limiters for the file upload / download proxy endpoints. */
	uploadLimiter   *tokenBucketLimiter
	downloadLimiter *tokenBucketLimiter
}

// NewRoomManager is a constructor for our service
func NewRoomManager() *RoomManager {
	return &RoomManager{
		rooms:           make(map[string]*Room),
		uploadLimiter:   newTokenBucketLimiter(uploadRateLimitPerSec, uploadRateLimitBurst),
		downloadLimiter: newTokenBucketLimiter(downloadRateLimitPerSec, downloadRateLimitBurst),
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
		id:               roomID,
		token:            token,
		clients:          make(map[*websocket.Conn]string),
		initSegments:     make(map[*websocket.Conn][]byte),
		audioRelays:      make(map[*websocket.Conn]*webmRelay),
		screenPublishers: make(map[*websocket.Conn]*screenPublisher),
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
	allConns := make([]*websocket.Conn, 0, len(room.clients))
	for conn := range room.clients {
		allConns = append(allConns, conn)
	}
	var sfus []*ScreenSFU
	for _, pub := range room.screenPublishers {
		allConns = append(allConns, pub.conn)
		for vConn := range pub.viewers {
			allConns = append(allConns, vConn)
		}
		if pub.sfu != nil {
			sfus = append(sfus, pub.sfu)
		}
	}
	// Clear maps so deferred LeaveRoom / RemoveScreenViewer are harmless no-ops
	room.clients = make(map[*websocket.Conn]string)
	room.initSegments = make(map[*websocket.Conn][]byte)
	room.audioRelays = make(map[*websocket.Conn]*webmRelay)
	room.screenPublishers = make(map[*websocket.Conn]*screenPublisher)
	vsfu := room.voiceSFU
	room.voiceSFU = nil
	totalClients := len(allConns)
	room.mutex.Unlock()

	// Close the SFUs (Pion PeerConnections) outside the room lock.
	for _, sfu := range sfus {
		sfu.Close()
	}
	if vsfu != nil {
		vsfu.Close()
	}

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
	room, exists := rm.rooms[roomID]
	rm.mutex.Unlock()

	if !exists {
		return
	}

	room.mutex.Lock()
	delete(room.clients, conn)
	delete(room.initSegments, conn)
	delete(room.audioRelays, conn)
	vsfu := room.voiceSFU
	isEmpty := room.isFullyEmpty()
	room.mutex.Unlock()

	// Remove the participant from the VoiceSFU outside the room lock so ICE
	// teardown does not block other room operations.
	if vsfu != nil {
		vsfu.RemoveParticipant(conn)
	}

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
	return len(r.clients) == 0 && len(r.screenPublishers) == 0
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

// screenPublisher tracks a single active screen sharer. Each publisher owns its
// own relay / init cache / signaling mode / SFU and its own viewer set, so
// multiple people can share their screen in the same voice channel at once.
// A viewer connects to exactly one publisher via the ?target= query param.
type screenPublisher struct {
	conn        *websocket.Conn
	userID      string
	mode        string                     // "sfu", "webrtc" or "" (treated as "mse")
	relay       *webmRelay                 // MSE Cluster-aligned relay
	initSegment []byte                     // cached init + first Cluster (MSE late joiners)
	initHeader  []byte                     // init alone (no Cluster) for fresh keyframes
	lastCluster []byte                     // most recent complete Cluster for fresh keyframes
	sfu         *ScreenSFU                 // Pion SFU for "sfu" mode (nil until announced)
	writeMu     sync.Mutex                 // serialize writes to this publisher's conn
	viewers     map[*websocket.Conn]string // viewer conn -> viewer user id
	initPending map[*websocket.Conn]bool   // viewers whose init has not been delivered yet
}

// AddScreenPublisher registers a connection as a screen publisher. Any number
// of publishers may share at once; each receives its own screenPublisher state.
func (r *Room) AddScreenPublisher(conn *websocket.Conn, userId string) *screenPublisher {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Hard limit: reject a new publisher once the room is at capacity. This is
	// enforced under the room lock so the count can never race two simultaneous
	// publishers past the cap.
	if len(r.screenPublishers) >= maxScreenSharesPerRoom {
		fmt.Printf("Screen share limit (%d) reached in room [%s]; rejecting publisher [%s]\n", maxScreenSharesPerRoom, r.id, userId)
		return nil
	}

	pub := &screenPublisher{
		conn:        conn,
		userID:      userId,
		viewers:     make(map[*websocket.Conn]string),
		initPending: make(map[*websocket.Conn]bool),
	}
	r.screenPublishers[conn] = pub
	r.idleTimer.Stop() // Room is active
	fmt.Printf("Screen publisher [%s] started in room [%s] (publishers: %d)\n", userId, r.id, len(r.screenPublishers))
	return pub
}

// getScreenPublisherByUserID returns the publisher registered for a user id, or nil.
func (r *Room) getScreenPublisherByUserID(userID string) *screenPublisher {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	for _, pub := range r.screenPublishers {
		if pub.userID == userID {
			return pub
		}
	}
	return nil
}

// ClearScreenPublisher removes a publisher and returns all of its viewer
// connections (which the caller must close).
func (r *Room) ClearScreenPublisher(pub *screenPublisher) []*websocket.Conn {
	r.mutex.Lock()
	if r.screenPublishers[pub.conn] != pub {
		r.mutex.Unlock()
		return nil
	}
	delete(r.screenPublishers, pub.conn)

	viewers := make([]*websocket.Conn, 0, len(pub.viewers))
	for vConn := range pub.viewers {
		viewers = append(viewers, vConn)
	}
	pub.viewers = make(map[*websocket.Conn]string)
	pub.initPending = make(map[*websocket.Conn]bool)

	sfu := pub.sfu
	pub.sfu = nil
	r.mutex.Unlock()

	if sfu != nil {
		sfu.Close()
	}

	fmt.Printf("Screen publisher [%s] left room [%s], %d viewers disconnected\n", pub.userID, r.id, len(viewers))
	return viewers
}

// CurrentScreenSharePublishers returns the user ids of all active publishers.
func (r *Room) CurrentScreenSharePublishers() []string {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	ids := make([]string, 0, len(r.screenPublishers))
	for _, pub := range r.screenPublishers {
		ids = append(ids, pub.userID)
	}
	return ids
}

// RegisterScreenViewer atomically adds a viewer to a publisher and returns the
// publisher's cached init segment to send first (nil if none yet). The viewer is
// marked "init pending" so BroadcastScreen will not relay media to it until the
// init has been delivered (see MarkScreenInitDelivered).
func (r *Room) RegisterScreenViewer(pub *screenPublisher, conn *websocket.Conn, userId string) []byte {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	pub.viewers[conn] = userId
	pub.initPending[conn] = true
	r.idleTimer.Stop()
	fmt.Printf("Screen viewer [%s] joined room [%s] for publisher [%s]. Viewers: %d\n", userId, r.id, pub.userID, len(pub.viewers))

	if pub.initSegment == nil {
		return nil
	}

	if pub.initHeader != nil && pub.lastCluster != nil {
		initSeg := make([]byte, 0, len(pub.initHeader)+len(pub.lastCluster))
		initSeg = append(initSeg, pub.initHeader...)
		initSeg = append(initSeg, pub.lastCluster...)
		return initSeg
	}

	initSeg := make([]byte, len(pub.initSegment))
	copy(initSeg, pub.initSegment)
	return initSeg
}

// MarkScreenInitDelivered clears the "init pending" gate for a viewer.
func (r *Room) MarkScreenInitDelivered(pub *screenPublisher, conn *websocket.Conn) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	delete(pub.initPending, conn)
}

// RemoveScreenViewer removes a screen viewer connection from a publisher.
func (r *Room) RemoveScreenViewer(pub *screenPublisher, conn *websocket.Conn) {
	r.mutex.Lock()
	userId := pub.viewers[conn]
	delete(pub.viewers, conn)
	delete(pub.initPending, conn)
	remaining := len(pub.viewers)
	mode := pub.mode
	sfu := pub.sfu
	r.mutex.Unlock()

	fmt.Printf("Screen viewer left room [%s] for publisher [%s]. Remaining viewers: %d\n", r.id, pub.userID, remaining)

	if sfu != nil {
		sfu.RemoveViewer(conn)
	}
	if mode != "sfu" && userId != "" {
		pub.notifyViewerLeft(userId)
	}
}

// BroadcastScreen relays a publisher's raw WebM to its viewers via a
// per-publisher webmRelay. Identical to the old single-publisher logic, scoped
// to `pub`.
func (r *Room) BroadcastScreen(pub *screenPublisher, message []byte) {
	r.mutex.Lock()

	relay := pub.relay
	if relay != nil && isEBMLHeader(message) {
		r.mutex.Unlock()
		return
	}
	if relay == nil {
		relay = &webmRelay{maxPending: maxScreenPendingBytes}
		pub.relay = relay
	}

	units, keyframeFinalized := relay.feed(message)

	if keyframeFinalized && relay.keyframe != nil {
		pub.initSegment = append([]byte(nil), relay.keyframe...)
		if relay.initHeader != nil {
			pub.initHeader = append([]byte(nil), relay.initHeader...)
		}
		log.Printf("screenshare init cached: room=%s publisher=%s bytes=%d", r.id, pub.userID, len(pub.initSegment))
	}
	if relay.lastCluster != nil {
		pub.lastCluster = append([]byte(nil), relay.lastCluster...)
	}

	type screenWrite struct {
		conn *websocket.Conn
		data []byte
	}
	var writes []screenWrite
	for i, unit := range units {
		isKeyframe := keyframeFinalized && i == 0
		for client := range pub.viewers {
			if client == pub.conn {
				continue
			}
			if isKeyframe {
				writes = append(writes, screenWrite{client, unit})
				delete(pub.initPending, client)
			} else if !pub.initPending[client] {
				writes = append(writes, screenWrite{client, unit})
			}
		}
	}

	r.mutex.Unlock()

	r.screenWriteMu.Lock()
	defer r.screenWriteMu.Unlock()
	for _, w := range writes {
		if err := w.conn.WriteMessage(websocket.BinaryMessage, w.data); err != nil {
			log.Printf("Error broadcasting screen in room %s: %v", r.id, err)
			w.conn.Close()
		}
	}
}

// SendScreenKeyframe re-sends a publisher's cached keyframe to a requesting viewer.
func (r *Room) SendScreenKeyframe(pub *screenPublisher, requester *websocket.Conn) {
	r.mutex.Lock()
	var keyframe []byte
	if pub.initHeader != nil && pub.lastCluster != nil {
		keyframe = make([]byte, 0, len(pub.initHeader)+len(pub.lastCluster))
		keyframe = append(keyframe, pub.initHeader...)
		keyframe = append(keyframe, pub.lastCluster...)
	} else {
		keyframe = pub.initSegment
	}
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

// --- WebRTC screen-share signaling (per publisher) ---

// setScreenPublisherMode records the signaling mode a publisher announced.
func (r *Room) setScreenPublisherMode(pub *screenPublisher, mode string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	pub.mode = mode
}

// screenPublisherMode returns a publisher's announced mode ("" => "mse").
func (r *Room) screenPublisherMode(pub *screenPublisher) string {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if pub.mode == "" {
		return "mse"
	}
	return pub.mode
}

// getScreenSFU returns a publisher's SFU, or nil.
func (r *Room) getScreenSFU(pub *screenPublisher) *ScreenSFU {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return pub.sfu
}

// ensureScreenSFU creates a publisher's SFU (Pion) if it does not already exist.
func (r *Room) ensureScreenSFU(pub *screenPublisher) {
	r.mutex.Lock()
	if pub.sfu != nil {
		r.mutex.Unlock()
		return
	}
	sfu, err := newScreenSFU(r, pub)
	if err != nil {
		r.mutex.Unlock()
		log.Printf("Failed to create SFU for room %s: %v", r.id, err)
		return
	}
	pub.sfu = sfu
	r.mutex.Unlock()
}

// addSFUViewer registers a viewer with a publisher's SFU (no-op if no SFU yet).
func (r *Room) addSFUViewer(pub *screenPublisher, conn *websocket.Conn, userId string) {
	r.mutex.Lock()
	sfu := pub.sfu
	r.mutex.Unlock()
	if sfu != nil {
		sfu.AddViewer(conn, userId)
	}
}

// setupAllSFUViewers creates SFU offers for every already-connected viewer. Used
// when a publisher announces "sfu" after viewers have already joined.
func (r *Room) setupAllSFUViewers(pub *screenPublisher) {
	type item struct {
		conn *websocket.Conn
		uid  string
	}

	r.mutex.Lock()
	sfu := pub.sfu
	viewers := make([]item, 0, len(pub.viewers))
	for conn, uid := range pub.viewers {
		viewers = append(viewers, item{conn, uid})
	}
	r.mutex.Unlock()

	if sfu == nil {
		return
	}
	for _, v := range viewers {
		sfu.AddViewer(v.conn, v.uid)
	}
}

// sendToViewerByUserID writes a JSON signaling message to one of a publisher's
// viewers (the publisher targets one viewer per RTCPeerConnection in mesh mode).
func (r *Room) sendToViewerByUserID(pub *screenPublisher, userID string, data []byte) {
	r.mutex.Lock()
	var target *websocket.Conn
	for conn, uid := range pub.viewers {
		if uid == userID {
			target = conn
			break
		}
	}
	r.mutex.Unlock()
	if target == nil {
		return
	}

	r.screenWriteMu.Lock()
	defer r.screenWriteMu.Unlock()
	if err := target.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("Error sending signaling to screen viewer %s in room %s: %v", userID, r.id, err)
	}
}

// send writes a JSON signaling message to this publisher, serialized so
// concurrent viewer/SFU goroutines never write to the same connection at once.
func (p *screenPublisher) send(data []byte) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := p.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("Error sending signaling to screen publisher: %v", err)
	}
}

// forward tags the given viewer message with from_userid and relays it to the
// publisher (mesh mode answer/ICE).
func (p *screenPublisher) forward(fromUserID string, raw []byte) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	m["from_userid"] = fromUserID
	out, err := json.Marshal(m)
	if err != nil {
		return
	}
	p.send(out)
}

// notifyViewerJoined tells this publisher a new viewer connected (mesh mode).
func (p *screenPublisher) notifyViewerJoined(userID string) {
	data, _ := json.Marshal(map[string]string{"type": "viewer_joined", "userid": userID})
	p.send(data)
}

// notifyViewerLeft tells this publisher a viewer disconnected (mesh mode).
func (p *screenPublisher) notifyViewerLeft(userID string) {
	data, _ := json.Marshal(map[string]string{"type": "viewer_left", "userid": userID})
	p.send(data)
}

// broadcastModeToViewers pushes a publisher's mode to every connected viewer.
func (r *Room) broadcastModeToViewers(pub *screenPublisher, mode string) {
	data, _ := json.Marshal(map[string]string{"type": "mode", "mode": mode})
	r.mutex.Lock()
	viewers := make([]*websocket.Conn, 0, len(pub.viewers))
	for conn := range pub.viewers {
		viewers = append(viewers, conn)
	}
	r.mutex.Unlock()

	r.screenWriteMu.Lock()
	defer r.screenWriteMu.Unlock()
	for _, conn := range viewers {
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			log.Printf("Error broadcasting mode in room %s: %v", r.id, err)
		}
	}
}

// NotifyViewerMode sends the publisher's current mode to a single viewer.
func (r *Room) NotifyViewerMode(conn *websocket.Conn, mode string) {
	data, _ := json.Marshal(map[string]string{"type": "mode", "mode": mode})
	r.screenWriteMu.Lock()
	defer r.screenWriteMu.Unlock()
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("Error sending mode to screen viewer in room %s: %v", r.id, err)
	}
}

// SendToScreenViewerConn writes a JSON signaling message to a specific screen
// viewer connection. Serialized via screenWriteMu so it can never race the
// publisher's BroadcastScreen writes or another SFU write to the same connection.
func (r *Room) SendToScreenViewerConn(conn *websocket.Conn, data []byte) {
	r.screenWriteMu.Lock()
	defer r.screenWriteMu.Unlock()
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("Error sending signaling to screen viewer in room %s: %v", r.id, err)
	}
}

// BroadcastScreenShareState notifies every audio client in the room that a
// screen share started or stopped. state is "screen_share_started" or
// "screen_share_stopped". Each publisher fires this once with its own user id.
func (r *Room) BroadcastScreenShareState(state, userID string) {
	data, _ := json.Marshal(map[string]string{
		"type":    state,
		"user_id": userID,
	})

	r.mutex.Lock()
	targets := make([]*websocket.Conn, 0, len(r.clients))
	for c, uid := range r.clients {
		if uid == userID {
			continue // don't notify the sharer themselves
		}
		targets = append(targets, c)
	}
	r.mutex.Unlock()

	r.audioWriteMu.Lock()
	defer r.audioWriteMu.Unlock()
	for _, c := range targets {
		if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
			log.Printf("Error broadcasting %s in room %s: %v", state, r.id, err)
		}
	}
}

// NotifyScreenShareState sends a single screen_share_started/stopped
// notification to one audio client (used for late joiners).
func (r *Room) NotifyScreenShareState(conn *websocket.Conn, state, userID string) {
	data, _ := json.Marshal(map[string]string{"type": state, "user_id": userID})
	r.SendToAudioClient(conn, data)
}

// SendToAudioClient writes a JSON signaling message to a specific audio client
// connection. Serialized via audioWriteMu so it can never race the MSE
// Broadcast writes to the same *websocket.Conn.
func (r *Room) SendToAudioClient(conn *websocket.Conn, data []byte) {
	r.audioWriteMu.Lock()
	defer r.audioWriteMu.Unlock()
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("Error sending signaling to audio client in room %s: %v", r.id, err)
	}
}

// EnsureVoiceSFU creates the room's voice SFU (Pion) if it does not already
// exist. Called when a participant announces "sfu" mode.
func (r *Room) EnsureVoiceSFU() {
	r.mutex.Lock()
	if r.voiceSFU != nil {
		r.mutex.Unlock()
		return
	}
	r.voiceSFU = newVoiceSFU(r)
	r.mutex.Unlock()
}

// VoiceSFUHandleOffer forwards a participant's upstream offer to the voice SFU.
func (r *Room) VoiceSFUHandleOffer(conn *websocket.Conn, userID, sdp string) {
	r.mutex.Lock()
	vsfu := r.voiceSFU
	r.mutex.Unlock()
	if vsfu != nil {
		vsfu.HandleOffer(conn, userID, sdp)
	}
}

// VoiceSFUHandleAnswer forwards a participant's downstream answer to the voice SFU.
func (r *Room) VoiceSFUHandleAnswer(conn *websocket.Conn, publisherID, sdp string) {
	r.mutex.Lock()
	vsfu := r.voiceSFU
	r.mutex.Unlock()
	if vsfu != nil {
		vsfu.HandleAnswer(conn, publisherID, sdp)
	}
}

// VoiceSFUHandleUpstreamICE forwards a publisher's upstream ICE candidate.
func (r *Room) VoiceSFUHandleUpstreamICE(conn *websocket.Conn, userID string, candidate webrtc.ICECandidateInit) {
	r.mutex.Lock()
	vsfu := r.voiceSFU
	r.mutex.Unlock()
	if vsfu != nil {
		vsfu.HandleUpstreamICE(conn, userID, candidate)
	}
}

// VoiceSFUHandleSubscriberICE forwards a subscriber's downstream ICE candidate.
func (r *Room) VoiceSFUHandleSubscriberICE(conn *websocket.Conn, publisherID string, candidate webrtc.ICECandidateInit) {
	r.mutex.Lock()
	vsfu := r.voiceSFU
	r.mutex.Unlock()
	if vsfu != nil {
		vsfu.HandleSubscriberICE(conn, publisherID, candidate)
	}
}
