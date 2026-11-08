package hostnames

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

var testNames = Names{
	SocketApp: "mtm", SocketTmp: "mtm-", SocketFile: "mtm.sock",
	StateApp: "mtm", StateServers: "mtm/servers", ServerApp: "", ConfigApp: "mtm",
	Env: []EnvName{{"MTM_SOCKET", "TUIOS_SOCKET"}, {"MTM_PANE_ID", "TUIOS_PANE_ID"}, {"MTM_SERVER", "TUIOS_SERVER"}},
}

func TestDefaultIsUpstream(t *testing.T) {
	if got := Get(); got.SocketFile != "tuios.sock" || got.StateServers != "tuios-servers" || got.ConfigApp != "tuios" || len(got.Env) != 0 {
		t.Fatalf("default table is not upstream's: %+v", got)
	}
	if got := Mirror([]string{"TUIOS_SOCKET=/s"}); !slices.Equal(got, []string{"TUIOS_SOCKET=/s"}) {
		t.Fatalf("upstream table mirrored: %v", got)
	}
}

func TestStateDir(t *testing.T) {
	cases := []struct {
		n            Names
		server, want string
	}{
		{Upstream, "", "/b/tuios"},
		{Upstream, "x", "/b/tuios-servers/x/tuios"},
		{testNames, "", "/b/mtm"},
		{testNames, "x", "/b/mtm/servers/x"},
	}
	for _, c := range cases {
		if got := c.n.StateDir("/b", c.server); got != filepath.FromSlash(c.want) {
			t.Errorf("StateDir(%q) under %s = %s, want %s", c.server, c.n.StateApp, got, c.want)
		}
	}
	if got := testNames.TmpSocketDir("/tmp", 501); got != "/tmp/mtm-501" {
		t.Errorf("TmpSocketDir = %s", got)
	}
}

func TestMirror(t *testing.T) {
	defer Set(&testNames)()
	env := []string{"A=1", "MTM_SOCKET=/stale", "TUIOS_SOCKET=/old", "TUIOS_PANE_ID=p1", "TUIOS_SOCKET=/new"}
	got := Mirror(env)
	want := []string{"A=1", "TUIOS_SOCKET=/old", "TUIOS_PANE_ID=p1", "TUIOS_SOCKET=/new", "MTM_SOCKET=/new", "MTM_PANE_ID=p1"}
	if !slices.Equal(got, want) {
		t.Fatalf("Mirror:\n got %v\nwant %v", got, want)
	}
	if Mirror(nil) != nil {
		t.Fatal("a nil env (inherit) must stay nil")
	}
}

// serial: it sets process environment variables.
func TestEnvReadsHostNameFirst(t *testing.T) {
	defer Set(&testNames)()
	t.Setenv("TUIOS_SOCKET", "/tuios")
	t.Setenv("MTM_SOCKET", "/mtm")
	t.Setenv("TUIOS_PANE_ID", "p")
	ApplyEnv()
	if v := os.Getenv("TUIOS_SOCKET"); v != "/mtm" {
		t.Fatalf("TUIOS_SOCKET = %q, want the MTM_ value", v)
	}
	if v := os.Getenv("TUIOS_PANE_ID"); v != "p" {
		t.Fatalf("an unset host name changed TUIOS_PANE_ID: %q", v)
	}
	t.Setenv("TUIOS_SERVER", "")
	t.Setenv("MTM_SERVER", "")
	if err := Setenv("TUIOS_SERVER", "srv"); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("MTM_SERVER") != "srv" || os.Getenv("TUIOS_SERVER") != "srv" {
		t.Fatal("Setenv did not set both names")
	}
	_ = Unsetenv("TUIOS_SERVER")
	if _, ok := os.LookupEnv("MTM_SERVER"); ok {
		t.Fatal("Unsetenv left MTM_SERVER")
	}
}
