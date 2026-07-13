package guard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

func TestTickRecoverDoesNotFightReassert(t *testing.T) {
	dir := t.TempDir()
	disabled := true // start disabled mid cool-down; list may flip
	setCalls := []bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			// simulate stale/enabled listing while cool-down still owned (old race)
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{
				"name": "xai-2v12a6cqa0@lovc.eu.cc.json", "auth_index": "e9e1174178280dc8",
				"email": "2v12a6cqa0@lovc.eu.cc", "provider": "xai", "type": "xai",
				"disabled": false, // looks open while cool-down due
			}}})
		case r.URL.Path == "/v0/management/auth-files/status":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["disabled"].(bool); ok {
				setCalls = append(setCalls, d)
				disabled = d
			}
			w.WriteHeader(200)
		default:
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.ManagementURL = srv.URL
	cfg.ManagementKey = "k"
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("e9e1174178280dc8")
	st.UpdateMeta("e9e1174178280dc8", "xai-2v12a6cqa0@lovc.eu.cc.json", "2v12a6cqa0@lovc.eu.cc", "")
	st.SetAccountState("e9e1174178280dc8", state.CooldownPermission, "plugin_auto")
	st.SetLastSignal("e9e1174178280dc8", "permission_403")
	st.SetRecoverAt("e9e1174178280dc8", time.Now().Add(-time.Second)) // DUE
	_ = st.Save()
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	acc := st.Get("e9e1174178280dc8")
	if acc.State != state.Active {
		t.Fatalf("want active after due recover, got %s", acc.State)
	}
	// should open (false), must NOT re-close after open
	opens, closes := 0, 0
	for _, d := range setCalls {
		if d {
			closes++
		} else {
			opens++
		}
	}
	if opens == 0 {
		t.Fatalf("expected reenable SetDisabled(false), calls=%v", setCalls)
	}
	// no reassert after reenable: last call should not be close if we recovered
	// allow zero closes (ideal) — if close happened before open that's the old bug when order was sync first
	// with recover-first, closes should be 0
	if closes != 0 {
		t.Fatalf("due recover must not reassert-close, setCalls=%v", setCalls)
	}
	// no reassert log
	for _, e := range st.SnapshotLogs() {
		if e.Action == "cooldown_reassert" && e.Auth == "e9e1174178280dc8" {
			t.Fatalf("unexpected reassert log: %+v", e)
		}
	}
	_ = disabled
}
