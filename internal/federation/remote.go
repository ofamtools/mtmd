package federation

import (
	"strings"
	"sync"

	"ofam.dev/mtmd/internal/lazyre"
)

// Finding the link's program on the far side.
//
// The far side of a link is a program that answers `stdio-proxy`: mtm, or
// tuios on a host not yet moved to mtm. RemotePrograms names them, in the
// order they are tried.
//
// ssh runs a command through a non-interactive shell, and that shell's PATH is
// the system default: /usr/local/bin, /usr/bin and their friends. It is not the
// PATH the person sees at their prompt, because the profile that adds
// ~/.local/bin is only read by a login shell. The install scripts put the
// program in ~/.local/bin, so the default install location was one the
// default link could not find, and every such host needed --command to work.
//
// The link now finds the binary itself. The whole mechanism is one shell
// script sent as the remote command, which tries three things in order and
// then runs what it found with the arguments the link wanted to run:
//
//  1. The program by name on the PATH. Free, and right on most machines.
//  2. A fixed list of places it is installed. Each is one stat, nothing is
//     searched, and the list is bounded so a person can read it when it fails.
//     Steps 1 and 2 run for one program before the next is tried.
//  3. The person's login shell, asked what each name means to it. This is the
//     step that reads the profile. It is last because it is the expensive one:
//     a profile can take seconds, can print, and differs between shells. The
//     fixed list resolves every install the project knows about without paying
//     that, so the login shell only runs on a machine where the program is
//     somewhere unusual, which is exactly where its answer is worth the cost.
//
// The script prints which path it chose on a line ahead of the link preamble,
// so the hub can report it and can run it directly on the next dial. A
// configured command skips all of this: it is sent as written, exactly as it
// always was.

// DefaultRemotePrograms are the file names of the programs the link may run
// on the far side, in the order it tries them: mtm first, then tuios for a
// host that has not been moved to mtm yet. Each answers `stdio-proxy` the same
// way. A host's configured command chooses the program itself and skips the
// list.
var DefaultRemotePrograms = []string{"mtm", "tuios"}

// remotePrograms is the list the probe uses; see SetRemotePrograms.
var (
	remoteProgramsMu sync.RWMutex
	remotePrograms   = DefaultRemotePrograms
)

// SetRemotePrograms sets the programs the link looks for on the far side, in
// order. Nil or empty restores DefaultRemotePrograms. A name that is not a
// plain file name (letters, digits, dot, dash, underscore) is left out, since
// it is spelled into the probe script, and returned so the caller can say so.
// A host calls it once, before the daemon starts (serve.Options.RemotePrograms).
func SetRemotePrograms(names []string) (dropped []string) {
	var keep []string
	for _, n := range names {
		if remoteProgramPattern().MatchString(n) {
			keep = append(keep, n)
		} else {
			dropped = append(dropped, n)
		}
	}
	if len(keep) == 0 {
		keep = DefaultRemotePrograms
	}
	remoteProgramsMu.Lock()
	remotePrograms = keep
	remoteProgramsMu.Unlock()
	return dropped
}

// RemotePrograms is the list the probe uses, in order.
func RemotePrograms() []string {
	remoteProgramsMu.RLock()
	defer remoteProgramsMu.RUnlock()
	return append([]string(nil), remotePrograms...)
}

// remoteProgramPattern is what a program name may be.
var remoteProgramPattern = lazyre.New(`^[A-Za-z0-9._-]+$`)

// remoteProgramsText names the programs for a person: "mtm or tuios".
func remoteProgramsText() string {
	return strings.Join(RemotePrograms(), " or ")
}

// remoteBinaryDirs are the fixed places the link looks, in order, after the
// PATH. Every entry is a place one of the project's own installers puts the
// binary, or one the updater already recognises as an install origin (see
// internal/release's provenance.go). Nothing here is a search: each is one
// executable-bit test. $HOME and $USER are expanded by the remote shell.
var remoteBinaryDirs = []string{
	// scripts/install.sh's default prefix, and the curl installer's second
	// choice. The one the report at the top of this file was about.
	"$HOME/.local/bin/",
	// The curl installer's third choice, for a home with no ~/.local/bin.
	"$HOME/bin/",
	// `go install` with the default GOPATH.
	"$HOME/go/bin/",
	// The curl installer's first choice, and Homebrew's prefix on Intel macOS.
	"/usr/local/bin/",
	// Homebrew on Apple silicon.
	"/opt/homebrew/bin/",
	// Homebrew on Linux.
	"/home/linuxbrew/.linuxbrew/bin/",
	// A per-user Nix profile.
	"$HOME/.nix-profile/bin/",
	// home-manager on NixOS.
	"/etc/profiles/per-user/$USER/bin/",
	// The default profile of a multi-user Nix install.
	"/nix/var/nix/profiles/default/bin/",
	// The NixOS system profile.
	"/run/current-system/sw/bin/",
	// A distribution package, such as the AUR build.
	"/usr/bin/",
}

// programCandidates are the fixed paths for one program, in order.
func programCandidates(program string) []string {
	out := make([]string, 0, len(remoteBinaryDirs))
	for _, d := range remoteBinaryDirs {
		out = append(out, d+program)
	}
	return out
}

// remoteBinaryCandidates are the fixed paths for every program, program by
// program in RemotePrograms order.
func remoteBinaryCandidates() []string {
	var out []string
	for _, p := range RemotePrograms() {
		out = append(out, programCandidates(p)...)
	}
	return out
}

// RemoteBinaryCandidates lists the fixed places the link looks, in order,
// spelled the way a person reads them. It is what a failed test prints so the
// person can see at once that their install is somewhere unusual.
func RemoteBinaryCandidates() []string {
	c := remoteBinaryCandidates()
	out := make([]string, 0, len(c))
	for _, p := range c {
		out = append(out, strings.Replace(p, "$HOME/", "~/", 1))
	}
	return out
}

// linkCommandPrefix begins the line the probe prints ahead of the preamble to
// say which binary it is about to run.
const linkCommandPrefix = "TUIOS-LINK-COMMAND "

// linkNoCommand is the line the probe prints when it found nothing.
const linkNoCommand = "TUIOS-LINK-NO-COMMAND"

// linkCommandLimit bounds the path the probe reports. It comes from another
// machine, so it gets a ceiling like everything else that crosses.
const linkCommandLimit = 512

// remoteProbeScript is the sh script that finds the program and runs it with the
// script's own positional arguments. announce is whether it prints the chosen
// path ahead of the program's output: the link wants that line, an interactive
// open does not.
//
// The script is one line, and it contains no single quote, no backslash, no
// newline and no exclamation mark. That is what lets it be sent inside single
// quotes through any login shell on the far side: sh, bash, zsh, fish and csh
// all read such a string literally. `case` is kept out of command
// substitutions, which older dash releases fail to parse there.
func remoteProbeScript(announce bool) string {
	var b strings.Builder
	write := func(s string) {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(s)
	}
	for _, s := range findBinaryStatements() {
		write(s)
	}

	// Run it, or say that nothing was found. 127 is what a shell exits with
	// for a command it could not find, so a caller that already keys on it
	// reads this the same way.
	if announce {
		write(`if [ -n "$p" ]; then echo "` + linkCommandPrefix + `$p"; exec "$p" "$@"; fi`)
		write(`echo "` + linkNoCommand + `"`)
	} else {
		write(`if [ -n "$p" ]; then exec "$p" "$@"; fi`)
		write(`echo "` + remoteProgramsText() + ` was not found on this host" >&2`)
	}
	write(`exit 127`)
	return b.String()
}

// FindBinaryScript is the part of the link's probe that finds the program: sh
// statements, joined by "; ", that leave the path in $p, or $p empty when
// nothing was found. It obeys the same quoting rules as the probe, so a caller
// can put it inside single quotes for any login shell. tuios hosts sync uses
// it, so it looks for the binary exactly where the link does.
func FindBinaryScript() string {
	return strings.Join(findBinaryStatements(), "; ")
}

// findBinaryStatements are the three lookups. Steps 1 and 2 are cheap and
// are taken program by program, so a program early in RemotePrograms wins at
// any cheap place over a later one on the PATH. Step 3 runs once, asking the
// login shell for every program in order.
func findBinaryStatements() []string {
	var out []string
	write := func(s string) { out = append(out, s) }
	programs := RemotePrograms()

	// first keeps the first executable absolute path of what it reads, so a
	// greeting ahead of the answer is skipped, and `command -v` naming a
	// function or an alias is not taken.
	write(`first() { while IFS= read -r l; do case $l in /*) if [ -x "$l" ]; then printf "%s" "$l"; return; fi;; esac; done; }`)
	write(`p=`)
	for _, prog := range programs {
		// 1. The PATH.
		write(`if [ -z "$p" ]; then p=$(command -v ` + prog + ` 2>/dev/null | first); fi`)
		// 2. The fixed list.
		var quoted []string
		for _, c := range programCandidates(prog) {
			quoted = append(quoted, `"`+c+`"`)
		}
		write(`if [ -z "$p" ]; then for c in ` + strings.Join(quoted, " ") +
			`; do if [ -x "$c" ]; then p=$c; break; fi; done; fi`)
	}

	// 3. The login shell, then plain sh as a login shell for a login shell
	// that is not POSIX. stdin is /dev/null so a profile that reads cannot
	// eat the link's bytes. One shell per program in order, each only while
	// nothing was found.
	write(`for n in ` + strings.Join(programs, " ") + `; do if [ -z "$p" ]; then for s in "$SHELL" sh; do p=$("$s" -l -c "command -v $n" </dev/null 2>/dev/null | first); if [ -n "$p" ]; then break; fi; done; fi; done`)
	return out
}

// remoteCommand is the command string ssh runs on the far side to start the
// link's program with args. args must already be safe for the remote shell.
//
// A configured command is sent as written, unquoted, so a "~/.local/bin/tuios"
// is expanded by the remote shell. Without one, the probe script runs as
// `sh -c '<script>' sh <args>`, and the script execs what it finds with the
// same args.
func (h Host) remoteCommand(announce bool, args ...string) string {
	if h.Command != "" {
		return strings.Join(append([]string{h.Command}, args...), " ")
	}
	return ShellCommand(remoteProbeScript(announce), args...)
}

// ShellCommand is the command string that runs script under sh on the far
// side with args as its positional parameters: sh -c '<script>' sh <args>.
// script must follow remoteProbeScript's rules (no single quote, backslash,
// newline or exclamation mark), and every arg must already be safe for the
// remote shell; see SafeRemoteArg.
func ShellCommand(script string, args ...string) string {
	parts := []string{"sh", "-c", "'" + script + "'", "sh"}
	return strings.Join(append(parts, args...), " ")
}

// SafeRemoteArg reports whether s can be sent to the remote shell as one
// plain word.
func SafeRemoteArg(s string) bool {
	return remoteArgPattern().MatchString(s)
}

// cacheableRemotePath reports whether a path the probe found can be sent back
// as the next dial's command without quoting. The remote shell re-parses the
// command, so only a path made of plain characters is safe to send bare; any
// other path is simply found again by the next probe.
func cacheableRemotePath(p string) bool {
	return p != "" && p[0] == '/' && remoteArgPattern().MatchString(p)
}

// preambleNote is what the hub learned from the lines ahead of the preamble.
type preambleNote struct {
	// command is the binary the probe said it would run, empty when the
	// remote command was configured and no probe ran.
	command string
	// missing is set when the probe said it found nothing.
	missing bool
}

// noteLine reads one line ahead of the preamble.
func (n *preambleNote) noteLine(line string) {
	switch {
	case line == linkNoCommand:
		n.missing = true
	case strings.HasPrefix(line, linkCommandPrefix):
		p := strings.TrimSpace(strings.TrimPrefix(line, linkCommandPrefix))
		if len(p) > linkCommandLimit {
			p = p[:linkCommandLimit]
		}
		n.command = p
	}
}
