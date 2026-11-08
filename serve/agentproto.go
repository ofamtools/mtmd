package serve

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"
	"ofam.dev/mtmd/internal/agentproto"
	"ofam.dev/mtmd/internal/session"
)

// agentProto runs a coding agent headless, over ACP or the Codex app-server
// protocol, and shows it as a transcript with a prompt line. It is what
// start-agent --protocol runs in the pane it opens (the daemon re-executes the
// host with the SelfCommand prefix and "agent-proto --protocol P -- AGENT...").
// The cobra command of upstream's cmd/tuios (agent_proto_command.go) is not in
// this module; this is the same body without it.
func (r *run) agentProto() int {
	argv := r.p.rest
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "agent-proto needs the agent to run after --")
		return 2
	}
	if r.p.protocol == "" {
		fmt.Fprintln(os.Stderr, "agent-proto needs --protocol acp|codex")
		return 2
	}
	if err := agentproto.CheckProtocol(r.p.protocol); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	cwd := r.p.cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	harness := r.p.harness
	if harness == "" {
		harness = r.p.protocol
	}
	version := r.version

	stdinFd := int(os.Stdin.Fd())
	if term.IsTerminal(stdinFd) {
		if old, err := term.MakeRaw(stdinFd); err == nil {
			defer func() { _ = term.Restore(stdinFd, old) }()
		}
	}
	// Bracketed paste, so a prompt tuios types arrives as one paste.
	_, _ = io.WriteString(os.Stdout, "\x1b[?2004h")
	defer func() { _, _ = io.WriteString(os.Stdout, "\x1b[?2004l\r\n") }()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()

	proc, err := agentproto.StartProcess(argv, cwd, nil)
	if err != nil {
		fmt.Fprintf(os.Stdout, "could not start %s: %v\r\n", strings.Join(argv, " "), err)
		return 1
	}
	defer proc.Stop()

	s := &agentproto.Session{
		Events: agentproto.NewEvents(),
		In:     os.Stdin,
		Out:    os.Stdout,
		Width: func() int {
			w, _, err := term.GetSize(int(os.Stdout.Fd()))
			if err != nil {
				return 0
			}
			return w
		},
		Header: r.p.protocol + ": " + strings.Join(argv, " "),
		Cwd:    cwd,
		Stderr: proc.Stderr,
	}
	agent, _ := agentproto.NewAgent(r.p.protocol, proc.Stdout, proc.Stdin, version, s.Emit)
	s.Agent = agent
	if window := os.Getenv("TUIOS_PANE_ID"); window != "" {
		rep := &paneReporter{
			session: os.Getenv("TUIOS_SESSION"),
			window:  window,
			harness: harness,
			dial:    func() (*session.VerbClient, error) { return session.DialVerbClientAs(version) },
		}
		defer rep.close()
		s.Reporter = rep
	}
	return s.Run(ctx)
}
