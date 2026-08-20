package main

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// File storage proxy.
//
// The browser never talks to the file/image storage service
// (SquirrelCommunicatorImage, port 8083) directly. It goes through this
// microservice instead, which:
//
//   - injects the SQRLL_IMAGE_API_KEY server-side (never exposed to the browser),
//   - rate-limits uploads per client IP,
//   - enforces a hard upload size cap,
//   - validates the content hash on download so it cannot be used for path
//     traversal or junk requests,
//   - streams downloads back with an immutable cache header (content is
//     addressable: same hash == same bytes).
//
// This lets the image service listen on localhost only (never publicly exposed)
// and centralizes abuse controls here.

const (
	// maxFileBytes matches the image service's MAX_UPLOAD_MB default.
	maxFileBytes = 8 * 1024 * 1024

	// Upload abuse controls (per client IP): a hard size cap plus a
	// token-bucket rate limit. These values are conservative so a normal
	// user pasting a few images never trips them.
	uploadRateLimitPerSec = 1.0
	uploadRateLimitBurst  = 10
	uploadRequestTimeout  = 60 * time.Second

	// Download rate limit is intentionally lenient: a single chat view loads
	// many images at once, so the burst is generous and refills quickly.
	downloadRateLimitPerSec = 10.0
	downloadRateLimitBurst  = 120
	downloadRequestTimeout  = 60 * time.Second
)

// fileHashRe matches a 64-character hex SHA-256 id.
var fileHashRe = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

// proxyHTTPClient returns a client suitable for proxying file requests to the
// image service. A fresh client per request keeps timeouts explicit without
// leaking connections across handlers.
func proxyHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// enableFileCORS mirrors enableCORS but also allows POST (uploads) and the
// Content-Type header the browser sends with a Blob body. Returns true when the
// request was a preflight we already answered.
func enableFileCORS(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-SQRLL-API-KEY")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// handleFileUpload proxies a file upload to the image service, injecting the
// API key server-side and applying size + rate-limit controls.
func (rm *RoomManager) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	if enableFileCORS(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if rm.uploadLimiter != nil && !rm.uploadLimiter.Allow(clientIP(r)) {
		writeJSONError(w, "Too many uploads, please slow down", http.StatusTooManyRequests)
		return
	}

	// Read (and cap) the body ourselves so we can reliably reject oversized
	// uploads before they ever reach the image service.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxFileBytes+1))
	if err != nil {
		writeJSONError(w, "Failed to read upload", http.StatusBadRequest)
		return
	}
	if len(body) > maxFileBytes {
		writeJSONError(w, "File is too large (max 8MB)", http.StatusRequestEntityTooLarge)
		return
	}

	upstream := GetImageServiceURL() + "/api/image/upload"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream, bytes.NewReader(body))
	if err != nil {
		writeJSONError(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Preserve the Content-Type the browser sent (Blob type), if any.
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if key := GetImageApiKey(); key != "" {
		req.Header.Set("X-SQRLL-API-KEY", key)
	}

	resp, err := proxyHTTPClient(uploadRequestTimeout).Do(req)
	if err != nil {
		writeJSONError(w, "Storage service unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Pass the image service's JSON response straight through (it already
	// returns {id, status, size, ...} in the shape the frontend expects).
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+4096))
	if err != nil {
		writeJSONError(w, "Storage service error", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// handleFileDownload streams a stored file from the image service. The path is
// /api/files/{sha256-hash}.
func (rm *RoomManager) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	if enableFileCORS(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if rm.downloadLimiter != nil && !rm.downloadLimiter.Allow(clientIP(r)) {
		writeJSONError(w, "Too many requests", http.StatusTooManyRequests)
		return
	}

	// Extract and validate the hash from the path.
	hash := strings.TrimPrefix(r.URL.Path, "/api/files/")
	if hash == r.URL.Path || hash == "" {
		writeJSONError(w, "Missing file id", http.StatusBadRequest)
		return
	}
	if i := strings.IndexByte(hash, '/'); i >= 0 {
		hash = hash[:i]
	}
	if !fileHashRe.MatchString(hash) {
		writeJSONError(w, "Invalid file id", http.StatusBadRequest)
		return
	}
	hash = strings.ToLower(hash)

	upstream := GetImageServiceURL() + "/api/image/" + hash
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream, nil)
	if err != nil {
		writeJSONError(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if key := GetImageApiKey(); key != "" {
		req.Header.Set("X-SQRLL-API-KEY", key)
	}

	resp, err := proxyHTTPClient(downloadRequestTimeout).Do(req)
	if err != nil {
		writeJSONError(w, "Storage service unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Forward the upstream Content-Type so <img>/<video>/<audio> render
	// correctly; fall back to octet-stream when the service omits it.
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	// Content-addressable: only cache successful responses immutably.
	if resp.StatusCode == http.StatusOK {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}

	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, maxFileBytes+1))
}
