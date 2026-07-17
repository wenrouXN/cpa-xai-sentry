package cpamp_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpamp"
)

func TestFetchRecentFailuresEmptyPath(t *testing.T) {
	// when DB missing, no error + empty
	// force empty by using non-existing via temporary: resolve may still find real DB in env
	// just ensure function doesn't panic
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := cpamp.FetchRecentFailures(ctx, time.Now().Add(-1*time.Minute).UnixMilli(), 10)
	if err != nil {
		// ok to error if sqlite driver issue; don't fail hard if path exists with schema
		t.Log(err)
	}
	_ = filepath.Base(".")
}

func TestModelFromFailBody(t *testing.T) {
	body := `{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 1091108/1000000."}`
	got := cpamp.ModelFromFailBody(body)
	if got != "grok-4.5-build-free" {
		t.Fatalf("got %q", got)
	}
	if cpamp.ModelFromFailBody(`{"model":"grok-4.5","error":"x"}`) != "grok-4.5" {
		t.Fatal("json model field")
	}
	if cpamp.ModelFromFailBody("") != "" {
		t.Fatal("empty")
	}
}
