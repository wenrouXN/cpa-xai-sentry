# cpa-xai-sentry

CLIProxyAPI **native CGO plugin** for xAI / Grok account fleet:

- **Observe** request failures (429 / 402 / 403 / 401 / 426-split / network / unmatched)
- **Guard** with optional auto cooldown / candidate / permanent disable / trash
- **Patrol** real HTTP probes (full / cooldown / permanent / candidate / trash) with per-job logs
- **Register** tab talks to local `grok-register-lite` (`:8788`) as a black box (manual / floor / schedule)
- **Web panel** for live accounts, error policies, trash, action logs

Current version: **v1.2.7** (single source: `internal/version.Version`)

> Successor to `cpa-xai-quota-guard`. Run **only this plugin** for xAI free-usage guarding — do not run both enforcers.

---

## Features

### Account live status (`账号实况`)

Status is **code + action**, not a blank “normal”:

| State | Meaning |
|---|---|
| `正常·可用` | Clean active, can take traffic |
| `正常·待观察` / `正常·有信号观察` | Still active; counting toward policy ladder |
| `429·额度冷却` | Free-usage exhausted → cooldown |
| `402·消费冷却` | Spending limit → cooldown |
| `403·权限冷却` | Permission denied → cooldown |
| `401·候删` | Auth invalid → candidate |
| `永久禁用` | Panel / policy permanent disable (auto re-enable only via **permanent patrol** alive probe) |
| `CPA已禁用` | Auth file disabled outside sentry (rare; may be reopened when configured) |
| `垃圾箱` | Soft-deleted snapshot, restorable |

State filter is **dynamic** (options with count > 0 only). Masking can hide email domains in the UI and logs.

### Error policy ladder

Per-error multi-tier rules (panel-editable). Display titles auto-prefix **HTTP code · 中文名**; storage keeps Chinese-only names. Split flow: user fills Chinese; server preserves `split_shape`.

Builtin focus:

| Key | Notes |
|---|---|
| `free_usage_429` | Free quota exhausted |
| `permission_403` | xAI permission-denied / chat-endpoint denied only — **not** generic gateway `Access Denied` |
| `auth_401` | Invalid credentials → candidate path |
| `reason:http_426` (example split) | SignalNone-class split still runs the **user ladder** (not observe-only early-return) |
| `unmatched` / `any_error` | Catch-all; can be split by shape |

Also supports:

- Count mode: **streak** (success clears) / **total** (success keeps)
- Escalations: observe → cooldown → candidate → permanent disable
- Error samples + recent request timeline per account

### Patrol

- Modes: full (sentry-enabled / non-disabled set), cooldown, permanent, candidate, trash
- **Permanent patrol** = scan permanent-disable set only; **alive (2xx)** reopens traffic; failures still go through policy
- Real xAI requests (consumes free quota)
- Results feed the **same** `HandleUsage` + policy path as live traffic
- Hot-reload safe: runner can refresh `Guard` via `GetGuard`; short post-job settle window avoids tick prune races
- Job history with expandable per-job logs

### Register (`注册`)

- Backend: local **8788** `grok-register-lite` (black box — health / session / CPA channel / window success)
- Manual / floor (min pool) / schedule registration
- Relogin only where product allows (error-policy ops column + local password inventory), not a full account-pool KPI dashboard

### Maintenance tick

- Recover cooldown when `recover_at` due → full clean active
- Optional reopen of **non-sentry** disabled auth files; leave our cool-downs alone
- CPAMP backfill / heal ordering and cooldown idempotency for long-run stability

### Trash

- Snapshot → remove from CPA live set
- Default retention **7 days**, auto-purge
- Super/Heavy protected from auto-trash; **402 never trash**

### Config sync

- Panel save dual-writes **runtime-overrides** + host `plugins.configs` (last-writer-wins)
- Host reconfigure / official plugin page: host wins, overrides rebased

---

## Architecture

```
CLIProxyAPI host
  └─ cpa-xai-sentry.so  (CGO plugin)
       ├─ usage hook → match → policy ladder → guard action
       ├─ patrol runner → same guard path
       ├─ register job → 8788 black box
       ├─ tick (recover / reopen foreign / trash purge / cpamp backfill)
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
| `regjob` / `registerlite` | Register jobs + 8788 client |
| `panel` | Management API + embedded UI |
| `state` | Persisted accounts / policies / logs |
| `persist` | Runtime overrides dual-write helpers |
| `cpaapi` | CPA auth file / management client |
| `cpamp` | Optional usage.sqlite read-only stats |

---

## Defaults

- Master switches can enable **safe-guard** (auto cooldown + candidate; trash optional)
- Remove path = **trash** (not hard delete first)
- Trash retention **7 days**, auto-purge on
- Super/Heavy never auto-trash
- **402 spending-limit never trash**
- `permission_403` match is **strict** (xAI permission text only)

---

## Install / mount / config (start here)

**Full production guide (build → Docker volumes → `config.yaml` → verify):**

- **[docs/INSTALL.md](docs/INSTALL.md)** — install, `.so` layout, mounts, config, rollout, troubleshooting  
- **[docs/CUTOVER.md](docs/CUTOVER.md)** — migrate from `cpa-xai-quota-guard`

### Quick path (Docker host)

```bash
# 1) build
git clone https://github.com/wenrouXN/cpa-xai-sentry.git
cd cpa-xai-sentry
DEPLOY=0 bash scripts/build-plugin.sh   # → bin/cpa-xai-sentry.so

# 2) install binary into CPA plugins tree (host side)
mkdir -p /path/to/CLIProxyAPIplus/plugins/linux/amd64
cp -f bin/cpa-xai-sentry.so /path/to/CLIProxyAPIplus/plugins/linux/amd64/
```

**Docker mounts (required):**

```yaml
volumes:
  - ./config.yaml:/CLIProxyAPI/config.yaml
  - ./auths:/root/.cli-proxy-api          # auth files + durable state/trash
  - ./plugins:/CLIProxyAPI/plugins        # must contain linux/amd64/cpa-xai-sentry.so
  - ./logs:/CLIProxyAPI/logs
  # CPAMP usage: mount **directory** (not only usage.sqlite) so WAL is visible
  - ./cpa-manager-data:/data:ro
```

**Minimal plugin config** (inside CPA `config.yaml`):

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-xai-quota-guard:
      enabled: false          # never run two enforcers
    cpa-xai-sentry:
      enabled: true
      sentry_enabled: true
      auto_cooldown: false    # observe first
      auto_candidate: false
      auto_delete: false
      management_url: "http://127.0.0.1:8317"
      management_key: "<CPA_MANAGEMENT_KEY>"
      auth_dir: "/root/.cli-proxy-api"
      state_path: "/root/.cli-proxy-api/cpa-xai-sentry/state.json"
      trash_dir: "/root/.cli-proxy-api/cpa-xai-sentry/trash"
```

```bash
# 3) restart CPA (or rely on host hot-reload if your build supports plugin reload)
cd /path/to/CLIProxyAPIplus && docker compose restart cli-proxy-api

# 4) open panel
# http://<host>:8317/v0/resource/plugins/cpa-xai-sentry/index.html
# Header for API: X-Management-Key: <CPA_MANAGEMENT_KEY>
```

> Put `state_path` / `trash_dir` **under the mounted auth volume**, or state is lost on container recreate.

---

## Build (details)

Requires Go + CGO, and CLIProxyAPI plugin ABI (vendored stubs under `third_party/CLIProxyAPI-src` for types).

```bash
export PATH=/path/to/go/bin:$PATH   # e.g. openclaw tools go
go test ./internal/...
DEPLOY=0 bash scripts/build-plugin.sh
# MUST use -tags cshared (script does this)
# → bin/cpa-xai-sentry.so → copy to plugins/linux/amd64/
# DEPLOY=1 RESTART=0  # deploy only; RESTART=1 also restarts container
```

---

## Panel

```text
http://<host>:8317/v0/resource/plugins/cpa-xai-sentry/index.html
```

API base: `/v0/management/cpa-xai-sentry`  
Header: `X-Management-Key: <your key>` (or `Authorization: Bearer …`)

Tabs: **账号实况** · **巡查** · **注册** · **错误策略** · **垃圾箱**  
Action log on the **right**; patrol / register job cards inside their tabs.

---

## Local smoke (no CPA host)

```bash
go run . -addr 127.0.0.1:18999 -data ./data
```

(`plugin_stub` / non-cshared build path for panel-only smoke when available.)

---

## Configuration (high level)

| Field | Meaning |
|---|---|
| `sentry_enabled` | Master switch |
| `auto_cooldown` | Allow policy cooldown |
| `auto_candidate` | Allow 401 candidate path |
| `auto_delete` | Allow trash path |
| `management_url` / `management_key` | CPA management loopback |
| `auth_dir` / `state_path` / `trash_dir` | Auth + durable paths (mount these) |
| `patrol_enabled` / `patrol_interval` / `patrol_mode` | Scheduled patrol (`permanent` = permanent set only) |
| `trash_retention_days` | Trash TTL |
| `reopen_foreign_disabled` | default **false**; do not auto-open operator/CPA disables |
| register / floor / schedule fields | See panel + `runtime-overrides` |

Full field list + Docker examples: **[docs/INSTALL.md](docs/INSTALL.md)**.  
Per-error ladder is edited in the panel (overrides YAML defaults).

---

## Development

```bash
export PATH=/path/to/go/bin:$PATH
go test ./internal/...
go test ./internal/policy -run Escalation -v
go test ./internal/match -run GenericAccessDenied -v
```

Notable design choices:

- **Closed-loop recovery**: cool-down due → `ResetToActive` (clear signal/streaks/lock)
- **No cooldown↔sync loop**: maintenance does not re-cool already cooling accounts
- **Permanent disable** ≠ cooldown: `user_manual` re-enables only via permanent patrol alive path (when enabled)
- **Strict permission match**: generic HTTP `Access Denied` does not become `permission_403`

---

## License / notice

Internal ops plugin for CLIProxyAPI + xAI free fleet.  
Do not commit live auth tokens, management keys, or production `state.json`.

`.gitignore` already excludes `bin/`, `*.so`, `data/`.

## Auth closed loop

See **[docs/AUTH_CLOSED_LOOP.md](docs/AUTH_CLOSED_LOOP.md)** for the full state machine and invariants.

## Production readiness

- Unit tests for match / guard / policy / patrol / state / panel paths
- Atomic state save (`*.tmp` + rename, mode 0600)
- Account `Get` returns a snapshot copy (no concurrent map/field races on returned pointers)
- Single version source: `internal/version.Version` (currently **1.2.7**)
- Permanent disable requires master `sentry_enabled`
- Default remove path is trash (7d); 402 / Super-Heavy protected from auto-trash
- Do not run together with `cpa-xai-quota-guard`
