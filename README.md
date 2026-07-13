# cpa-xai-sentry

CLIProxyAPI native plugin: xAI/Grok **observe + optional guard + trash bin** (7-day auto-purge).

## Status

Core packages implemented and unit-tested. CPA CGO `.so` glue still needs local CLIProxyAPI SDK (`-tags cshared`).

## Spec / Plan

- Spec: workspace `docs/superpowers/specs/2026-07-13-cpa-xai-sentry-design.md`
- Plan: workspace `docs/superpowers/plans/2026-07-13-cpa-xai-sentry.md`

## Defaults

- Observe-only (`auto_cooldown/candidate/delete` false)
- Remove path = **trash** (snapshot → CPA delete)
- Trash retention **7 days**, auto-purge on
- Super/Heavy never auto-trash; **402 never trash**

## Tests

```bash
export PATH=/vol1/1000/config/share/openclaw/tools/go/bin:$PATH
go test ./...
```

## Local panel smoke (no CPA)

```bash
go run . -addr 127.0.0.1:18999 -data ./data
```

## Build CPA plugin

```bash
# after adding CLIProxyAPI replace in go.mod
CGO_ENABLED=1 go build -tags cshared -buildmode=c-shared -o bin/cpa-xai-sentry.so .
```

## Cutover

See `docs/CUTOVER.md` — disable `cpa-xai-quota-guard`, install this only.
