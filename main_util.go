package main

import (
	"fmt"
	"os"
	"strconv"
)

func GetPort() string {
	port := os.Getenv("SQRLL_VOICE_PORT")
	if port == "" {
		port = "8082" // Fallback
	}

	return fmt.Sprintf(":%s", port)
}

func GetAPIKey() string {
	return os.Getenv("SQRLL_VOICE_API_KEY")
}

// GetKlipyAPIKey returns the KLIPY GIF API key used to proxy GIF search /
// trending / fetch requests. Empty means the GIF feature is disabled and the
// proxy endpoints respond with 503 (GIF provider not configured).
func GetKlipyAPIKey() string {
	return os.Getenv("SQRLL_KLIPY_API_KEY")
}

// GetMaxScreenSharesPerRoom returns the hard cap on concurrent screen shares
// per room. It defaults to 5 and is configurable via the
// SQRLL_MAX_SCREENSHARES_PER_ROOM environment variable (non-positive or
// non-numeric values fall back to the default).
func GetMaxScreenSharesPerRoom() int {
	raw := os.Getenv("SQRLL_MAX_SCREENSHARES_PER_ROOM")
	if raw == "" {
		return 5
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 5
	}
	return n
}
