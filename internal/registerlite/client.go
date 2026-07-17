// Package registerlite is an HTTP client for grok-register-lite (port 8788).
// Cookie session is process-local only; never exposed to the browser panel.
package registerlite

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client talks to register-lite admin API with cookie session.
type Client struct {
	mu       sync.Mutex
	baseURL  string
	admin    string // e.g. /admin
	password string
	timeout  time.Duration
	http     *http.Client
	loggedIn bool
	lastErr  string
}

func New(baseURL, adminBase, password string, timeoutSec int) *Client {
	jar, _ := cookiejar.New(nil)
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	admin := strings.TrimRight(strings.TrimSpace(adminBase), "/")
	if admin == "" {
		admin = "/admin"
	}
	if !strings.HasPrefix(admin, "/") {
		admin = "/" + admin
	}
	return &Client{
		baseURL:  strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		admin:    admin,
		password: password,
		timeout:  time.Duration(timeoutSec) * time.Second,
		http: &http.Client{
			Timeout: time.Duration(timeoutSec) * time.Second,
			Jar:     jar,
		},
	}
}

func (c *Client) Configure(baseURL, adminBase, password string, timeoutSec int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	admin := strings.TrimRight(strings.TrimSpace(adminBase), "/")
	if admin == "" {
		admin = "/admin"
	}
	if !strings.HasPrefix(admin, "/") {
		admin = "/" + admin
	}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	// rebuild jar if endpoint or password changed
	needNew := base != c.baseURL || admin != c.admin || (password != "" && password != c.password)
	c.baseURL = base
	c.admin = admin
	if password != "" {
		c.password = password
	}
	c.timeout = time.Duration(timeoutSec) * time.Second
	if needNew || c.http == nil {
		jar, _ := cookiejar.New(nil)
		c.http = &http.Client{Timeout: c.timeout, Jar: jar}
		c.loggedIn = false
	} else {
		c.http.Timeout = c.timeout
	}
}

func (c *Client) Enabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.baseURL != "" && c.password != ""
}

func (c *Client) path(parts ...string) string {
	p := c.admin + "/api"
	for _, s := range parts {
		s = strings.Trim(s, "/")
		if s != "" {
			p += "/" + s
		}
	}
	return c.baseURL + p
}

func (c *Client) do(ctx context.Context, method, fullURL string, body any) (int, map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.mu.Lock()
	cli := c.http
	c.mu.Unlock()
	if cli == nil {
		return 0, nil, fmt.Errorf("http client nil")
	}
	resp, err := cli.Do(req)
	if err != nil {
		c.mu.Lock()
		c.lastErr = err.Error()
		c.mu.Unlock()
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	if out == nil {
		out = map[string]any{}
	}
	if resp.StatusCode >= 400 && out["error"] == nil && out["detail"] == nil {
		out["error"] = strings.TrimSpace(string(raw))
		if out["error"] == "" {
			out["error"] = resp.Status
		}
	}
	return resp.StatusCode, out, nil
}

func (c *Client) ensureLogin(ctx context.Context) error {
	c.mu.Lock()
	base, admin, pass, logged := c.baseURL, c.admin, c.password, c.loggedIn
	c.mu.Unlock()
	if base == "" {
		return fmt.Errorf("register_base_url 未配置")
	}
	if pass == "" {
		return fmt.Errorf("register_password 未配置")
	}
	// try session first if we think we are logged in
	if logged {
		code, j, err := c.do(ctx, http.MethodGet, base+admin+"/api/session", nil)
		if err == nil && code == 200 {
			if ok, _ := j["authenticated"].(bool); ok {
				return nil
			}
			if ok, _ := j["ok"].(bool); ok {
				if auth, _ := j["authenticated"].(bool); auth || j["authenticated"] == nil {
					// some builds only return ok
					if j["authenticated"] == nil {
						return nil
					}
				}
			}
		}
	}
	code, j, err := c.do(ctx, http.MethodPost, base+admin+"/api/auth/login", map[string]any{"password": pass})
	if err != nil {
		c.mu.Lock()
		c.loggedIn = false
		c.lastErr = err.Error()
		c.mu.Unlock()
		return err
	}
	if code >= 400 {
		msg, _ := j["error"].(string)
		if msg == "" {
			msg = fmt.Sprintf("login HTTP %d", code)
		}
		c.mu.Lock()
		c.loggedIn = false
		c.lastErr = msg
		c.mu.Unlock()
		return fmt.Errorf("%s", msg)
	}
	if auth, ok := j["authenticated"].(bool); ok && !auth {
		c.mu.Lock()
		c.loggedIn = false
		c.lastErr = "login rejected"
		c.mu.Unlock()
		return fmt.Errorf("登录被拒绝")
	}
	c.mu.Lock()
	c.loggedIn = true
	c.lastErr = ""
	c.mu.Unlock()
	return nil
}

// Health runs connectivity checks without starting registration.
type Health struct {
	Backend     string  `json:"backend"`      // ok|warn|error|unknown
	Session     string  `json:"session"`      // ok|error|unknown
	CPA         string  `json:"cpa"`          // ok|warn|error|unknown
	BackendMsg  string  `json:"backend_msg,omitempty"`
	SessionMsg  string  `json:"session_msg,omitempty"`
	CPAMsg      string  `json:"cpa_msg,omitempty"`
	Remote      string  `json:"remote_backend,omitempty"`
	LatencyMS   int64   `json:"latency_ms,omitempty"`
	CheckedAt   string  `json:"checked_at"`
	Configured  bool    `json:"configured"`
	LoginOK     bool    `json:"login_ok"`
}

func (c *Client) Test(ctx context.Context) Health {
	h := Health{
		Backend:    "unknown",
		Session:    "unknown",
		CPA:        "unknown",
		CheckedAt:  time.Now().In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04:05"),
		Configured: c.Enabled(),
	}
	if !h.Configured {
		h.Backend = "unknown"
		h.BackendMsg = "未配置 base_url 或 password"
		h.Session = "unknown"
		h.SessionMsg = "未配置"
		h.CPA = "unknown"
		h.CPAMsg = "未测"
		return h
	}
	start := time.Now()
	// session after login
	if err := c.ensureLogin(ctx); err != nil {
		h.Backend = "error"
		h.BackendMsg = err.Error()
		h.Session = "error"
		h.SessionMsg = err.Error()
		h.LatencyMS = time.Since(start).Milliseconds()
		return h
	}
	h.LoginOK = true
	code, j, err := c.do(ctx, http.MethodGet, c.path("session"), nil)
	h.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		h.Backend = "error"
		h.BackendMsg = err.Error()
		h.Session = "error"
		h.SessionMsg = err.Error()
		return h
	}
	if code >= 400 {
		h.Backend = "error"
		h.BackendMsg = fmt.Sprintf("session HTTP %d", code)
		h.Session = "error"
		h.SessionMsg = h.BackendMsg
		return h
	}
	h.Backend = "ok"
	h.BackendMsg = "可达"
	if h.LatencyMS > 3000 {
		h.Backend = "warn"
		h.BackendMsg = fmt.Sprintf("慢 %dms", h.LatencyMS)
	}
	h.Session = "ok"
	h.SessionMsg = "已登录"
	if auth, ok := j["authenticated"].(bool); ok && !auth {
		h.Session = "error"
		h.SessionMsg = "未认证"
	}

	// remote backend
	_, rb, err := c.do(ctx, http.MethodGet, c.path("remote-backend"), nil)
	if err == nil {
		if b, ok := rb["backend"].(string); ok {
			h.Remote = b
		}
	}

	// cpa test — 8788 rejects empty body ("CPA 地址不能为空"); must send stored config.
	cpaBody := map[string]any{}
	if codeCfg, cfgJ, errCfg := c.do(ctx, http.MethodGet, c.path("cpa", "config"), nil); errCfg == nil && codeCfg < 400 {
		if cfg, ok := cfgJ["config"].(map[string]any); ok {
			for _, k := range []string{"base_url", "management_key", "limit", "auto_upload_after_probe", "auto_upload_after_relogin"} {
				if v, has := cfg[k]; has {
					cpaBody[k] = v
				}
			}
		}
	}
	// if management_key masked, still send base_url — store merge may keep key when ******
	code, ct, err := c.do(ctx, http.MethodPost, c.path("cpa", "test"), cpaBody)
	if err != nil {
		h.CPA = "error"
		h.CPAMsg = err.Error()
		return h
	}
	if code >= 400 {
		h.CPA = "error"
		msg, _ := ct["error"].(string)
		if msg == "" {
			if d, ok := ct["detail"]; ok {
				msg = fmt.Sprintf("%v", d)
			}
		}
		if msg == "" {
			msg = fmt.Sprintf("cpa/test HTTP %d", code)
		}
		h.CPAMsg = msg
		return h
	}
	// success shapes vary
	ok := true
	if v, has := ct["ok"].(bool); has {
		ok = v
	}
	if !ok {
		h.CPA = "error"
		msg, _ := ct["error"].(string)
		if msg == "" {
			msg = "cpa/test 返回 ok=false"
		}
		h.CPAMsg = msg
		return h
	}
	h.CPA = "ok"
	h.CPAMsg = "CPA 连通"
	if h.Remote != "" && h.Remote != "cpa" {
		h.CPA = "warn"
		h.CPAMsg = "CPA test 通但 remote-backend=" + h.Remote
	}
	return h
}

// RuntimeTasks returns registration/probe/relogin snapshot.
func (c *Client) RuntimeTasks(ctx context.Context) (map[string]any, error) {
	if err := c.ensureLogin(ctx); err != nil {
		return nil, err
	}
	code, j, err := c.do(ctx, http.MethodGet, c.path("runtime", "active-tasks"), nil)
	if err != nil {
		return nil, err
	}
	if code >= 400 {
		return j, fmt.Errorf("runtime HTTP %d", code)
	}
	return j, nil
}

// StartRegister starts email registration with count (rest from 8788 stored config).
func (c *Client) StartRegister(ctx context.Context, count int) (map[string]any, error) {
	if count <= 0 {
		count = 1
	}
	if err := c.ensureLogin(ctx); err != nil {
		return nil, err
	}
	// refuse if already running
	rt, _ := c.RuntimeTasks(ctx)
	if reg, ok := rt["registration"].(map[string]any); ok {
		if running, _ := reg["running"].(bool); running {
			return reg, fmt.Errorf("注册任务进行中")
		}
	}
	body := map[string]any{"count": count}
	code, j, err := c.do(ctx, http.MethodPost, c.path("accounts", "register-email"), body)
	if err != nil {
		return nil, err
	}
	if code >= 400 {
		msg, _ := j["error"].(string)
		if msg == "" {
			if d, ok := j["detail"]; ok {
				msg = fmt.Sprintf("%v", d)
			}
		}
		if msg == "" {
			msg = fmt.Sprintf("register HTTP %d", code)
		}
		return j, fmt.Errorf("%s", msg)
	}
	return j, nil
}

// StopRegister stops all registration sessions.
func (c *Client) StopRegister(ctx context.Context) (map[string]any, error) {
	if err := c.ensureLogin(ctx); err != nil {
		return nil, err
	}
	code, j, err := c.do(ctx, http.MethodPost, c.path("accounts", "register-email", "stop"), map[string]any{})
	if err != nil {
		return nil, err
	}
	if code >= 400 {
		return j, fmt.Errorf("stop HTTP %d", code)
	}
	return j, nil
}

// GetBatch polls a batch id.
func (c *Client) GetBatch(ctx context.Context, batchID string) (map[string]any, error) {
	if err := c.ensureLogin(ctx); err != nil {
		return nil, err
	}
	batchID = strings.TrimSpace(batchID)
	if batchID == "" {
		return nil, fmt.Errorf("empty batch_id")
	}
	u := c.path("accounts", "register-email", "batches", url.PathEscape(batchID))
	code, j, err := c.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if code >= 400 {
		return j, fmt.Errorf("batch HTTP %d", code)
	}
	return j, nil
}

// ReloginNote documents feasibility for panel.
const ReloginNote = "重登仅支持 8788 本地库里有真实 xAI 密码的账号；" +
	"非本机导入且无 password 的号会报「本地没有账号密码」，无法重登。" +
	"密码协议：captcha → CreateSession → 新 SSO → 写库 → 内部 probe → 可选 auto_upload。"

// ListEmails returns all emails known to register-lite (local inventory).
func (c *Client) ListEmails(ctx context.Context) ([]string, error) {
	if err := c.ensureLogin(ctx); err != nil {
		return nil, err
	}
	code, j, err := c.do(ctx, http.MethodGet, c.path("accounts", "emails"), nil)
	if err != nil {
		return nil, err
	}
	if code >= 400 {
		return nil, fmt.Errorf("emails HTTP %d", code)
	}
	out := []string{}
	if arr, ok := j["emails"].([]any); ok {
		for _, x := range arr {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.ToLower(strings.TrimSpace(s)))
			}
		}
	}
	return out, nil
}

// Relogin starts password relogin for emails that exist in 8788 local DB with real passwords.
func (c *Client) Relogin(ctx context.Context, emails []string, concurrency int) (map[string]any, error) {
	if err := c.ensureLogin(ctx); err != nil {
		return nil, err
	}
	clean := make([]string, 0, len(emails))
	seen := map[string]bool{}
	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		clean = append(clean, e)
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("emails 为空")
	}
	body := map[string]any{"emails": clean}
	if concurrency > 0 {
		body["concurrency"] = concurrency
	}
	code, j, err := c.do(ctx, http.MethodPost, c.path("accounts", "relogin"), body)
	if err != nil {
		return nil, err
	}
	if code >= 400 {
		msg, _ := j["error"].(string)
		if msg == "" {
			if d, ok := j["detail"]; ok {
				msg = fmt.Sprintf("%v", d)
			}
		}
		if msg == "" {
			msg = fmt.Sprintf("relogin HTTP %d", code)
		}
		return j, fmt.Errorf("%s", msg)
	}
	return j, nil
}

// ReloginStatus polls current relogin task.
func (c *Client) ReloginStatus(ctx context.Context) (map[string]any, error) {
	if err := c.ensureLogin(ctx); err != nil {
		return nil, err
	}
	code, j, err := c.do(ctx, http.MethodGet, c.path("accounts", "relogin", "status"), nil)
	if err != nil {
		return nil, err
	}
	if code >= 400 {
		return j, fmt.Errorf("relogin status HTTP %d", code)
	}
	return j, nil
}

// StopRelogin stops running relogin batch.
func (c *Client) StopRelogin(ctx context.Context) (map[string]any, error) {
	if err := c.ensureLogin(ctx); err != nil {
		return nil, err
	}
	code, j, err := c.do(ctx, http.MethodPost, c.path("accounts", "relogin", "stop"), map[string]any{})
	if err != nil {
		return nil, err
	}
	if code >= 400 {
		return j, fmt.Errorf("stop relogin HTTP %d", code)
	}
	return j, nil
}
