package tier

import "strings"

type Tier string

const (
	Free    Tier = "free"
	Super   Tier = "super"
	Heavy   Tier = "heavy"
	Unknown Tier = "unknown"
)

// Classify uses note/label/filename and optional string fields from auth JSON.
func Classify(note, label, fileName string, fields map[string]any) Tier {
	blob := strings.ToLower(note + " " + label + " " + fileName)
	for _, v := range fields {
		if s, ok := v.(string); ok {
			blob += " " + strings.ToLower(s)
		}
	}
	// order: heavy before super before free (avoid free substring false positives if needed)
	switch {
	case strings.Contains(blob, "heavy"):
		return Heavy
	case strings.Contains(blob, "super"):
		return Super
	case strings.Contains(blob, "free"):
		return Free
	default:
		return Unknown
	}
}

// ProtectFromAutoTrash: Super/Heavy always; Unknown treated cautiously (no auto-trash).
func ProtectFromAutoTrash(t Tier) bool {
	return t == Super || t == Heavy || t == Unknown
}
