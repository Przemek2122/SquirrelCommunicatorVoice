package main

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WebM/EBML init segment magic bytes (1A 45 DF A3).
const (
	ebmlMagic0 = 0x1A
	ebmlMagic1 = 0x45
	ebmlMagic2 = 0xDF
	ebmlMagic3 = 0xA3

	// initScanWindow is how many leading bytes we scan when looking for the
	// EBML magic. The init segment always begins with these bytes, so they are
	// expected near the start of the first publisher message (some browsers may
	// emit them at a small non-zero offset). Limiting the scan avoids false
	// positives deep inside large media frames.
	initScanWindow = 4096
)

// findInitOffset returns the byte offset of the EBML init magic within message,
// or -1 if the magic is not found within the scan window.
func findInitOffset(message []byte) int {
	limit := len(message)
	if limit > initScanWindow {
		limit = initScanWindow
	}
	for i := 0; i+4 <= limit; i++ {
		if message[i] == ebmlMagic0 &&
			message[i+1] == ebmlMagic1 &&
			message[i+2] == ebmlMagic2 &&
			message[i+3] == ebmlMagic3 {
			return i
		}
	}
	return -1
}

// WebM element IDs (raw byte values; WebM IDs are at most 4 bytes).
const (
	webmIDEBML       = 0x1A45DFA3
	webmIDSegment    = 0x18538067
	webmIDTracks     = 0x1654AE6B
	webmIDCluster    = 0x1F43B675
	webmIDTrackEntry = 0xAE
	webmIDCodecID    = 0x86
)

// readVint reads an EBML variable-length integer at pos. It returns the number
// of bytes consumed, the decoded value, whether it encodes EBML's "unknown
// size" (all value bits set), and whether it was valid/complete.
func readVint(data []byte, pos int) (length int, value uint64, unknown bool, ok bool) {
	if pos >= len(data) {
		return 0, 0, false, false
	}
	first := data[pos]
	var l int
	switch {
	case first&0x80 != 0:
		l = 1
	case first&0x40 != 0:
		l = 2
	case first&0x20 != 0:
		l = 3
	case first&0x10 != 0:
		l = 4
	case first&0x08 != 0:
		l = 5
	case first&0x04 != 0:
		l = 6
	case first&0x02 != 0:
		l = 7
	case first&0x01 != 0:
		l = 8
	default:
		return 0, 0, false, false
	}
	if pos+l > len(data) {
		return 0, 0, false, false
	}

	firstBits := 8 - l
	var val uint64
	val = uint64(first) & ((uint64(1) << uint(firstBits)) - 1)
	for i := 1; i < l; i++ {
		val = (val << 8) | uint64(data[pos+i])
	}

	// Unknown size: every value bit is 1.
	unknown = true
	firstMask := uint64(0xFF) >> uint(l)
	if uint64(first)&firstMask != firstMask {
		unknown = false
	}
	for i := 1; i < l && unknown; i++ {
		if data[pos+i] != 0xFF {
			unknown = false
		}
	}

	return l, val, unknown, true
}

// readID reads a WebM element ID (a VINT of at most 4 bytes) and returns its
// raw bytes as an unsigned integer plus the number of bytes consumed.
func readID(data []byte, pos int) (length int, id uint32, ok bool) {
	if pos >= len(data) {
		return 0, 0, false
	}
	first := data[pos]
	var l int
	switch {
	case first&0x80 != 0:
		l = 1
	case first&0x40 != 0:
		l = 2
	case first&0x20 != 0:
		l = 3
	case first&0x10 != 0:
		l = 4
	default:
		return 0, 0, false // WebM IDs are at most 4 bytes
	}
	if pos+l > len(data) {
		return 0, 0, false
	}
	var idv uint32
	for i := 0; i < l; i++ {
		idv = (idv << 8) | uint32(data[pos+i])
	}
	return l, idv, true
}

// parseWebMInit walks the EBML structure of an accumulated buffer and locates
// the init/media boundary. It returns:
//
//	complete - true only when the first Cluster element is reached and a codec
//	           was already found in Tracks. Everything before that Cluster is a
//	           complete init (EBML + Segment + Info + Tracks).
//	codec    - the raw CodecID string (e.g. "V_VP8") or "".
//	initEnd  - byte offset where media (the first Cluster) begins.
//
// A complete init is confirmed ONLY at a Cluster boundary. Without one the
// buffer may still be a truncated init (the codec string can appear before the
// rest of the Tracks element, e.g. the Video sub-element), so we return
// complete=false and the caller must keep accumulating.
func parseWebMInit(data []byte) (complete bool, codec string, initEnd int) {
	n := len(data)
	pos := 0

	// 1. EBML header.
	l, id, ok := readID(data, pos)
	if !ok || id != webmIDEBML {
		return false, "", 0
	}
	pos += l
	l, sz, unk, ok := readVint(data, pos)
	if !ok || unk {
		return false, "", 0
	}
	pos += l
	if sz > uint64(n) {
		return false, "", 0
	}
	ebmlEnd := pos + int(sz)
	if ebmlEnd > n {
		return false, "", 0
	}
	pos = ebmlEnd

	// 2. Segment.
	l, id, ok = readID(data, pos)
	if !ok || id != webmIDSegment {
		return false, "", 0
	}
	pos += l
	l, sz, unk, ok = readVint(data, pos)
	if !ok {
		return false, "", 0
	}
	pos += l
	// Segment size is usually "unknown" in a live stream; clamp to what we have
	// received so far. If the declared size is larger than what we have, walk
	// the bytes we DO have rather than bailing out (a complete init may still be
	// fully contained within them).
	segEnd := n
	if !unk && sz <= uint64(n) && pos+int(sz) < n {
		segEnd = pos + int(sz)
	}

	// 3. Walk Segment children until the first Cluster.
	for pos < segEnd {
		l, id, ok := readID(data, pos)
		if !ok {
			return false, "", 0
		}
		if id == webmIDCluster {
			// Reaching the first Cluster marks the end of the init, but a
			// decodable init must have a codec (found in the Tracks element).
			// Without one, this is not a usable init segment — do NOT report
			// it complete, or we'll cache + broadcast a codec-less init that
			// the viewer can never decode.
			if codec != "" {
				return true, codec, pos
			}
			return false, "", 0
		}
		pos += l
		l, sz, unk, ok := readVint(data, pos)
		if !ok {
			return false, "", 0
		}
		pos += l
		if unk {
			if id == webmIDTracks {
				codec = findCodecInTracks(data, pos, segEnd)
			}
			pos = segEnd
			break
		}
		if sz > uint64(n) {
			return false, "", 0
		}
		dataEnd := pos + int(sz)
		if dataEnd > n {
			return false, "", 0 // element extends past buffer -> truncated
		}
		if id == webmIDTracks {
			codec = findCodecInTracks(data, pos, dataEnd)
		}
		pos = dataEnd
	}

	// Reaching the end of the buffer without a Cluster means the init is still
	// truncated or split across messages. We never report a codec-only buffer as
	// complete -- doing so would cache a truncated init (missing the Video
	// sub-element for video tracks) that the viewer's SourceBuffer rejects.
	return false, "", 0
}

// findCodecInTracks walks the children of a Tracks element and returns the
// CodecID string of the first TrackEntry, or "".
func findCodecInTracks(data []byte, start, end int) string {
	pos := start
	for pos < end {
		l, id, ok := readID(data, pos)
		if !ok {
			return ""
		}
		if id == webmIDTrackEntry {
			pos += l
			l, sz, unk, ok := readVint(data, pos)
			if !ok {
				return ""
			}
			pos += l
			teEnd := end
			if !unk {
				if sz > uint64(end) {
					return ""
				}
				teEnd = pos + int(sz)
				if teEnd > end {
					return "" // TrackEntry truncated
				}
			}
			if c := findCodecInTrackEntry(data, pos, teEnd); c != "" {
				return c
			}
			pos = teEnd
			continue
		}
		pos += l
		l, sz, unk, ok := readVint(data, pos)
		if !ok {
			return ""
		}
		pos += l
		if unk {
			break
		}
		if sz > uint64(end) {
			return ""
		}
		pos += int(sz)
		if pos > end {
			return ""
		}
	}
	return ""
}

// findCodecInTrackEntry walks the children of a TrackEntry element and returns
// the ASCII value of the CodecID element (0x86), or "".
func findCodecInTrackEntry(data []byte, start, end int) string {
	pos := start
	for pos < end {
		l, id, ok := readID(data, pos)
		if !ok {
			return ""
		}
		if id == webmIDCodecID {
			pos += l
			l, sz, unk, ok := readVint(data, pos)
			if !ok || unk {
				return ""
			}
			pos += l
			if sz > uint64(end) {
				return ""
			}
			codecEnd := pos + int(sz)
			if codecEnd > end {
				return "" // codec value truncated
			}
			return string(data[pos:codecEnd])
		}
		pos += l
		l, sz, unk, ok := readVint(data, pos)
		if !ok {
			return ""
		}
		pos += l
		if unk {
			break
		}
		if sz > uint64(end) {
			return ""
		}
		pos += int(sz)
		if pos > end {
			return ""
		}
	}
	return ""
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

	/** Timer for room delete when empty */
	idleTimer *time.Timer

	// --- Screen share fields ---
	screenPublisher   *websocket.Conn            // Only one publisher per room
	screenViewers     map[*websocket.Conn]string // Viewers receiving the screen stream
	screenInitSegment []byte                     // Cached VP8/VP9 init segment for late joiners
	screenInitBuffer  []byte                     // Accumulating publisher bytes until a COMPLETE init is found
	screenInitReady   bool                       // true once screenInitSegment is a complete init

	// screenInitPending tracks viewers whose init segment has NOT yet been
	// delivered. Media frames must never reach a viewer before its init, so
	// BroadcastScreen skips any viewer still marked here; the mark is cleared
	// once the init has actually been written to that viewer.
	screenInitPending map[*websocket.Conn]bool

	// screenWriteMu serializes ALL writes to screen viewer connections.
	//
	// Without this, the one-time init write in handleScreenViewer can race a
	// media write from the publisher's BroadcastScreen goroutine on the SAME
	// *websocket.Conn. gorilla/websocket panics with
	// "concurrent write to websocket connection" when two goroutines write at
	// once, so every screen write must go through this mutex.
	screenWriteMu sync.Mutex

	// audioWriteMu serializes ALL writes to audio client connections, for
	// the same reason as screenWriteMu: two different senders' Broadcast
	// goroutines can target the SAME *websocket.Conn at once (two people
	// speaking, or a user rejoining while another is mid-stream), and
	// gorilla/websocket panics with "concurrent write to websocket
	// connection" on concurrent writes. The screen path is protected by
	// screenWriteMu; the audio path was not.
	audioWriteMu sync.Mutex
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
	room.screenPublisher = nil
	room.screenViewers = make(map[*websocket.Conn]string)
	room.screenInitSegment = nil
	room.screenInitBuffer = nil
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

	// Is token correct. Read under the room lock — SetToken writes
	// r.token under the same lock, so an unlocked read here is a data race.
	room.mutex.Lock()
	tokenOK := room.token == token
	room.mutex.Unlock()
	if !tokenOK {
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

	room.mutex.Lock()
	tokenOK := room.token == token
	room.mutex.Unlock()
	if !tokenOK {
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
	// If it's an initialization chunk, cache the FINAL message (init + trailing
	// media) so new clients joining later can properly decode this user's stream.
	//
	// We deliberately cache the ENTIRE first chunk rather than a stripped "pure
	// init". Appending a pure init (with no following Cluster) to a SourceBuffer
	// in 'sequence' mode made Chrome fire a SourceBuffer error and transition the
	// MediaSource to 'ended' (this made audio fail on EVERY join). Appending the
	// init together with the first media is what Chrome accepts, and 'sequence'
	// append mode handles timestamp continuity for the trailing media.
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

	// 5. Safely transmit the constructed package to all targeted clients.
	// Writes are serialized through audioWriteMu so two concurrent Broadcast
	// calls (different senders) can never write to the SAME target connection
	// at once — gorilla/websocket panics with "concurrent write to websocket
	// connection" on concurrent writes (the screen path has screenWriteMu for
	// exactly this reason; the audio path was missing the equivalent guard).
	r.audioWriteMu.Lock()
	defer r.audioWriteMu.Unlock()
	for _, client := range targets {
		err := client.WriteMessage(websocket.BinaryMessage, finalMessage)
		if err != nil {
			log.Printf("Error broadcasting to a client in room %s: %v", r.id, err)

			// Remove the dead connection immediately so we stop trying to write
			// to it on every broadcast. The caller's deferred LeaveRoom will
			// still fire, but the delete here is idempotent and keeps the map
			// accurate for clients whose read loop has not yet noticed the drop.
			r.mutex.Lock()
			delete(r.clients, client)
			r.mutex.Unlock()
			client.Close()
		}
	}
}

// SendKeyframe sends the cached init segment for the given target user to the
// requester connection. Used to recover a viewer whose SourceBuffer errored:
// the frontend requests a fresh init ("request_keyframe") and we reply with the
// cached init so it can re-seed. This matters because the audio init is only
// sent once per recording session, so without this a viewer that rejects the
// init has no way to recover.
func (r *Room) SendKeyframe(requester *websocket.Conn, targetUserId string) {
	r.mutex.Lock()
	var initPayload []byte
	for client, uid := range r.clients {
		if uid == targetUserId {
			if seg, ok := r.initSegments[client]; ok {
				initPayload = make([]byte, len(seg))
				copy(initPayload, seg)
			}
			break
		}
	}
	r.mutex.Unlock()

	if initPayload == nil {
		return
	}

	r.audioWriteMu.Lock()
	defer r.audioWriteMu.Unlock()
	if err := requester.WriteMessage(websocket.BinaryMessage, initPayload); err != nil {
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
	// Gate media until an init is delivered first — but only when an init is
	// actually available or still being assembled. After the 1MB "relay raw
	// frames" fallback (screenInitReady == true but screenInitSegment == nil)
	// there will never be an init, so leaving the viewer pending would starve
	// it forever (BroadcastScreen would skip it on every media frame).
	if r.screenInitSegment != nil || !r.screenInitReady {
		r.screenInitPending[conn] = true
	}
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
// It accumulates publisher bytes until a COMPLETE init segment can be extracted
// using a real EBML element walker (everything up to the first Cluster element
// at an element boundary), caches that for late joiners, and enforces
// init-before-media ordering: media frames are only relayed to viewers whose
// init has already been delivered.
func (r *Room) BroadcastScreen(sender *websocket.Conn, message []byte) {
	r.mutex.Lock()

	var completeInit []byte
	var trailingMedia []byte

	if !r.screenInitReady {
		// A real init begins with the EBML magic at offset 0 (the publisher
		// always sends it as the first chunk). We only start/restart
		// accumulation when:
		//   - we have not started accumulating yet, or
		//   - the message itself begins with the EBML magic (a fresh/re-sent
		//     init). Otherwise we treat the message as a continuation and append
		//     it, so a false-positive match deep inside a media frame can never
		//     discard an init that is already being accumulated.
		startsWithEBML := len(message) >= 4 &&
			message[0] == ebmlMagic0 &&
			message[1] == ebmlMagic1 &&
			message[2] == ebmlMagic2 &&
			message[3] == ebmlMagic3

		if len(r.screenInitBuffer) == 0 {
			// First accumulation: locate the magic (allow a small junk prefix).
			if offset := findInitOffset(message); offset >= 0 {
				r.screenInitBuffer = append([]byte(nil), message[offset:]...)
			}
		} else if startsWithEBML {
			// Fresh/re-sent init: restart accumulation from offset 0.
			r.screenInitBuffer = append([]byte(nil), message...)
		} else {
			// Continuation of the current init.
			r.screenInitBuffer = append(r.screenInitBuffer, message...)
		}

		if len(r.screenInitBuffer) > 0 {
			if complete, codec, initEnd := parseWebMInit(r.screenInitBuffer); complete {
				r.screenInitSegment = make([]byte, initEnd)
				copy(r.screenInitSegment, r.screenInitBuffer[:initEnd])
				if initEnd < len(r.screenInitBuffer) {
					trailingMedia = append([]byte(nil), r.screenInitBuffer[initEnd:]...)
				}
				r.screenInitReady = true
				r.screenInitBuffer = nil
				completeInit = r.screenInitSegment
				log.Printf("screenshare init cached: room=%s bytes=%d codec=%s", r.id, len(r.screenInitSegment), codec)
			} else if len(r.screenInitBuffer) > 1<<20 {
				// Safety valve: 1MB accumulated without a complete init. The
				// stream is undecodable; give up caching and relay raw frames
				// (the publisher re-broadcasts its init every 2s, so viewers can
				// still recover on their own).
				r.screenInitReady = true
				r.screenInitBuffer = nil
				log.Printf("screenshare init never identified in room=%s -- relaying raw frames", r.id)
			}
		}

		if !r.screenInitReady {
			r.mutex.Unlock()
			return
		}
	}

	// Build the ordered list of (connection, payload) writes under the room
	// lock. The init payload is delivered to EVERY viewer and clears its
	// init-pending gate; media payloads are delivered only to viewers whose gate
	// is already clear (init delivered first).
	type screenWrite struct {
		conn *websocket.Conn
		data []byte
	}
	var writes []screenWrite
	if completeInit != nil {
		for client := range r.screenViewers {
			if client == sender {
				continue
			}
			writes = append(writes, screenWrite{client, completeInit})
			delete(r.screenInitPending, client)
		}
		if len(trailingMedia) > 0 {
			for client := range r.screenViewers {
				if client == sender {
					continue
				}
				writes = append(writes, screenWrite{client, trailingMedia})
			}
		}
	} else {
		for client := range r.screenViewers {
			if client == sender {
				continue
			}
			if !r.screenInitPending[client] {
				writes = append(writes, screenWrite{client, message})
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
		err := w.conn.WriteMessage(websocket.BinaryMessage, w.data)
		if err != nil {
			log.Printf("Error broadcasting screen in room %s: %v", r.id, err)

			// Close the dead connection; the caller's deferred RemoveScreenViewer
			// will handle removing it from the viewer map and idle timer logic.
			w.conn.Close()
		}
	}
}
