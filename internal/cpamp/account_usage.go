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

// AccountDay is today's per-account usage aggregated from CPAMP usage.sqlite
// (same source as the request monitor account view).
type AccountDay struct {
	AuthIndex   string
	Account     string
	Label       string
	File        string
	Calls       int64
	Success     int64
	Failure     int64
	Tokens      int64
	InputTokens int64
	OutputTokens int64
	LastMS      int64
	// best-effort free-usage actual/limit parsed from latest fail_body
	Actual int64
	Limit  int64
	FailHTTP int
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

// FetchXAIAccountDay returns today's xAI per-auth aggregates (cached ~20s).
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
	// read-only-ish pragmas
	_, _ = db.ExecContext(ctx, `PRAGMA query_only=ON`)

	q := `
SELECT
  COALESCE(auth_index,''),
  COALESCE(account_snapshot,''),
  COALESCE(auth_label_snapshot,''),
  COALESCE(auth_file_snapshot,''),
  COUNT(*),
  SUM(CASE WHEN COALESCE(failed,0)=0 THEN 1 ELSE 0 END),
  SUM(CASE WHEN COALESCE(failed,0)!=0 THEN 1 ELSE 0 END),
  SUM(COALESCE(total_tokens,0)),
  SUM(COALESCE(input_tokens,0)),
  SUM(COALESCE(output_tokens,0)),
  MAX(COALESCE(timestamp_ms,0))
FROM usage_events
WHERE timestamp_ms >= ?
  AND (lower(COALESCE(provider,'')) LIKE '%xai%' OR lower(COALESCE(provider,'')) LIKE '%grok%')
GROUP BY auth_index
`
	rows, err := db.QueryContext(ctx, q, fromMS)
	if err != nil {
		return nil, path, err
	}
	defer rows.Close()
	out := map[string]AccountDay{}
	for rows.Next() {
		var a AccountDay
		if err := rows.Scan(&a.AuthIndex, &a.Account, &a.Label, &a.File, &a.Calls, &a.Success, &a.Failure, &a.Tokens, &a.InputTokens, &a.OutputTokens, &a.LastMS); err != nil {
			continue
		}
		if a.AuthIndex == "" {
			continue
		}
		out[a.AuthIndex] = a
	}
	// latest free-usage fail body per auth (for actual/limit)
	fq := `
SELECT auth_index, COALESCE(fail_status_code,0), COALESCE(fail_summary,''), COALESCE(fail_body,'')
FROM usage_events
WHERE timestamp_ms >= ?
  AND COALESCE(failed,0)!=0
  AND (lower(COALESCE(provider,'')) LIKE '%xai%' OR lower(COALESCE(provider,'')) LIKE '%grok%')
  AND (fail_body LIKE '%actual/limit%' OR fail_body LIKE '%free-usage%' OR fail_status_code=429)
ORDER BY timestamp_ms DESC
LIMIT 5000
`
	frows, err := db.QueryContext(ctx, fq, fromMS)
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
			// keep body snippet only if needed later via summary
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
