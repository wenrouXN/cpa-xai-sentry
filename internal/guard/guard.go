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
		g.clearStreaksByCountMode(ev.AuthIndex)
		// closed-loop: successful request while active clears residual error signal
		if acc := g.State.Get(ev.AuthIndex); acc != nil && acc.State == state.Active && acc.LastSignal != "" {
			g.State.SetLastSignal(ev.AuthIndex, "")
		}
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
		g.State.SetAccountState(ev.AuthIndex, state.CandidateDead, "plugin_auto")
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
	if name != "" && !cpaapi.LooksLikeOpaqueID(name) {
		g.State.UpdateMeta(ev.AuthIndex, name, ev.Email, "")
	}
	if g.CPA != nil && name != "" && !cpaapi.LooksLikeOpaqueID(name) {
		if err := g.CPA.SetDisabled(ctx, name, true); err != nil {
			g.State.Log(state.ActionLog{
				Auth: ev.AuthIndex, Source: ev.Source, Signal: string(res.Signal),
				Action: "cooldown_failed", Reason: err.Error(),
			})
			// keep plugin_auto cool-down ownership even if file disable failed
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
	// Align sentry state with CPA disabled files (do NOT auto-open by default).
	// Optional reopen of non-sentry disables is gated by reopen_foreign_disabled.
	if _, err := g.syncDisabledFromCPA(ctx, now); err != nil {
		g.State.Log(state.ActionLog{Source: "tick", Action: "sync_disabled_failed", Reason: err.Error()})
	}
	g.pruneDuplicateAccounts()
	// closed-loop hygiene: Active must be clean (no residual signal/plugin_auto lock)
	g.scrubDirtyActiveAccounts()
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
		// closed-loop: cool-down due → clean Active (not Active+plugin_auto leftovers)
		prevSig := acc.LastSignal
		g.State.ResetToActive(acc.AuthIndex)
		g.State.Log(state.ActionLog{
			Auth: acc.AuthIndex, Source: "tick", Signal: prevSig,
			Action: "reenable", Reason: "recover_at",
		})
	}
	if g.Trash != nil && g.Cfg.TrashAutoPurge {
		if _, err := g.Trash.PurgeExpired(now); err != nil {
			return err
		}
	}
	return g.State.Save()
}

// syncDisabledFromCPA inspects CPA auth files that are currently disabled.
//
// Default (reopen_foreign_disabled=false):
//   - Never auto-open disabled files.
//   - If sentry does not already own the disable (plugin_auto cool-down / user_manual),
//     mark state as user_manual + cpa_file_disabled so the panel shows「CPA已禁用」
//     and operators can enable manually.
//
// Optional (reopen_foreign_disabled=true):
//   - Re-enable files that are disabled but NOT owned by this sentry
//     (legacy behaviour; can undo operator/CPA-panel disables — use carefully).
func (g *Guard) syncDisabledFromCPA(ctx context.Context, now time.Time) (int, error) {
	if g.CPA == nil {
		return 0, nil
	}
	files, err := g.CPA.ListAuthFiles(ctx)
	if err != nil {
		return 0, err
	}
	// index sentry accounts by file/email
	byFile := map[string]*state.Account{}
	byEmail := map[string]*state.Account{}
	for _, acc := range g.State.AccountsSnapshot() {
		if acc.FileName != "" {
			byFile[strings.ToLower(strings.TrimSpace(acc.FileName))] = acc
		}
		if acc.Email != "" {
			byEmail[strings.ToLower(strings.TrimSpace(acc.Email))] = acc
		}
	}
	// protect=true: sentry owns this disable — never reopen, never re-tag as foreign.
	// Be conservative: any cool-down/候删/manual/plugin_auto residue is owned.
	protect := func(acc *state.Account) bool {
		if acc == nil {
			return false
		}
		switch acc.State {
		case state.Trashed, state.Purged:
			return true
		case state.CooldownQuota, state.CooldownSpending, state.CooldownPermission, state.CandidateDead:
			// cool-down / 候删 regardless of disable_source (source may be wiped by bugs)
			return true
		case state.UserManual:
			return true
		}
		if acc.PreDisabled {
			return true
		}
		switch acc.DisableSource {
		case "user_manual", "plugin_auto", "cpa_file_disabled", "cpa_disabled":
			// plugin_auto on Active is half-dirty but still OUR prior action — do not reopen
			return true
		}
		// future recover_at means we still intend a cool-down window
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

	n := 0
	for _, f := range files {
		if !f.Disabled {
			continue
		}
		name := strings.TrimSpace(f.Name)
		prov := f.Provider
		if prov == "" {
			prov = f.Type
		}
		if name == "" || !cpaapi.IsXAIName(name, prov) {
			continue
		}
		low := strings.ToLower(name)
		em := strings.ToLower(strings.TrimSpace(f.Email))
		// gather all candidates (map last-write is lossy; also scan snapshot keys)
		var cands []*state.Account
		if a := byFile[low]; a != nil {
			cands = append(cands, a)
		}
		if em != "" {
			if a := byEmail[em]; a != nil {
				cands = append(cands, a)
			}
			if a := byFile["xai-"+em+".json"]; a != nil {
				cands = append(cands, a)
			}
		}
		// also match by auth_index==name for rows keyed by filename
		for _, acc := range g.State.AccountsSnapshot() {
			if strings.EqualFold(strings.TrimSpace(acc.FileName), name) ||
				strings.EqualFold(strings.TrimSpace(acc.AuthIndex), name) ||
				(em != "" && strings.EqualFold(strings.TrimSpace(acc.Email), em)) {
				cands = append(cands, acc)
			}
		}
		acc := pickAcc(cands...)
		// sentry-owned disable: leave file alone
		if protect(acc) {
			continue
		}

		// Optional legacy reopen of non-owned disables
		if g.Cfg.ReopenForeignDisabled {
			if err := g.CPA.SetDisabled(ctx, name, false); err != nil {
				auth := name
				if acc != nil {
					auth = acc.AuthIndex
				}
				g.State.Log(state.ActionLog{Auth: auth, Source: "tick", Action: "reopen_foreign_failed", Reason: err.Error()})
				continue
			}
			if acc != nil {
				g.State.ClearManualLock(acc.AuthIndex)
				g.State.Log(state.ActionLog{Auth: acc.AuthIndex, Source: "tick", Action: "reopen_foreign", Reason: "reopen_foreign_disabled=true"})
			} else {
				g.State.Log(state.ActionLog{Auth: name, Source: "tick", Action: "reopen_foreign", Reason: "reopen_foreign_disabled=true untracked"})
			}
			n++
			continue
		}

		// Default: keep file disabled; align sentry state so panel shows CPA已禁用
		// and never treat it as a foreign disable to reopen later.
		authIndex := ""
		if acc != nil {
			authIndex = acc.AuthIndex
		} else {
			// create a tracked row keyed by file name so next ticks protect it
			authIndex = name
			g.State.Touch(authIndex)
			if em != "" {
				g.State.UpdateMeta(authIndex, name, em, "")
			} else {
				g.State.UpdateMeta(authIndex, name, "", "")
			}
		}
		g.State.SetAccountState(authIndex, state.UserManual, "cpa_file_disabled")
		g.State.SetRecoverAt(authIndex, time.Time{})
		g.State.Log(state.ActionLog{
			Auth: authIndex, Source: "tick", Action: "file_disabled_sync",
			Reason: "cpa_disabled_sync",
		})
		n++
	}
	return n, nil
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
		s := 0
		ai := strings.ToLower(a.AuthIndex)
		if ai != "" && !strings.Contains(ai, "@") && !strings.HasSuffix(ai, ".json") {
			s += 100
		}
		if a.LastSignal != "" {
			s += 20
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
	if len(totalKeys) == 0 {
		g.State.ClearAuthStreaks(authIndex)
		return
	}
	g.State.ClearAuthStreaksExcept(authIndex, totalKeys)
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
	if g.CPA != nil && name != "" && !cpaapi.LooksLikeOpaqueID(name) {
		if err := g.CPA.SetDisabled(ctx, name, true); err != nil {
			g.State.Log(state.ActionLog{Auth: ev.AuthIndex, Source: ev.Source, Action: "manual_disable", Reason: "disable_failed:" + err.Error()})
			return err
		}
	}
	g.State.SetAccountState(ev.AuthIndex, state.UserManual, "user_manual")
	g.State.SetRecoverAt(ev.AuthIndex, time.Time{})
	if reason == "" {
		reason = "policy_permanent_disable"
	}
	g.State.Log(state.ActionLog{Auth: ev.AuthIndex, Source: ev.Source, Action: "manual_disable", Reason: reason})
	return nil
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
	g.State.Log(state.ActionLog{Auth: authIndex, Source: "panel", Action: "manual_disable", Reason: "permanent_disable"})
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
