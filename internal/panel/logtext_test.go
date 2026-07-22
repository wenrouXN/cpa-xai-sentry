package panel

import (
	"strings"
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
)

func TestHumanizeReasonNeverSurfacesInternalKeys(t *testing.T) {
	// bare catalog key with Chinese signal label → 中文错误信息
	if got := humanizeReason("free_usage_429", "429·免费额度用尽", "cooldown"); got != "免费额度用尽" {
		t.Fatalf("want 免费额度用尽, got %q", got)
	}
	if got := humanizeReason("permission_403", "403·权限拒绝", "cooldown"); got != "权限拒绝" {
		t.Fatalf("got %q", got)
	}
	if got := humanizeReason("reason:fp_bb675aa3c700f17d", "426·终端版本过低", "observe"); got != "终端版本过低" {
		t.Fatalf("got %q", got)
	}
	// no signal label: still map builtins
	if got := humanizeReason("free_usage_429", "", "cooldown"); got != "免费额度用尽" {
		t.Fatalf("fallback got %q", got)
	}
	// Chinese policy reason kept
	if got := humanizeReason("策略阶梯冷却 · 连续≥1", "429·免费额度用尽", "cooldown"); got != "策略阶梯冷却 · 连续≥1" {
		t.Fatalf("got %q", got)
	}
}

func TestLiveActiveReasonSkipsAnyErrorForDisplay(t *testing.T) {
	tests := []struct {
		name    string
		streaks map[string]int
	}{
		{name: "prefer real streak", streaks: map[string]int{"any_error": 40, "unmatched": 39}},
		{name: "label global count with last signal", streaks: map[string]int{"any_error": 40}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc := &state.Account{State: state.Active, LastSignal: "unmatched", Streaks: tt.streaks}
			got := liveActiveReason(acc)
			if !strings.Contains(got, "未分类") || strings.Contains(got, "任意错误") {
				t.Fatalf("want unmatched display instead of any_error, got %q", got)
			}
		})
	}
}

func TestCandidateLabelNeverSurfacesFingerprintKey(t *testing.T) {
	st := state.New("")
	fp := "reason:fp_bb675aa3c700f17d"
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: fp, Label: "终端版本过低", DisplayMsg: "终端版本过低", Enabled: true,
	})
	st.ObserveError(fp, "终端版本过低", "none", "x", `{"error":"outdated"}`, "a", "f", "usage", 426)
	a := &API{State: st}
	acc := &state.Account{AuthIndex: "a", State: state.CandidateDead, LastSignal: fp}
	got := a.candidateStatusLabel(acc)
	if got != "426·候删" {
		t.Fatalf("want 426·候删, got %q", got)
	}
}

func TestSignalDisplayFallbackNeverSurfacesFingerprint(t *testing.T) {
	a := &API{State: state.New("")}
	sig := "reason:fp_bb675aa3c700f17d"
	if got := a.signalDisplayZH(sig); got == sig || got == "fp_bb675aa3c700f17d" {
		t.Fatalf("fingerprint leaked in display label: %q", got)
	}
	if got := humanizeReason(sig, a.signalDisplayZH(sig), "cooldown"); got == sig || got == "fp_bb675aa3c700f17d" {
		t.Fatalf("fingerprint leaked in reason: %q", got)
	}
}
