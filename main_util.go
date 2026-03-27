package main

import (
	"fmt"
	"os"
)

func GetPort() string {
	port := os.Getenv("SQRLL_VOICE_PORT")
	if port == "" {
		port = "8080" // Fallback
	}

	return fmt.Sprintf(":%s", port)
}

func GetAPIKey() string {
	return os.Getenv("SQRLL_VOICE_API_KEY")
}
