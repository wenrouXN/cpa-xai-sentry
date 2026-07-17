// Package regjob tracks register-lite jobs, health, and optional auto register.
package regjob

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/registerlite"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
)

type Job struct {
	ID             string         `json:"id"`
	Source         string         `json:"source"` // panel|auto|floor
	Epoch          int            `json:"epoch,omitempty"` // success-window epoch at job start
	Status         string         `json:"status"` // running|done|failed|stopped
	CountRequested int            `json:"count_requested"`
	Total          int            `json:"total"`
	Done           int            `json:"done"`
	Success        int            `json:"success"`
	Failed         int            `json:"failed"`
	BatchID        string         `json:"batch_id,omitempty"`
	Message        string         `json:"message,omitempty"`
	StartedAt      string         `json:"started_at,omitempty"`
	FinishedAt     string         `json:"finished_at,omitempty"`
	Error          string         `json:"error,omitempty"`
	HealthSnapshot map[string]any `json:"health_snapshot,omitempty"`
	Logs           []LogLine      `json:"logs,omitempty"`
}

type LogLine struct {
	At    string `json:"at"`
	Level string `json:"level"` // info|ok|warn|err
	Text  string `json:"text"`
}

type SuccessWindow struct {
	Level      string  `json:"level"` // ok|warn|error|unknown
	Rate       float64 `json:"rate"`
	Success    int     `json:"success"`
	Total      int     `json:"total"`
	Jobs       int     `json:"jobs"`
	Label      string  `json:"label"`
	SampleOK   bool    `json:"sample_ok"`
	MinSamples int     `json:"min_samples"`
}

type Runner struct {
	mu      sync.Mutex
	Cfg     sentrycfg.Config
	Client  *registerlite.Client
	current *Job
	history []Job
	seq     int
	histPath string

	lastHealth      registerlite.Health
	lastHealthAt    time.Time
	autoPausedUntil time.Time
	lastAutoAt      time.Time
	nextAutoAt      time.Time
	Logf            func(level, msg string) // optional action log bridge

	// 8788 inventory for relogin eligibility
	emailCache   map[string]bool
	emailCacheAt time.Time
	reloginStreak map[string]int
	reloginLastAt map[string]time.Time

	// PoolCounter: returns (enabled, cooldown) for floor gate. Set by runtime.
	PoolCounter func(ctx context.Context) (enabled, cooldown int)
	lastFloorAt time.Time
	nextFloorAt time.Time
	successEpoch   int
}

const maxHistory = 30

func New(cfg sentrycfg.Config) *Runner {
	r := &Runner{Cfg: cfg}
	r.Client = registerlite.New(cfg.RegisterBaseURL, cfg.RegisterAdminBase, cfg.RegisterPassword, cfg.RegisterTimeoutSec)
	return r
}

func (r *Runner) ApplyConfig(cfg sentrycfg.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Cfg = cfg
	if r.Client == nil {
		r.Client = registerlite.New(cfg.RegisterBaseURL, cfg.RegisterAdminBase, cfg.RegisterPassword, cfg.RegisterTimeoutSec)
	} else {
		r.Client.Configure(cfg.RegisterBaseURL, cfg.RegisterAdminBase, cfg.RegisterPassword, cfg.RegisterTimeoutSec)
	}
}

func (r *Runner) SetHistoryPath(path string) {
	r.mu.Lock()
	r.histPath = path
	var resumeID, resumeBatch string
	if path != "" && len(r.history) == 0 {
		if hist, err := loadHistory(path); err == nil {
			r.history = hist
			if len(r.history) > maxHistory {
				r.history = r.history[:maxHistory]
			}
		}
	}
	// resume unfinished job after restart
	if path != "" && r.current == nil {
		for i := range r.history {
			if r.history[i].Status == "running" {
				cp := r.history[i]
				r.current = &cp
				resumeID, resumeBatch = cp.ID, cp.BatchID
				break
			}
		}
	}
	r.mu.Unlock()
	if resumeID != "" {
		r.log("info", "【注册】恢复轮询 · job="+resumeID+" · batch="+resumeBatch)
		go r.pollUntilDone(resumeID, resumeBatch)
	}
}

func loadHistory(path string) ([]Job, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, nil
	}
	var hist []Job
	if err := json.Unmarshal(b, &hist); err != nil {
		return nil, err
	}
	return hist, nil
}

func (r *Runner) saveHistoryLocked() {
	path := r.histPath
	if path == "" {
		return
	}
	out := make([]Job, len(r.history))
	copy(out, r.history)
	go func(p string, h []Job) {
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

func (r *Runner) log(level, msg string) {
	if r.Logf != nil {
		r.Logf(level, msg)
	}
}

func nowCST() string {
	return time.Now().In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04:05")
}

func (r *Runner) appendJobLog(id, level, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil || r.current.ID != id {
		return
	}
	r.current.Logs = append(r.current.Logs, LogLine{At: nowCST(), Level: level, Text: text})
	if len(r.current.Logs) > 200 {
		r.current.Logs = r.current.Logs[len(r.current.Logs)-200:]
	}
	// also mirror latest message
	if text != "" {
		r.current.Message = text
	}
}

func (r *Runner) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current != nil && r.current.Status == "running"
}

func (r *Runner) Current() *Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil {
		return nil
	}
	cp := *r.current
	return &cp
}

func (r *Runner) History() []Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Job, len(r.history))
	copy(out, r.history)
	return out
}

func (r *Runner) SuccessWindow() SuccessWindow {
	r.mu.Lock()
	cfg := r.Cfg
	hist := append([]Job(nil), r.history...)
	r.mu.Unlock()

	win := cfg.RegisterHealthWindowJobs
	if win <= 0 {
		win = 10
	}
	minS := cfg.RegisterHealthMinSamples
	if minS <= 0 {
		minS = 10
	}
	okRate := cfg.RegisterHealthOKRate
	if okRate <= 0 {
		okRate = 0.85
	}
	warnRate := cfg.RegisterHealthWarnRate
	if warnRate <= 0 {
		warnRate = 0.60
	}

	sw := SuccessWindow{Level: "unknown", MinSamples: minS, Label: "样本不足"}
	n := 0
	succ, total := 0, 0
	for _, j := range hist {
		if j.Status == "running" {
			continue
		}
		// only finished register jobs
		t := j.Total
		if t <= 0 {
			t = j.CountRequested
		}
		if t <= 0 && j.Success+j.Failed > 0 {
			t = j.Success + j.Failed
		}
		if t <= 0 {
			continue
		}
		succ += j.Success
		total += t
		n++
		if n >= win {
			break
		}
	}
	sw.Jobs = n
	sw.Success = succ
	sw.Total = total
	if total < minS {
		sw.SampleOK = false
		sw.Label = fmt.Sprintf("样本不足（%d/%d 号）", total, minS)
		return sw
	}
	sw.SampleOK = true
	sw.Rate = float64(succ) / float64(total)
	pct := int(sw.Rate*100 + 0.5)
	sw.Label = fmt.Sprintf("%d%%（近%d批 %d/%d）", pct, n, succ, total)
	if sw.Rate >= okRate {
		sw.Level = "ok"
	} else if sw.Rate >= warnRate {
		sw.Level = "warn"
	} else {
		sw.Level = "error"
	}
	return sw
}

func (r *Runner) Health(ctx context.Context, force bool) registerlite.Health {
	r.mu.Lock()
	cfg := r.Cfg
	last := r.lastHealth
	lastAt := r.lastHealthAt
	cli := r.Client
	r.mu.Unlock()

	if !cfg.RegisterEnabled {
		return registerlite.Health{
			Backend: "unknown", Session: "unknown", CPA: "unknown",
			BackendMsg: "注册总开关关闭", SessionMsg: "未启用", CPAMsg: "未测",
			CheckedAt: nowCST(), Configured: false,
		}
	}
	if !force && !lastAt.IsZero() && time.Since(lastAt) < 20*time.Second {
		return last
	}
	if cli == nil {
		return registerlite.Health{Backend: "unknown", Session: "unknown", CPA: "unknown", BackendMsg: "client nil", CheckedAt: nowCST()}
	}
	h := cli.Test(ctx)
	r.mu.Lock()
	r.lastHealth = h
	r.lastHealthAt = time.Now()
	r.mu.Unlock()
	return h
}

func (r *Runner) Status(ctx context.Context) map[string]any {
	h := r.Health(ctx, false)
	sw := r.SuccessWindow()
	r.mu.Lock()
	cfg := r.Cfg
	cur := r.current
	var curCopy any
	if cur != nil {
		cp := *cur
		curCopy = cp
	}
	autoPaused := ""
	if time.Now().Before(r.autoPausedUntil) {
		autoPaused = r.autoPausedUntil.In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04:05")
	}
	lastAuto := ""
	if !r.lastAutoAt.IsZero() {
		lastAuto = r.lastAutoAt.In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04:05")
	}
	nextAuto := ""
	if cfg.RegisterAutoEnabled && cfg.RegisterEnabled && !r.nextAutoAt.IsZero() {
		nextAuto = r.nextAutoAt.In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04:05")
	}
	lastFloor, nextFloor := "", ""
	if !r.lastFloorAt.IsZero() {
		lastFloor = r.lastFloorAt.In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04:05")
	}
	if cfg.RegisterFloorEnabled && cfg.RegisterEnabled && !r.nextFloorAt.IsZero() {
		nextFloor = r.nextFloorAt.In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04:05")
	}
	histN := len(r.history)
	r.mu.Unlock()

	schedule := "注册：未启用"
	if cfg.RegisterEnabled {
		parts := "注册："
		if cur != nil && cur.Status == "running" {
			parts += fmt.Sprintf("进行中 %d/%d", cur.Done, cur.Total)
		} else if cur != nil && cur.FinishedAt != "" {
			parts += "上次 " + cur.FinishedAt
			if cur.Total > 0 {
				parts += fmt.Sprintf(" 成功%d/%d", cur.Success, cur.Total)
			}
		} else {
			parts += "空闲"
		}
		if cfg.RegisterFloorEnabled {
			parts += fmt.Sprintf(" · 保底开(低于%d补%d)", cfg.RegisterFloorMinPool, cfg.RegisterFloorCount)
			if nextFloor != "" {
				parts += " · 下次保底 " + nextFloor
			}
		} else {
			parts += " · 保底关"
		}
		if cfg.RegisterAutoEnabled {
			parts += " · 定时开"
			if cfg.RegisterAutoIntervalSec > 0 {
				parts += fmt.Sprintf(" · 每%ds", cfg.RegisterAutoIntervalSec)
			}
			if nextAuto != "" {
				parts += " · 下次定时 " + nextAuto
			}
			if autoPaused != "" {
				parts += " · 成功率暂停至 " + autoPaused
			}
		} else {
			parts += " · 定时关"
		}
		schedule = parts
	}

	return map[string]any{
		"enabled":            cfg.RegisterEnabled,
		"auto_enabled":       cfg.RegisterAutoEnabled,
		"dry_run":            cfg.RegisterDryRun,
		"health":             h,
		"success_window":     sw,
		"current":            curCopy,
		"schedule":           schedule,
		"last_auto_at":       lastAuto,
		"next_auto_at":       nextAuto,
		"auto_paused_until":  autoPaused,
		"history_count":      histN,
		"relogin_note":       registerlite.ReloginNote,
		"manual_default":     cfg.RegisterManualDefaultCount,
		"manual_max":         cfg.RegisterManualMaxCount,
		"auto_count":         cfg.RegisterAutoCount,
		"auto_interval_sec":  cfg.RegisterAutoIntervalSec,
		"floor_enabled":      cfg.RegisterFloorEnabled,
		"floor_min_pool":     cfg.RegisterFloorMinPool,
		"floor_count":        cfg.RegisterFloorCount,
		"floor_interval_sec": cfg.RegisterFloorIntervalSec,
		"next_floor_at":      nextFloor,
		"last_floor_at":      lastFloor,
	}
}

func (r *Runner) Start(ctx context.Context, count int, source string) (Job, error) {
	r.mu.Lock()
	cfg := r.Cfg
	if r.current != nil && r.current.Status == "running" {
		cp := *r.current
		r.mu.Unlock()
		return cp, fmt.Errorf("注册任务进行中")
	}
	if !cfg.RegisterEnabled {
		r.mu.Unlock()
		return Job{}, fmt.Errorf("注册总开关未打开")
	}
	if count <= 0 {
		count = cfg.RegisterManualDefaultCount
	}
	if count <= 0 {
		count = 10
	}
	max := cfg.RegisterManualMaxCount
	if max <= 0 {
		max = 50
	}
	if count > max {
		count = max
	}
	if source == "" {
		source = "panel"
	}
	r.seq++
	id := fmt.Sprintf("reg-%d-%d", time.Now().Unix()%100000, r.seq)
	job := &Job{
		ID:             id,
		Source:         source,
		Epoch:          r.successEpoch,
		Status:         "running",
		CountRequested: count,
		Total:          count,
		StartedAt:      nowCST(),
	}
	r.current = job
	r.upsertHistoryLocked(*job) // persist running job immediately so UI has a card + survives restart
	cli := r.Client
	dry := cfg.RegisterDryRun
	r.mu.Unlock()

	// health snapshot
	h := r.Health(ctx, true)
	r.mu.Lock()
	if r.current != nil && r.current.ID == id {
		r.current.HealthSnapshot = map[string]any{
			"backend": h.Backend, "session": h.Session, "cpa": h.CPA,
		}
	}
	r.mu.Unlock()

	r.appendJobLog(id, "info", fmt.Sprintf("健康检查：后端=%s 会话=%s CPA=%s", h.Backend, h.Session, h.CPA))
	if h.Backend == "error" || h.Session == "error" {
		return r.failJob(id, "后端/会话不健康: "+h.SessionMsg+h.BackendMsg)
	}
	if h.CPA == "error" {
		r.appendJobLog(id, "warn", "CPA 通道异常: "+h.CPAMsg)
		if cfg.RegisterRequireCPAok {
			return r.failJob(id, "CPA 通道不健康且 require_cpa_ok=true: "+h.CPAMsg)
		}
	} else if h.CPA == "ok" {
		r.appendJobLog(id, "ok", "CPA 通道正常 · "+h.CPAMsg)
	}

	r.log("info", fmt.Sprintf("【注册】%s注册 · 原因：%s · count=%d", map[string]string{"panel": "手动", "auto": "定时", "floor": "保底"}[source], map[string]string{"panel": "面板", "auto": "定时", "floor": "保底"}[source], count))
	r.appendJobLog(id, "info", fmt.Sprintf("开始注册 count=%d source=%s", count, source))

	if dry {
		r.mu.Lock()
		if r.current != nil && r.current.ID == id {
			r.current.Status = "done"
			r.current.Message = "dry_run：未真实调用 8788"
			r.current.Success = 0
			r.current.Failed = 0
			r.current.Done = count
			r.current.FinishedAt = nowCST()
			r.pushHistoryLocked(*r.current)
		}
		cp := *r.current
		r.mu.Unlock()
		r.log("info", "【注册】dry_run 完成 · count="+fmt.Sprint(count))
		return cp, nil
	}

	if cli == nil {
		return r.failJob(id, "client nil")
	}
	r.appendJobLog(id, "info", "已请求 8788 启动注册…")
	resp, err := cli.StartRegister(ctx, count)
	if err != nil {
		return r.failJob(id, err.Error())
	}
	batchID := pickString(resp, "batch_id", "id")
	if batchID == "" {
		if reg, ok := resp["registration"].(map[string]any); ok {
			batchID = pickString(reg, "batch_id", "id")
		}
	}
	// sometimes top-level is the task view
	if batchID == "" {
		batchID = pickString(resp, "batchId")
	}
	// also pull from runtime immediately
	if batchID == "" {
		if rt, e2 := cli.RuntimeTasks(ctx); e2 == nil {
			if reg, ok := rt["registration"].(map[string]any); ok {
				batchID = pickString(reg, "batch_id", "id")
			}
		}
	}
	r.mu.Lock()
	if r.current != nil && r.current.ID == id {
		r.current.BatchID = batchID
		if msg := pickString(resp, "message"); msg != "" {
			r.current.Message = msg
		}
		if total := pickInt(resp, "total", "count"); total > 0 {
			r.current.Total = total
		}
	}
	r.mu.Unlock()
	if batchID != "" {
		r.appendJobLog(id, "ok", "batch="+batchID+" 已启动，轮询进度中")
		r.log("info", "【注册】已启动 · batch="+batchID+" · count="+fmt.Sprint(count))
	} else {
		r.appendJobLog(id, "warn", "未解析到 batch_id，仍按 runtime 轮询")
		r.log("info", "【注册】已启动 · batch 未知 · count="+fmt.Sprint(count))
	}

	r.mu.Lock()
	if r.current != nil && r.current.ID == id {
		r.upsertHistoryLocked(*r.current)
	}
	r.mu.Unlock()
	go r.pollUntilDone(id, batchID)
	r.mu.Lock()
	cp := *r.current
	r.mu.Unlock()
	return cp, nil
}

func pickString(m map[string]any, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func pickInt(m map[string]any, keys ...string) int {
	if m == nil {
		return 0
	}
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case int64:
			return int(v)
		case json.Number:
			i, _ := v.Int64()
			return int(i)
		}
	}
	return 0
}

func (r *Runner) failJob(id, errMsg string) (Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil || r.current.ID != id {
		return Job{}, fmt.Errorf("%s", errMsg)
	}
	r.current.Status = "failed"
	r.current.Error = errMsg
	r.current.Message = errMsg
	r.current.FinishedAt = nowCST()
	r.pushHistoryLocked(*r.current)
	cp := *r.current
	r.log("error", "【注册】失败 · 原因："+errMsg)
	return cp, fmt.Errorf("%s", errMsg)
}

func (r *Runner) pushHistoryLocked(j Job) {
	// update-or-insert by ID (avoid running+done duplicates)
	r.upsertHistoryLocked(j)
	// low success auto-pause
	sw := r.successWindowLocked()
	if r.Cfg.RegisterAutoPauseOnLow && sw.Level == "error" && sw.SampleOK {
		r.autoPausedUntil = time.Now().Add(30 * time.Minute)
	}
}

// upsertHistoryLocked inserts or updates a job snapshot by ID (keeps newest-first order).
// Used so running jobs appear in register-history.json and survive restarts / UI refresh.
func (r *Runner) upsertHistoryLocked(j Job) {
	for i := range r.history {
		if r.history[i].ID == j.ID {
			r.history[i] = j
			// move to front
			if i != 0 {
				item := r.history[i]
				r.history = append([]Job{item}, append(r.history[:i], r.history[i+1:]...)...)
			}
			r.saveHistoryLocked()
			return
		}
	}
	r.history = append([]Job{j}, r.history...)
	if len(r.history) > maxHistory {
		r.history = r.history[:maxHistory]
	}
	r.saveHistoryLocked()
}

func (r *Runner) successWindowLocked() SuccessWindow {
	// caller holds lock — rebuild without re-lock
	cfg := r.Cfg
	win := cfg.RegisterHealthWindowJobs
	if win <= 0 {
		win = 10
	}
	minS := cfg.RegisterHealthMinSamples
	if minS <= 0 {
		minS = 10
	}
	okRate := cfg.RegisterHealthOKRate
	if okRate <= 0 {
		okRate = 0.85
	}
	warnRate := cfg.RegisterHealthWarnRate
	if warnRate <= 0 {
		warnRate = 0.60
	}
	sw := SuccessWindow{Level: "unknown", MinSamples: minS, Label: "样本不足"}
	n, succ, total := 0, 0, 0
	for _, j := range r.history {
		if j.Epoch != r.successEpoch {
			continue // excluded by success-rate reset
		}
		if j.Status == "running" {
			continue
		}
		t := j.Total
		if t <= 0 {
			t = j.CountRequested
		}
		if t <= 0 {
			t = j.Success + j.Failed
		}
		if t <= 0 {
			continue
		}
		succ += j.Success
		total += t
		n++
		if n >= win {
			break
		}
	}
	sw.Jobs, sw.Success, sw.Total = n, succ, total
	if total < minS {
		return sw
	}
	sw.SampleOK = true
	sw.Rate = float64(succ) / float64(total)
	pct := int(sw.Rate*100 + 0.5)
	sw.Label = fmt.Sprintf("%d%%（近%d批 %d/%d）", pct, n, succ, total)
	if sw.Rate >= okRate {
		sw.Level = "ok"
	} else if sw.Rate >= warnRate {
		sw.Level = "warn"
	} else {
		sw.Level = "error"
	}
	return sw
}

func (r *Runner) pollUntilDone(id, batchID string) {
	deadline := time.Now().Add(2 * time.Hour)
	for time.Now().Before(deadline) {
		time.Sleep(1 * time.Second)
		r.mu.Lock()
		if r.current == nil || r.current.ID != id || r.current.Status != "running" {
			r.mu.Unlock()
			return
		}
		cli := r.Client
		r.mu.Unlock()
		if cli == nil {
			_, _ = r.failJob(id, "client nil")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		rt, err := cli.RuntimeTasks(ctx)
		var reg map[string]any
		if err == nil {
			reg, _ = rt["registration"].(map[string]any)
		}
		if (reg == nil || pickString(reg, "batch_id", "id") == "") && batchID != "" {
			if b, e2 := cli.GetBatch(ctx, batchID); e2 == nil {
				reg = b
			}
		}
		cancel()
		if reg == nil {
			continue
		}
		total := pickInt(reg, "total", "count")
		done := pickInt(reg, "done", "completed")
		success := pickInt(reg, "success", "ok")
		failed := pickInt(reg, "failed", "fail")
		if failed == 0 && total > 0 && success > 0 && done >= total {
			failed = total - success
			if failed < 0 {
				failed = 0
			}
		}
		running, _ := reg["running"].(bool)
		status := pickString(reg, "status")
		msg := pickString(reg, "message")
		finished := !running && (status == "done" || status == "finished" || status == "completed" || status == "failed" || status == "stopped" || status == "error")
		// also treat done>=total as finished
		if !finished && total > 0 && done >= total && !running {
			finished = true
		}
		r.mu.Lock()
		if r.current != nil && r.current.ID == id {
			prevDone, prevSucc, prevFail := r.current.Done, r.current.Success, r.current.Failed
			if total > 0 {
				r.current.Total = total
			}
			r.current.Done = done
			r.current.Success = success
			r.current.Failed = failed
			if msg != "" {
				r.current.Message = msg
			}
			if bid := pickString(reg, "batch_id", "id"); bid != "" {
				r.current.BatchID = bid
			}
			// live progress log when counters or runtime message change
			prevMsg := r.current.Message
			changed := done != prevDone || success != prevSucc || failed != prevFail || (msg != "" && msg != prevMsg)
			if changed {
				line := fmt.Sprintf("进度 %d/%d · 成功%d · 失败%d", done, r.current.Total, success, failed)
				if msg != "" {
					line += " · " + msg
				}
				// de-dupe identical consecutive lines
				lastText := ""
				if n := len(r.current.Logs); n > 0 {
					lastText = r.current.Logs[n-1].Text
				}
				if line != lastText {
					r.current.Logs = append(r.current.Logs, LogLine{At: nowCST(), Level: "info", Text: line})
					if len(r.current.Logs) > 200 {
						r.current.Logs = r.current.Logs[len(r.current.Logs)-200:]
					}
					r.upsertHistoryLocked(*r.current)
					// throttle action log: counter change every 5 or first/finish; message-only every ~15s via done gate
					if done != prevDone && (done == r.current.Total || done%5 == 0 || done == 1) {
						r.mu.Unlock()
						r.log("info", "【注册】"+line)
						r.mu.Lock()
					}
				}
			}
			if finished {
				if status == "failed" || status == "error" {
					r.current.Status = "failed"
				} else if status == "stopped" {
					r.current.Status = "stopped"
				} else {
					r.current.Status = "done"
				}
				r.current.FinishedAt = nowCST()
				fin := fmt.Sprintf("完成 · batch=%s · 成功%d/失败%d · %s", r.current.BatchID, r.current.Success, r.current.Failed, r.current.Message)
				r.current.Logs = append(r.current.Logs, LogLine{At: nowCST(), Level: "ok", Text: fin})
				r.pushHistoryLocked(*r.current)
				r.log("info", "【注册】"+fin)
				r.mu.Unlock()
				return
			}
		}
		r.mu.Unlock()
	}
	_, _ = r.failJob(id, "轮询超时")
}

func (r *Runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	cli := r.Client
	id := ""
	if r.current != nil && r.current.Status == "running" {
		id = r.current.ID
	}
	r.mu.Unlock()
	if cli == nil {
		return fmt.Errorf("client nil")
	}
	_, err := cli.StopRegister(ctx)
	if err != nil {
		return err
	}
	if id != "" {
		r.mu.Lock()
		if r.current != nil && r.current.ID == id {
			r.current.Status = "stopped"
			r.current.Message = "已请求停止"
			r.current.FinishedAt = nowCST()
			r.pushHistoryLocked(*r.current)
		}
		r.mu.Unlock()
		r.log("info", "【注册】停止 · 原因：面板")
	}
	return nil
}

// MaybeAutoRegister is called by runtime ticker. Runs floor then schedule (independent).
func (r *Runner) MaybeAutoRegister(ctx context.Context) {
	r.maybeFloorRegister(ctx)
	r.maybeScheduleRegister(ctx)
}

func (r *Runner) autoGatesOK(ctx context.Context, cfg sentrycfg.Config) bool {
	h := r.Health(ctx, false)
	if h.Backend == "error" || h.Session == "error" {
		r.log("info", "【注册】跳过 · 原因：后端不健康")
		return false
	}
	// always require CPA green for auto paths
	if h.CPA == "error" {
		r.log("info", "【注册】跳过 · 原因：CPA 通道不健康")
		return false
	}
	sw := r.SuccessWindow()
	if sw.Level == "error" && sw.SampleOK {
		r.mu.Lock()
		r.autoPausedUntil = time.Now().Add(30 * time.Minute)
		r.mu.Unlock()
		r.log("info", "【注册】跳过 · 原因：近窗成功率低（已暂停30m）")
		return false
	}
	r.mu.Lock()
	cli := r.Client
	hasLocal := r.current != nil && r.current.Status == "running"
	r.mu.Unlock()
	if hasLocal {
		return false
	}
	if cli != nil {
		rt, err := cli.RuntimeTasks(ctx)
		if err == nil {
			if reg, ok := rt["registration"].(map[string]any); ok {
				if running, _ := reg["running"].(bool); running {
					r.adoptRemoteRegistration(reg)
					r.log("info", "【注册】发现 8788 进行中任务 · 已同步到注册页")
					return false
				}
			}
		}
	}
	return true
}

// adoptRemoteRegistration creates/updates a local job card from 8788 runtime snapshot.
func (r *Runner) adoptRemoteRegistration(reg map[string]any) {
	if reg == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current != nil && r.current.Status == "running" {
		return
	}
	batch := pickString(reg, "batch_id", "id")
	total := pickInt(reg, "total", "count")
	done := pickInt(reg, "done", "completed")
	success := pickInt(reg, "success", "ok")
	failed := pickInt(reg, "failed", "fail")
	msg := pickString(reg, "message")
	r.seq++
	id := fmt.Sprintf("reg-remote-%d-%d", time.Now().Unix()%100000, r.seq)
	// prefer existing history entry with same batch
	for i := range r.history {
		if r.history[i].BatchID != "" && r.history[i].BatchID == batch && r.history[i].Status == "running" {
			id = r.history[i].ID
			break
		}
	}
	job := &Job{
		ID: id, Source: "remote", Epoch: r.successEpoch, Status: "running",
		CountRequested: total, Total: total, Done: done, Success: success, Failed: failed,
		BatchID: batch, Message: msg, StartedAt: nowCST(),
		Logs: []LogLine{{At: nowCST(), Level: "info", Text: fmt.Sprintf("同步 8788 进行中任务 · batch=%s · %d/%d", batch, done, total)}},
	}
	if total <= 0 {
		job.Total = done + 1
	}
	r.current = job
	r.upsertHistoryLocked(*job)
	// resume poll in background
	go r.pollUntilDone(id, batch)
}

// maybeFloorRegister: when (enabled+cooldown) < min_pool, register floor_count each floor_interval.
func (r *Runner) maybeFloorRegister(ctx context.Context) {
	r.mu.Lock()
	cfg := r.Cfg
	if !cfg.RegisterEnabled || !cfg.RegisterFloorEnabled {
		r.mu.Unlock()
		return
	}
	if r.current != nil && r.current.Status == "running" {
		r.mu.Unlock()
		return
	}
	if time.Now().Before(r.autoPausedUntil) {
		r.mu.Unlock()
		return
	}
	interval := cfg.RegisterFloorIntervalSec
	if interval <= 0 {
		interval = 600
	}
	if !r.lastFloorAt.IsZero() && time.Since(r.lastFloorAt) < time.Duration(interval)*time.Second {
		r.nextFloorAt = r.lastFloorAt.Add(time.Duration(interval) * time.Second)
		r.mu.Unlock()
		return
	}
	poolFn := r.PoolCounter
	r.mu.Unlock()

	if !r.autoGatesOK(ctx, cfg) {
		return
	}
	enabled, cooldown := 0, 0
	if poolFn != nil {
		enabled, cooldown = poolFn(ctx)
	}
	pool := enabled + cooldown
	minPool := cfg.RegisterFloorMinPool
	if minPool <= 0 {
		minPool = 100
	}
	if pool >= minPool {
		r.mu.Lock()
		r.nextFloorAt = time.Now().Add(time.Duration(interval) * time.Second)
		r.mu.Unlock()
		// quiet when healthy
		return
	}
	count := cfg.RegisterFloorCount
	if count <= 0 {
		count = 10
	}
	r.log("info", fmt.Sprintf("【注册】保底触发 · 可接+冷却=%d < 阈值%d · 补%d", pool, minPool, count))
	r.mu.Lock()
	r.lastFloorAt = time.Now()
	r.nextFloorAt = r.lastFloorAt.Add(time.Duration(interval) * time.Second)
	r.mu.Unlock()
	_, err := r.Start(ctx, count, "floor")
	if err != nil {
		r.log("error", "【注册】保底注册失败 · 原因："+err.Error())
	}
}

// maybeScheduleRegister: fixed-interval batch (independent of floor).
func (r *Runner) maybeScheduleRegister(ctx context.Context) {
	r.mu.Lock()
	cfg := r.Cfg
	if !cfg.RegisterEnabled || !cfg.RegisterAutoEnabled {
		r.mu.Unlock()
		return
	}
	if r.current != nil && r.current.Status == "running" {
		r.mu.Unlock()
		return
	}
	if time.Now().Before(r.autoPausedUntil) {
		r.mu.Unlock()
		return
	}
	interval := cfg.RegisterAutoIntervalSec
	if interval <= 0 {
		interval = 3600
	}
	if !r.lastAutoAt.IsZero() && time.Since(r.lastAutoAt) < time.Duration(interval)*time.Second {
		r.nextAutoAt = r.lastAutoAt.Add(time.Duration(interval) * time.Second)
		r.mu.Unlock()
		return
	}
	r.nextAutoAt = time.Now().Add(time.Duration(interval) * time.Second)
	r.mu.Unlock()

	if !r.autoGatesOK(ctx, cfg) {
		return
	}
	count := cfg.RegisterAutoCount
	if count <= 0 {
		count = 10
	}
	r.mu.Lock()
	r.lastAutoAt = time.Now()
	r.nextAutoAt = r.lastAutoAt.Add(time.Duration(interval) * time.Second)
	r.mu.Unlock()
	_, err := r.Start(ctx, count, "auto")
	if err != nil {
		r.log("error", "【注册】定时注册失败 · 原因："+err.Error())
	}
}

// ClearSuccessHistory resets near-window success rate (panel reset button).
// Job history/logs are kept; only the success window starts a new epoch.
func (r *Runner) ClearSuccessHistory() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.successEpoch++
	r.autoPausedUntil = time.Time{}
}
