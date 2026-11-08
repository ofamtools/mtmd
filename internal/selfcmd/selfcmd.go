// Package selfcmd is the argv prefix by which this program runs itself again.
//
// Upstream tuios is a binary named tuios, so a re-exec is os.Executable plus
// the subcommand ("daemon", "agent-proto", "look pick"). When the code is
// linked into another program (a host that calls mtmd.Main behind a
// subcommand of its own), a re-exec must name the host and the word that
// routes to the embedded command line. The host sets the prefix once, before
// Main reads anything; the zero value is upstream's behaviour exactly.
//
// This is the fork's one seam for self-exec; see FORK.md.
package selfcmd

import (
	"os"
	"sync/atomic"
)

type prefix struct {
	exe   string
	words []string
}

var current atomic.Pointer[prefix]

// Set installs the prefix: prefix[0] is the executable (empty means
// os.Executable), the rest are the words the host routes on, for example
// {"", "mtmd"} so that "mtm mtmd daemon" is the daemon. A nil or empty
// prefix clears it.
func Set(p []string) {
	if len(p) == 0 {
		current.Store(nil)
		return
	}
	current.Store(&prefix{exe: p[0], words: append([]string(nil), p[1:]...)})
}

// Embedded reports whether a host installed a prefix.
func Embedded() bool { return current.Load() != nil }

// Exe is the executable a re-exec runs.
func Exe() (string, error) {
	if p := current.Load(); p != nil && p.exe != "" {
		return p.exe, nil
	}
	return os.Executable()
}

// Words are the host's words that go between the executable and the tuios
// arguments. Nil when not embedded.
func Words() []string {
	if p := current.Load(); p != nil {
		return append([]string(nil), p.words...)
	}
	return nil
}

// Argv is exe, then the host's words, then extra. Use it where the code ran
// "exe extra...". exe may be a path other than Exe (/proc/self/exe).
func Argv(exe string, extra ...string) []string {
	out := append([]string{exe}, Words()...)
	return append(out, extra...)
}
