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

func TestPermission403StreakSurvivesCooldownRecover(t *testing.T) {
	dir := t.TempDir()
	disabled := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{
				"name": "xai-a@x.com.json", "email": "a@x.com", "auth_index": "h1",
				"provider": "xai", "type": "xai", "disabled": disabled["xai-a@x.com.json"],
			}}})
		case r.URL.Path == "/v0/management/auth-files/status":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if n, _ := body["name"].(string); n != "" {
				if d, ok := body["disabled"].(bool); ok {
					disabled[n] = d
				}
			}
			w.WriteHeader(200)
		default:
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.AutoCooldown = true
	cfg.ManagementURL = srv.URL
	cfg.ManagementKey = "k"
	cfg.PermissionCooldownSec = 1
	st := state.New(filepath.Join(dir, "s.json"))
	// seed 403 ladder like production
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: "permission_403", Label: "403", Enabled: true, Action: "cooldown",
		Threshold: 3, CooldownSec: 1, CountMode: "streak",
		Escalations: []state.EscalationRule{
			{Streak: 3, Action: "cooldown", CooldownSec: 1},
			{Streak: 15, Action: "disable"},
		},
	})
	ts := trash.New(filepath.Join(dir, "t"), 7, true, st)
	g := guard.New(cfg, st, ts, cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))
	body403 := `{"code":"permission-denied","error":"Access to the chat endpoint is denied."}`
	// 3 fails → cool-down, streak=3
	for i := 0; i < 3; i++ {
		_ = g.HandleUsage(context.Background(), guard.UsageEvent{
			Provider: "xai", AuthIndex: "h1", FileName: "xai-a@x.com.json", Email: "a@x.com",
			StatusCode: 403, Body: body403, Source: "usage",
		})
	}
	acc := st.Get("h1")
	if acc.State != state.CooldownPermission || acc.Streaks["permission_403"] != 3 {
		t.Fatalf("after 3: state=%s streaks=%v", acc.State, acc.Streaks)
	}
	// expire cool-down
	st.SetRecoverAt("h1", time.Now().Add(-time.Second))
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	acc = st.Get("h1")
	if acc.State != state.Active {
		t.Fatalf("want active after recover, got %s", acc.State)
	}
	if acc.Streaks["permission_403"] != 3 {
		t.Fatalf("streak must survive recover, got %v", acc.Streaks)
	}
	// 4th fail → streak 4, still cool-down tier (not yet 15)
	_ = g.HandleUsage(context.Background(), guard.UsageEvent{
		Provider: "xai", AuthIndex: "h1", FileName: "xai-a@x.com.json", Email: "a@x.com",
		StatusCode: 403, Body: body403, Source: "usage",
	})
	acc = st.Get("h1")
	if acc.Streaks["permission_403"] != 4 {
		t.Fatalf("want streak 4, got %v", acc.Streaks)
	}
}
