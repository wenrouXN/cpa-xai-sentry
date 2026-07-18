package guard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/errorfp"
	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

// Replays sanitized bodies sampled from production CPAMP. It validates one account's
// lifecycle without issuing any external xAI request.
func TestRealLogLifecycleReplay(t *testing.T) {
	dir := t.TempDir()
	disabled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files/status":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if v, ok := body["disabled"].(bool); ok {
				disabled = v
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{
				"name": "xai-replay.json", "auth_index": "replay", "provider": "xai", "disabled": disabled,
			}}})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	cfg := sentrycfg.Default()
	cfg.SentryEnabled, cfg.AutoCooldown = true, true
	cfg.ManagementURL, cfg.ManagementKey = srv.URL, "k"
	st := state.New(filepath.Join(dir, "state.json"))
	st.Touch("replay")
	st.UpdateMeta("replay", "xai-replay.json", "replay@example.invalid", "")
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "trash"), 7, true, st), cpaapi.New(srv.URL, "k", dir))

	permission := `{"code":"permission-denied","error":"Access to the chat endpoint is denied. Please ensure you're using the correct credentials."}`
	for i := 0; i < 3; i++ {
		if err := g.HandleUsage(context.Background(), guard.UsageEvent{
			AuthIndex: "replay", FileName: "xai-replay.json", Provider: "xai",
			StatusCode: 403, Body: permission, Source: "usage",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := st.Get("replay"); got.State != state.CooldownPermission || !disabled {
		t.Fatalf("permission lifecycle must cool+close: account=%+v disabled=%v", got, disabled)
	}

	// A selected patrol returning real 2xx is authoritative, regardless of previous state/range.
	if err := g.HandleUsage(context.Background(), guard.UsageEvent{
		AuthIndex: "replay", FileName: "xai-replay.json", Provider: "xai",
		StatusCode: 200, Success: true, Body: `{"ok":true}`, Source: "patrol",
	}); err != nil {
		t.Fatal(err)
	}
	if got := st.Get("replay"); got.State != state.Active || got.LastSignal != "" || disabled {
		t.Fatalf("real patrol 2xx must open+activate+clear current error: account=%+v disabled=%v", got, disabled)
	}

	// The next actual error is a distinct 402 fingerprint and gets its own policy/state.
	spending := `{"code":"personal-team-blocked:spending-limit","error":"You have run out of credits or need a Grok subscription."}`
	fp := errorfp.Build(spending, 402)
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: fp.SuggestKey, Label: "消费限额", Enabled: true, Action: "cooldown",
		Threshold: 1, CooldownSec: 3600, CountMode: "streak", SplitShape: fp.Shape,
		Escalations: []state.EscalationRule{{Streak: 1, Action: "cooldown", CooldownSec: 3600}},
	})
	if err := g.HandleUsage(context.Background(), guard.UsageEvent{
		AuthIndex: "replay", FileName: "xai-replay.json", Provider: "xai",
		StatusCode: 402, Body: spending, Source: "patrol",
	}); err != nil {
		t.Fatal(err)
	}
	got := st.Get("replay")
	if got.State != state.CooldownSpending || got.LastSignal != fp.SuggestKey || got.Streaks[fp.SuggestKey] != 1 || !disabled {
		t.Fatalf("changed real error must enter its own lifecycle: fp=%s account=%+v disabled=%v", fp.SuggestKey, got, disabled)
	}
}
