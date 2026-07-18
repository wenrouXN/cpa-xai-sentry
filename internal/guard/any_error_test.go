package guard_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

// any_error enabled: consecutive failures across *classified* types trigger ladder.
// Unmatched-only failures must NOT be upgraded by any_error (see TestAnyErrorDoesNotUpgradeUnmatched).
func TestAnyErrorLadderCoversMixedSignals(t *testing.T) {
	dir := t.TempDir()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.AutoCooldown = true
	st := state.New(filepath.Join(dir, "s.json"))
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: "any_error", Label: "任意错误·连续", Enabled: true,
		Action: "cooldown", Threshold: 3, CooldownSec: 600, CountMode: "streak",
		Escalations: []state.EscalationRule{{Streak: 3, Action: "cooldown", CooldownSec: 600}},
		Source:      "builtin",
	})
	// per-signal needs high threshold so only any_error fires
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: "permission_403", Enabled: true, Action: "cooldown", Threshold: 99, CountMode: "streak",
		Escalations: []state.EscalationRule{{Streak: 99, Action: "observe"}},
	})
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: "free_usage_429", Enabled: true, Action: "cooldown", Threshold: 99, CountMode: "streak",
		Escalations: []state.EscalationRule{{Streak: 99, Action: "observe"}},
	})
	_ = st.Save()
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), nil)

	permBody := `{"code":"permission-denied","error":"Access to the chat endpoint is denied. Please ensure you're using the correct credentials."}`
	freeBody := `{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model x"}`
	evs := []guard.UsageEvent{
		{AuthIndex: "a1", FileName: "xai-a1@lovc.eu.cc.json", Email: "a1@lovc.eu.cc", Provider: "xai",
			StatusCode: 403, Body: permBody, Source: "usage", Model: "grok-4.5"},
		{AuthIndex: "a1", FileName: "xai-a1@lovc.eu.cc.json", Email: "a1@lovc.eu.cc", Provider: "xai",
			StatusCode: 429, Body: freeBody, Source: "usage", Model: "grok-3"},
		{AuthIndex: "a1", FileName: "xai-a1@lovc.eu.cc.json", Email: "a1@lovc.eu.cc", Provider: "xai",
			StatusCode: 403, Body: permBody, Source: "usage", Model: "grok-4.5"},
	}
	for _, ev := range evs {
		if err := g.HandleUsage(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	acc := st.Get("a1")
	if acc == nil {
		t.Fatal("missing account")
	}
	if acc.Streaks["any_error"] < 3 {
		t.Fatalf("any_error streak=%v want ≥3", acc.Streaks)
	}
	// should be in cool due to any_error ≥3
	if acc.State != state.CooldownPermission && acc.State != state.CooldownQuota && acc.State != state.CooldownSpending {
		t.Fatalf("state=%s want cool (any_error)", acc.State)
	}
}

// any_error must never demote the unmatched observe-only bucket.
func TestAnyErrorDoesNotUpgradeUnmatched(t *testing.T) {
	dir := t.TempDir()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.AutoCooldown = true
	st := state.New(filepath.Join(dir, "s.json"))
	st.EnsureBuiltinPolicies(map[string]state.ErrorPolicy{
		"unmatched": {Key: "unmatched", Enabled: true, Action: "observe", Threshold: 1, Source: "builtin"},
	})
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: "any_error", Enabled: true, Action: "cooldown", Threshold: 2, CooldownSec: 600, CountMode: "streak",
		Escalations: []state.EscalationRule{{Streak: 2, Action: "cooldown", CooldownSec: 600}},
		Source:      "builtin",
	})
	_ = st.Save()
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), nil)
	body := "local error: tls: bad record MAC"
	for i := 0; i < 3; i++ {
		if err := g.HandleUsage(context.Background(), guard.UsageEvent{
			AuthIndex: "u1", FileName: "xai-u1@lovc.eu.cc.json", Email: "u1@lovc.eu.cc", Provider: "xai",
			StatusCode: 0, Body: body, Source: "usage",
		}); err != nil {
			t.Fatal(err)
		}
	}
	acc := st.Get("u1")
	if acc == nil || acc.LastSignal != "unmatched" {
		t.Fatalf("want unmatched last_signal, got %+v", acc)
	}
	if acc.State != state.Active && acc.State != "" {
		t.Fatalf("unmatched must stay observe-only, state=%s", acc.State)
	}
	if acc.Streaks["any_error"] < 2 {
		t.Fatalf("any_error may still count, got %v", acc.Streaks)
	}
}

func TestObserveErrorStoresModel(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "s.json"))
	st.ObserveError("permission_403", "403", "permission_403", "x", "sample", "auth1", "f.json", "usage", 403, "grok-4.5")
	obs := st.ListObserved()
	var hit *state.ErrorHit
	for i := range obs {
		if obs[i].Key != "permission_403" {
			continue
		}
		if len(obs[i].Hits) > 0 {
			hit = &obs[i].Hits[len(obs[i].Hits)-1]
		}
	}
	if hit == nil || hit.Model != "grok-4.5" {
		t.Fatalf("want model grok-4.5, hit=%+v", hit)
	}
}
