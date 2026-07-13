package patrol_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/patrol"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

func TestProbeClassifies402NoTrash(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"code":"personal-team-blocked:spending-limit","error":"need a Grok subscription"}`))
	}))
	defer upstream.Close()

	cfg := sentrycfg.Default()
	cfg.PatrolEnabled = true
	cfg.AutoCooldown = true
	cfg.AutoDelete = true
	cfg.DeleteSignals = []string{"spending_limit_402", "auth_401"}
	cfg.PatrolBatchSize = 10
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "s.json"))
	tr := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	g := guard.New(cfg, st, tr, nil)
	r := patrol.New(cfg, g, nil)

	results := r.Run(context.Background(), []patrol.Target{{
		AuthIndex: "a", FileName: "xai-a.json", Provider: "xai",
		BaseURL: upstream.URL, Token: "t", Note: "free",
	}})
	if len(results) != 1 || results[0].StatusCode != 402 {
		t.Fatalf("%+v", results)
	}
	if len(st.ListTrash()) != 0 {
		t.Fatal("network/probe 402 must not trash")
	}
	acc := st.Get("a")
	if acc == nil {
		t.Fatal("missing account state")
	}
}

func TestNetworkErrorNoTrash(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.PatrolEnabled = true
	cfg.AutoDelete = true
	cfg.DeleteSignals = []string{"auth_401"}
	cfg.PatrolTimeout = 1
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "s.json"))
	tr := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	g := guard.New(cfg, st, tr, nil)
	r := patrol.New(cfg, g, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	res := r.Run(context.Background(), []patrol.Target{{
		AuthIndex: "n", FileName: "xai-n.json", Provider: "xai", BaseURL: url,
	}})
	if len(res) != 1 || res[0].Err == "" {
		t.Fatalf("%+v", res)
	}
	if len(st.ListTrash()) != 0 {
		t.Fatal("network must not trash")
	}
	_ = strings.Contains(res[0].Err, "connect")
}

func TestIsProbeShapeError(t *testing.T) {
	body := `{"error":"'max_tokens' is not supported on /v1/responses — use 'max_output_tokens'"}`
	if !patrol.IsProbeShapeError(body) {
		t.Fatal("expected shape error")
	}
	if patrol.IsProbeShapeError(`{"error":"permission-denied"}`) {
		t.Fatal("permission should not be shape error")
	}
}

func TestProbeFallsBackFromResponses404ToChat(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		if strings.Contains(r.URL.Path, "responses") {
			// missing endpoint → fall back to chat/completions
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		// chat/completions
		if _, ok := got["max_tokens"]; !ok {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"need max_tokens"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"c"}`))
	}))
	defer srv.Close()
	cfg := sentrycfg.Default()
	cfg.PatrolModel = "grok-4.5"
	st := state.New(filepath.Join(t.TempDir(), "s.json"))
	r := patrol.New(cfg, guard.New(cfg, st, trash.New(t.TempDir(), 7, true, st), nil), nil)
	res := r.Run(context.Background(), []patrol.Target{{
		AuthIndex: "h", FileName: "xai-h.json", Provider: "xai",
		BaseURL: srv.URL, Token: "tok",
	}})
	if len(res) != 1 || res[0].StatusCode != 200 {
		t.Fatalf("want 200 via chat fallback, got %+v paths=%v", res, paths)
	}
	if len(paths) < 2 || !strings.Contains(paths[0], "responses") {
		t.Fatalf("want responses-first then chat, paths=%v", paths)
	}
}

func TestProbeResponsesPayloadAndCLIHeaders(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		// require Grok CLI identity headers
		if r.Header.Get("x-grok-client-version") == "" || r.Header.Get("x-grok-client-identifier") == "" {
			w.WriteHeader(426)
			_, _ = w.Write([]byte(`{"error":"CLI version"}`))
			return
		}
		if strings.Contains(r.URL.Path, "responses") {
			if _, ok := got["max_tokens"]; ok {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"max_tokens not supported"}`))
				return
			}
			if got["input"] == nil {
				w.WriteHeader(422)
				_, _ = w.Write([]byte(`{"error":"missing field input"}`))
				return
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"id":"r"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"c"}`))
	}))
	defer srv.Close()
	cfg := sentrycfg.Default()
	cfg.PatrolModel = "grok-4.5"
	st := state.New(filepath.Join(t.TempDir(), "s.json"))
	r := patrol.New(cfg, guard.New(cfg, st, trash.New(t.TempDir(), 7, true, st), nil), nil)
	res := r.Run(context.Background(), []patrol.Target{{
		AuthIndex: "h", FileName: "xai-h.json", Provider: "xai",
		BaseURL: srv.URL + "/v1", Token: "tok",
	}})
	if len(res) != 1 || res[0].StatusCode != 200 {
		t.Fatalf("got %+v path=%s payload=%v", res, path, got)
	}
	if !strings.Contains(path, "responses") {
		t.Fatalf("want /responses first, path=%s", path)
	}
	if _, ok := got["max_tokens"]; ok {
		t.Fatal("responses must not send max_tokens")
	}
}
