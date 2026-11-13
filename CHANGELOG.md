# Changelog

## v0.4.3 (2026-11-13)

- Added: `hostnames.MTMEnv` (and `serve`-level use through `Options.Names.Env`), the complete `MTM_` table: every `TUIOS_` variable the daemon sets for panes, hook commands and plugin processes or reads (pane token and grants, host, tty, restored, plugin id and token, the hook context variables, the daemon's tunables). With `DropUpstream` no `TUIOS_` variable reaches a child; the `TUIOS_INTEGRATION_*` strings are file markers, not variables.
- Changed: a client (`VerbClient`, nested-attach check, socket pick) reads the pane variables host name first; `env` passed to an agent launch refuses a host name the daemon sets, as it refuses the `TUIOS_` one; a plugin pane's protected variables and the plugin env cleaner treat the host's names like upstream's; `TUIOS_PANE_GRANTS` of an outer pane is cleaned from a plugin's environment.
- Added: `hostnames.IsHost`, and `serve.MTMEnv` (the same table, for a host outside the module).
- Fixed: `hostnames.Mirror` is idempotent: with `DropUpstream`, a second pass keeps the host names instead of dropping them.

## v0.4.2 (2026-11-13)

- Added: `Names.DropUpstream` (`serve.Options.Names`), default false. When true, panes, hook commands and plugin processes carry only the host's names (`MTM_*`) for the paired variables, not the `TUIOS_*` ones. The daemon reads a pane process's variables host name first, then upstream, so nothing it depends on changes.

## v0.4.1

- Fixed: unplaced and unfocused panes inherit the focused pane's content size or the session's last applied pane size instead of a stale 80x24 session box. Explicit content dimensions are no longer capped by that box.
- Added: `appearance.new_window_inherit_size`, on by default. Turn it off to restore session-box startup sizing; later view size announcements still apply.

## v0.4.0

- Fixed: `get-window` answered by an attached client reports the pane's real PTY size, like the daemon's own answer.
- Added: `transcript_path` in `get-window` and `get-agent-state` replies when a transcript file is joined.
- Fixed: `focus-window --relative` accepts `previous` as well as `prev`.
- Added: `MTM_AGENT` is read before `TUIOS_AGENT` (hooks, status lines, the pane detector), and the bundled plugin assets start for `MTM_ENV`/`MTM_AGENT` as well as the `TUIOS_` names.

## v0.3.0

Host names for the daemon's folders, socket and pane variables (FORK.md patch 13).

- Added: `serve.Options.Names` (`serve.Names`, `serve.EnvName`, `serve.UpstreamNames`): a host names the
  socket folder and file, the state folders and the config folder. Nil keeps upstream's (tuios) names.
- Added: with a host table, panes, hooks and plugins get each host variable (for example `MTM_SOCKET`)
  beside its `TUIOS_` one, and a host variable set in the environment is read first.
- Added: `[--server NAME] migrate-paths [--dry-run] [--rollback]`, also run first by `daemon` and
  `start-server`: moves a named server's state folder to the host's names (rename, `MOVED-TO`/`MOVED-FROM`
  markers), copies `config.toml` when the host has none, and refuses while a daemon answers on the
  socket it would move from. A re-run does nothing; the default server is not moved.

## v0.2.0

Disk-backed scrollback for the pure Go emulator.

- Added: rows that leave a pane's in-memory history are written to a compressed
  per-pane log on disk (`internal/coldlog`), fed by an evict hook in the carried
  emulator (`internal/vt`, patch in `scripts/vt-ofam.patch`). Settings
  `daemon.scrollback_disk`, `_disk_rows_per_pane`, `_disk_mb_per_pane`,
  `_disk_mb_total`, `_grace_days`, `_hot_when`, `_hot_idle_seconds`,
  `_cold_ring_rows` (see README).
- Added: a pane that is not in use shrinks its in-memory ring to
  `scrollback_cold_ring_rows`; the older rows stay in its log. In a daemon with
  ten panes of 100k coloured rows, nine demoted, the process footprint falls
  from 143 MB to 77 MB (macOS) and RSS from 154 MB to 77 MB (Linux).
- Added: `capture-pane` (`source=recent`) and `search-panes` read through the
  log (`history_rows_disk`, `complete` in the replies).
- Added: nothing reads the log eagerly: a restart adopts it by its index, attach and promotion read none of it, and a capture or search reads only the blocks its range needs (`coldlog.BlockReads` counts them; tests assert zero for restore, attach and promotion).
- Added: the log survives a daemon restart and a live upgrade; the log of a
  closed pane is deleted after `scrollback_grace_days`.
- Added verbs: `list-history-logs`, `show-history-log`, `keep-history-log`,
  `unkeep-history-log`, `adopt-history-log` (alias `adopt-history`),
  `purge-history-logs`.
- Changed: a pane's ring shrinks in one step (no transient peak), the kept rows
  are compacted so the freed pages come back whole, and an idle log frees its
  queue and compression state.
- Known limits: libghostty panes have no log; `GetTerminalState` with a depth
  past the ring is not extended; `search-panes` of a deep log is bounded per
  call (`complete=false`).
