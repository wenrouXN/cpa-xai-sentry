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
	if !r.Cfg.PatrolEnabled && len(targets) == 0 {
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
			if res.Err != "" {
				return
			}
			// feed into guard (same policy path)
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
		}()
	}
	wg.Wait()
	return out
}

func (r *Runner) probeOne(ctx context.Context, t Target) Result {
	res := Result{AuthIndex: t.AuthIndex}
	base := strings.TrimRight(t.BaseURL, "/")
	if base == "" {
		base = "https://api.x.ai"
	}
	model := r.Cfg.PatrolModel
	if model == "" {
		model = "grok-4.5"
	}
	// try /v1/responses then /v1/chat/completions
	paths := []string{"/v1/responses", "/v1/chat/completions"}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		for _, p := range paths {
			code, body, err := r.postProbe(ctx, base+p, model, t.Token)
			if err != nil {
				lastErr = err
				// network: retry
				continue
			}
			res.StatusCode = code
			res.Body = body
			// 5xx retry
			if code >= 500 {
				lastErr = fmt.Errorf("upstream %d", code)
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
	if lastErr != nil {
		res.Err = lastErr.Error()
	} else {
		res.Err = "probe failed"
	}
	return res
}

func (r *Runner) postProbe(ctx context.Context, url, model, token string) (int, string, error) {
	payload := map[string]any{
		"model": model,
		"input": "ping",
		// chat fallback shape also accepted by some gateways if they ignore extra fields
		"messages": []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
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
	// grok cli-ish headers to reduce 426
	req.Header.Set("User-Agent", "cpa-xai-sentry/0.1")
	resp, err := r.HC.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(body), nil
}
