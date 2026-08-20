package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// KLIPY GIF API proxy.
//
// The browser never holds the KLIPY API key: it talks to these same-origin
// endpoints, and this microservice attaches the key server-side (fetched from
// the SQRLL_KLIPY_API_KEY environment variable) before forwarding to
// https://api.klipy.com.
//
// Endpoint shape (see the KLIPY docs / official SDKs):
//
//	https://api.klipy.com/api/v1/{app_key}/gifs/search?q=...
//	https://api.klipy.com/api/v1/{app_key}/gifs/trending
//
// Responses are wrapped as { "result": true, "data": { "data": [...], ... } }.
// We decode only the fields we need and re-serialize a compact, frontend-
// friendly payload.

const klipyBaseURL = "https://api.klipy.com"

// maxGifBytes caps the bytes we proxy when downloading a GIF for re-upload.
// It matches the file service's 8MB upload limit so anything larger would be
// rejected by the upload service anyway.
const maxGifBytes = 8 * 1024 * 1024

// --- KLIPY response models (subset) ---

type klipyEnvelope struct {
	Result bool            `json:"result"`
	Data   json.RawMessage `json:"data"`
}

type klipyPage struct {
	Data    []json.RawMessage `json:"data"`
	HasNext bool              `json:"has_next"`
}

type klipyFile struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Size   int64  `json:"size"`
}

type klipyFormats struct {
	GIF  *klipyFile `json:"gif"`
	WebP *klipyFile `json:"webp"`
	JPG  *klipyFile `json:"jpg"`
	MP4  *klipyFile `json:"mp4"`
	WebM *klipyFile `json:"webm"`
	PNG  *klipyFile `json:"png"`
}

type klipySizes struct {
	HD *klipyFormats `json:"hd"`
	MD *klipyFormats `json:"md"`
	SM *klipyFormats `json:"sm"`
	XS *klipyFormats `json:"xs"`
}

type klipyMediaItem struct {
	ID          int64      `json:"id"`
	Slug        string     `json:"slug"`
	Title       string     `json:"title"`
	File        klipySizes `json:"file"`
	Type        string     `json:"type"`
	BlurPreview string     `json:"blur_preview"`
}

// --- Compact payload returned to the browser ---

type gifMedia struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type gifResult struct {
	ID      int64     `json:"id"`
	Slug    string    `json:"slug"`
	Title   string    `json:"title"`
	Preview *gifMedia `json:"preview"`
	GIF     *gifMedia `json:"gif"`
	MP4     *gifMedia `json:"mp4,omitempty"`
}

func nonNil(files ...*klipyFile) *klipyFile {
	for _, f := range files {
		if f != nil && f.URL != "" {
			return f
		}
	}
	return nil
}

func toMedia(f *klipyFile) *gifMedia {
	if f == nil || f.URL == "" {
		return nil
	}
	return &gifMedia{URL: f.URL, Width: f.Width, Height: f.Height}
}

// previewMedia picks a small, cheap thumbnail (static formats preferred).
func (m *klipyMediaItem) previewMedia() *gifMedia {
	for _, size := range []*klipyFormats{m.File.SM, m.File.MD, m.File.XS, m.File.HD} {
		if size == nil {
			continue
		}
		if f := nonNil(size.WebP, size.JPG, size.PNG, size.GIF); f != nil {
			return toMedia(f)
		}
	}
	return nil
}

// gifMedia picks the GIF rendition to send (medium is a good chat size).
func (m *klipyMediaItem) gifMedia() *gifMedia {
	for _, size := range []*klipyFormats{m.File.MD, m.File.SM, m.File.HD, m.File.XS} {
		if size == nil {
			continue
		}
		if f := nonNil(size.GIF); f != nil {
			return toMedia(f)
		}
	}
	return nil
}

func (m *klipyMediaItem) mp4Media() *gifMedia {
	for _, size := range []*klipyFormats{m.File.MD, m.File.SM, m.File.HD, m.File.XS} {
		if size == nil {
			continue
		}
		if f := nonNil(size.MP4); f != nil {
			return toMedia(f)
		}
	}
	return nil
}

func (m *klipyMediaItem) toResult() *gifResult {
	// KLIPY may interleave advertisement objects ({ "type": "ad" }) with real
	// content. Skip them (and any item without a usable GIF).
	if strings.EqualFold(m.Type, "ad") {
		return nil
	}
	g := m.gifMedia()
	if g == nil {
		return nil
	}
	r := &gifResult{
		ID:      m.ID,
		Slug:    m.Slug,
		Title:   m.Title,
		Preview: m.previewMedia(),
		GIF:     g,
	}
	if mp4 := m.mp4Media(); mp4 != nil {
		r.MP4 = mp4
	}
	return r
}

// --- Shared helpers ---

// authorized mirrors the existing REST API auth: the X-API-Token header must
// match the configured service key. When the key is empty (local dev) the
// check passes, matching handleCreateRoomAPI's behaviour.
func (rm *RoomManager) authorized(r *http.Request) bool {
	return r.Header.Get("X-API-Token") == rm.APIKey
}

// enableCORS allows the dev frontend (served from another origin) to read
// these GET-only responses. In production everything is same-origin via the
// reverse proxy. Returns true if the request was a preflight that we already
// answered.
func enableCORS(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "X-API-Token")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func clampInt(raw string, min, max, def int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func klipyGet(path string, query url.Values) ([]byte, error) {
	key := GetKlipyAPIKey()
	u := fmt.Sprintf("%s/api/v1/%s/%s", klipyBaseURL, url.PathEscape(key), path)
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

func (rm *RoomManager) respondGifPage(w http.ResponseWriter, path string, query url.Values) {
	body, err := klipyGet(path, query)
	if err != nil {
		writeJSONError(w, "GIF provider error: "+err.Error(), http.StatusBadGateway)
		return
	}

	var env klipyEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		writeJSONError(w, "Invalid GIF provider response", http.StatusBadGateway)
		return
	}
	if !env.Result || len(env.Data) == 0 {
		writeJSONError(w, "GIF provider reported failure", http.StatusBadGateway)
		return
	}

	var page klipyPage
	if err := json.Unmarshal(env.Data, &page); err != nil {
		writeJSONError(w, "Invalid GIF provider page", http.StatusBadGateway)
		return
	}

	results := make([]*gifResult, 0, len(page.Data))
	for _, raw := range page.Data {
		var item klipyMediaItem
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		if r := item.toResult(); r != nil {
			results = append(results, r)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"results":  results,
		"has_next": page.HasNext,
	})
}

// --- Handlers ---

func (rm *RoomManager) handleGifsSearch(w http.ResponseWriter, r *http.Request) {
	if enableCORS(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !rm.authorized(r) {
		writeJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if GetKlipyAPIKey() == "" {
		writeJSONError(w, "GIF provider not configured", http.StatusServiceUnavailable)
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSONError(w, "Missing query (q)", http.StatusBadRequest)
		return
	}

	query := url.Values{}
	query.Set("q", q)
	query.Set("per_page", strconv.Itoa(clampInt(r.URL.Query().Get("limit"), 1, 50, 25)))
	query.Set("page", strconv.Itoa(clampInt(r.URL.Query().Get("page"), 1, 1000, 1)))
	query.Set("content_filter", "off")

	rm.respondGifPage(w, "gifs/search", query)
}

func (rm *RoomManager) handleGifsTrending(w http.ResponseWriter, r *http.Request) {
	if enableCORS(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !rm.authorized(r) {
		writeJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if GetKlipyAPIKey() == "" {
		writeJSONError(w, "GIF provider not configured", http.StatusServiceUnavailable)
		return
	}

	query := url.Values{}
	query.Set("per_page", strconv.Itoa(clampInt(r.URL.Query().Get("limit"), 1, 50, 25)))
	query.Set("page", strconv.Itoa(clampInt(r.URL.Query().Get("page"), 1, 1000, 1)))
	query.Set("content_filter", "off")

	rm.respondGifPage(w, "gifs/trending", query)
}

func (rm *RoomManager) handleGifsFetch(w http.ResponseWriter, r *http.Request) {
	if enableCORS(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !rm.authorized(r) {
		writeJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		writeJSONError(w, "Missing url", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		writeJSONError(w, "Invalid URL", http.StatusBadRequest)
		return
	}
	// SSRF guard: only allow hosts that resolve to public addresses, so this
	// proxy can never be used to reach internal/cloud-metadata services.
	if !isPublicHost(u.Hostname()) {
		writeJSONError(w, "URL host not allowed", http.StatusBadRequest)
		return
	}

	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		writeJSONError(w, "Download failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeJSONError(w, "Download failed (upstream status "+strconv.Itoa(resp.StatusCode)+")", http.StatusBadGateway)
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGifBytes+1))
	if err != nil {
		writeJSONError(w, "Download failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if len(body) > maxGifBytes {
		writeJSONError(w, "GIF is too large (max 8MB)", http.StatusRequestEntityTooLarge)
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "image/gif"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func isPublicHost(host string) bool {
	addrs, err := net.LookupIP(host)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, ip := range addrs {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
			return false
		}
	}
	return true
}
