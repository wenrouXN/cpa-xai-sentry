package cpaapi_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
)

func TestResolverFromAuthDir(t *testing.T) {
	dir := t.TempDir()
	name := "xai-alice@example.com.json"
	raw := []byte(`{"type":"xai","email":"alice@example.com","note":"free"}`)
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c := cpaapi.New("", "", dir)
	r := cpaapi.NewResolver(c)
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	id, ok := r.Resolve(name, "", "")
	if !ok || id.Email != "alice@example.com" {
		t.Fatalf("resolve by filename: %+v %v", id, ok)
	}
	id, ok = r.Resolve("", name, "")
	if !ok || id.FileName != name {
		t.Fatalf("resolve file: %+v %v", id, ok)
	}
	id, ok = r.Resolve("", "", "alice@example.com")
	if !ok || id.FileName != name {
		t.Fatalf("resolve email: %+v %v", id, ok)
	}
}

func TestLooksLikeOpaqueIDAndDisplay(t *testing.T) {
	if cpaapi.LooksLikeOpaqueID("xai-a@b.com.json") {
		t.Fatal("filename should not be opaque")
	}
	if cpaapi.LooksLikeOpaqueID("user@x.com") {
		t.Fatal("email should not be opaque")
	}
	if !cpaapi.LooksLikeOpaqueID("a1b2c3d4e5f67890") {
		t.Fatal("hex should be opaque")
	}
	if got := cpaapi.DisplayName("a@b.com", "xai-a@b.com.json", "hash"); got != "a@b.com" {
		t.Fatalf("display=%s", got)
	}
	if got := cpaapi.DisplayName("", "xai-a@b.com.json", "hash"); got != "a@b.com" {
		t.Fatalf("display from file=%s", got)
	}
	if got := cpaapi.DisplayName("", "a1b2c3d4e5f67890", "a1b2c3d4e5f67890"); !stringsHasPrefix(got, "未解析") {
		t.Fatalf("opaque display=%s", got)
	}
}

func stringsHasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}
