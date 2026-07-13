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
func (r *Runner) Run(ctx context.Context, targets []Target) []Result {
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
			if res.Err != "" {
				return
			}
			// feed into guard (same policy path)
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
				})
			}
		}()
	}
	wg.Wait()
	return out
}

func (r *Runner) probeOne(ctx context.Context, t Target) Result {
	res := Result{AuthIndex: t.AuthIndex}
	base := strings.TrimRight(strings.TrimSpace(t.BaseURL), "/")
	if base == "" {
		base = "https://api.x.ai"
	}
	model := r.Cfg.PatrolModel
	if model == "" {
		model = "grok-4.5"
	}
	// If base already ends with /v1 (common for grok cli-chat-proxy), do NOT prefix paths with /v1 again.
	// Wrong: https://cli-chat-proxy.grok.com/v1 + /v1/responses => /v1/v1/responses => nginx 404
	// Right: .../v1 + /responses
	// Prefer CPA/xAI executor path POST /responses first (Grok CLI chat-proxy + api.x.ai),
	// then chat/completions fallback.
	var paths []string
	// Prefer chat/completions first (stable max_tokens). /responses is fallback
	// with max_output_tokens-only body (max_tokens is rejected there → HTTP 400).
	if strings.HasSuffix(strings.ToLower(base), "/v1") {
		paths = []string{"/chat/completions", "/responses"}
	} else {
		paths = []string{"/v1/chat/completions", "/v1/responses"}
	}
	var lastErr error
	var lastCode int
	var lastBody string
	for attempt := 0; attempt < 3; attempt++ {
		for _, pth := range paths {
			code, body, err := r.postProbe(ctx, base+pth, pth, model, t.Token)
			if err != nil {
				lastErr = err
				// network: retry
				continue
			}
			lastCode, lastBody = code, body
			res.StatusCode = code
			res.Body = body
			// 5xx retry
			if code >= 500 {
				lastErr = fmt.Errorf("upstream %d", code)
				continue
			}
			// 404 on a path: try next path shape (not necessarily dead account)
			if code == 404 {
				lastErr = fmt.Errorf("http 404 on %s", pth)
				continue
			}
			// 400 due to wrong request shape for this path: try alternate endpoint
			// e.g. /v1/responses rejects max_tokens — not an account failure.
			if code == 400 && IsProbeShapeError(body) {
				lastErr = fmt.Errorf("http 400 shape on %s", pth)
				continue
			}
			res.SignalPath = true
			return res
		}
		select {
		case <-ctx.Done():
			res.Err = ctx.Err().Error()
			return res
		case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
		}
	}
	// Prefer last HTTP result over generic network error when we did get a response.
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
	if strings.Contains(low, "unknown field") || strings.Contains(low, "unsupported") {
		// only treat as shape if mentions token limits / responses
		if strings.Contains(low, "token") || strings.Contains(low, "responses") || strings.Contains(low, "messages") || strings.Contains(low, "input") {
			return true
		}
	}
	return false
}

func (r *Runner) postProbe(ctx context.Context, url, pathHint, model, token string) (int, string, error) {
	// Endpoint-specific payload:
	//  - /responses: OpenAI-style responses API — max_output_tokens + input (NO max_tokens)
	//  - /chat/completions: max_tokens + messages
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
		payload = map[string]any{
			"model": model,
			"messages": []map[string]string{
				{"role": "user", "content": "ping"},
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
	// Grok CLI identity required by cli-chat-proxy; missing => HTTP 426 "CLI version (none)".
	// Values aligned with cpa-xai-quota-guard DefaultProbeCLIVersion / defaultProbeHeaders.
	const cliVer = "0.2.93"
	req.Header.Set("User-Agent", "grok-pager/"+cliVer+" grok-shell/"+cliVer+" (linux; x86_64)")
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
