package policy

import (
	"github.com/openclaw-local/cpa-xai-sentry/internal/errorsig"
	"github.com/openclaw-local/cpa-xai-sentry/internal/match"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/tier"
)

type Action struct {
	Cooldown    bool
	Candidate   bool
	Disable     bool // permanent disable
	Trash       bool
	Reason      string
	ErrorKey    string
	PolicyAct   string
	CooldownSec int // from matched tier, 0 = use defaults
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
	tiers := p.NormalizedEscalations()
	// pick highest tier whose streak threshold is met
	var matched *state.EscalationRule
	for i := range tiers {
		if in.Streak >= tiers[i].Streak {
			matched = &tiers[i]
		}
	}
	if matched == nil {
		a.Reason = "未达该错误连续阈值"
		return a
	}
	a.PolicyAct = matched.Action
	a.CooldownSec = matched.CooldownSec
	// never_trash only from panel/policy field — no hard key ban
	return applyActionTier(cfg, a, errorsig.Action(matched.Action), p.NeverTrash, in, matched.Streak)
}

func applyActionTier(cfg sentrycfg.Config, a Action, act errorsig.Action, neverTrash bool, in Input, th int) Action {
	switch act {
	case errorsig.ActionObserve:
		a.Reason = "策略仅观察 · 连续≥" + itoa(th)
	case errorsig.ActionCooldown:
		if cfg.AutoCooldown {
			a.Cooldown = true
			a.Reason = "策略阶梯冷却 · 连续≥" + itoa(th)
		} else {
			a.Reason = "策略=冷却但全局自动冷却关闭"
		}
	case errorsig.ActionCandidate:
		if cfg.AutoCandidate {
			a.Candidate = true
			a.Cooldown = true
			a.Reason = "策略阶梯候删 · 连续≥" + itoa(th)
		} else if cfg.AutoCooldown {
			a.Cooldown = true
			a.Reason = "策略=候删但全局仅冷却开启"
		} else {
			a.Reason = "策略=候删但全局自动候选关闭"
		}
	case errorsig.ActionDisable:
		// permanent disable requires master sentry on; not gated by auto_delete
		if cfg.SentryEnabled {
			a.Disable = true
			a.Reason = "策略阶梯永久禁用 · 连续≥" + itoa(th)
		} else {
			a.Reason = "策略=永久禁用但总开关关闭"
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
			a.Reason = "策略阶梯进垃圾箱 · 连续≥" + itoa(th)
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
	return a
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func contains(ss []string, x string) bool {
	for _, s := range ss {
		if s == x {
			return true
		}
	}
	return false
}
