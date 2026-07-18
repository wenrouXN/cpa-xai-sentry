package patrol_test

import (
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/patrol"
)

func TestParseMode(t *testing.T) {
	cases := map[string]patrol.Mode{
		"all":       patrol.ModeAll,
		"全部":        patrol.ModeAll,
		"enabled":   patrol.ModeEnabled,
		"full":      patrol.ModeEnabled,
		"cooldown":  patrol.ModeCooldown,
		"permanent": patrol.ModePermanent,
		"永禁":        patrol.ModePermanent,
		"candidate": patrol.ModeCandidate,
		"候删":        patrol.ModeCandidate,
		"trash":     patrol.ModeTrash,
		"垃圾箱":       patrol.ModeTrash,
		"":          patrol.ModeEnabled,
		"weird":     patrol.ModeEnabled,
	}
	for in, want := range cases {
		got := patrol.ParseMode(in)
		// ModeFull and ModeEnabled both mean enabled filter
		if got == patrol.ModeFull {
			got = patrol.ModeEnabled
		}
		wantN := want
		if wantN == patrol.ModeFull {
			wantN = patrol.ModeEnabled
		}
		if got != wantN {
			t.Fatalf("ParseMode(%q)=%q want %q", in, got, wantN)
		}
	}
}

func TestModeLabel(t *testing.T) {
	if patrol.ModeLabel(patrol.ModeAll) != "全部" {
		t.Fatal(patrol.ModeLabel(patrol.ModeAll))
	}
	if patrol.ModeLabel(patrol.ModeCooldown) != "冷却" {
		t.Fatal(patrol.ModeLabel(patrol.ModeCooldown))
	}
	if patrol.ModeLabel(patrol.ModePermanent) != "永久禁用" {
		t.Fatal(patrol.ModeLabel(patrol.ModePermanent))
	}
	if patrol.ModeLabel(patrol.ModeCandidate) != "候删" {
		t.Fatal(patrol.ModeLabel(patrol.ModeCandidate))
	}
	if patrol.ModeLabel(patrol.ModeTrash) != "垃圾箱" {
		t.Fatal(patrol.ModeLabel(patrol.ModeTrash))
	}
	if patrol.ModeLabel(patrol.ModeEnabled) != "启用" && patrol.ModeLabel(patrol.ModeFull) != "启用" {
		t.Fatal("enabled label")
	}
}
