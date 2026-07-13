package policy_test

import (
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/match"
	"github.com/openclaw-local/cpa-xai-sentry/internal/policy"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/tier"
)

func Test402NeverTrash(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoDelete = true
	cfg.AutoCooldown = true
	cfg.DeleteSignals = []string{"spending_limit_402", "auth_401"}
	cfg = cfg.Validate()
	if contains(cfg.DeleteSignals, "spending_limit_402") {
		t.Fatal("validate must strip 402 from delete_signals")
	}
	d := policy.Decide(cfg, policy.Input{
		Signal: match.SignalSpendingLimit402, Streak: 5, Tier: tier.Free,
	})
	if d.Trash {
		t.Fatal("402 must not trash")
	}
	if !d.Cooldown {
		t.Fatal("402 should still cooldown when enabled")
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
