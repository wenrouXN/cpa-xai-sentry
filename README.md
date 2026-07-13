# cpa-xai-sentry

CLIProxyAPI **native CGO plugin** for xAI / Grok account fleet:

- **Observe** request failures (429 / 402 / 403 / 401 / network / unmatched)
- **Guard** with optional auto cooldown / candidate / permanent disable / trash
- **Patrol** active probe jobs with expandable per-job logs
- **Web panel** for live account state, error policies, trash bin, action logs

Current version: **v1.0.0**

> Successor to `cpa-xai-quota-guard`. Run **only this plugin** for xAI free-usage guarding.

---

## Features

### Account live status (`账号实况`)
Status is **code + action**, not a blank “normal”:

| State | Meaning |
|---|---|
| `正常·可用` | Clean active, can take traffic |
| `正常·403×N` / `正常·观察中` | Still active, counting errors toward policy ladder |
| `429·额度冷却` | Free-usage exhausted → cooldown |
| `402·消费冷却` | Spending limit → cooldown |
| `403·权限冷却` | Permission denied → cooldown |
| `401·候删` | Auth invalid → candidate |
| `永久禁用` | Panel / policy permanent disable (no auto re-enable) |
| `CPA已禁用` | Auth file disabled outside sentry (rare; may be reopened) |
| `垃圾箱` | Soft-deleted snapshot, restorable |

State filter is **dynamic** (options with count > 0 only).

### Error policy ladder
Per-error multi-tier rules, e.g. **403**:

| Streak | Action |
|---|---|
| ≥ 3 | Cooldown (default 1800s) |
| ≥ 15 | **Permanent disable** (optional; configurable) |

Also supports:

- Count mode: **streak** (success clears) / **total** (success keeps)
- Builtin keys: `free_usage_429`, `spending_limit_402`, `auth_401`, `permission_403`, …
- Unmatched errors can be **split by shape** into new policy keys

### Patrol
- Manual full / cooldown-only probe
- Real xAI requests (consumes free quota)
- Results feed the **same** `HandleUsage` + policy path as live traffic
- Job history with expandable per-job logs

### Maintenance tick
- Recover cooldown when `recover_at` due → full clean active
- Reopen **non-sentry** disabled auth files; leave our cool-downs alone
- No more scrub-loop on active accounts that only have policy streaks

### Trash
- Snapshot → remove from CPA live set
- Default retention **7 days**, auto-purge
- Super/Heavy protected from auto-trash; **402 never trash**

---

## Architecture

```
CLIProxyAPI host
  └─ cpa-xai-sentry.so  (CGO plugin)
       ├─ usage hook → match → policy ladder → guard action
       ├─ patrol runner → same guard path
       ├─ tick (recover / reopen foreign / trash purge)
       └─ management HTTP panel (/v0/management/cpa-xai-sentry/*)
```

Key packages under `internal/`:

| Package | Role |
|---|---|
| `match` | Classify HTTP status + body → signal |
| `errorsig` | Catalog keys, human messages, shapes |
| `policy` | Decide action from streak + escalations |
| `guard` | Apply cooldown / disable / trash / tick |
| `patrol` | Probe jobs + history |
| `panel` | Management API + embedded UI |
| `state` | Persisted accounts / policies / logs |
| `cpaapi` | CPA auth file / management client |
| `cpamp` | Optional usage.sqlite read-only stats |

---

## Defaults

- Master switches can enable **safe-guard** (auto cooldown + candidate, no auto trash)
- Remove path = **trash** (not hard delete first)
- Trash retention **7 days**, auto-purge on
- Super/Heavy never auto-trash
- **402 spending-limit never trash**

---

## Build

Requires Go + CGO, and CLIProxyAPI plugin ABI (vendored stubs under `third_party/CLIProxyAPI-src` for types).

```bash
# tests
go test ./...

# plugin .so (Linux amd64 example)
DEPLOY=0 bash scripts/build-plugin.sh
# → bin/cpa-xai-sentry.so
```

Deploy path depends on your CLIProxyAPI install, typically:

```text
CLIProxyAPI/plugins/linux/amd64/cpa-xai-sentry.so
```

Then restart the proxy / reload plugins.

---

## Panel

After host loads the plugin, open management UI (example):

```text
http://<host>:<mgmt-port>/v0/resource/plugins/cpa-xai-sentry/index.html
```

API base:

```text
/v0/management/cpa-xai-sentry
```

Header: `X-Management-Key: <your key>`

Main tabs:

1. **账号实况** — fleet table, bulk enable / permanent disable / trash / cooldown  
2. **巡检** — start probe jobs, expandable job logs  
3. **错误策略** — ladder editor, account hits, split unmatched shapes  
4. **垃圾箱** — restore / purge  

Action log stays on the **right**; patrol logs stay **inside 巡检**.

---

## Local smoke (no CPA host)

```bash
go run . -addr 127.0.0.1:18999 -data ./data
```

(`plugin_stub` / non-cshared build path for panel-only smoke when available.)

---

## Cutover from quota-guard

See [`docs/CUTOVER.md`](docs/CUTOVER.md):

1. Disable `cpa-xai-quota-guard`
2. Install only `cpa-xai-sentry`
3. Verify panel + one cooldown recover cycle

---

## Configuration (high level)

Host YAML / plugin config fields include:

| Field | Meaning |
|---|---|
| `sentry_enabled` | Master switch |
| `auto_cooldown` | Allow policy cooldown |
| `auto_candidate` | Allow 401 candidate path |
| `auto_delete` | Allow trash path |
| `patrol_enabled` / `patrol_interval` | Scheduled patrol |
| `permission_cooldown_seconds` | Default 403 cool window |
| `auth401_cooldown_seconds` | Default 401 window |
| `trash_retention_days` | Trash TTL |

Per-error policy (panel) overrides threshold / ladder / count mode.

---

## Development

```bash
export PATH=/path/to/go/bin:$PATH
go test ./internal/...
go test ./internal/policy -run Escalation -v
```

Notable design choices:

- **Closed-loop recovery**: cool-down due → `ResetToActive` (clear signal/streaks/lock)
- **No cooldown↔sync loop**: maintenance does not re-cool already cooling accounts
- **Permanent disable** ≠ cooldown: `user_manual` never auto re-enables

---

## License / notice

Internal ops plugin for CLIProxyAPI + xAI free fleet.  
Do not commit live auth tokens, management keys, or production `state.json`.

`.gitignore` already excludes `bin/`, `*.so`, `data/`.


## Production readiness (v1.0.0)

- Unit tests + race tests for state/guard/policy/patrol
- Atomic state save (`*.tmp` + rename, mode 0600)
- Account `Get` returns a snapshot copy (no concurrent map/field races on returned pointers)
- Single version source: `internal/version.Version`
- Permanent disable requires master `sentry_enabled`
- Default remove path is trash (7d); 402 / Super-Heavy protected from auto-trash
- Do not run together with `cpa-xai-quota-guard`
