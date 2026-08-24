package main

import (
	"crypto/subtle"
	"os"
	"strconv"
)

// GetPort returns the port (without the leading colon) the server listens on.
// It defaults to 8082 and is configurable via SQRLL_VOICE_PORT.
func GetPort() string {
	port := os.Getenv("SQRLL_VOICE_PORT")
	if port == "" {
		port = "8082" // Fallback
	}

	return port
}

// GetAddress returns the bind address for the server. It is configurable via
// SQRLL_VOICE_ADDRESS. An empty value (the default) means "all interfaces",
// which is equivalent to the previous "*" behavior.
func GetAddress() string {
	return os.Getenv("SQRLL_VOICE_ADDRESS")
}

func GetAPIKey() string {
	return os.Getenv("SQRLL_VOICE_API_KEY")
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

// secureEqual compares two strings in constant time, which prevents a timing
// side channel when checking API keys or room tokens. It returns false for
// different lengths (length is not considered secret).
func secureEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
