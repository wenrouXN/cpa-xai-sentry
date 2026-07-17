package cpamp

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"time"
)

// FailEvent is one failed xAI attempt from CPAMP usage.sqlite.
// Used to backfill sentry when host usage plugins miss failover intermediate legs.
type FailEvent struct {
	ID          int64
	AuthIndex   string
	File        string
	Account     string
	Status      int
	Body        string
	TimestampMS int64
	RequestID   string
	Model       string
}

// FetchRecentFailures returns xAI failures after sinceMS (exclusive), oldest first.
// sinceMS=0 means last 3 minutes (avoid replaying full history on first run).
func FetchRecentFailures(ctx context.Context, sinceMS int64, limit int) ([]FailEvent, string, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 300 {
		limit = 300
	}
	path := resolveUsageDB()
	if path == "" {
		return nil, "", nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil, path, nil
	}
	if sinceMS <= 0 {
		sinceMS = time.Now().Add(-3 * time.Minute).UnixMilli()
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, path, err
	}
	defer db.Close()
	_, _ = db.ExecContext(ctx, `PRAGMA query_only=ON`)

	// model: prefer model, then requested_model / resolved_model (some writers only fill those)
	q := `
SELECT
  COALESCE(id,0),
  COALESCE(auth_index,''),
  COALESCE(auth_file_snapshot,''),
  COALESCE(account_snapshot,''),
  COALESCE(fail_status_code,0),
  COALESCE(fail_body,''),
  COALESCE(timestamp_ms,0),
  COALESCE(request_id,''),
  COALESCE(NULLIF(TRIM(model),''), NULLIF(TRIM(requested_model),''), NULLIF(TRIM(resolved_model),''), '')
FROM usage_events
WHERE COALESCE(failed,0)!=0
  AND COALESCE(timestamp_ms,0) > ?
  AND COALESCE(fail_status_code,0) IN (401,402,403,429)
  AND ` + isXAIProvider() + `
ORDER BY timestamp_ms ASC, id ASC
LIMIT ?
`
	rows, err := db.QueryContext(ctx, q, sinceMS, limit)
	if err != nil {
		return nil, path, err
	}
	defer rows.Close()

	out := make([]FailEvent, 0, 32)
	for rows.Next() {
		var e FailEvent
		if err := rows.Scan(&e.ID, &e.AuthIndex, &e.File, &e.Account, &e.Status, &e.Body, &e.TimestampMS, &e.RequestID, &e.Model); err != nil {
			continue
		}
		if e.AuthIndex == "" && e.File == "" {
			continue
		}
		e.Model = strings.TrimSpace(e.Model)
		// body may be huge (headers appended); keep enough for match+quota
		if len(e.Body) > 1200 {
			e.Body = e.Body[:1200]
		}
		// strip trailing header dump if present (CPAMP sometimes concatenates)
		if i := strings.Index(e.Body, "\n{\""); i > 0 && strings.Contains(e.Body[i:], "Access-Control") {
			e.Body = strings.TrimSpace(e.Body[:i])
		}
		// last resort: free-usage body often embeds "for model X"
		if e.Model == "" {
			e.Model = ModelFromFailBody(e.Body)
		}
		out = append(out, e)
	}
	return out, path, rows.Err()
}

// ModelFromFailBody pulls model name from xAI free-usage / error bodies.
// Example: `... free usage for model grok-4.5-build-free for now ...`
func ModelFromFailBody(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	low := strings.ToLower(body)
	// "for model NAME"
	if i := strings.Index(low, "for model "); i >= 0 {
		rest := body[i+len("for model "):]
		return firstTokenModel(rest)
	}
	// "model\": \"NAME\"" json
	for _, key := range []string{`"model":"`, `"model": "`, `"model" : "`} {
		if i := strings.Index(low, strings.ToLower(key)); i >= 0 {
			rest := body[i+len(key):]
			return firstTokenModel(rest)
		}
	}
	return ""
}

func firstTokenModel(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'`)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
			continue
		}
		break
	}
	out := b.String()
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}
