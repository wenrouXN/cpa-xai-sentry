package patrol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
)

type Runner struct {
	Cfg   sentrycfg.Config
	Guard *guard.Guard
	CPA   *cpaapi.Client
	HC    *http.Client
	// GetGuard, if set, returns the current Guard from Runtime.
	// Called at the start of each patrol run to avoid stale Guard references
	// after ApplyConfig/rebuild replaces the Guard object.
	GetGuard func() *guard.Guard
	// patrolFinishedAt tracks when the last patrol finished, used by IsRunning
	// to provide a settle window so tick prune doesn't delete freshly created accounts.
	patrolFinishedAt time.Time
}

// IsRunning returns true when a patrol job is currently executing,
// or within 30s after finishing (settle window to protect newly created accounts
// from being pruned by the tick before they're fully persisted).
func (r *Runner) IsRunning() bool {
	jobMu.Lock()
	running := jobStatus.Running
	finished := r.patrolFinishedAt
	jobMu.Unlock()
	if running {
		return true
	}
	if !finished.IsZero() && time.Since(finished) < 30*time.Second {
		return true
	}
	return false
}

func New(cfg sentrycfg.Config, g *guard.Guard, cpa *cpaapi.Client) *Runner {
	timeout := time.Duration(cfg.PatrolTimeout) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Runner{
		Cfg: cfg.Validate(), Guard: g, CPA: cpa,
		HC: &http.Client{Timeout: timeout},
	}
}

type Target struct {
	AuthIndex string
	FileName  string
	Email     string
	Provider  string
	Note      string
	Label     string
	BaseURL   string
	Token     string
}

type Result struct {
	AuthIndex  string
	StatusCode int
	Body       string
	Err        string
	SignalPath bool
}

// Run probes targets with limited concurrency. Network errors never trash.
// v1.1.36: each probe appends job log immediately (not only after whole batch).
func (r *Runner) Run(ctx context.Context, targets []Target) []Result {
	return r.RunMode(ctx, targets, ModeEnabled)
}

// RunMode keeps mode selection at target collection; once selected, every real
// result follows the same Guard state machine.
func (r *Runner) RunMode(ctx context.Context, targets []Target, _ Mode) []Result {
	if len(targets) == 0 {
		return nil
	}
	n := r.Cfg.PatrolConcurrency
	if n <= 0 {
		n = 8
	}
	batch := r.Cfg.PatrolBatchSize
	if batch > 0 && len(targets) > batch {
		targets = targets[:batch]
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	var mu sync.Mutex
	out := make([]Result, 0, len(targets))

	for _, t := range targets {
		t := t
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res := r.probeOne(ctx, t)
			mu.Lock()
			out = append(out, res)
			mu.Unlock()
			// live progress for panel progress bar
			bumpPatrolProgress(res)
			// live per-probe job log (full + cooldown same path)
			r.appendProbeResultLive(t, res)
			if res.Err != "" {
				return
			}
			// feed into guard (same policy path) — success may reopen cool / open file
			if r.Guard != nil {
				_ = r.Guard.HandleUsage(ctx, guard.UsageEvent{
					Provider:   t.Provider,
					AuthIndex:  t.AuthIndex,
					FileName:   t.FileName,
					Email:      t.Email,
					StatusCode: res.StatusCode,
					Body:       res.Body,
					Success:    res.StatusCode >= 200 && res.StatusCode < 300,
					Source:     "patrol",
					Note:       t.Note,
					Label:      t.Label,
					Model:      r.Cfg.PatrolModel,
				})
			}
		}()
	}
	wg.Wait()
	return out
}

// DefaultProbeBaseURL is used when an auth file has no base_url (Grok CLI chat-proxy).
const DefaultProbeBaseURL = "https://cli-chat-proxy.grok.com/v1"

// DefaultProbeCLIVersion is sent as Grok CLI client identity (avoids HTTP 426).
const DefaultProbeCLIVersion = "0.2.93"

func (r *Runner) probeOne(ctx context.Context, t Target) Result {
	res := Result{AuthIndex: t.AuthIndex}
	base := strings.TrimRight(strings.TrimSpace(t.BaseURL), "/")
	if base == "" {
		base = DefaultProbeBaseURL
	}
	model := r.Cfg.PatrolModel
	if model == "" {
		model = "grok-4.5"
	}
	// Probe order:
	//   1) POST .../responses  (input + max_output_tokens, never max_tokens)
	//   2) fall back to .../chat/completions on 404/405 or body-shape errors
	// If base already ends with /v1, do not prefix /v1 again.
	var paths []string
	if strings.HasSuffix(strings.ToLower(base), "/v1") {
		paths = []string{"/responses", "/chat/completions"}
	} else {
		paths = []string{"/v1/responses", "/v1/chat/completions"}
	}
	var lastErr error
	var lastCode int
	var lastBody string
	for attempt := 0; attempt < 3; attempt++ {
		for _, pth := range paths {
			code, body, err := r.postProbe(ctx, base+pth, pth, model, t.Token)
			if err != nil {
				lastErr = err
				continue
			}
			lastCode, lastBody = code, body
			res.StatusCode = code
			res.Body = body
			// 5xx retry same model/path
			if code >= 500 || code == 408 {
				lastErr = fmt.Errorf("upstream %d", code)
				continue
			}
			// endpoint missing / wrong method → try next path
			if code == 404 || code == 405 {
				lastErr = fmt.Errorf("http %d on %s", code, pth)
				continue
			}
			// wrong body shape for this path → try alternate
			if (code == 400 || code == 422) && IsProbeShapeError(body) {
				lastErr = fmt.Errorf("http %d shape on %s", code, pth)
				continue
			}
			res.SignalPath = true
			return res
		}
		select {
		case <-ctx.Done():
			res.Err = ctx.Err().Error()
			return res
		case <-time.After(time.Duration(attempt+1) * 400 * time.Millisecond):
		}
	}
	if lastCode > 0 {
		res.StatusCode = lastCode
		res.Body = lastBody
		res.SignalPath = true
		return res
	}
	if lastErr != nil {
		res.Err = lastErr.Error()
	} else {
		res.Err = "probe failed"
	}
	return res
}

func IsProbeShapeError(body string) bool {
	low := strings.ToLower(body)
	// CPA/xAI: wrong field for endpoint — not auth death
	if strings.Contains(low, "max_tokens") && strings.Contains(low, "not supported") {
		return true
	}
	if strings.Contains(low, "max_output_tokens") && strings.Contains(low, "not supported") {
		return true
	}
	if strings.Contains(low, "unknown field") || strings.Contains(low, "unsupported") || strings.Contains(low, "missing field") {
		if strings.Contains(low, "token") || strings.Contains(low, "responses") || strings.Contains(low, "messages") || strings.Contains(low, "input") {
			return true
		}
	}
	return false
}

func (r *Runner) postProbe(ctx context.Context, url, pathHint, model, token string) (int, string, error) {
	// Endpoint-specific JSON body.
	lowPath := strings.ToLower(pathHint)
	var payload map[string]any
	if strings.Contains(lowPath, "responses") {
		payload = map[string]any{
			"model":             model,
			"input":             "ping",
			"max_output_tokens": 1,
			"stream":            false,
		}
	} else {
		// chat/completions fallback
		payload = map[string]any{
			"model": model,
			"messages": []map[string]string{
				{"role": "user", "content": "hi"},
			},
			"max_tokens": 1,
			"stream":     false,
		}
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// Grok CLI identity headers. Missing these → HTTP 426 (CLI version none).
	cliVer := DefaultProbeCLIVersion
	req.Header.Set("User-Agent", "grok-pager/"+cliVer+" grok-shell/"+cliVer+" (linux; x86_64)")
	req.Header.Set("x-authenticateresponse", "authenticate-response")
	req.Header.Set("x-grok-client-identifier", "grok-pager")
	req.Header.Set("x-grok-client-version", cliVer)
	req.Header.Set("x-xai-token-auth", "xai-grok-cli")
	resp, err := r.HC.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(body), nil
}
