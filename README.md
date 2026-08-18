# Voice server for comm.sqrll.net (designed as microservice)

##### Light server for voice streaming using web.
###### Frontend available at https://github.com/Przemek2122/voice.sqrll.net

##### Initial setup: (os env)
###### SQRLL_VOICE_PORT – Change port which this server will run on.
###### SQRLL_VOICE_API_KEY – Key for server to access sensitive functions (create room, etc)

---

## REST API Reference

All REST endpoints return JSON. Protected endpoints require the `X-API-Token` header.

---

### `POST /api/rooms/create`

Creates a new room (or returns the existing room if the token matches).

**Auth:** `X-API-Token` header (must match `SQRLL_VOICE_API_KEY`)

**Request body (JSON):**
```json
{
  "roomId": "my-room",
  "token":  "room-password"
}
```

| Field    | Type   | Required | Description                        |
|----------|--------|----------|------------------------------------|
| `roomId` | string | ✅       | Unique room identifier             |
| `token`  | string | ✅       | Room access token (shared secret)  |

**Response `201 Created`:**
```json
{
  "created": true,
  "roomId":  "my-room"
}
```

**Errors:**
| Status | Condition                  |
|--------|----------------------------|
| 400    | Invalid JSON / missing roomId |
| 401    | Missing or wrong API key   |
| 405    | Not a POST request         |

---

### `GET /api/rooms/check`

Checks whether a room exists.

**Auth:** `X-API-Token` header (must match `SQRLL_VOICE_API_KEY`)

**Query params:**

| Param  | Required | Description            |
|--------|----------|------------------------|
| `room` | ✅       | Room ID to check       |

**Response `200 OK`:**
```json
{
  "exists": true
}
```

**Errors:**
| Status | Condition                |
|--------|--------------------------|
| 400    | Missing `room` param     |
| 401    | Missing or wrong API key |
| 405    | Not a GET request        |

---

### `PUT /api/rooms/update-token`

Updates the access token for an existing room. Existing WebSocket connections are **not** affected — they remain connected with the old token. Only new connections must use the updated token.

**Auth:** `X-API-Token` header (must match `SQRLL_VOICE_API_KEY`)

**Request body (JSON):**
```json
{
  "roomId": "my-room",
  "token":  "new-password"
}
```

| Field    | Type   | Required | Description                          |
|----------|--------|----------|--------------------------------------|
| `roomId` | string | ✅       | Room ID to update                    |
| `token`  | string | ✅       | New room access token                |

**Response `200 OK`:**
```json
{
  "updated": true,
  "roomId":  "my-room"
}
```

**Errors:**
| Status | Condition                  |
|--------|----------------------------|
| 400    | Invalid JSON / missing roomId / missing token |
| 401    | Missing or wrong API key   |
| 404    | Room does not exist         |
| 405    | Not a PUT request          |

---

### `DELETE /api/rooms/remove`

Forcefully removes a room and disconnects **all** participants (audio clients, screen publisher, and screen viewers). Use this to terminate a room immediately without waiting for the 10-minute idle timeout.

**Auth:** `X-API-Token` header (must match `SQRLL_VOICE_API_KEY`)

**Request body (JSON):**
```json
{
  "roomId": "my-room"
}
```

| Field    | Type   | Required | Description                          |
|----------|--------|----------|--------------------------------------|
| `roomId` | string | ✅       | Room ID to remove                    |

**Response `200 OK`:**
```json
{
  "removed": true,
  "roomId":  "my-room"
}
```

**Errors:**
| Status | Condition                  |
|--------|----------------------------|
| 400    | Invalid JSON / missing roomId |
| 401    | Missing or wrong API key   |
| 404    | Room does not exist         |
| 405    | Not a DELETE request       |

---

### `GET /health`

Health check for load balancers and monitoring.

**Auth:** None

**Response `200 OK`:**
```
OK
```

---

## Voice Streaming (Audio)

Real-time audio streaming. Multiple clients per room — each client sends
their microphone audio and receives the streams of all other participants.

> **Primary mode — SFU (WebRTC).** When participants support
> `RTCPeerConnection`, the server acts as a **Selective Forwarding Unit** built
> on [Pion WebRTC](https://github.com/pion/webrtc). Each participant publishes
> **one** audio stream to the server, and the server forwards the RTP packets
> (no decode/re-encode) to every other participant — sub-second latency, and
> a participant's upload stays at **1× bitrate regardless of how many people
> are listening**. The legacy `MediaRecorder → WebSocket → MediaSource` path
> below remains only as an automatic fallback for browsers without WebRTC.

### Endpoint

```
ws://host/api/rooms/stream?room=X&userid=Y&token=Z
```

| Query Param | Required | Description                                 |
|-------------|----------|---------------------------------------------|
| `room`      | ✅       | Room ID to join                             |
| `userid`    | ✅       | Unique user identifier (used as audio label)|
| `token`     | ✅       | Room access token                           |

### Protocol (Binary Framing)

The server uses a **custom binary framing** so receivers can identify which user sent each audio chunk:

```
┌──────────────┬──────────┬─────────────────────┐
│  ID Length   │   ID Bytes   │   WebM Audio Chunk  │
│   (1 byte)   │  (1–255 B)   │     (variable)       │
└──────────────┴──────────┴─────────────────────┘
```

| Segment       | Size       | Description                                      |
|---------------|------------|--------------------------------------------------|
| ID Length     | 1 byte     | Number of bytes in the user ID (0–255)           |
| ID Bytes      | 1–255 B    | The sender's `userid` as raw bytes               |
| Audio Chunk   | variable   | Raw WebM (Opus) audio frame                      |

The sender sends **only raw WebM chunks** — the server prepends the ID framing automatically before broadcasting.

### Init Segment Caching (Late-Join Support)

- Each sender's audio is relayed through a **Cluster-aligned feed** that only emits
  complete WebM units: one keyframe (`EBML + Segment + Info + Tracks + FIRST complete
  Cluster`) followed by complete Clusters.
- The keyframe is cached **per sender** (keyed by connection). When a new client joins,
  every cached keyframe is sent to them immediately, so a late joiner always starts with
  `init + media` — never a bare init (which Chrome's MediaSource rejects with a fatal
  SourceBuffer error) and never a mid-Cluster fragment.
- Cached keyframes clear when that sender disconnects.

### Room Lifecycle

| Event                       | Behavior                                          |
|-----------------------------|---------------------------------------------------|
| First client joins          | Room is created, idle timer stopped               |
| Client sends audio           | Broadcast to **all other clients** (not self)     |
| Last client leaves          | 10-minute idle timer starts                       |
| Timer expires, room empty   | Room destroyed, memory freed                      |

### Frontend Integration

**Sending audio** (uses `getUserMedia` + `MediaRecorder`):

```js
const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
const recorder = new MediaRecorder(stream, { mimeType: 'audio/webm;codecs=opus' });

recorder.ondataavailable = (e) => {
    if (e.data.size > 0) ws.send(e.data);
};
recorder.start(100); // ~100ms chunks for low latency
```

**Receiving audio** (parses ID prefix, feeds WebM to `<audio>`):

```js
const mediaSource = new MediaSource();
audioElement.src = URL.createObjectURL(mediaSource);

// Map userid → SourceBuffer
const sourceBuffers = {};

mediaSource.addEventListener('sourceopen', () => {
    ws.onmessage = (e) => {
        if (!(e.data instanceof Blob)) return;

        e.data.arrayBuffer().then(buf => {
            const bytes = new Uint8Array(buf);

            // Parse framing: [1B idLen] [idBytes] [audioChunk]
            const idLen = bytes[0];
            const userId = new TextDecoder().decode(bytes.slice(1, 1 + idLen));
            const audioChunk = bytes.slice(1 + idLen);

            // Create per-user SourceBuffer on first chunk
            if (!sourceBuffers[userId]) {
                sourceBuffers[userId] = mediaSource.addSourceBuffer('audio/webm;codecs=opus');
            }

            sourceBuffers[userId].appendBuffer(audioChunk);
        });
    };
});
```

> **Note:** Each user gets their own `SourceBuffer` because each stream has its own WebM header/init segment. The browser mixes them automatically.

---

## Screen Share Streaming

Screen sharing allows one user per room to broadcast their screen to multiple viewers via WebSocket.

### Endpoint

```
ws://host/api/rooms/screenshare?room=X&userid=Y&token=Z&role=publisher|viewer
```

| Query Param | Required | Description                                                  |
|-------------|----------|--------------------------------------------------------------|
| `room`      | ✅       | Room ID to join                                              |
| `userid`    | ✅       | Unique user identifier                                       |
| `token`     | ✅       | Room access token                                            |
| `role`      | ✅       | `publisher` (sends video) or `viewer` (receives video)       |

### How it works

> **Primary mode — SFU (WebRTC).** When both the publisher and viewers support
> `RTCPeerConnection`, the server acts as a **Selective Forwarding Unit** built on
> [Pion WebRTC](https://github.com/pion/webrtc). The publisher sends **one** stream
> to the server, and the server forwards the RTP packets (no decode/re-encode) to
> every viewer. This keeps the publisher's upload at **1× bitrate regardless of
> the number of viewers**, adds only a few milliseconds of latency, and removes the
> MediaRecorder/MSE backlog entirely. The legacy `MediaRecorder → WebSocket →
> MediaSource` path below remains only as an automatic fallback.

- **Publisher** — Exactly **one per room**. In MSE fallback mode it sends raw WebM video binary frames (VP8/VP9) via WebSocket. All frames are broadcast to every viewer. If a second publisher tries to connect, they receive a JSON error and are rejected.

- **Viewer** — Receives raw `video/webm` binary frames pushed by the publisher. On join, the viewer is immediately sent the cached WebM initialization segment (if available) so they can decode the ongoing stream without waiting for the next keyframe.

- **Init segment caching** — The publisher's raw WebM is relayed through a
  **Cluster-aligned feed**: the server emits one keyframe (`init + first complete
  Cluster`) and then complete Clusters. The keyframe is cached and sent to late-joining
  viewers immediately, so they never receive a bare init or a mid-Cluster fragment.

- **Exactly-once init delivery** — Keyframe caching, the per-viewer "init pending" gate, and viewer registration all happen under the same room mutex. This guarantees each viewer receives exactly **one** keyframe before any media Cluster — a duplicate init would corrupt the browser's `SourceBuffer`.

- **Duplicate init protection** — The server drops any WebSocket binary message that
  starts with the EBML magic (`0x1A 0x45 0xDF 0xA3`) once the publisher's relay is
  active. The publisher emits its init exactly once; any later EBML header is a
  client re-broadcast that would otherwise be concatenated onto a Cluster and
  emitted as one corrupt unit.

- **Viewer recovery (`request_keyframe`)** — A viewer whose `SourceBuffer` fails and
  is rebuilt sends `{"type":"request_keyframe"}` as a WebSocket text message. The
  server replies with the cached keyframe (init + first complete Cluster) so the
  viewer can re-seed its `MediaSource` without waiting for a new publisher.

- **Publisher disconnect** — When the publisher disconnects, **all viewers are forcibly disconnected**. This gives the frontend a clean signal that the stream has ended.

- **Cleanup** — Rooms are auto-destroyed after 10 minutes of being fully empty (no audio clients, no screen publisher, no screen viewers).

### Frontend integration

**Publisher side** (uses `getDisplayMedia` + `MediaRecorder`):

```js
const stream = await navigator.mediaDevices.getDisplayMedia({ video: true });
const recorder = new MediaRecorder(stream, { mimeType: 'video/webm;codecs=vp8' });

recorder.ondataavailable = (e) => {
    if (e.data.size > 0) ws.send(e.data);
};
recorder.start(100); // ~100ms chunks for low latency
```

**Viewer side** (receives frames via `MediaSource` API):

```js
const mediaSource = new MediaSource();
videoElement.src = URL.createObjectURL(mediaSource);

mediaSource.addEventListener('sourceopen', () => {
    const sourceBuffer = mediaSource.addSourceBuffer('video/webm;codecs=vp8');
    ws.onmessage = (e) => {
        if (e.data instanceof Blob) {
            e.data.arrayBuffer().then(buf => sourceBuffer.appendBuffer(buf));
        }
    };
});
```

### Limits

| Limit                     | Value        |
|---------------------------|--------------|
| Max publishers per room   | 1            |
| Max video frame size      | 5 MB         |
| Room idle timeout         | 10 minutes   |

---

## Architecture Overview

```
                           ┌──────────────────────┐
                           │     HTTP Server      │
                           │   (Go net/http)      │
                           └──────────┬───────────┘
                                      │
          ┌───────────────────────────┼───────────────────────────┐
          │                           │                           │
    ┌─────▼─────┐          ┌──────────▼───────────┐          ┌─────▼─────┐
    │  REST API │          │ Audio WS            │          │ Screen WS │
    │  /create  │          │  /stream            │          │/screenshare│
    │  /check   │          │                     │          │           │
    │/update-token│        │                     │          │           │
    │  /remove  │          │                     │          │           │
    └───────────┘          └──────────┬──────────┘          └─────┬─────┘
                                      │                           │
                              ┌───────▼───────┐        ┌──────────▼───────────┐
                              │   Room Map    │        │  Room Map           │
                              │  clients[]    │        │ publisher           │
                              │  initSegs[]   │        │ viewers[]           │
                              └───────┬───────┘        └──────────┬───────────┘
                                      │                           │
                              ┌───────▼───────┐                   │
                              │   Broadcast   │                   │
                              │  [ID+chunk]   │                   │
                              └───────────────┘                   │
```

| Layer            | Audio                          | Screen Share                     |
|------------------|--------------------------------|----------------------------------|
| **Protocol**     | WebRTC (SFU) / WS binary fallback | WebSocket binary               |
| **Topology**     | Many-to-many (SFU)             | One-to-many (broadcast)          |
| **Framing**      | Custom: `[1B len][ID][chunk]`  | Raw WebM frames                  |
| **Init caching** | Per-client (one per sender)    | Single (per-room)                |
| **Late-join**    | All cached init segments sent  | Cached init segment sent         |
| **Max senders**  | Unlimited                      | 1 publisher                      |
