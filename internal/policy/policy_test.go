package policy_test

import (
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/match"
	"github.com/openclaw-local/cpa-xai-sentry/internal/policy"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/tier"
)

// 402 is NOT a builtin; global delete_signals alone must not act without policy.
func Test402CanTrashWhenConfigured(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoDelete = true
	cfg.AutoCooldown = true
	cfg.DeleteSignals = []string{"spending_limit_402", "auth_401"}
	cfg = cfg.Validate()
	if !contains(cfg.DeleteSignals, "spending_limit_402") {
		t.Fatal("validate must keep 402 in delete_signals when configured")
	}
	// no policy → observe only
	d := policy.Decide(cfg, policy.Input{
		Signal: match.SignalSpendingLimit402, ErrorKey: "reason:fp_spending", Streak: 5, Tier: tier.Free,
	})
	if d.Trash || d.Cooldown {
		t.Fatalf("402 fingerprint without policy must only observe, got %+v", d)
	}
	// with explicit policy → trash
	pol := state.ErrorPolicy{
		Key: "reason:fp_spending", Enabled: true,
		Escalations: []state.EscalationRule{{Streak: 1, Action: "trash"}},
	}
	d = policy.Decide(cfg, policy.Input{
		Signal: match.SignalSpendingLimit402, ErrorKey: "reason:fp_spending",
		Streak: 1, Tier: tier.Free, Policy: &pol,
	})
	if !d.Trash {
		t.Fatalf("402 should trash when explicit policy says so, got %+v", d)
	}
}

// Policy ladder trash on 402 when never_trash is false.
func Test402PolicyLadderTrash(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoDelete = true
	cfg.SentryEnabled = true
	pol := state.ErrorPolicy{
		Key: "reason:fp_spending", Enabled: true, Action: "trash", Threshold: 1,
		Escalations: []state.EscalationRule{{Streak: 1, Action: "trash"}},
		NeverTrash:  false,
	}
	d := policy.Decide(cfg, policy.Input{
		Signal: match.SignalSpendingLimit402, ErrorKey: "reason:fp_spending",
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
		Key: "reason:fp_spending", Enabled: true,
		Escalations: []state.EscalationRule{{Streak: 1, Action: "trash"}},
		NeverTrash:  true,
	}
	d := policy.Decide(cfg, policy.Input{
		Signal: match.SignalSpendingLimit402, ErrorKey: "reason:fp_spending",
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
	// even with global delete_signals, non-builtin without policy must not trash
	d := policy.Decide(cfg, policy.Input{Signal: match.SignalAuth401, ErrorKey: "reason:fp_auth", Streak: 9, Tier: tier.Super})
	if d.Trash {
		t.Fatal("super protected / no policy")
	}
	// with policy trash still blocked for super
	pol := state.ErrorPolicy{
		Key: "reason:fp_auth", Enabled: true,
		Escalations: []state.EscalationRule{{Streak: 1, Action: "trash"}},
	}
	d = policy.Decide(cfg, policy.Input{
		Signal: match.SignalAuth401, ErrorKey: "reason:fp_auth", Streak: 9, Tier: tier.Super, Policy: &pol,
	})
	if d.Trash {
		t.Fatal("super protected from trash policy")
	}
}

func TestFreeAuth401AutoTrash(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoDelete = true
	cfg.DeleteSignals = []string{"auth_401"}
	// without policy: observe only
	d := policy.Decide(cfg, policy.Input{Signal: match.SignalAuth401, ErrorKey: "reason:fp_auth", Streak: 2, Tier: tier.Free})
	if d.Trash {
		t.Fatalf("auth fingerprint without policy must not trash, got %+v", d)
	}
	pol := state.ErrorPolicy{
		Key: "reason:fp_auth", Enabled: true,
		Escalations: []state.EscalationRule{{Streak: 2, Action: "trash"}},
	}
	d = policy.Decide(cfg, policy.Input{
		Signal: match.SignalAuth401, ErrorKey: "reason:fp_auth", Streak: 2, Tier: tier.Free, Policy: &pol,
	})
	if !d.Trash {
		t.Fatalf("want trash from explicit policy, got %+v", d)
	}
}

func TestPermissionDefaultNoCandidate(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoCandidate = true
	// builtin permission with no policy falls to global; candidate not in default list
	d := policy.Decide(cfg, policy.Input{Signal: match.SignalPermission403, ErrorKey: "permission_403", Streak: 10, Tier: tier.Free})
	if d.Candidate {
		t.Fatal("permission not in default candidate_signals")
	}
}

func TestBelowThreshold(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoDelete = true
	cfg.DeleteSignals = []string{"auth_401"}
	// fingerprint without policy: no action at any streak
	d := policy.Decide(cfg, policy.Input{Signal: match.SignalAuth401, ErrorKey: "reason:fp_auth", Streak: 1, Tier: tier.Free})
	if d.Trash {
		t.Fatal("no policy must not trash")
	}
	// builtin free_usage with threshold
	cfg.SignalThresholds = map[string]int{"free_usage_429": 2}
	d = policy.Decide(cfg, policy.Input{Signal: match.SignalFreeUsage429, ErrorKey: "free_usage_429", Streak: 1, Tier: tier.Free})
	if d.Cooldown {
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
