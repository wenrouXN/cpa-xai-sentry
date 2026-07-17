package patrol_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/patrol"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

// Each probe must append a job log line immediately (v1.1.36 live path).
func TestRunAppendsLiveJobLogs(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := sentrycfg.Default()
	cfg.PatrolEnabled = true
	cfg.PatrolConcurrency = 2
	cfg.PatrolBatchSize = 10
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "s.json"))
	tr := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	g := guard.New(cfg, st, tr, nil)
	r := patrol.New(cfg, g, nil)

	targets := []patrol.Target{
		{AuthIndex: "a1", FileName: "xai-a1.json", Provider: "xai", Email: "a1@x.com", BaseURL: upstream.URL, Token: "t"},
		{AuthIndex: "a2", FileName: "xai-a2.json", Provider: "xai", Email: "a2@x.com", BaseURL: upstream.URL, Token: "t"},
		{AuthIndex: "a3", FileName: "xai-a3.json", Provider: "xai", Email: "a3@x.com", BaseURL: upstream.URL, Token: "t"},
	}
	res := r.Run(context.Background(), targets)
	if len(res) != 3 {
		t.Fatalf("results=%d", len(res))
	}
	// appendProbeResultLive writes into jobStatus.Logs regardless of Running
	stt := r.Status()
	nAlive := 0
	for _, l := range stt.Logs {
		if l.Action == "alive" {
			nAlive++
		}
	}
	if nAlive < 3 {
		t.Fatalf("want 3 live alive logs during Run, got %d (logs=%d)", nAlive, len(stt.Logs))
	}
	// success path also touches state accounts
	for _, id := range []string{"a1", "a2", "a3"} {
		if st.Get(id) == nil {
			t.Fatalf("missing account %s after patrol probe", id)
		}
	}
}
