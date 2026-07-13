package quota

import "testing"

func TestParseJSONRemaining(t *testing.T) {
	body := `{"error":{"code":"free-usage-exhausted","remaining":0,"limit":100,"used":100,"retry_after":3600}}`
	info := Parse(body)
	if info.Remaining != 0 || info.Limit != 100 || info.Used != 100 {
		t.Fatalf("got %+v", info)
	}
	if info.ResetAt.IsZero() {
		t.Fatal("expected reset from retry_after")
	}
}

func TestParseTextRetry(t *testing.T) {
	body := `rate limited retry-after: 120 remaining: 3 limit: 50`
	info := Parse(body)
	if info.Remaining != 3 || info.Limit != 50 {
		t.Fatalf("got %+v", info)
	}
	if info.ResetAt.IsZero() {
		t.Fatal("expected reset")
	}
}
