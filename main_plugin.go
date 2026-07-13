//go:build cshared

package main

/*
CPA plugin entry (CGO). Build:
  CGO_ENABLED=1 go build -tags cshared -buildmode=c-shared -o bin/cpa-xai-sentry.so .

Requires replace:
  github.com/router-for-me/CLIProxyAPI/v7 => <local CPA source>

Wire host methods: plugin init, usage.handle, management routes -> panel.API,
periodic tick -> guard.Tick, optional patrol.
*/

func main() {}
