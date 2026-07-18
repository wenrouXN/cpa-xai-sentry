package errorfp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
)

// Result is the stable identity of one actual error response.
type Result struct {
	Shape      string
	SuggestKey string
	Code       string
	Message    string
}

var (
	uuidRe = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	numRe  = regexp.MustCompile(`\b\d{4,}\b`)
	urlRe  = regexp.MustCompile(`https?://\S+`)
	wsRe   = regexp.MustCompile(`\s+`)
)

// Build derives identity from HTTP status + business code/type + normalized message.
// Status alone is never an error class.
func Build(sample string, status int) Result {
	code, msg := fields(sample)
	low := strings.ToLower(sample + " " + msg)
	codeLow := strings.ToLower(code)

	if status == 429 &&
		(strings.Contains(codeLow, "free-usage") || strings.Contains(low, "included free usage")) {
		return Result{Shape: "free_usage_429", SuggestKey: "free_usage_429", Code: code, Message: msg}
	}
	if (status == 403 || status == 0) &&
		(strings.Contains(codeLow, "permission-denied") || strings.Contains(low, "permission-denied")) &&
		strings.Contains(low, "access to the chat endpoint is denied") {
		return Result{Shape: "permission_403", SuggestKey: "permission_403", Code: code, Message: msg}
	}

	norm := Normalize(msg)
	if norm == "" {
		norm = Normalize(firstLine(sample))
	}
	canonical := strconv.Itoa(status) + "|" + codeLow + "|" + norm
	sum := sha256.Sum256([]byte(canonical))
	shape := "fp_" + hex.EncodeToString(sum[:8])
	return Result{Shape: shape, SuggestKey: "reason:" + shape, Code: code, Message: msg}
}

func Normalize(s string) string {
	s = strings.ToLower(html.UnescapeString(strings.TrimSpace(s)))
	s = uuidRe.ReplaceAllString(s, "<uuid>")
	s = urlRe.ReplaceAllString(s, "<url>")
	s = numRe.ReplaceAllString(s, "<n>")
	s = wsRe.ReplaceAllString(s, " ")
	if len(s) > 320 {
		s = s[:320]
	}
	return strings.TrimSpace(s)
}

func firstLine(sample string) string {
	sample = html.UnescapeString(sample)
	if i := strings.IndexByte(sample, '\n'); i >= 0 {
		sample = sample[:i]
	}
	return strings.TrimSpace(sample)
}

func fields(sample string) (code, msg string) {
	// Prefer the full sample so pretty / multi-line JSON keeps code+message.
	// Fall back to the first line for log prefixes like `Post "https://…": {...}`.
	payloads := []string{strings.TrimSpace(html.UnescapeString(sample)), firstLine(sample)}
	var v any
	parsed := false
	for _, payload := range payloads {
		if payload == "" {
			continue
		}
		if json.Unmarshal([]byte(payload), &v) == nil {
			parsed = true
			break
		}
		// body may be prefixed: `Post "...": {json}`
		if i := strings.Index(payload, "{"); i >= 0 {
			if json.Unmarshal([]byte(payload[i:]), &v) == nil {
				parsed = true
				break
			}
		}
	}
	if parsed {
		var walk func(any)
		walk = func(x any) {
			switch t := x.(type) {
			case map[string]any:
				for _, k := range []string{"code", "type", "error_code"} {
					if code == "" {
						if z, ok := t[k]; ok {
							code = fmt.Sprint(z)
						}
					}
				}
				for _, k := range []string{"error", "message", "detail"} {
					if msg != "" {
						break
					}
					switch z := t[k].(type) {
					case string:
						msg = z
					case map[string]any:
						walk(z)
					}
				}
				if msg == "" {
					for _, z := range t {
						walk(z)
					}
				}
			case []any:
				for _, z := range t {
					walk(z)
				}
			}
		}
		walk(v)
	}
	if msg == "" {
		msg = firstLine(sample)
	}
	return strings.TrimSpace(code), strings.TrimSpace(msg)
}
