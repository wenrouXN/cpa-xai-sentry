//go:build cshared

package main

import (
	"encoding/json"
	"fmt"
)

func hostLog(level, message string) {
	payload, _ := json.Marshal(map[string]any{
		"level":   level,
		"message": message,
	})
	_, _ = callHostRaw("host.log", payload)
}

func callHostRaw(method string, request []byte) ([]byte, error) {
	return hostCall(method, request)
}

// hostCall is set to cgoHostCall from main.go init.
var hostCall = func(method string, request []byte) ([]byte, error) {
	return nil, fmt.Errorf("host call unavailable")
}
