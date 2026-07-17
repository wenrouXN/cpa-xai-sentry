package policy_test

import (
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/match"
	"github.com/openclaw-local/cpa-xai-sentry/internal/policy"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/tier"
)

// 402 no longer hard-blocks trash: global delete_signals + auto_delete must work.
func Test402CanTrashWhenConfigured(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoDelete = true
	cfg.AutoCooldown = true
	cfg.DeleteSignals = []string{"spending_limit_402", "auth_401"}
	cfg = cfg.Validate()
	if !contains(cfg.DeleteSignals, "spending_limit_402") {
		t.Fatal("validate must keep 402 in delete_signals when configured")
	}
	d := policy.Decide(cfg, policy.Input{
		Signal: match.SignalSpendingLimit402, Streak: 5, Tier: tier.Free,
	})
	if !d.Trash {
		t.Fatalf("402 should trash when in delete_signals, got %+v", d)
	}
}

// Policy ladder trash on 402 when never_trash is false.
func Test402PolicyLadderTrash(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoDelete = true
	cfg.SentryEnabled = true
	pol := state.ErrorPolicy{
		Key: "spending_limit_402", Enabled: true, Action: "trash", Threshold: 1,
		Escalations: []state.EscalationRule{{Streak: 1, Action: "trash"}},
		NeverTrash:  false,
	}
	d := policy.Decide(cfg, policy.Input{
		Signal: match.SignalSpendingLimit402, ErrorKey: "spending_limit_402",
		Streak: 1, Tier: tier.Free, Policy: &pol,
	})
	if !d.Trash {
		t.Fatalf("want trash from ladder, got %+v", d)
	}
}

// Explicit never_trash on policy still blocks (optional panel flag, not hard key).
func TestPolicyNeverTrashFlagStillHonored(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoDelete = true
	cfg.SentryEnabled = true
	pol := state.ErrorPolicy{
		Key: "spending_limit_402", Enabled: true,
		Escalations: []state.EscalationRule{{Streak: 1, Action: "trash"}},
		NeverTrash:  true,
	}
	d := policy.Decide(cfg, policy.Input{
		Signal: match.SignalSpendingLimit402, ErrorKey: "spending_limit_402",
		Streak: 1, Tier: tier.Free, Policy: &pol,
	})
	if d.Trash {
		t.Fatal("policy never_trash=true must still block trash")
	}
}

func TestSuperNoAutoTrash(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoDelete = true
	cfg.DeleteSignals = []string{"auth_401"}
	d := policy.Decide(cfg, policy.Input{Signal: match.SignalAuth401, Streak: 9, Tier: tier.Super})
	if d.Trash {
		t.Fatal("super protected")
	}
}

func TestFreeAuth401AutoTrash(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoDelete = true
	cfg.DeleteSignals = []string{"auth_401"}
	d := policy.Decide(cfg, policy.Input{Signal: match.SignalAuth401, Streak: 2, Tier: tier.Free})
	if !d.Trash {
		t.Fatalf("want trash, got %+v", d)
	}
}

func TestPermissionDefaultNoCandidate(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoCandidate = true
	d := policy.Decide(cfg, policy.Input{Signal: match.SignalPermission403, Streak: 10, Tier: tier.Free})
	if d.Candidate {
		t.Fatal("permission not in default candidate_signals")
	}
}

func TestBelowThreshold(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoDelete = true
	cfg.DeleteSignals = []string{"auth_401"}
	d := policy.Decide(cfg, policy.Input{Signal: match.SignalAuth401, Streak: 1, Tier: tier.Free})
	if d.Trash {
		t.Fatal("streak 1 < threshold 2")
	}
}

func contains(ss []string, x string) bool {
	for _, s := range ss {
		if s == x {
			return true
		}
	}
	return false
}
