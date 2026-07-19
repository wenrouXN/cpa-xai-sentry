package quota

import (
	"testing"
	"time"
)

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

func TestFreeUsageExhaustedEstimateWithCustomLimit(t *testing.T) {
	// Body with numbers → keep real limit, ignore estimate
	body := `tokens (actual/limit): 900000/1000000`
	info := FreeUsageExhaustedEstimateWith(body, time.Time{}, 2_000_000)
	if info.Limit != 1_000_000 || info.Used != 900_000 {
		t.Fatalf("real body preferred: %+v", info)
	}
	// No numbers → use estimate
	info2 := FreeUsageExhaustedEstimateWith("free usage exhausted", time.Time{}, 1_000_000)
	if info2.Limit != 1_000_000 || info2.Used != 1_000_000 || info2.Source != "free_usage_exhausted_est" {
		t.Fatalf("estimate fallback: %+v", info2)
	}
	// Non-positive estimate → default 2M
	info3 := FreeUsageExhaustedEstimateWith("free usage exhausted", time.Time{}, 0)
	if info3.Limit != DefaultFreeQuotaPerAccount {
		t.Fatalf("default estimate: %+v", info3)
	}
}

func TestEffective(t *testing.T) {
	if Effective(0) != DefaultFreeQuotaPerAccount {
		t.Fatal("0 should default")
	}
	if Effective(1_000_000) != 1_000_000 {
		t.Fatal("positive kept")
	}
}
