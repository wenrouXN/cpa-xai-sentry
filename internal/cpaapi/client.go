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
	"time"
)

type Client struct {
	BaseURL string
	Key     string
	AuthDir string
	HC      *http.Client
}

type AuthFile struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Disabled bool   `json:"disabled"`
	Email    string `json:"email"`
	Type     string `json:"type"`
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

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n])
}
