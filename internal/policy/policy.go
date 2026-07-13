package policy

import (
	"github.com/openclaw-local/cpa-xai-sentry/internal/errorsig"
	"github.com/openclaw-local/cpa-xai-sentry/internal/match"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/tier"
)

type Action struct {
	Cooldown  bool
	Candidate bool
	Trash     bool
	Reason    string
	ErrorKey  string
	PolicyAct string
}

type Input struct {
	Signal   match.Signal
	ErrorKey string
	Streak   int
	Tier     tier.Tier
	// optional per-error policy; if empty falls back to legacy lists
	Policy *state.ErrorPolicy
}

func Decide(cfg sentrycfg.Config, in Input) Action {
	cfg = cfg.Validate()
	key := in.ErrorKey
	if key == "" {
		key = string(in.Signal)
	}
	if key == "" || in.Signal == match.SignalNone && key == "unmatched" {
		// unmatched without policy action stays observe
	}

	// Prefer dynamic per-error policy when present.
	if in.Policy != nil && in.Policy.Key != "" {
		return decidePolicy(cfg, in, *in.Policy)
	}

	// Legacy global switches + scopes
	if in.Signal == match.SignalNone {
		return Action{Reason: "无匹配信号", ErrorKey: key}
	}
	th := cfg.SignalThresholds[string(in.Signal)]
	if th <= 0 {
		th = 1
	}
	if in.Streak < th {
		return Action{Reason: "未达连续阈值", ErrorKey: key}
	}
	sig := string(in.Signal)
	var a Action
	a.ErrorKey = key
	if cfg.AutoCooldown && contains(cfg.CooldownSignals, sig) {
		a.Cooldown = true
		a.Reason = "全局冷却策略"
		a.PolicyAct = string(errorsig.ActionCooldown)
	}
	if cfg.AutoCandidate && contains(cfg.CandidateSignals, sig) {
		a.Candidate = true
		a.Cooldown = true
		a.Reason = "全局候选策略"
		a.PolicyAct = string(errorsig.ActionCandidate)
	}
	if in.Signal == match.SignalSpendingLimit402 || errorsig.HardNeverTrash(key) {
		if a.Reason == "" {
			a.Reason = "消费限额禁止删除"
		}
		return a
	}
	if cfg.AutoDelete && contains(cfg.DeleteSignals, sig) && !tier.ProtectFromAutoTrash(in.Tier) {
		a.Trash = true
		a.Cooldown = true
		a.Candidate = true
		a.Reason = "全局自动垃圾箱"
		a.PolicyAct = string(errorsig.ActionTrash)
	}
	if a.Reason == "" {
		a.Reason = "无自动动作"
	}
	return a
}

func decidePolicy(cfg sentrycfg.Config, in Input, p state.ErrorPolicy) Action {
	a := Action{ErrorKey: p.Key, PolicyAct: p.Action}
	if !p.Enabled {
		a.Reason = "该错误策略已关闭"
		return a
	}
	// master sentry still gates in guard; here only policy
	th := p.Threshold
	if th <= 0 {
		th = 1
	}
	if in.Streak < th {
		a.Reason = "未达该错误连续阈值"
		return a
	}
	neverTrash := p.NeverTrash || errorsig.HardNeverTrash(p.Key) || in.Signal == match.SignalSpendingLimit402
	switch errorsig.Action(p.Action) {
	case errorsig.ActionObserve:
		a.Reason = "策略=仅观察"
	case errorsig.ActionCooldown:
		if !cfg.AutoCooldown && !cfg.SentryEnabled {
			// still allow when policy path used under sentry; guard checks master switches
		}
		// require global auto_cooldown OR treat policy as intent when auto_cooldown true
		if cfg.AutoCooldown || cfg.AutoCandidate || cfg.AutoDelete {
			a.Cooldown = cfg.AutoCooldown || cfg.AutoCandidate || cfg.AutoDelete
		}
		// If only policy-driven and global auto_cooldown on:
		if cfg.AutoCooldown {
			a.Cooldown = true
			a.Reason = "策略=冷却"
		} else {
			a.Reason = "策略=冷却但全局自动冷却关闭"
		}
	case errorsig.ActionCandidate:
		if cfg.AutoCandidate {
			a.Candidate = true
			a.Cooldown = true
			a.Reason = "策略=候选"
		} else if cfg.AutoCooldown {
			a.Cooldown = true
			a.Reason = "策略=候选但全局仅冷却开启"
		} else {
			a.Reason = "策略=候选但全局自动候选关闭"
		}
	case errorsig.ActionTrash:
		if neverTrash {
			if cfg.AutoCooldown {
				a.Cooldown = true
			}
			a.Reason = "策略想删除但硬禁止"
			return a
		}
		if tier.ProtectFromAutoTrash(in.Tier) {
			if cfg.AutoCandidate {
				a.Candidate = true
				a.Cooldown = true
			} else if cfg.AutoCooldown {
				a.Cooldown = true
			}
			a.Reason = "高价值套餐禁止自动垃圾箱"
			return a
		}
		if cfg.AutoDelete {
			a.Trash = true
			a.Candidate = true
			a.Cooldown = true
			a.Reason = "策略=进垃圾箱"
		} else if cfg.AutoCandidate {
			a.Candidate = true
			a.Cooldown = true
			a.Reason = "策略=删除但全局自动垃圾箱关闭→降级候选"
		} else if cfg.AutoCooldown {
			a.Cooldown = true
			a.Reason = "策略=删除但全局自动垃圾箱关闭→降级冷却"
		} else {
			a.Reason = "策略=删除但所有自动动作关闭"
		}
	default:
		a.Reason = "未知策略动作"
	}
	// Global delete scope can upgrade any non-protected error after threshold.
	if cfg.AutoDelete && !neverTrash && !tier.ProtectFromAutoTrash(in.Tier) {
		if contains(cfg.DeleteSignals, p.Key) || contains(cfg.DeleteSignals, string(in.Signal)) {
			a.Trash = true
			a.Candidate = true
			a.Cooldown = true
			a.Reason = "全局删除范围覆盖该错误"
			a.PolicyAct = string(errorsig.ActionTrash)
		}
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
