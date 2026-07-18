package panel

import (
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

func TestSignalDisplayPrefersDisplayMsg(t *testing.T) {
	st := state.New("")
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: "free_usage_429", Label: "免费额度用尽", DisplayMsg: "额度用完了", Enabled: true,
	})
	a := &API{State: st}
	got := a.signalDisplayZH("free_usage_429")
	if got != "429·额度用完了" {
		t.Fatalf("want display_msg in title, got %q", got)
	}
	// 原因 strip code prefix → 用户设置的中文错误信息
	if why := humanizeReason("free_usage_429", got, "cooldown"); why != "额度用完了" {
		t.Fatalf("want 额度用完了 in log reason, got %q", why)
	}
}
