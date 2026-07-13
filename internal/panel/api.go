package panel

import (
	"encoding/json"
	"html"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/cpamp"
	"github.com/openclaw-local/cpa-xai-sentry/internal/errorsig"
	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/patrol"
	"github.com/openclaw-local/cpa-xai-sentry/internal/quota"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

var (
	invMu    sync.Mutex
	invCache map[string]any
	invAt    time.Time
)

type API struct {
	Cfg    *sentrycfg.Config
	State  *state.Store
	Trash  *trash.Store
	Guard  *guard.Guard
	Patrol *patrol.Runner
	// optional hooks wired by main runtime for durable panel toggles
	PersistConfig func(c sentrycfg.Config) error
	GetConfig     func() sentrycfg.Config
	SetConfig     func(c sentrycfg.Config)
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/state", a.handleState)
	mux.HandleFunc("/config", a.handleConfig)
	mux.HandleFunc("/logs", a.handleLogs)
	mux.HandleFunc("/candidates", a.handleCandidates)
	mux.HandleFunc("/trash", a.handleTrash)
	mux.HandleFunc("/trash/restore", a.handleTrashRestore)
	mux.HandleFunc("/trash/purge", a.handleTrashPurge)
	mux.HandleFunc("/run-tick", a.handleRunTick)
	mux.HandleFunc("/toggle", a.handleToggle)
	mux.HandleFunc("/preset", a.handlePreset)
	mux.HandleFunc("/health", a.handleHealth)
	mux.HandleFunc("/errors", a.handleErrors)
	mux.HandleFunc("/errors/policy", a.handleErrorPolicy)
	mux.HandleFunc("/backfill", a.handleBackfill)
	mux.HandleFunc("/metrics", a.handleMetrics)
	mux.HandleFunc("/accounts/bulk", a.handleAccountsBulk)
	mux.HandleFunc("/accounts/cooldown-suggested", a.handleCooldownSuggested)
	mux.HandleFunc("/ui", a.handleUI)
	mux.HandleFunc("/", a.handleUI)
	return mux
}

func (a *API) persistSwitches() {
	if a.Cfg == nil {
		return
	}
	if a.SetConfig != nil {
		a.SetConfig(*a.Cfg)
	}
	if a.PersistConfig != nil {
		_ = a.PersistConfig(*a.Cfg)
	}
	if a.Guard != nil {
		a.Guard.Cfg = *a.Cfg
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}


func suggestAction(acc *state.Account) (action, reason string) {
	if acc == nil {
		return "none", ""
	}
	switch acc.State {
	case state.CandidateDead:
		return "trash", "候选死号"
	case state.CooldownQuota:
		return "wait", "额度冷却中"
	case state.CooldownSpending:
		return "wait", "消费限额冷却中"
	case state.CooldownPermission:
		return "review", "权限冷却中"
	case state.UserManual:
		return "manual", "手动禁用"
	case state.Trashed:
		return "restore_or_purge", "垃圾箱"
	}
	switch acc.LastSignal {
	case "auth_401":
		return "candidate", "凭证失效"
	case "permission_403":
		return "cooldown", "权限拒绝"
	case "free_usage_429":
		return "cooldown", "免费额度用尽"
	case "spending_limit_402":
		return "cooldown", "消费限额"
	}
	if acc.State == state.Active && acc.LastSignal == "" {
		return "none", ""
	}
	return "observe", ""
}

// inventoryFromCPA counts xAI auth files enabled/disabled (cached ~20s).
func (a *API) inventoryFromCPA(r *http.Request) map[string]any {
	invMu.Lock()
	defer invMu.Unlock()
	if invCache != nil && time.Since(invAt) < 20*time.Second {
		return invCache
	}
	out := map[string]any{
		"auth_total": 0, "auth_enabled": 0, "auth_disabled": 0, "auth_xai": 0, "source": "none",
	}
	if a.Guard == nil || a.Guard.CPA == nil {
		invCache, invAt = out, time.Now()
		return out
	}
	files, err := a.Guard.CPA.ListAuthFiles(r.Context())
	if err != nil || len(files) == 0 {
		// fallback: scan auth_dir via resolver cache if present
		if a.Guard.Resolver != nil {
			_ = a.Guard.Resolver.Ensure(r.Context())
		}
		out["source"] = "error"
		if err != nil {
			out["error"] = err.Error()
		}
		invCache, invAt = out, time.Now()
		return out
	}
	total, en, dis, xai := 0, 0, 0, 0
	for _, f := range files {
		name := strings.ToLower(f.Name + " " + f.Provider + " " + f.Type)
		isXAI := strings.Contains(name, "xai") || strings.Contains(name, "grok") || strings.HasPrefix(strings.ToLower(f.Name), "xai-")
		if !isXAI {
			continue
		}
		xai++
		total++
		if f.Disabled {
			dis++
		} else {
			en++
		}
	}
	out["auth_total"] = total
	out["auth_xai"] = xai
	out["auth_enabled"] = en
	out["auth_disabled"] = dis
	out["source"] = "cpa_auth_files"
	invCache, invAt = out, time.Now()
	return out
}

func (a *API) handleState(w http.ResponseWriter, r *http.Request) {
	view := r.URL.Query().Get("view")
	if view == "" {
		view = "focus"
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	stateFilter := strings.TrimSpace(r.URL.Query().Get("state"))
	signalFilter := strings.TrimSpace(r.URL.Query().Get("signal"))
	actionFilter := strings.TrimSpace(r.URL.Query().Get("action"))
	// Best-effort resolve identities for display.
	if a.Guard != nil && a.Guard.Resolver != nil {
		_ = a.Guard.Resolver.Ensure(r.Context())
		for _, acc := range a.State.AccountsSnapshot() {
			if id, ok := a.Guard.Resolver.Resolve(acc.AuthIndex, acc.FileName, acc.Email); ok {
				a.State.UpdateMeta(acc.AuthIndex, id.FileName, id.Email, "")
			}
		}
	}
	accs := a.State.AccountsSnapshot()
	type row struct {
		AuthIndex       string         `json:"auth_index"`
		FileName        string         `json:"file_name"`
		Email           string         `json:"email"`
		DisplayName     string         `json:"display_name"`
		Tier            string         `json:"tier"`
		State           string         `json:"state"`
		Signal          string         `json:"last_signal"`
		DisableSource   string         `json:"disable_source"`
		Streaks         map[string]int `json:"streaks,omitempty"`
		StreakSummary   string         `json:"streak_summary"`
		SuggestedAction string         `json:"suggested_action"`
		Reason          string         `json:"reason"`
		RecoverAt       any            `json:"recover_at,omitempty"`
		UpdatedAt       any            `json:"updated_at,omitempty"`
		QuotaLimit      int64          `json:"quota_limit,omitempty"`
		QuotaUsed       int64          `json:"quota_used,omitempty"`
		QuotaRemaining  int64          `json:"quota_remaining,omitempty"`
		QuotaSource     string         `json:"quota_source,omitempty"`
		DayCalls        int64          `json:"day_calls,omitempty"`
		DayFailCalls    int64          `json:"day_fail_calls,omitempty"`
		DayTokens       int64          `json:"day_tokens,omitempty"`
		QuotaText       string         `json:"quota_text,omitempty"`
	}
	summary := map[string]int{
		"total": 0, "active": 0, "cooldown": 0, "candidate": 0,
		"user_manual": 0, "trashed": 0, "with_signal": 0,
		"suggest_cooldown": 0, "suggest_candidate": 0, "suggest_trash": 0,
		"suggest_review": 0, "suggest_wait": 0,
	}
	signalCounts := map[string]int{}
	var dayCalls, dayFails, dayTokens int64
	rows := make([]row, 0, len(accs))
	for _, acc := range accs {
		summary["total"]++
		dayCalls += acc.DayCalls
		dayFails += acc.DayFailCalls
		dayTokens += acc.DayTokens
		switch acc.State {
		case state.Active:
			summary["active"]++
		case state.CooldownQuota, state.CooldownSpending, state.CooldownPermission:
			summary["cooldown"]++
		case state.CandidateDead:
			summary["candidate"]++
		case state.UserManual:
			summary["user_manual"]++
		case state.Trashed, state.Purged:
			summary["trashed"]++
		}
		if acc.LastSignal != "" {
			summary["with_signal"]++
			signalCounts[acc.LastSignal]++
		}
		act, reason := suggestAction(acc)
		switch act {
		case "cooldown":
			summary["suggest_cooldown"]++
		case "candidate":
			summary["suggest_candidate"]++
		case "trash":
			summary["suggest_trash"]++
		case "review":
			summary["suggest_review"]++
		case "wait":
			summary["suggest_wait"]++
		}
		if view == "focus" {
			if acc.State == state.Active && acc.LastSignal == "" {
				continue
			}
		}
		if stateFilter != "" && string(acc.State) != stateFilter {
			continue
		}
		if signalFilter != "" && acc.LastSignal != signalFilter {
			continue
		}
		if actionFilter != "" && act != actionFilter {
			continue
		}
		display := cpaapi.DisplayName(acc.Email, acc.FileName, acc.AuthIndex)
		if q != "" {
			blob := strings.ToLower(display + " " + acc.Email + " " + acc.FileName + " " + acc.AuthIndex + " " + acc.Tier + " " + acc.LastSignal + " " + act + " " + reason)
			if !strings.Contains(blob, q) {
				continue
			}
		}
		var ra, ua any
		if !acc.RecoverAt.IsZero() {
			ra = acc.RecoverAt.In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04")
		}
		if !acc.UpdatedAt.IsZero() {
			ua = acc.UpdatedAt.In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04:05")
		}
		streakSum := ""
		if len(acc.Streaks) > 0 {
			parts := make([]string, 0, len(acc.Streaks))
			for k, v := range acc.Streaks {
				if v > 0 {
					parts = append(parts, k+":"+itoa(v))
				}
			}
			streakSum = strings.Join(parts, " ")
		}
		qLimit, qUsed, qRem := acc.QuotaLimit, acc.QuotaUsed, acc.QuotaRemaining
		qSrc := acc.QuotaSource
		// free-usage exhausted often arrives without numeric fields — show 2M free-tier estimate
		if (qLimit == 0 && qUsed == 0 && qRem == 0) &&
			(acc.LastSignal == "free_usage_429" || qSrc == "free_usage_exhausted" ||
				acc.State == state.CooldownQuota) {
			qLimit = quota.FreeQuotaPerAccount
			qUsed = quota.FreeQuotaPerAccount
			qRem = 0
			if qSrc == "" {
				qSrc = "free_usage_exhausted"
			}
		}
		qText := ""
		if qLimit > 0 || qUsed > 0 || qRem > 0 {
			qText = formatTokens(qUsed) + " / " + formatTokens(qLimit) + " 剩 " + formatTokens(qRem)
			if qSrc == "free_usage_exhausted" {
				qText += " · 免费额用尽"
			} else if qSrc != "" && qSrc != "body_field" {
				qText += " · " + qSrc
			}
		} else if acc.DayTokens > 0 {
			qText = "今日 token " + formatTokens(acc.DayTokens)
		} else if acc.DayCalls > 0 {
			qText = "今日调用 " + itoa64(acc.DayCalls)
			if acc.DayFailCalls > 0 {
				qText += " · 失败 " + itoa64(acc.DayFailCalls)
			}
		}
		rows = append(rows, row{
			AuthIndex: acc.AuthIndex, FileName: acc.FileName, Email: acc.Email, DisplayName: display,
			Tier: acc.Tier, State: string(acc.State), Signal: acc.LastSignal,
			DisableSource: acc.DisableSource, Streaks: acc.Streaks, StreakSummary: streakSum,
			SuggestedAction: act, Reason: reason, RecoverAt: ra, UpdatedAt: ua,
			QuotaLimit: qLimit, QuotaUsed: qUsed, QuotaRemaining: qRem,
			QuotaSource: qSrc, DayCalls: acc.DayCalls, DayFailCalls: acc.DayFailCalls,
			DayTokens: acc.DayTokens, QuotaText: qText,
		})
	}
	m := a.State.MetricsSnapshot()
	cool := a.State.CooldownStats(time.Now())
	// enrich summary with cooldown capacity
	if v, ok := cool["cooling"].(int); ok {
		summary["cooldown"] = v
	}
	if v, ok := cool["pending_suggest"].(int); ok {
		summary["pending_suggest"] = v
	}
	inv := a.inventoryFromCPA(r)
	enabledN := asInt(inv["auth_enabled"])
	poolEst := int64(enabledN) * quota.FreeQuotaPerAccount
	// used: real usage tokens preferred, else CPAMP floor (same spirit as quota-guard status bar)
	usedTok := dayTokens
	if usedTok == 0 {
		usedTok = m.TokensFloor
	}
	remainTok := poolEst - usedTok
	if remainTok < 0 {
		remainTok = 0
	}
	pct := 0.0
	if poolEst > 0 {
		pct = float64(usedTok) / float64(poolEst) * 100
		if pct > 100 {
			pct = 100
		}
	}
	usage := map[string]any{
		"day_calls":      dayCalls,
		"day_fail_calls": dayFails,
		"day_tokens":     dayTokens,
		"cpamp_tokens":   m.TokensFloor,
		"cpamp_calls":    m.CallsFloor,
		"cpamp_day":      m.DayKey,
		// quota-guard style daily pool estimate
		"pool_est":          poolEst,
		"pool_per_account":  quota.FreeQuotaPerAccount,
		"pool_enabled":      enabledN,
		"pool_used":         usedTok,
		"pool_remaining":    remainTok,
		"pool_used_pct":     pct,
		"pool_source":       "enabled×2M (rolling free-tier est)",
	}
	writeJSON(w, 200, map[string]any{
		"plugin":         "cpa-xai-sentry",
		"version":        "0.3.9",
		"mode":           modeOf(*a.Cfg),
		"mode_label":     modeLabel(modeOf(*a.Cfg)),
		"summary":        summary,
		"inventory":      inv,
		"usage":          usage,
		"signal_counts":  signalCounts,
		"cooldown_stats": cool,
		"accounts":       rows,
		"account_count":  len(rows),
		"trash_count":    len(a.State.ListTrash()),
		"error_count":    len(a.State.ListObserved()),
		"policy_count":   len(a.State.ListErrorPolicies()),
		"metrics":        m,
		"config":         a.Cfg.Redact(),
		"updated_at":     time.Now().In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04:05"),
	})
}

func asInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	default:
		return 0
	}
}

// formatTokens renders large token counts compactly (1.0M / 549k / 120).
func formatTokens(n int64) string {
	if n < 0 {
		n = -n
	}
	if n >= 1_000_000 {
		// one decimal for millions
		whole := n / 1_000_000
		frac := (n % 1_000_000) / 100_000
		if frac == 0 {
			return itoa64(whole) + "M"
		}
		return itoa64(whole) + "." + itoa64(frac) + "M"
	}
	if n >= 10_000 {
		return itoa64(n/1000) + "k"
	}
	return itoa64(n)
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}


func modeOf(c sentrycfg.Config) string {
	if !c.SentryEnabled {
		return "off"
	}
	if !c.AutoCooldown && !c.AutoCandidate && !c.AutoDelete {
		return "observe"
	}
	if c.AutoDelete {
		return "auto_trash"
	}
	if c.AutoCandidate {
		return "safe-guard"
	}
	return "cooldown"
}

func modeLabel(mode string) string {
	switch mode {
	case "off":
		return "已关闭"
	case "observe":
		return "仅观察"
	case "safe-guard":
		return "安全防护（自动冷却+候选，不自动进垃圾箱）"
	case "cooldown":
		return "自动冷却"
	case "auto_trash":
		return "自动垃圾箱"
	default:
		return mode
	}
}

func (a *API) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, a.Cfg.Redact())
	case http.MethodPost:
		var in sentrycfg.Config
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		// preserve secrets when client omits them (redacted GET round-trip)
		if in.ManagementKey == "" && a.Cfg != nil {
			in.ManagementKey = a.Cfg.ManagementKey
		}
		if in.CPAMPAdminKey == "" && a.Cfg != nil {
			in.CPAMPAdminKey = a.Cfg.CPAMPAdminKey
		}
		in = in.Validate()
		*a.Cfg = in
		a.persistSwitches()
		writeJSON(w, 200, a.Cfg.Redact())
	default:
		w.WriteHeader(405)
	}
}

func (a *API) handleLogs(w http.ResponseWriter, r *http.Request) {
	type L struct {
		At          string `json:"at"`
		Auth        string `json:"auth"`
		AuthLabel   string `json:"auth_label"`
		Source      string `json:"source"`
		Signal      string `json:"signal"`
		SignalLabel string `json:"signal_label"`
		Action      string `json:"action"`
		ActionLabel string `json:"action_label"`
		Reason      string `json:"reason"`
		Text        string `json:"text"`
	}
	out := make([]L, 0)
	for _, e := range a.State.SnapshotLogs() {
		label := e.Auth
		if acc := a.State.Get(e.Auth); acc != nil {
			label = cpaapi.DisplayName(acc.Email, acc.FileName, acc.AuthIndex)
		}
		actL := logActionZH(e.Action)
		sigL := logSignalZH(e.Signal)
		reason := e.Reason
		if reason == "free_usage" || reason == "bulk_suggested_cooldown" {
			reason = "免费额度用尽"
		}
		if reason == "recover_at" {
			reason = "到期恢复"
		}
		text := actL
		if sigL != "" {
			text += " · " + sigL
		}
		if label != "" {
			text += " · " + label
		}
		if reason != "" && reason != sigL && reason != e.Action {
			text += " · " + reason
		}
		out = append(out, L{
			At: e.At.In(time.FixedZone("CST", 8*3600)).Format("15:04:05"),
			Auth: e.Auth, AuthLabel: label, Source: e.Source,
			Signal: e.Signal, SignalLabel: sigL,
			Action: e.Action, ActionLabel: actL, Reason: reason, Text: text,
		})
	}
	writeJSON(w, 200, map[string]any{"logs": out})
}

func logActionZH(a string) string {
	switch a {
	case "cooldown":
		return "冷却"
	case "cooldown_failed":
		return "冷却失败"
	case "reenable":
		return "恢复启用"
	case "reenable_failed":
		return "恢复失败"
	case "candidate":
		return "进候选"
	case "manual_disable":
		return "手动禁用"
	case "manual_enable":
		return "手动启用"
	case "backfill":
		return "用量回补"
	case "trash", "delete":
		return "进垃圾箱"
	default:
		if a == "" {
			return "—"
		}
		return a
	}
}

func logSignalZH(s string) string {
	switch s {
	case "free_usage_429":
		return "免费额度用尽"
	case "spending_limit_402":
		return "消费限额"
	case "auth_401":
		return "凭证失效"
	case "permission_403":
		return "权限拒绝"
	default:
		return s
	}
}

func (a *API) handleCandidates(w http.ResponseWriter, r *http.Request) {
	var out []any
	for _, acc := range a.State.AccountsSnapshot() {
		if acc.State == state.CandidateDead {
			out = append(out, map[string]string{
				"auth_index": acc.AuthIndex, "file_name": acc.FileName,
				"email": acc.Email, "signal": acc.LastSignal,
			})
		}
	}
	writeJSON(w, 200, map[string]any{"candidates": out})
}

func (a *API) handleTrash(w http.ResponseWriter, r *http.Request) {
	// metadata only
	writeJSON(w, 200, map[string]any{"trash": a.Trash.ListMeta()})
}

func (a *API) handleTrashRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var in struct {
		ID     string `json:"id"`
		Enable bool   `json:"enable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ID == "" {
		writeJSON(w, 400, map[string]string{"error": "id required"})
		return
	}
	err := a.Trash.Restore(in.ID, in.Enable, func(fileName string, raw []byte) error {
		// caller/host wires CPA write; for unit tests inject via Guard.CPA if present
		if a.Guard != nil && a.Guard.CPA != nil {
			return a.Guard.CPA.WriteAuthFileToDir(fileName, raw)
		}
		return nil
	})
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *API) handleTrashPurge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var in struct {
		ID      string `json:"id"`
		Confirm bool   `json:"confirm"`
		Expired bool   `json:"expired"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if !in.Confirm {
		writeJSON(w, 400, map[string]string{"error": "confirm=true required"})
		return
	}
	if in.Expired {
		n, err := a.Trash.PurgeExpired(time.Now())
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"purged": n})
		return
	}
	if strings.TrimSpace(in.ID) == "" {
		writeJSON(w, 400, map[string]string{"error": "id required"})
		return
	}
	if err := a.Trash.Purge(in.ID); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *API) handleRunTick(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	if err := a.Guard.Tick(r.Context()); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}


func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ok": true, "plugin": "cpa-xai-sentry", "version": "0.3.9",
		"mode": modeOf(*a.Cfg), "mode_label": modeLabel(modeOf(*a.Cfg)), "config": a.Cfg.Redact(),
		"cooldown_stats": a.State.CooldownStats(time.Now()),
	})
}

func (a *API) handleToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var in struct {
		SentryEnabled *bool `json:"sentry_enabled"`
		AutoCooldown   *bool `json:"auto_cooldown"`
		AutoCandidate *bool `json:"auto_candidate"`
		AutoDelete    *bool `json:"auto_delete"`
		PatrolEnabled *bool `json:"patrol_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if in.SentryEnabled != nil {
		a.Cfg.SentryEnabled = *in.SentryEnabled
	}
	if in.AutoCooldown != nil {
		a.Cfg.AutoCooldown = *in.AutoCooldown
	}
	if in.AutoCandidate != nil {
		a.Cfg.AutoCandidate = *in.AutoCandidate
	}
	if in.AutoDelete != nil {
		a.Cfg.AutoDelete = *in.AutoDelete
	}
	if in.PatrolEnabled != nil {
		a.Cfg.PatrolEnabled = *in.PatrolEnabled
	}
	*a.Cfg = a.Cfg.Validate()
	a.persistSwitches()
	writeJSON(w, 200, map[string]any{"ok": true, "mode": modeOf(*a.Cfg), "mode_label": modeLabel(modeOf(*a.Cfg)), "config": a.Cfg.Redact()})
}

func (a *API) handlePreset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	switch strings.ToLower(strings.TrimSpace(in.Name)) {
	case "observe", "observe-only":
		a.Cfg.SentryEnabled = true
		a.Cfg.AutoCooldown = false
		a.Cfg.AutoCandidate = false
		a.Cfg.AutoDelete = false
		a.Cfg.PatrolEnabled = false
	case "safe-guard", "safeguard":
		a.Cfg.SentryEnabled = true
		a.Cfg.AutoCooldown = true
		a.Cfg.AutoCandidate = true
		a.Cfg.AutoDelete = false
		a.Cfg.CandidateSignals = []string{"auth_401"}
		a.Cfg.DeleteSignals = []string{}
		a.Cfg.PatrolEnabled = true
	case "off":
		a.Cfg.SentryEnabled = false
	default:
		writeJSON(w, 400, map[string]string{"error": "未知预设，可用：observe|safe-guard|off"})
		return
	}
	*a.Cfg = a.Cfg.Validate()
	a.persistSwitches()
	writeJSON(w, 200, map[string]any{"ok": true, "mode": modeOf(*a.Cfg), "mode_label": modeLabel(modeOf(*a.Cfg)), "config": a.Cfg.Redact()})
}

func (a *API) handleAccountsBulk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var in struct {
		Action  string   `json:"action"` // disable|enable|trash|cooldown
		Auths   []string `json:"auths"`
		Confirm bool     `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	in.Action = strings.ToLower(strings.TrimSpace(in.Action))
	if in.Action == "delete" {
		in.Action = "trash"
	}
	if in.Action == "" || len(in.Auths) == 0 {
		writeJSON(w, 400, map[string]string{"error": "action 与 auths 必填"})
		return
	}
	if (in.Action == "trash" || in.Action == "disable") && !in.Confirm {
		writeJSON(w, 400, map[string]string{"error": "危险操作需要 confirm=true"})
		return
	}
	if a.Guard == nil {
		writeJSON(w, 503, map[string]string{"error": "guard 未就绪"})
		return
	}
	okN, failN, errs := a.Guard.Bulk(r.Context(), in.Action, in.Auths)
	writeJSON(w, 200, map[string]any{"ok": true, "action": in.Action, "success": okN, "failed": failN, "errors": errs})
}

func (a *API) handleCooldownSuggested(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var in struct {
		Auths  []string `json:"auths"`
		Hours  int      `json:"hours"`
		Confirm bool    `json:"confirm"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if !in.Confirm {
		writeJSON(w, 400, map[string]string{"error": "confirm=true required"})
		return
	}
	if a.Guard == nil {
		writeJSON(w, 503, map[string]string{"error": "guard 未就绪"})
		return
	}
	n, err := a.Guard.ApplySuggestedCooldown(r.Context(), in.Auths, in.Hours)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "cooled": n})
}


func (a *API) handleErrors(w http.ResponseWriter, r *http.Request) {
	// ensure builtins
	if a.Guard != nil {
		// Guard.New already seeds; re-seed safe
		builtins := map[string]state.ErrorPolicy{}
		for k, p := range errorsig.BuiltinDefaults() {
			builtins[k] = state.ErrorPolicy{
				Key: p.Key, Label: p.Label, Enabled: p.Enabled, Action: string(p.Action),
				Threshold: p.Threshold, CooldownSec: p.CooldownSec, NeverTrash: p.NeverTrash,
				Note: p.Note, Source: p.Source,
			}
		}
		a.State.EnsureBuiltinPolicies(builtins)
	}
	obs := a.State.ListObserved()
	pols := a.State.ListErrorPolicies()
	// join: every observed + every policy
	type row struct {
		Key         string `json:"key"`
		Label       string `json:"label"`
		Enabled     bool   `json:"enabled"`
		Action      string `json:"action"`
		ActionLabel string `json:"action_label"`
		Threshold   int    `json:"threshold"`
		CooldownSec int    `json:"cooldown_seconds"`
		NeverTrash  bool   `json:"never_trash"`
		Note        string `json:"note"`
		Source      string `json:"source"`
		Count       int64  `json:"count"`
		LastAt      string `json:"last_at,omitempty"`
		Sample      string `json:"sample,omitempty"`
		SampleMsg   string `json:"sample_msg,omitempty"`
		SamplePretty string `json:"sample_pretty,omitempty"`
		StatusCode  int    `json:"status_code,omitempty"`
		Code        string `json:"code,omitempty"`
		LastAuth    string `json:"last_auth,omitempty"`
		LastFile    string `json:"last_file,omitempty"`
	}
	byKey := map[string]row{}
	for _, p := range pols {
		byKey[p.Key] = row{
			Key: p.Key, Label: p.Label, Enabled: p.Enabled, Action: p.Action,
			ActionLabel: actionLabel(p.Action), Threshold: p.Threshold, CooldownSec: p.CooldownSec,
			NeverTrash: p.NeverTrash, Note: p.Note, Source: p.Source,
		}
	}
	for _, o := range obs {
		r0, ok := byKey[o.Key]
		if !ok {
			r0 = row{Key: o.Key, Label: o.Label, Enabled: true, Action: "observe", ActionLabel: actionLabel("observe"), Threshold: 1, Source: "learned"}
		}
		if r0.Label == "" {
			r0.Label = o.Label
		}
		r0.Count = o.Count
		if !o.LastAt.IsZero() {
			r0.LastAt = o.LastAt.UTC().Format(time.RFC3339)
		}
		r0.Sample = html.UnescapeString(o.Sample)
		r0.StatusCode = o.StatusCode
		r0.Code = o.Code
		byKey[o.Key] = r0
	}
	out := make([]row, 0, len(byKey))
	for _, r0 := range byKey {
		out = append(out, r0)
	}
	writeJSON(w, 200, map[string]any{"errors": out, "count": len(out)})
}

func actionLabel(a string) string {
	switch a {
	case "observe":
		return "仅观察"
	case "cooldown":
		return "冷却"
	case "candidate":
		return "进候选"
	case "trash":
		return "进垃圾箱"
	default:
		return a
	}
}

func (a *API) handleErrorPolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var in state.ErrorPolicy
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	in.Key = strings.TrimSpace(in.Key)
	if in.Key == "" {
		writeJSON(w, 400, map[string]string{"error": "key 必填"})
		return
	}
	if in.Action == "" {
		in.Action = "observe"
	}
	if in.Threshold <= 0 {
		in.Threshold = 1
	}
	if errorsig.HardNeverTrash(in.Key) {
		in.NeverTrash = true
		if in.Action == "trash" {
			in.Action = "cooldown"
		}
	}
	if in.Label == "" {
		in.Label = in.Key
	}
	if in.Source == "" {
		in.Source = "user"
	}
	a.State.UpsertErrorPolicy(in)
	_ = a.State.Save()
	writeJSON(w, 200, map[string]any{"ok": true, "policy": in})
}

func (a *API) handleMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"metrics": a.State.MetricsSnapshot(), "cpamp_configured": a.Cfg.CPAMPURL != "" && a.Cfg.CPAMPAdminKey != ""})
}

func (a *API) handleBackfill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	if a.Cfg.CPAMPURL == "" || a.Cfg.CPAMPAdminKey == "" {
		writeJSON(w, 400, map[string]string{"error": "未配置 CPAMP 地址/密钥"})
		return
	}
	cli := cpamp.New(a.Cfg.CPAMPURL, a.Cfg.CPAMPAdminKey)
	from, to := cpamp.DayRangeShanghai(time.Now())
	sum, err := cli.FetchXAISummary(r.Context(), from, to)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "CPAMP 回补失败: " + err.Error()})
		return
	}
	// day key shanghai
	loc, _ := time.LoadLocation("Asia/Shanghai")
	if loc == nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	day := time.Now().In(loc).Format("2006-01-02")
	m := a.State.ApplyCPAMPBackfill(day, sum.TotalTokens, sum.TotalCalls, sum.SuccessCalls, sum.FailureCalls, "cpamp_analytics")
	_ = a.State.Save()
	a.State.Log(state.ActionLog{Source: "cpamp", Action: "backfill", Reason: "今日用量回补"})
	_ = a.State.Save()
	writeJSON(w, 200, map[string]any{
		"ok": true,
		"day": day,
		"cpamp": sum,
		"metrics": m,
		"help": "从 CPAMP 监控分析拉取今日 xAI 汇总，写入用量地板（只升不降），用于对照插件观察数据。",
	})
}


func (a *API) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(uiHTML))
}
