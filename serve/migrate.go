package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/adrg/xdg"
	"ofam.dev/mtmd/internal/hostnames"
	"ofam.dev/mtmd/internal/serverscope"
	"ofam.dev/mtmd/internal/sockpath"
)

// The one-time move of a named server's state from upstream's folders to the
// host's (FORK.md patch 13). A daemon or start-server whose host gives a table
// of its own runs it before anything opens the state; migrate-paths runs it
// on its own, as a plan (--dry-run) or backwards (--rollback).
//
// Forward: when the server's state folder under the host's names is absent
// and the one under upstream's names exists, it is renamed (same volume)
// into place, and a MOVED-TO file in the folder that held it says where it
// went. The saved sessions and the scrollback cold logs live inside that
// folder and move with it. The configuration file is copied, not moved, and
// only when the host's has none: the old one stays for the owner to delete.
// A daemon still answering on the upstream socket is a live writer, and the
// move is refused, changing nothing. Run again it does nothing.
//
// Rollback: the reverse rename, refused while a daemon answers on the
// host's socket. The copied configuration file stays.

const (
	movedToFile   = "MOVED-TO"
	movedFromFile = "MOVED-FROM"
	configName    = "config.toml"
)

// errLiveWriter is a refusal: a daemon runs on the side being moved from.
var errLiveWriter = errors.New("a daemon is running on the socket the state would move from")

// Migration is what one run did or, for a plan, would do.
type Migration struct {
	Server       string `json:"server"`
	Direction    string `json:"direction"` // forward | rollback
	DryRun       bool   `json:"dry_run"`
	State        string `json:"state"` // moved | would-move | nothing | refused
	Reason       string `json:"reason,omitempty"`
	From         string `json:"from"`
	To           string `json:"to"`
	Marker       string `json:"marker,omitempty"`
	LiveSocket   string `json:"live_socket,omitempty"`
	Config       string `json:"config"` // copied | would-copy | nothing
	ConfigFrom   string `json:"config_from,omitempty"`
	ConfigTo     string `json:"config_to,omitempty"`
	ConfigReason string `json:"config_reason,omitempty"`
}

// migrateEnv is where the migration looks; the tests give their own.
type migrateEnv struct {
	stateHome  string                           // the base state home (no server scope)
	configHome string                           // where the host's configuration goes
	findConfig func(rel string) (string, error) // an existing file under the config homes
	socket     func(n hostnames.Names) string   // the server's daemon socket under n
	live       func(socket string) bool
	now        func() time.Time
}

func defaultMigrateEnv() migrateEnv {
	return migrateEnv{
		stateHome:  serverscope.BaseStateHome(),
		configHome: xdg.ConfigHome,
		findConfig: xdg.SearchConfigFile,
		socket: func(n hostnames.Names) string {
			return filepath.Join(sockpath.DirFor(n), n.SocketFile)
		},
		live: socketLive,
		now:  time.Now,
	}
}

// socketLive reports whether a daemon owns socket: it accepts a connection,
// or its pid file names a live process (one that is starting, or stuck,
// still writes to the state).
func socketLive(socket string) bool {
	if conn, err := net.DialTimeout("unix", socket, time.Second); err == nil {
		_ = conn.Close()
		return true
	}
	data, err := os.ReadFile(socket + ".pid")
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	return err == nil && pidAlive(pid)
}

// migrate moves server's state from the names of from to those of to (or
// back, with rollback). An error is a refusal or a failed step; the report
// says how far it got.
func migrate(from, to hostnames.Names, server string, rollback, dry bool, env migrateEnv) (Migration, error) {
	m := Migration{Server: server, Direction: "forward", DryRun: dry, State: "nothing", Config: "nothing"}
	if rollback {
		m.Direction = "rollback"
	}
	if server == "" {
		m.Reason = "the default server is not moved: its state shares a folder with the configuration"
		m.ConfigReason = m.Reason
		return m, nil
	}
	src, dst := from.StateDir(env.stateHome, server), to.StateDir(env.stateHome, server)
	liveSide := from
	if rollback {
		src, dst = dst, src
		liveSide = to
	}
	m.From, m.To = src, dst
	if filepath.Clean(src) == filepath.Clean(dst) {
		m.Reason = "both tables name the same folder"
		m.ConfigReason = m.Reason
		return m, nil
	}
	if sock := env.socket(liveSide); env.live(sock) {
		m.State, m.LiveSocket = "refused", sock
		m.Reason = "stop that daemon first; nothing was changed"
		return m, fmt.Errorf("%w (%s): stop it first; nothing was changed", errLiveWriter, sock)
	}
	switch {
	case exists(dst):
		m.Reason = "the state is already at " + dst
	case !exists(src):
		m.Reason = "there is no state at " + src
	case dry:
		m.State = "would-move"
	default:
		if err := moveState(src, dst, rollback, env.now()); err != nil {
			m.Reason = err.Error()
			return m, err
		}
		m.State = "moved"
	}
	if !rollback && m.State != "nothing" {
		m.Marker = filepath.Join(filepath.Dir(src), movedToFile)
	}
	if rollback {
		m.ConfigReason = "a rollback leaves both configuration files"
		return m, nil
	}
	if err := copyConfig(from, to, dry, env, &m); err != nil {
		return m, err
	}
	return m, nil
}

// moveState renames src to dst and writes the markers: MOVED-TO beside the
// folder that held the state, MOVED-FROM inside it. A rollback removes them.
func moveState(src, dst string, rollback bool, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("move %s to %s: %w", src, dst, err)
	}
	if rollback {
		_ = os.Remove(filepath.Join(filepath.Dir(dst), movedToFile))
		_ = os.Remove(filepath.Join(dst, movedFromFile))
		return nil
	}
	stamp := now.UTC().Format(time.RFC3339)
	note := fmt.Sprintf("This server's state moved to %s on %s (mtmd migrate-paths).\nRoll back with: <host> --server <name> migrate-paths --rollback (the daemon stopped first).\n", dst, stamp)
	if err := os.WriteFile(filepath.Join(filepath.Dir(src), movedToFile), []byte(note), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dst, movedFromFile), []byte(src+"\n"+stamp+"\n"), 0o600)
}

// copyConfig copies the configuration file to the host's folder when the
// host has none and upstream's exists; the old file stays.
func copyConfig(from, to hostnames.Names, dry bool, env migrateEnv, m *Migration) error {
	if from.ConfigApp == to.ConfigApp {
		m.ConfigReason = "both tables name the same configuration folder"
		return nil
	}
	m.ConfigTo = hostnames.Path(env.configHome, to.ConfigApp+"/"+configName)
	if exists(m.ConfigTo) {
		m.ConfigReason = "the configuration is already at " + m.ConfigTo
		return nil
	}
	old, err := env.findConfig(from.ConfigApp + "/" + configName)
	if err != nil || !exists(old) {
		m.ConfigReason = "there is no " + from.ConfigApp + "/" + configName
		return nil
	}
	m.ConfigFrom = old
	if dry {
		m.Config = "would-copy"
		return nil
	}
	if err := copyFile(old, m.ConfigTo); err != nil {
		m.ConfigReason = err.Error()
		return err
	}
	m.Config = "copied"
	return nil
}

// copyFile copies src to dst through a temporary file renamed into place,
// with src's permission bits.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// migrateOnStart is the forward move a daemon or start-server makes before it
// opens the state, when the host gave names of its own. A refusal stops the
// start: a daemon that started anyway would make the new folder and the
// state would never move.
func (r *run) migrateOnStart() int {
	to := hostnames.Get()
	if to.StateApp == hostnames.Upstream.StateApp && to.StateServers == hostnames.Upstream.StateServers && to.ServerApp == hostnames.Upstream.ServerApp && to.ConfigApp == hostnames.Upstream.ConfigApp {
		return 0
	}
	m, err := migrate(hostnames.Upstream, to, serverscope.Name(), false, false, defaultMigrateEnv())
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate-paths: %v\n", err)
		return 1
	}
	if m.State == "moved" {
		fmt.Fprintf(os.Stderr, "migrate-paths: moved %s to %s\n", m.From, m.To)
	}
	if m.Config == "copied" {
		fmt.Fprintf(os.Stderr, "migrate-paths: copied %s to %s\n", m.ConfigFrom, m.ConfigTo)
	}
	return 0
}

// migratePaths is the migrate-paths command: the move (or its plan, or its
// rollback) as one JSON object on stdout; status 1 when refused or failed.
func (r *run) migratePaths() int {
	m, err := migrate(hostnames.Upstream, hostnames.Get(), serverscope.Name(), r.p.rollback, r.p.dryRun, defaultMigrateEnv())
	data, _ := json.MarshalIndent(m, "", "  ")
	_, _ = fmt.Fprintln(os.Stdout, string(data))
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate-paths: %v\n", err)
		return 1
	}
	return 0
}
