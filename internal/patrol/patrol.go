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
	if strings.HasSuffix(strings.ToLower(base), "/v1") {
		paths = []string{"/responses", "/chat/completions"}
	} else {
		paths = []string{"/v1/responses", "/v1/chat/completions"}
	}
	var lastErr error
	var lastCode int
	var lastBody string
	for attempt := 0; attempt < 3; attempt++ {
		for _, p := range paths {
			code, body, err := r.postProbe(ctx, base+p, model, t.Token)
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
				lastErr = fmt.Errorf("http 404 on %s", p)
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

func (r *Runner) postProbe(ctx context.Context, url, model, token string) (int, string, error) {
	// Prefer chat/completions-compatible payload; /v1/responses may ignore extra fields or require max_output_tokens.
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "ping"},
		},
		"max_tokens":        1,
		"max_output_tokens": 1,
		"stream":            false,
	}
	// responses-style field (harmless on chat endpoints that ignore unknown keys)
	payload["input"] = "ping"
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
