package cpamp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	BaseURL  string
	AdminKey string
	HTTP     *http.Client
}

func New(baseURL, adminKey string) *Client {
	return &Client{
		BaseURL:  strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		AdminKey: strings.TrimSpace(adminKey),
		HTTP:     &http.Client{Timeout: 20 * time.Second},
	}
}

func (c *Client) Enabled() bool {
	return c != nil && c.BaseURL != "" && c.AdminKey != ""
}

type Summary struct {
	TotalCalls   int64 `json:"total_calls"`
	SuccessCalls int64 `json:"success_calls"`
	FailureCalls int64 `json:"failure_calls"`
	TotalTokens  int64 `json:"total_tokens"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

func (c *Client) FetchXAISummary(ctx context.Context, fromMS, toMS int64) (Summary, error) {
	var zero Summary
	if !c.Enabled() {
		return zero, fmt.Errorf("CPAMP 未配置")
	}
	if toMS <= fromMS {
		return zero, fmt.Errorf("时间范围无效")
	}
	payload := map[string]any{
		"from_ms":   fromMS,
		"to_ms":     toMS,
		"now_ms":    time.Now().UnixMilli(),
		"time_zone": "Asia/Shanghai",
		"filters":   map[string]any{"providers": []string{"xai"}},
		"include":   map[string]any{"summary": true},
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v0/management/monitoring/analytics", bytes.NewReader(raw))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.AdminKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return zero, fmt.Errorf("CPAMP HTTP %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	var parsed struct {
		Summary Summary `json:"summary"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return zero, err
	}
	return parsed.Summary, nil
}

func DayRangeShanghai(t time.Time) (fromMS, toMS int64) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	tt := t.In(loc)
	start := time.Date(tt.Year(), tt.Month(), tt.Day(), 0, 0, 0, 0, loc)
	end := start.Add(24 * time.Hour)
	return start.UnixMilli(), end.UnixMilli()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
