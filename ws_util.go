package main

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// pingInterval is how often the server sends a native WebSocket ping
	// control frame (opcode 0x9) to keep connections alive. Cloudflare (and
	// other proxies) drop idle WebSocket connections after ~100 seconds, so a
	// 60-second ping keeps every connection comfortably under that threshold.
	pingInterval = 60 * time.Second

	// pongWait is how long we allow without hearing from the peer before
	// declaring it dead. It must be greater than pingInterval so a client that
	// answers every ping with a pong never trips the read deadline.
	pongWait = 70 * time.Second

	// pingWriteTimeout bounds how long a single ping write may block before the
	// connection is considered dead and closed.
	pingWriteTimeout = 10 * time.Second

	// writeTimeout bounds each data write on the audio/signaling path. It keeps
	// one slow or stalled client from holding a room's write mutex and freezing
	// everyone else's stream (head-of-line blocking).
	writeTimeout = 2 * time.Second

	// screenWriteTimeout is the same bound for the (larger) screen-share data
	// path. Screen keyframes can be a few MB, so the budget is more generous.
	screenWriteTimeout = 5 * time.Second
)

// writeMessage sets a short write deadline and writes a single message. It is
// used on every mutex-protected data-path write so a stalled client fails fast
// (and is then dropped by the caller) instead of blocking the whole room.
func writeMessage(conn *websocket.Conn, messageType int, data []byte, timeout time.Duration) error {
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	return conn.WriteMessage(messageType, data)
}

// startHeartbeat configures a WebSocket connection with the standard gorilla
// keep-alive pattern and starts a background ping goroutine:
//
//   - SetPongHandler: a pong from the peer extends the read deadline, so an
//     idle-but-alive client never times out.
//   - SetReadDeadline(pongWait): if the peer neither sends data nor answers a
//     ping within pongWait, the handler's ReadMessage returns a timeout and the
//     connection is torn down. This detects half-dead clients (laptop sleep,
//     network drop without FIN) that would otherwise leak forever.
//   - The ping goroutine sends a native ping (opcode 0x9) every pingInterval.
//     If the write fails, the peer is gone, so the connection is closed to
//     unblock the handler's read loop.
//
// It returns a stop function that cleanly terminates the goroutine.
func startHeartbeat(conn *websocket.Conn) func() {
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))

	stop := make(chan struct{})
	var once sync.Once

	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				deadline := time.Now().Add(pingWriteTimeout)
				if err := conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
					// The peer is gone (write failed). Close the connection so
					// the handler's blocked ReadMessage unblocks and cleans up.
					_ = conn.Close()
					return
				}
			case <-stop:
				return
			}
		}
	}()

	return func() {
		once.Do(func() { close(stop) })
	}
}
