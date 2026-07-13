> **Policy:** auto-open **unowned** disables is **ON** (`reopen_foreign_disabled: true`). Owned cool-downs are never opened.

# Auth closed-loop state machine

Single source of truth for how sentry owns CPA auth files.

## States

| State | CPA file | disable_source | Auto reopen? |
|---|---|---|---|
| `active` | enabled | `""` | n/a |
| `cooldown_quota` / `_spending` / `_permission` | disabled | `plugin_auto` | yes when `recover_at` due |
| `candidate_dead` | disabled | `plugin_auto` | yes when `recover_at` due (if set) |
| `user_manual` + `user_manual` | disabled | permanent | **never** (panel 启用 only) |
| `user_manual` + `cpa_file_disabled` | disabled | external/operator | **never** by default |
| `trashed` | deleted (snapshot kept) | `plugin_auto` | restore API only |

## Transitions

```
usage/patrol error
  → match signal → policy ladder
  → cooldown:  SetAccountState(cooldown_*, plugin_auto) + recover_at + CPA disabled=true
  → candidate: SetAccountState(candidate_dead, plugin_auto) + CPA disabled=true
  → disable:   SetAccountState(user_manual, user_manual) + CPA disabled=true
  → trash:     snapshot + delete live file + state=trashed

tick
  → syncDisabledFromCPA:
       protect(plugin_auto|user_manual|cooldown/候删|cpa_file_disabled) → leave file
       else if reopen_foreign_disabled: reopen (optional, default OFF)
       else: mark user_manual+cpa_file_disabled (keep disabled)
  → pruneDuplicateAccounts: keep ownership-heavy row (cooldown > empty active)
  → scrub/repair: Active+plugin_auto+future recover_at → repair cooldown state
  → recover_at due && CanAutoReenable(plugin_auto) → CPA enable + ResetToActive

panel
  → 永久禁用: user_manual + CPA disabled
  → 启用: CPA enable + ResetToActive
  → 冷却: same as policy cooldown
```

## Invariants (must hold)

1. **Ownership before file I/O** for sentry-initiated disables (`plugin_auto` / `user_manual`).
2. **Never auto-open** `user_manual` / `cpa_file_disabled` / `PreDisabled`.
3. **plugin_auto** is enough for `CanAutoReenable` (Owner may be empty on legacy rows).
4. **Duplicate rows**: cool-down ownership row wins over empty Active shells.
5. **Success** clears streaks (streak mode) but does not open a disabled cool-down file.
