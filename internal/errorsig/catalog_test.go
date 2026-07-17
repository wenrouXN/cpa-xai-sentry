package errorsig_test

import (
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/errorsig"
	"github.com/openclaw-local/cpa-xai-sentry/internal/match"
)

func TestCollapseTargetKeepsUserSplits(t *testing.T) {
	for _, k := range []string{"reason:net_eof", "reason:auth_401", "my_custom", "free_usage_429", "permission_403", "unmatched", "any_error"} {
		if tgt, ok := errorsig.CollapseTarget(k); ok {
			t.Fatalf("%s should keep, got %q ok=%v", k, tgt, ok)
		}
	}
	cases := map[string]string{
		"permission_403:access_denied": "permission_403",
		"free_usage_429:foo":           "free_usage_429",
		"auth_401":                     "unmatched",
		"spending_limit_402":           "unmatched",
		"http_404":                     "unmatched",
		"code:invalid-argument":        "unmatched",
		"http_502":                     "unmatched",
	}
	for k, want := range cases {
		got, ok := errorsig.CollapseTarget(k)
		if !ok || got != want {
			t.Fatalf("%s: got %q ok=%v want %q", k, got, ok, want)
		}
	}
}

func TestKeyFromMatchOnly429403(t *testing.T) {
	if k := errorsig.KeyFromMatch(match.Result{Signal: match.SignalFreeUsage429}, 429); k != "free_usage_429" {
		t.Fatal(k)
	}
	if k := errorsig.KeyFromMatch(match.Result{}, 403, `{"error":"Access Denied"}`); k != "unmatched" {
		t.Fatal(k)
	}
	if k := errorsig.KeyFromMatch(match.Result{Signal: match.SignalPermission403}, 403); k != "permission_403" {
		t.Fatal(k)
	}
	if k := errorsig.KeyFromMatch(match.Result{}, 401); k != "unmatched" {
		t.Fatal(k)
	}
	if k := errorsig.KeyFromMatch(match.Result{}, 402); k != "unmatched" {
		t.Fatal(k)
	}
	if k := errorsig.KeyFromMatch(match.Result{}, 404); k != "unmatched" {
		t.Fatal(k)
	}
}
