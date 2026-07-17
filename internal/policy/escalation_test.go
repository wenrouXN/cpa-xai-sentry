package policy

import (
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/match"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/tier"
)

func TestEscalationLadder403(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.AutoCooldown = true
	cfg.AutoCandidate = true
	p := state.ErrorPolicy{
		Key: "permission_403", Enabled: true, CountMode: "streak",
		Escalations: []state.EscalationRule{
			{Streak: 3, Action: "cooldown", CooldownSec: 1800},
			{Streak: 15, Action: "disable"},
		},
	}
	// streak 2 -> none
	a := Decide(cfg, Input{Signal: match.SignalPermission403, ErrorKey: "permission_403", Streak: 2, Tier: tier.Unknown, Policy: &p})
	if a.Cooldown || a.Disable {
		t.Fatalf("streak2 should observe, got cooldown=%v disable=%v reason=%s", a.Cooldown, a.Disable, a.Reason)
	}
	// streak 3 -> cool
	a = Decide(cfg, Input{Signal: match.SignalPermission403, ErrorKey: "permission_403", Streak: 3, Tier: tier.Unknown, Policy: &p})
	if !a.Cooldown || a.Disable {
		t.Fatalf("streak3 want cool, got cool=%v disable=%v reason=%s", a.Cooldown, a.Disable, a.Reason)
	}
	if a.CooldownSec != 1800 {
		t.Fatalf("cd=%d", a.CooldownSec)
	}
	// streak 15 -> disable
	a = Decide(cfg, Input{Signal: match.SignalPermission403, ErrorKey: "permission_403", Streak: 15, Tier: tier.Unknown, Policy: &p})
	if !a.Disable {
		t.Fatalf("streak15 want disable, got cool=%v disable=%v reason=%s", a.Cooldown, a.Disable, a.Reason)
	}
}

// Inverted ladder (user trap): disable@1 + candidate@5 must stay permanent disable,
// not demote to 候删 when streak climbs past 5 (old last-match-by-streak bug).
func TestEscalationSeverityWinsOverHigherStreak(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.AutoCooldown = true
	cfg.AutoCandidate = true
	p := state.ErrorPolicy{
		Key: "permission_403", Enabled: true, CountMode: "streak",
		Escalations: []state.EscalationRule{
			{Streak: 1, Action: "disable"},
			{Streak: 5, Action: "candidate"},
		},
	}
	a := Decide(cfg, Input{Signal: match.SignalPermission403, ErrorKey: "permission_403", Streak: 5, Tier: tier.Unknown, Policy: &p})
	if !a.Disable || a.Candidate {
		t.Fatalf("streak5 inverted ladder want disable only, got disable=%v candidate=%v cool=%v reason=%s",
			a.Disable, a.Candidate, a.Cooldown, a.Reason)
	}
	a = Decide(cfg, Input{Signal: match.SignalPermission403, ErrorKey: "permission_403", Streak: 19, Tier: tier.Unknown, Policy: &p})
	if !a.Disable || a.Candidate {
		t.Fatalf("streak19 want disable only, got disable=%v candidate=%v reason=%s", a.Disable, a.Candidate, a.Reason)
	}
}
