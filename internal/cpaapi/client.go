package cpaapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	Key     string
	AuthDir string
	HC      *http.Client
}

type AuthFile struct {
	Name      string `json:"name"`
	ID        string `json:"id"`
	Path      string `json:"path"`
	AuthIndex string `json:"auth_index"`
	Provider  string `json:"provider"`
	Disabled  bool   `json:"disabled"`
	Email     string `json:"email"`
	Type      string `json:"type"`
	Account   string `json:"account"`
}

func New(baseURL, key, authDir string) *Client {
	return &Client{
		BaseURL: baseURL,
		Key:     key,
		AuthDir: authDir,
		HC:      &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
		req.Header.Set("X-Management-Key", c.Key)
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

func (c *Client) ListAuthFiles(ctx context.Context) ([]AuthFile, error) {
	b, code, err := c.do(ctx, http.MethodGet, "/v0/management/auth-files", nil)
	if err != nil {
		return nil, err
	}
	if code >= 300 {
		return nil, fmt.Errorf("list auth-files: %d %s", code, truncate(b, 200))
	}
	// response may be array or {files:[]}
	var arr []AuthFile
	if err := json.Unmarshal(b, &arr); err == nil {
		return arr, nil
	}
	var wrap struct {
		Files []AuthFile `json:"files"`
		Data  []AuthFile `json:"data"`
	}
	if err := json.Unmarshal(b, &wrap); err != nil {
		return nil, err
	}
	if len(wrap.Files) > 0 {
		return wrap.Files, nil
	}
	return wrap.Data, nil
}

func (c *Client) SetDisabled(ctx context.Context, name string, disabled bool) error {
	body := map[string]any{"name": name, "disabled": disabled}
	b, code, err := c.do(ctx, http.MethodPatch, "/v0/management/auth-files/status", body)
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("set disabled: %d %s", code, truncate(b, 200))
	}
	return nil
}

func (c *Client) DeleteAuthFile(ctx context.Context, name string) error {
	q := url.Values{"name": {name}}
	b, code, err := c.do(ctx, http.MethodDelete, "/v0/management/auth-files?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("delete auth-file: %d %s", code, truncate(b, 200))
	}
	return nil
}

func (c *Client) ReadAuthFileFromDir(name string) ([]byte, error) {
	if c.AuthDir == "" {
		return nil, fmt.Errorf("auth_dir empty")
	}
	return os.ReadFile(filepath.Join(c.AuthDir, name))
}

func (c *Client) WriteAuthFileToDir(name string, raw []byte) error {
	if c.AuthDir == "" {
		return fmt.Errorf("auth_dir empty")
	}
	if err := os.MkdirAll(c.AuthDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(c.AuthDir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// PluginID is the CPA plugins.configs key / binary basename.
const PluginID = "cpa-xai-sentry"

// GetPluginConfig fetches plugins.configs.<id> from CPA management API.
func (c *Client) GetPluginConfig(ctx context.Context) (map[string]any, error) {
	if c == nil || strings.TrimSpace(c.BaseURL) == "" {
		return nil, fmt.Errorf("management not configured")
	}
	path := "/v0/management/plugins/" + PluginID + "/config"
	b, code, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if code >= 300 {
		return nil, fmt.Errorf("get plugin config: %d %s", code, truncate(b, 200))
	}
	var full map[string]any
	if err := json.Unmarshal(b, &full); err != nil {
		return nil, err
	}
	if full == nil {
		full = map[string]any{}
	}
	return full, nil
}

// WritePluginConfig merges patch into CPA host plugin config and PUTs the full block back.
// Official CPA path: PUT /v0/management/plugins/<id>/config replaces the whole object,
// so we always GET+merge first (same pattern as cpa plugin examples / quota-guard).
func (c *Client) WritePluginConfig(ctx context.Context, patch map[string]any) error {
	if c == nil || strings.TrimSpace(c.BaseURL) == "" || strings.TrimSpace(c.Key) == "" {
		return fmt.Errorf("management not configured")
	}
	full, err := c.GetPluginConfig(ctx)
	if err != nil {
		return err
	}
	for k, v := range patch {
		full[k] = v
	}
	// host-owned
	full["enabled"] = true
	path := "/v0/management/plugins/" + PluginID + "/config"
	b, code, err := c.do(ctx, http.MethodPut, path, full)
	if err != nil {
		return err
	}
	if code >= 300 {
		// try PATCH shallow merge if PUT not allowed
		if code == 404 || code == 405 {
			b2, code2, err2 := c.do(ctx, http.MethodPatch, path, patch)
			if err2 != nil {
				return err2
			}
			if code2 >= 300 {
				return fmt.Errorf("patch plugin config: %d %s", code2, truncate(b2, 200))
			}
			return nil
		}
		return fmt.Errorf("put plugin config: %d %s", code, truncate(b, 200))
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n])
}
