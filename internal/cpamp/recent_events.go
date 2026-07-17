package cpamp

import (
	"context"
	"database/sql"
	"html"
	"os"
	"strings"
	"time"
)

// RecentEvent is one usage_events row for an account timeline (success or fail).
type RecentEvent struct {
	TimestampMS int64  `json:"timestamp_ms"`
	At          string `json:"at"` // Shanghai display
	Model       string `json:"model,omitempty"`
	Path        string `json:"path,omitempty"`
	Failed      bool   `json:"failed"`
	Status      int    `json:"status,omitempty"`
	Tokens      int64  `json:"tokens,omitempty"`
	RequestID   string `json:"request_id,omitempty"`
	// short human error / body snippet (fails only)
	Summary string `json:"summary,omitempty"`
}

// FetchAuthRecentEvents returns the last N requests for one xAI auth (newest first in SQL, returned oldest→newest).
// Match by auth_index first; also try account_snapshot / auth_file_snapshot when provided.
func FetchAuthRecentEvents(ctx context.Context, authIndex, account, file string, limit int) ([]RecentEvent, string, error) {
	if limit <= 0 {
		limit = 15
	}
	if limit > 30 {
		limit = 30
	}
	authIndex = strings.TrimSpace(authIndex)
	account = strings.TrimSpace(account)
	file = strings.TrimSpace(file)
	if authIndex == "" && account == "" && file == "" {
		return nil, "", nil
	}
	path := resolveUsageDB()
	if path == "" {
		return nil, "", nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil, path, nil
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, path, err
	}
	defer db.Close()
	_, _ = db.ExecContext(ctx, `PRAGMA query_only=ON`)

	// Pull a bit more than limit then filter in-process for multi-key match.
	// Prefer exact auth_index; fall back to account/file if auth alone empty.
	q := `
SELECT
  COALESCE(timestamp_ms,0),
  COALESCE(NULLIF(TRIM(model),''), NULLIF(TRIM(requested_model),''), NULLIF(TRIM(resolved_model),''), ''),
  COALESCE(path,''),
  COALESCE(failed,0),
  COALESCE(fail_status_code,0),
  COALESCE(total_tokens,0),
  COALESCE(request_id,''),
  COALESCE(fail_summary,''),
  COALESCE(fail_body,''),
  COALESCE(auth_index,''),
  COALESCE(account_snapshot,''),
  COALESCE(auth_file_snapshot,'')
FROM usage_events
WHERE ` + isXAIProvider() + `
  AND (
    (? != '' AND auth_index = ?)
    OR (? != '' AND lower(COALESCE(account_snapshot,'')) = lower(?))
    OR (? != '' AND (
         lower(COALESCE(auth_file_snapshot,'')) = lower(?)
      OR lower(COALESCE(auth_file_snapshot,'')) LIKE '%' || lower(?)
      OR lower(COALESCE(auth_file_snapshot,'')) LIKE lower(?) || '%'
    ))
  )
ORDER BY timestamp_ms DESC
LIMIT ?
`
	rows, err := db.QueryContext(ctx, q,
		authIndex, authIndex,
		account, account,
		file, file, file, file,
		limit*3,
	)
	if err != nil {
		return nil, path, err
	}
	defer rows.Close()

	loc := time.FixedZone("CST", 8*3600)
	tmp := make([]RecentEvent, 0, limit)
	for rows.Next() {
		var (
			ts, tokens                               int64
			model, pth, rid, sum, body, ai, acc, af string
			failed, status                           int
		)
		if err := rows.Scan(&ts, &model, &pth, &failed, &status, &tokens, &rid, &sum, &body, &ai, &acc, &af); err != nil {
			continue
		}
		// prefer exact auth_index matches first: if authIndex set and row differs, skip unless no auth hits yet
		if authIndex != "" && ai != "" && !strings.EqualFold(ai, authIndex) {
			// allow if account/file matched and auth was empty on row
			if !(account != "" && strings.EqualFold(acc, account)) && !(file != "" && fileMatch(af, file)) {
				continue
			}
		}
		ev := RecentEvent{
			TimestampMS: ts,
			Model:       strings.TrimSpace(model),
			Path:        strings.TrimSpace(pth),
			Failed:      failed != 0,
			Status:      status,
			Tokens:      tokens,
			RequestID:   strings.TrimSpace(rid),
		}
		if ts > 0 {
			ev.At = time.UnixMilli(ts).In(loc).Format("01-02 15:04:05")
		}
		// fill model from fail body when column empty (rare writers)
		if ev.Model == "" && (failed != 0 || status >= 400) {
			ev.Model = ModelFromFailBody(body)
		}
		if !ev.Failed && status >= 200 && status < 300 {
			// ok
		} else if !ev.Failed && status == 0 && failed == 0 {
			// success with status missing
		} else if failed != 0 || status >= 400 {
			ev.Failed = true
			if ev.Status == 0 {
				ev.Status = status
			}
			ev.Summary = shortFailSummary(sum, body)
		}
		tmp = append(tmp, ev)
		if len(tmp) >= limit {
			break
		}
	}
	// reverse newest-first → oldest→newest (left old, right new)
	for i, j := 0, len(tmp)-1; i < j; i, j = i+1, j-1 {
		tmp[i], tmp[j] = tmp[j], tmp[i]
	}
	return tmp, path, nil
}

func fileMatch(snapshot, file string) bool {
	s := strings.ToLower(strings.TrimSpace(snapshot))
	f := strings.ToLower(strings.TrimSpace(file))
	if s == "" || f == "" {
		return false
	}
	if s == f {
		return true
	}
	// basename
	if i := strings.LastIndexAny(s, "/\\"); i >= 0 {
		s = s[i+1:]
	}
	if j := strings.LastIndexAny(f, "/\\"); j >= 0 {
		f = f[j+1:]
	}
	return s == f || strings.Contains(s, f) || strings.Contains(f, s)
}

func shortFailSummary(sum, body string) string {
	s := strings.TrimSpace(sum)
	if s == "" {
		s = strings.TrimSpace(body)
	}
	s = html.UnescapeString(s)
	// try extract error message from json-ish
	low := strings.ToLower(s)
	if i := strings.Index(low, `"error"`); i >= 0 {
		// crude pull
		rest := s[i:]
		if j := strings.Index(rest, ":"); j >= 0 {
			rest = strings.TrimSpace(rest[j+1:])
			rest = strings.Trim(rest, `"'{}[] `)
			if k := strings.IndexAny(rest, `",}`); k > 0 {
				rest = rest[:k]
			}
			if rest != "" {
				s = rest
			}
		}
	}
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
