package errorfp_test

import (
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/errorfp"
)

func TestStatusParticipatesInFingerprint(t *testing.T) {
	body := `{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage."}`
	a := errorfp.Build(body, 429)
	b := errorfp.Build(body, 500)
	if a.SuggestKey == b.SuggestKey {
		t.Fatalf("different statuses must not collapse: 429=%s 500=%s", a.SuggestKey, b.SuggestKey)
	}
	if a.SuggestKey != "free_usage_429" {
		t.Fatalf("real 429 free usage should retain builtin key: %s", a.SuggestKey)
	}
}

func TestPrettyJSONHasSameFingerprintAsCompactJSON(t *testing.T) {
	compact := `{"code":"personal-team-blocked:spending-limit","error":"You have run out of credits"}`
	pretty := "{\n  \"code\": \"personal-team-blocked:spending-limit\",\n  \"error\": \"You have run out of credits\"\n}"
	a, b := errorfp.Build(compact, 402), errorfp.Build(pretty, 402)
	if a.SuggestKey != b.SuggestKey || b.Code == "" || b.Message == "" {
		t.Fatalf("pretty JSON must preserve fields and identity: compact=%+v pretty=%+v", a, b)
	}
}
