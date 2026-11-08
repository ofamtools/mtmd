package serve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ofam.dev/mtmd/internal/hostnames"
)

var mtmNames = hostnames.Names{
	SocketApp: "mtm", SocketTmp: "mtm-", SocketFile: "mtm.sock",
	StateApp: "mtm", StateServers: "mtm/servers", ServerApp: "", ConfigApp: "mtm",
}

type migrateFixture struct {
	t     *testing.T
	base  string
	env   migrateEnv
	alive map[string]bool
}

func newMigrateFixture(t *testing.T) *migrateFixture {
	t.Helper()
	base := t.TempDir()
	f := &migrateFixture{t: t, base: base, alive: map[string]bool{}}
	state, config := filepath.Join(base, "state"), filepath.Join(base, "config")
	f.env = migrateEnv{
		stateHome:  state,
		configHome: config,
		findConfig: func(rel string) (string, error) {
			p := filepath.Join(config, filepath.FromSlash(rel))
			if _, err := os.Stat(p); err != nil {
				return "", err
			}
			return p, nil
		},
		socket: func(n hostnames.Names) string {
			return filepath.Join(base, "run", n.SocketApp, "servers", "srv", n.SocketFile)
		},
		live: func(s string) bool { return f.alive[s] },
		now:  func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) },
	}
	return f
}

func (f *migrateFixture) write(rel, body string) {
	f.t.Helper()
	p := filepath.Join(f.base, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *migrateFixture) read(rel string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(f.base, filepath.FromSlash(rel)))
	return string(data), err == nil
}

// seed is a named server's state under upstream's names: a saved session, a
// scrollback cold log, the daemon log, and the configuration file.
func (f *migrateFixture) seed() {
	f.write("state/tuios-servers/srv/tuios/sessions/work.json", `{"name":"work"}`)
	f.write("state/tuios-servers/srv/tuios/sessions/scrollback/work/w1.hist", "cold rows")
	f.write("state/tuios-servers/srv/tuios/daemon.log", "log")
	f.write("config/tuios/config.toml", "[appearance]\n")
}

func (f *migrateFixture) run(rollback, dry bool) (Migration, error) {
	return migrate(hostnames.Upstream, mtmNames, "srv", rollback, dry, f.env)
}

func TestMigrateFresh(t *testing.T) {
	f := newMigrateFixture(t)
	m, err := f.run(false, false)
	if err != nil || m.State != "nothing" || m.Config != "nothing" {
		t.Fatalf("fresh: %+v %v", m, err)
	}
	if _, err := os.Stat(filepath.Join(f.base, "state", "mtm")); err == nil {
		t.Fatal("a fresh migration made the new folder")
	}
}

func TestMigrateMovesStateAndCopiesConfig(t *testing.T) {
	f := newMigrateFixture(t)
	f.seed()
	m, err := f.run(false, false)
	if err != nil || m.State != "moved" || m.Config != "copied" {
		t.Fatalf("migrate: %+v %v", m, err)
	}
	for rel, want := range map[string]string{
		"state/mtm/servers/srv/sessions/work.json":               `{"name":"work"}`,
		"state/mtm/servers/srv/sessions/scrollback/work/w1.hist": "cold rows",
		"state/mtm/servers/srv/daemon.log":                       "log",
		"config/mtm/config.toml":                                 "[appearance]\n",
		"config/tuios/config.toml":                               "[appearance]\n", // the old one stays
	} {
		if got, ok := f.read(rel); !ok || got != want {
			t.Errorf("%s = %q (%v), want %q", rel, got, ok, want)
		}
	}
	if _, err := os.Stat(filepath.Join(f.base, "state/tuios-servers/srv/tuios")); err == nil {
		t.Error("the old state folder is still there")
	}
	marker, ok := f.read("state/tuios-servers/srv/MOVED-TO")
	if !ok || !strings.Contains(marker, filepath.Join(f.base, "state/mtm/servers/srv")) {
		t.Errorf("MOVED-TO = %q (%v)", marker, ok)
	}
	if st, err := os.Stat(filepath.Join(f.base, "config/mtm/config.toml")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("copied config mode: %v %v", st, err)
	}

	// Run again: nothing to do, nothing changes.
	again, err := f.run(false, false)
	if err != nil || again.State != "nothing" || again.Config != "nothing" {
		t.Fatalf("re-run: %+v %v", again, err)
	}
	if got, _ := f.read("state/mtm/servers/srv/sessions/work.json"); got != `{"name":"work"}` {
		t.Fatal("re-run changed the state")
	}
}

func TestMigrateDryRunChangesNothing(t *testing.T) {
	f := newMigrateFixture(t)
	f.seed()
	m, err := f.run(false, true)
	if err != nil || m.State != "would-move" || m.Config != "would-copy" {
		t.Fatalf("dry run: %+v %v", m, err)
	}
	if _, ok := f.read("state/tuios-servers/srv/tuios/sessions/work.json"); !ok {
		t.Fatal("dry run moved the state")
	}
	if _, ok := f.read("config/mtm/config.toml"); ok {
		t.Fatal("dry run copied the config")
	}
}

func TestMigrateRefusesLiveWriter(t *testing.T) {
	f := newMigrateFixture(t)
	f.seed()
	f.alive[f.env.socket(hostnames.Upstream)] = true
	m, err := f.run(false, false)
	if !errors.Is(err, errLiveWriter) || m.State != "refused" || m.LiveSocket == "" {
		t.Fatalf("live writer: %+v %v", m, err)
	}
	if _, ok := f.read("state/tuios-servers/srv/tuios/sessions/work.json"); !ok {
		t.Fatal("a refused migration moved the state")
	}
	if _, ok := f.read("config/mtm/config.toml"); ok {
		t.Fatal("a refused migration copied the config")
	}
}

func TestMigrateRollback(t *testing.T) {
	f := newMigrateFixture(t)
	f.seed()
	if _, err := f.run(false, false); err != nil {
		t.Fatal(err)
	}
	// A daemon on the new socket is the live writer of a rollback.
	f.alive[f.env.socket(mtmNames)] = true
	if _, err := f.run(true, false); !errors.Is(err, errLiveWriter) {
		t.Fatalf("rollback with a live daemon: %v", err)
	}
	delete(f.alive, f.env.socket(mtmNames))
	m, err := f.run(true, false)
	if err != nil || m.State != "moved" || m.Direction != "rollback" {
		t.Fatalf("rollback: %+v %v", m, err)
	}
	if got, ok := f.read("state/tuios-servers/srv/tuios/sessions/scrollback/work/w1.hist"); !ok || got != "cold rows" {
		t.Fatal("rollback did not bring the cold log back")
	}
	for _, rel := range []string{"state/tuios-servers/srv/MOVED-TO", "state/tuios-servers/srv/tuios/MOVED-FROM", "state/mtm/servers/srv"} {
		if _, err := os.Stat(filepath.Join(f.base, rel)); err == nil {
			t.Errorf("%s left after rollback", rel)
		}
	}
	// Forward again after a rollback works.
	if m, err := f.run(false, false); err != nil || m.State != "moved" {
		t.Fatalf("forward after rollback: %+v %v", m, err)
	}
}

func TestMigrateDefaultServerIsLeft(t *testing.T) {
	f := newMigrateFixture(t)
	f.write("state/tuios/sessions/a.json", "{}")
	m, err := migrate(hostnames.Upstream, mtmNames, "", false, false, f.env)
	if err != nil || m.State != "nothing" {
		t.Fatalf("default server: %+v %v", m, err)
	}
	if _, ok := f.read("state/tuios/sessions/a.json"); !ok {
		t.Fatal("the default server's state moved")
	}
}
