package match

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Signal string

const (
	SignalNone             Signal = ""
	SignalFreeUsage429     Signal = "free_usage_429"
	SignalSpendingLimit402 Signal = "spending_limit_402"
	SignalAuth401          Signal = "auth_401"
	SignalPermission403    Signal = "permission_403"
)

type Kind string

const (
	KindNone    Kind = ""
	KindQuota   Kind = "quota"
	KindAuth    Kind = "auth"
	KindObserve Kind = "observe"
)

type Result struct {
	Signal    Signal
	Kind      Kind
	Code      string
	RecoverAt time.Time
	Reason    string
}

func Classify(statusCode int, body string) Result {
	lower := strings.ToLower(body)
	code := extractCode(body)

	if isRegion(lower) {
		return Result{Signal: SignalNone, Kind: KindObserve, Code: code, Reason: "region_block"}
	}
	if isSpending(statusCode, lower, code) {
		return Result{
			Signal:    SignalSpendingLimit402,
			Kind:      KindQuota,
			Code:      code,
			Reason:    "spending_limit",
			RecoverAt: time.Now().Add(24 * time.Hour),
		}
	}
	if isFreeUsage(statusCode, lower, code) {
		ra := parseReset(body)
		if ra.IsZero() {
			ra = time.Now().Add(24 * time.Hour)
		}
		return Result{
			Signal:    SignalFreeUsage429,
			Kind:      KindQuota,
			Code:      code,
			Reason:    "free_usage",
			RecoverAt: ra,
		}
	}
	if isPermission(statusCode, lower, code) {
		return Result{
			Signal: SignalPermission403,
			Kind:   KindAuth,
			Code:   code,
			Reason: "permission_denied",
		}
	}
	if isInvalidCreds(statusCode, lower) {
		return Result{
			Signal: SignalAuth401,
			Kind:   KindAuth,
			Code:   code,
			Reason: "invalid_credentials",
		}
	}
	return Result{Signal: SignalNone, Kind: KindObserve, Code: code, Reason: "unmatched"}
}

func isRegion(lower string) bool {
	keys := []string{
		"not available in your region",
		"unavailable in your region",
		"region is not supported",
		"unsupported region",
	}
	for _, k := range keys {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

func isSpending(status int, lower, code string) bool {
	if status != 402 && status != 0 {
		// still allow body-only detection when status missing from log text
		if status != 0 {
			// non-402 with spending text: still treat as spending if explicit code
			if !strings.Contains(code, "spending-limit") && !strings.Contains(lower, "personal-team-blocked:spending-limit") {
				return false
			}
		}
	}
	return strings.Contains(code, "spending-limit") ||
		strings.Contains(lower, "personal-team-blocked:spending-limit") ||
		strings.Contains(lower, "run out of credits") ||
		strings.Contains(lower, "need a grok subscription")
}

func isFreeUsage(status int, lower, code string) bool {
	if status != 429 && status != 0 {
		return false
	}
	return strings.Contains(code, "free-usage") ||
		strings.Contains(lower, "free-usage-exhausted") ||
		strings.Contains(lower, "included free usage") ||
		strings.Contains(lower, "rolling 24-hour")
}

func isPermission(status int, lower, code string) bool {
	if isRegion(lower) {
		return false
	}
	if status != 403 && status != 0 {
		return false
	}
	return strings.Contains(code, "permission-denied") ||
		strings.Contains(lower, "permission-denied") ||
		strings.Contains(lower, "access to the chat endpoint is denied")
}

func isInvalidCreds(status int, lower string) bool {
	if status != 401 && status != 0 {
		return false
	}
	return strings.Contains(lower, "invalid or expired credentials") ||
		strings.Contains(lower, "no auth context") ||
		strings.Contains(lower, "invalid_grant") ||
		strings.Contains(lower, "unauthorized")
}

func extractCode(body string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return ""
	}
	if c, ok := m["code"].(string); ok {
		return c
	}
	if e, ok := m["error"].(map[string]any); ok {
		if c, ok := e["code"].(string); ok {
			return c
		}
	}
	return ""
}

var resetRe = regexp.MustCompile(`(?i)retry[- ]after["':\s]+(\d+)`)

func parseReset(body string) time.Time {
	if m := resetRe.FindStringSubmatch(body); len(m) == 2 {
		sec, err := strconv.Atoi(m[1])
		if err == nil && sec > 0 {
			return time.Now().Add(time.Duration(sec) * time.Second)
		}
	}
	return time.Time{}
}
