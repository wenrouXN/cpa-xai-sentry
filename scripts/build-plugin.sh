#!/usr/bin/env bash
# Build cpa-xai-sentry as a CPA c-shared plugin (.so) and optionally deploy.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

GO_BIN="${GO_BIN:-}"
if [[ -z "$GO_BIN" ]]; then
  if [[ -x /vol1/1000/config/share/openclaw/tools/go/bin/go ]]; then
    GO_BIN=/vol1/1000/config/share/openclaw/tools/go/bin/go
  elif [[ -x /vol1/1000/config/share/openclaw/state/workspace/tools/go/bin/go ]]; then
    GO_BIN=/vol1/1000/config/share/openclaw/state/workspace/tools/go/bin/go
  elif command -v go >/dev/null 2>&1; then
    GO_BIN="$(command -v go)"
  else
    echo "go not found; set GO_BIN" >&2
    exit 1
  fi
fi

export CGO_ENABLED=1
export GOOS=linux
export GOARCH=amd64

mkdir -p bin
OUT="${OUT:-$ROOT/bin/cpa-xai-sentry.so}"

echo "==> go=$GO_BIN"
"$GO_BIN" version

echo "==> unit tests (internal)"
"$GO_BIN" test ./internal/... -count=1

echo "==> build with -buildmode=c-shared -tags cshared"
# IMPORTANT: without -tags cshared, plugin_stub.go is selected and exports vanish.
"$GO_BIN" build -tags cshared -buildmode=c-shared -o "$OUT" .

echo "==> verify symbols"
if command -v nm >/dev/null 2>&1; then
  if ! nm -D "$OUT" 2>/dev/null | grep -q 'cliproxy_plugin_init'; then
    echo "ERROR: cliproxy_plugin_init not exported in $OUT" >&2
    nm -D "$OUT" 2>/dev/null | head -50 || true
    exit 2
  fi
  nm -D "$OUT" | grep cliproxy || true
fi

ls -la "$OUT" "$ROOT/bin/cpa-xai-sentry.h" 2>/dev/null || true

DEPLOY_DIR="${DEPLOY_DIR:-/vol1/1000/config/share/CLIProxyAPIplus/plugins/linux/amd64}"
if [[ "${DEPLOY:-0}" == "1" ]]; then
  if [[ ! -d "$DEPLOY_DIR" ]]; then
    echo "deploy dir missing: $DEPLOY_DIR" >&2
    exit 3
  fi
  cp -f "$OUT" "$DEPLOY_DIR/cpa-xai-sentry.so"
  echo "==> deployed to $DEPLOY_DIR/cpa-xai-sentry.so"
  if [[ "${RESTART:-0}" == "1" ]]; then
    if command -v docker >/dev/null 2>&1; then
      (cd /vol1/1000/config/share/CLIProxyAPIplus && docker compose restart cli-proxy-api 2>/dev/null) \
        || docker restart cli-proxy-api 2>/dev/null \
        || echo "restart manually: docker compose restart cli-proxy-api"
    fi
  fi
fi

echo "OK: $OUT"
