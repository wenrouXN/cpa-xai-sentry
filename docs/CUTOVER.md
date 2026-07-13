# Cutover: cpa-xai-quota-guard → cpa-xai-sentry (plugin v1.0.0+)

## Goal

Only run **cpa-xai-sentry**. Keep `cpa-xai-quota-guard` disabled permanently.

For first-time install (build, Docker mounts, full config), use **[INSTALL.md](./INSTALL.md)**.

## Steps

1. Build plugin (on host with Go + CGO):

```bash
cd cpa-xai-sentry
# recommended (tests + -tags cshared)
DEPLOY=0 bash scripts/build-plugin.sh
# or:
CGO_ENABLED=1 go build -tags cshared -buildmode=c-shared -o bin/cpa-xai-sentry.so .
```

2. Install `.so` into CPA plugins dir (host path that is bind-mounted into the container):

```text
CLIProxyAPIplus/plugins/linux/amd64/cpa-xai-sentry.so
```

Docker must mount plugins + auths, e.g.:

```yaml
volumes:
  - ./config.yaml:/CLIProxyAPI/config.yaml
  - ./auths:/root/.cli-proxy-api
  - ./plugins:/CLIProxyAPI/plugins
```

3. In `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-xai-quota-guard:
      enabled: false
    cpa-xai-sentry:
      enabled: true
      sentry_enabled: true
      auto_cooldown: false
      auto_candidate: false
      auto_delete: false
      trash_retention_days: 7
      trash_auto_purge: true
      management_url: "http://127.0.0.1:8317"
      management_key: "<CPA_MANAGEMENT_KEY>"
      auth_dir: "/root/.cli-proxy-api"
      # durable on mounted volume:
      state_path: "/root/.cli-proxy-api/cpa-xai-sentry/state.json"
      trash_dir: "/root/.cli-proxy-api/cpa-xai-sentry/trash"
```

4. Restart `cli-proxy-api` (`docker compose restart cli-proxy-api`).

5. Open panel:

```text
http://<host>:8317/v0/resource/plugins/cpa-xai-sentry/index.html
```

Confirm version `1.0.0`, mode observe-friendly, trash empty.

6. Soak, then enable **safe-guard**:

```yaml
auto_cooldown: true
auto_candidate: true
auto_delete: false
# patrol_enabled: true   # optional; burns free quota
```

7. Remove quota-guard binary/store-source when stable.

## Safety

- Default remove = trash (7d auto-purge), not hard wipe only
- 402 never auto-trash
- Super/Heavy never auto-trash
- Never run two enforcers at once
- Keep state/trash on the **mounted auth volume**
