package regjob

import (
	"context"
	"strings"
	"time"
)

// CanReloginEmail reports whether email is in 8788 local inventory (hence has password for relogin path).
func (r *Runner) CanReloginEmail(ctx context.Context, email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false
	}
	set := r.EmailSet(ctx, false)
	return set[email]
}

// EmailSet returns lowercased emails from 8788. Cached ~120s.
func (r *Runner) EmailSet(ctx context.Context, force bool) map[string]bool {
	r.mu.Lock()
	if !force && r.emailCache != nil && time.Since(r.emailCacheAt) < 120*time.Second {
		out := make(map[string]bool, len(r.emailCache))
		for k, v := range r.emailCache {
			out[k] = v
		}
		r.mu.Unlock()
		return out
	}
	cli := r.Client
	enabled := r.Cfg.RegisterEnabled
	r.mu.Unlock()

	out := map[string]bool{}
	if !enabled || cli == nil {
		return out
	}
	emails, err := cli.ListEmails(ctx)
	if err != nil {
		// keep stale cache on error
		r.mu.Lock()
		if r.emailCache != nil {
			for k, v := range r.emailCache {
				out[k] = v
			}
		}
		r.mu.Unlock()
		return out
	}
	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" {
			out[e] = true
		}
	}
	r.mu.Lock()
	r.emailCache = out
	r.emailCacheAt = time.Now()
	r.mu.Unlock()
	// copy
	cp := make(map[string]bool, len(out))
	for k, v := range out {
		cp[k] = v
	}
	return cp
}

// StartRelogin triggers 8788 relogin for emails that exist in local inventory.
// Non-local emails are filtered out and reported in skipped.
func (r *Runner) StartRelogin(ctx context.Context, emails []string, source string) (map[string]any, error) {
	r.mu.Lock()
	cfg := r.Cfg
	cli := r.Client
	r.mu.Unlock()
	if !cfg.RegisterEnabled {
		return nil, errf("注册总开关未打开")
	}
	if cli == nil {
		return nil, errf("client nil")
	}
	set := r.EmailSet(ctx, true)
	local := make([]string, 0, len(emails))
	skipped := make([]string, 0)
	seen := map[string]bool{}
	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		if set[e] {
			local = append(local, e)
		} else {
			skipped = append(skipped, e)
		}
	}
	if len(local) == 0 {
		return map[string]any{"ok": false, "local": local, "skipped": skipped}, errf("没有可重登的 8788 本地账号（需本地有密码）")
	}
	if cfg.RegisterDryRun {
		r.log("info", "【注册】重登 dry_run · 原因："+source+" · n="+itoa(len(local)))
		return map[string]any{"ok": true, "dry_run": true, "local": local, "skipped": skipped}, nil
	}
	conc := 2
	if cfg.RegisterReloginConcurrency > 0 {
		conc = cfg.RegisterReloginConcurrency
	}
	resp, err := cli.Relogin(ctx, local, conc)
	if err != nil {
		r.log("error", "【注册】重登失败 · 原因："+err.Error())
		return map[string]any{"ok": false, "local": local, "skipped": skipped, "error": err.Error()}, err
	}
	r.log("info", "【注册】重登 · 原因："+source+" · 本地"+itoa(len(local))+" · 跳过"+itoa(len(skipped)))
	return map[string]any{"ok": true, "local": local, "skipped": skipped, "task": resp}, nil
}

// TryAuth401Relogin is called from guard when auth_401 hits; returns true if relogin was attempted (caller should skip candidate).
func (r *Runner) TryAuth401Relogin(ctx context.Context, email, auth string) (attempted bool, reason string) {
	r.mu.Lock()
	cfg := r.Cfg
	r.mu.Unlock()
	if !cfg.RegisterEnabled || !cfg.RegisterReloginOnAuth401 {
		return false, "relogin_on_auth401_off"
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false, "no_email"
	}
	if !r.CanReloginEmail(ctx, email) {
		return false, "not_in_8788"
	}
	// streak limit per email
	max := cfg.RegisterReloginMaxStreak
	if max <= 0 {
		max = 2
	}
	r.mu.Lock()
	if r.reloginStreak == nil {
		r.reloginStreak = map[string]int{}
	}
	if r.reloginStreak[email] >= max {
		r.mu.Unlock()
		return false, "max_relogin_streak"
	}
	// cooldown: don't spam same email within 10m
	if r.reloginLastAt != nil {
		if t, ok := r.reloginLastAt[email]; ok && time.Since(t) < 10*time.Minute {
			r.mu.Unlock()
			return false, "relogin_cooldown"
		}
	} else {
		r.reloginLastAt = map[string]time.Time{}
	}
	r.reloginStreak[email]++
	r.reloginLastAt[email] = time.Now()
	r.mu.Unlock()

	_, err := r.StartRelogin(ctx, []string{email}, "auth_401")
	if err != nil {
		return true, "attempted_fail:" + err.Error()
	}
	return true, "attempted"
}

// MarkReloginSuccess clears streak for email after later success (optional).
func (r *Runner) MarkReloginSuccess(email string) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return
	}
	r.mu.Lock()
	if r.reloginStreak != nil {
		delete(r.reloginStreak, email)
	}
	r.mu.Unlock()
}

func errf(s string) error { return &simpleErr{s} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
