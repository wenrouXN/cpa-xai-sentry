package guard

import (
	"context"
	"strings"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
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
	Cfg      sentrycfg.Config
	State    *state.Store
	Trash    *trash.Store
	CPA      *cpaapi.Client
	Resolver *cpaapi.Resolver
	// hooks for tests
	Now func() time.Time
}

func New(cfg sentrycfg.Config, st *state.Store, tr *trash.Store, cpa *cpaapi.Client) *Guard {
	g := &Guard{Cfg: cfg.Validate(), State: st, Trash: tr, CPA: cpa, Now: time.Now}
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
		st.EnsureBuiltinPolicies(builtins)
	}
	return g
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
}

// HandleUsage applies match+policy. source defaults to usage.
func (g *Guard) HandleUsage(ctx context.Context, ev UsageEvent) error {
	if !g.Cfg.Enabled || !g.Cfg.SentryEnabled {
		return nil
	}
	g.enrichIdentity(ctx, &ev)
	if !IsXAI(ev.Provider, ev.FileName) && !IsXAI(ev.Provider, ev.AuthIndex) && !IsXAI(ev.Provider, ev.Email) {
		return nil
	}
	if ev.Source == "" {
		ev.Source = "usage"
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
		g.State.ClearAuthStreaks(ev.AuthIndex)
		// still try parse remaining from success body if any
		if q := quota.Parse(ev.Body); q.Limit > 0 || q.Remaining > 0 || q.Used > 0 {
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
	errKey := errorsig.KeyFromMatch(res, ev.StatusCode)
	label := errorsig.LabelOf(errKey, res, ev.StatusCode)
	// always learn/observe errors into dynamic catalog (even unmatched)
	// keep enough of body to retain tokens (actual/limit) for UI/quota rehydration
	sample := ev.Body
	if len(sample) > 900 {
		sample = sample[:900]
	}
	g.State.ObserveError(errKey, label, string(res.Signal), res.Code, sample, ev.AuthIndex, ev.FileName, ev.Source, ev.StatusCode)

	// seed policy entry for newly learned errors (default observe)
	if _, ok := g.State.GetErrorPolicy(errKey); !ok {
		act := "observe"
		th := 1
		cd := 0
		never := errorsig.HardNeverTrash(errKey)
		if res.Signal != match.SignalNone {
			if p0, ok := errorsig.BuiltinDefaults()[string(res.Signal)]; ok {
				act = string(p0.Action)
				th = p0.Threshold
				cd = p0.CooldownSec
				never = p0.NeverTrash || never
				label = p0.Label
			}
		}
		g.State.UpsertErrorPolicy(state.ErrorPolicy{
			Key: errKey, Label: label, Enabled: true, Action: act,
			Threshold: th, CooldownSec: cd, NeverTrash: never,
			Note: "动态采集", Source: "learned",
		})
	}

	if res.Signal == match.SignalNone {
		// unmatched: cataloged only
		_ = g.State.Save()
		return nil
	}
	if res.Kind == match.KindQuota {
		g.State.ClearAuthStreaks(ev.AuthIndex)
	}

	streakKey := errKey
	if res.Signal != match.SignalNone {
		streakKey = string(res.Signal)
	}
	streak := g.State.IncStreak(ev.AuthIndex, streakKey)
	var polPtr *state.ErrorPolicy
	if p, ok := g.State.GetErrorPolicy(errKey); ok {
		polPtr = &p
	}
	act := policy.Decide(g.Cfg, policy.Input{
		Signal: res.Signal, ErrorKey: errKey, Streak: streak,
		Tier: tier.Tier(acc.Tier), Policy: polPtr,
	})

	if act.Cooldown {
		if err := g.applyCooldown(ctx, ev, res, acc, polPtr); err != nil {
			return err
		}
	}
	if act.Candidate {
		g.State.SetAccountState(ev.AuthIndex, state.CandidateDead, "plugin_auto")
		g.State.Log(state.ActionLog{
			Auth: ev.AuthIndex, Source: ev.Source, Signal: string(res.Signal),
			Action: "candidate", Reason: act.Reason,
		})
	}
	if act.Trash {
		return g.applyTrash(ctx, ev, res, acc)
	}
	_ = g.State.Save()
	return nil
}

func (g *Guard) applyCooldown(ctx context.Context, ev UsageEvent, res match.Result, acc *state.Account, pol *state.ErrorPolicy) error {
	st := state.CooldownQuota
	switch res.Signal {
	case match.SignalSpendingLimit402:
		st = state.CooldownSpending
	case match.SignalPermission403:
		st = state.CooldownPermission
	case match.SignalAuth401:
		st = state.CandidateDead
	}
	recoverAt := res.RecoverAt
	if recoverAt.IsZero() {
		if pol != nil && pol.CooldownSec > 0 {
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
	g.State.SetAccountState(ev.AuthIndex, st, "plugin_auto")
	g.State.SetRecoverAt(ev.AuthIndex, recoverAt)
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
	if g.CPA != nil && name != "" && !cpaapi.LooksLikeOpaqueID(name) {
		if err := g.CPA.SetDisabled(ctx, name, true); err != nil {
			g.State.Log(state.ActionLog{
				Auth: ev.AuthIndex, Source: ev.Source, Signal: string(res.Signal),
				Action: "cooldown_failed", Reason: err.Error(),
			})
			return err
		}
	}
	g.State.Log(state.ActionLog{
		Auth: ev.AuthIndex, Source: ev.Source, Signal: string(res.Signal),
		Action: "cooldown", Reason: res.Reason,
	})
	return nil
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

// Tick recovers cooldowns, syncs CPA disabled status, refreshes identity metadata, and auto-purges trash.
func (g *Guard) Tick(ctx context.Context) error {
	if !g.Cfg.Enabled || !g.Cfg.SentryEnabled {
		return nil
	}
	now := g.Now()
	// Best-effort identity refresh so panel can show email/file even for opaque auth_index.
	if g.Resolver != nil {
		_ = g.Resolver.Ensure(ctx)
		for _, acc := range g.State.AccountsSnapshot() {
			if id, ok := g.Resolver.Resolve(acc.AuthIndex, acc.FileName, acc.Email); ok {
				g.State.UpdateMeta(acc.AuthIndex, id.FileName, id.Email, "")
			}
		}
	}
	// Align sentry state with CPA auth-file disabled flags (no network probe, no quota cost).
	// Covers cases where file was disabled outside sentry (manual / other tools) or usage callback missed.
	synced, syncErr := g.syncDisabledFromCPA(ctx, now)
	_ = synced
	if syncErr != nil {
		// non-fatal: still continue recover/purge
		g.State.Log(state.ActionLog{Source: "tick", Action: "sync_disabled_failed", Reason: syncErr.Error()})
	}
	for _, acc := range g.State.AccountsSnapshot() {
		if acc.RecoverAt.IsZero() || acc.RecoverAt.After(now) {
			continue
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
			if err := g.CPA.SetDisabled(ctx, name, false); err != nil {
				g.State.Log(state.ActionLog{
					Auth: acc.AuthIndex, Source: "tick", Action: "reenable_failed", Reason: err.Error(),
				})
				continue
			}
		}
		g.State.SetAccountState(acc.AuthIndex, state.Active, "plugin_auto")
		g.State.SetRecoverAt(acc.AuthIndex, time.Time{})
		g.State.Log(state.ActionLog{
			Auth: acc.AuthIndex, Source: "tick", Action: "reenable", Reason: "recover_at",
		})
	}
	if g.Trash != nil && g.Cfg.TrashAutoPurge {
		if _, err := g.Trash.PurgeExpired(now); err != nil {
			return err
		}
	}
	return g.State.Save()
}

// clean up verbose comments in sync - simplify the signal stamp block
func (g *Guard) syncDisabledFromCPA(ctx context.Context, now time.Time) (int, error) {
	if g.CPA == nil {
		return 0, nil
	}
	files, err := g.CPA.ListAuthFiles(ctx)
	if err != nil {
		return 0, err
	}
	byName := map[string]cpaapi.AuthFile{}
	byEmail := map[string]cpaapi.AuthFile{}
	for _, f := range files {
		name := strings.ToLower(strings.TrimSpace(f.Name))
		if name != "" {
			byName[name] = f
		}
		em := strings.ToLower(strings.TrimSpace(f.Email))
		if em != "" {
			byEmail[em] = f
		}
	}
	n := 0
	for _, acc := range g.State.AccountsSnapshot() {
		if acc.State == state.UserManual || acc.State == state.Trashed || acc.State == state.Purged || acc.State == state.CandidateDead {
			continue
		}
		// already cooling with recover_at — skip
		if (acc.State == state.CooldownQuota || acc.State == state.CooldownSpending || acc.State == state.CooldownPermission) && !acc.RecoverAt.IsZero() {
			continue
		}
		fn := strings.ToLower(strings.TrimSpace(acc.FileName))
		em := strings.ToLower(strings.TrimSpace(acc.Email))
		f, ok := byName[fn]
		if !ok && em != "" {
			f, ok = byEmail[em]
		}
		if !ok && em != "" {
			f, ok = byName["xai-"+em+".json"]
		}
		if !ok || !f.Disabled {
			continue
		}
		isFree := acc.LastSignal == string(match.SignalFreeUsage429) ||
			strings.Contains(acc.QuotaSource, "free_usage") ||
			strings.Contains(acc.QuotaSource, "cpamp_fail_body") ||
			(acc.QuotaLimit > 0 && acc.QuotaUsed >= acc.QuotaLimit)
		sig := acc.LastSignal
		reason := "cpa_disabled_sync"
		if isFree {
			sig = string(match.SignalFreeUsage429)
			reason = "cpa_disabled_free_usage_sync"
		}
		// mark cooling if still active or cooling without recover_at
		if acc.State == state.Active || acc.State == "" ||
			((acc.State == state.CooldownQuota || acc.State == state.CooldownSpending || acc.State == state.CooldownPermission) && acc.RecoverAt.IsZero()) {
			g.State.SetAccountState(acc.AuthIndex, state.CooldownQuota, "plugin_auto")
		}
		if acc.RecoverAt.IsZero() || acc.RecoverAt.Before(now) {
			rec := now.Add(24 * time.Hour)
			if g.Cfg.MaxResetSeconds > 0 {
				maxAt := now.Add(time.Duration(g.Cfg.MaxResetSeconds) * time.Second)
				if rec.After(maxAt) {
					rec = maxAt
				}
			}
			g.State.SetRecoverAt(acc.AuthIndex, rec)
		}
		if sig != "" {
			g.stampLastSignal(acc.AuthIndex, sig)
		}
		// also record into error catalog so policy page has account rows
		sample := "CPA auth file disabled; synced by maintenance tick"
		if isFree {
			sample = "CPA auth file disabled + free-usage evidence; synced by maintenance tick"
			g.State.ObserveError("free_usage_429", "免费额度用尽(429)", "free_usage_429", "subscription:free-usage-exhausted", sample, acc.AuthIndex, acc.FileName, "tick", 429)
		} else {
			g.State.ObserveError("http_0_disabled", "CPA已禁用(同步)", "", "cpa_disabled", sample, acc.AuthIndex, acc.FileName, "tick", 0)
		}
		g.State.Log(state.ActionLog{
			Auth: acc.AuthIndex, Source: "tick", Signal: sig,
			Action: "cooldown", Reason: reason,
		})
		n++
	}
	return n, nil
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

// ManualDisable disables one account via CPA and marks user_manual.
func (g *Guard) ManualDisable(ctx context.Context, authIndex string) error {
	acc := g.State.Get(authIndex)
	if acc == nil {
		acc = g.State.Touch(authIndex)
	}
	name := g.resolveFileName(ctx, authIndex, acc.FileName, acc.Email)
	if g.CPA != nil && name != "" && !cpaapi.LooksLikeOpaqueID(name) {
		if err := g.CPA.SetDisabled(ctx, name, true); err != nil {
			return err
		}
	}
	g.State.SetAccountState(authIndex, state.UserManual, "user_manual")
	g.State.SetRecoverAt(authIndex, time.Time{})
	g.State.Log(state.ActionLog{Auth: authIndex, Source: "panel", Action: "manual_disable", Reason: "panel bulk/manual"})
	return g.State.Save()
}

// ManualEnable re-enables account even if previously user_manual.
func (g *Guard) ManualEnable(ctx context.Context, authIndex string) error {
	acc := g.State.Get(authIndex)
	if acc == nil {
		acc = g.State.Touch(authIndex)
	}
	name := g.resolveFileName(ctx, authIndex, acc.FileName, acc.Email)
	if g.CPA != nil && name != "" && !cpaapi.LooksLikeOpaqueID(name) {
		if err := g.CPA.SetDisabled(ctx, name, false); err != nil {
			return err
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
