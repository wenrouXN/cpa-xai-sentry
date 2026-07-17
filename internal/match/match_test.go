package match_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/match"
)

func loadFixture(t *testing.T, name string) (int, string) {
	t.Helper()
	candidates := []string{
		filepath.Join("testdata", "fixtures", name),
		filepath.Join("..", "..", "testdata", "fixtures", name),
	}
	var b []byte
	var err error
	for _, p := range candidates {
		b, err = os.ReadFile(p)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	var f struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.StatusCode, f.Body
}

func TestClassifyFixtures(t *testing.T) {
	cases := []struct {
		file string
		want match.Signal
	}{
		{"free_usage_429.json", match.SignalFreeUsage429},
		{"spending_limit_402.json", match.SignalSpendingLimit402},
		{"permission_403.json", match.SignalPermission403},
		{"auth_401.json", match.SignalAuth401},
	}
	for _, tc := range cases {
		code, body := loadFixture(t, tc.file)
		got := match.Classify(code, body)
		if got.Signal != tc.want {
			t.Fatalf("%s: got %q want %q (reason=%s)", tc.file, got.Signal, tc.want, got.Reason)
		}
	}
}

func TestRegionNotPermission(t *testing.T) {
	got := match.Classify(403, `{"error":"model is not available in your region"}`)
	if got.Signal != match.SignalNone {
		t.Fatalf("region must not be permission_403, got %s", got.Signal)
	}
	if got.Reason != "region_block" {
		t.Fatalf("reason=%s", got.Reason)
	}
}

func TestGenericAccessDeniedNotPermission(t *testing.T) {
	cases := []string{
		`{"error":"Access Denied"}`,
		`access denied`,
	}
	for _, body := range cases {
		got := match.Classify(403, body)
		if got.Signal == match.SignalPermission403 {
			t.Fatalf("generic access denied must not be permission_403: body=%q reason=%s", body, got.Reason)
		}
	}
}

func Test402NeverLooksLikeDead(t *testing.T) {
	got := match.Classify(402, `{"code":"personal-team-blocked:spending-limit","error":"need a Grok subscription"}`)
	if got.Signal != match.SignalSpendingLimit402 {
		t.Fatalf("got %s", got.Signal)
	}
	if got.Kind != match.KindQuota {
		t.Fatalf("kind=%s", got.Kind)
	}
}

func TestSuccessUnmatched(t *testing.T) {
	got := match.Classify(200, `{"ok":true}`)
	if got.Signal != match.SignalNone {
		t.Fatalf("got %s", got.Signal)
	}
}
