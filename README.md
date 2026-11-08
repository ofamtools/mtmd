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
