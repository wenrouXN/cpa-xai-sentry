package guard

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/errorfp"
	"github.com/openclaw-local/cpa-xai-sentry/internal/errorsig"
	"github.com/openclaw-local/cpa-xai-sentry/internal/match"
	"github.com/openclaw-local/cpa-xai-sentry/internal/policy"
	"github.com/openclaw-local/cpa-xai-sentry/internal/quota"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/tier"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

type Guard struct {
	Cfg   sentrycfg.Config
	State *state.Store
	// PatrolRunning, if set, returns true when a patrol job is in progress.
	// Used to skip pruneDuplicateAccounts during patrol to avoid deleting
	// accounts that patrol goroutines are still creating.
	PatrolRunning func() bool
	Trash         *trash.Store
	CPA           *cpaapi.Client
	Resolver      *cpaapi.Resolver
	// mu serializes HandleUsage / Tick / manual ops so cool-down ownership
	// cannot race with self-heal reopen.
	mu sync.Mutex
	// hooks for tests
	Now func() time.Time

	// listVerifyFailStreak: consecutive IsAuthFileDisabled list failures (verify path).
	// When high, skip trust-PATCH blind success and avoid empty reassert/heal thrash.
	listVerifyFailStreak int
	// lastListOK: last time verify list succeeded.
	lastListOK time.Time

	// pendingBackfillAuths: auth_index set by CPAMP fail backfill this tick cycle.
	// healActiveFileDisabled skips these so we never force-open then cool same second.
	pendingBackfillAuths map[string]struct{}

	// TryRelogin: optional hook for 8788-local password relogin on auth_401.
	// Return attempted=true to skip candidate path this hit.
	TryRelogin func(ctx context.Context, email, auth string) (attempted bool, reason string)
}

func New(cfg sentrycfg.Config, st *state.Store, tr *trash.Store, cpa *cpaapi.Client) *Guard {
	g := &Guard{
		Cfg: cfg.Validate(), State: st, Trash: tr, CPA: cpa, Now: time.Now,
		pendingBackfillAuths: map[string]struct{}{},
	}
	if cpa != nil {
		g.Resolver = cpaapi.NewResolver(cpa)
	}
	if st != nil {
		builtins := map[string]state.ErrorPolicy{}
		for k, p := range errorsig.BuiltinDefaults() {
			builtins[k] = state.ErrorPolicy{
				Key: p.Key, Label: p.Label, Enabled: p.Enabled, Action: string(p.Action),
				Threshold: p.Threshold, CooldownSec: p.CooldownSec, NeverTrash: p.NeverTrash,
				Note: p.Note, Source: p.Source,
			}
		}
		// any_error: global consecutive-any-failure ladder (disabled by default)
		if _, ok := builtins["any_error"]; !ok {
			builtins["any_error"] = state.ErrorPolicy{
				Key: "any_error", Label: "任意错误·连续", Enabled: false,
				Action: "observe", Threshold: 5, CooldownSec: 1800, CountMode: "streak",
				Escalations: []state.EscalationRule{
					{Streak: 5, Action: "cooldown", CooldownSec: 1800},
				},
				Note: "不管错误类型，连续失败达到 N 次按阶梯处置；默认关闭", Source: "builtin",
			}
		}
		st.EnsureBuiltinPolicies(builtins)
	}
	return g
}

// routeBySplitShape maps an unmatched body to a user-split policy key via any
// split route shape owned by the policy.
func (g *Guard) routeBySplitShape(body string, status int) string {
	if g.State == nil {
		return ""
	}
	fp := errorfp.Build(body, status)
	shape := fp.Shape
	if shape == "" {
		return ""
	}
	for _, p := range g.State.ListErrorPolicies() {
		if p.Key == "" || p.Key == "unmatched" || p.Key == "any_error" {
			continue
		}
		for _, ss := range p.SplitShapeList() {
			if ss == shape {
				return p.Key
			}
		}
	}
	return ""
}

func (g *Guard) enrichIdentity(ctx context.Context, ev *UsageEvent) {
	if ev == nil {
		return
	}
	// Prefer readable file name over opaque hash.
	if cpaapi.LooksLikeOpaqueID(ev.FileName) && !cpaapi.LooksLikeOpaqueID(ev.AuthIndex) && strings.Contains(ev.AuthIndex, "@") {
		ev.FileName = ev.AuthIndex
	}
	if g.Resolver != nil {
		_ = g.Resolver.Ensure(ctx)
		if id, ok := g.Resolver.Resolve(ev.AuthIndex, ev.FileName, ev.Email); ok {
			if id.FileName != "" {
				ev.FileName = id.FileName
			}
			if id.Email != "" {
				ev.Email = id.Email
			}
			if ev.Note == "" && id.Note != "" {
				ev.Note = id.Note
			}
			if ev.Label == "" && id.Label != "" {
				ev.Label = id.Label
			}
		}
	}
	if ev.Email == "" {
		ev.Email = cpaapi.EmailFromFileName(ev.FileName)
	}
	if ev.Email == "" {
		ev.Email = cpaapi.EmailFromFileName(ev.AuthIndex)
	}
}

func IsXAI(provider, fileName string) bool {
	p := strings.ToLower(provider)
	f := strings.ToLower(fileName)
	return p == "xai" || p == "grok" || strings.HasPrefix(f, "xai-") || strings.Contains(f, "grok")
}

type UsageEvent struct {
	Provider   string
	AuthIndex  string
	FileName   string
	Email      string
	StatusCode int
	Body       string
	Success    bool
	Source     string // usage|patrol
	Note       string
	Label      string
	Model      string // request model when known
}

// HandleUsage applies match+policy. source defaults to usage.
func (g *Guard) HandleUsage(ctx context.Context, ev UsageEvent) error {
	if !g.Cfg.Enabled || !g.Cfg.SentryEnabled {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.enrichIdentity(ctx, &ev)
	if !IsXAI(ev.Provider, ev.FileName) && !IsXAI(ev.Provider, ev.AuthIndex) && !IsXAI(ev.Provider, ev.Email) {
		return nil
	}
	if ev.Source == "" {
		ev.Source = "usage"
	}
	// Short-window dedupe: same auth + fail family within a few seconds
	// (double-publish / retry) — skip full cool path to avoid twin logs + recover stretch.
	if !(ev.Success || (ev.StatusCode >= 200 && ev.StatusCode < 300)) {
		if g.shouldDedupeFailUsage(ev) {
			return nil
		}
	}
	tierName := string(tier.Classify(ev.Note, ev.Label, ev.FileName, nil))
	g.State.UpdateMeta(ev.AuthIndex, ev.FileName, ev.Email, tierName)
	acc := g.State.Get(ev.AuthIndex)
	if acc == nil {
		acc = g.State.Touch(ev.AuthIndex)
	}
	// day usage counters (local)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	if loc == nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	day := g.Now().In(loc).Format("2006-01-02")
	failN := int64(0)
	if !(ev.Success || (ev.StatusCode >= 200 && ev.StatusCode < 300)) {
		failN = 1
	}
	g.State.IncDayUsage(ev.AuthIndex, day, 1, failN, 0)

	if ev.Success || (ev.StatusCode >= 200 && ev.StatusCode < 300) {
		g.clearStreaksByCountMode(ev.AuthIndex)
		// A real success is authoritative. Patrol first reopens any selected account;
		// then all success paths clear the current error lifecycle.
		if strings.EqualFold(ev.Source, "patrol") {
			g.reopenAfterProbeAlive(ctx, ev)
		}
		if acc := g.State.Get(ev.AuthIndex); acc != nil {
			if acc.LastSignal != "" {
				g.State.SetLastSignal(ev.AuthIndex, "")
			}
			if acc.PendingObserve {
				g.State.ClearPendingObserve(ev.AuthIndex)
			}
		}
		// success body may still carry remaining/used quota floors
		if q := quota.Parse(ev.Body); q.Limit > 0 || q.Remaining > 0 || q.Used > 0 || !q.ResetAt.IsZero() {
			g.State.UpdateQuota(ev.AuthIndex, q.Limit, q.Used, q.Remaining, q.Source, q.ResetAt)
		}
		return nil
	}

	res := match.Classify(ev.StatusCode, ev.Body)
	// best-effort quota parse from failure body
	q := quota.Parse(ev.Body)
	if res.Signal == match.SignalFreeUsage429 {
		q = quota.FreeUsageExhaustedEstimate(ev.Body, res.RecoverAt)
	}
	if q.Limit > 0 || q.Remaining > 0 || q.Used > 0 || !q.ResetAt.IsZero() {
		g.State.UpdateQuota(ev.AuthIndex, q.Limit, q.Used, q.Remaining, q.Source, q.ResetAt)
	}
	errKey := errorsig.KeyFromMatch(res, ev.StatusCode, ev.Body)
	shape, fingerprintLabel, _ := errorsig.ShapeOf(ev.Body, ev.StatusCode)
	// A saved split/merge mapping wins. Otherwise only strict builtins become
	// standalone classes; unfamiliar shapes stay in unmatched until user split.
	if k := g.routeBySplitShape(ev.Body, ev.StatusCode); k != "" {
		errKey = k
	}
	// User withdrew a class → new hits go to unmatched until explicitly merged/split again.
	if g.State != nil && g.State.IsPolicyHidden(errKey) {
		errKey = "unmatched"
	}
	// User-split shapes: route unmatched hits that match a SplitShape policy.
	if errKey == "unmatched" && g.State != nil {
		if k := g.routeBySplitShape(ev.Body, ev.StatusCode); k != "" {
			errKey = k
		}
	}
	label := errorsig.LabelOf(errKey, res, ev.StatusCode)
	if strings.HasPrefix(errKey, "reason:fp_") && fingerprintLabel != "" {
		label = fingerprintLabel
	}
	// The normalized fingerprint/class is the source of truth for logs and state.
	// Signal remains classifier metadata used for default cooldown subtype only.
	res.Reason = errKey
	// always learn/observe errors into dynamic catalog (even unmatched)
	// keep enough of body to retain tokens (actual/limit) for UI/quota rehydration
	sample := ev.Body
	if len(sample) > 900 {
		sample = sample[:900]
	}
	g.State.ObserveError(errKey, label, string(res.Signal), res.Code, sample, ev.AuthIndex, ev.FileName, ev.Source, ev.StatusCode, ev.Model, shape)

	// seed policy for builtins/unmatched only; user splits already have a card.
	// Skip if user explicitly deleted (hid) this policy — respect user's choice.
	if g.State != nil && !g.State.IsPolicyHidden(errKey) {
		if _, ok := g.State.GetErrorPolicy(errKey); !ok {
			if errKey == "free_usage_429" || errKey == "permission_403" || errKey == "unmatched" {
				act := "observe"
				th := 1
				cd := 0
				never := false
				if p0, ok := errorsig.BuiltinDefaults()[errKey]; ok {
					act = string(p0.Action)
					th = p0.Threshold
					cd = p0.CooldownSec
					never = p0.NeverTrash
					label = p0.Label
				}
				g.State.UpsertErrorPolicy(state.ErrorPolicy{
					Key: errKey, Label: label, Enabled: true, Action: act,
					Threshold: th, CooldownSec: cd, NeverTrash: never,
					Note: "动态采集", Source: "learned",
				})
			}
		}
	}

	if res.Signal == match.SignalNone {
		// Non-builtin fingerprints use the same policy engine as builtins.
		g.beginFingerprintRun(ev.AuthIndex, errKey)
		g.State.SetLastSignal(ev.AuthIndex, errKey)
		anyStreak := g.State.IncStreak(ev.AuthIndex, "any_error")
		var act policy.Action
		var polPtr *state.ErrorPolicy

		// 1) 具体指纹类 / 已路由策略
		if errKey != "" && errKey != "unmatched" && errKey != "any_error" {
			streak := g.State.IncStreak(ev.AuthIndex, errKey)
			if p, ok := g.State.GetErrorPolicy(errKey); ok {
				polPtr = &p
				act = policy.Decide(g.Cfg, policy.Input{
					Signal: res.Signal, ErrorKey: errKey, Streak: streak,
					Tier: tier.Tier(acc.Tier), Policy: &p,
				})
			}
		} else if errKey == "unmatched" {
			g.State.IncStreak(ev.AuthIndex, errKey)
		}

		// 2) 任意错误阶梯（若更强则覆盖）— never upgrades unmatched (observe-only bucket)
		if errKey != "unmatched" {
			if ap, ok := g.State.GetErrorPolicy("any_error"); ok && ap.Enabled {
				anyAct := policy.Decide(g.Cfg, policy.Input{
					Signal: res.Signal, ErrorKey: "any_error", Streak: anyStreak,
					Tier: tier.Tier(acc.Tier), Policy: &ap,
				})
				if policyActionRank(anyAct) > policyActionRank(act) {
					act = anyAct
					polPtr = &ap
					if act.Reason != "" && !strings.Contains(act.Reason, "any_error") && !strings.Contains(act.Reason, "任意错误") {
						act.Reason = act.Reason + " · 任意错误连续≥" + itoaGuard(anyStreak)
					}
				}
			}
		}

		// 3) 执行阶梯动作
		if act.Disable {
			if err := g.applyPermanentDisable(ctx, ev, act.Reason); err != nil {
				return err
			}
		} else if act.Cooldown {
			syn := res
			syn.Reason = act.Reason
			if err := g.applyCooldown(ctx, ev, syn, acc, polPtr, act.CooldownSec); err != nil {
				return err
			}
		}
		if act.Candidate && !act.Disable {
			name := ev.FileName
			if name == "" || cpaapi.LooksLikeOpaqueID(name) {
				if acc != nil {
					name = g.resolveFileName(ctx, ev.AuthIndex, acc.FileName, acc.Email)
				} else {
					name = g.resolveFileName(ctx, ev.AuthIndex, ev.FileName, ev.Email)
				}
			}
			g.State.SetAccountState(ev.AuthIndex, state.CandidateDead, "plugin_auto")
			// SignalNone path: never stamp short recover (stops 403-style loop for split keys)
			g.State.SetRecoverAt(ev.AuthIndex, time.Time{})
			if g.CPA != nil && name != "" && !cpaapi.LooksLikeOpaqueID(name) {
				_, _, _ = g.ensureAuthDisabled(ctx, name)
			}
			g.State.Log(state.ActionLog{
				Auth: ev.AuthIndex, Source: ev.Source, Signal: errKey,
				Action: "candidate", Reason: act.Reason,
			})
		}
		if act.Trash && g.Trash != nil && !act.Disable {
			_ = g.applyTrash(ctx, ev, res, acc)
		}
		_ = g.State.Save()
		return nil
	}
	// The policy streak belongs to the actual classified fingerprint, not merely
	// to HTTP status/signal. Changing error shape starts a new consecutive run.
	streakKey := errKey
	g.beginFingerprintRun(ev.AuthIndex, streakKey)
	streak := g.State.IncStreak(ev.AuthIndex, streakKey)
	// any_error: consecutive failures of any kind (cleared on success with streak mode)
	anyStreak := g.State.IncStreak(ev.AuthIndex, "any_error")
	var polPtr *state.ErrorPolicy
	if p, ok := g.State.GetErrorPolicy(errKey); ok {
		// unmatched card is UI dump only; unfamiliar 401/402/etc. remain observe-only.
		if errKey != "unmatched" {
			polPtr = &p
		}
	}
	act := policy.Decide(g.Cfg, policy.Input{
		Signal: res.Signal, ErrorKey: errKey, Streak: streak,
		Tier: tier.Tier(acc.Tier), Policy: polPtr,
	})
	// global any_error ladder: if stronger, upgrade act — never upgrades unmatched
	if errKey != "unmatched" {
		if ap, ok := g.State.GetErrorPolicy("any_error"); ok && ap.Enabled {
			anyAct := policy.Decide(g.Cfg, policy.Input{
				Signal: res.Signal, ErrorKey: "any_error", Streak: anyStreak,
				Tier: tier.Tier(acc.Tier), Policy: &ap,
			})
			if policyActionRank(anyAct) > policyActionRank(act) {
				act = anyAct
				if act.Reason != "" && !strings.Contains(act.Reason, "any_error") && !strings.Contains(act.Reason, "任意错误") {
					act.Reason = act.Reason + " · 任意错误连续≥" + itoaGuard(anyStreak)
				}
				// use any_error policy cooldown seconds when cool
				polPtr = &ap
			}
		}
	}

	// Permanent accounts no longer skip demote: patrol permanent range = scan only.
	// Alive → reopenAfterProbeAlive; errors → normal cool/候删/策略阶梯.

	// permanent disable is strongest; skip lighter cool if both somehow set
	if act.Disable {
		if err := g.applyPermanentDisable(ctx, ev, act.Reason); err != nil {
			return err
		}
	} else if act.Cooldown {
		if err := g.applyCooldown(ctx, ev, res, acc, polPtr, act.CooldownSec); err != nil {
			return err
		}
	}
	if act.Candidate && !act.Disable {
		// auth_401 + 8788-local: try password relogin first (config register_relogin_on_auth401)
		if res.Signal == match.SignalAuth401 && g.TryRelogin != nil {
			email := strings.TrimSpace(ev.Email)
			if email == "" && acc != nil {
				email = acc.Email
			}
			attempted, why := g.TryRelogin(ctx, email, ev.AuthIndex)
			if attempted {
				g.State.Log(state.ActionLog{
					Auth: ev.AuthIndex, Source: ev.Source, Signal: string(res.Signal),
					Action: "relogin", Reason: "【注册】auth_401 先重登 · " + why,
				})
				// short cool while relogin runs — do not enter candidate yet
				sec := g.Cfg.Auth401CooldownSec
				if sec <= 0 {
					sec = 1800
				}
				if sec > 1800 {
					sec = 1800
				}
				// light cool: keep file open? better disable briefly to avoid bad token traffic
				name := ev.FileName
				if name == "" || cpaapi.LooksLikeOpaqueID(name) {
					if acc != nil {
						name = g.resolveFileName(ctx, ev.AuthIndex, acc.FileName, acc.Email)
					} else {
						name = g.resolveFileName(ctx, ev.AuthIndex, ev.FileName, ev.Email)
					}
				}
				g.State.SetAccountState(ev.AuthIndex, state.CooldownPermission, "plugin_auto")
				g.State.SetRecoverAt(ev.AuthIndex, g.Now().Add(time.Duration(sec)*time.Second))
				g.State.SetLastSignal(ev.AuthIndex, string(res.Signal))
				if g.CPA != nil && name != "" && !cpaapi.LooksLikeOpaqueID(name) {
					_, _, _ = g.ensureAuthDisabled(ctx, name)
				}
				_ = g.State.Save()
				return nil
			}
			// not attempted → fall through to candidate with note
			if why != "" && why != "relogin_on_auth401_off" {
				act.Reason = act.Reason + " · 重登跳过:" + why
			}
		}
		// 候删 must also disable CPA file and stamp ownership (closed loop).
		// If applyCooldown already ran, this reinforces CandidateDead + plugin_auto.
		name := ev.FileName
		if name == "" || cpaapi.LooksLikeOpaqueID(name) {
			if acc != nil {
				name = g.resolveFileName(ctx, ev.AuthIndex, acc.FileName, acc.Email)
			} else {
				name = g.resolveFileName(ctx, ev.AuthIndex, ev.FileName, ev.Email)
			}
		}
		g.State.SetAccountState(ev.AuthIndex, state.CandidateDead, "plugin_auto")
		// recover_at only for real auth_401 (optional retry / relogin window).
		// Non-401 候删 (e.g. permission_403 ladder) must NOT auto reenable — that was
		// the 403→候删→30m recover→再403→候删 loop (UI also mislabeled it 401·候删).
		if res.Signal == match.SignalAuth401 {
			if cur := g.State.Get(ev.AuthIndex); cur != nil && cur.RecoverAt.IsZero() {
				sec := g.Cfg.Auth401CooldownSec
				if sec <= 0 {
					sec = 3600
				}
				g.State.SetRecoverAt(ev.AuthIndex, g.Now().Add(time.Duration(sec)*time.Second))
			}
		} else {
			// clear any cool window stamped by the paired Cooldown=true path
			g.State.SetRecoverAt(ev.AuthIndex, time.Time{})
		}
		if name != "" && !cpaapi.LooksLikeOpaqueID(name) {
			g.State.UpdateMeta(ev.AuthIndex, name, ev.Email, "")
			if g.CPA != nil {
				if err := g.CPA.SetDisabled(ctx, name, true); err != nil {
					g.State.Log(state.ActionLog{
						Auth: ev.AuthIndex, Source: ev.Source, Signal: string(res.Signal),
						Action: "candidate_disable_failed", Reason: err.Error(),
					})
				}
			}
		}
		g.State.Log(state.ActionLog{
			Auth: ev.AuthIndex, Source: ev.Source, Signal: string(res.Signal),
			Action: "candidate", Reason: act.Reason,
		})
	}
	if act.Trash && !act.Disable {
		return g.applyTrash(ctx, ev, res, acc)
	}
	_ = g.State.Save()
	return nil
}

func (g *Guard) applyCooldown(ctx context.Context, ev UsageEvent, res match.Result, acc *state.Account, pol *state.ErrorPolicy, cooldownSecOverride ...int) error {
	st := state.CooldownQuota
	switch res.Signal {
	case match.SignalSpendingLimit402:
		st = state.CooldownSpending
	case match.SignalPermission403:
		st = state.CooldownPermission
	case match.SignalAuth401:
		st = state.CandidateDead
	}
	// Idempotent: already plugin_auto cool with same family + recover not due →
	// do not re-log cool / do not stretch recover_at; only reassert file closed.
	if acc != nil && acc.DisableSource == "plugin_auto" {
		sameFamily := coolStatesMatch(acc.State, st) || coolSignalFamily(acc.LastSignal, string(res.Signal))
		recoverFuture := !acc.RecoverAt.IsZero() && acc.RecoverAt.After(g.Now())
		if sameFamily && recoverFuture {
			name := ev.FileName
			if name == "" || cpaapi.LooksLikeOpaqueID(name) {
				if acc.FileName != "" && !cpaapi.LooksLikeOpaqueID(acc.FileName) {
					name = acc.FileName
				}
			}
			if (name == "" || cpaapi.LooksLikeOpaqueID(name)) && g.Resolver != nil {
				_ = g.Resolver.Ensure(ctx)
				if id, ok := g.Resolver.Resolve(ev.AuthIndex, ev.FileName, ev.Email); ok && id.FileName != "" {
					name = id.FileName
				}
			}
			if g.CPA != nil && name != "" && !cpaapi.LooksLikeOpaqueID(name) {
				_, _, _ = g.ensureAuthDisabled(ctx, name)
			}
			// quiet stamp — no second 【冷却】 log
			g.State.StampLastAction(ev.AuthIndex, "cooldown")
			return nil
		}
	}
	recoverAt := res.RecoverAt
	cdOverride := 0
	if len(cooldownSecOverride) > 0 {
		cdOverride = cooldownSecOverride[0]
	}
	if recoverAt.IsZero() {
		if cdOverride > 0 {
			recoverAt = g.Now().Add(time.Duration(cdOverride) * time.Second)
		} else if pol != nil && pol.CooldownSec > 0 {
			recoverAt = g.Now().Add(time.Duration(pol.CooldownSec) * time.Second)
		} else {
			switch res.Signal {
			case match.SignalPermission403:
				recoverAt = g.Now().Add(time.Duration(g.Cfg.PermissionCooldownSec) * time.Second)
			case match.SignalAuth401:
				recoverAt = g.Now().Add(time.Duration(g.Cfg.Auth401CooldownSec) * time.Second)
			default:
				recoverAt = g.Now().Add(24 * time.Hour)
			}
		}
	}
	if g.Cfg.MaxResetSeconds > 0 {
		maxAt := g.Now().Add(time.Duration(g.Cfg.MaxResetSeconds) * time.Second)
		if recoverAt.After(maxAt) {
			recoverAt = maxAt
		}
	}
	// LastSignal is the classified error key (free_usage_429 / permission_403 / reason:fp_* / unmatched).
	// Never overwrite a real class with empty SignalNone — callers stamp errKey first.
	sig := ""
	if cur := g.State.Get(ev.AuthIndex); cur != nil {
		sig = cur.LastSignal
	}
	if sig == "" || sig == "any_error" {
		if s := string(res.Signal); s != "" && s != "any_error" {
			sig = s
		}
	}
	if sig != "" && sig != "any_error" {
		g.State.SetLastSignal(ev.AuthIndex, sig)
	}
	name := ev.FileName
	if name == "" || cpaapi.LooksLikeOpaqueID(name) {
		if acc != nil && acc.FileName != "" && !cpaapi.LooksLikeOpaqueID(acc.FileName) {
			name = acc.FileName
		}
	}
	if (name == "" || cpaapi.LooksLikeOpaqueID(name)) && g.Resolver != nil {
		_ = g.Resolver.Ensure(ctx)
		if id, ok := g.Resolver.Resolve(ev.AuthIndex, ev.FileName, ev.Email); ok && id.FileName != "" {
			name = id.FileName
			g.State.UpdateMeta(ev.AuthIndex, id.FileName, id.Email, "")
		}
	}
	// Always stamp ownership first so tick protect() cannot treat this as foreign disable
	// even if CPA SetDisabled is slow/racy with the next tick.
	g.State.SetAccountState(ev.AuthIndex, st, "plugin_auto")
	g.State.SetRecoverAt(ev.AuthIndex, recoverAt)
	// stamp last action early (before Log) so grace window covers SetDisabled latency
	g.State.Log(state.ActionLog{
		Auth: ev.AuthIndex, Source: ev.Source, Signal: sig,
		Action: "cooldown", Reason: res.Reason,
	})
	_ = g.State.Save() // persist ownership before file I/O so concurrent ticks see it
	if name != "" && !cpaapi.LooksLikeOpaqueID(name) {
		g.State.UpdateMeta(ev.AuthIndex, name, ev.Email, "")
	}
	if g.CPA != nil && name != "" && !cpaapi.LooksLikeOpaqueID(name) {
		closed, err, stillOpen := g.ensureAuthDisabled(ctx, name)
		if err != nil {
			g.State.Log(state.ActionLog{
				Auth: ev.AuthIndex, Source: ev.Source, Signal: string(res.Signal),
				Action: "cooldown_failed", Reason: err.Error(),
			})
			// keep plugin_auto cool-down ownership even if file disable failed
			return err
		}
		if !closed || stillOpen {
			// PATCH ok but list still shows open after retry — state stays cool; tick reassert will retry
			g.State.Log(state.ActionLog{
				Auth: ev.AuthIndex, Source: ev.Source, Signal: string(res.Signal),
				Action: "cooldown_file_still_open",
				Reason: "冷却已记录但文件校验仍开 · 将由冷却补关重试",
			})
		}
	}
	// cooldown already logged once before SetDisabled (avoid duplicate action log lines)
	return nil
}

func coolStatesMatch(cur state.AccountState, want state.AccountState) bool {
	if cur == want {
		return true
	}
	// treat all cool subtypes as same family for idempotent re-entry
	cool := func(s state.AccountState) bool {
		switch s {
		case state.CooldownQuota, state.CooldownSpending, state.CooldownPermission, state.CandidateDead:
			return true
		}
		return false
	}
	return cool(cur) && cool(want)
}

func coolSignalFamily(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	// free_usage and empty any_error cool both map to quota cool
	quota := map[string]bool{"free_usage_429": true, "spending_limit_402": true}
	if quota[a] && quota[b] {
		return true
	}
	return false
}

// shouldDedupeFailUsage: same auth recently cooled / failed within short window.
const usageFailDedupeWindow = 8 * time.Second

func (g *Guard) shouldDedupeFailUsage(ev UsageEvent) bool {
	if g.State == nil || ev.AuthIndex == "" {
		return false
	}
	// only HTTP cool-family statuses
	switch ev.StatusCode {
	case 401, 402, 403, 429:
	default:
		if ev.StatusCode < 400 {
			return false
		}
	}
	acc := g.State.Get(ev.AuthIndex)
	if acc == nil || acc.LastActionAt.IsZero() {
		return false
	}
	// Deduplicate only the same normalized response shape. A changed real error
	// must pass through immediately even if the previous action was seconds ago.
	_, _, incomingKey := errorsig.ShapeOf(ev.Body, ev.StatusCode)
	if incomingKey != "" && acc.LastSignal != "" && incomingKey != acc.LastSignal {
		return false
	}
	age := g.Now().Sub(acc.LastActionAt)
	if age < 0 || age > usageFailDedupeWindow {
		return false
	}
	switch acc.LastAction {
	case "cooldown", "cooldown_failed", "candidate", "manual_disable", "cooldown_file_still_open":
		return true
	}
	// already in cool with recover still future
	switch acc.State {
	case state.CooldownQuota, state.CooldownSpending, state.CooldownPermission, state.CandidateDead:
		if !acc.RecoverAt.IsZero() && acc.RecoverAt.After(g.Now()) {
			return true
		}
	}
	return false
}

func (g *Guard) applyTrash(ctx context.Context, ev UsageEvent, res match.Result, acc *state.Account) error {
	if g.Trash == nil {
		return nil
	}
	name := ev.FileName
	if name == "" || cpaapi.LooksLikeOpaqueID(name) {
		if acc != nil && acc.FileName != "" && !cpaapi.LooksLikeOpaqueID(acc.FileName) {
			name = acc.FileName
		}
	}
	if (name == "" || cpaapi.LooksLikeOpaqueID(name)) && g.Resolver != nil {
		_ = g.Resolver.Ensure(ctx)
		if id, ok := g.Resolver.Resolve(ev.AuthIndex, ev.FileName, ev.Email); ok && id.FileName != "" {
			name = id.FileName
			if ev.Email == "" {
				ev.Email = id.Email
			}
			g.State.UpdateMeta(ev.AuthIndex, id.FileName, id.Email, "")
		}
	}
	var raw []byte
	var err error
	if g.CPA != nil && name != "" && !cpaapi.LooksLikeOpaqueID(name) {
		raw, err = g.CPA.ReadAuthFileFromDir(name)
		if err != nil {
			g.State.Log(state.ActionLog{
				Auth: ev.AuthIndex, Source: ev.Source, Signal: string(res.Signal),
				Action: "trash_snapshot_failed", Reason: err.Error(),
			})
			return err
		}
	}
	now := g.Now()
	meta := state.TrashMeta{
		ID:        trash.NewID(ev.AuthIndex, now),
		AuthIndex: ev.AuthIndex,
		Email:     ev.Email,
		FileName:  name,
		Tier:      acc.Tier,
		Signal:    string(res.Signal),
		Source:    ev.Source,
		TrashedAt: now,
		ExpiresAt: now.Add(time.Duration(g.Cfg.TrashRetentionDays) * 24 * time.Hour),
	}
	return g.Trash.MoveToTrash(meta, raw, func() error {
		if g.CPA == nil || name == "" {
			return nil
		}
		return g.CPA.DeleteAuthFile(ctx, name)
	})
}

// Tick is the periodic maintenance path: recover due cool-downs, purge trash,
// and reassert owned disables. It does NOT scan unowned/foreign disables
// (no reopen_foreign / file_disabled_sync). Use TickManual for that.
func (g *Guard) Tick(ctx context.Context) error {
	return g.tick(ctx, false)
}

// TickManual is panel "立即维护": full foreign-disable scan once
// (open unowned if reopen_foreign_disabled, or mark CPA已禁用 if not).
func (g *Guard) TickManual(ctx context.Context) error {
	return g.tick(ctx, true)
}

func (g *Guard) tick(ctx context.Context, manual bool) error {
	if !g.Cfg.Enabled || !g.Cfg.SentryEnabled {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.Now()
	// recovered / reopened(foreign self-heal) / reasserted(cool re-close) / healed
	recovered, reopened, reasserted := 0, 0, 0
	// Best-effort identity refresh so panel can show email/file even for opaque auth_index.
	if g.Resolver != nil {
		_ = g.Resolver.Ensure(ctx)
		for _, acc := range g.State.AccountsSnapshot() {
			if id, ok := g.Resolver.Resolve(acc.AuthIndex, acc.FileName, acc.Email); ok {
				g.State.UpdateMeta(acc.AuthIndex, id.FileName, id.Email, "")
			}
		}
	}
	// IMPORTANT order: recover due cool-downs FIRST, then file sync/reassert.
	// Old order (sync then recover) caused same-tick fights:
	//   cooldown_reassert (close owned cool-down file) + reenable (open because recover_at due)
	// which showed as 「到期恢复」and「冷却补关」in the same second.
	//
	// Hold non-auth_401 候删: clear accidental recover_at so tick cannot re-open them.
	// (legacy rows entered candidate via 403 ladder with PermissionCooldownSec recover window)
	for _, acc := range g.State.AccountsSnapshot() {
		if acc.State != state.CandidateDead || acc.RecoverAt.IsZero() {
			continue
		}
		sig := acc.LastSignal
		if sig == "" || sig == string(match.SignalAuth401) || sig == "auth_401" {
			continue
		}
		g.State.SetRecoverAt(acc.AuthIndex, time.Time{})
		g.State.Log(state.ActionLog{
			Auth: acc.AuthIndex, Source: "tick", Signal: sig,
			Action: "candidate_hold", Reason: "非401候删取消自动恢复 · 避免候删循环",
		})
	}
	// P2: rate-limit recover opens per tick to avoid expiry tsunami hammering CPA API.
	const maxRecoverPerTick = 40
	for _, acc := range g.State.AccountsSnapshot() {
		if recovered >= maxRecoverPerTick {
			break
		}
		if acc.RecoverAt.IsZero() || acc.RecoverAt.After(now) {
			continue
		}
		// candidate_dead only auto-recovers for real auth_401 windows
		if acc.State == state.CandidateDead {
			sig := acc.LastSignal
			if sig != string(match.SignalAuth401) && sig != "auth_401" {
				g.State.SetRecoverAt(acc.AuthIndex, time.Time{})
				continue
			}
		}
		if !g.State.CanAutoReenable(acc.AuthIndex) {
			continue
		}
		name := acc.FileName
		if (name == "" || cpaapi.LooksLikeOpaqueID(name)) && g.Resolver != nil {
			if id, ok := g.Resolver.Resolve(acc.AuthIndex, acc.FileName, acc.Email); ok && id.FileName != "" {
				name = id.FileName
				g.State.UpdateMeta(acc.AuthIndex, id.FileName, id.Email, "")
			}
		}
		if g.CPA != nil && name != "" && !cpaapi.LooksLikeOpaqueID(name) {
			opened, err, stillClosed := g.ensureAuthEnabled(ctx, name)
			if err != nil {
				g.State.Log(state.ActionLog{
					Auth: acc.AuthIndex, Source: "tick", Action: "reenable_failed", Reason: err.Error(),
				})
				continue
			}
			if !opened || stillClosed {
				g.State.Log(state.ActionLog{
					Auth: acc.AuthIndex, Source: "tick", Action: "reenable_file_still_closed",
					Reason: "到期恢复后校验文件仍关 · 可能被外部重关或列表滞后",
				})
				// still advance state so recover_at does not loop forever; heal will retry open
			}
		}
		// closed-loop: cool-down due → Active (streaks retained for ladders)
		prevSig := acc.LastSignal
		g.State.ResetToActive(acc.AuthIndex)
		g.State.Log(state.ActionLog{
			Auth: acc.AuthIndex, Source: "tick", Signal: prevSig,
			Action: "reenable", Reason: "recover_at",
		})
		recovered++
	}
	// Owned cool-down reassert (always); unowned foreign open only on manual maintenance.
	// IMPORTANT: do not assign reassert counts into reopened — that inflated「自愈打开」.
	if fo, ra, err := g.syncDisabledFromCPA(ctx, now, manual); err != nil {
		g.State.Log(state.ActionLog{Source: "tick", Action: "sync_disabled_failed", Reason: err.Error()})
	} else {
		reopened = fo
		reasserted = ra
	}
	// Active + CPA file still disabled → force open (both periodic + manual).
	// This closes the residual "未归类" gap after reenable / file rewrite races.
	// Skips auths marked by CPAMP fail backfill this cycle (pendingBackfillAuths).
	healed := 0
	if n, err := g.healActiveFileDisabled(ctx, now); err != nil {
		g.State.Log(state.ActionLog{Source: "tick", Action: "heal_active_file_failed", Reason: err.Error()})
	} else {
		healed = n
	}
	// long-idle「恢复待观察」TTL (6h): drop pending so filter/KPI stop ballooning
	expiredPending := g.expireIdlePendingObserve(now)
	// Skip prune during patrol: patrol goroutines create accounts concurrently;
	// prune reads a snapshot then deletes "non-best" entries, which would delete
	// accounts that patrol just created but weren't in the snapshot.
	if g.PatrolRunning == nil || !g.PatrolRunning() {
		g.pruneDuplicateAccounts()
	}
	// closed-loop hygiene: Active must be clean (no residual signal/plugin_auto lock)
	g.scrubDirtyActiveAccounts()
	if g.Trash != nil && g.Cfg.TrashAutoPurge {
		if _, err := g.Trash.PurgeExpired(now); err != nil {
			return err
		}
	}
	// 立即维护：无论有无变更，都写一条汇总
	// 定时 tick：有变更时写汇总；分账号日志已写（强制打开/自愈/补关）
	if manual || healed > 0 || reopened > 0 || reasserted > 0 || expiredPending > 0 || recovered > 0 {
		src := "tick"
		if manual {
			src = "tick_manual"
		}
		parts := []string{}
		if recovered > 0 {
			parts = append(parts, "到期恢复"+itoaGuard(recovered))
		}
		if reopened > 0 {
			parts = append(parts, "自愈打开"+itoaGuard(reopened))
		}
		if healed > 0 {
			parts = append(parts, "强制打开"+itoaGuard(healed))
		}
		if expiredPending > 0 {
			parts = append(parts, "空闲待观察过期"+itoaGuard(expiredPending))
		}
		if reasserted > 0 {
			parts = append(parts, "冷却补关"+itoaGuard(reasserted))
		}
		if manual {
			reason := "立即维护完成 · 无到期冷却、无文件需打开"
			if len(parts) > 0 {
				reason = "立即维护完成 · " + strings.Join(parts, " · ")
			}
			g.State.Log(state.ActionLog{
				Source: src, Action: "maintenance",
				Reason: reason,
			})
		} else if len(parts) > 0 {
			g.State.Log(state.ActionLog{
				Source: src, Action: "heal_summary",
				Reason: strings.Join(parts, " · "),
			})
		}
	}
	return g.State.Save()
}

func itoaGuard(n int) string {
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

// waitFileVerify sleeps fileVerifyWait unless ctx cancelled (tests use real short sleep).
func waitFileVerify(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(fileVerifyWait):
		return nil
	}
}

// ensureAuthDisabled PATCHes disabled=true then re-lists. Returns (closed, patchErr, stillOpenAfterVerify).
// One retry on verify-still-open. Used by applyCooldown and cooldown_reassert.
func (g *Guard) ensureAuthDisabled(ctx context.Context, name string) (closed bool, patchErr error, verifyOpen bool) {
	if g.CPA == nil || name == "" || cpaapi.LooksLikeOpaqueID(name) {
		return true, nil, false
	}
	if err := g.CPA.SetDisabled(ctx, name, true); err != nil {
		return false, err, false
	}
	if err := waitFileVerify(ctx); err != nil {
		return false, err, false
	}
	still, err := g.CPA.IsAuthFileDisabled(ctx, name)
	if err != nil {
		g.noteListVerifyFail()
		// list failed: only trust PATCH if list is not in fuse; else report still-open
		if g.listVerifyBroken() {
			return false, nil, true
		}
		return true, nil, false
	}
	g.noteListVerifyOK()
	if still {
		return true, nil, false
	}
	// still open → retry once
	if err := g.CPA.SetDisabled(ctx, name, true); err != nil {
		return false, err, true
	}
	if err := waitFileVerify(ctx); err != nil {
		return false, err, true
	}
	still, err = g.CPA.IsAuthFileDisabled(ctx, name)
	if err != nil {
		g.noteListVerifyFail()
		if g.listVerifyBroken() {
			return false, nil, true
		}
		return true, nil, false
	}
	g.noteListVerifyOK()
	if still {
		return true, nil, false
	}
	return false, nil, true
}

// ensureAuthEnabled PATCHes disabled=false then re-lists. Returns (opened, patchErr, stillClosedAfterVerify).
// One retry on verify-still-closed. Used by reenable path.
func (g *Guard) ensureAuthEnabled(ctx context.Context, name string) (opened bool, patchErr error, stillClosed bool) {
	if g.CPA == nil || name == "" || cpaapi.LooksLikeOpaqueID(name) {
		return true, nil, false
	}
	if err := g.CPA.SetDisabled(ctx, name, false); err != nil {
		return false, err, false
	}
	if err := waitFileVerify(ctx); err != nil {
		return false, err, false
	}
	disabled, err := g.CPA.IsAuthFileDisabled(ctx, name)
	if err != nil {
		g.noteListVerifyFail()
		if g.listVerifyBroken() {
			return false, nil, true
		}
		return true, nil, false
	}
	g.noteListVerifyOK()
	if !disabled {
		return true, nil, false
	}
	// still closed → retry once
	if err := g.CPA.SetDisabled(ctx, name, false); err != nil {
		return false, err, true
	}
	if err := waitFileVerify(ctx); err != nil {
		return false, err, true
	}
	disabled, err = g.CPA.IsAuthFileDisabled(ctx, name)
	if err != nil {
		g.noteListVerifyFail()
		if g.listVerifyBroken() {
			return false, nil, true
		}
		return true, nil, false
	}
	g.noteListVerifyOK()
	if !disabled {
		return true, nil, false
	}
	return false, nil, true
}

const listVerifyFailFuse = 5 // consecutive list failures → stop blind trust-PATCH

func (g *Guard) noteListVerifyFail() {
	if g == nil {
		return
	}
	g.listVerifyFailStreak++
}

func (g *Guard) noteListVerifyOK() {
	if g == nil {
		return
	}
	g.listVerifyFailStreak = 0
	g.lastListOK = g.Now()
}

func (g *Guard) listVerifyBroken() bool {
	if g == nil {
		return false
	}
	return g.listVerifyFailStreak >= listVerifyFailFuse
}

// MarkPendingBackfillAuths records auths about to be cooled via CPAMP backfill
// so heal skips them this cycle (must hold Guard.mu or call from same tick chain).
func (g *Guard) MarkPendingBackfillAuths(auths []string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pendingBackfillAuths == nil {
		g.pendingBackfillAuths = map[string]struct{}{}
	}
	// reset each cycle then add
	g.pendingBackfillAuths = map[string]struct{}{}
	for _, a := range auths {
		a = strings.ToLower(strings.TrimSpace(a))
		if a != "" {
			g.pendingBackfillAuths[a] = struct{}{}
		}
	}
}

// ClearPendingBackfillAuths drops the backfill skip set after heal finishes.
func (g *Guard) ClearPendingBackfillAuths() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pendingBackfillAuths = map[string]struct{}{}
}

func (g *Guard) isPendingBackfillAuth(auth string) bool {
	if g == nil || g.pendingBackfillAuths == nil {
		return false
	}
	_, ok := g.pendingBackfillAuths[strings.ToLower(strings.TrimSpace(auth))]
	return ok
}

// inReassertSettleWindow: recent primary cool close — skip 冷却补关 while CPA list settles.
// Only primary actions (cooldown/candidate/manual_disable/failed close), not prior reassert.
func inReassertSettleWindow(acc *state.Account, now time.Time) bool {
	if acc == nil || acc.LastActionAt.IsZero() || reassertSettleAfterCool <= 0 {
		return false
	}
	age := now.Sub(acc.LastActionAt)
	if age < 0 || age >= reassertSettleAfterCool {
		return false
	}
	switch acc.LastAction {
	case "cooldown", "candidate", "manual_disable", "cooldown_failed", "candidate_disable_failed":
		return true
	default:
		return false
	}
}

// inHealSettleWindow: recent primary open intent — skip 强制打开 while CPA list settles.
// Do not include reenable/manual_enable_file_still_closed (open already failed).
func inHealSettleWindow(acc *state.Account, now time.Time) bool {
	if acc == nil || acc.LastActionAt.IsZero() || healSettleAfterOpen <= 0 {
		return false
	}
	age := now.Sub(acc.LastActionAt)
	if age < 0 || age >= healSettleAfterOpen {
		return false
	}
	switch acc.LastAction {
	case "manual_enable", "reenable", "patrol_alive_open", "patrol_alive",
		"reopen_foreign", "clear_cpa_disabled_tag":
		return true
	default:
		return false
	}
}

// shouldRateLimitActionLog: same account + same action family within window → skip narrative log.
func shouldRateLimitActionLog(acc *state.Account, now time.Time, window time.Duration, actions ...string) bool {
	if acc == nil || acc.LastActionAt.IsZero() || window <= 0 {
		return false
	}
	if now.Sub(acc.LastActionAt) >= window {
		return false
	}
	for _, a := range actions {
		if acc.LastAction == a {
			return true
		}
	}
	return false
}

// authFileBase normalizes CPA auth file names for matching (strip dirs, lower).
func authFileBase(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	// path-like from some list APIs
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(name))
}

// emailFromXAIFile extracts email from xai-<email>.json style names.
func emailFromXAIFile(name string) string {
	base := authFileBase(name)
	base = strings.TrimSuffix(base, ".json")
	if strings.HasPrefix(base, "xai-") {
		em := strings.TrimSpace(base[4:])
		if strings.Contains(em, "@") {
			return em
		}
	}
	return ""
}

// shouldProtectDisable reports whether this account's CPA disable must not be auto-opened.
// Includes cool-down grace: recent cooldown/candidate actions stay protected even if
// state was briefly wiped (race with concurrent tick).
func (g *Guard) shouldProtectDisable(acc *state.Account, now time.Time) bool {
	if acc == nil {
		return false
	}
	// primary protect rules (same as protect closure intent)
	switch acc.State {
	case state.Trashed, state.Purged:
		return true
	case state.CooldownQuota, state.CooldownSpending, state.CooldownPermission, state.CandidateDead:
		return true
	}
	if acc.PreDisabled {
		return true
	}
	if acc.DisableSource == "user_manual" {
		return true
	}
	if acc.State == state.UserManual && acc.DisableSource != "cpa_file_disabled" && acc.DisableSource != "cpa_disabled" {
		return true
	}
	if acc.DisableSource == "plugin_auto" {
		return true
	}
	if !acc.RecoverAt.IsZero() && acc.RecoverAt.After(now) {
		return true
	}
	// grace: just cooled/disabled — concurrent tick must not reopen
	if !acc.LastActionAt.IsZero() {
		age := now.Sub(acc.LastActionAt)
		if age >= 0 && age < 15*time.Minute {
			switch acc.LastAction {
			case "cooldown", "candidate", "manual_disable", "cooldown_failed", "candidate_disable_failed", "cooldown_reassert":
				return true
			}
		}
	}
	// any active cool-down signal with plugin ownership residue
	if acc.LastSignal == string(match.SignalFreeUsage429) || acc.LastSignal == string(match.SignalSpendingLimit402) ||
		acc.LastSignal == string(match.SignalPermission403) || acc.LastSignal == string(match.SignalAuth401) {
		if acc.DisableSource == "plugin_auto" || (!acc.RecoverAt.IsZero() && acc.RecoverAt.After(now)) {
			return true
		}
	}
	return false
}

// healActiveFileDisabled opens CPA files that are disabled while sentry still says Active.
// Root cause of residual KPI「未归类」: reenable/ResetToActive succeeded but file stayed
// (or was rewritten) disabled — no cool/候删/永禁 ownership, so foreign scan may miss
// identity or only runs on manual. This path runs every tick (cheap residual fix).
// Never opens protected cool-downs / permanent disables.
//
// v1.1.33: same account at most once per healActiveFileCooldown (15m).
// v1.1.34: verify reopen after short wait; escalate sticky closes to CPA已禁用 after
// healStuckAfter fails; success heal no per-account spam log.
// v1.1.35: clean Active heal does NOT set pending_observe (stop KPI balloon).
// v1.1.45: rate-limit on LastHealAt (not LastAction — cooldown/patrol overwrote it
// and re-fired force-open every tick). ManualEnable also verifies open.
// v1.1.47: settle 2m after manual_enable/reenable/patrol_alive/reopen_foreign —
// skip heal while list lags (bulk enable wave).
const healActiveFileCooldown = 15 * time.Minute
const fileVerifyWait = 1200 * time.Millisecond // shared by heal open / cool close verify
const healVerifyWait = fileVerifyWait          // alias
const healStuckAfter = 3                       // consecutive failed verifies → sticky CPA已禁用
const reassertLogCooldown = 15 * time.Minute   // cooldown_reassert log rate limit per account
// reassertSettleAfterCool: after primary cool/candidate/manual_disable, skip 冷却补关.
// Live 2k logs (v1.1.45): 89% cool→reassert pairs are 30–60s (one tick), p90≈61s,
// and cooldown_file_still_open=0 — almost all early reasserts are CPA list/hotload lag
// after ensureAuthDisabled already ran, not real external reopen. 2m covers p90 + margin.
const reassertSettleAfterCool = 2 * time.Minute

// healSettleAfterOpen: after primary open intent, skip 强制打开 while CPA list settles.
// Live 2k logs: 88% heal pairs follow manual_enable in 60–120s (p50≈80s); bulk 21:00
// wave was list lag not sticky closed. 2m mirrors cool-side settle. Exclude
// *_file_still_closed (open already failed — heal should retry soon).
const healSettleAfterOpen = 2 * time.Minute

func (g *Guard) healActiveFileDisabled(ctx context.Context, now time.Time) (int, error) {
	if g.CPA == nil || g.State == nil {
		return 0, nil
	}
	files, err := g.CPA.ListAuthFiles(ctx)
	if err != nil {
		return 0, err
	}
	// index disabled xAI files by basename / email / auth_index
	type fileRef struct {
		Name string
	}
	byKey := map[string]fileRef{}
	for _, f := range files {
		if !f.Disabled {
			continue
		}
		name := strings.TrimSpace(f.Name)
		prov := f.Provider
		if prov == "" {
			prov = f.Type
		}
		if name == "" {
			name = strings.TrimSpace(f.ID)
		}
		if name == "" {
			name = strings.TrimSpace(f.Path)
		}
		if name == "" || !cpaapi.IsXAIName(name, prov) {
			continue
		}
		base := authFileBase(name)
		ref := fileRef{Name: name}
		if base != "" {
			byKey[base] = ref
		}
		if id := authFileBase(f.ID); id != "" {
			byKey[id] = ref
		}
		if p := authFileBase(f.Path); p != "" {
			byKey[p] = ref
		}
		if ai := strings.ToLower(strings.TrimSpace(f.AuthIndex)); ai != "" {
			byKey["auth:"+ai] = ref
		}
		em := strings.ToLower(strings.TrimSpace(f.Email))
		if em == "" {
			em = strings.ToLower(strings.TrimSpace(f.Account))
		}
		if em == "" {
			em = emailFromXAIFile(name)
		}
		if em != "" {
			byKey["email:"+em] = ref
			byKey["xai-"+em+".json"] = ref
		}
	}
	if len(byKey) == 0 {
		return 0, nil
	}
	n := 0
	for _, acc := range g.State.AccountsSnapshot() {
		if acc == nil {
			continue
		}
		// only Active (接流态) with no cool ownership
		if acc.State != state.Active && acc.State != "" {
			continue
		}
		if g.shouldProtectDisable(acc, now) {
			continue
		}
		// hard rate limit on LastHealAt — survives LastAction overwrite by cooldown/patrol
		if !acc.LastHealAt.IsZero() && now.Sub(acc.LastHealAt) < healActiveFileCooldown {
			continue
		}
		// also respect legacy LastAction heal window
		if !acc.LastActionAt.IsZero() && now.Sub(acc.LastActionAt) < healActiveFileCooldown {
			switch acc.LastAction {
			case "heal_active_file", "heal_active_file_failed", "heal_active_file_stuck":
				continue
			}
		}
		// Settle after primary open: list lag looks closed for ~1–2 ticks after enable.
		// Skip heal PATCH/log; do NOT TouchLastHealAt so real sticky close after settle still heals.
		if inHealSettleWindow(acc, now) {
			continue
		}
		// resolve disabled file for this account
		var ref fileRef
		var ok bool
		if af := authFileBase(acc.FileName); af != "" {
			ref, ok = byKey[af]
		}
		if !ok {
			if em := strings.ToLower(strings.TrimSpace(acc.Email)); em != "" {
				ref, ok = byKey["email:"+em]
				if !ok {
					ref, ok = byKey["xai-"+em+".json"]
				}
			}
		}
		if !ok {
			if em := emailFromXAIFile(acc.FileName); em != "" {
				ref, ok = byKey["email:"+em]
				if !ok {
					ref, ok = byKey["xai-"+em+".json"]
				}
			}
		}
		if !ok {
			if ai := strings.ToLower(strings.TrimSpace(acc.AuthIndex)); ai != "" {
				ref, ok = byKey["auth:"+ai]
			}
		}
		if !ok || ref.Name == "" || cpaapi.LooksLikeOpaqueID(ref.Name) {
			continue
		}
		// Skip auths that CPAMP fail backfill will cool this cycle (P0: no force-open then cool).
		if g.isPendingBackfillAuth(acc.AuthIndex) {
			continue
		}
		// If ANY identity-matched account is owned cool/permanent, do not open the file
		// (Active duplicate shell must not defeat cool-down protect).
		baseName := authFileBase(ref.Name)
		emKey := strings.ToLower(strings.TrimSpace(acc.Email))
		if emKey == "" {
			emKey = emailFromXAIFile(acc.FileName)
		}
		if emKey == "" {
			emKey = emailFromXAIFile(ref.Name)
		}
		aiKey := strings.ToLower(strings.TrimSpace(acc.AuthIndex))
		siblingProtected := false
		for _, other := range g.State.AccountsSnapshot() {
			if other == nil || other.AuthIndex == acc.AuthIndex {
				continue
			}
			if !g.shouldProtectDisable(other, now) {
				continue
			}
			oaf := authFileBase(other.FileName)
			oem := strings.ToLower(strings.TrimSpace(other.Email))
			if oem == "" {
				oem = emailFromXAIFile(other.FileName)
			}
			oai := strings.ToLower(strings.TrimSpace(other.AuthIndex))
			if (baseName != "" && oaf == baseName) ||
				(emKey != "" && (oem == emKey || emailFromXAIFile(other.FileName) == emKey)) ||
				(aiKey != "" && oai == aiKey) {
				siblingProtected = true
				break
			}
		}
		if siblingProtected {
			continue
		}
		// stamp heal attempt *before* PATCH so rate limit holds even if later logs overwrite LastAction
		g.State.TouchLastHealAt(acc.AuthIndex, now)

		opened, err, stillClosed := g.ensureAuthEnabled(ctx, ref.Name)
		if err != nil {
			g.State.Log(state.ActionLog{
				Auth: acc.AuthIndex, Source: "tick", Action: "heal_active_file_failed",
				Reason: err.Error(),
			})
			continue
		}
		if !opened || stillClosed {
			failN := g.State.IncHealFailStreak(acc.AuthIndex)
			if failN >= healStuckAfter {
				g.State.MarkCPAFileDisabled(acc.AuthIndex)
				g.State.Log(state.ActionLog{
					Auth: acc.AuthIndex, Source: "tick", Action: "heal_active_file_stuck",
					Reason: "强制打开后文件仍关 · 连续≥" + itoaGuard(failN) + " → 标为CPA已禁用",
				})
			} else {
				g.State.Log(state.ActionLog{
					Auth: acc.AuthIndex, Source: "tick", Action: "heal_active_file_failed",
					Reason: "强制打开后校验仍关 · 连续" + itoaGuard(failN),
				})
			}
			continue
		}
		g.State.ClearHealFailStreak(acc.AuthIndex)
		// v1.1.35: clean Active heal must NOT ResetToActive (that forced pending_observe
		// on hundreds of never-cooled accounts). Only non-Active residue uses ResetToActive.
		if acc.State != state.Active && acc.State != "" {
			g.State.ResetToActive(acc.AuthIndex)
		}
		if ref.Name != "" {
			em := strings.ToLower(strings.TrimSpace(acc.Email))
			if em == "" {
				em = emailFromXAIFile(ref.Name)
			}
			g.State.UpdateMeta(acc.AuthIndex, authFileBase(ref.Name), em, "")
		}
		// success: log once per rate window (this call already rate-limited)
		g.State.Log(state.ActionLog{
			Auth: acc.AuthIndex, Source: "tick", Action: "heal_active_file",
			Reason: "active_file_was_disabled",
		})
		n++
	}
	return n, nil
}

// expireIdlePendingObserve clears long-idle「恢复待观察」so KPI/filter stop ballooning
// when accounts never get a success request (large cold pool).
// Keeps 403/401 ladder streaks; drops free_usage residual signal.
//
// v1.1.35 also: heal-inflated pending (last_action=heal_active_file, no ladder signal)
// is cleared immediately on tick — those were never cool recoveries.
const pendingObserveIdleTTL = 6 * time.Hour

func (g *Guard) expireIdlePendingObserve(now time.Time) int {
	if g.State == nil {
		return 0
	}
	n := 0
	for _, acc := range g.State.AccountsSnapshot() {
		if acc == nil {
			continue
		}
		if acc.State != state.Active && acc.State != "" {
			continue
		}
		if !acc.PendingObserve {
			continue
		}
		// v1.1.35: drop heal-only pending balloon (no cool residual ladder)
		if acc.LastAction == "heal_active_file" && !hasActiveLadderSignal(acc) {
			g.State.ExpireIdlePending(acc.AuthIndex)
			n++
			continue
		}
		since := acc.PendingSince
		if since.IsZero() {
			// legacy rows: fall back to last_action_at / updated_at
			since = acc.LastActionAt
			if since.IsZero() {
				since = acc.UpdatedAt
			}
		}
		if since.IsZero() || now.Sub(since) < pendingObserveIdleTTL {
			continue
		}
		g.State.ExpireIdlePending(acc.AuthIndex)
		n++
	}
	return n
}

func hasActiveLadderSignal(acc *state.Account) bool {
	if acc == nil {
		return false
	}
	if acc.Streaks != nil {
		for _, v := range acc.Streaks {
			if v > 0 {
				return true
			}
		}
	}
	switch acc.LastSignal {
	case "permission_403", "auth_401", "code:invalid-argument", "free_usage_429", "spending_limit_402":
		return true
	}
	return false
}

// syncDisabledFromCPA inspects CPA auth files that are currently disabled.
//
// Returns (foreignOpened, reasserted, err):
//   - foreignOpened: unowned disables reopened (「自愈打开」) — only when scanForeign
//   - reasserted: owned cool-down files that were open and re-closed (「冷却补关」)
//
// Default (reopen_foreign_disabled=true) — ops self-heal model:
//   - If sentry OWNS the disable (plugin_auto cool-down/候删, panel user_manual), NEVER open;
//     if file was wrongly enabled, re-disable (cooldown_reassert).
//   - If unowned: enable CPA file only; do NOT ResetToActive cool-downs.
//     Next real usage/patrol error re-stamps ownership.
//
// Optional (reopen_foreign_disabled=false) — keep unowned closed + mark CPA已禁用.
func (g *Guard) syncDisabledFromCPA(ctx context.Context, now time.Time, scanForeign bool) (foreignOpened int, reasserted int, err error) {
	if g.CPA == nil {
		return 0, 0, nil
	}
	files, err := g.CPA.ListAuthFiles(ctx)
	if err != nil {
		return 0, 0, err
	}
	nForeign, nReassert := 0, 0
	// index sentry accounts by auth_index / file basename / email
	byAuth := map[string]*state.Account{}
	byFile := map[string]*state.Account{}
	byEmail := map[string]*state.Account{}
	for _, acc := range g.State.AccountsSnapshot() {
		if acc.AuthIndex != "" {
			byAuth[strings.ToLower(strings.TrimSpace(acc.AuthIndex))] = acc
		}
		if acc.FileName != "" {
			byFile[authFileBase(acc.FileName)] = acc
			byFile[strings.ToLower(strings.TrimSpace(acc.FileName))] = acc
		}
		if acc.Email != "" {
			byEmail[strings.ToLower(strings.TrimSpace(acc.Email))] = acc
		}
		if em := emailFromXAIFile(acc.FileName); em != "" {
			if _, ok := byEmail[em]; !ok {
				byEmail[em] = acc
			}
		}
	}
	// protect=true: sentry intentionally owns this disable — do not reopen.
	// cpa_file_disabled is NOT protect: it was a "unknown disable" tag and should
	// self-heal (reopen) so next real error can re-stamp ownership.
	protect := func(acc *state.Account) bool {
		if acc == nil {
			return false
		}
		switch acc.State {
		case state.Trashed, state.Purged:
			return true
		case state.CooldownQuota, state.CooldownSpending, state.CooldownPermission, state.CandidateDead:
			// cool-down / 候删 regardless of disable_source
			return true
		}
		if acc.PreDisabled {
			return true
		}
		// panel permanent disable only (not cpa_file_disabled)
		if acc.DisableSource == "user_manual" {
			return true
		}
		if acc.State == state.UserManual && acc.DisableSource != "cpa_file_disabled" && acc.DisableSource != "cpa_disabled" {
			return true
		}
		// plugin_auto ownership (including half-dirty Active+plugin_auto)
		if acc.DisableSource == "plugin_auto" {
			return true
		}
		// future recover_at means cool-down window still intended
		if !acc.RecoverAt.IsZero() && acc.RecoverAt.After(now) {
			return true
		}
		return false
	}
	// When multiple state rows map to one auth file, prefer the most "owned" one.
	pickAcc := func(cands ...*state.Account) *state.Account {
		var best *state.Account
		bestScore := -1
		for _, a := range cands {
			if a == nil {
				continue
			}
			score := 0
			if protect(a) {
				score += 1000
			}
			switch a.State {
			case state.CooldownQuota, state.CooldownSpending, state.CooldownPermission:
				score += 50
			case state.CandidateDead, state.UserManual:
				score += 40
			}
			if a.DisableSource == "plugin_auto" {
				score += 30
			}
			if a.LastSignal != "" {
				score += 5
			}
			if score > bestScore {
				bestScore, best = score, a
			}
		}
		return best
	}

	// Precompute identities that currently own a CPA disable (must never self-heal open).
	ownedFile := map[string]*state.Account{}
	ownedEmail := map[string]*state.Account{}
	ownedAuth := map[string]*state.Account{}
	for _, a := range g.State.AccountsSnapshot() {
		if !g.shouldProtectDisable(a, now) {
			continue
		}
		if a.AuthIndex != "" {
			ownedAuth[strings.ToLower(strings.TrimSpace(a.AuthIndex))] = a
		}
		if af := authFileBase(a.FileName); af != "" {
			ownedFile[af] = a
		}
		if em := strings.ToLower(strings.TrimSpace(a.Email)); em != "" {
			ownedEmail[em] = a
		}
		if em := emailFromXAIFile(a.FileName); em != "" {
			ownedEmail[em] = a
		}
	}

	for _, f := range files {
		name := strings.TrimSpace(f.Name)
		prov := f.Provider
		if prov == "" {
			prov = f.Type
		}
		if name == "" || !cpaapi.IsXAIName(name, prov) {
			continue
		}
		// CPA list fields (name/id/path/auth_index/email/account)
		base := authFileBase(name)
		if base == "" {
			base = authFileBase(f.ID)
		}
		if base == "" {
			base = authFileBase(f.Path)
		}
		em := strings.ToLower(strings.TrimSpace(f.Email))
		if em == "" {
			em = strings.ToLower(strings.TrimSpace(f.Account))
		}
		if em == "" {
			em = emailFromXAIFile(name)
		}
		if em == "" {
			em = emailFromXAIFile(f.ID)
		}
		if em == "" {
			em = emailFromXAIFile(f.Path)
		}
		ai := strings.ToLower(strings.TrimSpace(f.AuthIndex))
		// Absolute ownership map (immune to pickAcc / empty cand races)
		var forced *state.Account
		if ai != "" {
			forced = ownedAuth[ai]
		}
		if forced == nil && base != "" {
			forced = ownedFile[base]
		}
		if forced == nil && em != "" {
			forced = ownedEmail[em]
		}
		if forced == nil && authFileBase(f.ID) != "" {
			forced = ownedFile[authFileBase(f.ID)]
		}
		if forced != nil {
			// Owned cool-down/manual: never open. If CPA file is enabled, re-disable —
			// BUT not when recover_at is already due (that account should reenable, not re-close).
			if !f.Disabled {
				due := !forced.RecoverAt.IsZero() && !forced.RecoverAt.After(now)
				if due && g.State.CanAutoReenable(forced.AuthIndex) {
					// leave enabled; reenable path owns this account
					if name != "" {
						g.State.UpdateMeta(forced.AuthIndex, authFileBase(name), em, "")
					}
					continue
				}
				// Settle after primary cool close: list lag looks like "still open" for ~1 tick.
				// Skip reassert PATCH/log until window ends; real reopen after settle still补关.
				if inReassertSettleWindow(forced, now) {
					if name != "" {
						g.State.UpdateMeta(forced.AuthIndex, authFileBase(name), em, "")
					}
					continue
				}
				// rate-limit narrative log: same account reassert within 15m only stamps
				rateLimited := shouldRateLimitActionLog(forced, now, reassertLogCooldown,
					"cooldown_reassert", "cooldown_reassert_failed", "cooldown_file_still_open")
				closed, err, stillOpen := g.ensureAuthDisabled(ctx, name)
				if err != nil {
					if !rateLimited {
						g.State.Log(state.ActionLog{Auth: forced.AuthIndex, Source: "tick", Action: "cooldown_reassert_failed", Reason: err.Error()})
					} else {
						g.State.StampLastAction(forced.AuthIndex, "cooldown_reassert_failed")
					}
				} else if !closed || stillOpen {
					if !rateLimited {
						g.State.Log(state.ActionLog{
							Auth: forced.AuthIndex, Source: "tick", Action: "cooldown_file_still_open",
							Reason: "冷却补关后校验文件仍开 · 可能被外部重开或列表滞后",
						})
					} else {
						g.State.StampLastAction(forced.AuthIndex, "cooldown_file_still_open")
					}
					nReassert++
				} else {
					if !rateLimited {
						g.State.Log(state.ActionLog{Auth: forced.AuthIndex, Source: "tick", Action: "cooldown_reassert", Reason: "owned_disable_was_enabled"})
					} else {
						g.State.StampLastAction(forced.AuthIndex, "cooldown_reassert")
					}
					nReassert++
				}
			}
			if name != "" {
				g.State.UpdateMeta(forced.AuthIndex, authFileBase(name), em, "")
			}
			continue
		}
		if !f.Disabled {
			// not owned and already enabled — nothing to self-heal
			continue
		}
		// Periodic tick: only owned reassert above. Foreign open/mark is manual-only.
		if !scanForeign {
			continue
		}
		var cands []*state.Account
		// 1) strongest: auth_index from CPA runtime
		if ai != "" {
			if a := byAuth[ai]; a != nil {
				cands = append(cands, a)
			}
		}
		// 2) file basename / id / path
		for _, key := range []string{base, authFileBase(f.ID), authFileBase(f.Path), strings.ToLower(strings.TrimSpace(name))} {
			if key == "" {
				continue
			}
			if a := byFile[key]; a != nil {
				cands = append(cands, a)
			}
		}
		// 3) email
		if em != "" {
			if a := byEmail[em]; a != nil {
				cands = append(cands, a)
			}
			if a := byFile["xai-"+em+".json"]; a != nil {
				cands = append(cands, a)
			}
		}
		// 4) full scan fallback
		for _, acc := range g.State.AccountsSnapshot() {
			if ai != "" && strings.EqualFold(strings.TrimSpace(acc.AuthIndex), ai) {
				cands = append(cands, acc)
				continue
			}
			af := authFileBase(acc.FileName)
			if af != "" && (af == base || af == authFileBase(f.ID) || af == authFileBase(f.Path)) {
				cands = append(cands, acc)
				continue
			}
			if em != "" && (strings.EqualFold(strings.TrimSpace(acc.Email), em) || emailFromXAIFile(acc.FileName) == em) {
				cands = append(cands, acc)
			}
		}
		acc := pickAcc(cands...)
		// CRITICAL: if ANY identity-matched row is owned (cool-down/plugin_auto/manual),
		// never reopen — pickAcc might prefer a weak Active shell over the cool-down row.
		owned := acc
		if !protect(owned) {
			for _, c := range cands {
				if protect(c) {
					owned = c
					break
				}
			}
		}
		if !protect(owned) {
			// identity scan including recent cool-down grace
			for _, a := range g.State.AccountsSnapshot() {
				if !g.shouldProtectDisable(a, now) {
					continue
				}
				af := authFileBase(a.FileName)
				if (ai != "" && strings.EqualFold(strings.TrimSpace(a.AuthIndex), ai)) ||
					(base != "" && af == base) ||
					(em != "" && (strings.EqualFold(strings.TrimSpace(a.Email), em) || emailFromXAIFile(a.FileName) == em)) {
					owned = a
					break
				}
			}
		}
		if g.shouldProtectDisable(owned, now) {
			// keep disabled; refresh meta onto the owned row
			if owned != nil && name != "" {
				g.State.UpdateMeta(owned.AuthIndex, authFileBase(name), em, "")
			}
			continue
		}

		auth := name
		if owned != nil {
			auth = owned.AuthIndex
		} else if acc != nil {
			auth = acc.AuthIndex
		}

		// Ops self-heal (ON by default): open UNOWNED disables only.
		//  - NEVER open if shouldProtectDisable (owned cool-down/manual)
		//  - NEVER ResetToActive on protectable accounts
		//  - only enable the CPA file; next real error re-stamps cool-down
		//  - NEVER open files that sentry has never seen (no identity match at all).
		//    These are untracked registration-machine files; opening them floods CPA
		//    with enabled files that have no sentry state. Patrol HandleUsage will
		//    bring them into sentry on next probe.
		if owned == nil && acc == nil && len(cands) == 0 {
			continue
		}
		if g.Cfg.ReopenForeignDisabled {
			// final hard gate
			if g.shouldProtectDisable(owned, now) || g.shouldProtectDisable(acc, now) {
				continue
			}
			// refuse to open if any protected account shares identity (belt)
			skip := false
			for _, a := range g.State.AccountsSnapshot() {
				if !g.shouldProtectDisable(a, now) {
					continue
				}
				af := authFileBase(a.FileName)
				if (ai != "" && strings.EqualFold(strings.TrimSpace(a.AuthIndex), ai)) ||
					(base != "" && af == base) ||
					(em != "" && (strings.EqualFold(strings.TrimSpace(a.Email), em) || emailFromXAIFile(a.FileName) == em)) {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			if err := g.CPA.SetDisabled(ctx, name, false); err != nil {
				g.State.Log(state.ActionLog{Auth: auth, Source: "tick", Action: "reopen_foreign_failed", Reason: err.Error()})
				continue
			}
			// clear only sticky cpa_file_disabled tag — do NOT ResetToActive
			target := owned
			if target == nil {
				target = acc
			}
			if target != nil {
				if target.DisableSource == "cpa_file_disabled" || target.DisableSource == "cpa_disabled" {
					// drop sticky tag only if not a real cool-down
					if target.State == state.UserManual || target.State == state.Active || target.State == "" {
						// full clean active without inventing cool-down wipe of recover_at ownership
						g.State.ResetToActive(target.AuthIndex)
					}
				}
				if name != "" {
					g.State.UpdateMeta(target.AuthIndex, authFileBase(name), em, "")
				}
				g.State.Log(state.ActionLog{
					Auth: target.AuthIndex, Source: "tick", Action: "reopen_foreign",
					Reason: "unowned_disabled_self_heal_file_only",
				})
			} else {
				g.State.Log(state.ActionLog{
					Auth: name, Source: "tick", Action: "reopen_foreign",
					Reason: "unowned_disabled_untracked_file_only",
				})
			}
			nForeign++
			continue
		}

		// Conservative mode: keep file disabled. Only tag as CPA已禁用 when we are
		// sure this is NOT an owned cool-down (double-check again).
		target := owned
		if target == nil {
			target = acc
		}
		if g.shouldProtectDisable(target, now) {
			continue
		}
		// refuse if any protected identity exists
		blocked := false
		for _, a := range g.State.AccountsSnapshot() {
			if !g.shouldProtectDisable(a, now) {
				continue
			}
			af := authFileBase(a.FileName)
			if (ai != "" && strings.EqualFold(strings.TrimSpace(a.AuthIndex), ai)) ||
				(base != "" && af == base) ||
				(em != "" && (strings.EqualFold(strings.TrimSpace(a.Email), em) || emailFromXAIFile(a.FileName) == em)) {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		authIndex := auth
		if target != nil {
			authIndex = target.AuthIndex
		} else {
			authIndex = name
			g.State.Touch(authIndex)
			g.State.UpdateMeta(authIndex, authFileBase(name), em, "")
		}
		// do not clear recover_at / wipe cool-down fields if present
		cur := g.State.Get(authIndex)
		if cur != nil && g.shouldProtectDisable(cur, now) {
			continue
		}
		g.State.SetAccountState(authIndex, state.UserManual, "cpa_file_disabled")
		// only zero recover_at when not protecting
		g.State.SetRecoverAt(authIndex, time.Time{})
		g.State.Log(state.ActionLog{
			Auth: authIndex, Source: "tick", Action: "file_disabled_sync",
			Reason: "cpa_disabled_sync",
		})
	}

	// Clear sticky cpa_file_disabled when the CPA file is already enabled
	// (self-heal already opened it, or operator enabled outside).
	disabledSet := map[string]bool{}
	for _, f := range files {
		if !f.Disabled {
			continue
		}
		nm := authFileBase(f.Name)
		if nm != "" {
			disabledSet[nm] = true
		}
		em := strings.ToLower(strings.TrimSpace(f.Email))
		if em != "" {
			disabledSet["email:"+em] = true
		}
	}
	for _, acc := range g.State.AccountsSnapshot() {
		if acc.DisableSource != "cpa_file_disabled" && acc.DisableSource != "cpa_disabled" {
			continue
		}
		fn := authFileBase(acc.FileName)
		em := strings.ToLower(strings.TrimSpace(acc.Email))
		if em == "" {
			em = emailFromXAIFile(acc.FileName)
		}
		still := (fn != "" && disabledSet[fn]) || (em != "" && disabledSet["email:"+em])
		if still {
			// file still disabled: if self-heal on, reopen path above should have handled;
			// if conservative, keep tag.
			continue
		}
		// file not disabled anymore → drop sticky tag
		g.State.ResetToActive(acc.AuthIndex)
		g.State.Log(state.ActionLog{
			Auth: acc.AuthIndex, Source: "tick", Action: "clear_cpa_disabled_tag",
			Reason: "file_already_enabled",
		})
	}
	return nForeign, nReassert, nil
}

// scrubDirtyActiveAccounts fixes half-recovered cool-downs only:
// state=active but still carrying plugin_auto lock / recover_at / stale last_signal
// from a previous cool-down recovery path.
//
// Do NOT treat non-empty Streaks as dirty — active accounts normally accumulate
// consecutive-error counters toward policy thresholds; clearing them every tick
// would spam "清理脏正常态" forever (e.g. failing requests that only observe).
func (g *Guard) scrubDirtyActiveAccounts() {
	if g.State == nil {
		return
	}
	for _, acc := range g.State.AccountsSnapshot() {
		if acc.State != state.Active && acc.State != "" {
			continue
		}
		// Active + future recover_at + plugin_auto = cool-down still in force but state flipped wrong.
		// Restore cool-down state instead of scrubbing ownership away (which caused false "foreign reopen").
		if acc.DisableSource == "plugin_auto" && !acc.RecoverAt.IsZero() && acc.RecoverAt.After(g.Now()) {
			st := state.CooldownQuota
			switch acc.LastSignal {
			case string(match.SignalSpendingLimit402):
				st = state.CooldownSpending
			case string(match.SignalPermission403):
				st = state.CooldownPermission
			case string(match.SignalAuth401):
				st = state.CandidateDead
			}
			g.State.SetAccountState(acc.AuthIndex, st, "plugin_auto")
			g.State.Log(state.ActionLog{
				Auth: acc.AuthIndex, Source: "tick", Signal: acc.LastSignal,
				Action: "repair_cooldown_state", Reason: "active_with_future_recover_at",
			})
			continue
		}
		// Active + plugin_auto without future recover: keep plugin_auto marker (ownership),
		// only clear expired recover_at. Do NOT wipe disable_source — ownership must stick
		// until explicit reenable/ResetToActive.
		if acc.DisableSource == "plugin_auto" {
			if !acc.RecoverAt.IsZero() && !acc.RecoverAt.After(g.Now()) {
				// expired: normal recover path in Tick loop will handle; don't scrub ownership here
			}
			continue
		}
		// only scrub pure recover_at residue without ownership lock (should be rare)
		if acc.RecoverAt.IsZero() {
			continue
		}
		prevSig := acc.LastSignal
		g.State.ClearCoolDownResidue(acc.AuthIndex)
		g.State.Log(state.ActionLog{
			Auth: acc.AuthIndex, Source: "tick", Signal: prevSig,
			Action: "scrub_active", Reason: "stale_recover_at_only",
		})
	}
}

// pruneDuplicateAccounts keeps one state entry per email/file (prefer hash auth_index).
func (g *Guard) pruneDuplicateAccounts() {
	if g.State == nil {
		return
	}
	type wrap struct {
		a *state.Account
	}
	// work on snapshot then delete losers
	accs := g.State.AccountsSnapshot()
	best := map[string]string{} // key -> authIndex to keep
	score := func(a *state.Account) int {
		if a == nil {
			return -1
		}
		s := 0
		// ownership / cool-down ALWAYS beats empty Active shells (closed-loop safety)
		switch a.State {
		case state.CooldownQuota, state.CooldownSpending, state.CooldownPermission:
			s += 1000
		case state.CandidateDead:
			s += 900
		case state.UserManual:
			s += 950
		case state.Trashed, state.Purged:
			s += 800
		}
		switch a.DisableSource {
		case "plugin_auto":
			s += 500
		case "user_manual":
			s += 480
		case "cpa_file_disabled", "cpa_disabled":
			s += 450
		}
		if !a.RecoverAt.IsZero() {
			s += 100
		}
		ai := strings.ToLower(a.AuthIndex)
		if ai != "" && !strings.Contains(ai, "@") && !strings.HasSuffix(ai, ".json") {
			s += 50 // prefer real runtime auth index over filename keys
		}
		if a.LastSignal != "" {
			s += 10
		}
		s += int(a.DayCalls)
		return s
	}
	keyOf := func(a *state.Account) string {
		if e := strings.ToLower(strings.TrimSpace(a.Email)); e != "" {
			return e
		}
		return strings.ToLower(strings.TrimSpace(a.FileName))
	}
	for _, a := range accs {
		k := keyOf(a)
		if k == "" {
			continue
		}
		if cur, ok := best[k]; !ok {
			best[k] = a.AuthIndex
		} else {
			// compare
			var curA *state.Account
			for _, x := range accs {
				if x.AuthIndex == cur {
					curA = x
					break
				}
			}
			if score(a) > score(curA) {
				best[k] = a.AuthIndex
			}
		}
	}
	keep := map[string]bool{}
	for _, id := range best {
		keep[id] = true
	}
	for _, a := range accs {
		k := keyOf(a)
		if k == "" {
			continue
		}
		if !keep[a.AuthIndex] {
			g.State.DeleteAccount(a.AuthIndex)
		}
	}
}

func (g *Guard) stampLastSignal(authIndex, signal string) {
	if g.State == nil || authIndex == "" || signal == "" {
		return
	}
	g.State.SetLastSignal(authIndex, signal)
}

func (g *Guard) resolveFileName(ctx context.Context, authIndex, fileName, email string) string {
	name := fileName
	if name == "" || cpaapi.LooksLikeOpaqueID(name) {
		if g.Resolver != nil {
			_ = g.Resolver.Ensure(ctx)
			if id, ok := g.Resolver.Resolve(authIndex, fileName, email); ok && id.FileName != "" {
				name = id.FileName
				g.State.UpdateMeta(authIndex, id.FileName, id.Email, "")
			}
		}
	}
	return name
}

func (g *Guard) beginFingerprintRun(authIndex, key string) {
	if g.State == nil || authIndex == "" || key == "" {
		return
	}
	acc := g.State.Get(authIndex)
	if acc == nil || acc.LastSignal == "" || acc.LastSignal == key || acc.LastSignal == "any_error" {
		return
	}
	// Historical totals remain in ObservedError.Count/Hits. Account streaks are
	// the current consecutive lifecycle. Preserve only the global any_error run;
	// the previous primary shape is no longer consecutive.
	g.State.ClearAuthStreaksExcept(authIndex, map[string]bool{"any_error": true})
}

// clearStreaksByCountMode clears streaks for keys using streak mode; keeps total-mode counters.
func (g *Guard) clearStreaksByCountMode(authIndex string) {
	if g.State == nil {
		return
	}
	totalKeys := map[string]bool{}
	for _, p := range g.State.ListErrorPolicies() {
		if strings.EqualFold(p.CountMode, "total") || strings.EqualFold(p.CountMode, "accumulate") {
			totalKeys[p.Key] = true
		}
	}
	// any_error defaults to streak (success clears) unless user set total
	if len(totalKeys) == 0 {
		g.State.ClearAuthStreaks(authIndex)
		return
	}
	g.State.ClearAuthStreaksExcept(authIndex, totalKeys)
}

// policyActionRank compares policy outcomes for any_error upgrade (higher wins).
func policyActionRank(a policy.Action) int {
	if a.Disable {
		return 50
	}
	if a.Trash {
		return 40
	}
	if a.Candidate {
		return 20
	}
	if a.Cooldown {
		return 10
	}
	return 0
}

func (g *Guard) applyPermanentDisable(ctx context.Context, ev UsageEvent, reason string) error {
	name := ev.FileName
	if name == "" || cpaapi.LooksLikeOpaqueID(name) {
		if acc := g.State.Get(ev.AuthIndex); acc != nil {
			name = g.resolveFileName(ctx, ev.AuthIndex, acc.FileName, acc.Email)
		} else {
			name = g.resolveFileName(ctx, ev.AuthIndex, ev.FileName, ev.Email)
		}
	}
	// ownership first (closed loop), then CPA file
	g.State.SetAccountState(ev.AuthIndex, state.UserManual, "user_manual")
	g.State.SetRecoverAt(ev.AuthIndex, time.Time{})
	if name != "" && !cpaapi.LooksLikeOpaqueID(name) {
		g.State.UpdateMeta(ev.AuthIndex, name, ev.Email, "")
	}
	if g.CPA != nil && name != "" && !cpaapi.LooksLikeOpaqueID(name) {
		if err := g.CPA.SetDisabled(ctx, name, true); err != nil {
			g.State.Log(state.ActionLog{Auth: ev.AuthIndex, Source: ev.Source, Action: "manual_disable", Reason: "disable_failed:" + err.Error()})
			return err
		}
	}
	if reason == "" {
		reason = "policy_permanent_disable"
	}
	g.State.Log(state.ActionLog{Auth: ev.AuthIndex, Source: ev.Source, Action: "manual_disable", Reason: reason})
	return nil
}

// reopenAfterProbeAlive opens CPA file and exits cool/候删/永禁 when a real patrol probe
// returns HTTP 2xx. Patrol range may be permanent — alive means restore traffic;
// subsequent failures still follow error policies. Trash/purged still refused.
func (g *Guard) reopenAfterProbeAlive(ctx context.Context, ev UsageEvent) {
	if g.State == nil {
		return
	}
	acc := g.State.Get(ev.AuthIndex)
	if acc == nil {
		acc = g.State.Touch(ev.AuthIndex)
	}
	// Trash/purged is destructive storage state, not a schedulable patrol state.
	if acc.State == state.Trashed || acc.State == state.Purged {
		return
	}
	wasCool := acc.State == state.CooldownQuota || acc.State == state.CooldownSpending ||
		acc.State == state.CooldownPermission || acc.State == state.CandidateDead
	wasPermanent := acc.State == state.UserManual || acc.DisableSource == "user_manual"
	// cpa_file_disabled sticky still treated as an owned closed file that probe can open.
	if acc.State == state.UserManual && (acc.DisableSource == "cpa_file_disabled" || acc.DisableSource == "cpa_disabled") {
		wasPermanent = true
	}
	name := ev.FileName
	if name == "" || cpaapi.LooksLikeOpaqueID(name) {
		if acc.FileName != "" && !cpaapi.LooksLikeOpaqueID(acc.FileName) {
			name = acc.FileName
		}
	}
	if (name == "" || cpaapi.LooksLikeOpaqueID(name)) && g.Resolver != nil {
		if id, ok := g.Resolver.Resolve(ev.AuthIndex, ev.FileName, ev.Email); ok && id.FileName != "" {
			name = id.FileName
			g.State.UpdateMeta(ev.AuthIndex, id.FileName, id.Email, "")
		}
	}
	openedFile := false
	if g.CPA != nil && name != "" && !cpaapi.LooksLikeOpaqueID(name) {
		if err := g.CPA.SetDisabled(ctx, name, false); err != nil {
			g.State.Log(state.ActionLog{
				Auth: ev.AuthIndex, Source: "patrol", Action: "patrol_alive_open_failed",
				Reason: err.Error(),
			})
		} else {
			openedFile = true
			em := strings.ToLower(strings.TrimSpace(ev.Email))
			if em == "" {
				em = strings.ToLower(strings.TrimSpace(acc.Email))
			}
			g.State.UpdateMeta(ev.AuthIndex, authFileBase(name), em, "")
		}
	}
	if wasCool || wasPermanent {
		prev := string(acc.State)
		if prev == "" {
			prev = "user_manual"
		}
		sig := acc.LastSignal
		if wasPermanent {
			// full unlock (clear user_manual sticky + streaks); mark 正常·待观察 via pending
			g.State.ClearManualLock(ev.AuthIndex)
			// still want short watch after auto-revive from permanent
			g.State.ResetToActive(ev.AuthIndex) // re-set pending_observe after ClearManualLock cleared it
		} else {
			// cool/候删: keep streak continuity for ladder
			g.State.ResetToActive(ev.AuthIndex)
		}
		reason := "探活成功 · 已打开文件并退出" + prev
		if wasPermanent {
			reason = "探活成功 · 永禁号恢复接流（后续错误仍按策略）"
		}
		g.State.Log(state.ActionLog{
			Auth: ev.AuthIndex, Source: "patrol", Signal: sig,
			Action: "patrol_alive_reopen",
			Reason: reason,
		})
		return
	}
	// Active (or empty): ensure file open + stamp so action rail updates live
	if openedFile {
		g.State.Log(state.ActionLog{
			Auth: ev.AuthIndex, Source: "patrol",
			Action: "patrol_alive_open",
			Reason: "探活成功 · 文件已确保开启",
		})
	} else {
		// still stamp success for live UI even if no file name / already open
		g.State.StampLastAction(ev.AuthIndex, "patrol_alive")
	}
}

// ManualDisable disables one account via CPA and marks user_manual.
func (g *Guard) ManualDisable(ctx context.Context, authIndex string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	acc := g.State.Get(authIndex)
	if acc == nil {
		acc = g.State.Touch(authIndex)
	}
	name := g.resolveFileName(ctx, authIndex, acc.FileName, acc.Email)
	g.State.SetAccountState(authIndex, state.UserManual, "user_manual")
	g.State.SetRecoverAt(authIndex, time.Time{})
	if name != "" && !cpaapi.LooksLikeOpaqueID(name) {
		g.State.UpdateMeta(authIndex, name, acc.Email, "")
	}
	if g.CPA != nil && name != "" && !cpaapi.LooksLikeOpaqueID(name) {
		if err := g.CPA.SetDisabled(ctx, name, true); err != nil {
			_ = g.State.Save()
			return err
		}
	}
	g.State.Log(state.ActionLog{Auth: authIndex, Source: "panel", Action: "manual_disable", Reason: "permanent_disable"})
	return g.State.Save()
}

// ManualEnable re-enables account even if previously user_manual.
func (g *Guard) ManualEnable(ctx context.Context, authIndex string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	acc := g.State.Get(authIndex)
	if acc == nil {
		acc = g.State.Touch(authIndex)
	}
	name := g.resolveFileName(ctx, authIndex, acc.FileName, acc.Email)
	if g.CPA != nil && name != "" && !cpaapi.LooksLikeOpaqueID(name) {
		opened, err, stillClosed := g.ensureAuthEnabled(ctx, name)
		if err != nil {
			g.State.Log(state.ActionLog{Auth: authIndex, Source: "panel", Action: "manual_enable_failed", Reason: err.Error()})
			_ = g.State.Save()
			return err
		}
		if !opened || stillClosed {
			// still mark active so panel reflects intent; heal will retry with rate limit
			g.State.ClearManualLock(authIndex)
			g.State.Log(state.ActionLog{
				Auth: authIndex, Source: "panel", Action: "manual_enable_file_still_closed",
				Reason: "面板启用后校验文件仍关 · 将由维护强制打开补救",
			})
			return g.State.Save()
		}
	}
	g.State.ClearManualLock(authIndex)
	g.State.Log(state.ActionLog{Auth: authIndex, Source: "panel", Action: "manual_enable", Reason: "panel bulk/manual"})
	return g.State.Save()
}

// ManualTrash snapshots+deletes auth file into trash bin.
func (g *Guard) ManualTrash(ctx context.Context, authIndex string) error {
	acc := g.State.Get(authIndex)
	if acc == nil {
		acc = g.State.Touch(authIndex)
	}
	ev := UsageEvent{AuthIndex: authIndex, FileName: acc.FileName, Email: acc.Email, Source: "panel"}
	res := match.Result{Signal: match.Signal(acc.LastSignal), Reason: "manual_trash"}
	if res.Signal == "" {
		res.Signal = match.SignalAuth401
	}
	return g.applyTrash(ctx, ev, res, acc)
}

// ApplySuggestedCooldown cools accounts currently active with free_usage_429 (or provided list).
func (g *Guard) ApplySuggestedCooldown(ctx context.Context, authIndexes []string, hours int) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if hours <= 0 {
		hours = 24
	}
	n := 0
	now := g.Now()
	recoverAt := now.Add(time.Duration(hours) * time.Hour)
	if g.Cfg.MaxResetSeconds > 0 {
		maxAt := now.Add(time.Duration(g.Cfg.MaxResetSeconds) * time.Second)
		if recoverAt.After(maxAt) {
			recoverAt = maxAt
		}
	}
	targets := authIndexes
	if len(targets) == 0 {
		for _, acc := range g.State.AccountsSnapshot() {
			if acc.State == state.Active && acc.LastSignal == string(match.SignalFreeUsage429) {
				targets = append(targets, acc.AuthIndex)
			}
		}
	}
	for _, id := range targets {
		acc := g.State.Get(id)
		if acc == nil {
			continue
		}
		name := g.resolveFileName(ctx, id, acc.FileName, acc.Email)
		if g.CPA != nil && name != "" && !cpaapi.LooksLikeOpaqueID(name) {
			if err := g.CPA.SetDisabled(ctx, name, true); err != nil {
				g.State.Log(state.ActionLog{Auth: id, Source: "panel", Action: "cooldown_failed", Reason: err.Error()})
				continue
			}
		}
		g.State.SetAccountState(id, state.CooldownQuota, "plugin_auto")
		g.State.SetRecoverAt(id, recoverAt)
		g.State.Log(state.ActionLog{Auth: id, Source: "panel", Signal: acc.LastSignal, Action: "cooldown", Reason: "bulk_suggested_cooldown"})
		n++
	}
	_ = g.State.Save()
	return n, nil
}

// Bulk runs disable|enable|trash|cooldown on a list of auth indexes.
func (g *Guard) Bulk(ctx context.Context, action string, authIndexes []string) (ok int, fail int, errors []string) {
	for _, id := range authIndexes {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		var err error
		switch action {
		case "disable":
			err = g.ManualDisable(ctx, id)
		case "enable":
			err = g.ManualEnable(ctx, id)
		case "trash", "delete":
			err = g.ManualTrash(ctx, id)
		case "cooldown":
			var n int
			n, err = g.ApplySuggestedCooldown(ctx, []string{id}, 24)
			if err == nil && n == 0 {
				err = nil
			}
		default:
			err = nil
			fail++
			errors = append(errors, id+": unknown action")
			continue
		}
		if err != nil {
			fail++
			errors = append(errors, id+": "+err.Error())
		} else {
			ok++
		}
	}
	return ok, fail, errors
}
