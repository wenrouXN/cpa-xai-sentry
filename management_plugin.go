//go:build cshared

package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

)

type managementRequest struct {
	Method  string          `json:"Method"`
	Path    string          `json:"Path"`
	Headers http.Header     `json:"Headers"`
	Query   url.Values      `json:"Query"`
	Body    json.RawMessage `json:"Body"`

	MethodAlt string          `json:"method"`
	PathAlt   string          `json:"path"`
	BodyAlt   json.RawMessage `json:"body"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

type managementRegistration struct {
	Routes    []managementRoute    `json:"routes,omitempty"`
	Resources []managementResource `json:"resources,omitempty"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description"`
}

type managementResource struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

func buildManagementRegistration() managementRegistration {
	return managementRegistration{
		Resources: []managementResource{
			{
				Path:        "/index.html",
				Menu:        "XAI Sentry",
				Description: "xAI 观察/冷却/候选/垃圾箱（7天可恢复）",
			},
		},
		Routes: []managementRoute{
			{Method: "GET", Path: "/" + pluginID + "/state", Description: "状态"},
			{Method: "GET", Path: "/" + pluginID + "/config", Description: "配置（脱敏）"},
			{Method: "POST", Path: "/" + pluginID + "/config", Description: "更新配置"},
			{Method: "GET", Path: "/" + pluginID + "/logs", Description: "动作日志"},
			{Method: "GET", Path: "/" + pluginID + "/candidates", Description: "候选列表"},
			{Method: "GET", Path: "/" + pluginID + "/trash", Description: "垃圾箱"},
			{Method: "POST", Path: "/" + pluginID + "/trash/restore", Description: "从垃圾箱恢复"},
			{Method: "POST", Path: "/" + pluginID + "/trash/purge", Description: "彻底清除"},
			{Method: "POST", Path: "/" + pluginID + "/run-tick", Description: "手动 tick"},
			{Method: "GET", Path: "/" + pluginID + "/health", Description: "健康检查"},
			{Method: "POST", Path: "/" + pluginID + "/toggle", Description: "切换开关"},
			{Method: "POST", Path: "/" + pluginID + "/preset", Description: "应用预设"},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return errorEnvelope("decode_management", err.Error()), nil
		}
	}
	method := strings.ToUpper(strings.TrimSpace(firstNonEmpty(req.Method, req.MethodAlt)))
	if method == "" {
		method = http.MethodGet
	}
	path := strings.TrimSpace(firstNonEmpty(req.Path, req.PathAlt))
	body := req.Body
	if len(body) == 0 && len(req.BodyAlt) > 0 {
		body = req.BodyAlt
	}
	body = decodeManagementBody(body)

	const resourcePrefix = "/v0/resource/plugins/" + pluginID + "/"
	if strings.HasPrefix(path, resourcePrefix) {
		return serveUI()
	}
	if path == "/index.html" || path == "index.html" || path == "/ui" {
		return serveUI()
	}

	action := ""
	const mgmtPrefix = "/v0/management/" + pluginID + "/"
	const shortPrefix = "/" + pluginID + "/"
	switch {
	case strings.HasPrefix(path, mgmtPrefix):
		action = strings.TrimPrefix(path, mgmtPrefix)
	case strings.HasPrefix(path, shortPrefix):
		action = strings.TrimPrefix(path, shortPrefix)
	default:
		action = strings.Trim(path, "/")
	}
	action = strings.Trim(action, "/")

	// map to panel HTTP handlers
	rt := runtimeInstance()
	if rt.Panel == nil {
		return okEnvelope(managementResponse{
			StatusCode: 503,
			Headers:    http.Header{"content-type": []string{"application/json"}},
			Body:       []byte(`{"error":"runtime not ready"}`),
		})
	}

	// special health
	if action == "health" {
		b, _ := json.Marshal(map[string]any{
			"ok":      true,
			"plugin":  pluginID,
			"version": pluginVer,
			"mode":    modeName(rt.Cfg.Enabled, rt.Cfg.SentryEnabled, rt.Cfg.AutoCooldown, rt.Cfg.AutoCandidate, rt.Cfg.AutoDelete),
			"config":  redactConfig(rt.Cfg),
		})
		return okEnvelope(managementResponse{
			StatusCode: 200,
			Headers:    http.Header{"content-type": []string{"application/json"}},
			Body:       b,
		})
	}

	// rewrite path for panel mux
	panelPath := "/" + action
	if action == "" {
		return serveUI()
	}
	// panel expects /state /config /trash etc
	// body for POST
	var r *http.Request
	if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
		r, _ = http.NewRequest(method, panelPath, strings.NewReader(string(body)))
	} else {
		r, _ = http.NewRequest(method, panelPath, nil)
	}
	if r == nil {
		return errorEnvelope("bad_request", "cannot build request"), nil
	}
	if req.Query != nil {
		r.URL.RawQuery = req.Query.Encode()
	}
	// also parse query from path if present
	if i := strings.Index(panelPath, "?"); i >= 0 {
		r, _ = http.NewRequest(method, panelPath, r.Body)
	}
	w := &captureResponse{}
	rt.Panel.Handler().ServeHTTP(w, r)
	return okEnvelope(managementResponse{
		StatusCode: w.code,
		Headers:    w.header,
		Body:       w.body,
	})
}

func serveUI() ([]byte, error) {
	// use panel UI constant via a tiny request
	rt := runtimeInstance()
	w := &captureResponse{}
	r, _ := http.NewRequest(http.MethodGet, "/ui", nil)
	if rt.Panel != nil {
		rt.Panel.Handler().ServeHTTP(w, r)
	} else {
		w.code = 200
		w.header = http.Header{"content-type": []string{"text/html; charset=utf-8"}}
		w.body = []byte("<html><body>XAI Sentry loading</body></html>")
	}
	if w.header == nil {
		w.header = http.Header{}
	}
	if w.header.Get("content-type") == "" {
		w.header.Set("content-type", "text/html; charset=utf-8")
	}
	return okEnvelope(managementResponse{StatusCode: w.code, Headers: w.header, Body: w.body})
}

type captureResponse struct {
	code   int
	header http.Header
	body   []byte
}

func (c *captureResponse) Header() http.Header {
	if c.header == nil {
		c.header = http.Header{}
	}
	return c.header
}
func (c *captureResponse) Write(b []byte) (int, error) {
	if c.code == 0 {
		c.code = 200
	}
	c.body = append(c.body, b...)
	return len(b), nil
}
func (c *captureResponse) WriteHeader(statusCode int) { c.code = statusCode }

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func decodeManagementBody(body json.RawMessage) json.RawMessage {
	if len(body) == 0 {
		return body
	}
	// sometimes body is base64 string JSON
	var s string
	if json.Unmarshal(body, &s) == nil && s != "" {
		if dec, err := base64.StdEncoding.DecodeString(s); err == nil {
			return json.RawMessage(dec)
		}
		return json.RawMessage(s)
	}
	return body
}

func modeName(enabled, sentry, cool, cand, del bool) string {
	if !sentry {
		return "off"
	}
	if !cool && !cand && !del {
		return "observe"
	}
	if del {
		return "auto_trash"
	}
	if cand {
		return "safe-guard"
	}
	return "cooldown"
}
