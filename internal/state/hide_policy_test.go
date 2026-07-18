package state

import (
	"path/filepath"
	"testing"
)

func TestUnclassifyBuiltinEmptyCardHidesReseed(t *testing.T) {
	dir := t.TempDir()
	st := New(filepath.Join(dir, "s.json"))
	// seed builtin-like policy with no observed hits
	st.UpsertErrorPolicy(ErrorPolicy{
		Key: "http_404", Label: "404", Enabled: true, Action: "observe", Source: "builtin",
	})
	if err := st.ReclassifyErrorKey("http_404", "unmatched", "未分类错误"); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GetErrorPolicy("http_404"); ok {
		t.Fatal("policy should be dropped after unclassify")
	}
	// EnsureBuiltin would re-seed without hide mark
	st.EnsureBuiltinPolicies(map[string]ErrorPolicy{
		"http_404": {Key: "http_404", Label: "404", Enabled: true, Action: "observe", Source: "builtin"},
	})
	if _, ok := st.GetErrorPolicy("http_404"); ok {
		t.Fatal("hidden builtin must not re-seed")
	}
}

func TestUnclassifyWithHitsMergesToUnmatched(t *testing.T) {
	dir := t.TempDir()
	st := New(filepath.Join(dir, "s.json"))
	st.ObserveError("permission_403", "403", "permission_403", "x", "sample", "a1", "f.json", "usage", 403)
	st.UpsertErrorPolicy(ErrorPolicy{Key: "permission_403", Label: "403", Enabled: true, Action: "cooldown"})
	if err := st.ReclassifyErrorKey("permission_403", "unmatched", "未分类错误"); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GetErrorPolicy("permission_403"); ok {
		t.Fatal("want policy gone")
	}
	obs := st.ListObserved()
	var um *ObservedError
	for i := range obs {
		if obs[i].Key == "unmatched" {
			um = &obs[i]
		}
		if obs[i].Key == "permission_403" {
			t.Fatal("observed should move off permission_403")
		}
	}
	if um == nil || um.Count < 1 {
		t.Fatalf("unmatched missing hits: %+v", um)
	}
}

func TestReclassifyPersistsSourceShapeOnExistingDestination(t *testing.T) {
	st := New("")
	from := "reason:fp_source"
	to := "merged"
	st.ObserveError(from, "来源", "none", "x", "sample", "a", "f", "usage", 402)
	st.UpsertErrorPolicy(ErrorPolicy{Key: from, Label: "来源", SplitShape: "fp_source", Enabled: true})
	st.UpsertErrorPolicy(ErrorPolicy{Key: to, Label: "合并", Enabled: true})
	if err := st.ReclassifyErrorKey(from, to, "合并"); err != nil {
		t.Fatal(err)
	}
	p, ok := st.GetErrorPolicy(to)
	if !ok || p.SplitShape != "fp_source" {
		t.Fatalf("destination must retain source fingerprint route: %+v ok=%v", p, ok)
	}
}

func TestUpsertUnhides(t *testing.T) {
	dir := t.TempDir()
	st := New(filepath.Join(dir, "s.json"))
	_ = st.ReclassifyErrorKey("auth_401", "unmatched", "未分类错误") // empty hide
	st.UpsertErrorPolicy(ErrorPolicy{Key: "auth_401", Label: "401", Enabled: true, Action: "candidate"})
	st.EnsureBuiltinPolicies(map[string]ErrorPolicy{
		"auth_401": {Key: "auth_401", Label: "401", Enabled: true, Action: "candidate", Source: "builtin"},
	})
	if _, ok := st.GetErrorPolicy("auth_401"); !ok {
		t.Fatal("explicit upsert should unhide")
	}
}
