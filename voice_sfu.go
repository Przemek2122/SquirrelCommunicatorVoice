package main

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// voicePublisher is one participant's upstream connection (mic -> SFU).
type voicePublisher struct {
	userID     string
	conn       *websocket.Conn
	pc         *webrtc.PeerConnection
	remote     *webrtc.TrackRemote
	local      *webrtc.TrackLocalStaticRTP
	ready      bool // true once the answer has been sent (ICE can flow)
	icePending []webrtc.ICECandidateInit
}

// voiceSubscriber is one downstream connection (SFU -> one subscriber) carrying
// a single publisher's audio track.
type voiceSubscriber struct {
	conn        *websocket.Conn
	pc          *webrtc.PeerConnection
	publisherID string
	iceReady    bool
	icePending  []webrtc.ICECandidateInit
}

// VoiceSFU relays every participant's microphone to every other participant.
//
// Topology (per participant):
//
//	mic ---(1 upstream PC)---> SFU ---(N-1 downstream PCs)---> peers
//
// Every participant publishes ONE audio stream to the SFU (1x upload) and
// receives one stream per other participant. The SFU forwards RTP packets
// without decode/re-encode, so latency stays sub-second and a participant's
// upload cost stays flat no matter how many people are listening.
//
// Unlike the screen-share SFU (one publisher, many viewers), voice is
// many-to-many, so this is generalized: each participant is both a publisher
// and a subscriber. A fresh downstream PeerConnection is created per
// (subscriber, publisher) pair, which avoids the renegotiation dance a
// single-connection SFU would otherwise require.
type VoiceSFU struct {
	room  *Room
	mutex sync.Mutex

	// publishers maps userID -> the participant's upstream PC + fan-out track.
	publishers map[string]*voicePublisher

	// participants maps a signaling conn -> userID. It is the set of clients
	// currently in SFU mode; fan-out targets are drawn from here.
	participants map[*websocket.Conn]string

	// subscribers maps a subscriber's conn -> (publisherID -> downstream PC).
	subscribers map[*websocket.Conn]map[string]*voiceSubscriber

	closed bool
}

func newVoiceSFU(room *Room) *VoiceSFU {
	return &VoiceSFU{
		room:         room,
		publishers:   make(map[string]*voicePublisher),
		participants: make(map[*websocket.Conn]string),
		subscribers:  make(map[*websocket.Conn]map[string]*voiceSubscriber),
	}
}

// sendTo serializes obj as JSON and writes it to a participant's signaling
// connection. Writes are serialized through the room's audioWriteMu so they
// can never race the MSE Broadcast path on the same *websocket.Conn.
func (s *VoiceSFU) sendTo(conn *websocket.Conn, obj map[string]interface{}) {
	data, err := json.Marshal(obj)
	if err != nil {
		return
	}
	s.room.SendToAudioClient(conn, data)
}

// registerParticipant adds a conn -> userID mapping so future publishers fan
// out to it. Idempotent.
func (s *VoiceSFU) registerParticipant(conn *websocket.Conn, userID string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.closed {
		return
	}
	s.participants[conn] = userID
	if s.subscribers[conn] == nil {
		s.subscribers[conn] = make(map[string]*voiceSubscriber)
	}
}

// HandleOffer processes a participant's upstream offer (their microphone) and
// answers it. The participant is registered as a fan-out target, and any
// already-known publishers are offered to this newly-joined participant.
//
// Correctness note: registration here and track-arrival in onPublisherTrack are
// both mutating scans performed under s.mutex. Whichever happens second always
// sees the other's state, so every (publisher -> participant) pair gets exactly
// one offer (sendOfferTo dedupes by connection).
func (s *VoiceSFU) HandleOffer(conn *websocket.Conn, userID, sdp string) {
	s.registerParticipant(conn, userID)

	s.mutex.Lock()
	if s.closed {
		s.mutex.Unlock()
		return
	}
	if _, exists := s.publishers[userID]; exists {
		// Same userID already publishing (e.g. same account on two devices) —
		// keep the first publisher; this connection still becomes a subscriber.
		s.mutex.Unlock()
		s.fanOutExistingTo(conn, userID)
		return
	}
	pub := &voicePublisher{userID: userID, conn: conn}
	s.publishers[userID] = pub
	s.mutex.Unlock()

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: sfuICEServers})
	if err != nil {
		log.Printf("[voice-sfu] publisher PC create failed in room %s: %v", s.room.id, err)
		s.removePublisher(userID)
		return
	}
	pub.pc = pc

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		s.mutex.Lock()
		if !pub.ready {
			pub.icePending = append(pub.icePending, init)
			s.mutex.Unlock()
			return
		}
		s.mutex.Unlock()
		s.sendTo(conn, map[string]interface{}{"type": "ice", "candidate": init})
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		s.onPublisherTrack(pub, track)
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			log.Printf("[voice-sfu] publisher %s connection state=%s in room %s", userID, state, s.room.id)
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		log.Printf("[voice-sfu] publisher SetRemoteDescription failed in room %s: %v", s.room.id, err)
		s.removePublisher(userID)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Printf("[voice-sfu] publisher CreateAnswer failed in room %s: %v", s.room.id, err)
		s.removePublisher(userID)
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		log.Printf("[voice-sfu] publisher SetLocalDescription failed in room %s: %v", s.room.id, err)
		s.removePublisher(userID)
		return
	}
	s.sendTo(conn, map[string]interface{}{"type": "answer", "sdp": answer.SDP})

	// The answer is on its way; flush any ICE candidates gathered while it was
	// being created so the browser never receives a candidate before the answer.
	s.mutex.Lock()
	pub.ready = true
	pending := pub.icePending
	pub.icePending = nil
	s.mutex.Unlock()
	for _, cand := range pending {
		s.sendTo(conn, map[string]interface{}{"type": "ice", "candidate": cand})
	}

	// Fan out already-known publishers to this newly-joined participant.
	s.fanOutExistingTo(conn, userID)
}

// onPublisherTrack is called when a publisher's audio track arrives. It creates
// the shared local fan-out track and offers it to every other participant.
func (s *VoiceSFU) onPublisherTrack(pub *voicePublisher, track *webrtc.TrackRemote) {
	local, err := webrtc.NewTrackLocalStaticRTP(track.Codec().RTPCodecCapability, "audio", pub.userID)
	if err != nil {
		log.Printf("[voice-sfu] NewTrackLocalStaticRTP failed in room %s: %v", s.room.id, err)
		return
	}

	type target struct {
		conn *websocket.Conn
		uid  string
	}

	s.mutex.Lock()
	pub.remote = track
	pub.local = local
	var targets []target
	for conn, uid := range s.participants {
		if uid == pub.userID {
			continue
		}
		targets = append(targets, target{conn, uid})
	}
	s.mutex.Unlock()

	log.Printf("[voice-sfu] publisher %s track arrived in room %s: %s pt=%d",
		pub.userID, s.room.id, track.Codec().MimeType, track.PayloadType())

	go s.relay(pub, local)

	for _, t := range targets {
		s.sendOfferTo(t.conn, t.uid, pub.userID, local)
	}
}

// relay reads RTP packets from a publisher and fans them out through the shared
// local track. WriteRTP rewrites SSRC + payload type per subscriber binding.
func (s *VoiceSFU) relay(pub *voicePublisher, local *webrtc.TrackLocalStaticRTP) {
	buf := make([]byte, 1500)
	pkt := &rtp.Packet{}
	for {
		n, _, err := pub.remote.Read(buf)
		if err != nil {
			log.Printf("[voice-sfu] publisher %s RTP read ended in room %s: %v", pub.userID, s.room.id, err)
			return
		}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			log.Printf("[voice-sfu] RTP unmarshal failed in room %s: %v", s.room.id, err)
			return
		}
		// RTP header extension IDs differ between publisher<->SFU and
		// SFU<->subscriber negotiations; they must not be forwarded verbatim.
		pkt.Header.Extension = false
		pkt.Header.Extensions = nil
		if err := local.WriteRTP(pkt); err != nil {
			// One subscriber may have failed; keep serving the others.
			log.Printf("[voice-sfu] relay write error in room %s: %v", s.room.id, err)
		}
	}
}

// fanOutExistingTo offers every already-known publisher (whose track has
// arrived) to a newly-joined participant.
func (s *VoiceSFU) fanOutExistingTo(conn *websocket.Conn, newUserID string) {
	type pubInfo struct {
		userID string
		local  *webrtc.TrackLocalStaticRTP
	}

	s.mutex.Lock()
	var pubs []pubInfo
	for uid, p := range s.publishers {
		if uid == newUserID {
			continue
		}
		if p.local != nil {
			pubs = append(pubs, pubInfo{uid, p.local})
		}
	}
	s.mutex.Unlock()

	for _, p := range pubs {
		s.sendOfferTo(conn, newUserID, p.userID, p.local)
	}
}

// sendOfferTo creates a downstream PeerConnection carrying a single publisher's
// audio track and sends an offer to the subscriber.
func (s *VoiceSFU) sendOfferTo(conn *websocket.Conn, subscriberID, publisherID string, local *webrtc.TrackLocalStaticRTP) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: sfuICEServers})
	if err != nil {
		log.Printf("[voice-sfu] subscriber PC create failed in room %s: %v", s.room.id, err)
		return
	}

	sub := &voiceSubscriber{conn: conn, pc: pc, publisherID: publisherID}

	s.mutex.Lock()
	if s.closed {
		s.mutex.Unlock()
		_ = pc.Close()
		return
	}
	if s.subscribers[conn] == nil {
		s.subscribers[conn] = make(map[string]*voiceSubscriber)
	}
	if _, exists := s.subscribers[conn][publisherID]; exists {
		s.mutex.Unlock()
		_ = pc.Close()
		return
	}
	s.subscribers[conn][publisherID] = sub
	s.mutex.Unlock()

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		s.mutex.Lock()
		if !sub.iceReady {
			sub.icePending = append(sub.icePending, init)
			s.mutex.Unlock()
			return
		}
		s.mutex.Unlock()
		s.sendTo(conn, map[string]interface{}{"type": "ice", "candidate": init, "userid": publisherID})
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed ||
			state == webrtc.PeerConnectionStateClosed ||
			state == webrtc.PeerConnectionStateDisconnected {
			s.removeSubscriber(conn, publisherID)
		}
	})

	if _, err := pc.AddTrack(local); err != nil {
		log.Printf("[voice-sfu] subscriber AddTrack failed in room %s: %v", s.room.id, err)
		s.removeSubscriber(conn, publisherID)
		return
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		log.Printf("[voice-sfu] subscriber CreateOffer failed in room %s: %v", s.room.id, err)
		s.removeSubscriber(conn, publisherID)
		return
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		log.Printf("[voice-sfu] subscriber SetLocalDescription failed in room %s: %v", s.room.id, err)
		s.removeSubscriber(conn, publisherID)
		return
	}
	s.sendTo(conn, map[string]interface{}{"type": "offer", "sdp": offer.SDP, "userid": publisherID})

	// The offer is on its way; flush any ICE candidates gathered while it was
	// being created so the browser never receives a candidate before the offer.
	s.mutex.Lock()
	sub.iceReady = true
	pending := sub.icePending
	sub.icePending = nil
	s.mutex.Unlock()
	for _, cand := range pending {
		s.sendTo(conn, map[string]interface{}{"type": "ice", "candidate": cand, "userid": publisherID})
	}
}

// HandleAnswer sets a subscriber's answer for the downstream PC that carries
// the given publisher's audio.
func (s *VoiceSFU) HandleAnswer(conn *websocket.Conn, publisherID, sdp string) {
	s.mutex.Lock()
	var sub *voiceSubscriber
	if m := s.subscribers[conn]; m != nil {
		sub = m[publisherID]
	}
	s.mutex.Unlock()
	if sub == nil || sdp == "" {
		return
	}
	if err := sub.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		log.Printf("[voice-sfu] subscriber SetRemoteDescription(answer) failed in room %s: %v", s.room.id, err)
	}
}

// HandleUpstreamICE adds a trickled ICE candidate from a publisher's upstream PC.
func (s *VoiceSFU) HandleUpstreamICE(conn *websocket.Conn, userID string, candidate webrtc.ICECandidateInit) {
	s.mutex.Lock()
	pub := s.publishers[userID]
	s.mutex.Unlock()
	if pub == nil || pub.pc == nil {
		return
	}
	if err := pub.pc.AddICECandidate(candidate); err != nil {
		log.Printf("[voice-sfu] publisher AddICECandidate failed in room %s: %v", s.room.id, err)
	}
}

// HandleSubscriberICE adds a trickled ICE candidate from a subscriber's
// downstream PC that carries the given publisher's audio.
func (s *VoiceSFU) HandleSubscriberICE(conn *websocket.Conn, publisherID string, candidate webrtc.ICECandidateInit) {
	s.mutex.Lock()
	var sub *voiceSubscriber
	if m := s.subscribers[conn]; m != nil {
		sub = m[publisherID]
	}
	s.mutex.Unlock()
	if sub == nil || sub.pc == nil {
		return
	}
	if err := sub.pc.AddICECandidate(candidate); err != nil {
		log.Printf("[voice-sfu] subscriber AddICECandidate failed in room %s: %v", s.room.id, err)
	}
}

// removeSubscriber closes and forgets a single downstream PC.
func (s *VoiceSFU) removeSubscriber(conn *websocket.Conn, publisherID string) {
	s.mutex.Lock()
	var sub *voiceSubscriber
	if m := s.subscribers[conn]; m != nil {
		sub = m[publisherID]
		delete(m, publisherID)
	}
	s.mutex.Unlock()
	if sub != nil && sub.pc != nil {
		_ = sub.pc.Close()
	}
}

// removePublisher closes and forgets a participant's upstream publisher state.
func (s *VoiceSFU) removePublisher(userID string) {
	s.mutex.Lock()
	pub := s.publishers[userID]
	delete(s.publishers, userID)
	s.mutex.Unlock()
	if pub != nil && pub.pc != nil {
		_ = pub.pc.Close()
	}
}

// RemoveParticipant tears down everything related to a disconnected signaling
// connection: their upstream publisher, their own downstream subscriptions, and
// every other participant's downstream PC carrying this participant's audio. It
// then notifies the remaining participants so their UI can drop the audio
// element immediately.
func (s *VoiceSFU) RemoveParticipant(conn *websocket.Conn) {
	s.mutex.Lock()
	userID := s.participants[conn]
	delete(s.participants, conn)

	var pub *voicePublisher
	if userID != "" {
		pub = s.publishers[userID]
		delete(s.publishers, userID)
	}

	ownSubs := s.subscribers[conn]
	delete(s.subscribers, conn)

	var staleSubs []*voiceSubscriber
	if userID != "" {
		for otherConn, subs := range s.subscribers {
			if otherConn == conn {
				continue
			}
			if sub := subs[userID]; sub != nil {
				delete(subs, userID)
				staleSubs = append(staleSubs, sub)
			}
		}
	}

	remaining := make([]*websocket.Conn, 0, len(s.participants))
	for c := range s.participants {
		remaining = append(remaining, c)
	}
	s.mutex.Unlock()

	if pub != nil && pub.pc != nil {
		_ = pub.pc.Close()
	}
	if ownSubs != nil {
		for _, sub := range ownSubs {
			if sub.pc != nil {
				_ = sub.pc.Close()
			}
		}
	}
	for _, sub := range staleSubs {
		if sub.pc != nil {
			_ = sub.pc.Close()
		}
	}
	if userID != "" {
		msg, _ := json.Marshal(map[string]string{"type": "participant_left", "userid": userID})
		for _, c := range remaining {
			s.room.SendToAudioClient(c, msg)
		}
	}
}

// Close tears down the publisher and all subscriber PeerConnections.
func (s *VoiceSFU) Close() {
	s.mutex.Lock()
	if s.closed {
		s.mutex.Unlock()
		return
	}
	s.closed = true
	pubs := make([]*voicePublisher, 0, len(s.publishers))
	for _, p := range s.publishers {
		pubs = append(pubs, p)
	}
	subs := make([]*voiceSubscriber, 0)
	for _, m := range s.subscribers {
		for _, sub := range m {
			subs = append(subs, sub)
		}
	}
	s.publishers = make(map[string]*voicePublisher)
	s.participants = make(map[*websocket.Conn]string)
	s.subscribers = make(map[*websocket.Conn]map[string]*voiceSubscriber)
	s.mutex.Unlock()

	for _, p := range pubs {
		if p.pc != nil {
			_ = p.pc.Close()
		}
	}
	for _, sub := range subs {
		if sub.pc != nil {
			_ = sub.pc.Close()
		}
	}
}
