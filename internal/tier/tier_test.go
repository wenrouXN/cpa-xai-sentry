package tier_test

import (
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/tier"
)

func TestClassify(t *testing.T) {
	if tier.Classify("", "", "xai-heavy-1.json", nil) != tier.Heavy {
		t.Fatal("heavy")
	}
	if tier.Classify("super gork", "", "a.json", nil) != tier.Super {
		t.Fatal("super")
	}
	if tier.Classify("", "free", "x.json", nil) != tier.Free {
		t.Fatal("free")
	}
	if tier.Classify("", "", "xai-abc.json", nil) != tier.Unknown {
		t.Fatal("unknown")
	}
}

func TestProtect(t *testing.T) {
	if !tier.ProtectFromAutoTrash(tier.Super) || !tier.ProtectFromAutoTrash(tier.Heavy) || !tier.ProtectFromAutoTrash(tier.Unknown) {
		t.Fatal("protected tiers")
	}
	if tier.ProtectFromAutoTrash(tier.Free) {
		t.Fatal("free not protected")
	}
}
