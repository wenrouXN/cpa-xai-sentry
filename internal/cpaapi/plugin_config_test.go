package cpaapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
)

func TestWritePluginConfigGetMergePut(t *testing.T) {
	var putBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/plugins/cpa-xai-sentry/config":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"enabled": true, "priority": 1, "management_key": "keep-me", "patrol_batch_size": 50,
			})
		case r.Method == http.MethodPut && r.URL.Path == "/v0/management/plugins/cpa-xai-sentry/config":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &putBody)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := cpaapi.New(srv.URL, "k", "")
	if err := c.WritePluginConfig(context.Background(), map[string]any{
		"patrol_batch_size": 500,
		"sentry_enabled":    true,
	}); err != nil {
		t.Fatal(err)
	}
	if putBody["patrol_batch_size"] != float64(500) && putBody["patrol_batch_size"] != 500 {
		t.Fatalf("batch=%v", putBody["patrol_batch_size"])
	}
	if putBody["management_key"] != "keep-me" {
		t.Fatalf("should keep existing key from GET, got %v", putBody["management_key"])
	}
	if putBody["enabled"] != true {
		t.Fatalf("enabled=%v", putBody["enabled"])
	}
}
