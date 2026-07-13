# Install & mount guide (CLIProxyAPI)

This document is the **production install path** for `cpa-xai-sentry` v1.0.0+.

> Goal: build a Linux amd64 `.so`, place it where CPA loads plugins, mount auth/state/plugins correctly, enable only this sentry (not quota-guard), restart, open panel.

---

## 0. What you need

| Item | Notes |
|---|---|
| Host OS | Linux **amd64** (plugin is built `GOOS=linux GOARCH=amd64`) |
| Go + CGO | `CGO_ENABLED=1`, gcc/linker available |
| CLIProxyAPI | Docker image or binary that supports **c-shared plugins** |
| Management API | CPA `remote-management.secret-key` set (panel/API need a key) |
| xAI auth files | CPA auth dir with `xai-*.json` (or your naming) |

Optional:

| Item | Notes |
|---|---|
| CPAMP / usage.sqlite | For per-account day usage bars (read-only mount) |
| HTTP proxy | If patrol/live traffic needs egress proxy |

---

## 1. Build the plugin

```bash
git clone https://github.com/wenrouXN/cpa-xai-sentry.git
cd cpa-xai-sentry

# tests + build (does NOT copy into CPA unless DEPLOY=1)
DEPLOY=0 bash scripts/build-plugin.sh
# output: ./bin/cpa-xai-sentry.so
```

Manual build (must include `-tags cshared`):

```bash
export CGO_ENABLED=1 GOOS=linux GOARCH=amd64
go test ./internal/... -count=1
go build -tags cshared -buildmode=c-shared -o bin/cpa-xai-sentry.so .
# verify export
nm -D bin/cpa-xai-sentry.so | grep cliproxy_plugin_init
```

> Without `-tags cshared`, the stub build is selected and **plugin exports disappear**.

---

## 2. Install the `.so` into CPA plugins directory

CPA loads plugins from `plugins/<os>/<arch>/`.

### Typical layout (host)

```text
CLIProxyAPIplus/                    # CPA data dir next to docker-compose
├── config.yaml
├── docker-compose.yml
├── auths/                          # mounted as auth-dir inside container
├── logs/
└── plugins/
    └── linux/
        └── amd64/
            └── cpa-xai-sentry.so   # ← put binary here
```

```bash
mkdir -p /path/to/CLIProxyAPIplus/plugins/linux/amd64
cp -f bin/cpa-xai-sentry.so /path/to/CLIProxyAPIplus/plugins/linux/amd64/
```

Or from this repo (if your deploy dir matches the script default):

```bash
DEPLOY=1 DEPLOY_DIR=/path/to/CLIProxyAPIplus/plugins/linux/amd64 \
  bash scripts/build-plugin.sh
```

### Docker: required volume mounts

Example `docker-compose.yml` (minimal for sentry):

```yaml
services:
  cli-proxy-api:
    image: eceasy/cli-proxy-api:latest   # or your CPA image
    container_name: cli-proxy-api
    ports:
      - "8317:8317"                      # management + API port (match config)
    volumes:
      - ./config.yaml:/CLIProxyAPI/config.yaml
      - ./auths:/root/.cli-proxy-api       # auth files + durable sentry state
      - ./logs:/CLIProxyAPI/logs
      - ./plugins:/CLIProxyAPI/plugins     # plugin .so tree
      # optional: CPAMP usage DB for day-token display
      # - ./cpa-manager-data/usage.sqlite:/data/usage.sqlite:ro
    restart: always
```

| Host path | Container path | Why |
|---|---|---|
| `./config.yaml` | `/CLIProxyAPI/config.yaml` | enable plugin + keys |
| `./auths` | `/root/.cli-proxy-api` | xAI auth JSON; **state/trash live under here** |
| `./plugins` | `/CLIProxyAPI/plugins` | loads `linux/amd64/cpa-xai-sentry.so` |
| `./logs` | `/CLIProxyAPI/logs` | CPA logs (optional but recommended) |
| usage.sqlite (ro) | e.g. `/data/usage.sqlite` | optional CPAMP usage |

**Persistence rule:** put `state_path` / `trash_dir` **under the mounted auth dir**, not inside the container writable layer, or restarts wipe panel state.

---

## 3. Configure CPA (`config.yaml`)

### 3.1 Host management (required for panel)

```yaml
port: 8317

remote-management:
  allow-remote: true          # false = localhost only
  secret-key: "<MANAGEMENT_KEY_PLAIN_OR_BCRYPT>"
  # plaintext is hashed on startup by CPA

auth-dir: "/root/.cli-proxy-api"
```

All `/v0/management/*` calls need the key:

```bash
curl -H 'X-Management-Key: <MANAGEMENT_KEY>' \
  http://127.0.0.1:8317/v0/management/cpa-xai-sentry/health
# or Authorization: Bearer <MANAGEMENT_KEY>
```

### 3.2 Plugin block

```yaml
plugins:
  enabled: true
  dir: "plugins"              # relative to CPA workdir; with Docker mount → /CLIProxyAPI/plugins
  configs:
    # IMPORTANT: never run two enforcers
    cpa-xai-quota-guard:
      enabled: false

    cpa-xai-sentry:
      enabled: true

      # master + action gates (start conservative)
      sentry_enabled: true
      auto_cooldown: false
      auto_candidate: false
      auto_delete: false

      trash_retention_days: 7
      trash_auto_purge: true
      tick_seconds: 30

      # CPA management loopback (inside container)
      management_url: "http://127.0.0.1:8317"
      management_key: "<SAME_AS_REMOTE_MANAGEMENT_KEY>"

      # durable paths (on mounted auth volume)
      auth_dir: "/root/.cli-proxy-api"
      state_path: "/root/.cli-proxy-api/cpa-xai-sentry/state.json"
      trash_dir: "/root/.cli-proxy-api/cpa-xai-sentry/trash"

      # patrol (real probes consume free quota)
      patrol_enabled: false
      patrol_interval: 3600
      patrol_timeout: 15
      patrol_concurrency: 8
      patrol_batch_size: 50
      patrol_model: "grok-4.5"

      # optional CPAMP analytics
      # cpamp_url: "http://192.168.1.x:18317"
      # cpamp_admin_key: "<CPAMP_KEY>"
      # cpamp_usage_floor: true
```

#### Field reference (plugin)

| Field | Required | Meaning |
|---|---|---|
| `enabled` | yes | load this plugin |
| `sentry_enabled` | yes | master switch for automated actions |
| `auto_cooldown` | no | allow policy cooldown |
| `auto_candidate` | no | allow 401 candidate path |
| `auto_delete` | no | allow trash path |
| `management_url` | yes | CPA management base (loopback in Docker) |
| `management_key` | yes | must match CPA management key |
| `auth_dir` | yes | auth JSON directory |
| `state_path` | yes | durable sentry state |
| `trash_dir` | yes | trash snapshots |
| `tick_seconds` | no | maintain interval (default 30) |
| `patrol_*` | no | active probe settings |
| `cpamp_*` | no | usage analytics |

---

## 4. Restart & verify

```bash
cd /path/to/CLIProxyAPIplus
docker compose restart cli-proxy-api
# or: docker restart cli-proxy-api
```

Checks:

```bash
# 1) plugin file present on host
ls -la plugins/linux/amd64/cpa-xai-sentry.so

# 2) health / version
curl -sS -H 'X-Management-Key: <KEY>' \
  http://127.0.0.1:8317/v0/management/cpa-xai-sentry/state | jq .version
# expect: "1.0.0"

# 3) panel
# browser:
# http://<host>:8317/v0/resource/plugins/cpa-xai-sentry/index.html
```

Inside panel:

1. Version badge shows **v1.0.0**
2. 账号实况 lists xAI accounts
3. After soak, enable **安全防护** (auto cooldown + candidate; still no auto trash)

---

## 5. Safe rollout sequence

1. Install with `auto_*=false` (observe + manual actions only)
2. Confirm errors catalog fills (`429` / `403` / …)
3. Enable `auto_cooldown` + `auto_candidate`
4. Leave `auto_delete=false` unless you accept trash automation
5. Turn on `patrol_enabled` only if you accept free-quota burn

Cutover from old guard: see [CUTOVER.md](./CUTOVER.md).

---

## 6. Common failures

| Symptom | Likely cause | Fix |
|---|---|---|
| Plugin missing / no panel route | `.so` wrong path or arch | Use `plugins/linux/amd64/`; rebuild amd64 |
| Panel 404 | management disabled / wrong port | set `remote-management.secret-key`, port 8317 |
| 401 on API | wrong key header | `X-Management-Key` or `Authorization: Bearer` |
| State resets every restart | state on non-mounted path | put under mounted `auth_dir` |
| No exports in `.so` | built without `-tags cshared` | rebuild with tags |
| Double disable / fight | quota-guard still on | `cpa-xai-quota-guard.enabled: false` |
| Patrol 404 HTML | base_url double `/v1` | fix auth `base_url` |

---

## 7. Uninstall

```yaml
plugins:
  configs:
    cpa-xai-sentry:
      enabled: false
```

```bash
rm -f plugins/linux/amd64/cpa-xai-sentry.so
docker compose restart cli-proxy-api
# optional: keep or delete state/trash under auths/cpa-xai-sentry/
```

---

## 8. Security notes

- Do not commit real `management_key`, auth tokens, or `state.json`
- Prefer bcrypt-hashed management secret in CPA config after first run
- Restrict `allow-remote` if management port is exposed
- Panel permanent-disable / trash are destructive — use roles/network ACL on 8317
