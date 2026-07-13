package trash_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

const testToken = "tok_test_value"

func TestMoveRequiresSnapshotBeforeDelete(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "state.json"))
	ts := trash.New(filepath.Join(dir, "trash"), 7, true, st)

	deleted := false
	err := ts.MoveToTrash(state.TrashMeta{ID: "x", AuthIndex: "a"}, nil, func() error {
		deleted = true
		return nil
	})
	if err == nil || deleted {
		t.Fatalf("empty snapshot err=%v deleted=%v", err, deleted)
	}

	raw := []byte("{\"email\":\"a@b.com\",\"access_token\":\""+testToken+"\",\"disabled\":false}")
	meta := state.TrashMeta{
		ID: "id1", AuthIndex: "a", FileName: "xai-a.json", Email: "a@b.com",
		Signal: "auth_401", Source: "manual", TrashedAt: time.Now(),
	}
	if err := ts.MoveToTrash(meta, raw, func() error {
		deleted = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("delete should run after snapshot")
	}
	if _, err := os.Stat(filepath.Join(dir, "trash", "id1.json")); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreDefaultDisabled(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "state.json"))
	ts := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	raw := []byte("{\"email\":\"a@b.com\",\"access_token\":\""+testToken+"\",\"disabled\":false}")
	meta := state.TrashMeta{ID: "id2", AuthIndex: "a", FileName: "xai-a.json", Email: "a@b.com"}
	if err := ts.MoveToTrash(meta, raw, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	var written []byte
	if err := ts.Restore("id2", false, func(fileName string, b []byte) error {
		written = b
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(written, &m); err != nil {
		t.Fatal(err)
	}
	if m["disabled"] != true {
		t.Fatalf("disabled=%v", m["disabled"])
	}
	if m["access_token"] != testToken {
		t.Fatalf("token preserved in write path: %v", m["access_token"])
	}
	for _, item := range ts.ListMeta() {
		b, _ := json.Marshal(item)
		if strings.Contains(string(b), testToken) || strings.Contains(string(b), "access_token") {
			t.Fatalf("leaked: %s", b)
		}
	}
}

func TestAutoPurgeExpired(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "state.json"))
	ts := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	raw := []byte(`{"email":"a@b.com"}`)
	past := time.Now().Add(-time.Hour)
	meta := state.TrashMeta{
		ID: "old", AuthIndex: "a", FileName: "f.json",
		TrashedAt: past.Add(-8 * 24 * time.Hour), ExpiresAt: past,
	}
	if err := ts.MoveToTrash(meta, raw, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	n, err := ts.PurgeExpired(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("purged=%d", n)
	}
	if len(ts.ListMeta()) != 0 {
		t.Fatal("still listed")
	}
}

func TestSnapshotFailNoDelete(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "state.json"))
	bad := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := trash.New(bad, 7, true, st)
	deleted := false
	err := ts.MoveToTrash(state.TrashMeta{ID: "z", AuthIndex: "a"}, []byte(`{}`), func() error {
		deleted = true
		return errors.New("should not")
	})
	if err == nil || deleted {
		t.Fatalf("err=%v deleted=%v", err, deleted)
	}
}
