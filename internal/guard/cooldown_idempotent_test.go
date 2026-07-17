package guard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

func TestCooldownIdempotentNoDoubleLog(t *testing.T) {
	dir := t.TempDir()
	disabled := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch || r.URL.Path == "/v0/management/auth-files/status" {
			w.WriteHeader(200)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"files": []any{
				map[string]any{"name": "xai-a@x.com.json", "disabled": disabled, "provider": "xai", "auth_index": "h1"},
			},
		})
	}))
	defer srv.Close()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.AutoCooldown = true
	cfg.ManagementURL = srv.URL
	cfg.ManagementKey = "k"
	st := state.New(filepath.Join(dir, "s.json"))
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: "free_usage_429", Label: "429", Enabled: true, Action: "cooldown",
		Threshold: 1, CooldownSec: 3600, CountMode: "streak",
		Escalations: []state.EscalationRule{{Streak: 1, Action: "cooldown", CooldownSec: 3600}},
	})
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))
	now := time.Now()
	g.Now = func() time.Time { return now }

	body := `{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5 for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 1091108/1000000."}`
	ev := guard.UsageEvent{
		Provider: "xai", AuthIndex: "h1", FileName: "xai-a@x.com.json", Email: "a@x.com",
		StatusCode: 429, Body: body, Source: "usage",
	}
	if err := g.HandleUsage(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	// first cool must have landed
	n1 := 0
	for _, e := range st.SnapshotLogs() {
		if e.Action == "cooldown" && e.Auth == "h1" {
			n1++
		}
	}
	if n1 != 1 {
		acc := st.Get("h1")
		t.Fatalf("first cool: want 1 cooldown log, got %d state=%+v", n1, acc)
	}
	// second fail 3s later — must not emit second cooldown log
	now = now.Add(3 * time.Second)
	if err := g.HandleUsage(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range st.SnapshotLogs() {
		if e.Action == "cooldown" && e.Auth == "h1" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want 1 cooldown log after idempotent re-entry, got %d", n)
	}
	acc := st.Get("h1")
	if acc == nil || acc.State != state.CooldownQuota {
		t.Fatalf("want still cooldown_quota, got %+v", acc)
	}
}

func TestAnyErrorStreakDoesNotOverwriteLastSignal(t *testing.T) {
	st := state.New("")
	st.IncStreak("a1", "free_usage_429")
	st.IncStreak("a1", "any_error")
	acc := st.Get("a1")
	if acc.LastSignal != "free_usage_429" {
		t.Fatalf("last_signal want free_usage_429, got %q", acc.LastSignal)
	}
	if acc.Streaks["any_error"] != 1 || acc.Streaks["free_usage_429"] != 1 {
		t.Fatalf("streaks %+v", acc.Streaks)
	}
}
