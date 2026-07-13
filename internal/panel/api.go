package panel

import (
	"encoding/json"
	"html"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/cpamp"
	"github.com/openclaw-local/cpa-xai-sentry/internal/errorsig"
	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/match"
	"github.com/openclaw-local/cpa-xai-sentry/internal/patrol"
	"github.com/openclaw-local/cpa-xai-sentry/internal/quota"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
	"github.com/openclaw-local/cpa-xai-sentry/internal/version"
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
	mux.HandleFunc("/patrol/start", a.handlePatrolStart)
	mux.HandleFunc("/patrol/status", a.handlePatrolStatus)
	mux.HandleFunc("/toggle", a.handleToggle)
	mux.HandleFunc("/preset", a.handlePreset)
	mux.HandleFunc("/health", a.handleHealth)
	mux.HandleFunc("/errors", a.handleErrors)
	mux.HandleFunc("/errors/policy", a.handleErrorPolicy)
	mux.HandleFunc("/errors/reclassify", a.handleErrorReclassify)
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
	if a.Patrol != nil {
		a.Patrol.Cfg = *a.Cfg
		a.Patrol.Guard = a.Guard
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}



// dedupeAccountsForPanel collapses hash-id + filename-id duplicates of the same xAI account.
func dedupeAccountsForPanel(accs []*state.Account) []*state.Account {
	if len(accs) <= 1 {
		return accs
	}
	best := map[string]*state.Account{}
	order := make([]string, 0, len(accs))
	keyOf := func(a *state.Account) string {
		if a == nil {
			return ""
		}
		if e := strings.ToLower(strings.TrimSpace(a.Email)); e != "" {
			return "e:" + e
		}
		if f := strings.ToLower(strings.TrimSpace(a.FileName)); f != "" {
			return "f:" + f
		}
		// filename-like auth index
		ai := strings.ToLower(strings.TrimSpace(a.AuthIndex))
		if strings.HasSuffix(ai, ".json") || strings.Contains(ai, "@") {
			return "f:" + ai
		}
		return "a:" + a.AuthIndex
	}
	score := func(a *state.Account) int {
		if a == nil {
			return -1
		}
		s := 0
		ai := strings.ToLower(a.AuthIndex)
		if ai != "" && !strings.Contains(ai, "@") && !strings.HasSuffix(ai, ".json") {
			s += 100 // real runtime auth index
		}
		if a.LastSignal != "" {
			s += 20
		}
		if a.Email != "" {
			s += 5
		}
		if a.FileName != "" {
			s += 5
		}
		if a.DayCalls > 0 {
			s += int(a.DayCalls)
		}
		return s
	}
	merge := func(dst, src *state.Account) {
		if dst == nil || src == nil {
			return
		}
		if dst.LastSignal == "" {
			dst.LastSignal = src.LastSignal
		}
		if dst.Email == "" {
			dst.Email = src.Email
		}
		if dst.FileName == "" {
			dst.FileName = src.FileName
		}
		if dst.DisableSource == "" {
			dst.DisableSource = src.DisableSource
		}
		if dst.DayCalls < src.DayCalls {
			dst.DayCalls = src.DayCalls
		}
		if dst.DayFailCalls < src.DayFailCalls {
			dst.DayFailCalls = src.DayFailCalls
		}
		if dst.RecoverAt.IsZero() && !src.RecoverAt.IsZero() {
			dst.RecoverAt = src.RecoverAt
		}
	}
	for _, a := range accs {
		if a == nil {
			continue
		}
		k := keyOf(a)
		if k == "" {
			continue
		}
		cur, ok := best[k]
		if !ok {
			cp := *a
			best[k] = &cp
			order = append(order, k)
			continue
		}
		// always merge metadata
		if score(a) > score(cur) {
			cp := *a
			merge(&cp, cur)
			best[k] = &cp
		} else {
			merge(cur, a)
		}
	}
	out := make([]*state.Account, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}


func itoaPanel(n int) string {
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

func streakTotal(acc *state.Account) int {
	if acc == nil || acc.Streaks == nil {
		return 0
	}
	n := 0
	for _, v := range acc.Streaks {
		n += v
	}
	return n
}

func liveActiveReason(acc *state.Account) string {
	if acc == nil {
		return "正常·观察中"
	}
	bestK, bestN := "", 0
	if acc.Streaks != nil {
		for k, v := range acc.Streaks {
			if v > bestN {
				bestK, bestN = k, v
			}
		}
	}
	sig := acc.LastSignal
	if bestN > 0 {
		sig = bestK
	}
	switch sig {
	case "permission_403":
		if bestN > 0 {
			return "正常·403×" + itoaPanel(bestN)
		}
		return "正常·403观察"
	case "free_usage_429":
		if bestN > 0 {
			return "正常·429×" + itoaPanel(bestN)
		}
		return "正常·429观察"
	case "spending_limit_402":
		return "正常·402观察"
	case "auth_401":
		if bestN > 0 {
			return "正常·401×" + itoaPanel(bestN)
		}
		return "正常·401观察"
	case "code:invalid-argument":
		return "正常·参数观察"
	}
	if bestN > 0 {
		return "正常·观察×" + itoaPanel(bestN)
	}
	return "正常·观察中"
}

func matchStateFilter(acc *state.Account, filter string) bool {
	if acc == nil {
		return false
	}
	switch filter {
	case "", "all":
		return true
	case "active", "active_clean":
		return (acc.State == state.Active || acc.State == "") && acc.LastSignal == "" && streakTotal(acc) == 0 && acc.DisableSource != "plugin_auto"
	case "active_watch":
		return (acc.State == state.Active || acc.State == "") && (acc.LastSignal != "" || streakTotal(acc) > 0)
	case "user_manual", "permanent_disable":
		return acc.State == state.UserManual && acc.DisableSource != "cpa_file_disabled" && acc.DisableSource != "cpa_disabled"
	case "cpa_disabled":
		return acc.State == state.UserManual && (acc.DisableSource == "cpa_file_disabled" || acc.DisableSource == "cpa_disabled")
	default:
		return string(acc.State) == filter
	}
}

func suggestAction(acc *state.Account) (action, reason string) {
	if acc == nil {
		return "none", ""
	}
	switch acc.State {
	case state.CandidateDead:
		return "trash", "401·候删"
	case state.CooldownQuota:
		return "wait", "429·额度冷却"
	case state.CooldownSpending:
		return "wait", "402·消费冷却"
	case state.CooldownPermission:
		return "review", "403·权限冷却"
	case state.UserManual:
		if acc.DisableSource == "cpa_file_disabled" || acc.DisableSource == "cpa_disabled" {
			return "manual", "CPA文件已禁用"
		}
		return "manual", "永久禁用"
	case state.Trashed:
		return "restore_or_purge", "垃圾箱"
	}
	if acc.State == state.Active || acc.State == "" {
		if n := streakTotal(acc); n > 0 || acc.LastSignal != "" {
			return "observe", liveActiveReason(acc)
		}
		return "none", "正常·可用"
	}
	switch acc.LastSignal {
	case "auth_401":
		return "candidate", "401·候删"
	case "permission_403":
		return "cooldown", "403·权限冷却"
	case "free_usage_429":
		return "cooldown", "429·额度冷却"
	case "spending_limit_402":
		return "cooldown", "402·消费冷却"
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
	// Best-effort resolve identities for display (read-only; UpdateMeta no longer bumps UpdatedAt).
	if a.Guard != nil && a.Guard.Resolver != nil {
		_ = a.Guard.Resolver.Ensure(r.Context())
		for _, acc := range a.State.AccountsSnapshot() {
			if id, ok := a.Guard.Resolver.Resolve(acc.AuthIndex, acc.FileName, acc.Email); ok {
				a.State.UpdateMeta(acc.AuthIndex, id.FileName, id.Email, "")
			}
		}
	}
	accs := a.State.AccountsSnapshot()
	accs = dedupeAccountsForPanel(accs)
	// rehydrate free-usage actual/limit from observed error samples (does not bump UpdatedAt)
	for _, o := range a.State.ListObserved() {
		if o.Sample == "" || o.LastAuth == "" {
			continue
		}
		if o.Key != "free_usage_429" && o.Signal != "free_usage_429" && !strings.Contains(o.Sample, "free-usage-exhausted") {
			continue
		}
		qi := quota.FreeUsageExhaustedEstimate(o.Sample, time.Time{})
		if qi.Used > 0 || qi.Limit > 0 {
			a.State.UpdateQuotaQuiet(o.LastAuth, qi.Limit, qi.Used, qi.Remaining, "free_usage_exhausted", qi.ResetAt)
		}
	}
	accs = a.State.AccountsSnapshot()
	// CPAMP usage.sqlite per-account day stats (same source as request monitor)
	cpampByAuth, cpampDB, _ := cpamp.FetchXAIAccountDay(r.Context())
	// also index by email/file for fallback matching
	cpampByEmail := map[string]cpamp.AccountDay{}
	for _, v := range cpampByAuth {
		if v.Account != "" {
			cpampByEmail[strings.ToLower(v.Account)] = v
		}
		if v.Label != "" {
			cpampByEmail[strings.ToLower(v.Label)] = v
		}
	}
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
		DaySuccess      int64          `json:"day_success,omitempty"`
		DayInputTokens  int64          `json:"day_input_tokens,omitempty"`
		DayOutputTokens int64          `json:"day_output_tokens,omitempty"`
		TotalCalls      int64          `json:"total_calls,omitempty"`
		TotalSuccess    int64          `json:"total_success,omitempty"`
		TotalFailure    int64          `json:"total_failure,omitempty"`
		TotalTokens     int64          `json:"total_tokens,omitempty"`
		// Recent15: last 15 request outcomes, oldest→newest; true=success false=fail
		Recent15        []bool         `json:"recent15,omitempty"`
		QuotaText       string         `json:"quota_text,omitempty"`
		UsageSource     string         `json:"usage_source,omitempty"`
		SuccessRate     float64        `json:"success_rate,omitempty"` // total success rate
		DaySuccessRate  float64        `json:"day_success_rate,omitempty"`
		QuotaRatioText  string         `json:"quota_ratio_text,omitempty"` // 今日/24h额度
		SortMS          int64          `json:"-"`
	}
	summary := map[string]int{
		"total": 0, "active": 0, "cooldown": 0, "candidate": 0,
		"user_manual": 0, "trashed": 0, "with_signal": 0,
		"suggest_cooldown": 0, "suggest_candidate": 0, "suggest_trash": 0,
		"suggest_review": 0, "suggest_wait": 0,
	}
	signalCounts := map[string]int{}
	var dayCalls, dayFails, dayTokens int64
	var cpampTokSum, cpampCallSum int64
	rows := make([]row, 0, len(accs))
	for _, acc := range accs {
		// prefer CPAMP usage.sqlite per-auth stats (display-only; no state mutation on read)
		usageSrc := "local"
		dayC, dayF, dayT := acc.DayCalls, acc.DayFailCalls, acc.DayTokens
		var dayS, dayIn, dayOut int64
		var totC, totS, totF, totT int64
		var recent15 []bool
		var lastReqMS int64
		var cuOK cpamp.AccountDay
		var hasCU bool
		if cu, ok := cpampByAuth[acc.AuthIndex]; ok {
			usageSrc = "cpamp"
			dayC, dayS, dayF = cu.Calls, cu.Success, cu.Failure
			dayT, dayIn, dayOut = cu.Tokens, cu.InputTokens, cu.OutputTokens
			totC, totS, totF, totT = cu.TotalCalls, cu.TotalSuccess, cu.TotalFailure, cu.TotalTokens
			recent15 = cu.Recent15
			lastReqMS = cu.LastMS
			cuOK, hasCU = cu, true
		} else if acc.Email != "" {
			if cu, ok := cpampByEmail[strings.ToLower(acc.Email)]; ok {
				usageSrc = "cpamp"
				dayC, dayS, dayF = cu.Calls, cu.Success, cu.Failure
				dayT, dayIn, dayOut = cu.Tokens, cu.InputTokens, cu.OutputTokens
				totC, totS, totF, totT = cu.TotalCalls, cu.TotalSuccess, cu.TotalFailure, cu.TotalTokens
				recent15 = cu.Recent15
				lastReqMS = cu.LastMS
				cuOK, hasCU = cu, true
			}
		}
		// apply actual/limit for THIS response only (do not mutate store on /state read)
		if hasCU && (cuOK.Actual > 0 || cuOK.Limit > 0) {
			acc.QuotaLimit, acc.QuotaUsed = cuOK.Limit, cuOK.Actual
			acc.QuotaRemaining = max64(0, cuOK.Limit-cuOK.Actual)
			acc.QuotaSource = "cpamp_fail_body"
		}
		// if still no success count, derive
		if dayS == 0 && dayC >= dayF {
			dayS = dayC - dayF
		}
		if totC == 0 {
			totC, totS, totF, totT = dayC, dayS, dayF, dayT
		}
		if totS == 0 && totC >= totF {
			totS = totC - totF
		}

		summary["total"]++
		dayCalls += dayC
		dayFails += dayF
		dayTokens += dayT
		if usageSrc == "cpamp" {
			cpampTokSum += dayT
			cpampCallSum += dayC
		}
		switch acc.State {
		case state.Active, "":
			summary["active"]++
			if acc.LastSignal != "" || streakTotal(acc) > 0 {
				summary["active_watch"]++
			} else {
				summary["active_clean"]++
			}
		case state.CooldownQuota:
			summary["cooldown"]++
			summary["cooldown_quota"]++
		case state.CooldownSpending:
			summary["cooldown"]++
			summary["cooldown_spending"]++
		case state.CooldownPermission:
			summary["cooldown"]++
			summary["cooldown_permission"]++
		case state.CandidateDead:
			summary["candidate"]++
		case state.UserManual:
			if acc.DisableSource == "cpa_file_disabled" || acc.DisableSource == "cpa_disabled" {
				summary["cpa_disabled"]++
			} else {
				summary["user_manual"]++
			}
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
			if acc.State == state.Active && acc.LastSignal == "" && streakTotal(acc) == 0 {
				continue
			}
		}
		if stateFilter != "" && !matchStateFilter(acc, stateFilter) {
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
			// broad match: email/file/auth/state/signal/action/reason/quota text/tokens
			stZH := map[string]string{
				"active": "正常·可用", "cooldown_quota": "429·额度冷却", "cooldown_spending": "402·消费冷却",
				"cooldown_permission": "403·权限冷却", "candidate_dead": "401·候删", "user_manual": "永久禁用",
				"trashed": "垃圾箱", "purged": "已清除",
			}
			blob := strings.ToLower(strings.Join([]string{
				display, acc.Email, acc.FileName, acc.AuthIndex, acc.Tier, acc.LastSignal, act, reason,
				string(acc.State), stZH[string(acc.State)], acc.DisableSource, acc.QuotaSource,
				formatTokens(dayT), formatTokens(totT), usageSrc,
			}, " "))
			// also allow multi-token AND if query has spaces
			ok := true
			for _, part := range strings.Fields(q) {
				if part == "" {
					continue
				}
				if !strings.Contains(blob, part) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
		}
		var ra, ua any
		if !acc.RecoverAt.IsZero() {
			ra = acc.RecoverAt.In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04")
		}
		// Prefer last real request time from CPAMP usage events (not panel refresh time)
		if lastReqMS > 0 {
			ua = time.UnixMilli(lastReqMS).In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04:05")
		} else if !acc.UpdatedAt.IsZero() {
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
		qText := ""
		// Prefer real body/cpamp numbers. If none, show "用尽" without inventing 2M/2M.
		if (qLimit == 0 && qUsed == 0 && qRem == 0) &&
			(acc.LastSignal == "free_usage_429" || qSrc == "free_usage_exhausted" ||
				acc.State == state.CooldownQuota) {
			// if CPAMP day tokens present, show that as used under free-tier limit estimate
			if dayT > 0 {
				qUsed = dayT
				qLimit = quota.FreeQuotaPerAccount
				if qUsed > qLimit {
					qRem = 0
				} else {
					qRem = qLimit - qUsed
				}
				qSrc = "cpamp_day_tokens"
				qText = formatTokens(qUsed) + " / " + formatTokens(qLimit) + " 剩 " + formatTokens(qRem)
			} else {
				qText = "用尽"
				qSrc = "free_usage_exhausted_est"
			}
		} else {
			if qLimit > 0 && qUsed > qLimit {
				qRem = 0
			} else if qLimit > 0 && qUsed >= 0 && qRem == 0 && qUsed <= qLimit {
				qRem = qLimit - qUsed
			}
			if qLimit > 0 || qUsed > 0 || qRem > 0 {
				qText = formatTokens(qUsed) + " / " + formatTokens(qLimit) + " 剩 " + formatTokens(qRem)
			} else if dayT > 0 {
				qText = formatTokens(dayT)
				qUsed = dayT
				qSrc = usageSrc
			}
		}
		// total success rate (primary) + day success rate
		rate := 0.0
		if totC > 0 {
			rate = float64(totS) / float64(totC) * 100
		}
		dayRate := 0.0
		if dayC > 0 {
			dayRate = float64(dayS) / float64(dayC) * 100
		}
		// free-tier 24h quota: prefer real limit from error body, else 2M estimate
		quota24 := qLimit
		if quota24 <= 0 {
			quota24 = quota.FreeQuotaPerAccount
		}
		ratioText := ""
		if dayT > 0 || qUsed > 0 {
			// 今日用量 / 24小时额度
			ratioText = formatTokens(dayT) + " / " + formatTokens(quota24)
		}
		// usage display: 总计 · 今日 · 今日/24h额度
		usageMain := ""
		if totT > 0 || dayT > 0 {
			usageMain = formatTokens(totT) + " · " + formatTokens(dayT)
			if ratioText != "" {
				usageMain += " · " + ratioText
			}
		} else if qText != "" {
			usageMain = qText
		}
		rows = append(rows, row{
			AuthIndex: acc.AuthIndex, FileName: acc.FileName, Email: acc.Email, DisplayName: display,
			Tier: acc.Tier, State: string(acc.State), Signal: acc.LastSignal,
			DisableSource: acc.DisableSource, Streaks: acc.Streaks, StreakSummary: streakSum,
			SuggestedAction: act, Reason: reason, RecoverAt: ra, UpdatedAt: ua,
			QuotaLimit: qLimit, QuotaUsed: qUsed, QuotaRemaining: qRem,
			QuotaSource: qSrc, DayCalls: dayC, DayFailCalls: dayF,
			DayTokens: dayT, DaySuccess: dayS, DayInputTokens: dayIn, DayOutputTokens: dayOut,
			TotalCalls: totC, TotalSuccess: totS, TotalFailure: totF, TotalTokens: totT,
			Recent15: recent15,
			QuotaText: usageMain, UsageSource: usageSrc, SuccessRate: rate, DaySuccessRate: dayRate,
			QuotaRatioText: ratioText,
			SortMS: lastReqMS,
		})
	}
	// newest request first (CPAMP last_ms); fallback UpdatedAt string desc
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].SortMS != rows[j].SortMS {
			return rows[i].SortMS > rows[j].SortMS
		}
		si, _ := rows[i].UpdatedAt.(string)
		sj, _ := rows[j].UpdatedAt.(string)
		if si != sj {
			return si > sj
		}
		return rows[i].DisplayName < rows[j].DisplayName
	})
	// if local day tokens empty, use cpamp sum for pool used
	if dayTokens == 0 && cpampTokSum > 0 {
		dayTokens = cpampTokSum
	}
	if dayCalls == 0 && cpampCallSum > 0 {
		dayCalls = cpampCallSum
	}
	_ = cpampDB
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
	// pool total uses ALL xAI auths (not only currently enabled), so cooldown
	// does not shrink the estimated pool capacity.
	xaiN := asInt(inv["auth_xai"])
	if xaiN == 0 {
		xaiN = asInt(inv["auth_total"])
	}
	enabledN := asInt(inv["auth_enabled"])
	poolEst := int64(xaiN) * quota.FreeQuotaPerAccount
	// used: prefer CPAMP per-account token sum, else floor
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
		"cpamp_db":       cpampDB,
		"cpamp_accounts": len(cpampByAuth),
		// pool = 全量 xAI × 2M；冷却只影响「可接流量」，不影响总量口径
		"pool_est":         poolEst,
		"pool_per_account": quota.FreeQuotaPerAccount,
		"pool_xai_total":   xaiN,
		"pool_enabled":     enabledN,
		"pool_used":        usedTok,
		"pool_remaining":   remainTok,
		"pool_used_pct":    pct,
		"pool_source":      "xai_total×2M est; used=cpamp per-account tokens",
	}
	writeJSON(w, 200, map[string]any{
		"plugin":         "cpa-xai-sentry",
		"version":        version.Version,
		"mode":           modeOf(*a.Cfg),
		"mode_label":     modeLabel(modeOf(*a.Cfg)),
		"summary":        summary,
		"state_filters": []map[string]any{
			{"value": "", "label": "全部状态", "count": summary["total"]},
			{"value": "active_clean", "label": "正常·可用", "count": summary["active_clean"]},
			{"value": "active_watch", "label": "正常·观察中", "count": summary["active_watch"]},
			{"value": "cooldown_quota", "label": "429·额度冷却", "count": summary["cooldown_quota"]},
			{"value": "cooldown_spending", "label": "402·消费冷却", "count": summary["cooldown_spending"]},
			{"value": "cooldown_permission", "label": "403·权限冷却", "count": summary["cooldown_permission"]},
			{"value": "candidate_dead", "label": "401·候删", "count": summary["candidate"]},
			{"value": "user_manual", "label": "永久禁用", "count": summary["user_manual"]},
			{"value": "cpa_disabled", "label": "CPA已禁用", "count": summary["cpa_disabled"]},
			{"value": "trashed", "label": "垃圾箱", "count": summary["trashed"]},
		},
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

func asFloatAny(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// formatTokens renders large token counts compactly (2.05M / 549k / 120).
func formatTokens(n int64) string {
	if n < 0 {
		n = -n
	}
	if n >= 1_000_000 {
		whole := n / 1_000_000
		// two decimal digits from remainder
		frac := (n % 1_000_000) / 10_000
		if frac == 0 {
			return itoa64(whole) + "M"
		}
		if frac%10 == 0 {
			return itoa64(whole) + "." + itoa64(frac/10) + "M"
		}
		// pad two digits
		if frac < 10 {
			return itoa64(whole) + ".0" + itoa64(frac) + "M"
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
		// partial update: only override provided patrol/runtime fields from JSON object
		var raw map[string]any
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		cfg := *a.Cfg
		// merge known fields if present
		if v, ok := raw["patrol_enabled"].(bool); ok {
			cfg.PatrolEnabled = v
		}
		if v, ok := asFloatAny(raw["patrol_interval"]); ok && v > 0 {
			cfg.PatrolInterval = int(v)
		}
		if v, ok := asFloatAny(raw["patrol_timeout"]); ok && v > 0 {
			cfg.PatrolTimeout = int(v)
		}
		if v, ok := asFloatAny(raw["patrol_concurrency"]); ok && v > 0 {
			cfg.PatrolConcurrency = int(v)
		}
		if v, ok := asFloatAny(raw["patrol_batch_size"]); ok && v >= 0 {
			cfg.PatrolBatchSize = int(v)
		}
		if v, ok := raw["patrol_model"].(string); ok && strings.TrimSpace(v) != "" {
			cfg.PatrolModel = strings.TrimSpace(v)
		}
		if v, ok := raw["patrol_proxy_url"].(string); ok {
			cfg.PatrolProxyURL = strings.TrimSpace(v)
		}
		if v, ok := raw["patrol_auto_model_switch"].(bool); ok {
			cfg.PatrolAutoModelSwitch = v
		}
		// also allow full config shape if client sent nested-less full body with bools
		if v, ok := raw["auto_cooldown"].(bool); ok {
			cfg.AutoCooldown = v
		}
		if v, ok := raw["auto_candidate"].(bool); ok {
			cfg.AutoCandidate = v
		}
		if v, ok := raw["auto_delete"].(bool); ok {
			cfg.AutoDelete = v
		}
		if v, ok := raw["sentry_enabled"].(bool); ok {
			cfg.SentryEnabled = v
		}
		cfg = cfg.Validate()
		*a.Cfg = cfg
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
		SourceLabel string `json:"source_label"`
		Signal      string `json:"signal"`
		SignalLabel string `json:"signal_label"`
		Action      string `json:"action"`
		ActionLabel string `json:"action_label"`
		Reason      string `json:"reason"`
		Text        string `json:"text"`
		Level       string `json:"level"` // ok|warn|err|info
	}
	out := make([]L, 0)
	for _, e := range a.State.SnapshotLogs() {
		label := e.Auth
		if acc := a.State.Get(e.Auth); acc != nil {
			label = cpaapi.DisplayName(acc.Email, acc.FileName, acc.AuthIndex)
		}
		actL := logActionZH(e.Action)
		sigL := logSignalZH(e.Signal)
		srcL := logSourceZH(e.Source)
		reason := humanizeReason(e.Reason, sigL, e.Action)
		text, level := composeLogText(actL, e.Action, sigL, e.Signal, label, reason, srcL, e.Source)
		out = append(out, L{
			At: e.At.In(time.FixedZone("CST", 8*3600)).Format("15:04:05"),
			Auth: e.Auth, AuthLabel: label, Source: e.Source, SourceLabel: srcL,
			Signal: e.Signal, SignalLabel: sigL,
			Action: e.Action, ActionLabel: actL, Reason: reason, Text: text, Level: level,
		})
	}
	writeJSON(w, 200, map[string]any{"logs": out})
}

func humanizeReason(reason, sigL, action string) string {
	r := strings.TrimSpace(reason)
	switch r {
	case "", "free_usage", "bulk_suggested_cooldown":
		if sigL != "" {
			return sigL
		}
		if r == "bulk_suggested_cooldown" {
			return "批量按建议冷却"
		}
		return ""
	case "recover_at":
		return "冷却/额度重置到期"
	case "panel bulk/manual":
		return "面板批量操作"
	case "cpa_disabled_sync":
		return "CPA凭证文件已禁用（非面板操作）"
	case "foreign_or_unknown_disabled", "foreign_disabled_untracked":
		return "非本哨兵冷却禁用，已重新打开"
	case "closed_loop_clean":
		return "闭环清理脏正常态"
	case "cpa_disabled_free_usage_sync":
		return "CPA已禁用且有免费额度证据（状态对齐）"
	case "demote_false_quota_cooldown":
		return "纠正误标额度冷却（CPA文件禁用，无免费额度证据）"
	case "今日用量自动回补", "今日用量回补":
		return r
	}
	// already chinese / readable
	if strings.ContainsAny(r, "冷却恢复候选垃圾箱额度权限凭证") {
		return r
	}
	// map common english leftovers
	switch r {
	case "free_usage_exhausted", "subscription:free-usage-exhausted":
		return "免费额度用尽"
	}
	if sigL != "" && (r == action || r == sigL) {
		return sigL
	}
	return r
}

func composeLogText(actL, action, sigL, signal, who, reason, srcL, source string) (string, string) {
	level := "info"
	switch action {
	case "cooldown_failed", "reenable_failed":
		level = "err"
	case "cooldown", "candidate", "trash", "delete", "manual_disable":
		level = "warn"
	case "reenable", "manual_enable", "backfill", "auto_backfill", "restore":
		level = "ok"
	}
	who = strings.TrimSpace(who)
	reason = strings.TrimSpace(reason)
	sigL = strings.TrimSpace(sigL)
	// prefer reason over signal if more informative
	why := reason
	if why == "" {
		why = sigL
	} else if sigL != "" && why != sigL && !strings.Contains(why, sigL) {
		// keep compact: if reason already covers, skip
	}
	// narrative templates
	switch action {
	case "cooldown":
		if source == "tick" && who != "" {
			return "维护同步：CPA 已禁用 " + who + "，已对齐为冷却", level
		}
		if who != "" && why != "" {
			return "因" + why + "，已将 " + who + " 转入冷却", level
		}
		if who != "" {
			return "已将 " + who + " 转入冷却", level
		}
		return "已执行冷却", level
	case "cooldown_failed":
		if who != "" && why != "" {
			return "冷却 " + who + " 失败：" + why, level
		}
		if who != "" {
			return "冷却 " + who + " 失败", level
		}
		return "冷却失败", level
	case "reenable":
		if who != "" {
			return "冷却/额度重置到期，已恢复启用 " + who + "（状态已重置为正常）", "ok"
		}
		return "冷却/额度重置到期，已恢复启用（状态已重置为正常）", "ok"
	case "reenable_failed":
		if who != "" && why != "" {
			return "恢复启用 " + who + " 失败：" + why, level
		}
		if who != "" {
			return "恢复启用 " + who + " 失败", level
		}
		return "恢复启用失败", level
	case "candidate":
		if who != "" && why != "" {
			return "因" + why + "，已将 " + who + " 移入候选", level
		}
		if who != "" {
			return "已将 " + who + " 移入候选", level
		}
		return "账号已移入候选", level
	case "trash", "delete":
		if who != "" && why != "" {
			return "因" + why + "，已将 " + who + " 移入垃圾箱", level
		}
		if who != "" {
			return "已将 " + who + " 移入垃圾箱", level
		}
		return "账号已移入垃圾箱", level
	case "file_disabled_sync":
		if who != "" {
			return "维护发现 CPA 凭证文件已禁用，已将 " + who + " 标记为「CPA已禁用」（不是你在面板点的禁用）", "warn"
		}
		return "维护发现 CPA 凭证文件已禁用（不是面板手动禁用）", "warn"
	case "manual_disable":
		if who != "" {
			return "已永久禁用 " + who, level
		}
		return "已手动禁用账号", level
	case "manual_enable":
		if who != "" {
			return "已在面板手动启用 " + who, level
		}
		return "已手动启用账号", level
	case "backfill", "auto_backfill":
		if why != "" {
			return why, "ok"
		}
		return "已回补今日用量数据", "ok"
	case "restore", "restore_enable":
		if who != "" {
			return "已从垃圾箱恢复 " + who, "ok"
		}
		return "已从垃圾箱恢复账号", "ok"
	case "purge":
		if who != "" {
			return "已彻底清除 " + who, "warn"
		}
		return "已彻底清除垃圾箱条目", "warn"
	case "patrol_start":
		return "开始主动巡查：" + firstNonEmpty(why, "执行中"), "info"
	case "patrol_done":
		return firstNonEmpty(why, "主动巡查已完成"), "ok"
	case "observe":
		if who != "" && why != "" {
			return "观察到 " + who + " 出现" + why + "（仅记录，未处置）", "info"
		}
		return "记录到异常信号（仅观察）", "info"
	}
	// generic fluent fallback
	parts := make([]string, 0, 4)
	if actL != "" && actL != "—" {
		parts = append(parts, actL)
	}
	if who != "" {
		parts = append(parts, who)
	}
	if why != "" && why != actL {
		parts = append(parts, "原因："+why)
	}
	if len(parts) == 0 {
		return "系统事件", level
	}
	return strings.Join(parts, " · "), level
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
	case "sync_disabled_failed":
		return "同步禁用失败"
	case "candidate":
		return "进候选"
	case "manual_disable":
		return "手动禁用"
	case "file_disabled_sync":
		return "CPA文件禁用对齐"
	case "reopen_foreign":
		return "打开非自有禁用"
	case "scrub_active":
		return "清理脏正常态"
	case "reopen_foreign_failed":
		return "打开非自有禁用失败"
	case "manual_enable":
		return "手动启用"
	case "backfill", "auto_backfill":
		return "用量回补"
	case "trash", "delete":
		return "进垃圾箱"
	case "restore", "restore_enable":
		return "恢复"
	case "purge":
		return "彻底清除"
	case "observe":
		return "仅观察"
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

func logSourceZH(s string) string {
	switch s {
	case "usage":
		return "请求监控"
	case "panel":
		return "面板"
	case "patrol":
		return "巡查"
	case "patrol_start":
		return "开始巡查"
	case "patrol_done":
		return "巡查完成"
	case "probe_error":
		return "探测失败"
	case "cpamp":
		return "用量回补"
	case "tick", "sentry":
		return "哨兵"
	case "trash":
		return "垃圾箱"
	default:
		if s == "" {
			return ""
		}
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
	// report cooldown count after sync for panel toast/debug
	cool := a.State.CooldownStats(time.Now())
	writeJSON(w, 200, map[string]any{
		"ok":             true,
		"cooldown_stats": cool,
		"note":           "recovered due cooldowns + synced CPA disabled → sentry cooldown + purged trash",
	})
}

func (a *API) handlePatrolStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	if a.Patrol == nil {
		writeJSON(w, 503, map[string]string{"error": "patrol runtime 未就绪"})
		return
	}
	var in struct {
		Mode string `json:"mode"` // full | cooldown
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	mode := patrol.ModeFull
	if strings.EqualFold(strings.TrimSpace(in.Mode), "cooldown") || strings.EqualFold(strings.TrimSpace(in.Mode), "spending") {
		mode = patrol.ModeCooldown
	}
	// apply latest cfg
	if a.Cfg != nil {
		a.Patrol.Cfg = *a.Cfg
	}
	if a.Guard != nil {
		a.Patrol.Guard = a.Guard
	}
	st, err := a.Patrol.Start(r.Context(), mode)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "patrol": st})
}

func (a *API) handlePatrolStatus(w http.ResponseWriter, r *http.Request) {
	st := map[string]any{"running": false, "message": "未就绪"}
	if a.Patrol != nil {
		ps := a.Patrol.Status()
		b, _ := json.Marshal(ps)
		_ = json.Unmarshal(b, &st)
	}
	// schedule hints from config
	interval := 3600
	if a.Cfg != nil && a.Cfg.PatrolInterval > 0 {
		interval = a.Cfg.PatrolInterval
	}
	enabled := a.Cfg != nil && a.Cfg.PatrolEnabled
	st["patrol_enabled"] = enabled
	st["patrol_interval"] = interval
	// last from status finished/started
	last := ""
	if v, ok := st["finished_at"].(string); ok && v != "" {
		last = v
	} else if v, ok := st["started_at"].(string); ok {
		last = v
	}
	st["last_patrol_at"] = last
	if enabled && interval > 0 {
		// best-effort next: now+interval if idle, else unknown
		running, _ := st["running"].(bool)
		if !running {
			st["next_patrol_at"] = time.Now().In(time.FixedZone("CST", 8*3600)).Add(time.Duration(interval) * time.Second).Format("01-02 15:04:05")
			st["next_patrol_hint"] = "约 " + itoa(interval) + " 秒后（定时轮询估算）"
		} else {
			st["next_patrol_at"] = ""
			st["next_patrol_hint"] = "巡查进行中"
		}
	} else {
		st["next_patrol_at"] = ""
		st["next_patrol_hint"] = "定时巡查已关闭"
	}
	// recent finished jobs for expandable task list
	var hist any = []any{}
	if a.Patrol != nil {
		hist = a.Patrol.History()
	}
	writeJSON(w, 200, map[string]any{"ok": true, "patrol": st, "history": hist})
}


func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ok": true, "plugin": "cpa-xai-sentry", "version": version.Version,
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
			pol := state.ErrorPolicy{
				Key: p.Key, Label: p.Label, Enabled: p.Enabled, Action: string(p.Action),
				Threshold: p.Threshold, CooldownSec: p.CooldownSec, NeverTrash: p.NeverTrash,
				Note: p.Note, Source: p.Source, CountMode: "streak",
			}
			if k == "permission_403" {
				pol.Escalations = []state.EscalationRule{
					{Streak: 3, Action: "cooldown", CooldownSec: 1800},
					{Streak: 15, Action: "disable"},
				}
				pol.Threshold = 3
				pol.Action = "cooldown"
				pol.CooldownSec = 1800
			}
			builtins[k] = pol
		}
		a.State.EnsureBuiltinPolicies(builtins)
	}
	// collapse legacy duplicate keys into builtins
	for _, pair := range [][3]string{
		{"http_401", "auth_401", "401·凭证失效"},
		{"http_0_disabled", "unmatched", "未分类错误"},
	} {
		if err := a.State.ReclassifyErrorKey(pair[0], pair[1], pair[2]); err == nil {
			_ = a.State.Save()
		}
	}
	obs := a.State.ListObserved()
	pols := a.State.ListErrorPolicies()
	// join: every observed + every policy
	type row struct {
		Key          string         `json:"key"`
		Label        string         `json:"label"`
		Enabled      bool           `json:"enabled"`
		Action       string         `json:"action"`
		ActionLabel  string         `json:"action_label"`
		Threshold    int            `json:"threshold"`
		CooldownSec  int            `json:"cooldown_seconds"`
		CountMode    string         `json:"count_mode,omitempty"`
		Escalations  []state.EscalationRule `json:"escalations,omitempty"`
		NeverTrash   bool           `json:"never_trash"`
		Note         string         `json:"note"`
		Source       string         `json:"source"`
		Count        int64          `json:"count"`
		LastAt       string         `json:"last_at,omitempty"`
		Sample       string         `json:"sample,omitempty"`
		SampleMsg    string         `json:"sample_msg,omitempty"`
		SamplePretty string         `json:"sample_pretty,omitempty"`
		StatusCode   int            `json:"status_code,omitempty"`
		Code         string         `json:"code,omitempty"`
		LastAuth     string         `json:"last_auth,omitempty"`
		LastFile     string         `json:"last_file,omitempty"`
		// AccountHits for policy page table (time/account/error/streak/ops)
		AccountHits  []map[string]any `json:"account_hits,omitempty"`
		Shapes       []map[string]any `json:"shapes,omitempty"` // unmatched split candidates
	}
	byKey := map[string]row{}
	for _, p := range pols {
		esc := p.NormalizedEscalations()
		// upgrade legacy 403 single-tier to default ladder if still only one cooldown@3
		if p.Key == "permission_403" && (len(p.Escalations) == 0) {
			esc = []state.EscalationRule{
				{Streak: 3, Action: "cooldown", CooldownSec: 1800},
				{Streak: 15, Action: "disable"},
			}
			// persist default ladder once
			pp := p
			pp.Escalations = esc
			pp.Threshold = 3
			pp.Action = "cooldown"
			pp.CooldownSec = 1800
			if pp.CountMode == "" {
				pp.CountMode = "streak"
			}
			a.State.UpsertErrorPolicy(pp)
			_ = a.State.Save()
		}
		cm := p.CountMode
		if cm == "" {
			cm = "streak"
		}
		byKey[p.Key] = row{
			Key: p.Key, Label: p.Label, Enabled: p.Enabled, Action: p.Action,
			ActionLabel: actionLabel(p.Action), Threshold: p.Threshold, CooldownSec: p.CooldownSec,
			CountMode: cm, Escalations: esc,
			NeverTrash: p.NeverTrash, Note: p.Note, Source: p.Source,
		}
	}
	// account meta for labels/streaks
	accBy := map[string]*state.Account{}
	for _, acc := range a.State.AccountsSnapshot() {
		accBy[acc.AuthIndex] = acc
		if acc.Email != "" {
			accBy[strings.ToLower(acc.Email)] = acc
		}
	}
	for _, o := range obs {
		r0, ok := byKey[o.Key]
		if !ok {
			r0 = row{Key: o.Key, Label: o.Label, Enabled: true, Action: "observe", ActionLabel: actionLabel("observe"), Threshold: 1, Source: "learned"}
		}
		// normalize labels to short Chinese
		r0.Label = errorsig.LabelOf(o.Key, match.Result{Code: o.Code, Signal: match.Signal(o.Signal)}, o.StatusCode)
		if r0.Label == "" {
			r0.Label = o.Label
		}
		r0.SampleMsg = errorsig.HumanMsg(o.Key, o.Sample, o.StatusCode)
		r0.Count = o.Count
		if !o.LastAt.IsZero() {
			r0.LastAt = o.LastAt.In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04:05")
		}
		r0.Sample = html.UnescapeString(o.Sample)
		r0.StatusCode = o.StatusCode
		r0.Code = o.Code
		r0.LastAuth = o.LastAuth
		r0.LastFile = o.LastFile
		// build account hits from observed Hits ring
		type agg struct {
			Auth, Label, File, Source, Sample string
			Status, Hits, Streak              int
			LastAt                            time.Time
		}
		am := map[string]*agg{}
		for _, h := range o.Hits {
			id := h.Auth
			if id == "" {
				id = h.File
			}
			if id == "" {
				id = "unknown"
			}
			a0 := am[id]
			if a0 == nil {
				a0 = &agg{Auth: h.Auth, File: h.File, Source: h.Source, Sample: h.Sample, Status: h.Status}
				am[id] = a0
			}
			a0.Hits++
			if h.At.After(a0.LastAt) {
				a0.LastAt = h.At
				a0.Source = h.Source
				a0.Sample = h.Sample
				a0.Status = h.Status
				a0.File = h.File
			}
		}
		// fallback: if no hits ring yet, use last_auth
		if len(am) == 0 && o.LastAuth != "" {
			am[o.LastAuth] = &agg{Auth: o.LastAuth, File: o.LastFile, Hits: int(o.Count), LastAt: o.LastAt, Sample: o.Sample, Status: o.StatusCode}
		}
		hits := make([]map[string]any, 0, len(am))
		for _, a0 := range am {
			label := a0.Auth
			streak := 0
			if acc := accBy[a0.Auth]; acc != nil {
				label = cpaapi.DisplayName(acc.Email, acc.FileName, acc.AuthIndex)
				// streak for this error key / signal
				if acc.Streaks != nil {
					if v := acc.Streaks[o.Key]; v > 0 {
						streak = v
					} else if o.Signal != "" {
						if v := acc.Streaks[o.Signal]; v > 0 {
							streak = v
						}
					}
					if streak == 0 && acc.LastSignal == o.Key {
						// at least 1 if currently holding this signal
						streak = 1
					}
				}
			} else if a0.File != "" {
				label = a0.File
			}
			src := a0.Source
			if src == "" {
				src = "usage"
			}
			srcZH := map[string]string{"usage": "请求", "patrol": "巡查", "tick": "维护同步", "panel": "面板", "cpamp": "回补"}[src]
			if srcZH == "" {
				srcZH = src
			}
			msg := errorsig.HumanMsg(o.Key, a0.Sample, a0.Status)
			if msg == "" {
				msg = r0.Label
			}
			msg = msg + "（" + srcZH + "）"
			shape, shapeLabel, suggestKey := errorsig.ShapeOf(a0.Sample, a0.Status)
			hits = append(hits, map[string]any{
				"auth": a0.Auth, "label": label, "file": a0.File,
				"source": src, "source_label": srcZH,
				"hits": a0.Hits, "streak": streak,
				"status": a0.Status,
				"shape": shape, "shape_label": shapeLabel, "suggest_key": suggestKey,
				"last_at": func() string {
					if a0.LastAt.IsZero() {
						return ""
					}
					return a0.LastAt.In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04:05")
				}(),
				"message": msg,
				"sample":  a0.Sample,
			})
		}
		// sort by last_at desc
		sort.SliceStable(hits, func(i, j int) bool {
			si, _ := hits[i]["last_at"].(string)
			sj, _ := hits[j]["last_at"].(string)
			return si > sj
		})
		if len(hits) > 50 {
			hits = hits[:50]
		}
		r0.AccountHits = hits
		if o.Key == "unmatched" {
			shCount := map[string]map[string]any{}
			for _, h := range hits {
				sh, _ := h["shape"].(string)
				if sh == "" {
					continue
				}
				cur := shCount[sh]
				if cur == nil {
					cur = map[string]any{"shape": sh, "label": h["shape_label"], "suggest_key": h["suggest_key"], "count": 0, "sample": h["message"]}
					shCount[sh] = cur
				}
				cur["count"] = asInt(cur["count"]) + asInt(h["hits"])
			}
			shapes := make([]map[string]any, 0, len(shCount))
			for _, v := range shCount {
				shapes = append(shapes, v)
			}
			sort.SliceStable(shapes, func(i, j int) bool { return asInt(shapes[i]["count"]) > asInt(shapes[j]["count"]) })
			r0.Shapes = shapes
		}
		byKey[o.Key] = r0
	}
	out := make([]row, 0, len(byKey))
	for _, r0 := range byKey {
		// final label normalize
		if r0.Key == "free_usage_429" {
			r0.Label = "429·免费额度用尽"
		}
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
		return "候删"
	case "disable":
		return "永久禁用"
	case "trash":
		return "进垃圾箱"
	default:
		return a
	}
}

func (a *API) handleErrorReclassify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var in struct {
		From  string `json:"from"`
		To    string `json:"to"`
		Label string `json:"label"`
		Shape string `json:"shape"` // if set, split only this error shape from from(default unmatched)
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	in.From = strings.TrimSpace(in.From)
	in.To = strings.TrimSpace(in.To)
	in.Shape = strings.TrimSpace(in.Shape)
	if in.From == "" {
		in.From = "unmatched"
	}
	if in.To == "" {
		in.To = "unmatched"
	}
	if in.Label == "" && in.To == "unmatched" {
		in.Label = "未分类错误"
	}
	if in.Shape != "" {
		n, err := a.State.SplitObservedByShape(in.From, in.To, in.Label, in.Shape)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		_ = a.State.Save()
		writeJSON(w, 200, map[string]any{"ok": true, "from": in.From, "to": in.To, "shape": in.Shape, "moved": n})
		return
	}
	if err := a.State.ReclassifyErrorKey(in.From, in.To, in.Label); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	_ = a.State.Save()
	writeJSON(w, 200, map[string]any{"ok": true, "from": in.From, "to": in.To})
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
	if in.CountMode == "" {
		in.CountMode = "streak"
	}
	// normalize escalations; if empty, synthesize from legacy fields
	if len(in.Escalations) == 0 {
		if in.Action == "" {
			in.Action = "observe"
		}
		if in.Threshold <= 0 {
			in.Threshold = 1
		}
		in.Escalations = []state.EscalationRule{{
			Streak: in.Threshold, Action: in.Action, CooldownSec: in.CooldownSec,
		}}
	} else {
		// sort + fill; sync legacy fields from lowest tier for back-compat
		in.Escalations = state.ErrorPolicy{Escalations: in.Escalations}.NormalizedEscalations()
		low := in.Escalations[0]
		in.Threshold = low.Streak
		in.Action = low.Action
		if low.CooldownSec > 0 {
			in.CooldownSec = low.CooldownSec
		}
	}
	if errorsig.HardNeverTrash(in.Key) {
		in.NeverTrash = true
		for i := range in.Escalations {
			if in.Escalations[i].Action == "trash" {
				in.Escalations[i].Action = "cooldown"
			}
		}
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
