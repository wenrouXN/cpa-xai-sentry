//go:build cshared

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/cpamp"
	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/panel"
	"github.com/openclaw-local/cpa-xai-sentry/internal/patrol"
	"github.com/openclaw-local/cpa-xai-sentry/internal/regjob"
	"github.com/openclaw-local/cpa-xai-sentry/internal/persist"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type Runtime struct {
	mu     sync.Mutex
	Cfg    sentrycfg.Config
	State  *state.Store
	Trash  *trash.Store
	CPA    *cpaapi.Client
	Guard  *guard.Guard
	Patrol   *patrol.Runner
	Register *regjob.Runner
	Panel    *panel.API

	stopCh chan struct{}
	wg     sync.WaitGroup
}

var (
	rtOnce sync.Once
	rtInst *Runtime
)

func runtimeInstance() *Runtime {
	rtOnce.Do(func() {
		cfg := configDefaults()
		rt := &Runtime{Cfg: cfg, stopCh: make(chan struct{})}
		if err := rt.rebuild(cfg); err != nil {
			hostLog("error", "init runtime failed: "+err.Error())
		}
		rt.startTicker()
		rt.startPatrolTicker()
		rt.startRegisterTicker()
		rtInst = rt
	})
	return rtInst
}

func shutdownRuntime() {
	if rtInst == nil {
		return
	}
	rtInst.mu.Lock()
	defer rtInst.mu.Unlock()
	select {
	case <-rtInst.stopCh:
	default:
		close(rtInst.stopCh)
	}
	rtInst.wg.Wait()
}

func (r *Runtime) ApplyConfig(cfg sentrycfg.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg = normalizePaths(cfg).Validate()
	// Bidirectional last-writer-wins:
	//  - Panel save  → dual-write host plugins.configs + overrides (PersistPanelConfig)
	//  - Host/official plugin page / config.yaml reconfigure → host is authority;
	//    mirror into overrides so the next panel load matches (no overrides stomping host).
	if err := persist.Save(persist.PathFor(cfg), persist.FromConfig(cfg)); err != nil {
		hostLog("warn", "mirror host config → overrides: "+err.Error())
	}
	if err := r.rebuild(cfg); err != nil {
		hostLog("error", "apply config failed: "+err.Error())
	}
}

func normalizePaths(cfg sentrycfg.Config) sentrycfg.Config {
	// Prefer mounted auth_dir for durable state/trash when relative paths are used.
	if cfg.AuthDir != "" {
		if cfg.StatePath == "" || strings.HasPrefix(cfg.StatePath, "data/") || cfg.StatePath == "data/cpa-xai-sentry-state.json" {
			cfg.StatePath = filepath.Join(cfg.AuthDir, "cpa-xai-sentry", "state.json")
		}
		if cfg.TrashDir == "" || strings.HasPrefix(cfg.TrashDir, "data/") || cfg.TrashDir == "data/cpa-xai-sentry-trash" {
			cfg.TrashDir = filepath.Join(cfg.AuthDir, "cpa-xai-sentry", "trash")
		}
	}
	return cfg
}

// PersistPanelConfig is the panel "save" path (last writer = panel):
//  1. runtime-overrides.json
//  2. CPA host plugins.configs.cpa-xai-sentry (GET+merge+PUT)
//
// Host reconfigure path is ApplyConfig (last writer = host → mirrors to overrides).
func (r *Runtime) PersistPanelConfig() error {
	r.mu.Lock()
	cfg := r.Cfg
	cpa := r.CPA
	r.mu.Unlock()

	path := persist.PathFor(cfg)
	if err := persist.Save(path, persist.FromConfig(cfg)); err != nil {
		return err
	}
	// Official CPA: PUT plugins.configs.<pluginID> (GET+merge inside WritePluginConfig)
	if cpa != nil {
		if err := cpa.WritePluginConfig(context.Background(), cfg.HostPluginPatch()); err != nil {
			hostLog("warn", "host plugin config sync: "+err.Error())
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Panel != nil && r.Panel.Cfg != nil {
		*r.Panel.Cfg = r.Cfg
	}
	if r.Guard != nil {
		r.Guard.Cfg = r.Cfg
	}
	if r.Patrol != nil {
		r.Patrol.Cfg = r.Cfg
		r.Patrol.Guard = r.Guard
	}
	return nil
}

func (r *Runtime) rebuild(cfg sentrycfg.Config) error {
	cfg = normalizePaths(cfg).Validate()

	var st *state.Store
	if r.State != nil {
		// Hot reconfigure: keep the existing in-memory state to avoid losing
		// accounts that patrol/tick created since the last Save.
		// Only reload from disk on cold start (first init).
		st = r.State
		// Save current state so disk is up-to-date before rewiring.
		_ = st.Save()
		hostLog("info", fmt.Sprintf("rebuild: reusing in-memory state (%d accounts)", len(st.AccountsSnapshot())))
	} else {
		// Cold start: load from disk.
		var err error
		st, err = state.Load(cfg.StatePath)
		if err != nil {
			hostLog("error", "load state: "+err.Error())
			st = state.New(cfg.StatePath)
		}
	}

	tr := trash.New(cfg.TrashDir, cfg.TrashRetentionDays, cfg.TrashAutoPurge, st)
	cpa := cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, cfg.AuthDir)
	g := guard.New(cfg, st, tr, cpa)
	p := patrol.New(cfg, g, cpa)
	// Wire GetGuard so patrol refreshes its Guard reference before each run,
	// avoiding stale pointers after ApplyConfig/rebuild.
	p.GetGuard = func() *guard.Guard {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.Guard
	}
	g.PatrolRunning = func() bool {
		return p.IsRunning()
	}
	// durable patrol job list (survives docker/plugin restart)
	if cfg.StatePath != "" {
		patrol.SetHistoryPath(filepath.Join(filepath.Dir(cfg.StatePath), "patrol-history.json"))
	} else if cfg.AuthDir != "" {
		patrol.SetHistoryPath(filepath.Join(cfg.AuthDir, "cpa-xai-sentry", "patrol-history.json"))
	}
	var reg *regjob.Runner
	if r.Register != nil {
		reg = r.Register
		reg.ApplyConfig(cfg)
	} else {
		reg = regjob.New(cfg)
	}
	if cfg.StatePath != "" {
		reg.SetHistoryPath(filepath.Join(filepath.Dir(cfg.StatePath), "register-history.json"))
	} else if cfg.AuthDir != "" {
		reg.SetHistoryPath(filepath.Join(cfg.AuthDir, "cpa-xai-sentry", "register-history.json"))
	}
	reg.Logf = func(level, msg string) {
		hostLog(level, msg)
		if st != nil {
			st.Log(state.ActionLog{At: time.Now(), Source: "register", Action: "register", Reason: msg})
		}
	}
		reg.PoolCounter = func(ctx context.Context) (enabled, cooldown int) {
		// CPA enabled files + sentry cooldown accounts (current cool, not only same-day recover)
		if r.CPA != nil {
			files, err := r.CPA.ListAuthFiles(ctx)
			if err == nil {
				for _, f := range files {
					name := strings.TrimSpace(f.Name)
					prov := f.Provider
					if prov == "" {
						prov = f.Type
					}
					if name == "" || !cpaapi.IsXAIName(name, prov) {
						continue
					}
					if !f.Disabled {
						enabled++
					}
				}
			}
		}
		if st != nil {
			for _, acc := range st.AccountsSnapshot() {
				if acc == nil {
					continue
				}
				if strings.Contains(string(acc.State), "cooldown") {
					cooldown++
				}
			}
		}
		return enabled, cooldown
	}

	g.TryRelogin = func(ctx context.Context, email, auth string) (bool, string) {
		if reg == nil {
			return false, "register_nil"
		}
		return reg.TryAuth401Relogin(ctx, email, auth)
	}
	api := &panel.API{
		Cfg: &cfg, State: st, Trash: tr, Guard: g, Patrol: p, Register: reg,
		PersistConfig: func(c sentrycfg.Config) error {
			r.mu.Lock()
			r.Cfg = c
			if r.CPA != nil {
				r.CPA.BaseURL = c.ManagementURL
				r.CPA.Key = c.ManagementKey
				r.CPA.AuthDir = c.AuthDir
			}
			r.mu.Unlock()
			return r.PersistPanelConfig()
		},
		GetConfig: func() sentrycfg.Config {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.Cfg
		},
		SetConfig: func(c sentrycfg.Config) {
			r.mu.Lock()
			r.Cfg = c
			if r.Guard != nil {
				r.Guard.Cfg = c
			}
			if r.Patrol != nil {
				r.Patrol.Cfg = c
				r.Patrol.Guard = r.Guard
			}
			if r.Register != nil {
				r.Register.ApplyConfig(c)
			}
			r.mu.Unlock()
		},
	}
	r.Cfg = cfg
	r.State = st
	r.Trash = tr
	r.CPA = cpa
	r.Guard = g
	r.Patrol = p
	r.Register = reg
	r.Panel = api
	return nil
}

func (r *Runtime) startTicker() {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		// first tick soon after load for identity refresh + optional CPAMP backfill
		first := true
		for {
			r.mu.Lock()
			sec := r.Cfg.TickSeconds
			if sec <= 0 {
				sec = 30
			}
			if first {
				sec = 3
				first = false
			}
			g := r.Guard
			cfg := r.Cfg
			st := r.State
			r.mu.Unlock()
			select {
			case <-r.stopCh:
				return
			case <-time.After(time.Duration(sec) * time.Second):
				if g != nil {
					// P0: CPAMP fail backfill BEFORE Tick heal so we never
					// force-open an account that will be cooled same cycle.
					r.maybeBackfillCPAMPFailures(cfg, st, g)
					if err := g.Tick(context.Background()); err != nil {
						hostLog("error", "tick: "+err.Error())
					}
					// clear skip set after heal path ran
					g.ClearPendingBackfillAuths()
				}
				r.maybeAutoBackfill(cfg, st)
			}
		}
	}()
}

// startPatrolTicker runs scheduled auto patrol using cfg.PatrolMode (all/enabled/cooldown/permanent).
func (r *Runtime) startPatrolTicker() {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		// stagger first run a bit after boot so identity resolver can warm
		first := true
		for {
			r.mu.Lock()
			enabled := r.Cfg.PatrolEnabled
			interval := r.Cfg.PatrolInterval
			modeStr := r.Cfg.PatrolMode
			p := r.Patrol
			r.mu.Unlock()
			if interval <= 0 {
				interval = 3600
			}
			wait := interval
			if first {
				wait = 20 // first auto patrol ~20s after start when enabled
				first = false
			}
			select {
			case <-r.stopCh:
				return
			case <-time.After(time.Duration(wait) * time.Second):
				if !enabled || p == nil {
					continue
				}
				mode := patrol.ParseMode(modeStr)
				if _, err := p.Start(context.Background(), mode); err != nil {
					hostLog("error", "auto patrol: "+err.Error())
				} else {
					hostLog("info", "auto patrol started mode="+string(mode))
				}
			}
		}
	}()
}

// maybeAutoBackfill pulls today's CPAMP xAI summary once per day when configured.
// Prefers analytics summary; if thin, falls back to usage.sqlite per-account day sum.
func (r *Runtime) maybeAutoBackfill(cfg sentrycfg.Config, st *state.Store) {
	if st == nil || !cfg.CPAMPUsageFloor {
		return
	}
	loc, _ := time.LoadLocation("Asia/Shanghai")
	if loc == nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	day := time.Now().In(loc).Format("2006-01-02")
	m := st.MetricsSnapshot()
	// re-run floor if missing or suspiciously thin (e.g. analytics returned 3 calls for 500 pool)
	need := m.DayKey != day || m.BackfillSource == "" || m.BackfillAt == "" || m.CallsFloor < 50
	if !need {
		return
	}
	var tokens, calls, success, failure int64
	src := ""
	if strings.TrimSpace(cfg.CPAMPURL) != "" && strings.TrimSpace(cfg.CPAMPAdminKey) != "" {
		cli := cpamp.New(cfg.CPAMPURL, cfg.CPAMPAdminKey)
		from, to := cpamp.DayRangeShanghai(time.Now())
		sum, err := cli.FetchXAISummary(context.Background(), from, to)
		if err != nil {
			hostLog("warn", "cpamp auto-backfill analytics: "+err.Error())
		} else {
			tokens, calls, success, failure = sum.TotalTokens, sum.TotalCalls, sum.SuccessCalls, sum.FailureCalls
			src = "cpamp_auto"
		}
	}
	// sqlite day sum is authoritative for local volume; use when larger / analytics empty
	if days, path, err := cpamp.FetchXAIAccountDay(context.Background()); err != nil {
		hostLog("warn", "cpamp auto-backfill sqlite: "+err.Error())
	} else if path != "" && len(days) > 0 {
		var tTok, tCalls, tOK, tFail int64
		for _, a := range days {
			tTok += a.Tokens
			tCalls += a.Calls
			tOK += a.Success
			tFail += a.Failure
		}
		if tCalls > calls || tokens == 0 {
			tokens, calls, success, failure = tTok, tCalls, tOK, tFail
			src = "cpamp_sqlite_day"
		}
	}
	if src == "" || calls <= 0 {
		return
	}
	st.ApplyCPAMPBackfill(day, tokens, calls, success, failure, src)
	st.Log(state.ActionLog{Source: "cpamp", Action: "auto_backfill", Reason: "今日用量自动回补 · " + src})
	_ = st.Save()
}

func (r *Runtime) HandleUsage(ev guard.UsageEvent) {
	r.mu.Lock()
	g := r.Guard
	r.mu.Unlock()
	if g == nil {
		return
	}
	// observability: every usage event (esp. failover intermediate failures)
	if !ev.Success || ev.StatusCode >= 400 {
		body := strings.TrimSpace(ev.Body)
		if len(body) > 80 {
			body = body[:80]
		}
		hostLog("info", fmt.Sprintf("usage_in source=%s auth=%s file=%s status=%d success=%v body=%q",
			ev.Source, ev.AuthIndex, ev.FileName, ev.StatusCode, ev.Success, body))
	}
	if err := g.HandleUsage(context.Background(), ev); err != nil {
		hostLog("error", "usage: "+err.Error())
	}
}

// maybeBackfillCPAMPFailures replays recent xAI failures from usage.sqlite into HandleUsage.
// Closes the gap where host usage plugins miss failover intermediate 401/402/403/429 legs
// that CPAMP still stores. Watermarked by Metrics.LastCPAMPFailMS.
//
// Must run BEFORE Guard.Tick/heal in the same cycle: pre-marks auths so heal skips them.
func (r *Runtime) maybeBackfillCPAMPFailures(cfg sentrycfg.Config, st *state.Store, g *guard.Guard) {
	if st == nil || g == nil || !cfg.Enabled || !cfg.SentryEnabled {
		return
	}
	m := st.MetricsSnapshot()
	since := m.LastCPAMPFailMS
	// first run: only last 2 minutes so we don't mass-replay history
	if since <= 0 {
		since = time.Now().Add(-2 * time.Minute).UnixMilli()
	}
	events, path, err := cpamp.FetchRecentFailures(context.Background(), since, 80)
	if err != nil {
		hostLog("warn", "cpamp fail backfill: "+err.Error())
		return
	}
	if path == "" || len(events) == 0 {
		return
	}
	// Pre-mark auths that will be applied so Tick heal skips force-open this cycle.
	pending := make([]string, 0, len(events))
	for _, e := range events {
		auth := strings.TrimSpace(e.AuthIndex)
		if auth == "" {
			continue
		}
		if alreadyHandledRecently(st, auth, e.Status, e.TimestampMS) {
			continue
		}
		pending = append(pending, auth)
	}
	if len(pending) > 0 {
		g.MarkPendingBackfillAuths(pending)
	}
	var maxMS int64 = since
	applied := 0
	for _, e := range events {
		if e.TimestampMS > maxMS {
			maxMS = e.TimestampMS
		}
		auth := strings.TrimSpace(e.AuthIndex)
		if auth == "" {
			continue
		}
		// skip if we already have a very recent matching action for this auth+signal window
		// (avoid double-applying when plugin path already worked)
		if alreadyHandledRecently(st, auth, e.Status, e.TimestampMS) {
			continue
		}
		ev := guard.UsageEvent{
			Provider:   "xai",
			AuthIndex:  auth,
			FileName:   e.File,
			Email:      e.Account,
			StatusCode: e.Status,
			Body:       e.Body,
			Success:    false,
			Source:     "cpamp_backfill",
			Model:      strings.TrimSpace(e.Model),
		}
		if ev.Model == "" {
			ev.Model = cpamp.ModelFromFailBody(e.Body)
		}
		if err := g.HandleUsage(context.Background(), ev); err != nil {
			hostLog("warn", "cpamp fail backfill apply: "+err.Error())
			continue
		}
		applied++
	}
	if maxMS > since {
		st.SetLastCPAMPFailMS(maxMS)
		_ = st.Save()
	}
	if applied > 0 {
		hostLog("info", fmt.Sprintf("cpamp fail backfill applied=%d since_ms=%d path=%s", applied, since, path))
	}
}

// alreadyHandledRecently: if last action for auth is cooldown/disable within ~2m of event, skip.
func alreadyHandledRecently(st *state.Store, auth string, status int, eventMS int64) bool {
	if st == nil || auth == "" {
		return false
	}
	acc := st.Get(auth)
	if acc == nil {
		return false
	}
	// if permanent already, no need to re-apply cool from backfill
	if acc.State == state.UserManual || acc.DisableSource == "user_manual" {
		return true
	}
	// if currently in cool with matching signal / any cool ownership, skip
	if (acc.State == state.CooldownQuota || acc.State == state.CooldownSpending || acc.State == state.CooldownPermission || acc.State == state.CandidateDead) &&
		acc.DisableSource == "plugin_auto" {
		return true
	}
	if acc.LastActionAt.IsZero() {
		return false
	}
	// map status to expected signal family
	wantCool := status == 429 || status == 402 || status == 403 || status == 401
	if !wantCool {
		return false
	}
	// if we already cooled / disabled around this event time (±3m), skip
	evT := time.UnixMilli(eventMS)
	delta := acc.LastActionAt.Sub(evT)
	if delta < 0 {
		delta = -delta
	}
	if delta > 3*time.Minute {
		return false
	}
	switch acc.LastAction {
	case "cooldown", "manual_disable", "candidate", "cooldown_file_still_open", "cooldown_failed":
		return true
	}
	return false
}

func handleUsageEvent(request []byte) ([]byte, error) {
	if len(request) == 0 {
		return okEnvelopeJSON("{}")
	}
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(request, &record); err == nil {
		ev := usageEventFromRecord(record)
		if fill := usageEventFromRaw(request); fill.AuthIndex != "" || fill.Body != "" || fill.Model != "" {
			if ev.AuthIndex == "" {
				ev.AuthIndex = fill.AuthIndex
			}
			if ev.Provider == "" {
				ev.Provider = fill.Provider
			}
			if ev.Body == "" {
				ev.Body = fill.Body
				ev.StatusCode = fill.StatusCode
				ev.Success = fill.Success
			}
			if ev.FileName == "" {
				ev.FileName = fill.FileName
			}
			if ev.Email == "" {
				ev.Email = fill.Email
			}
			if ev.Model == "" {
				ev.Model = fill.Model
			}
		}
		if ev.Model == "" {
			ev.Model = cpamp.ModelFromFailBody(ev.Body)
		}
		// also map Fail field aliases
		if ev.StatusCode == 0 && record.Failed {
			ev.StatusCode = record.Failure.StatusCode
			if ev.Body == "" {
				ev.Body = record.Failure.Body
			}
			ev.Success = false
		}
		runtimeInstance().HandleUsage(ev)
		return okEnvelopeJSON("{}")
	}
	if ev, ok := usageEventFromRawOK(request); ok {
		runtimeInstance().HandleUsage(ev)
		return okEnvelopeJSON("{}")
	}
	hostLog("warn", "usage decode failed: "+truncateForLog(string(request), 200))
	return errorEnvelope("decode_usage", "invalid usage payload"), nil
}

func truncateForLog(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func usageEventFromRecord(r pluginapi.UsageRecord) guard.UsageEvent {
	body := ""
	status := 0
	success := !r.Failed
	if r.Failed {
		body = r.Failure.Body
		status = r.Failure.StatusCode
	}
	authIndex := firstNonEmptyStr(r.AuthIndex, r.AuthID)
	// AuthID is often the relative auth file name; AuthIndex may be opaque host id.
	fileName := firstNonEmptyStr(r.AuthID, r.AuthIndex)
	model := strings.TrimSpace(r.Model)
	if model == "" {
		model = cpamp.ModelFromFailBody(body)
	}
	return guard.UsageEvent{
		Provider:   r.Provider,
		AuthIndex:  authIndex,
		FileName:   fileName,
		Email:      "",
		StatusCode: status,
		Body:       body,
		Success:    success,
		Source:     "usage",
		Model:      model,
	}
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func usageEventFromRawOK(request []byte) (guard.UsageEvent, bool) {
	ev := usageEventFromRaw(request)
	if ev.AuthIndex == "" && ev.Body == "" && ev.StatusCode == 0 {
		return guard.UsageEvent{}, false
	}
	return ev, true
}

func usageEventFromRaw(request []byte) guard.UsageEvent {
	var raw map[string]any
	if err := json.Unmarshal(request, &raw); err != nil || raw == nil {
		return guard.UsageEvent{}
	}
	getS := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := raw[k]; ok {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
		return ""
	}
	getB := func(keys ...string) bool {
		for _, k := range keys {
			if v, ok := raw[k]; ok {
				switch t := v.(type) {
				case bool:
					return t
				case float64:
					return t != 0
				}
			}
		}
		return false
	}
	getI := func(m map[string]any, keys ...string) int {
		if m == nil {
			return 0
		}
		for _, k := range keys {
			if v, ok := m[k]; ok {
				switch t := v.(type) {
				case float64:
					return int(t)
				case int:
					return t
				case json.Number:
					n, _ := t.Int64()
					return int(n)
				}
			}
		}
		return 0
	}
	failed := getB("Failed", "failed")
	status := 0
	body := ""
	for _, key := range []string{"Failure", "failure"} {
		if f, ok := raw[key].(map[string]any); ok {
			if status == 0 {
				status = getI(f, "StatusCode", "status_code", "statusCode")
			}
			if body == "" {
				if s, ok := f["Body"].(string); ok {
					body = s
				} else if s, ok := f["body"].(string); ok {
					body = s
				}
			}
		}
	}
	auth := getS("AuthIndex", "auth_index", "authIndex", "AuthID", "auth_id", "authId")
	// Prefer AuthID / FileName (often real file) over opaque AuthIndex hash.
	file := getS("FileName", "file_name", "AuthID", "auth_id", "authId", "name", "AuthIndex", "auth_index")
	model := getS("Model", "model", "RequestedModel", "requested_model", "ResolvedModel", "resolved_model")
	if model == "" {
		model = cpamp.ModelFromFailBody(body)
	}
	return guard.UsageEvent{
		Provider:   getS("Provider", "provider"),
		AuthIndex:  auth,
		FileName:   file,
		Email:      getS("Email", "email", "Account", "account"),
		StatusCode: status,
		Body:       body,
		Success:    !failed && status < 400,
		Source:     "usage",
		Note:       getS("Note", "note", "Label", "label"),
		Label:      getS("Label", "label"),
		Model:      model,
	}
}

// Ensure fmt used if needed for future logs.
var _ = fmt.Sprintf

func (r *Runtime) startRegisterTicker() {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		// health + auto register loop
		for {
			r.mu.Lock()
			cfg := r.Cfg
			reg := r.Register
			r.mu.Unlock()
			sec := cfg.RegisterHealthIntervalSec
			if sec <= 0 {
				sec = 300
			}
			// also tick auto more frequently when interval smaller
			if cfg.RegisterAutoEnabled && cfg.RegisterAutoIntervalSec > 0 && cfg.RegisterAutoIntervalSec < sec {
				sec = cfg.RegisterAutoIntervalSec
			}
			if cfg.RegisterFloorEnabled && cfg.RegisterFloorIntervalSec > 0 && cfg.RegisterFloorIntervalSec < sec {
				sec = cfg.RegisterFloorIntervalSec
			}
			if sec < 30 {
				sec = 30
			}
			select {
			case <-r.stopCh:
				return
			case <-time.After(time.Duration(sec) * time.Second):
			}
			if reg == nil || !cfg.RegisterEnabled {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			_ = reg.Health(ctx, false)
			reg.MaybeAutoRegister(ctx)
			cancel()
		}
	}()
}

