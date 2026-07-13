package cpaapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
)

func TestListAndDisable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/management/auth-files", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"name": "xai-a.json", "provider": "xai", "disabled": false, "email": "a@b.com"},
		})
	})
	mux.HandleFunc("/v0/management/auth-files/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Fatalf("method %s", r.Method)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := cpaapi.New(srv.URL, "k", "")
	files, err := c.ListAuthFiles(context.Background())
	if err != nil || len(files) != 1 {
		t.Fatalf("%v %v", files, err)
	}
	if err := c.SetDisabled(context.Background(), "xai-a.json", true); err != nil {
		t.Fatal(err)
	}
}

func TestDelete(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/management/auth-files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method %s", r.Method)
		}
		if r.URL.Query().Get("name") != "xai-a.json" {
			t.Fatalf("name=%s", r.URL.Query().Get("name"))
		}
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := cpaapi.New(srv.URL, "k", "")
	if err := c.DeleteAuthFile(context.Background(), "xai-a.json"); err != nil {
		t.Fatal(err)
	}
}

func TestAuthDirIO(t *testing.T) {
	dir := t.TempDir()
	c := cpaapi.New("", "", dir)
	name := "xai-a.json"
	if err := c.WriteAuthFileToDir(name, []byte(`{"ok":1}`)); err != nil {
		t.Fatal(err)
	}
	b, err := c.ReadAuthFileFromDir(name)
	if err != nil || string(b) != `{"ok":1}` {
		t.Fatalf("%s %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}
