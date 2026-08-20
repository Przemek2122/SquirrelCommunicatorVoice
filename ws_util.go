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

	// pingWriteTimeout bounds how long a single ping write may block before the
	// connection is considered dead and the ping goroutine gives up.
	pingWriteTimeout = 10 * time.Second
)

// startPingLoop starts a background goroutine that sends a native WebSocket
// ping control frame (opcode 0x9) on every tick of interval. It returns a stop
// function that cleanly terminates the goroutine and stops the ticker.
//
// Why WriteControl (not WriteMessage):
//
//   - WriteControl is explicitly safe to call concurrently with the data path
//     (WriteMessage) and with the read loop, so the pings never race media
//     writes or interfere with incoming messages.
//   - A ping is a control frame (opcode 0x9), so the browser answers it with a
//     pong automatically at the protocol level without any JS involvement.
//
// The goroutine also self-terminates when the underlying connection closes,
// because WriteControl returns an error once the connection is closed. The
// returned stop function is therefore a fast, deterministic shutdown path on
// top of that (it lets the handler stop the goroutine before conn.Close runs,
// rather than waiting up to one full interval for the next failed write).
func startPingLoop(conn *websocket.Conn, interval time.Duration) func() {
	stop := make(chan struct{})
	var once sync.Once

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				deadline := time.Now().Add(pingWriteTimeout)
				if err := conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
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
