# Changelog

## 1.0.1

### Fixed
- **Stop auto-reopening operator disables**: maintenance no longer re-enables CPA auth files that are disabled outside sentry cool-down/manual locks.
- Disabled files are synced to panel state as `CPA已禁用` (`cpa_file_disabled`) so they stay locked until you click **启用**.
- New config `reopen_foreign_disabled` (default `false`) restores the old reopen behaviour only if explicitly enabled.

# Changelog

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
