# Changelog

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
