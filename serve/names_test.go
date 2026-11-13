package serve_test

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"ofam.dev/mtmd/internal/hostnames"
	"ofam.dev/mtmd/serve"
)

// hostNamesEnv makes the host of the test binary give serve.Main a table of
// its own, the way mtm does.
const hostNamesEnv = "MTMD_TEST_NAMES"

var testHostNames = serve.Names{
	SocketApp: "mtm", SocketTmp: "mtm-", SocketFile: "mtm.sock",
	StateApp: "mtm", StateServers: "mtm/servers", ServerApp: "", ConfigApp: "mtm",
	Env: []serve.EnvName{
		{Name: "MTM_ENV", Upstream: "TUIOS_ENV"}, {Name: "MTM_SESSION", Upstream: "TUIOS_SESSION"},
		{Name: "MTM_PANE_ID", Upstream: "TUIOS_PANE_ID"}, {Name: "MTM_WINDOW_ID", Upstream: "TUIOS_WINDOW_ID"},
		{Name: "MTM_SOCKET", Upstream: "TUIOS_SOCKET"}, {Name: "MTM_SERVER", Upstream: "TUIOS_SERVER"},
	},
}

func newNamedHost(t *testing.T) *host {
	h := newHost(t)
	h.names = &testHostNames
	h.env = append(h.env, hostNamesEnv+"=1")
	return h
}

func (h *host) seedUpstreamState() {
	h.t.Helper()
	for rel, body := range map[string]string{
		"state/tuios-servers/" + h.server + "/tuios/keep.txt": "kept",
		"config/tuios/config.toml":                            "# old config\n",
	} {
		p := filepath.Join(h.base, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
}

// A host with names of its own: start-server moves the server's state out of
// upstream's folders, listens on the host's socket, and every pane gets the
// host's variables beside TUIOS_ ones.
func TestHostNamesMigrateAndExportBoth(t *testing.T) {
	h := newNamedHost(t)
	h.seedUpstreamState()
	h.start()
	if _, err := os.Stat(h.socket()); err != nil {
		t.Fatalf("no socket at the host's path: %v", err)
	}
	newRoot := filepath.Join(h.base, "state", "mtm", "servers", h.server)
	if data, err := os.ReadFile(filepath.Join(newRoot, "keep.txt")); err != nil || string(data) != "kept" {
		t.Fatalf("state not moved: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(newRoot, "sessions")); err != nil {
		t.Errorf("the daemon does not keep its sessions in the moved folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.base, "state", "tuios-servers", h.server, "MOVED-TO")); err != nil {
		t.Errorf("no MOVED-TO marker: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(h.base, "config", "mtm", "config.toml")); err != nil || string(data) != "# old config\n" {
		t.Errorf("config not copied: %q %v", data, err)
	}
	h.must("new-session", map[string]any{"name": "s"})
	res := h.must("new-window", map[string]any{"session": "s", "name": "env", "focus": false, "command": []string{
		"sh", "-c", `echo "M=$MTM_SOCKET|T=$TUIOS_SOCKET|S=$MTM_SERVER|E=$MTM_ENV|P=$MTM_PANE_ID|W=$TUIOS_PANE_ID|N=$MTM_SESSION|"; sleep 30`,
	}})
	win := res["window_id"].(string)
	screen := h.waitPane("s", win, "N=s|", 10*time.Second)
	for _, want := range []string{"M=" + h.socket() + "|", "T=" + h.socket() + "|", "S=" + h.server + "|", "E=1|", "P=" + win + "|", "W=" + win + "|"} {
		if !strings.Contains(strings.ReplaceAll(screen, "\n", ""), want) {
			t.Errorf("pane env lacks %q:\n%s", want, screen)
		}
	}
}

// A daemon still answering on upstream's socket is a live writer: the start
// is refused and the state stays where it is.
func TestHostNamesRefuseLiveUpstreamDaemon(t *testing.T) {
	h := newNamedHost(t)
	h.seedUpstreamState()
	old := filepath.Join(h.runtime, "tuios", "servers", h.server, "tuios.sock")
	if err := os.MkdirAll(filepath.Dir(old), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", old)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	out, err := h.cmd("start-server").CombinedOutput()
	if err == nil {
		t.Fatalf("start-server ran beside a live upstream daemon:\n%s", out)
	}
	if !strings.Contains(string(out), "migrate-paths") {
		t.Errorf("the refusal does not say why: %s", out)
	}
	if _, err := os.Stat(filepath.Join(h.base, "state", "tuios-servers", h.server, "tuios", "keep.txt")); err != nil {
		t.Fatalf("a refused start moved the state: %v", err)
	}
	if _, err := os.Stat(h.socket()); err == nil {
		t.Fatal("a refused start left a daemon on the host's socket")
	}
}

// migrate-paths --dry-run plans and changes nothing; --rollback puts it back.
func TestHostNamesMigratePathsCommand(t *testing.T) {
	h := newNamedHost(t)
	h.seedUpstreamState()
	out, err := h.cmd("migrate-paths", "--dry-run").Output()
	if err != nil || !strings.Contains(string(out), `"state": "would-move"`) {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if out, err := h.cmd("migrate-paths").Output(); err != nil || !strings.Contains(string(out), `"state": "moved"`) {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	if out, err := h.cmd("migrate-paths", "--rollback").Output(); err != nil || !strings.Contains(string(out), `"state": "moved"`) {
		t.Fatalf("rollback: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(h.base, "state", "tuios-servers", h.server, "tuios", "keep.txt")); err != nil {
		t.Fatalf("rollback did not restore the state: %v", err)
	}
}

// serve.MTMEnv is the library's table, so a host keeps no copy of the pairs.
func TestMTMEnvIsHostnamesTable(t *testing.T) {
	if len(serve.MTMEnv) < 50 || !slices.Equal(serve.MTMEnv, hostnames.MTMEnv) {
		t.Fatalf("serve.MTMEnv = %v", serve.MTMEnv)
	}
}
