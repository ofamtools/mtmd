// Package serve is the daemon-only door to the o-family fork of tuios (mtmd).
//
// A host program (mtm) links this package and calls Main behind a subcommand of
// its own. Main runs the part of tuios's command line a host needs and nothing
// else: the daemon, its background starter, the live-upgrade preflight, the
// pane program that protocol agents run (agent-proto) and the plugin reaper.
// The rest of tuios's command line is not in this module; see FORK.md.
//
// Every re-exec the daemon makes (a protocol pane, a live upgrade, the
// start-server child) goes through internal/selfcmd, so with the host's
// SelfCommand prefix it runs "mtm <words> daemon ...", never a tuios binary.
package serve

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
	"ofam.dev/mtmd/internal/config"
	"ofam.dev/mtmd/internal/federation"
	"ofam.dev/mtmd/internal/guestenv"
	"ofam.dev/mtmd/internal/herdrplugin"
	"ofam.dev/mtmd/internal/hooks"
	"ofam.dev/mtmd/internal/hostnames"
	"ofam.dev/mtmd/internal/selfcmd"
	"ofam.dev/mtmd/internal/serverscope"
	"ofam.dev/mtmd/internal/session"
	"ofam.dev/mtmd/internal/vt"
)

// Options is what a host tells Main.
type Options struct {
	// SelfCommand is the prefix by which the host runs itself again:
	// SelfCommand[0] is the executable (empty means os.Executable), the rest
	// are the words the host routes on to reach Main, for example
	// {"", "mtmd"} so that "mtm mtmd daemon" is the daemon. Nil is upstream's
	// behaviour: the program re-runs itself as "exe daemon".
	SelfCommand []string
	// Version is the build label the daemon reports in hello and writes in
	// its live-upgrade preflight. Empty means DefaultVersion.
	Version string
	// Names is the table of folder, socket and variable names the daemon
	// uses (hostnames). Nil is upstream's. With a table of its own, a daemon
	// or start-server first moves a named server's state from upstream's
	// folders (Migrate), and every pane gets the table's variables beside
	// TUIOS_ ones.
	Names *Names
	// RemotePrograms are the programs a link looks for on another machine,
	// in the order it tries them, when that host's row has no command. Nil
	// is DefaultRemotePrograms. A host that has moved every machine to
	// itself passes just its own name.
	RemotePrograms []string
}

// DefaultRemotePrograms is the link's list when a host gives none: mtm,
// then tuios.
var DefaultRemotePrograms = federation.DefaultRemotePrograms

// Names is the table of names a host gives the daemon; see Options.Names.
type Names = hostnames.Names

// EnvName is one variable exported beside an upstream one.
type EnvName = hostnames.EnvName

// MTMEnv is the complete table of MTM_ variable pairs (hostnames.MTMEnv):
// pass it as Names.Env, with DropUpstream, so a host keeps no copy of the pairs.
var MTMEnv = hostnames.MTMEnv

// UpstreamNames is tuios's own table.
var UpstreamNames = hostnames.Upstream

// DefaultVersion is the label when a host gives none.
const DefaultVersion = "v0.12.1-mtmd"

const (
	usage        = "usage: [--server NAME] daemon [--log-level L] [--no-restore] | daemon handoff-accepts | start-server | [--server NAME] migrate-paths [--dry-run] [--rollback] | agent-proto --protocol acp|codex [--harness H] [--cwd D] -- AGENT... | [--server NAME] stdio-proxy [--as PEER] | __plugin-reaper PROGRAM..."
	adoptFDFlag  = "adopt-fd"
	startTimeout = 5 * time.Second
)

// Main runs one of the daemon-side commands and returns the exit status. args
// are the words after the host's own (os.Args[1:] of a tuios binary).
func Main(args []string, opt Options) int {
	selfcmd.Set(opt.SelfCommand)
	// The host's names, before anything resolves a path; and its variables
	// read first (FORK.md patch 13).
	hostnames.Set(opt.Names)
	hostnames.ApplyEnv()
	version := opt.Version
	if version == "" {
		version = DefaultVersion
	}
	vt.SetBuildVersion(version)
	guestenv.SetBuildVersion(version)
	if dropped := federation.SetRemotePrograms(opt.RemotePrograms); len(dropped) > 0 {
		fmt.Fprintf(os.Stderr, "mtmd: remote programs %q are not plain file names and are left out.\n", dropped)
	}

	// The reaper does nothing else (internal/herdrplugin, reaper.go).
	if len(args) > 0 && args[0] == herdrplugin.ReaperArg {
		return herdrplugin.RunReaper(args[1:])
	}
	// The server a pane's daemon was started for reaches every program run in
	// that pane through TUIOS_SERVER; --server replaces it below.
	if err := serverscope.ApplyEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "tuios: %v.\n", err)
		return 1
	}
	p, err := parse(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if p.serverSet {
		if err := serverscope.Apply(p.server); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	r := &run{p: p, version: version}
	switch p.verb {
	case "start-server":
		if code := r.migrateOnStart(); code != 0 {
			return code
		}
		return r.startServer()
	case "daemon":
		if len(p.rest) > 0 && p.rest[0] == "handoff-accepts" {
			return r.handoffAccepts()
		}
		if p.adoptFD == 0 {
			if code := r.migrateOnStart(); code != 0 {
				return code
			}
		}
		return r.daemon()
	case "migrate-paths":
		return r.migratePaths()
	case "agent-proto":
		return r.agentProto()
	case "stdio-proxy":
		return r.stdioProxy()
	}
	fmt.Fprintln(os.Stderr, usage)
	return 2
}

// parsed is a command line, split into the facade's flags and the rest.
type parsed struct {
	args      []string // as given
	verb      string
	server    string
	serverSet bool
	logLevel  string
	noRestore bool
	dryRun    bool
	rollback  bool
	adoptFD   int
	protocol  string
	harness   string
	cwd       string
	as        string   // stdio-proxy --as: the peer name this machine's link policy is resolved for
	rest      []string // arguments after the verb that are not flags (agent argv, subcommand)
}

func parse(args []string) (*parsed, error) {
	p := &parsed{args: args}
	// value reads "--name VALUE" or "--name=VALUE" at args[i].
	value := func(i *int, name string) (string, bool, error) {
		a := args[*i]
		if a == name {
			if *i+1 >= len(args) {
				return "", false, fmt.Errorf("%s needs a value", name)
			}
			*i++
			return args[*i], true, nil
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v, true, nil
		}
		return "", false, nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			p.rest = append(p.rest, args[i+1:]...)
			break
		}
		if p.verb != "" && !strings.HasPrefix(a, "-") {
			p.rest = append(p.rest, args[i:]...)
			break
		}
		matched := false
		for _, f := range []struct {
			name string
			dst  *string
			set  *bool
		}{
			{"--server", &p.server, &p.serverSet},
			{"--log-level", &p.logLevel, nil},
			{"--protocol", &p.protocol, nil},
			{"--harness", &p.harness, nil},
			{"--cwd", &p.cwd, nil},
			{"--as", &p.as, nil},
		} {
			v, ok, err := value(&i, f.name)
			if err != nil {
				return nil, err
			}
			if ok {
				*f.dst = v
				if f.set != nil {
					*f.set = true
				}
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		switch {
		case a == "--no-restore":
			p.noRestore = true
		case a == "--dry-run":
			p.dryRun = true
		case a == "--rollback":
			p.rollback = true
		case strings.HasPrefix(a, "--"+adoptFDFlag+"="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--"+adoptFDFlag+"="))
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("bad %s", a)
			}
			p.adoptFD = n
		case a == "--"+adoptFDFlag && i+1 < len(args):
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("bad --%s %s", adoptFDFlag, args[i])
			}
			p.adoptFD = n
		case strings.HasPrefix(a, "-"):
			return nil, fmt.Errorf("unknown flag %s", a)
		default:
			p.verb = a
		}
	}
	if p.verb == "" {
		return nil, fmt.Errorf("no command")
	}
	return p, nil
}

type run struct {
	p       *parsed
	version string
}

// handoffArgs is the command line, after the program, that runs the next
// image of this daemon: what this process was given, without the adopt flag,
// plus the descriptor of the manifest. A live upgrade execs it as
// selfcmd.Argv(bin, handoffArgs...).
func (r *run) handoffArgs(manifestFD int) []string {
	out := make([]string, 0, len(r.p.args)+1)
	for i := 0; i < len(r.p.args); i++ {
		a := r.p.args[i]
		if a == "--"+adoptFDFlag {
			i++
			continue
		}
		if strings.HasPrefix(a, "--"+adoptFDFlag+"=") {
			continue
		}
		out = append(out, a)
	}
	return append(out, "--"+adoptFDFlag+"="+strconv.Itoa(manifestFD))
}

// daemonConfig is the configuration of the daemon this process runs, with its
// logging installed.
func (r *run) daemonConfig() *session.DaemonConfig {
	var cfg *session.DaemonConfig
	if uc, err := config.LoadUserConfig(); err == nil {
		cfg = session.DaemonConfigFromUser(uc)
	} else {
		cfg = session.DaemonConfigFromUser(nil)
	}
	cfg.Version = r.version
	cfg.DisableAutoRestore = r.p.noRestore
	cfg.HandoffArgs = r.handoffArgs
	// A terminal on stderr makes this a foreground daemon for logging. The
	// background daemon has stderr pointed at its log file, so echoing there
	// would write every line twice.
	cfg.Foreground = term.IsTerminal(int(os.Stderr.Fd()))
	session.InstallDaemonLogging(cfg.Foreground, cfg.LogFile)
	hooks.SetVerbose(session.GetDebugLevel() >= session.DebugBasic)
	return cfg
}

func (r *run) daemon() int {
	if err := session.CheckSocketEnv(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if r.p.logLevel != "" {
		session.SetDebugLevel(session.ParseDebugLevel(r.p.logLevel))
	}
	// Plugin commands run under the reaper: this program again.
	if reaper := pluginReaper(); reaper != nil {
		session.PluginReaper = reaper
	}
	var err error
	if r.p.adoptFD > 0 {
		// The socket is live, so the running-daemon check a fresh start makes
		// would find this very process.
		err = session.NewDaemon(r.daemonConfig()).RunAdopted(r.p.adoptFD)
	} else {
		if session.IsDaemonRunning() {
			if pid := session.GetDaemonPID(); pid > 0 {
				fmt.Fprintf(os.Stderr, "a daemon is already running (PID %d)\n", pid)
			} else {
				fmt.Fprintln(os.Stderr, "a daemon is already running")
			}
			return 1
		}
		err = session.NewDaemon(r.daemonConfig()).Run()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// handoffAccepts is the preflight a live upgrade runs on the binary it is
// about to exec: it prints which handoff manifests this binary reads.
func (r *run) handoffAccepts() int {
	data, err := json.Marshal(session.LocalHandoffAccepts(r.version))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_, _ = fmt.Fprintln(os.Stdout, string(data))
	return 0
}

// startServer spawns a detached daemon and returns once one is reachable.
// Success is "a daemon is up", not "my child is up": two clients can start one
// at the same moment, the daemon's start lock picks a winner and the loser
// exits.
func (r *run) startServer() int {
	if err := session.CheckSocketEnv(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if session.IsDaemonRunning() {
		return 0
	}
	exe, err := selfcmd.Exe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get executable path: %v\n", err)
		return 1
	}
	var dargs []string
	if r.p.serverSet {
		dargs = append(dargs, "--server", r.p.server)
	}
	dargs = append(dargs, "daemon")
	if r.p.logLevel != "" {
		dargs = append(dargs, "--log-level", r.p.logLevel)
	}
	if r.p.noRestore {
		dargs = append(dargs, "--no-restore")
	}
	argv := selfcmd.Argv(exe, dargs...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout = nil, nil
	if f := daemonStderr(); f != nil {
		cmd.Stderr = f
		defer f.Close()
	}
	cmd.SysProcAttr = daemonSysProcAttr()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start daemon: %v\n", err)
		return 1
	}
	// Reap the child: one that loses the start race exits at once and would
	// sit as a zombie for as long as this process runs.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.Now().Add(startTimeout)
	var childErr error
	childGone := false
	for {
		if session.IsDaemonRunning() {
			return 0
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case childErr = <-exited:
			childGone = true
			exited = nil
			// The child may have lost the lock to another client's daemon:
			// give that one a moment to finish binding.
			if grace := time.Now().Add(time.Second); grace.Before(deadline) {
				deadline = grace
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	switch {
	case childGone && childErr != nil:
		fmt.Fprintf(os.Stderr, "the daemon exited immediately: %v\n", childErr)
	case childGone:
		fmt.Fprintln(os.Stderr, "the daemon exited immediately without taking the socket")
	default:
		fmt.Fprintf(os.Stderr, "daemon did not start within %v\n", startTimeout)
	}
	return 1
}

// daemonStderr opens the daemon log file for the child's stderr, or nil. It is
// for the one thing the daemon cannot log: a Go runtime panic writes straight
// to stderr, and then the process is gone.
func daemonStderr() *os.File {
	path := session.DefaultDaemonLogPath()
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o700)
	}
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return fh
}

// pluginReaper is the argv the daemon runs a plugin command under, or nil
// where no native plugin runs. On Linux it names /proc/self/exe, which still
// runs this build after an update replaced the file.
func pluginReaper() []string {
	switch runtime.GOOS {
	case "linux":
		return selfcmd.Argv("/proc/self/exe", herdrplugin.ReaperArg)
	case "darwin":
		if exe, err := selfcmd.Exe(); err == nil {
			return selfcmd.Argv(exe, herdrplugin.ReaperArg)
		}
	}
	return nil
}
