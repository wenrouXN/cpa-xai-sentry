package cpaapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Identity is a resolved xAI auth file identity for display/ops.
type Identity struct {
	AuthIndex string
	FileName  string
	Email     string
	Disabled  bool
	Note      string
	Label     string
	Provider  string
	Token     string
	BaseURL   string
}

// Resolver maps auth_index / filename / email to a concrete auth file.
type Resolver struct {
	Client *Client
	TTL    time.Duration

	mu       sync.Mutex
	loadedAt time.Time
	byIndex  map[string]Identity
	byFile   map[string]Identity
	byEmail  map[string]Identity
	byStem   map[string]Identity
	all      []Identity
}

func NewResolver(c *Client) *Resolver {
	return &Resolver{
		Client:  c,
		TTL:     30 * time.Second,
		byIndex: map[string]Identity{},
		byFile:  map[string]Identity{},
		byEmail: map[string]Identity{},
		byStem:  map[string]Identity{},
	}
}

func (r *Resolver) Invalidate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadedAt = time.Time{}
}

func (r *Resolver) Ensure(ctx context.Context) error {
	if r == nil || r.Client == nil {
		return nil
	}
	r.mu.Lock()
	ttl := r.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	fresh := !r.loadedAt.IsZero() && time.Since(r.loadedAt) < ttl && len(r.all) > 0
	r.mu.Unlock()
	if fresh {
		return nil
	}
	return r.Reload(ctx)
}

func (r *Resolver) Reload(ctx context.Context) error {
	if r == nil || r.Client == nil {
		return nil
	}
	ids := make([]Identity, 0, 256)
	seenFile := map[string]struct{}{}

	// Prefer management API when key works.
	if files, err := r.Client.ListAuthFiles(ctx); err == nil {
		for _, f := range files {
			if !IsXAIName(f.Name, f.Provider) {
				continue
			}
			name := strings.TrimSpace(f.Name)
			email := strings.TrimSpace(f.Email)
			if email == "" {
				email = EmailFromFileName(name)
			}
			id := Identity{
				AuthIndex: name,
				FileName:  name,
				Email:     email,
				Disabled:  f.Disabled,
				Provider:  firstNonEmpty(f.Provider, "xai"),
			}
			ids = append(ids, id)
			seenFile[strings.ToLower(name)] = struct{}{}
		}
	}

	// Always merge local dir scan — ground truth for email/filename/token.
		if r.Client.AuthDir != "" {
			entries, err := os.ReadDir(r.Client.AuthDir)
			if err == nil {
				for _, e := range entries {
					if e.IsDir() {
						continue
					}
					name := e.Name()
					if !strings.HasSuffix(strings.ToLower(name), ".json") {
						continue
					}
					if !IsXAIName(name, "") {
						continue
					}
					email := EmailFromFileName(name)
					note, label, tok, base := "", "", "", ""
					dis := false
					raw, err := os.ReadFile(filepath.Join(r.Client.AuthDir, name))
					if err == nil {
						email2, n, l, t, b, d := PeekAuthMeta(raw)
						if email2 != "" {
							email = email2
						}
						note, label, tok, base, dis = n, l, t, b, d
					}
					key := strings.ToLower(name)
					if _, ok := seenFile[key]; ok {
						for i := range ids {
							if strings.EqualFold(ids[i].FileName, name) {
								if ids[i].Email == "" {
									ids[i].Email = email
								}
								if ids[i].Note == "" {
									ids[i].Note = note
								}
								if ids[i].Label == "" {
									ids[i].Label = label
								}
								if ids[i].Token == "" {
									ids[i].Token = tok
								}
								if ids[i].BaseURL == "" {
									ids[i].BaseURL = base
								}
								ids[i].Disabled = dis
							}
						}
						continue
					}
					ids = append(ids, Identity{
						AuthIndex: name, FileName: name, Email: email,
						Note: note, Label: label, Provider: "xai",
						Token: tok, BaseURL: base, Disabled: dis,
					})
					seenFile[key] = struct{}{}
				}
			}
		}
	byIndex := map[string]Identity{}
	byFile := map[string]Identity{}
	byEmail := map[string]Identity{}
	byStem := map[string]Identity{}
	for _, id := range ids {
		if id.FileName != "" {
			byFile[strings.ToLower(id.FileName)] = id
			stem := strings.TrimSuffix(id.FileName, filepath.Ext(id.FileName))
			byStem[strings.ToLower(stem)] = id
			for _, h := range HashCandidates(id.FileName, id.Email, stem) {
				byIndex[strings.ToLower(h)] = id
			}
		}
		if id.AuthIndex != "" {
			byIndex[strings.ToLower(id.AuthIndex)] = id
		}
		if id.Email != "" {
			byEmail[strings.ToLower(id.Email)] = id
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.all = ids
	r.byIndex = byIndex
	r.byFile = byFile
	r.byEmail = byEmail
	r.byStem = byStem
	r.loadedAt = time.Now()
	return nil
}

// Resolve tries auth_index, filename, email.
func (r *Resolver) Resolve(authIndex, fileName, email string) (Identity, bool) {
	if r == nil {
		return Identity{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	try := func(m map[string]Identity, k string) (Identity, bool) {
		if k == "" {
			return Identity{}, false
		}
		id, ok := m[strings.ToLower(strings.TrimSpace(k))]
		return id, ok
	}
	if id, ok := try(r.byIndex, authIndex); ok {
		return id, true
	}
	if id, ok := try(r.byFile, fileName); ok {
		return id, true
	}
	if id, ok := try(r.byStem, fileName); ok {
		return id, true
	}
	if id, ok := try(r.byStem, authIndex); ok {
		return id, true
	}
	if id, ok := try(r.byFile, authIndex); ok {
		return id, true
	}
	if id, ok := try(r.byEmail, email); ok {
		return id, true
	}
	if e := EmailFromFileName(fileName); e != "" {
		if id, ok := try(r.byEmail, e); ok {
			return id, true
		}
	}
	if e := EmailFromFileName(authIndex); e != "" {
		if id, ok := try(r.byEmail, e); ok {
			return id, true
		}
	}
	return Identity{}, false
}

func (r *Resolver) All() []Identity {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Identity, len(r.all))
	copy(out, r.all)
	return out
}

func IsXAIName(name, provider string) bool {
	p := strings.ToLower(provider)
	if p == "xai" || p == "grok" {
		return true
	}
	n := strings.ToLower(name)
	return strings.HasPrefix(n, "xai-") || strings.Contains(n, "grok") || strings.Contains(n, "xai_")
}

func EmailFromFileName(name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	base = strings.TrimPrefix(base, "xai-")
	base = strings.TrimPrefix(base, "xai_")
	base = strings.TrimPrefix(base, "grok-")
	if strings.Contains(base, "@") {
		return base
	}
	return ""
}

func PeekAuthMeta(raw []byte) (email, note, label, token, baseURL string, disabled bool) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", "", "", "", "", false
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		return ""
	}
	email = get("email", "Email", "account", "Account")
	note = get("note", "Note", "remark", "Remark")
	label = get("label", "Label")
	token = get("access_token", "AccessToken", "token", "Token", "api_key", "apiKey")
	baseURL = get("base_url", "BaseURL", "baseUrl", "endpoint", "api_base")
	if v, ok := m["disabled"].(bool); ok {
		disabled = v
	}
	if email == "" {
		if meta, ok := m["metadata"].(map[string]any); ok {
			if s, ok := meta["email"].(string); ok {
				email = strings.TrimSpace(s)
			}
		}
	}
	return email, note, label, token, baseURL, disabled
}

func HashCandidates(fileName, email, stem string) []string {
	out := make([]string, 0, 12)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		sum := sha256.Sum256([]byte(s))
		h := hex.EncodeToString(sum[:])
		out = append(out, h, h[:16], h[:12])
	}
	add(fileName)
	add(stem)
	add(email)
	add("xai:" + email)
	add("xai:" + fileName)
	add("auth:" + fileName)
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// LooksLikeOpaqueID reports whether s looks like a host-generated opaque id.
func LooksLikeOpaqueID(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, "@") || strings.Contains(s, "/") || strings.HasSuffix(strings.ToLower(s), ".json") {
		return false
	}
	if strings.HasPrefix(strings.ToLower(s), "xai-") {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') || c == '-') {
			return false
		}
	}
	return len(s) >= 12
}

// DisplayName picks best human label.
func DisplayName(email, fileName, authIndex string) string {
	if strings.TrimSpace(email) != "" {
		return strings.TrimSpace(email)
	}
	if e := EmailFromFileName(fileName); e != "" {
		return e
	}
	if e := EmailFromFileName(authIndex); e != "" {
		return e
	}
	if strings.TrimSpace(fileName) != "" && !LooksLikeOpaqueID(fileName) {
		return strings.TrimSpace(fileName)
	}
	if strings.TrimSpace(authIndex) != "" {
		return "未解析 · " + shortID(authIndex)
	}
	return "未知账号"
}

func shortID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 12 {
		return s
	}
	return s[:8] + "…"
}
