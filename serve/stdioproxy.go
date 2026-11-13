package serve

import (
	"fmt"
	"io"
	"net"
	"os"

	"golang.org/x/term"
	"ofam.dev/mtmd/internal/federation"
	"ofam.dev/mtmd/internal/session"
)

// stdioProxy is the far side of a link: what a hub runs over ssh as
// "<program> stdio-proxy". Copied from upstream's runStdioProxy
// (cmd/tuios/host_commands.go).
//
// It connects the link framing on stdin and stdout to this machine's daemon
// socket. It refuses to run on a terminal: frames written to a tty would be
// read as escape sequences by whatever is watching. It does not start a
// daemon: starting one restores this machine's saved sessions, a change to
// remote state nobody here asked for; the hub reports a machine with no
// daemon as such.
//
// --as is the name this machine's link policy is resolved for, whatever the
// hub says it is called. Set in a forced command in authorized_keys, it is
// the one name the hub cannot choose (session.DialForLink).
func (r *run) stdioProxy() int {
	if err := serveStdioProxy(os.Stdin, os.Stdout, term.IsTerminal(int(os.Stdin.Fd())), r.p.as); err != nil {
		fmt.Fprintf(os.Stderr, "stdio-proxy: %v\n", err)
		return 1
	}
	return 0
}

// serveStdioProxy is stdioProxy over any pipe; tty says whether in is a
// terminal.
func serveStdioProxy(in io.Reader, out io.Writer, tty bool, pinnedPeer string) error {
	if tty {
		return fmt.Errorf("it is not meant to be run by hand: the daemon on another machine runs it over ssh to read this machine's listings")
	}
	if pinnedPeer != "" {
		if err := session.ValidLinkPeerName(pinnedPeer); err != nil {
			return err
		}
	}
	socketPath, err := session.GetSocketPath()
	if err != nil {
		return err
	}
	return federation.ServeProxyFor(in, out, func(open federation.StreamOpen) (net.Conn, error) {
		return session.DialForLink(socketPath, open, pinnedPeer)
	})
}
