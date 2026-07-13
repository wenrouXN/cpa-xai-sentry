package cpamp

import (
	"context"
	"database/sql"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// AccountDay is per-account usage aggregated from CPAMP usage.sqlite
// (same source as the request monitor account view).
type AccountDay struct {
	AuthIndex    string
	Account      string
	Label        string
	File         string
	// today (Shanghai day)
	Calls        int64
	Success      int64
	Failure      int64
	Tokens       int64
	InputTokens  int64
	OutputTokens int64
	LastMS       int64
	// all-time (in local usage.sqlite history)
	TotalCalls   int64
	TotalSuccess int64
	TotalFailure int64
	TotalTokens  int64
	// last 15 request outcomes: true=success, false=fail; chronological oldest→newest
	Recent15 []bool
	// best-effort free-usage actual/limit parsed from latest fail_body
	Actual      int64
	Limit       int64
	FailHTTP    int
	FailSummary string
}

var (
	reActualLimit = regexp.MustCompile(`(?i)tokens?\s*\(\s*actual\s*/\s*limit\s*\)\s*:\s*(\d+)\s*/\s*(\d+)`)
	usageMu       sync.Mutex
	usageCache    map[string]AccountDay
	usageAt       time.Time
	usagePath     string
)

// DefaultUsageDBPaths are candidate mount points for CPAMP usage.sqlite.
func DefaultUsageDBPaths() []string {
	return []string{
		"/data/usage.sqlite",
		"/vol1/1000/config/share/CLIProxyAPIplus/cpa-manager-data/usage.sqlite",
		"cpa-manager-data/usage.sqlite",
	}
}

func resolveUsageDB() string {
	if usagePath != "" {
		if _, err := os.Stat(usagePath); err == nil {
			return usagePath
		}
	}
	for _, p := range DefaultUsageDBPaths() {
		if _, err := os.Stat(p); err == nil {
			usagePath = p
			return p
		}
	}
	return ""
}

func isXAIProvider() string {
	return `(lower(COALESCE(provider,'')) LIKE '%xai%' OR lower(COALESCE(provider,'')) LIKE '%grok%')`
}

// FetchXAIAccountDay returns xAI per-auth aggregates (cached ~20s).
func FetchXAIAccountDay(ctx context.Context) (map[string]AccountDay, string, error) {
	usageMu.Lock()
	defer usageMu.Unlock()
	if usageCache != nil && time.Since(usageAt) < 20*time.Second {
		return usageCache, usagePath, nil
	}
	path := resolveUsageDB()
	if path == "" {
		return map[string]AccountDay{}, "", nil
	}
	fromMS, _ := DayRangeShanghai(time.Now())
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, path, err
	}
	defer db.Close()
	_, _ = db.ExecContext(ctx, `PRAGMA query_only=ON`)

	out := map[string]AccountDay{}

	// all-time totals
	tq := `
SELECT
  COALESCE(auth_index,''),
  COALESCE(account_snapshot,''),
  COALESCE(auth_label_snapshot,''),
  COALESCE(auth_file_snapshot,''),
  COUNT(*),
  SUM(CASE WHEN COALESCE(failed,0)=0 THEN 1 ELSE 0 END),
  SUM(CASE WHEN COALESCE(failed,0)!=0 THEN 1 ELSE 0 END),
  SUM(COALESCE(total_tokens,0)),
  MAX(COALESCE(timestamp_ms,0))
FROM usage_events
WHERE ` + isXAIProvider() + `
GROUP BY auth_index
`
	trows, err := db.QueryContext(ctx, tq)
	if err != nil {
		return nil, path, err
	}
	for trows.Next() {
		var a AccountDay
		if err := trows.Scan(&a.AuthIndex, &a.Account, &a.Label, &a.File, &a.TotalCalls, &a.TotalSuccess, &a.TotalFailure, &a.TotalTokens, &a.LastMS); err != nil {
			continue
		}
		if a.AuthIndex == "" {
			continue
		}
		out[a.AuthIndex] = a
	}
	trows.Close()

	// today
	dq := `
SELECT
  COALESCE(auth_index,''),
  COUNT(*),
  SUM(CASE WHEN COALESCE(failed,0)=0 THEN 1 ELSE 0 END),
  SUM(CASE WHEN COALESCE(failed,0)!=0 THEN 1 ELSE 0 END),
  SUM(COALESCE(total_tokens,0)),
  SUM(COALESCE(input_tokens,0)),
  SUM(COALESCE(output_tokens,0)),
  MAX(COALESCE(timestamp_ms,0))
FROM usage_events
WHERE timestamp_ms >= ?
  AND ` + isXAIProvider() + `
GROUP BY auth_index
`
	drows, err := db.QueryContext(ctx, dq, fromMS)
	if err != nil {
		return nil, path, err
	}
	for drows.Next() {
		var auth string
		var calls, ok, fail, tok, inTok, outTok, last int64
		if err := drows.Scan(&auth, &calls, &ok, &fail, &tok, &inTok, &outTok, &last); err != nil || auth == "" {
			continue
		}
		a := out[auth]
		a.AuthIndex = auth
		a.Calls, a.Success, a.Failure = calls, ok, fail
		a.Tokens, a.InputTokens, a.OutputTokens = tok, inTok, outTok
		if last > a.LastMS {
			a.LastMS = last
		}
		out[auth] = a
	}
	drows.Close()

	// last 15 request outcomes per auth (newest first from SQL, reverse to oldest→newest)
	rq := `
SELECT auth_index, COALESCE(failed,0), COALESCE(timestamp_ms,0)
FROM usage_events
WHERE ` + isXAIProvider() + `
ORDER BY timestamp_ms DESC
LIMIT 30000
`
	rrows, err := db.QueryContext(ctx, rq)
	if err == nil {
		defer rrows.Close()
		// collect up to 15 newest per auth (newest-first), then reverse
		tmp := map[string][]bool{}
		for rrows.Next() {
			var auth string
			var failed int
			var ts int64
			if err := rrows.Scan(&auth, &failed, &ts); err != nil || auth == "" {
				continue
			}
			if len(tmp[auth]) >= 15 {
				continue
			}
			tmp[auth] = append(tmp[auth], failed == 0)
		}
		for auth, seq := range tmp {
			// reverse to chronological oldest→newest
			for i, j := 0, len(seq)-1; i < j; i, j = i+1, j-1 {
				seq[i], seq[j] = seq[j], seq[i]
			}
			a := out[auth]
			a.AuthIndex = auth
			a.Recent15 = seq
			out[auth] = a
		}
	}

	// latest free-usage fail body per auth (for actual/limit)
	fq := `
SELECT auth_index, COALESCE(fail_status_code,0), COALESCE(fail_summary,''), COALESCE(fail_body,'')
FROM usage_events
WHERE COALESCE(failed,0)!=0
  AND ` + isXAIProvider() + `
  AND (fail_body LIKE '%actual/limit%' OR fail_body LIKE '%free-usage%' OR fail_status_code=429)
ORDER BY timestamp_ms DESC
LIMIT 5000
`
	frows, err := db.QueryContext(ctx, fq)
	if err == nil {
		defer frows.Close()
		seen := map[string]bool{}
		for frows.Next() {
			var auth string
			var code int
			var sum, body string
			if err := frows.Scan(&auth, &code, &sum, &body); err != nil || auth == "" || seen[auth] {
				continue
			}
			seen[auth] = true
			a := out[auth]
			a.AuthIndex = auth
			a.FailHTTP = code
			a.FailSummary = sum
			if m := reActualLimit.FindStringSubmatch(body); len(m) == 3 {
				if u, e1 := strconv.ParseInt(m[1], 10, 64); e1 == nil {
					a.Actual = u
				}
				if l, e2 := strconv.ParseInt(m[2], 10, 64); e2 == nil {
					a.Limit = l
				}
			}
			if strings.TrimSpace(a.FailSummary) == "" && body != "" {
				if len(body) > 160 {
					a.FailSummary = body[:160]
				} else {
					a.FailSummary = body
				}
			}
			out[auth] = a
		}
	}
	usageCache, usageAt = out, time.Now()
	return out, path, nil
}
