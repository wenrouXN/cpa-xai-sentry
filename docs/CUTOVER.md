# Cutover: cpa-xai-quota-guard → cpa-xai-sentry (plugin v1.0.0+)


## Goal

Only run **cpa-xai-sentry**. Keep `cpa-xai-quota-guard` disabled permanently.

## Steps

1. Build plugin (on host with Go + CGO + CPA SDK replace if needed):

```bash
cd projects/cpa-xai-sentry
# recommended
DEPLOY=1 bash scripts/build-plugin.sh
# or manually (must include -tags cshared):
CGO_ENABLED=1 go build -tags cshared -buildmode=c-shared -o bin/cpa-xai-sentry.so .
```

2. Copy `.so` into CPA plugins dir, e.g.:

```text
CLIProxyAPIplus/plugins/linux/amd64/cpa-xai-sentry.so
```

3. In `config.yaml`:

```yaml
plugins:
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
      state_path: "data/cpa-xai-sentry-state.json"
      trash_dir: "data/cpa-xai-sentry-trash"
```

4. Restart `cli-proxy-api`.

5. Open management panel for Sentry — confirm mode=`observe`, trash empty.

6. Soak, then enable **safe-guard**:

```yaml
auto_cooldown: true
auto_candidate: true
auto_delete: false
patrol_enabled: true
```

7. Remove quota-guard store-source when stable.

## Safety

- Default remove = trash (7d auto-purge), not hard wipe only
- 402 never auto-trash
- Super/Heavy never auto-trash
- Never run two enforcers at once
