package serve_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ofam.dev/mtmd/internal/federation"
	"ofam.dev/mtmd/internal/session"
)

// TestStdioProxyLinksAHubToThisDaemon is the far side of a link run through
// the facade: a hub whose dial runs "<host> mtmd stdio-proxy" lists the
// sessions of the daemon on that side.
func TestStdioProxyLinksAHubToThisDaemon(t *testing.T) {
	h := newHost(t)
	h.start()
	h.newShell("far-s", "w1")

	// The far program: this test binary as the host, in the far side's own
	// environment and nothing else.
	far := filepath.Join(h.base, "bin", "far")
	var env []string
	for _, kv := range h.env {
		k, _, _ := strings.Cut(kv, "=")
		if k == hostEnv || k == "HOME" || k == "PATH" || strings.HasPrefix(k, "XDG_") {
			env = append(env, "'"+kv+"'")
		}
	}
	script := "#!/bin/sh\nexec env -i " + strings.Join(env, " ") + " " + h.exe + " mtmd --server " + h.server + " stdio-proxy\n"
	if err := os.WriteFile(far, []byte(script), 0o700); err != nil { //nolint:gosec // the far program this test runs
		t.Fatal(err)
	}

	table, problems := federation.NewTable([]federation.Host{{Name: "far", Addr: "unused"}})
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	m := federation.New(table, federation.Options{
		Dial:            federation.CommandDialer(far),
		ClientName:      "serve-test",
		ClientVersion:   "0",
		VerbProtocol:    session.VerbProtocolVersion,
		MinVerbProtocol: session.MinVerbProtocolVersion,
		InitialBackoff:  20 * time.Millisecond,
		MaxBackoff:      100 * time.Millisecond,
	})
	m.Start(context.Background())
	t.Cleanup(m.Stop)

	var last string
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res, err := m.Call(ctx, "far", "list-sessions", nil)
		cancel()
		if err == nil && strings.Contains(string(res), "far-s") {
			return
		}
		r, _ := m.Report("far")
		last = string(res) + " / " + string(r.Status) + ": " + r.Reason + " " + r.Detail
		if err != nil {
			last += " / " + err.Error()
		}
	}
	t.Fatalf("ASSERTION: the hub never listed the far session through stdio-proxy: %s", last)
}
