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

// any_error enabled: consecutive failures across types trigger ladder.
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
	// per-signal 403 needs 3; we only fire 1x403 + 1x401 + 1x unmatched-ish body → any_error=3
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: "permission_403", Enabled: true, Action: "cooldown", Threshold: 99, CountMode: "streak",
		Escalations: []state.EscalationRule{{Streak: 99, Action: "observe"}},
	})
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: "auth_401", Enabled: true, Action: "candidate", Threshold: 99, CountMode: "streak",
		Escalations: []state.EscalationRule{{Streak: 99, Action: "observe"}},
	})
	_ = st.Save()
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), nil)

	evs := []guard.UsageEvent{
		{AuthIndex: "a1", FileName: "xai-a1@lovc.eu.cc.json", Email: "a1@lovc.eu.cc", Provider: "xai",
			StatusCode: 403, Body: `{"error":"permission denied"}`, Source: "usage", Model: "grok-4.5"},
		{AuthIndex: "a1", FileName: "xai-a1@lovc.eu.cc.json", Email: "a1@lovc.eu.cc", Provider: "xai",
			StatusCode: 401, Body: `{"error":"Authentication required"}`, Source: "usage", Model: "grok-3"},
		{AuthIndex: "a1", FileName: "xai-a1@lovc.eu.cc.json", Email: "a1@lovc.eu.cc", Provider: "xai",
			StatusCode: 403, Body: `{"error":"permission denied"}`, Source: "usage", Model: "grok-4.5"},
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
		// 403 cool types as permission when signal is 403
		t.Fatalf("state=%s want cool (any_error)", acc.State)
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
