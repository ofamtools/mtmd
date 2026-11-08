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
// the last assignment of its Upstream. A nil env (inherit the parent's) is
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
	out := make([]string, 0, len(env)+len(pairs))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if host[k] {
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
