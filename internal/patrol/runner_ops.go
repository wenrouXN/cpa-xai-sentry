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

	// Full = sentry-not-disabled accounts (still schedulable). Real HTTP only.
	// Cooldown mode = sentry cool-down accounts. No synthetic disabled→429.
	targets := r.collectTargets(ctx, mode)
	jobMu.Lock()
	jobStatus.Total = len(targets)
	jobMu.Unlock()
	r.appendLog("", "", 0, "info", "开始巡查 · 模式="+string(mode)+" · 探测目标="+itoa(len(targets)), "start")
	if r.Guard.State != nil {
		r.Guard.State.Log(state.ActionLog{
			Source: "patrol", Action: "patrol_start",
			Reason: "mode=" + string(mode) + " probe=" + itoa(len(targets)),
		})
	}
	if len(targets) == 0 {
		r.appendLog("", "", 0, "warn", "没有可巡查目标（全量=哨兵未禁用且可接流；仅冷却=哨兵冷却中）", "")
		return
	}

	// run probes (existing Run feeds HandleUsage with real status/body)
	results := r.Run(ctx, targets)
	alive, cool, sig, errs := 0, 0, 0, 0
	// cool = free-usage/402 cool-down signals from real probe
	// sig  = 401/403 auth signals (fed to policy)
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
			sig++
			r.appendLog(res.AuthIndex, label, res.StatusCode, "warn", "探测到权限/凭证异常 HTTP "+itoa(res.StatusCode)+"，已交策略处理", "signal")
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
		// unexpected 3xx etc. — count as other so totals add up
		errs++
		r.appendLog(res.AuthIndex, label, res.StatusCode, "info", "探测完成 HTTP "+itoa(res.StatusCode), "done")
	}
	probeN := len(results)
	msg := "完成：探测" + itoa(probeN) + " · 存活" + itoa(alive) + " · 冷却信号" + itoa(cool) + " · 权限信号" + itoa(sig) + " · 异常" + itoa(errs)
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


func (r *Runner) collectTargets(ctx context.Context, mode Mode) []Target {
	out := make([]Target, 0, 64)
	// Sentry-side "disabled for traffic" = cool-down / 候删 / 永久禁用 / 垃圾箱.
	// Full patrol = NOT these (哨兵未禁用、仍可能接流). Real HTTP probe; no synthetic 429.
	blocked := map[string]*state.Account{} // by auth/email/file
	cooling := map[string]*state.Account{}
	if r.Guard != nil && r.Guard.State != nil {
		for _, acc := range r.Guard.State.AccountsSnapshot() {
			keys := []string{}
			if acc.AuthIndex != "" {
				keys = append(keys, strings.ToLower(strings.TrimSpace(acc.AuthIndex)))
			}
			if acc.Email != "" {
				keys = append(keys, strings.ToLower(strings.TrimSpace(acc.Email)))
			}
			if acc.FileName != "" {
				keys = append(keys, strings.ToLower(strings.TrimSpace(acc.FileName)))
			}
			isCool := acc.State == state.CooldownQuota || acc.State == state.CooldownSpending || acc.State == state.CooldownPermission
			// Full-patrol "sentry disabled" = not schedulable for new traffic.
			isBlocked := isCool ||
				acc.State == state.CandidateDead ||
				acc.State == state.UserManual ||
				acc.State == state.Trashed ||
				acc.State == state.Purged ||
				acc.DisableSource == "user_manual"
			// Active + residual plugin_auto tag remains probeable
			if isCool {
				for _, k := range keys {
					cooling[k] = acc
				}
			}
			if isBlocked {
				for _, k := range keys {
					blocked[k] = acc
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
		// need token to probe (works even if CPA file is disabled)
		tok := id.Token
		base := id.BaseURL
		if tok == "" && r.CPA != nil && id.FileName != "" {
			if raw, err := r.CPA.ReadAuthFileFromDir(id.FileName); err == nil {
				_, _, _, tk, b, dis := cpaapi.PeekAuthMeta(raw)
				tok, base = tk, b
				id.Disabled = dis
			}
		}
		if tok == "" {
			continue
		}
		authIdx := id.AuthIndex
		// prefer hashed/runtime auth index if sentry already knows this email/file
		var sentryAcc *state.Account
		if r.Guard != nil && r.Guard.State != nil {
			for _, acc := range r.Guard.State.AccountsSnapshot() {
				if (id.Email != "" && strings.EqualFold(acc.Email, id.Email)) ||
					(id.FileName != "" && strings.EqualFold(acc.FileName, id.FileName)) ||
					(id.AuthIndex != "" && strings.EqualFold(acc.AuthIndex, id.AuthIndex)) {
					authIdx = acc.AuthIndex
					sentryAcc = acc
					break
				}
			}
		}
		emKey := strings.ToLower(strings.TrimSpace(id.Email))
		fnKey := strings.ToLower(strings.TrimSpace(id.FileName))
		aiKey := strings.ToLower(strings.TrimSpace(authIdx))
		switch mode {
		case ModeFull:
			// 全量 = 哨兵侧未禁用（可接流）：Active / 无状态；不含冷却/候删/永禁/垃圾箱
			// CPA 文件是否 disabled 不作为排除条件 — 有 token 就实探，结果按真实 HTTP 走策略
			if sentryAcc != nil {
				if _, ok := blocked[strings.ToLower(strings.TrimSpace(sentryAcc.AuthIndex))]; ok {
					continue
				}
				if emKey != "" {
					if _, ok := blocked[emKey]; ok {
						continue
					}
				}
				if fnKey != "" {
					if _, ok := blocked[fnKey]; ok {
						continue
					}
				}
			} else {
				// no sentry row: only skip if listed as blocked under email/file/auth keys
				if (aiKey != "" && blocked[aiKey] != nil) || (emKey != "" && blocked[emKey] != nil) || (fnKey != "" && blocked[fnKey] != nil) {
					continue
				}
			}
		case ModeCooldown:
			// 仅冷却 = 哨兵冷却中的号（可含 CPA disabled）
			ok := false
			if aiKey != "" && cooling[aiKey] != nil {
				ok = true
			}
			if emKey != "" && cooling[emKey] != nil {
				ok = true
			}
			if fnKey != "" && cooling[fnKey] != nil {
				ok = true
			}
			if !ok {
				continue
			}
		}
		dk := emKey
		if dk == "" {
			dk = fnKey
		}
		if dk == "" {
			dk = aiKey
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
