package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ofam.dev/mtmd/internal/herdrplugin"
	"ofam.dev/mtmd/internal/hooks"
	"ofam.dev/mtmd/internal/hostnames"
)

// With the complete MTM table and DropUpstream, no child of the daemon (a
// pane, a hook command, a plugin process) carries a TUIOS_ variable; without
// DropUpstream each carries both names. Hosted panes are built the same way
// and go through the same Mirror, so they are not repeated here.
func TestChildrenCarryNoTUIOSNamesWithCompleteTable(t *testing.T) {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "TUIOS_") || strings.HasPrefix(k, "MTM_") {
			old := os.Getenv(k)
			_ = os.Unsetenv(k)
			t.Cleanup(func() { _ = os.Setenv(k, old) })
		}
	}
	for _, drop := range []bool{false, true} {
		n := hostnames.Upstream
		n.Env, n.DropUpstream = hostnames.MTMEnv, drop
		restore := hostnames.Set(&n)

		sess, err := NewSession("names", &SessionConfig{Shell: "/bin/sh", HostName: "box",
			PaneToken: func(string) string { return "tok" }}, 80, 24)
		if err != nil {
			restore()
			t.Fatal(err)
		}
		sess.config.grants = newPaneGrantTable()
		sess.config.SocketPath = "/s"
		paneEnv := append(sess.buildEnv("win-1", true), PaneTTYEnv+"=/dev/ttys9")
		sess.Stop()

		m := &Manager{paneTokenKey: []byte("key")}
		m.socketPath.Store("/s")
		h := &pluginHost{d: &Daemon{manager: m}}
		plugEnv := h.baseEnv(&herdrplugin.Plugin{PluginID: "pl", Format: herdrplugin.FormatTuios}, herdrplugin.Dirs{})

		hm := hooks.NewManager()
		out := filepath.Join(t.TempDir(), "env")
		hm.Register(hooks.AfterFocusChange, "env > "+out)
		hm.Fire(hooks.AfterFocusChange, hooks.Context{WindowID: "w", WindowName: "n", SessionID: "s", Theme: "t"})
		hm.Wait()
		data, _ := os.ReadFile(out)
		hookEnv := strings.Split(string(data), "\n")

		for name, env := range map[string][]string{
			"pane":   hostnames.Mirror(paneEnv),
			"plugin": plugEnv, // baseEnv mirrors already
			"hook":   hookEnv,
		} {
			var tuios, mtm int
			for _, kv := range env {
				switch {
				case strings.HasPrefix(kv, "TUIOS_"):
					tuios++
					if drop {
						t.Errorf("%s: DropUpstream left %s", name, kv)
					}
				case strings.HasPrefix(kv, "MTM_"):
					mtm++
				}
			}
			if mtm == 0 || (!drop && tuios == 0) {
				t.Errorf("%s drop=%v: %d TUIOS_ and %d MTM_ names, want both without drop and MTM_ only with it", name, drop, tuios, mtm)
			}
		}
		for _, want := range []string{"MTM_HOST=box", "MTM_PANE_TOKEN=tok", "MTM_RESTORED=1", "MTM_PANE_TTY=/dev/ttys9", "MTM_PANE_GRANTS=" + sess.config.grants.envValue("win-1")} {
			if !hasKV(hostnames.Mirror(paneEnv), want) {
				t.Errorf("drop=%v: pane lacks %s", drop, want)
			}
		}
		restore()
	}
}

func hasKV(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

// A caller cannot pass the host's name of a variable the daemon sets, any
// more than the upstream one.
func TestCallerEnvRefusesHostNames(t *testing.T) {
	defer hostnames.Set(&hostnames.Names{Env: hostnames.MTMEnv})()
	for _, k := range []string{"MTM_PANE_ID", "TUIOS_PANE_ID"} {
		if _, _, verr := callerEnv(nil, map[string]string{k: "x"}); verr == nil {
			t.Errorf("%s was accepted", k)
		}
	}
}
