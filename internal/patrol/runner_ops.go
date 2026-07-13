package patrol

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
)

// Mode selects which accounts to probe.
// full = currently enabled xAI only
// cooldown = plugin_auto cooling accounts only
type Mode string

const (
	ModeFull     Mode = "full"
	ModeCooldown Mode = "cooldown"
)

type Status struct {
	Running    bool      `json:"running"`
	Mode       string    `json:"mode"`
	StartedAt  string    `json:"started_at,omitempty"`
	FinishedAt string    `json:"finished_at,omitempty"`
	Total      int       `json:"total"`
	Probed     int       `json:"probed"`
	Alive      int       `json:"alive"`
	Cooldown   int       `json:"cooldown"`
	Errors     int       `json:"errors"`
	Skipped    int       `json:"skipped"`
	Message    string    `json:"message,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	Logs       []LogLine `json:"logs,omitempty"`
	// ID is assigned when job finishes (history) or "current" while running.
	ID string `json:"id,omitempty"`
}

type LogLine struct {
	At     string `json:"at"`
	Auth   string `json:"auth,omitempty"`
	Label  string `json:"label,omitempty"`
	Code   int    `json:"code,omitempty"`
	Level  string `json:"level"` // ok|warn|err|info
	Text   string `json:"text"`
	Action string `json:"action,omitempty"`
}

var (
	jobMu      sync.Mutex
	jobStatus  = Status{Message: "空闲"}
	jobLogs    []LogLine
	jobHistory []Status // newest first
	jobSeq     int
)

const maxJobHistory = 30

func (r *Runner) Status() Status {
	jobMu.Lock()
	defer jobMu.Unlock()
	st := jobStatus
	if len(jobLogs) > 0 {
		st.Logs = append([]LogLine(nil), jobLogs...)
	}
	return st
}

// History returns recent finished jobs (newest first), each with embedded logs.
func (r *Runner) History() []Status {
	jobMu.Lock()
	defer jobMu.Unlock()
	out := make([]Status, len(jobHistory))
	copy(out, jobHistory)
	return out
}

func (r *Runner) Start(ctx context.Context, mode Mode) (Status, error) {
	jobMu.Lock()
	if jobStatus.Running {
		st := jobStatus
		jobMu.Unlock()
		return st, nil
	}
	if mode == "" {
		mode = ModeFull
	}
	jobSeq++
	id := "job-" + itoa(jobSeq)
	jobStatus = Status{
		Running:   true,
		Mode:      string(mode),
		ID:        id,
		StartedAt: time.Now().In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04:05"),
		Message:   "巡查进行中",
	}
	jobLogs = nil
	jobMu.Unlock()

	go r.runJob(context.Background(), mode)
	return r.Status(), nil
}

func (r *Runner) runJob(ctx context.Context, mode Mode) {
	defer func() {
		jobMu.Lock()
		jobStatus.Running = false
		jobStatus.FinishedAt = time.Now().In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04:05")
		if jobStatus.Message == "巡查进行中" {
			jobStatus.Message = "巡查完成"
		}
		// snapshot into history
		h := jobStatus
		if len(jobLogs) > 0 {
			h.Logs = append([]LogLine(nil), jobLogs...)
		}
		jobHistory = append([]Status{h}, jobHistory...)
		if len(jobHistory) > maxJobHistory {
			jobHistory = jobHistory[:maxJobHistory]
		}
		jobMu.Unlock()
	}()

	if r == nil || r.Guard == nil {
		r.appendLog("", "", 0, "err", "runtime 未就绪", "")
		jobMu.Lock()
		jobStatus.LastError = "runtime 未就绪"
		jobStatus.Message = "失败"
		jobMu.Unlock()
		return
	}
	// refresh cfg from guard
	r.Cfg = r.Guard.Cfg
	if r.Guard.Resolver != nil {
		_ = r.Guard.Resolver.Ensure(ctx)
	}

	// Align CPA-disabled free accounts that sentry still shows as Active.
	// Full patrol only probes enabled files, so already-disabled 429s would be skipped forever.
	aligned := r.alignDisabledQuotaAccounts(ctx, mode)

	targets := r.collectTargets(ctx, mode)
	jobMu.Lock()
	jobStatus.Total = len(targets) + aligned
	jobMu.Unlock()
	r.appendLog("", "", 0, "info", "开始巡查 · 模式="+string(mode)+" · 探测目标="+itoa(len(targets))+" · 已禁用额度对齐="+itoa(aligned), "start")
	if r.Guard.State != nil {
		r.Guard.State.Log(state.ActionLog{
			Source: "patrol", Action: "patrol_start",
			Reason: "mode=" + string(mode) + " probe=" + itoa(len(targets)) + " align_disabled_quota=" + itoa(aligned),
		})
	}
	if len(targets) == 0 && aligned == 0 {
		r.appendLog("", "", 0, "warn", "没有可巡查目标（全量只扫已启用；仅冷却只扫冷却号；也无已禁用待对齐额度号）", "")
		return
	}

	// run probes (existing Run feeds HandleUsage)
	results := r.Run(ctx, targets)
	alive, cool, errs := 0, aligned, 0
	for _, res := range results {
		label := res.AuthIndex
		// find target label
		for _, t := range targets {
			if t.AuthIndex == res.AuthIndex {
				if t.Email != "" {
					label = t.Email
				} else if t.FileName != "" {
					label = t.FileName
				}
				break
			}
		}
		if res.Err != "" {
			errs++
			r.appendLog(res.AuthIndex, label, res.StatusCode, "err", "探测失败："+res.Err, "probe_error")
			continue
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			alive++
			r.appendLog(res.AuthIndex, label, res.StatusCode, "ok", "探测存活", "alive")
			continue
		}
		body := strings.ToLower(res.Body)
		if res.StatusCode == 429 || strings.Contains(body, "free-usage") {
			cool++
			r.appendLog(res.AuthIndex, label, res.StatusCode, "warn", "探测到免费额度用尽，已交策略处理", "cooldown")
			continue
		}
		if res.StatusCode == 402 {
			cool++
			r.appendLog(res.AuthIndex, label, res.StatusCode, "warn", "探测到消费限额，已交策略处理", "cooldown")
			continue
		}
		if res.StatusCode == 401 || res.StatusCode == 403 {
			r.appendLog(res.AuthIndex, label, res.StatusCode, "warn", "探测到权限/凭证异常 HTTP "+itoa(res.StatusCode), "signal")
			continue
		}
		if res.StatusCode >= 400 {
			hint := strings.TrimSpace(res.Body)
			if len(hint) > 120 {
				hint = hint[:120]
			}
			if strings.Contains(strings.ToLower(hint), "<html") {
				hint = "路径/网关 404（请检查 base_url 是否已含 /v1 被重复拼接）"
			}
			msg := "探测返回 HTTP " + itoa(res.StatusCode)
			if hint != "" {
				msg += " · " + hint
			}
			errs++
			r.appendLog(res.AuthIndex, label, res.StatusCode, "err", msg, "http_error")
			continue
		}
		r.appendLog(res.AuthIndex, label, res.StatusCode, "info", "探测完成 HTTP "+itoa(res.StatusCode), "done")
	}
	msg := "完成：探测" + itoa(len(results)) + " · 存活" + itoa(alive) + " · 冷却信号" + itoa(cool) + " · 异常" + itoa(errs)
	jobMu.Lock()
	jobStatus.Probed = len(results)
	jobStatus.Alive = alive
	jobStatus.Cooldown = cool
	jobStatus.Errors = errs
	jobStatus.Message = msg
	jobMu.Unlock()
	if r.Guard.State != nil {
		r.Guard.State.Log(state.ActionLog{
			Source: "patrol", Action: "patrol_done",
			Reason: msg,
		})
		_ = r.Guard.State.Save()
	}
	r.appendLog("", "", 0, "info", msg, "done")
}


// alignDisabledQuotaAccounts finds xAI auth files that are CPA-disabled while
// sentry state is still Active (typical free-usage 429 already disabled by CPA
// or another tool). Feeds a synthetic free_usage_429 into Guard so state becomes
// cooldown_quota without needing a live probe (full patrol skips disabled files).
func (r *Runner) alignDisabledQuotaAccounts(ctx context.Context, mode Mode) int {
	if r.Guard == nil || r.CPA == nil || mode != ModeFull {
		return 0
	}
	files, err := r.CPA.ListAuthFiles(ctx)
	if err != nil || len(files) == 0 {
		return 0
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
		em := strings.TrimSpace(f.Email)
		if em == "" {
			em = strings.TrimSpace(f.Account)
		}
		ai := strings.TrimSpace(f.AuthIndex)
		var acc *state.Account
		if r.Guard.State != nil {
			if ai != "" {
				acc = r.Guard.State.Get(ai)
			}
			if acc == nil {
				for _, a := range r.Guard.State.AccountsSnapshot() {
					if (em != "" && strings.EqualFold(a.Email, em)) ||
						(name != "" && strings.EqualFold(a.FileName, name)) {
						acc = a
						break
					}
				}
			}
			if acc == nil {
				acc = r.Guard.State.Get(name)
			}
		}
		if acc != nil {
			switch acc.State {
			case state.CooldownQuota, state.CooldownSpending, state.CooldownPermission, state.CandidateDead, state.UserManual, state.Trashed, state.Purged:
				continue
			}
			if acc.DisableSource == "plugin_auto" || acc.DisableSource == "user_manual" || acc.DisableSource == "cpa_file_disabled" {
				continue
			}
			// do not rebrand 401/403 accounts as free-usage cool-down
			switch acc.LastSignal {
			case "permission_403", "auth_401", "spending_limit_402":
				continue
			}
		}
		auth := ai
		if auth == "" && acc != nil {
			auth = acc.AuthIndex
		}
		if auth == "" {
			auth = name
		}
		// Direct cool-down stamp (do not rely on HTTP probe). File is already disabled.
		if r.Guard.State != nil {
			r.Guard.State.Touch(auth)
			if name != "" || em != "" {
				r.Guard.State.UpdateMeta(auth, name, em, "")
			}
			// 24h free-usage cool-down window
			recoverAt := time.Now().Add(24 * time.Hour)
			if r.Cfg.MaxResetSeconds > 0 {
				maxAt := time.Now().Add(time.Duration(r.Cfg.MaxResetSeconds) * time.Second)
				if recoverAt.After(maxAt) {
					recoverAt = maxAt
				}
			}
			r.Guard.State.SetAccountState(auth, state.CooldownQuota, "plugin_auto")
			r.Guard.State.SetRecoverAt(auth, recoverAt)
			r.Guard.State.SetLastSignal(auth, "free_usage_429")
			r.Guard.State.IncStreak(auth, "free_usage_429")
			r.Guard.State.Log(state.ActionLog{
				Auth: auth, Source: "patrol", Signal: "free_usage_429",
				Action: "cooldown", Reason: "align_disabled_quota",
			})
			_ = r.Guard.State.Save()
		}
		label := em
		if label == "" {
			label = name
		}
		r.appendLog(auth, label, 429, "warn", "CPA 已禁用且哨兵仍为正常 → 按免费额度用尽对齐冷却（全量巡检不探测已禁用号）", "align_disabled_quota")
		n++
	}
	return n
}

func (r *Runner) collectTargets(ctx context.Context, mode Mode) []Target {
	out := make([]Target, 0, 64)
	// map sentry cooling accounts by email/file/auth
	cooling := map[string]*state.Account{}
	if r.Guard != nil && r.Guard.State != nil {
		for _, acc := range r.Guard.State.AccountsSnapshot() {
			if acc.State == state.CooldownQuota || acc.State == state.CooldownSpending || acc.State == state.CooldownPermission {
				if acc.DisableSource == "user_manual" {
					continue
				}
				cooling[acc.AuthIndex] = acc
				if acc.Email != "" {
					cooling[strings.ToLower(acc.Email)] = acc
				}
				if acc.FileName != "" {
					cooling[strings.ToLower(acc.FileName)] = acc
				}
			}
		}
	}
	// identities from resolver (preferred) or CPA list + dir
	var ids []cpaapi.Identity
	if r.Guard != nil && r.Guard.Resolver != nil {
		_ = r.Guard.Resolver.Ensure(ctx)
		ids = r.Guard.Resolver.All()
	}
	seenTarget := map[string]bool{}
	for _, id := range ids {
		if !cpaapi.IsXAIName(id.FileName, id.Provider) {
			continue
		}
		// need token to probe
		tok := id.Token
		base := id.BaseURL
		if tok == "" && r.CPA != nil && id.FileName != "" {
			if raw, err := r.CPA.ReadAuthFileFromDir(id.FileName); err == nil {
				_, _, _, t, b, dis := cpaapi.PeekAuthMeta(raw)
				tok, base = t, b
				id.Disabled = dis
			}
		}
		if tok == "" {
			continue
		}
		switch mode {
		case ModeFull:
			if id.Disabled {
				continue // only enabled
			}
		case ModeCooldown:
			// only cooling accounts
			_, ok1 := cooling[id.AuthIndex]
			_, ok2 := cooling[strings.ToLower(id.Email)]
			_, ok3 := cooling[strings.ToLower(id.FileName)]
			if !ok1 && !ok2 && !ok3 {
				// also allow disabled files as cooldown candidates
				if !id.Disabled {
					continue
				}
			}
		}
		authIdx := id.AuthIndex
		// prefer hashed/runtime auth index if sentry already knows this email/file
		if r.Guard != nil && r.Guard.State != nil {
			for _, acc := range r.Guard.State.AccountsSnapshot() {
				if (id.Email != "" && strings.EqualFold(acc.Email, id.Email)) ||
					(id.FileName != "" && strings.EqualFold(acc.FileName, id.FileName)) {
					authIdx = acc.AuthIndex
					break
				}
			}
		}
		dk := strings.ToLower(strings.TrimSpace(id.Email))
		if dk == "" {
			dk = strings.ToLower(strings.TrimSpace(id.FileName))
		}
		if dk == "" {
			dk = strings.ToLower(strings.TrimSpace(authIdx))
		}
		if seenTarget[dk] {
			continue
		}
		seenTarget[dk] = true
		out = append(out, Target{
			AuthIndex: authIdx,
			FileName:  id.FileName,
			Email:     id.Email,
			Provider:  firstNonEmpty(id.Provider, "xai"),
			Note:      id.Note,
			Label:     id.Label,
			BaseURL:   base,
			Token:     tok,
		})
	}
	// batch limit
	batch := r.Cfg.PatrolBatchSize
	if batch > 0 && len(out) > batch {
		out = out[:batch]
	}
	return out
}

func (r *Runner) appendLog(auth, label string, code int, level, text, action string) {
	jobMu.Lock()
	defer jobMu.Unlock()
	line := LogLine{
		At: time.Now().In(time.FixedZone("CST", 8*3600)).Format("15:04:05"),
		Auth: auth, Label: label, Code: code, Level: level, Text: text, Action: action,
	}
	jobLogs = append(jobLogs, line)
	if len(jobLogs) > 300 {
		jobLogs = jobLogs[len(jobLogs)-300:]
	}
	jobStatus.Logs = append([]LogLine(nil), jobLogs...)
	if level == "err" {
		jobStatus.LastError = text
	}
}

// bumpPatrolProgress updates live counters for the panel progress bar.
func bumpPatrolProgress(res Result) {
	jobMu.Lock()
	defer jobMu.Unlock()
	if !jobStatus.Running {
		return
	}
	jobStatus.Probed++
	if res.Err != "" {
		jobStatus.Errors++
		return
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		jobStatus.Alive++
		return
	}
	body := strings.ToLower(res.Body)
	if res.StatusCode == 429 || res.StatusCode == 402 || strings.Contains(body, "free-usage") || strings.Contains(body, "spending-limit") {
		jobStatus.Cooldown++
		return
	}
	if res.StatusCode >= 400 {
		jobStatus.Errors++
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [16]byte
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

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
