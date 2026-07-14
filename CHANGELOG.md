# Changelog

## 1.1.18

### Align config persistence with official CLIProxyAPI plugin model
- Panel save dual-writes:
  1. `runtime-overrides.json` (local)
  2. CPA host `PUT /v0/management/plugins/cpa-xai-sentry/config` (GET+merge+PUT)
- Keeps existing host `enabled`/`priority`/secrets via merge.


## 1.1.17

### Broader runtime-overrides + persist viewer
- Persist ops knobs: `tick_seconds`, permission/401 cool-down seconds, `max_reset_seconds`,
  `reopen_foreign_disabled`, trash/cpamp floor flags (in addition to patrol fields).
- Panel: save writes disk; **查看持久化** shows overrides path + on-disk JSON + live values.
- Secrets remain YAML-only.


## 1.1.16

### Persist patrol config + job history across restarts
- Root cause: `patrol_batch_size` etc. only lived in memory; `runtime-overrides.json` only saved bool switches, so restart reloaded YAML `50`.
- Root cause: patrol job history was package memory only — docker/plugin restart wiped the 巡查 list.
- Now: all patrol knobs saved in `runtime-overrides.json` and reapplied on load.
- Now: finished jobs saved to `patrol-history.json` next to state and reloaded on start.


## 1.1.15

### Panel layout copy
- Runtime line `维护每30s · 巡查… · 密钥` next to version badge.
- Schedule `巡查：上次… · 下次…` under 巡查中心 actions.
- Unified wording: 巡检 → 巡查.


## 1.1.14

### Patrol probe
- Default base: `https://cli-chat-proxy.grok.com/v1` when auth file has no base_url.
- Probe order: **`/responses` first** (`input` + `max_output_tokens`, never `max_tokens`), then `/chat/completions` on 404/405/shape errors.
- Grok CLI headers: `x-authenticateresponse`, `x-grok-client-identifier`, `x-grok-client-version=0.2.93`, `x-xai-token-auth`.
- Chat fallback content `"hi"`.


## 1.1.13

### Full patrol = sentry-not-disabled (schedulable) accounts
- **Full** no longer means "all CPA files" or "CPA-enabled only".
- Full = accounts **not** in sentry cool-down / 候删 / 永久禁用 / 垃圾箱 (still may receive traffic).
- CPA file `disabled` does **not** exclude a target if sentry still treats it as open — probe with token, real HTTP decides.
- No synthetic disabled→429 alignment.
- Cooldown mode = sentry cool-down accounts only.


## 1.1.12

### Patrol: real probe only (no synthetic disabled→429)
- Removed `align_disabled_quota` heuristic (disabled file ≠ proven free-usage).
- Full patrol includes **disabled** xAI auths when token is available; probe uses token directly.
- Policy follows **real** HTTP status/body only (200/429/403/…).


## 1.1.11

### Fix: same-tick 「到期恢复」vs「冷却补关」fight
- Tick order: **recover due cool-downs first**, then CPA file reassert/sync.
- Do not reassert-close when `recover_at` is already due (account should open).
- Stops duplicate opposite logs in the same second for one account.


## 1.1.10

### Patrol summary: count 401/403 as 权限信号
- Previously 401/403 were logged but not included in 冷却/异常, so
  `探测N · 存活A · 冷却0 · 异常0` could leave N-A accounts unaccounted.
- Summary now: 探测 · 存活 · 冷却信号 · **权限信号** · 异常 (+ 已禁用额度对齐).


## 1.1.9

### Fix: disabled-quota align actually stamps cool-down
- Direct `cooldown_quota` write (not only HandleUsage synthetic) when CPA file disabled + sentry Active.
- Skip 401/403-signaled accounts so they are not rebranded as free-usage.

## 1.1.8

### Fix: full patrol skips CPA-disabled 429 accounts
- Full mode only probes **enabled** files, so accounts already disabled for free-usage never get cooled in sentry state.
- Patrol now **aligns** CPA-disabled + sentry-Active xAI accounts as `free_usage_429` cool-down (no HTTP probe).
- Example: `jc3f4dnnjh@…` file disabled, panel still active → becomes cooldown_quota on next full patrol.


## 1.1.7

### Fix: duplicate cool-down action logs
- `applyCooldown` logged `cooldown` twice (before + after CPA SetDisabled).
- Single log entry now; `cooldown_failed` still logged on disable failure.
- Patrol targets deduped by email/file.


## 1.1.6

### Fix: patrol all accounts HTTP 400
- Root cause: probe body sent `max_tokens` to `/v1/responses`, which rejects it.
- Use endpoint-specific payloads (`chat/completions` → `max_tokens`; `responses` → `max_output_tokens` only).
- Prefer `/chat/completions` first; on shape-related 400, fall through to the other path.
- Do not treat request-shape 400 as account death.


## 1.1.5

### Fix: permission_403 streak stuck at 3
- Cool-down **recover** (`ResetToActive`) no longer clears `streaks`.
- Streak ladders (e.g. ≥3 cooldown, ≥15 disable) now accumulate across cool-down cycles.
- Streaks still clear on **successful request** (streak mode) and **panel 启用**.
- Day fail count is not the same as continuous N (hits vs streak).


## 1.1.4

### Action log readability
- Human labels: 冷却补关 / 到期恢复 / 自愈打开 / CPA禁用对齐 …
- Full sentences with 【标签】prefix for key events.
- `owned_disable_was_enabled` → 「自有冷却期间文件被打开，已重新关闭」.
- `permission_denied` → 「权限拒绝(403)」; `recover_at` → 「冷却到期自动恢复」.


## 1.1.3

### Foreign-disable scan only on manual maintenance
- Periodic Tick: recover cool-downs, trash purge, **owned reassert only** — no `file_disabled_sync` / unowned reopen spam.
- Panel **立即维护** (`TickManual`): one-shot full scan — open unowned (if enabled) or mark CPA已禁用.
- Rationale: unowned disables are rare after one audit; continuous scan only creates noise and races.


## 1.1.2

### Ops preference restored: open unowned disables
- Default `reopen_foreign_disabled=true` again (open non-owned disables; wait next error).
- **Still hard-protects** owned cool-downs / panel permanent disable (mutex, ownership map, reassert, no ResetToActive on cool-downs).
- Production config set `reopen_foreign_disabled: true`.


## 1.1.1

### Fixed: conservative mode overwriting cool-down as CPA已禁用
- `file_disabled_sync` must not run when account is owned cool-down/manual.
- Triple-check identity ownership before tagging cpa_file_disabled.
- Lock bulk cooldown path.

## 1.1.0

### Hard fix: stop cool-down being reopened as "非自有禁用"
Root cause was structural, not one-account matching:
1. Self-heal reopen default ON + `ResetToActive` wiped cool-downs after 429/403.
2. HandleUsage and Tick raced on ownership.

Changes:
- **Default `reopen_foreign_disabled=false`** (safe). Unknown disables stay closed; only `recover_at` / panel enable open files.
- Serialize **HandleUsage / Tick / ManualEnable/Disable** with a mutex.
- Self-heal (if explicitly enabled) **never** opens owned cool-downs and **never** `ResetToActive` on protectable accounts; file enable only.
- Owned cool-down with file enabled still **re-disables** (`cooldown_reassert`).
- Production config sets `reopen_foreign_disabled: false`.


## 1.0.11

### Fixed: owned cool-down file left enabled after false reopen
- Precompute owned disable identities (auth_index/file/email).
- If CPA file is enabled but account is owned cool-down/manual → **re-disable** (`cooldown_reassert`).
- Self-heal only runs for non-owned disables.


## 1.0.10

### Fixed: cooldown then immediate "打开非自有禁用"
- Protect if **any** matched identity row is owned (not only pickAcc winner).
- 2-minute grace after `cooldown`/`candidate`/`manual_disable` action even if state briefly Active.
- Persist ownership (Log+Save) before CPA SetDisabled to shrink tick race.
- Do not ResetToActive owned cool-downs during self-heal.


## 1.0.9

### Panel sync: dual time columns + parallel refresh
- Account table: **最后请求** (CPAMP) and **最后动作** (sentry action log).
- Stamp `last_action` / `last_action_at` on every action log; backfill from retained logs on load.
- Sort by max(request, action) activity.
- Refresh loads `/state` + `/logs` in parallel; interval 8s; log timestamps include date.


## 1.0.8

### Fixed: unowned reopen despite auth_index cool-down
- Parse CPA auth-files `auth_index` / `id` / `path` / `account`.
- Match disabled files to sentry cool-downs by **auth_index first**, then file/email.
- Last-chance guard: never reopen if any owned cool-down shares email/file/auth_index.


## 1.0.7

### Action log pagination
- `/logs?limit=&offset=&q=` returns newest-first pages (`page.total/has_more/next_offset`).
- Panel loads 80 lines first; **更早** loads older pages; search uses server `q`.
- Log expiry still independent of counters (see below).


## 1.0.6

### Fixed: false "unowned" reopen on owned cool-downs
- Match CPA list entries to sentry accounts by **filename basename**, path suffix, and email derived from `xai-<email>.json` when list omits email.
- Prevents 429/`plugin_auto` cool-downs from being treated as untracked and reopened (e.g. `xai-5w4ggr8txx@...json`).


## 1.0.4

### Self-heal unowned disables (operator preference)
- Default `reopen_foreign_disabled=true`: if a CPA file is disabled but sentry cannot prove ownership (`plugin_auto` cool-down / panel `user_manual`), **reopen** it and `ResetToActive`.
- Next real usage/patrol error re-applies policy and re-stamps ownership (self-heal).
- Still **protects** real cool-downs, 候删, and panel permanent disables.
- `cpa_file_disabled` tags are no longer sticky protect — they self-heal on the next tick.
- Set `reopen_foreign_disabled=false` for the old conservative "keep closed + mark CPA已禁用" mode.


## 1.0.3

### Auth closed-loop hardening
- `CanAutoReenable`: `plugin_auto` sufficient; never open manual/CPA-file locks
- Candidate path also disables CPA file + stamps ownership/recover_at
- Permanent disable stamps ownership before CPA I/O
- Duplicate-account prune prefers cool-down/ownership rows over empty Active shells
- Regression tests for cooldown→recover, manual never auto-open, prune, legacy owner


## 1.0.2

### Fixed (plugin_auto ownership holes)
- Protect **any** cool-down/候删/manual/plugin_auto residue from foreign-reopen, not only exact state+source pairs.
- Pick the best matching account row when multiple state keys map to one auth file (email/file duplicates).
- Do not scrub away `plugin_auto` on Active; if `recover_at` is still in the future, **repair** cool-down state instead.
- Stamp cool-down ownership before CPA disable to close race with concurrent ticks.


## 1.0.1

### Fixed
- **Stop auto-reopening operator disables**: maintenance no longer re-enables CPA auth files that are disabled outside sentry cool-down/manual locks.
- Disabled files are synced to panel state as `CPA已禁用` (`cpa_file_disabled`) so they stay locked until you click **启用**.
- New config `reopen_foreign_disabled` (default `false`) restores the old reopen behaviour only if explicitly enabled.

# Changelog

## 1.1.18

### Align config persistence with official CLIProxyAPI plugin model
- Panel save dual-writes:
  1. `runtime-overrides.json` (local)
  2. CPA host `PUT /v0/management/plugins/cpa-xai-sentry/config` (GET+merge+PUT)
- Keeps existing host `enabled`/`priority`/secrets via merge.


## 1.1.17

### Broader runtime-overrides + persist viewer
- Persist ops knobs: `tick_seconds`, permission/401 cool-down seconds, `max_reset_seconds`,
  `reopen_foreign_disabled`, trash/cpamp floor flags (in addition to patrol fields).
- Panel: save writes disk; **查看持久化** shows overrides path + on-disk JSON + live values.
- Secrets remain YAML-only.


## 1.1.16

### Persist patrol config + job history across restarts
- Root cause: `patrol_batch_size` etc. only lived in memory; `runtime-overrides.json` only saved bool switches, so restart reloaded YAML `50`.
- Root cause: patrol job history was package memory only — docker/plugin restart wiped the 巡查 list.
- Now: all patrol knobs saved in `runtime-overrides.json` and reapplied on load.
- Now: finished jobs saved to `patrol-history.json` next to state and reloaded on start.


## 1.1.15

### Panel layout copy
- Runtime line `维护每30s · 巡查… · 密钥` next to version badge.
- Schedule `巡查：上次… · 下次…` under 巡查中心 actions.
- Unified wording: 巡检 → 巡查.


## 1.1.14

### Patrol probe
- Default base: `https://cli-chat-proxy.grok.com/v1` when auth file has no base_url.
- Probe order: **`/responses` first** (`input` + `max_output_tokens`, never `max_tokens`), then `/chat/completions` on 404/405/shape errors.
- Grok CLI headers: `x-authenticateresponse`, `x-grok-client-identifier`, `x-grok-client-version=0.2.93`, `x-xai-token-auth`.
- Chat fallback content `"hi"`.


## 1.1.13

### Full patrol = sentry-not-disabled (schedulable) accounts
- **Full** no longer means "all CPA files" or "CPA-enabled only".
- Full = accounts **not** in sentry cool-down / 候删 / 永久禁用 / 垃圾箱 (still may receive traffic).
- CPA file `disabled` does **not** exclude a target if sentry still treats it as open — probe with token, real HTTP decides.
- No synthetic disabled→429 alignment.
- Cooldown mode = sentry cool-down accounts only.


## 1.1.12

### Patrol: real probe only (no synthetic disabled→429)
- Removed `align_disabled_quota` heuristic (disabled file ≠ proven free-usage).
- Full patrol includes **disabled** xAI auths when token is available; probe uses token directly.
- Policy follows **real** HTTP status/body only (200/429/403/…).


## 1.1.11

### Fix: same-tick 「到期恢复」vs「冷却补关」fight
- Tick order: **recover due cool-downs first**, then CPA file reassert/sync.
- Do not reassert-close when `recover_at` is already due (account should open).
- Stops duplicate opposite logs in the same second for one account.


## 1.1.10

### Patrol summary: count 401/403 as 权限信号
- Previously 401/403 were logged but not included in 冷却/异常, so
  `探测N · 存活A · 冷却0 · 异常0` could leave N-A accounts unaccounted.
- Summary now: 探测 · 存活 · 冷却信号 · **权限信号** · 异常 (+ 已禁用额度对齐).


## 1.1.9

### Fix: disabled-quota align actually stamps cool-down
- Direct `cooldown_quota` write (not only HandleUsage synthetic) when CPA file disabled + sentry Active.
- Skip 401/403-signaled accounts so they are not rebranded as free-usage.

## 1.1.8

### Fix: full patrol skips CPA-disabled 429 accounts
- Full mode only probes **enabled** files, so accounts already disabled for free-usage never get cooled in sentry state.
- Patrol now **aligns** CPA-disabled + sentry-Active xAI accounts as `free_usage_429` cool-down (no HTTP probe).
- Example: `jc3f4dnnjh@…` file disabled, panel still active → becomes cooldown_quota on next full patrol.


## 1.1.7

### Fix: duplicate cool-down action logs
- `applyCooldown` logged `cooldown` twice (before + after CPA SetDisabled).
- Single log entry now; `cooldown_failed` still logged on disable failure.
- Patrol targets deduped by email/file.


## 1.1.6

### Fix: patrol all accounts HTTP 400
- Root cause: probe body sent `max_tokens` to `/v1/responses`, which rejects it.
- Use endpoint-specific payloads (`chat/completions` → `max_tokens`; `responses` → `max_output_tokens` only).
- Prefer `/chat/completions` first; on shape-related 400, fall through to the other path.
- Do not treat request-shape 400 as account death.


## 1.1.5

### Fix: permission_403 streak stuck at 3
- Cool-down **recover** (`ResetToActive`) no longer clears `streaks`.
- Streak ladders (e.g. ≥3 cooldown, ≥15 disable) now accumulate across cool-down cycles.
- Streaks still clear on **successful request** (streak mode) and **panel 启用**.
- Day fail count is not the same as continuous N (hits vs streak).


## 1.1.4

### Action log readability
- Human labels: 冷却补关 / 到期恢复 / 自愈打开 / CPA禁用对齐 …
- Full sentences with 【标签】prefix for key events.
- `owned_disable_was_enabled` → 「自有冷却期间文件被打开，已重新关闭」.
- `permission_denied` → 「权限拒绝(403)」; `recover_at` → 「冷却到期自动恢复」.


## 1.1.3

### Foreign-disable scan only on manual maintenance
- Periodic Tick: recover cool-downs, trash purge, **owned reassert only** — no `file_disabled_sync` / unowned reopen spam.
- Panel **立即维护** (`TickManual`): one-shot full scan — open unowned (if enabled) or mark CPA已禁用.
- Rationale: unowned disables are rare after one audit; continuous scan only creates noise and races.


## 1.1.2

### Ops preference restored: open unowned disables
- Default `reopen_foreign_disabled=true` again (open non-owned disables; wait next error).
- **Still hard-protects** owned cool-downs / panel permanent disable (mutex, ownership map, reassert, no ResetToActive on cool-downs).
- Production config set `reopen_foreign_disabled: true`.


## 1.1.1

### Fixed: conservative mode overwriting cool-down as CPA已禁用
- `file_disabled_sync` must not run when account is owned cool-down/manual.
- Triple-check identity ownership before tagging cpa_file_disabled.
- Lock bulk cooldown path.

## 1.1.0

### Hard fix: stop cool-down being reopened as "非自有禁用"
Root cause was structural, not one-account matching:
1. Self-heal reopen default ON + `ResetToActive` wiped cool-downs after 429/403.
2. HandleUsage and Tick raced on ownership.

Changes:
- **Default `reopen_foreign_disabled=false`** (safe). Unknown disables stay closed; only `recover_at` / panel enable open files.
- Serialize **HandleUsage / Tick / ManualEnable/Disable** with a mutex.
- Self-heal (if explicitly enabled) **never** opens owned cool-downs and **never** `ResetToActive` on protectable accounts; file enable only.
- Owned cool-down with file enabled still **re-disables** (`cooldown_reassert`).
- Production config sets `reopen_foreign_disabled: false`.


## 1.0.11

### Fixed: owned cool-down file left enabled after false reopen
- Precompute owned disable identities (auth_index/file/email).
- If CPA file is enabled but account is owned cool-down/manual → **re-disable** (`cooldown_reassert`).
- Self-heal only runs for non-owned disables.


## 1.0.10

### Fixed: cooldown then immediate "打开非自有禁用"
- Protect if **any** matched identity row is owned (not only pickAcc winner).
- 2-minute grace after `cooldown`/`candidate`/`manual_disable` action even if state briefly Active.
- Persist ownership (Log+Save) before CPA SetDisabled to shrink tick race.
- Do not ResetToActive owned cool-downs during self-heal.


## 1.0.9

### Panel sync: dual time columns + parallel refresh
- Account table: **最后请求** (CPAMP) and **最后动作** (sentry action log).
- Stamp `last_action` / `last_action_at` on every action log; backfill from retained logs on load.
- Sort by max(request, action) activity.
- Refresh loads `/state` + `/logs` in parallel; interval 8s; log timestamps include date.


## 1.0.8

### Fixed: unowned reopen despite auth_index cool-down
- Parse CPA auth-files `auth_index` / `id` / `path` / `account`.
- Match disabled files to sentry cool-downs by **auth_index first**, then file/email.
- Last-chance guard: never reopen if any owned cool-down shares email/file/auth_index.


## 1.0.7

### Action log pagination
- `/logs?limit=&offset=&q=` returns newest-first pages (`page.total/has_more/next_offset`).
- Panel loads 80 lines first; **更早** loads older pages; search uses server `q`.
- Log expiry still independent of counters (see below).


## 1.0.6

### Fixed: false "unowned" reopen on owned cool-downs
- Match CPA list entries to sentry accounts by **filename basename**, path suffix, and email derived from `xai-<email>.json` when list omits email.
- Prevents 429/`plugin_auto` cool-downs from being treated as untracked and reopened (e.g. `xai-5w4ggr8txx@...json`).


## 1.0.4

### Self-heal unowned disables (operator preference)
- Default `reopen_foreign_disabled=true`: if a CPA file is disabled but sentry cannot prove ownership (`plugin_auto` cool-down / panel `user_manual`), **reopen** it and `ResetToActive`.
- Next real usage/patrol error re-applies policy and re-stamps ownership (self-heal).
- Still **protects** real cool-downs, 候删, and panel permanent disables.
- `cpa_file_disabled` tags are no longer sticky protect — they self-heal on the next tick.
- Set `reopen_foreign_disabled=false` for the old conservative "keep closed + mark CPA已禁用" mode.


## 1.0.3

### Auth closed-loop hardening
- `CanAutoReenable`: `plugin_auto` sufficient; never open manual/CPA-file locks
- Candidate path also disables CPA file + stamps ownership/recover_at
- Permanent disable stamps ownership before CPA I/O
- Duplicate-account prune prefers cool-down/ownership rows over empty Active shells
- Regression tests for cooldown→recover, manual never auto-open, prune, legacy owner


## 1.0.2

### Fixed (plugin_auto ownership holes)
- Protect **any** cool-down/候删/manual/plugin_auto residue from foreign-reopen, not only exact state+source pairs.
- Pick the best matching account row when multiple state keys map to one auth file (email/file duplicates).
- Do not scrub away `plugin_auto` on Active; if `recover_at` is still in the future, **repair** cool-down state instead.
- Stamp cool-down ownership before CPA disable to close race with concurrent ticks.


## 1.0.0 — production release

### Added
- Error policy **escalation ladder** (e.g. 403: ≥3 cooldown, ≥15 permanent disable)
- Count modes: `streak` (success clears) / `total` (success keeps)
- Dynamic account state filters with live counts
- Live active labels (`正常·可用` / `正常·403×N` / …)
- Patrol job history with expandable per-job logs
- Unmatched error **shape split** into new policy keys
- Single version package `internal/version`

### Fixed
- Cooldown ↔ maintenance re-sync loop
- Scrub loop clearing policy streaks on active accounts
- Concurrent safety: `Get` / `AccountsSnapshot` return deep copies
- Patrol completion message race on unlocked `jobStatus`
- Policy ops permanent-disable button visibility
- State filter options not populated (`fillStateFilter` hook)

### Safety defaults
- Trash retention 7d; 402 never auto-trash; Super/Heavy protected
- Permanent disable requires `sentry_enabled`
- Atomic state save (tmp + rename, 0600)
