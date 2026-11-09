# mtmd

mtmd is the daemon of [tuios](https://github.com/Gaurav-Gosain/tuios),
extracted so that a host program ([mtm](https://github.com/ofamtools)) can link
it into its own binary and need no `tuios` on PATH. It runs the same persistent
terminal sessions, panes and wire protocol as the tuios daemon. It has no
terminal interface and no command line of its own: the host calls
`serve.Main(args, serve.Options{...})` behind a subcommand of its own.

This is a modified fork. It is local to the o-family and is not published. See
[FORK.md](FORK.md) for what was changed, how upstream is tracked, and how to
merge a new upstream release.

## Scrollback on disk

A pane keeps its newest rows of history in memory (`appearance.scrollback_lines`)
and writes the rows that leave memory to a compressed, append-only log on disk
under the state directory (`<session>/<window-id>.cold`). Capture and search
read through the log, so a pane's history is as deep as its log, not its ring.
A pane that nobody is using (not displayed, not focused, silent for a while)
shrinks its in-memory ring and lets the log hold the rest. A log outlives a
daemon restart and a live upgrade, and the log of a closed pane is kept for a
grace period. The pure Go emulator only: panes on the libghostty backend have
no log and keep today's behaviour.

The log is read on demand only. A restart adopts a pane's log by its index (no
block is read; the ring gets the saved tail as before), attaching to or
promoting a pane reads none of it, and older rows are read in blocks of 256
rows when a capture (`start`/`end`/`lines`) or a search asks for them, a window
costing the blocks it lies in. `unwrap` with a window and a capture of the
whole history read the log from its start, because they number lines by it.

Settings (`[daemon]` in config.toml; each is also in the option table):

| Setting | Default | Meaning |
|---|---|---|
| `scrollback_disk` | `true` | Write evicted rows to a log. `false` turns it off and deletes the logs when the daemon next starts. |
| `scrollback_disk_rows_per_pane` | 2000000 | Most rows one log keeps, oldest segment first. 0 = no limit. |
| `scrollback_disk_mb_per_pane` | 128 | Most MiB one log takes. |
| `scrollback_disk_mb_total` | 2048 | Most MiB all logs take together; closed logs go first. |
| `scrollback_grace_days` | 3 | Days a closed pane's log is kept. 0 deletes at close. |
| `scrollback_hot_when` | `["displayed","focused","output"]` | Reasons a pane counts as in use. `[]` = always cold. |
| `scrollback_hot_idle_seconds` | 300 | How long a silent pane still counts as in use by `output`. |
| `scrollback_cold_ring_rows` | 10000 | Rows a pane that is not in use keeps in memory (floor 5000). |

Memory after a flood: a pane that printed lately is in use, so it keeps its whole
ring (`appearance.scrollback_lines`; a 200 MB flood with 1,000,000 lines holds
about 260 MB). The daemon hands the garbage back after about 30 s (260 to 144 MB)
and the ring itself when the pane goes cold, `scrollback_hot_idle_seconds` after
its last output (144 to 36 MB, measured with macOS `footprint`). macOS `ps` RSS
can keep reading the peak long after that, because the runtime's freed pages stay
resident until the kernel needs them; `footprint` is the true figure. Lower
`scrollback_hot_idle_seconds` to shed the ring sooner.

To keep today's memory behaviour with the log as a backup, set
`scrollback_hot_when = ["displayed","focused","output"]` and a large
`scrollback_cold_ring_rows`; to have no logs at all, `scrollback_disk = false`.

Verbs (wire, over the daemon socket; `serve` hosts relay them):

- `list-history-logs {state: all|live|orphaned}`, `show-history-log {log}`: what
  logs exist (id is `session/window`), rows, bytes, gaps, for an orphan the days left.
- `keep-history-log {log}` / `unkeep-history-log {log}`: exempt an orphan from the
  grace purge and the total cap, or not.
- `adopt-history-log {log, session, window}` (alias `adopt-history`): a pane takes an
  orphan's log over; its new output follows the old rows.
- `purge-history-logs {log | all_orphaned, dry_run, confirm}`: delete orphans. Run it
  with `dry_run` first; the reply's `confirm` token is what the real run needs.
  Adopt and a real purge need a person's connection.
- `capture-pane` (`source=recent`) and `search-panes` read the disk rows too;
  `history_rows_disk` and `complete` say what was reached.

## Credit

**All of the code in this repository is the work of
[Gaurav Gosain](https://github.com/Gaurav-Gosain) and the tuios contributors.**
It is a fork of **tuios** (<https://github.com/Gaurav-Gosain/tuios>), based on
upstream tag **v0.12.1** (`850335c0c`). mtmd keeps tuios's git history, so the
authorship of every line is in `git log` and `git blame`.

Modifications made here: the Go module path is `ofam.dev/mtmd`; a small
`serve` package is the entry point; self re-execution goes through
`internal/selfcmd`; and everything outside the daemon and the packages it needs
is deleted (the terminal interface, the command line, the docs, the other
modules). `internal/vt` is a copy of the terminal emulator `tuios.dev/vt`
(also Gaurav Gosain's, with its LICENSE and NOTICE) at the version in
`scripts/vt-version`, plus the patch `scripts/vt-ofam.patch`, made by
`scripts/ofam-vt.go`. The list is in [FORK.md](FORK.md). Upstream endorses none of it, and
a problem found here is not to be reported upstream before it is reproduced
with an unmodified tuios.

## Licence

tuios is MIT licensed: "Copyright (c) 2025 Gaurav Gosain". The licence text is
in [LICENSE](LICENSE), unchanged from upstream, and it applies to this
repository as a whole. Keep it, and the copyright notice in it, with every copy
and every substantial portion of this code, in source or binary form.

Third-party code that tuios includes, and the licence files that travel with
it, are listed in [THIRD_PARTY.md](THIRD_PARTY.md): Charmbracelet's MIT code,
the Go authors' BSD-3 code, Alec Thomas's chroma lexers (MIT), herdr's
agent-detection manifests (Apache-2.0), and the licences and notices of the
`tuios.dev/*` modules that the daemon links, kept under
[third_party/](third_party/). The licences of Go modules that mtmd only imports
are in those modules and are collected by the host when it builds a binary.
