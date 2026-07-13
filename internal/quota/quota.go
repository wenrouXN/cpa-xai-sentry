package quota

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Info is best-effort quota extraction from upstream error/success bodies.
type Info struct {
	Limit     int64
	Used      int64
	Remaining int64
	ResetAt   time.Time
	Source    string // body_field|retry_after|heuristic
	RawHint   string
}

var (
	reRemaining = regexp.MustCompile(`(?i)(remaining|left|left_to_use|tokens?_remaining)["'\s:=]+(\d+)`)
	reLimit     = regexp.MustCompile(`(?i)(limit|quota|max_tokens?|total)["'\s:=]+(\d+)`)
	reUsed      = regexp.MustCompile(`(?i)(used|consumed|usage)["'\s:=]+(\d+)`)
	reRetry     = regexp.MustCompile(`(?i)retry[- ]after["':\s]+(\d+)`)
	reResetUnix = regexp.MustCompile(`(?i)(reset(?:_at|_time)?|resets_in)["'\s:=]+(\d+)`)
)

// Parse extracts quota hints from an HTTP body (JSON or text).
func Parse(body string) Info {
	var info Info
	if strings.TrimSpace(body) == "" {
		return info
	}
	// JSON walk first
	var m map[string]any
	if json.Unmarshal([]byte(body), &m) == nil {
		info = fromMap(m)
		if info.Limit > 0 || info.Remaining > 0 || info.Used > 0 || !info.ResetAt.IsZero() {
			info.Source = "body_field"
			return info
		}
	}
	// regex fallback
	if m := reRemaining.FindStringSubmatch(body); len(m) == 3 {
		info.Remaining, _ = strconv.ParseInt(m[2], 10, 64)
	}
	if m := reLimit.FindStringSubmatch(body); len(m) == 3 {
		info.Limit, _ = strconv.ParseInt(m[2], 10, 64)
	}
	if m := reUsed.FindStringSubmatch(body); len(m) == 3 {
		info.Used, _ = strconv.ParseInt(m[2], 10, 64)
	}
	if m := reRetry.FindStringSubmatch(body); len(m) == 2 {
		if sec, err := strconv.Atoi(m[1]); err == nil && sec > 0 {
			info.ResetAt = time.Now().Add(time.Duration(sec) * time.Second)
		}
	}
	if info.Limit > 0 || info.Remaining > 0 || info.Used > 0 || !info.ResetAt.IsZero() {
		info.Source = "heuristic"
		if info.Limit > 0 && info.Used == 0 && info.Remaining >= 0 && info.Remaining <= info.Limit {
			info.Used = info.Limit - info.Remaining
		}
		if info.Limit > 0 && info.Remaining == 0 && info.Used > 0 && info.Used <= info.Limit {
			info.Remaining = info.Limit - info.Used
		}
	}
	if len(body) > 160 {
		info.RawHint = body[:160]
	} else {
		info.RawHint = body
	}
	return info
}

func fromMap(m map[string]any) Info {
	var info Info
	// common nesting: error, usage, quota, rate_limit, limits
	cands := []map[string]any{m}
	for _, k := range []string{"error", "usage", "quota", "rate_limit", "rateLimit", "limits", "data", "metadata"} {
		if sub, ok := m[k].(map[string]any); ok {
			cands = append(cands, sub)
		}
	}
	getI := func(mm map[string]any, keys ...string) int64 {
		for _, k := range keys {
			if v, ok := mm[k]; ok {
				switch t := v.(type) {
				case float64:
					return int64(t)
				case int64:
					return t
				case int:
					return int64(t)
				case string:
					n, _ := strconv.ParseInt(t, 10, 64)
					return n
				case json.Number:
					n, _ := t.Int64()
					return n
				}
			}
		}
		return 0
	}
	for _, mm := range cands {
		if info.Limit == 0 {
			info.Limit = getI(mm, "limit", "quota", "max", "max_tokens", "token_limit", "total")
		}
		if info.Used == 0 {
			info.Used = getI(mm, "used", "consumed", "usage", "tokens_used", "total_tokens")
		}
		if info.Remaining == 0 {
			info.Remaining = getI(mm, "remaining", "left", "tokens_remaining", "remaining_tokens")
		}
		if info.ResetAt.IsZero() {
			if sec := getI(mm, "retry_after", "retryAfter", "resets_in", "reset_in"); sec > 0 && sec < 86400*7 {
				info.ResetAt = time.Now().Add(time.Duration(sec) * time.Second)
			} else if ts := getI(mm, "reset", "reset_at", "resetAt", "reset_time"); ts > 1_000_000_000 {
				// unix seconds or ms
				if ts > 1_000_000_000_000 {
					info.ResetAt = time.UnixMilli(ts)
				} else {
					info.ResetAt = time.Unix(ts, 0)
				}
			}
		}
	}
	if info.Limit > 0 && info.Remaining == 0 && info.Used > 0 && info.Used <= info.Limit {
		info.Remaining = info.Limit - info.Used
	}
	if info.Limit > 0 && info.Used == 0 && info.Remaining >= 0 && info.Remaining <= info.Limit {
		info.Used = info.Limit - info.Remaining
	}
	return info
}

// FreeQuotaPerAccount is the rolling free-tier estimate used by quota-guard
// (enabled xAI accounts × 1M tokens / 24h). Used when upstream body has no numbers.
const FreeQuotaPerAccount int64 = 1_000_000

// FreeUsageExhaustedEstimate marks remaining=0 when free usage exhausted.
// If the body has no numeric limit/used, fill the 1M free-tier estimate so UI
// can show 已用/限额/剩余 instead of empty "今日调用N".
func FreeUsageExhaustedEstimate(body string, recoverAt time.Time) Info {
	info := Parse(body)
	if info.Limit == 0 && info.Used == 0 && info.Remaining == 0 {
		info.Limit = FreeQuotaPerAccount
		info.Used = FreeQuotaPerAccount
		info.Remaining = 0
		info.Source = "free_usage_exhausted"
	} else if info.Remaining == 0 && info.Limit == 0 {
		info.Remaining = 0
		info.Source = "free_usage_exhausted"
	} else if info.Source == "" {
		info.Source = "free_usage_exhausted"
	}
	if info.ResetAt.IsZero() && !recoverAt.IsZero() {
		info.ResetAt = recoverAt
	}
	if info.RawHint == "" {
		if len(body) > 120 {
			info.RawHint = body[:120]
		} else {
			info.RawHint = body
		}
	}
	return info
}
