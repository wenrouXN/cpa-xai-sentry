//go:build cshared

package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

func hostLog(level, message string) {
	payload, _ := json.Marshal(map[string]any{
		"level":   level,
		"message": message,
	})
	_, _ = callHostRaw("host.log", payload)
}

// hostLogThrottled writes at most once per key within window to CPA host logs.
// Used for high-churn paths so sentry never floods cli-proxy-api main.log.
func hostLogThrottled(level, message string, window time.Duration, key string) {
	if key == "" {
		key = level + "|" + message
	}
	if window <= 0 {
		window = 60 * time.Second
	}
	now := time.Now()
	hostLogMu.Lock()
	if t, ok := hostLogLast[key]; ok && now.Sub(t) < window {
		hostLogMu.Unlock()
		return
	}
	hostLogLast[key] = now
	// bound map growth (rare keys accumulate)
	if len(hostLogLast) > 256 {
		for k, t := range hostLogLast {
			if now.Sub(t) > window {
				delete(hostLogLast, k)
			}
		}
	}
	hostLogMu.Unlock()
	hostLog(level, message)
}

var (
	hostLogMu   sync.Mutex
	hostLogLast = map[string]time.Time{}
)

func callHostRaw(method string, request []byte) ([]byte, error) {
	return hostCall(method, request)
}

// hostCall is set to cgoHostCall from main.go init.
var hostCall = func(method string, request []byte) ([]byte, error) {
	return nil, fmt.Errorf("host call unavailable")
}
