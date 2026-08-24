package main

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// STUN servers for the SFU. The SFU is normally a public server (VPS), so host
// candidates suffice; STUN is a harmless fallback if the SFU ever sits behind a
// NAT. TURN is deliberately omitted: the SFU has a public address.
var sfuICEServers = []webrtc.ICEServer{
	{URLs: []string{"stun:stun.l.google.com:19302", "stun:stun1.l.google.com:19302"}},
}

// screenShareMediaEngine returns a MediaEngine restricted to VP8 (plus its RTX
// retransmission codec). VP8 is the one video codec that BOTH Chrome and
// Chromium always support for getDisplayMedia, so pinning the screen SFU to VP8
// removes the directional interop failures that occur when a publisher's
// preferred codec (e.g. Chromium's VP9/AV1, or Chrome's H264) does not line up
// with what the SFU would otherwise re-offer to a viewer. The publisher-facing
// and viewer-facing PeerConnections of the screen SFU both use this engine, so
// the relayed codec is always VP8 regardless of browser.
func screenShareMediaEngine() (*webrtc.MediaEngine, error) {
	m := &webrtc.MediaEngine{}

	videoRTCPFeedback := []webrtc.RTCPFeedback{
		{Type: "goog-remb", Parameter: ""},
		{Type: "ccm", Parameter: "fir"},
		{Type: "nack", Parameter: ""},
		{Type: "nack", Parameter: "pli"},
	}

	for _, codec := range []webrtc.RTPCodecParameters{
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeVP8,
				ClockRate:    90000,
				Channels:     0,
				SDPFmtpLine:  "",
				RTCPFeedback: videoRTCPFeedback,
			},
			PayloadType: 96,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeRTX,
				ClockRate:    90000,
				Channels:     0,
				SDPFmtpLine:  "apt=96",
				RTCPFeedback: nil,
			},
			PayloadType: 97,
		},
	} {
		if err := m.RegisterCodec(codec, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
	}

	return m, nil
}

// sfuViewer is a single viewer's PeerConnection on the SFU.
type sfuViewer struct {
	pc         *webrtc.PeerConnection
	offerSent  bool
	iceReady   bool
	icePending []webrtc.ICECandidateInit
}

// ScreenSFU relays one publisher's screen-share video track to many viewers.
//
// Topology:
//
//	publisher ---(1 x RTCPeerConnection)---> SFU ---(N x RTCPeerConnection)---> viewer
//
// The publisher sends ONE stream to the SFU; the SFU forwards the RTP packets
// (no decode/re-encode) to every viewer. This keeps the publisher's upload at
// 1x bitrate regardless of the number of viewers, and moves the fan-out cost
// onto the server (which has the bandwidth for it).
type ScreenSFU struct {
	room  *Room
	pub   *screenPublisher
	mutex sync.Mutex

	publisherPC *webrtc.PeerConnection
	remoteTrack *webrtc.TrackRemote
	localTrack  *webrtc.TrackLocalStaticRTP

	// Outgoing publisher ICE candidates are buffered until the answer has been
	// sent, so the browser never receives a candidate before the remote
	// description it belongs to.
	publisherReady      bool
	publisherICEPending []webrtc.ICECandidateInit

	viewers map[*websocket.Conn]*sfuViewer
	closed  bool
}

// newScreenSFU creates the SFU and its publisher-side PeerConnection. The
// publisher connects first; viewers can be added before or after the
// publisher's track arrives (offers are deferred until the track is known).
func newScreenSFU(room *Room, pub *screenPublisher) (*ScreenSFU, error) {
	s := &ScreenSFU{
		room:    room,
		pub:     pub,
		viewers: make(map[*websocket.Conn]*sfuViewer),
	}

	mediaEngine, err := screenShareMediaEngine()
	if err != nil {
		return nil, err
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: sfuICEServers})
	if err != nil {
		return nil, err
	}
	s.publisherPC = pc
	log.Printf("[sfu] ScreenSFU created for room %s (publisher PC ready)", room.id)

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		s.mutex.Lock()
		if !s.publisherReady {
			s.publisherICEPending = append(s.publisherICEPending, init)
			s.mutex.Unlock()
			return
		}
		s.mutex.Unlock()
		s.sendPublisherICE(init)
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		s.onPublisherTrack(track)
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[sfu] publisher connection state=%s in room %s", state, room.id)
		if state == webrtc.PeerConnectionStateConnected {
			// The publisher sends its initial keyframe as soon as the connection
			// is up, but if the first PLI was sent before ICE completed it was
			// dropped. Request one now that the transport is ready.
			s.requestKeyframe()
		}
	})

	return s, nil
}

// sendPublisherICE serializes an ICE candidate for the publisher and sends it
// through the room's screen-publisher write path.
func (s *ScreenSFU) sendPublisherICE(candidate webrtc.ICECandidateInit) {
	data, err := json.Marshal(map[string]interface{}{
		"type":      "ice",
		"candidate": candidate,
	})
	if err != nil {
		return
	}
	s.pub.send(data)
}

// sendViewerICE serializes an ICE candidate for a viewer and sends it through
// the room's screen-viewer write path.
func (s *ScreenSFU) sendViewerICE(conn *websocket.Conn, candidate webrtc.ICECandidateInit) {
	data, err := json.Marshal(map[string]interface{}{
		"type":      "ice",
		"candidate": candidate,
	})
	if err != nil {
		return
	}
	s.room.SendToScreenViewerConn(conn, data)
}

// onPublisherTrack is called when the publisher's video track arrives. It
// creates the shared local track (the fan-out point) and sends deferred offers
// to any viewers that joined before the publisher.
func (s *ScreenSFU) onPublisherTrack(track *webrtc.TrackRemote) {
	localTrack, err := webrtc.NewTrackLocalStaticRTP(track.Codec().RTPCodecCapability, "video", "screen")
	if err != nil {
		log.Printf("[sfu] NewTrackLocalStaticRTP failed: %v", err)
		return
	}

	type pendingViewer struct {
		conn *websocket.Conn
		v    *sfuViewer
	}

	s.mutex.Lock()
	s.remoteTrack = track
	s.localTrack = localTrack
	var pending []pendingViewer
	for conn, v := range s.viewers {
		if !v.offerSent {
			v.offerSent = true
			pending = append(pending, pendingViewer{conn, v})
		}
	}
	s.mutex.Unlock()

	log.Printf("[sfu] publisher track arrived in room %s: %s pt=%d", s.room.id, track.Codec().MimeType, track.PayloadType())

	go s.relay(track, localTrack)

	for _, p := range pending {
		s.sendOfferToViewer(p.conn, p.v, localTrack)
	}

	// Force a keyframe so viewers joining after the stream started receive a
	// decodable frame (VP8 can otherwise be garbage until the next keyframe).
	s.requestKeyframe()
}

// relay reads RTP packets from the publisher and fans them out through the
// shared local track. WriteRTP rewrites SSRC + payload type per viewer.
//
// It also runs a lightweight keyframe watchdog. A mid-stream joiner cannot
// decode VP8/VP9/H264 until a keyframe arrives, and the keyframe request sent
// at answer time races the viewer's ICE setup (the keyframe can be relayed
// before the viewer is ready to receive and is then dropped). If no keyframe
// has been forwarded within a short window, we re-request one so the viewer is
// never stuck on a black screen.
func (s *ScreenSFU) relay(remote *webrtc.TrackRemote, local *webrtc.TrackLocalStaticRTP) {
	buf := make([]byte, 64<<10) // generous: RTP packets are MTU-sized, but this is safe
	pkt := &rtp.Packet{}
	mime := remote.Codec().MimeType
	packets := 0
	keyframes := 0
	lastKeyframeReq := time.Now()
	for {
		n, _, err := remote.Read(buf)
		if err != nil {
			log.Printf("[sfu] publisher RTP read ended in room %s: %v", s.room.id, err)
			return
		}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			// A single malformed packet should not kill the whole relay.
			log.Printf("[sfu] RTP unmarshal failed in room %s: %v", s.room.id, err)
			continue
		}
		// RTP header extension IDs differ between the publisher<->SFU and
		// SFU<->viewer negotiations, so they must not be forwarded verbatim.
		pkt.Header.Extension = false
		pkt.Header.Extensions = nil
		if err := local.WriteRTP(pkt); err != nil {
			// One viewer may have failed; keep serving the others. The failed
			// viewer is removed via its OnConnectionStateChange handler.
			log.Printf("[sfu] relay write error in room %s: %v", s.room.id, err)
		}
		packets++
		if packets == 1 {
			log.Printf("[sfu] relay forwarding started in room %s (codec=%s ssrc=%d pt=%d)", s.room.id, mime, pkt.SSRC, pkt.PayloadType)
		}
		if isKeyframePacket(pkt, mime) {
			keyframes++
			if keyframes == 1 {
				log.Printf("[sfu] first keyframe relayed in room %s (codec=%s)", s.room.id, mime)
			}
		}
		// Watchdog: if no keyframe has been forwarded yet, re-request one at
		// most once per second. The first PLI (sent at track-arrival / answer
		// time) can race the viewer's ICE setup and be dropped.
		if keyframes == 0 && packets > 0 && time.Since(lastKeyframeReq) > time.Second {
			lastKeyframeReq = time.Now()
			s.requestKeyframe()
		}
	}
}

// isKeyframePacket reports whether an RTP packet carries a decodable keyframe
// for the given negotiated MIME type. Used by the relay watchdog to know when a
// viewer can actually start decoding.
func isKeyframePacket(pkt *rtp.Packet, mime string) bool {
	if pkt == nil || len(pkt.Payload) == 0 {
		return false
	}
	switch mime {
	case "video/VP8":
		// VP8 payload descriptor: bit 0 of the first byte is the inverse
		// keyframe flag (0 = keyframe).
		return pkt.Payload[0]&0x01 == 0
	case "video/H264":
		nalType := pkt.Payload[0] & 0x1F
		if nalType == 5 { // IDR
			return true
		}
		// FU-A fragmentation: an IDR may be split across packets. The start
		// fragment (S bit set) carries the real NAL type in the FU header.
		if nalType == 28 && len(pkt.Payload) >= 2 &&
			pkt.Payload[1]&0x80 != 0 && pkt.Payload[1]&0x1F == 5 {
			return true
		}
		return false
	case "video/VP9":
		// VP9 payload descriptor: the "P" bit (bit 6 of the first octet) is set
		// for inter-predicted frames; a keyframe has it clear.
		if len(pkt.Payload) < 1 {
			return false
		}
		return pkt.Payload[0]&0x40 == 0
	default:
		return false
	}
}

// AddViewer creates a viewer PeerConnection and, if the publisher's track is
// already known, sends an offer. Otherwise the offer is deferred until the
// track arrives.
func (s *ScreenSFU) AddViewer(conn *websocket.Conn, _ string) {
	s.mutex.Lock()
	if s.closed {
		s.mutex.Unlock()
		return
	}
	if _, exists := s.viewers[conn]; exists {
		s.mutex.Unlock()
		return
	}
	s.mutex.Unlock()

	mediaEngine, err := screenShareMediaEngine()
	if err != nil {
		log.Printf("[sfu] viewer media engine failed in room %s: %v", s.room.id, err)
		return
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: sfuICEServers})
	if err != nil {
		log.Printf("[sfu] viewer PC create failed in room %s: %v", s.room.id, err)
		return
	}

	viewer := &sfuViewer{pc: pc}
	s.mutex.Lock()
	if s.closed {
		s.mutex.Unlock()
		_ = pc.Close()
		return
	}
	s.viewers[conn] = viewer
	localTrack := s.localTrack
	s.mutex.Unlock()
	log.Printf("[sfu] viewer added in room %s (trackKnown=%v, viewers=%d)", s.room.id, localTrack != nil, len(s.viewers))

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		s.mutex.Lock()
		if !viewer.iceReady {
			viewer.icePending = append(viewer.icePending, init)
			s.mutex.Unlock()
			return
		}
		s.mutex.Unlock()
		s.sendViewerICE(conn, init)
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			// The viewer can now receive RTP. Request a keyframe so a
			// mid-stream joiner immediately gets a decodable frame (the
			// keyframe request sent at answer time raced the ICE setup).
			s.requestKeyframe()
		}
		if state == webrtc.PeerConnectionStateFailed ||
			state == webrtc.PeerConnectionStateClosed ||
			state == webrtc.PeerConnectionStateDisconnected {
			s.RemoveViewer(conn)
		}
	})

	if localTrack != nil {
		s.mutex.Lock()
		viewer.offerSent = true
		s.mutex.Unlock()
		s.sendOfferToViewer(conn, viewer, localTrack)
	}
}

// sendOfferToViewer binds the shared local track to the viewer and sends an SDP
// offer. The viewer answers and its ICE candidates are added via
// HandleViewerICE.
func (s *ScreenSFU) sendOfferToViewer(conn *websocket.Conn, v *sfuViewer, localTrack *webrtc.TrackLocalStaticRTP) {
	if _, err := v.pc.AddTrack(localTrack); err != nil {
		log.Printf("[sfu] viewer AddTrack failed in room %s: %v", s.room.id, err)
		s.RemoveViewer(conn)
		return
	}
	offer, err := v.pc.CreateOffer(nil)
	if err != nil {
		log.Printf("[sfu] viewer CreateOffer failed in room %s: %v", s.room.id, err)
		s.RemoveViewer(conn)
		return
	}
	if err := v.pc.SetLocalDescription(offer); err != nil {
		log.Printf("[sfu] viewer SetLocalDescription failed in room %s: %v", s.room.id, err)
		s.RemoveViewer(conn)
		return
	}
	data, err := json.Marshal(map[string]interface{}{
		"type": "offer",
		"sdp":  offer.SDP,
	})
	if err != nil {
		return
	}
	s.room.SendToScreenViewerConn(conn, data)
	log.Printf("[sfu] offer sent to viewer in room %s (codec=%s)", s.room.id, localTrack.Codec().MimeType)

	// The offer is now on its way; flush any ICE candidates that were gathered
	// while the offer was being created.
	s.mutex.Lock()
	v.iceReady = true
	pending := v.icePending
	v.icePending = nil
	s.mutex.Unlock()
	for _, cand := range pending {
		s.sendViewerICE(conn, cand)
	}
}

// HandlePublisherOffer sets the publisher's offer and replies with an answer.
func (s *ScreenSFU) HandlePublisherOffer(sdp string) {
	s.mutex.Lock()
	pc := s.publisherPC
	s.mutex.Unlock()
	if pc == nil || sdp == "" {
		log.Printf("[sfu] publisher offer dropped in room %s (pc=%v sdpLen=%d)", s.room.id, pc != nil, len(sdp))
		return
	}
	log.Printf("[sfu] publisher offer received in room %s (%d bytes)", s.room.id, len(sdp))
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		log.Printf("[sfu] publisher SetRemoteDescription(offer) failed: %v", err)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Printf("[sfu] publisher CreateAnswer failed: %v", err)
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		log.Printf("[sfu] publisher SetLocalDescription(answer) failed: %v", err)
		return
	}
	data, err := json.Marshal(map[string]interface{}{
		"type": "answer",
		"sdp":  answer.SDP,
	})
	if err != nil {
		return
	}
	s.pub.send(data)
	log.Printf("[sfu] publisher answer sent in room %s", s.room.id)

	// The answer is now on its way; flush any ICE candidates that were gathered
	// while the answer was being created.
	s.mutex.Lock()
	s.publisherReady = true
	pending := s.publisherICEPending
	s.publisherICEPending = nil
	s.mutex.Unlock()
	for _, cand := range pending {
		s.sendPublisherICE(cand)
	}
}

// HandlePublisherICE adds a trickled ICE candidate from the publisher.
func (s *ScreenSFU) HandlePublisherICE(candidate webrtc.ICECandidateInit) {
	s.mutex.Lock()
	pc := s.publisherPC
	s.mutex.Unlock()
	if pc == nil {
		return
	}
	if err := pc.AddICECandidate(candidate); err != nil {
		log.Printf("[sfu] publisher AddICECandidate failed: %v", err)
	}
}

// HandleViewerAnswer sets a viewer's answer.
func (s *ScreenSFU) HandleViewerAnswer(conn *websocket.Conn, sdp string) {
	s.mutex.Lock()
	v := s.viewers[conn]
	s.mutex.Unlock()
	if v == nil || sdp == "" {
		return
	}
	if err := v.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		log.Printf("[sfu] viewer SetRemoteDescription(answer) failed in room %s: %v", s.room.id, err)
		return
	}
	log.Printf("[sfu] viewer answer applied in room %s", s.room.id)

	// The viewer is now able to receive RTP. Ask the publisher for a keyframe so a
	// mid-stream joiner immediately gets a decodable frame instead of waiting for
	// the next periodic keyframe (VP8/VP9 delta frames alone are undecodable, and
	// the PLI sent at track-arrival time races the viewer's answer + ICE setup).
	s.requestKeyframe()
}

// HandleViewerICE adds a trickled ICE candidate from a viewer.
func (s *ScreenSFU) HandleViewerICE(conn *websocket.Conn, candidate webrtc.ICECandidateInit) {
	s.mutex.Lock()
	v := s.viewers[conn]
	s.mutex.Unlock()
	if v == nil {
		return
	}
	if err := v.pc.AddICECandidate(candidate); err != nil {
		log.Printf("[sfu] viewer AddICECandidate failed in room %s: %v", s.room.id, err)
	}
}

// RemoveViewer closes and forgets a viewer.
func (s *ScreenSFU) RemoveViewer(conn *websocket.Conn) {
	s.mutex.Lock()
	v, exists := s.viewers[conn]
	if exists {
		delete(s.viewers, conn)
	}
	s.mutex.Unlock()
	if v != nil {
		_ = v.pc.Close()
	}
}

// requestKeyframe asks the publisher for a keyframe (PLI) so a mid-stream
// joiner gets a decodable frame.
func (s *ScreenSFU) requestKeyframe() {
	s.mutex.Lock()
	pc := s.publisherPC
	track := s.remoteTrack
	s.mutex.Unlock()
	if pc == nil || track == nil {
		return
	}
	if err := pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}}); err != nil {
		log.Printf("[sfu] keyframe request failed in room %s: %v", s.room.id, err)
	}
}

// Close tears down the publisher and all viewer PeerConnections.
func (s *ScreenSFU) Close() {
	s.mutex.Lock()
	if s.closed {
		s.mutex.Unlock()
		return
	}
	s.closed = true
	publisherPC := s.publisherPC
	viewers := make([]*sfuViewer, 0, len(s.viewers))
	for _, v := range s.viewers {
		viewers = append(viewers, v)
	}
	s.viewers = make(map[*websocket.Conn]*sfuViewer)
	s.mutex.Unlock()

	if publisherPC != nil {
		_ = publisherPC.Close()
	}
	for _, v := range viewers {
		_ = v.pc.Close()
	}
}
