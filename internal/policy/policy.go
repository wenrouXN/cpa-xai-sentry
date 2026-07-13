package policy

import (
	"github.com/openclaw-local/cpa-xai-sentry/internal/match"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/tier"
)

type Action struct {
	Cooldown  bool
	Candidate bool
	Trash     bool
	Reason    string
}

type Input struct {
	Signal match.Signal
	Streak int
	Tier   tier.Tier
}

func Decide(cfg sentrycfg.Config, in Input) Action {
	cfg = cfg.Validate()
	if in.Signal == match.SignalNone {
		return Action{Reason: "no_signal"}
	}
	th := cfg.SignalThresholds[string(in.Signal)]
	if th <= 0 {
		th = 1
	}
	if in.Streak < th {
		return Action{Reason: "below_threshold"}
	}
	sig := string(in.Signal)
	var a Action
	if cfg.AutoCooldown && contains(cfg.CooldownSignals, sig) {
		a.Cooldown = true
		a.Reason = "cooldown"
	}
	if cfg.AutoCandidate && contains(cfg.CandidateSignals, sig) {
		a.Candidate = true
		a.Cooldown = true
		a.Reason = "candidate"
	}
	// hard ban: spending limit never auto-trash
	if in.Signal == match.SignalSpendingLimit402 {
		if a.Reason == "" {
			a.Reason = "spending_never_trash"
		}
		return a
	}
	if cfg.AutoDelete && contains(cfg.DeleteSignals, sig) && !tier.ProtectFromAutoTrash(in.Tier) {
		a.Trash = true
		a.Cooldown = true
		a.Candidate = true
		a.Reason = "auto_trash"
	}
	if a.Reason == "" {
		a.Reason = "no_action"
	}
	return a
}

func contains(ss []string, x string) bool {
	for _, s := range ss {
		if s == x {
			return true
		}
	}
	return false
}
