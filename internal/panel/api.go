package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/patrol"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

type API struct {
	Cfg    *sentrycfg.Config
	State  *state.Store
	Trash  *trash.Store
	Guard  *guard.Guard
	Patrol *patrol.Runner
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
	mux.HandleFunc("/ui", a.handleUI)
	mux.HandleFunc("/", a.handleUI)
	return mux
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
		return "trash", "候选死号，建议移入垃圾箱"
	case state.CooldownQuota:
		return "wait", "free-usage 冷却中，到期自动恢复"
	case state.CooldownSpending:
		return "wait", "spending-limit 冷却中，勿删除"
	case state.CooldownPermission:
		return "review", "permission 软故障，复查后决定"
	case state.UserManual:
		return "manual", "用户手动禁用，不自动启用"
	case state.Trashed:
		return "restore_or_purge", "已在垃圾箱"
	}
	switch acc.LastSignal {
	case "auth_401":
		return "candidate", "凭证失效信号，建议候选/人工确认"
	case "permission_403":
		return "cooldown", "权限拒绝，默认可恢复冷却"
	case "free_usage_429":
		return "cooldown", "免费额度耗尽，应冷却不应删除"
	case "spending_limit_402":
		return "cooldown", "消费限额，硬禁止自动删除"
	}
	if acc.State == state.Active && acc.LastSignal == "" {
		return "none", "正常"
	}
	return "observe", "仅观察"
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
	accs := a.State.AccountsSnapshot()
	type row struct {
		AuthIndex       string         `json:"auth_index"`
		FileName        string         `json:"file_name"`
		Email           string         `json:"email"`
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
	}
	summary := map[string]int{
		"total": 0, "active": 0, "cooldown": 0, "candidate": 0,
		"user_manual": 0, "trashed": 0, "with_signal": 0,
		"suggest_cooldown": 0, "suggest_candidate": 0, "suggest_trash": 0,
		"suggest_review": 0, "suggest_wait": 0,
	}
	signalCounts := map[string]int{}
	rows := make([]row, 0, len(accs))
	for _, acc := range accs {
		summary["total"]++
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
		if q != "" {
			blob := strings.ToLower(acc.Email + " " + acc.FileName + " " + acc.AuthIndex + " " + acc.Tier + " " + acc.LastSignal + " " + act + " " + reason)
			if !strings.Contains(blob, q) {
				continue
			}
		}
		var ra, ua any
		if !acc.RecoverAt.IsZero() {
			ra = acc.RecoverAt.UTC().Format(time.RFC3339)
		}
		if !acc.UpdatedAt.IsZero() {
			ua = acc.UpdatedAt.UTC().Format(time.RFC3339)
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
		rows = append(rows, row{
			AuthIndex: acc.AuthIndex, FileName: acc.FileName, Email: acc.Email,
			Tier: acc.Tier, State: string(acc.State), Signal: acc.LastSignal,
			DisableSource: acc.DisableSource, Streaks: acc.Streaks, StreakSummary: streakSum,
			SuggestedAction: act, Reason: reason, RecoverAt: ra, UpdatedAt: ua,
		})
	}
	writeJSON(w, 200, map[string]any{
		"plugin":        "cpa-xai-sentry",
		"version":       "0.1.1",
		"mode":          modeOf(*a.Cfg),
		"summary":       summary,
		"signal_counts": signalCounts,
		"accounts":      rows,
		"account_count": len(rows),
		"trash_count":   len(a.State.ListTrash()),
		"config":        a.Cfg.Redact(),
		"updated_at":    time.Now().UTC().Format(time.RFC3339),
	})
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
		if a.Guard != nil {
			a.Guard.Cfg = in
		}
		writeJSON(w, 200, a.Cfg.Redact())
	default:
		w.WriteHeader(405)
	}
}

func (a *API) handleLogs(w http.ResponseWriter, r *http.Request) {
	// return last logs without secrets
	type L struct {
		At     string `json:"at"`
		Auth   string `json:"auth"`
		Source string `json:"source"`
		Signal string `json:"signal"`
		Action string `json:"action"`
		Reason string `json:"reason"`
	}
	out := make([]L, 0)
	for _, e := range a.State.SnapshotLogs() {
		out = append(out, L{
			At: e.At.UTC().Format(time.RFC3339), Auth: e.Auth, Source: e.Source,
			Signal: e.Signal, Action: e.Action, Reason: e.Reason,
		})
	}
	writeJSON(w, 200, map[string]any{"logs": out})
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
		"ok": true, "plugin": "cpa-xai-sentry", "version": "0.1.0",
		"mode": modeOf(*a.Cfg), "config": a.Cfg.Redact(),
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
	if a.Guard != nil {
		a.Guard.Cfg = *a.Cfg
	}
	writeJSON(w, 200, map[string]any{"ok": true, "mode": modeOf(*a.Cfg), "config": a.Cfg.Redact()})
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
		writeJSON(w, 400, map[string]string{"error": "unknown preset; use observe|safe-guard|off"})
		return
	}
	*a.Cfg = a.Cfg.Validate()
	if a.Guard != nil {
		a.Guard.Cfg = *a.Cfg
	}
	writeJSON(w, 200, map[string]any{"ok": true, "mode": modeOf(*a.Cfg), "config": a.Cfg.Redact()})
}

func (a *API) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(uiHTML))
}
