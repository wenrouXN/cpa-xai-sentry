package guard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

// Already user_manual permanent → second policy disable must not emit another
// manual_disable action log (same quiet-idempotent pattern as cooldown).
func TestPermanentDisableIdempotentNoDoubleLog(t *testing.T) {
	dir := t.TempDir()
	var setTrue int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files/status" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["disabled"].(bool); ok && d {
				setTrue++
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.ManagementURL = srv.URL
	cfg.ManagementKey = "k"
	st := state.New(filepath.Join(dir, "s.json"))
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: "permission_403", Label: "权限拒绝", Enabled: true, Action: "disable",
		Threshold: 1, CountMode: "streak",
		Escalations: []state.EscalationRule{{Streak: 1, Action: "disable"}},
	})
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))

	body := `{"code":"permission-denied","error":"Access to the chat endpoint is denied. Please ensure you're using the correct credentials."}`
	ev := guard.UsageEvent{
		Provider: "xai", AuthIndex: "p1", FileName: "xai-p1@x.com.json", Email: "p1@x.com",
		StatusCode: 403, Body: body, Source: "patrol",
	}
	if err := g.HandleUsage(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	acc := st.Get("p1")
	if acc == nil || acc.State != state.UserManual || acc.DisableSource != "user_manual" {
		t.Fatalf("first disable: want user_manual, got %+v", acc)
	}
	n1 := 0
	for _, e := range st.SnapshotLogs() {
		if e.Action == "manual_disable" && e.Auth == "p1" {
			n1++
		}
	}
	if n1 != 1 {
		t.Fatalf("first disable: want 1 manual_disable log, got %d", n1)
	}
	setAfterFirst := setTrue

	// second same error while already permanent — no new log; may reassert file closed
	if err := g.HandleUsage(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range st.SnapshotLogs() {
		if e.Action == "manual_disable" && e.Auth == "p1" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want 1 manual_disable log after idempotent re-entry, got %d", n)
	}
	acc = st.Get("p1")
	if acc == nil || acc.State != state.UserManual || acc.DisableSource != "user_manual" {
		t.Fatalf("want still user_manual, got %+v", acc)
	}
	if setTrue < setAfterFirst {
		t.Fatalf("want reassert SetDisabled still attempted (setTrue=%d first=%d)", setTrue, setAfterFirst)
	}
}
