package errorsig_test

import (
	"strings"
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/errorsig"
	"github.com/openclaw-local/cpa-xai-sentry/internal/match"
)

func TestKeyFromMatchStrictBuiltins(t *testing.T) {
	if k := errorsig.KeyFromMatch(match.Result{Signal: match.SignalFreeUsage429}, 429); k != "free_usage_429" {
		t.Fatal(k)
	}
	if k := errorsig.KeyFromMatch(match.Result{Signal: match.SignalSpendingLimit402}, 402); k != "spending_limit_402" {
		t.Fatal(k)
	}
	if k := errorsig.KeyFromMatch(match.Result{}, 403, `{"error":"Access Denied"}`); k != "unmatched" {
		t.Fatal(k)
	}
	if k := errorsig.KeyFromMatch(match.Result{Signal: match.SignalPermission403}, 403); k != "permission_403" {
		t.Fatal(k)
	}
	if k := errorsig.KeyFromMatch(match.Result{Signal: match.SignalAuth401}, 401); k != "auth_401" {
		t.Fatal(k)
	}
	// bare 429 without free-usage evidence is not free_usage_429
	if k := errorsig.KeyFromMatch(match.Result{}, 429); k != "unmatched" {
		t.Fatalf("bare 429 want unmatched, got %s", k)
	}
	if k := errorsig.KeyFromMatch(match.Result{}, 429, `{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage"}`); k != "free_usage_429" {
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

func TestHumanMsgBare429IsNotFreeUsage(t *testing.T) {
	if got := errorsig.HumanMsg("unmatched", `{"error":"Too many requests"}`, 429); got != "HTTP 429" {
		t.Fatalf("bare 429 message = %q", got)
	}
	if got := errorsig.HumanMsg("free_usage_429", `{"error":"Too many requests"}`, 429); got != "免费额度用尽" {
		t.Fatalf("free_usage key message = %q", got)
	}
}

func TestShapeOfLabelIsMachineCodeNotChinese(t *testing.T) {
	body := `{"code":"personal-team-blocked:spending-limit","error":"You have run out of credits or need a Grok subscription."}`
	_, lab, _ := errorsig.ShapeOf(body, 402)
	if lab != "402·personal-team-blocked:spending-limit" {
		t.Fatalf("shape label want machine code, got %q", lab)
	}
	if strings.Contains(lab, "消费") || strings.Contains(lab, "连接") {
		t.Fatalf("shape label must not auto-translate Chinese: %q", lab)
	}
	_, eofLab, _ := errorsig.ShapeOf(`Post "https://cli-chat-proxy.grok.com/v1/responses": unexpected EOF`, 0)
	if eofLab == "连接中断" || strings.Contains(eofLab, "连接") {
		t.Fatalf("EOF shape must not be Chinese 连接中断, got %q", eofLab)
	}
	if !strings.Contains(strings.ToLower(eofLab), "eof") {
		t.Fatalf("EOF shape should keep raw fragment, got %q", eofLab)
	}
}

func TestShapeUsesActualErrorFingerprintNotBareHTTPStatus(t *testing.T) {
	permission := `{"code":"permission-denied","error":"Access to the chat endpoint is denied. Please ensure you're using the correct credentials."}`
	region := `{"code":"permission-denied","error":"Model is not available in your region"}`
	gateway := `{"error":"Access Denied"}`

	sp, _, _ := errorsig.ShapeOf(permission, 403)
	sr, _, _ := errorsig.ShapeOf(region, 403)
	sg, _, _ := errorsig.ShapeOf(gateway, 403)
	if sp == sr || sp == sg || sr == sg {
		t.Fatalf("actual 403 samples must have distinct fingerprints: permission=%q region=%q gateway=%q", sp, sr, sg)
	}
	if sp != "permission_403" {
		t.Fatalf("known chat permission sample should keep builtin fingerprint, got %q", sp)
	}
}

func TestFingerprintNormalizesDynamicValues(t *testing.T) {
	a := `{"code":"permission-denied","error":"Your team 16edcf33-2122-44f3-ad42-def578e8fc43 reached monthly spending limit 2061725"}`
	b := `{"code":"permission-denied","error":"Your team aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee reached monthly spending limit 1999999"}`
	fa, _, _ := errorsig.ShapeOf(a, 402)
	fb, _, _ := errorsig.ShapeOf(b, 402)
	if fa != fb {
		t.Fatalf("dynamic UUID/numbers must not split one real error shape: %q != %q", fa, fb)
	}
}

func TestRealLogSamplesProduceStableDistinctFingerprints(t *testing.T) {
	cases := []struct {
		status int
		body   string
	}{
		{500, `Post "https://cli-chat-proxy.grok.com/v1/responses": EOF`},
		{402, `{"code":"personal-team-blocked:spending-limit","error":"You have run out of credits or need a Grok subscription."}`},
		{402, `{"code":"permission-denied","error":"Your team 16edcf33-2122-44f3-ad42-def578e8fc43 has either used all available credits or reached its monthly spending limit."}`},
		{429, `{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. tokens (actual/limit): 2061725/2000000"}`},
		{403, `{"code":"permission-denied","error":"Access to the chat endpoint is denied."}`},
		{426, `{"error":"Your Grok CLI version (none) is outdated. Please update to version 0.1.202 or later"}`},
		{401, `{"error":"Invalid or expired credentials (reason=no auth context)"}`},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		fp, _, _ := errorsig.ShapeOf(tc.body, tc.status)
		if fp == "" {
			t.Fatalf("empty fingerprint for %d %s", tc.status, tc.body)
		}
		if seen[fp] {
			t.Fatalf("different real samples collapsed into %q", fp)
		}
		seen[fp] = true
	}
}
