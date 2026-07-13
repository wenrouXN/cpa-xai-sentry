//go:build cshared

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/cpamp"
	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/panel"
	"github.com/openclaw-local/cpa-xai-sentry/internal/patrol"
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
	Patrol *patrol.Runner
	Panel  *panel.API

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
	cfg = cfg.Validate()
	if err := r.rebuild(cfg); err != nil {
		hostLog("error", "apply config failed: "+err.Error())
	}
}

func (r *Runtime) rebuild(cfg sentrycfg.Config) error {
	st, err := state.Load(cfg.StatePath)
	if err != nil {
		// start empty on corrupt
		hostLog("error", "load state: "+err.Error())
		st = state.New(cfg.StatePath)
	}
	tr := trash.New(cfg.TrashDir, cfg.TrashRetentionDays, cfg.TrashAutoPurge, st)
	cpa := cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, cfg.AuthDir)
	g := guard.New(cfg, st, tr, cpa)
	p := patrol.New(cfg, g, cpa)
	api := &panel.API{Cfg: &cfg, State: st, Trash: tr, Guard: g, Patrol: p}
	r.Cfg = cfg
	r.State = st
	r.Trash = tr
	r.CPA = cpa
	r.Guard = g
	r.Patrol = p
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
					if err := g.Tick(context.Background()); err != nil {
						hostLog("error", "tick: "+err.Error())
					}
				}
				r.maybeAutoBackfill(cfg, st)
			}
		}
	}()
}

// maybeAutoBackfill pulls today's CPAMP xAI summary once per day when configured.
func (r *Runtime) maybeAutoBackfill(cfg sentrycfg.Config, st *state.Store) {
	if st == nil || !cfg.CPAMPUsageFloor {
		return
	}
	if strings.TrimSpace(cfg.CPAMPURL) == "" || strings.TrimSpace(cfg.CPAMPAdminKey) == "" {
		return
	}
	loc, _ := time.LoadLocation("Asia/Shanghai")
	if loc == nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	day := time.Now().In(loc).Format("2006-01-02")
	m := st.MetricsSnapshot()
	if m.DayKey == day && m.BackfillSource != "" && m.BackfillAt != "" {
		// already backfilled today
		return
	}
	cli := cpamp.New(cfg.CPAMPURL, cfg.CPAMPAdminKey)
	from, to := cpamp.DayRangeShanghai(time.Now())
	sum, err := cli.FetchXAISummary(context.Background(), from, to)
	if err != nil {
		hostLog("warn", "cpamp auto-backfill skipped: "+err.Error())
		return
	}
	st.ApplyCPAMPBackfill(day, sum.TotalTokens, sum.TotalCalls, sum.SuccessCalls, sum.FailureCalls, "cpamp_auto")
	st.Log(state.ActionLog{Source: "cpamp", Action: "auto_backfill", Reason: "今日用量自动回补"})
	_ = st.Save()
}

func (r *Runtime) HandleUsage(ev guard.UsageEvent) {
	r.mu.Lock()
	g := r.Guard
	r.mu.Unlock()
	if g == nil {
		return
	}
	if err := g.HandleUsage(context.Background(), ev); err != nil {
		hostLog("error", "usage: "+err.Error())
	}
}

func handleUsageEvent(request []byte) ([]byte, error) {
	if len(request) == 0 {
		return okEnvelopeJSON("{}")
	}
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(request, &record); err == nil {
		ev := usageEventFromRecord(record)
		if fill := usageEventFromRaw(request); fill.AuthIndex != "" || fill.Body != "" {
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
		}
		runtimeInstance().HandleUsage(ev)
		return okEnvelopeJSON("{}")
	}
	if ev, ok := usageEventFromRawOK(request); ok {
		runtimeInstance().HandleUsage(ev)
		return okEnvelopeJSON("{}")
	}
	return errorEnvelope("decode_usage", "invalid usage payload"), nil
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
	return guard.UsageEvent{
		Provider:   r.Provider,
		AuthIndex:  authIndex,
		FileName:   fileName,
		Email:      "",
		StatusCode: status,
		Body:       body,
		Success:    success,
		Source:     "usage",
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
	}
}

// Ensure fmt used if needed for future logs.
var _ = fmt.Sprintf
