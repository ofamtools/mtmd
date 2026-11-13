// Package hostnames holds the names the daemon gives its folders, its socket
// and the variables it exports to panes: one table, in data.
//
// Upstream's names are the default, so a program that never calls Set behaves
// exactly as tuios does. A host that embeds the daemon (mtm) gives its own
// table through serve.Options.Names before anything resolves a path. The
// package is the fork's own (FORK.md, patch 13); the upstream files that build
// a path read it through a one-line call, so upstream merges stay small.
package hostnames

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
)

// EnvName is a variable the daemon exports beside an upstream one, with the
// same value. Name is read first: a process that finds Name set treats it as
// the value of Upstream (see ApplyEnv).
type EnvName struct {
	Name     string // the host's name, for example MTM_SOCKET
	Upstream string // the name upstream code reads and writes, for example TUIOS_SOCKET
}

// Names is the table. Every path field is one or more path elements written
// with '/', joined under the folder its comment names.
type Names struct {
	// SocketApp is the socket folder under the runtime folder
	// ($XDG_RUNTIME_DIR, /run/user/<uid>, %LOCALAPPDATA%).
	SocketApp string
	// SocketTmp is the socket folder in /tmp when there is no runtime
	// folder; the user id is appended.
	SocketTmp string
	// SocketFile is the daemon socket's file name.
	SocketFile string
	// StateApp is the default server's state folder under the state home.
	StateApp string
	// StateServers holds one state home per named server, under the state
	// home: <state home>/<StateServers>/<name>.
	StateServers string
	// ServerApp is a named server's state folder under its own state home;
	// empty is that state home itself.
	ServerApp string
	// ConfigApp is the configuration folder under the config home.
	ConfigApp string
	// Env are the variables exported beside upstream's.
	Env []EnvName
	// DropUpstream leaves the Upstream name of every Env pair out of the
	// environment of the children Mirror handles (panes, hook commands,
	// plugin processes): they carry only the host's names. False, the
	// default, keeps both names. The daemon reads both names from a
	// process it inspects (Lookup, UpstreamOf), so it works either way.
	DropUpstream bool
}

// Upstream is tuios's own table.
var Upstream = Names{
	SocketApp:    "tuios",
	SocketTmp:    "tuios-",
	SocketFile:   "tuios.sock",
	StateApp:     "tuios",
	StateServers: "tuios-servers",
	ServerApp:    "tuios",
	ConfigApp:    "tuios",
}

// MTMEnv is the complete table of variable pairs for a host that uses the
// MTM_ prefix: every TUIOS_ variable the daemon sets for a child (a pane, a
// hook command, a plugin process) or reads from a process's environment or
// its own. A host passes it, or its own copy, as Names.Env (with
// DropUpstream) and no TUIOS_ variable reaches a child. Adding a variable the
// daemon starts to set or read means adding its pair here (a test lists the
// daemon's names against this table).
//
// Left out on purpose: TUIOS_AGENT (a hint a user's wrapper sets; read as
// MTM_AGENT first, see integration.AgentHintEnvs), TUIOS_E2E* and
// TUIOS_TEST_* (test switches), TUIOS_*_LEGACY, TUIOS_DEBUG_INTERNAL,
// TUIOS_PTY_LOG, TUIOS_LOAD1, TUIOS_VIEW_ONLY, TUIOS_HANDOFF_FALLBACK (a
// marker between two daemon images, never a pane), TUIOS_SOUND_PATH (the
// Windows sound player's own script), the tuios binary's TUIOS_CELL_SIZE,
// TUIOS_KITTY_*, TUIOS_SIXEL_GRAPHICS and TUIOS_HOST_RECONNECT_BUDGET,
// TUIOS_NO_DAEMON* and
// TUIOS_WEB_PASSWORD (read by the tuios binary, not the library), and the
// TUIOS_INTEGRATION_ID/_VERSION/SKILL_SUM markers, which are text inside
// installed files and not variables.
var MTMEnv = []EnvName{
	// The pane's identity: set in every pane, hook command and plugin process.
	{Name: "MTM_ENV", Upstream: "TUIOS_ENV"},
	{Name: "MTM_SESSION", Upstream: "TUIOS_SESSION"},
	{Name: "MTM_SESSION_ID", Upstream: "TUIOS_SESSION_ID"},
	{Name: "MTM_PANE_ID", Upstream: "TUIOS_PANE_ID"},
	{Name: "MTM_WINDOW_ID", Upstream: "TUIOS_WINDOW_ID"},
	{Name: "MTM_WINDOW_NAME", Upstream: "TUIOS_WINDOW_NAME"},
	{Name: "MTM_SOCKET", Upstream: "TUIOS_SOCKET"},
	{Name: "MTM_SERVER", Upstream: "TUIOS_SERVER"},
	// Set in a pane, beyond the identity.
	{Name: "MTM_HOST", Upstream: "TUIOS_HOST"},
	{Name: "MTM_PANE_TOKEN", Upstream: "TUIOS_PANE_TOKEN"},
	{Name: "MTM_PANE_GRANTS", Upstream: "TUIOS_PANE_GRANTS"},
	{Name: "MTM_PANE_TTY", Upstream: "TUIOS_PANE_TTY"},
	{Name: "MTM_PANE_HOSTED", Upstream: "TUIOS_PANE_HOSTED"},
	{Name: "MTM_SESSION_REMOTE", Upstream: "TUIOS_SESSION_REMOTE"},
	{Name: "MTM_RESTORED", Upstream: "TUIOS_RESTORED"},
	{Name: "MTM_KITTY_ANIMATION", Upstream: "TUIOS_KITTY_ANIMATION"},
	// Set in a plugin process.
	{Name: "MTM_PLUGIN_ID", Upstream: "TUIOS_PLUGIN_ID"},
	{Name: "MTM_PLUGIN_TOKEN", Upstream: "TUIOS_PLUGIN_TOKEN"},
	// Set in a hook command.
	{Name: "MTM_EVENT", Upstream: "TUIOS_EVENT"},
	{Name: "MTM_WORKSPACE", Upstream: "TUIOS_WORKSPACE"},
	{Name: "MTM_PREV_WORKSPACE", Upstream: "TUIOS_PREV_WORKSPACE"},
	{Name: "MTM_LAYOUT", Upstream: "TUIOS_LAYOUT"},
	{Name: "MTM_WIDTH", Upstream: "TUIOS_WIDTH"},
	{Name: "MTM_HEIGHT", Upstream: "TUIOS_HEIGHT"},
	{Name: "MTM_AGENT_STATE", Upstream: "TUIOS_AGENT_STATE"},
	{Name: "MTM_AGENT_PREV_STATE", Upstream: "TUIOS_AGENT_PREV_STATE"},
	{Name: "MTM_AGENT_HARNESS", Upstream: "TUIOS_AGENT_HARNESS"},
	{Name: "MTM_AGENT_MESSAGE", Upstream: "TUIOS_AGENT_MESSAGE"},
	{Name: "MTM_COMMAND", Upstream: "TUIOS_COMMAND"},
	{Name: "MTM_EXIT_CODE", Upstream: "TUIOS_EXIT_CODE"},
	{Name: "MTM_DURATION_MS", Upstream: "TUIOS_DURATION_MS"},
	{Name: "MTM_THEME", Upstream: "TUIOS_THEME"},
	{Name: "MTM_THEME_BG", Upstream: "TUIOS_THEME_BG"},
	{Name: "MTM_THEME_LIGHT", Upstream: "TUIOS_THEME_LIGHT"},
	{Name: "MTM_WORKTREE_PATH", Upstream: "TUIOS_WORKTREE_PATH"},
	{Name: "MTM_WORKTREE_BRANCH", Upstream: "TUIOS_WORKTREE_BRANCH"},
	{Name: "MTM_WORKTREE_REPO_ROOT", Upstream: "TUIOS_WORKTREE_REPO_ROOT"},
	{Name: "MTM_WORKTREE_CREATED_BRANCH", Upstream: "TUIOS_WORKTREE_CREATED_BRANCH"},
	{Name: "MTM_SETTLE_REASON", Upstream: "TUIOS_SETTLE_REASON"},
	// Read by the daemon from its own environment (ApplyEnv makes the MTM_
	// name win), and from a client's.
	{Name: "MTM_ALLOW_NESTED", Upstream: "TUIOS_ALLOW_NESTED"},
	{Name: "MTM_AGENT_AUTODETECT", Upstream: "TUIOS_AGENT_AUTODETECT"},
	{Name: "MTM_AGENT_DETECT_SECONDS", Upstream: "TUIOS_AGENT_DETECT_SECONDS"},
	{Name: "MTM_AGENT_BINARIES", Upstream: "TUIOS_AGENT_BINARIES"},
	{Name: "MTM_AGENT_STALL_SECONDS", Upstream: "TUIOS_AGENT_STALL_SECONDS"},
	{Name: "MTM_CLEANUP_SECONDS", Upstream: "TUIOS_CLEANUP_SECONDS"},
	{Name: "MTM_TRANSFER_KEEP_SECONDS", Upstream: "TUIOS_TRANSFER_KEEP_SECONDS"},
	{Name: "MTM_SUBAGENT_FADE_SECONDS", Upstream: "TUIOS_SUBAGENT_FADE_SECONDS"},
	{Name: "MTM_PR_POLL_SECONDS", Upstream: "TUIOS_PR_POLL_SECONDS"},
	{Name: "MTM_SSH", Upstream: "TUIOS_SSH"},
	{Name: "MTM_WORKTREE_DIR", Upstream: "TUIOS_WORKTREE_DIR"},
	{Name: "MTM_HARNESS_DIR", Upstream: "TUIOS_HARNESS_DIR"},
	{Name: "MTM_LOG_LEVEL", Upstream: "TUIOS_LOG_LEVEL"},
	{Name: "MTM_NO_SOUND", Upstream: "TUIOS_NO_SOUND"},
}

var current atomic.Pointer[Names]

// Get is the table in force.
func Get() Names {
	if n := current.Load(); n != nil {
		return *n
	}
	return Upstream
}

// Set makes n the table in force and returns the function that puts the
// previous one back. A nil n is Upstream.
func Set(n *Names) (restore func()) {
	prev := current.Load()
	if n == nil {
		current.Store(nil)
	} else {
		c := *n
		c.Env = append([]EnvName(nil), n.Env...)
		current.Store(&c)
	}
	return func() { current.Store(prev) }
}

// Path joins a '/'-separated relative path from the table under base.
func Path(base, rel string) string {
	if rel == "" {
		return base
	}
	return filepath.Join(base, filepath.FromSlash(rel))
}

// TmpSocketDir is the socket folder in tmp for uid.
func (n Names) TmpSocketDir(tmp string, uid int) string {
	return filepath.Join(tmp, n.SocketTmp+strconv.Itoa(uid))
}

// StateDir is a server's state folder under the state home base: the
// StateApp folder for the default server (""), the ServerApp folder of
// <base>/<StateServers>/<server> for a named one.
func (n Names) StateDir(base, server string) string {
	if server == "" {
		return Path(base, n.StateApp)
	}
	return Path(filepath.Join(Path(base, n.StateServers), server), n.ServerApp)
}

// ConfigFile is rel under the configuration folder, relative to the config
// home, as xdg.ConfigFile and xdg.SearchConfigFile take it.
func ConfigFile(rel string) string {
	return Get().ConfigApp + "/" + rel
}

// ApplyEnv makes this process read the host's variables first: for every
// pair whose Name is set, Upstream takes its value. Upstream code then reads
// the host's value wherever it reads its own variable.
func ApplyEnv() {
	for _, e := range Get().Env {
		if v, ok := os.LookupEnv(e.Name); ok && v != "" {
			_ = os.Setenv(e.Upstream, v)
		}
	}
}

// Setenv sets upstream in this process's environment and the host's name
// paired with it, so a child started from here reads the same value
// under either name.
func Setenv(upstream, value string) error {
	for _, e := range Get().Env {
		if e.Upstream == upstream {
			if err := os.Setenv(e.Name, value); err != nil {
				return err
			}
		}
	}
	return os.Setenv(upstream, value)
}

// Unsetenv clears upstream and the host's name paired with it.
func Unsetenv(upstream string) error {
	for _, e := range Get().Env {
		if e.Upstream == upstream {
			_ = os.Unsetenv(e.Name)
		}
	}
	return os.Unsetenv(upstream)
}

// Mirror returns env with the host's names set from upstream's: every
// assignment of a Name is dropped, and each Name is added with the value of
// the last assignment of its Upstream. With DropUpstream the Upstream
// assignments are removed too. A nil env (inherit the parent's) is
// returned as it is. With no pairs in the table env is returned unchanged.
func Mirror(env []string) []string {
	pairs := Get().Env
	if env == nil || len(pairs) == 0 {
		return env
	}
	host := make(map[string]bool, len(pairs))
	for _, e := range pairs {
		host[e.Name] = true
	}
	last := make(map[string]string, len(pairs))
	upstream := make(map[string]bool, len(pairs))
	for _, e := range pairs {
		upstream[e.Upstream] = true
	}
	drop := Get().DropUpstream
	out := make([]string, 0, len(env)+len(pairs))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if host[k] {
			continue
		}
		if drop && upstream[k] {
			last[k] = v
			continue
		}
		out = append(out, kv)
		last[k] = v
	}
	for _, e := range pairs {
		if v, ok := last[e.Upstream]; ok {
			out = append(out, e.Name+"="+v)
		}
	}
	return out
}

// IsHost reports whether name is the host's name in a pair.
func IsHost(name string) bool {
	for _, e := range Get().Env {
		if e.Name == name {
			return true
		}
	}
	return false
}

// HostName is the host's name paired with upstream, or "" when there is none.
func HostName(upstream string) string {
	for _, e := range Get().Env {
		if e.Upstream == upstream {
			return e.Name
		}
	}
	return ""
}

// UpstreamOf is the upstream name paired with name (a host name or an
// upstream one), or name itself when it is in no pair.
func UpstreamOf(name string) string {
	for _, e := range Get().Env {
		if e.Name == name || e.Upstream == name {
			return e.Upstream
		}
	}
	return name
}

// Lookup reads upstream through get, the host's name first, so a source
// that holds only the host's names (a pane started with DropUpstream) is
// read as well as one that holds both.
func Lookup(get func(name string) (string, bool), upstream string) (string, bool) {
	if h := HostName(upstream); h != "" {
		if v, ok := get(h); ok {
			return v, true
		}
	}
	return get(upstream)
}

// Getenv is Lookup over this process's environment.
func Getenv(upstream string) string {
	v, _ := Lookup(os.LookupEnv, upstream)
	return v
}
