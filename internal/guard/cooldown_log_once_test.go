package guard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

func TestApplyCooldownLogsOnce(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files/status" {
			w.WriteHeader(200)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{}})
	}))
	defer srv.Close()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.AutoCooldown = true
	cfg.ManagementURL = srv.URL
	cfg.ManagementKey = "k"
	st := state.New(filepath.Join(dir, "s.json"))
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: "permission_403", Label: "403", Enabled: true, Action: "cooldown",
		Threshold: 1, CooldownSec: 60, CountMode: "streak",
		Escalations: []state.EscalationRule{{Streak: 1, Action: "cooldown", CooldownSec: 60}},
	})
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))
	_ = g.HandleUsage(context.Background(), guard.UsageEvent{
		Provider: "xai", AuthIndex: "h", FileName: "xai-a@x.com.json", Email: "a@x.com",
		StatusCode: 403, Body: `{"code":"permission-denied","error":"denied"}`, Source: "patrol",
	})
	n := 0
	for _, e := range st.SnapshotLogs() {
		if e.Action == "cooldown" && e.Auth == "h" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly 1 cooldown log, got %d", n)
	}
}
