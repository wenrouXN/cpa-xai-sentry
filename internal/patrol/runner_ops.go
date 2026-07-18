package patrol

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
)

// Mode selects which accounts to probe.
//
//	all        = every xAI with a token (any sentry state)
//	enabled    = sentry not cool/候删/永禁/垃圾箱 (旧名 full)
//	cooldown   = sentry cool-down only
//	permanent  = sentry permanent disable (user_manual) only
//	candidate  = sentry 候删 (candidate_dead) only
//	trash      = sentry 垃圾箱 (trashed) only
type Mode string

const (
	ModeAll       Mode = "all"
	ModeEnabled   Mode = "enabled" // legacy alias: "full"
	ModeFull      Mode = "full"    // = enabled (kept for old clients)
	ModeCooldown  Mode = "cooldown"
	ModePermanent Mode = "permanent"
	ModeCandidate Mode = "candidate"
	ModeTrash     Mode = "trash"
)

// ParseMode normalizes user/API input to a known Mode. Unknown → enabled.
func ParseMode(s string) Mode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "all", "全部", "any":
		return ModeAll
	case "enabled", "full", "active", "启用", "可接流":
		return ModeEnabled
	case "cooldown", "cool", "spending", "冷却":
		return ModeCooldown
	case "permanent", "manual", "disabled", "user_manual", "永久禁用", "永禁":
		return ModePermanent
	case "candidate", "候删", "候选", "candidate_dead":
		return ModeCandidate
	case "trash", "trashed", "垃圾箱", "箱":
		return ModeTrash
	default:
		return ModeEnabled
	}
}

func ModeLabel(m Mode) string {
	switch ParseMode(string(m)) {
	case ModeAll:
		return "全部"
	case ModeCooldown:
		return "冷却"
	case ModePermanent:
		return "永久禁用"
	case ModeCandidate:
		return "候删"
	case ModeTrash:
		return "垃圾箱"
	default:
		return "启用"
	}
}

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

const maxJobHistory = 10

// historyPath is set once from sentry state dir so job history survives restarts.
var historyPath string

// SetHistoryPath configures durable patrol job history file (call at runtime rebuild).
func SetHistoryPath(path string) {
	jobMu.Lock()
	defer jobMu.Unlock()
	historyPath = path
	if path == "" {
		return
	}
	// load existing history if empty
	if len(jobHistory) == 0 {
		if hist, err := loadHistoryFile(path); err == nil && len(hist) > 0 {
			jobHistory = hist
			if len(jobHistory) > maxJobHistory {
				jobHistory = jobHistory[:maxJobHistory]
			}
		}
	}
}

func historyFilePath() string {
	return historyPath
}

func loadHistoryFile(path string) ([]Status, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, nil
	}
	var hist []Status
	if err := json.Unmarshal(b, &hist); err != nil {
		return nil, err
	}
	return hist, nil
}

func saveHistoryLocked() {
	path := historyPath
	if path == "" {
		return
	}
	// copy
	out := make([]Status, len(jobHistory))
	copy(out, jobHistory)
	// unlock not held? caller holds jobMu
	go func(p string, h []Status) {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		b, err := json.Marshal(h)
		if err != nil {
			return
		}
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return
		}
		_ = os.Rename(tmp, p)
	}(path, out)
}

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

// HistoryPage returns a paginated slice of recent finished jobs.
// limit=0 means default 10, max 50. offset is 0-based.
// maxLogs caps logs per job (0 = no logs, -1 = all logs).
func (r *Runner) HistoryPage(limit, offset, maxLogs int) ([]Status, int) {
	jobMu.Lock()
	defer jobMu.Unlock()
	total := len(jobHistory)
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return []Status{}, total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	out := make([]Status, end-offset)
	for i, j := offset, 0; i < end; i, j = i+1, j+1 {
		s := jobHistory[i]
		if maxLogs >= 0 && len(s.Logs) > maxLogs {
			s.Logs = s.Logs[:maxLogs]
		}
		out[j] = s
	}
	return out, total
}

func (r *Runner) Start(ctx context.Context, mode Mode) (Status, error) {
	jobMu.Lock()
	if jobStatus.Running {
		st := jobStatus
		jobMu.Unlock()
		return st, nil
	}
	if mode == "" {
		mode = ModeEnabled
	}
	mode = ParseMode(string(mode))
	jobSeq++
	// Unique across process restarts: timestamp + seq (old "job-1" collided after reload
	// and UI dropped history entries sharing the same id).
	id := "job-" + time.Now().In(time.FixedZone("CST", 8*3600)).Format("20060102-150405") + "-" + itoa(jobSeq)
	jobStatus = Status{
		Running:   true,
		Mode:      string(mode),
		ID:        id,
		StartedAt: time.Now().In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04:05"),
		Message:   "巡查进行中 · " + ModeLabel(mode),
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
		r.patrolFinishedAt = time.Now()
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
		saveHistoryLocked()
		jobMu.Unlock()
	}()

	// Refresh Guard reference from Runtime to avoid stale pointer after rebuild.
	if r.GetGuard != nil {
		if fresh := r.GetGuard(); fresh != nil {
			r.Guard = fresh
		}
	}
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
	r.appendLog("", "", 0, "info", "开始巡查 · 模式="+ModeLabel(mode)+"("+string(mode)+") · 探测目标="+itoa(len(targets)), "start")
	if r.Guard.State != nil {
		r.Guard.State.Log(state.ActionLog{
			Source: "patrol", Action: "patrol_start",
			Reason: "mode=" + string(mode) + " label=" + ModeLabel(mode) + " probe=" + itoa(len(targets)),
		})
	}
	if len(targets) == 0 {
		r.appendLog("", "", 0, "warn", "没有可巡查目标（模式="+ModeLabel(mode)+"：全部/启用/冷却/永禁/候删/垃圾箱）", "")
		return
	}

	// run probes (existing Run feeds HandleUsage with real status/body;
	// each probe also appends job log live via appendProbeResultLive)
	results := r.RunMode(ctx, targets, mode)
	alive, cool, sig, errs := 0, 0, 0, 0
	// cool = free-usage/402 cool-down signals from real probe
	// sig  = 401/403 auth signals (fed to policy)
	// recount from results for final summary (live counters already bumped)
	for _, res := range results {
		if res.Err != "" {
			errs++
			continue
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			alive++
			continue
		}
		body := strings.ToLower(res.Body)
		if res.StatusCode == 429 || strings.Contains(body, "free-usage") {
			cool++
			continue
		}
		if res.StatusCode == 402 {
			cool++
			continue
		}
		if res.StatusCode == 401 || res.StatusCode == 403 {
			sig++
			continue
		}
		if res.StatusCode >= 400 {
			errs++
			continue
		}
		errs++
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
	mode = ParseMode(string(mode))
	out := make([]Target, 0, 64)
	// Sentry-side buckets for mode filters.
	blocked := map[string]*state.Account{}    // cool/候删/永禁/垃圾箱 — not "enabled"
	cooling := map[string]*state.Account{}    // cool-down only
	permanent := map[string]*state.Account{}  // user_manual permanent only
	candidates := map[string]*state.Account{} // 候删
	trashed := map[string]*state.Account{}    // 垃圾箱
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
			isCool := state.IsCooldownState(acc.State)
			isPerm := acc.State == state.UserManual || acc.DisableSource == "user_manual"
			isCand := acc.State == state.CandidateDead
			isTrash := acc.State == state.Trashed || acc.State == state.Purged
			// "enabled" mode excludes anything not schedulable for new traffic
			isBlocked := isCool || isCand || isPerm || isTrash
			if isCool {
				for _, k := range keys {
					cooling[k] = acc
				}
			}
			if isPerm {
				for _, k := range keys {
					permanent[k] = acc
				}
			}
			if isCand {
				for _, k := range keys {
					candidates[k] = acc
				}
			}
			if isTrash {
				for _, k := range keys {
					trashed[k] = acc
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
	inMap := func(m map[string]*state.Account, aiKey, emKey, fnKey string) bool {
		if aiKey != "" && m[aiKey] != nil {
			return true
		}
		if emKey != "" && m[emKey] != nil {
			return true
		}
		if fnKey != "" && m[fnKey] != nil {
			return true
		}
		return false
	}
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
		if sentryAcc != nil {
			aiKey = strings.ToLower(strings.TrimSpace(sentryAcc.AuthIndex))
		}
		switch mode {
		case ModeAll:
			// every xAI with token — no state filter
		case ModeEnabled, ModeFull:
			// 启用 = 哨兵侧未禁用（可接流）：Active / 无状态；不含冷却/候删/永禁/垃圾箱
			// CPA 文件是否 disabled 不作为排除条件 — 有 token 就实探
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
			} else if inMap(blocked, aiKey, emKey, fnKey) {
				continue
			}
		case ModeCooldown:
			if !inMap(cooling, aiKey, emKey, fnKey) {
				continue
			}
		case ModePermanent:
			if !inMap(permanent, aiKey, emKey, fnKey) {
				continue
			}
		case ModeCandidate:
			if !inMap(candidates, aiKey, emKey, fnKey) {
				continue
			}
		case ModeTrash:
			if !inMap(trashed, aiKey, emKey, fnKey) {
				continue
			}
		default:
			// same as enabled
			if inMap(blocked, aiKey, emKey, fnKey) {
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
		At:   time.Now().In(time.FixedZone("CST", 8*3600)).Format("15:04:05"),
		Auth: auth, Label: label, Code: code, Level: level, Text: text, Action: action,
	}
	jobLogs = append(jobLogs, line)
	if len(jobLogs) > 500 {
		jobLogs = jobLogs[len(jobLogs)-500:]
	}
	jobStatus.Logs = append([]LogLine(nil), jobLogs...)
	if level == "err" {
		jobStatus.LastError = text
	}
}

// appendProbeResultLive writes one job log line as soon as a single probe finishes.
// Shared by full + cooldown modes (called from Run for every target).
func (r *Runner) appendProbeResultLive(t Target, res Result) {
	label := t.Email
	if label == "" {
		label = t.FileName
	}
	if label == "" {
		label = res.AuthIndex
	}
	if res.Err != "" {
		r.appendLog(res.AuthIndex, label, res.StatusCode, "err", "探测失败："+res.Err, "probe_error")
		return
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		r.appendLog(res.AuthIndex, label, res.StatusCode, "ok", "探测存活 · 已交策略/可打开", "alive")
		return
	}
	body := strings.ToLower(res.Body)
	if res.StatusCode == 429 || strings.Contains(body, "free-usage") {
		r.appendLog(res.AuthIndex, label, res.StatusCode, "warn", "探测到免费额度用尽，已交策略处理", "cooldown")
		return
	}
	if res.StatusCode == 402 {
		r.appendLog(res.AuthIndex, label, res.StatusCode, "warn", "探测到消费限额，已交策略处理", "cooldown")
		return
	}
	if res.StatusCode == 401 || res.StatusCode == 403 {
		r.appendLog(res.AuthIndex, label, res.StatusCode, "warn", "探测到权限/凭证异常 HTTP "+itoa(res.StatusCode)+"，已交策略处理", "signal")
		return
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
		r.appendLog(res.AuthIndex, label, res.StatusCode, "err", msg, "http_error")
		return
	}
	r.appendLog(res.AuthIndex, label, res.StatusCode, "info", "探测完成 HTTP "+itoa(res.StatusCode), "done")
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
