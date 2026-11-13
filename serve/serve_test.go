package serve_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"ofam.dev/mtmd/internal/testutil"
	"ofam.dev/mtmd/serve"
)

// hostEnv marks a run of the test binary as the host program: it routes the
// word "mtmd" to serve.Main, as mtm will, so that the daemon the test starts
// is the test binary running itself again through SelfCommand and no tuios
// is needed on PATH.
const hostEnv = "MTMD_TEST_HOST"

func TestMain(m *testing.M) {
	if os.Getenv(hostEnv) == "1" && len(os.Args) > 1 && os.Args[1] == "mtmd" {
		opt := serve.Options{SelfCommand: []string{"", "mtmd"}, Version: "v0-test"}
		if os.Getenv(hostNamesEnv) == "1" {
			opt.Names = &testHostNames
		}
		os.Exit(serve.Main(os.Args[2:], opt))
	}
	os.Exit(testutil.RunIsolated(m))
}

type host struct {
	t       *testing.T
	exe     string
	server  string
	env     []string
	runtime string
	base    string
	names   *serve.Names // the host's table (hostNamesEnv), nil for upstream's
}

func newHost(t *testing.T) *host {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets and Setsid")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A short base: a unix socket path is limited to about 100 bytes.
	base, err := os.MkdirTemp("/tmp", "msv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	h := &host{t: t, exe: exe, base: base, server: "msv-" + strconv.Itoa(os.Getpid()), runtime: filepath.Join(base, "run")}
	for _, d := range []string{"home", "run", "state", "config", "data", "bin"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "TUIOS_"), strings.HasPrefix(k, "XDG_"), k == "HOME", k == "TMUX", k == "NO_COLOR", k == "TERM", k == "PATH", k == "SHELL":
			continue
		}
		env = append(env, kv)
	}
	h.env = append(env,
		hostEnv+"=1",
		"TUIOS_TEST_HANDOFF=1", // the live-upgrade primitive: the test-handoff verb
		"TERM=xterm-256color",
		"SHELL=/bin/sh",
		"HOME="+filepath.Join(base, "home"),
		"XDG_RUNTIME_DIR="+h.runtime,
		"XDG_STATE_HOME="+filepath.Join(base, "state"),
		"XDG_CONFIG_HOME="+filepath.Join(base, "config"),
		"XDG_DATA_HOME="+filepath.Join(base, "data"),
		"PATH=/usr/bin:/bin", // no tuios anywhere on PATH
	)
	return h
}

func (h *host) cmd(args ...string) *exec.Cmd {
	c := exec.Command(h.exe, append([]string{"mtmd", "--server", h.server}, args...)...)
	c.Env = h.env
	return c
}

func (h *host) socket() string {
	if h.names != nil {
		return filepath.Join(h.runtime, h.names.SocketApp, "servers", h.server, h.names.SocketFile)
	}
	return filepath.Join(h.runtime, "tuios", "servers", h.server, "tuios.sock")
}

// call sends one request on a fresh connection and returns the result object.
func (h *host) call(verb string, params map[string]any) (map[string]any, error) {
	conn, err := net.DialTimeout("unix", h.socket(), 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	req, _ := json.Marshal(map[string]any{"id": 1, "verb": verb, "params": params})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && (err != io.EOF || len(line) == 0) {
		return nil, fmt.Errorf("read %s: %w", verb, err)
	}
	var resp struct {
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", verb, err, line)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s: error %v", verb, resp.Error)
	}
	return resp.Result, nil
}

func (h *host) must(verb string, params map[string]any) map[string]any {
	h.t.Helper()
	res, err := h.call(verb, params)
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

// start brings the daemon up through the facade's start-server and returns
// its pid. The daemon is stopped by that pid when the test ends.
func (h *host) start() int {
	h.t.Helper()
	out, err := h.cmd("start-server").CombinedOutput()
	if err != nil {
		h.t.Fatalf("start-server: %v\n%s", err, out)
	}
	hello := h.must("hello", map[string]any{})
	pid := int(hello["pid"].(float64))
	if hello["type"] != "hello" || pid <= 0 {
		h.t.Fatalf("hello: %v", hello)
	}
	h.t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		for i := 0; i < 100 && syscall.Kill(pid, 0) == nil; i++ {
			time.Sleep(50 * time.Millisecond)
		}
		if syscall.Kill(pid, 0) == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return pid
}

func argvOf(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps %d: %v", pid, err)
	}
	return strings.TrimSpace(string(out))
}

func (h *host) capture(sess, win string) string {
	res, err := h.call("capture-pane", map[string]any{"session": sess, "window": win})
	if err != nil {
		return ""
	}
	s, _ := res["content"].(string)
	return s
}

func (h *host) waitPane(sess, win, want string, d time.Duration) string {
	h.t.Helper()
	var last string
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if last = h.capture(sess, win); strings.Contains(last, want) {
			return last
		}
	}
	h.t.Fatalf("pane %s/%s never showed %q; last screen:\n%s", sess, win, want, last)
	return ""
}

func (h *host) newShell(sess, name string) string {
	h.must("new-session", map[string]any{"name": sess})
	res := h.must("new-window", map[string]any{"session": sess, "name": name, "focus": false, "command": []string{"sh"}})
	return res["window_id"].(string)
}

func TestDaemonLifecycle(t *testing.T) {
	h := newHost(t)
	pid := h.start()
	if argv := argvOf(t, pid); !strings.HasPrefix(argv, h.exe+" mtmd --server "+h.server+" daemon") {
		t.Fatalf("the daemon did not run through SelfCommand: argv %q", argv)
	}
	if v := h.must("hello", map[string]any{})["daemon_version"]; v != "v0-test" {
		t.Fatalf("daemon_version = %v, want v0-test", v)
	}
	win := h.newShell("s1", "w1")
	time.Sleep(500 * time.Millisecond)
	h.must("send-text", map[string]any{"session": "s1", "window": win, "text": "echo msv-$((6*7))\n"})
	h.waitPane("s1", win, "msv-42", 10*time.Second)

	// Disconnect, reconnect: every connection above is gone, the daemon and
	// the shell stay, and the screen is still there.
	if again := h.must("hello", map[string]any{}); int(again["pid"].(float64)) != pid {
		t.Fatalf("a different daemon answered: %v", again)
	}
	h.waitPane("s1", win, "msv-42", time.Second)

	// A second start-server finds the daemon and does nothing.
	if out, err := h.cmd("start-server").CombinedOutput(); err != nil {
		t.Fatalf("second start-server: %v\n%s", err, out)
	}
	if again := h.must("hello", map[string]any{}); int(again["pid"].(float64)) != pid {
		t.Fatalf("second start-server replaced the daemon: %v", again)
	}
}

func TestUsageErrors(t *testing.T) {
	h := newHost(t)
	for _, args := range [][]string{{}, {"nope"}, {"agent-proto"}, {"agent-proto", "--protocol", "acp"}, {"agent-proto", "--protocol", "bogus", "--", "x"}} {
		c := exec.Command(h.exe, append([]string{"mtmd"}, args...)...)
		c.Env = h.env
		out, err := c.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
			t.Errorf("%v: err = %v, want exit 2\n%s", args, err, out)
		}
	}
	c := exec.Command(h.exe, "mtmd", "daemon", "handoff-accepts")
	c.Env = h.env
	out, err := c.Output()
	if err != nil || !strings.Contains(string(out), `"version":"v0-test"`) {
		t.Fatalf("handoff-accepts: %v %s", err, out)
	}
}

// fakeACP is a minimal ACP agent: it answers initialize, session/new and
// session/prompt (streaming "pong: <n>" for the nth prompt).
const fakeACP = `#!/bin/sh
n=0
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{}}}\n' "$id" ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"s-1"}}\n' "$id" ;;
    *'"method":"session/prompt"'*)
      n=$((n+1))
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"pong-%s"}}}}\n' "$n"
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$id" ;;
  esac
done
`

// startProtocolPane opens an ACP protocol pane; the daemon re-executes the
// host as "<exe> mtmd agent-proto --protocol acp -- fakeagent".
func (h *host) startProtocolPane(sess string) (string, string) {
	h.t.Helper()
	agent := filepath.Join(h.base, "bin", "fakeacp")
	if err := os.WriteFile(agent, []byte(fakeACP), 0o755); err != nil {
		h.t.Fatal(err)
	}
	h.must("new-session", map[string]any{"name": sess})
	res := h.must("start-agent", map[string]any{"session": sess, "agent": agent, "protocol": "acp", "focus": false, "ready_timeout": 20})
	win, _ := res["window_id"].(string)
	if win == "" {
		h.t.Fatalf("start-agent: %v", res)
	}
	return win, agent
}

func TestProtocolPane(t *testing.T) {
	h := newHost(t)
	h.start()
	win, agent := h.startProtocolPane("ap")
	screen := h.waitPane("ap", win, "acp: "+agent, 15*time.Second)
	_ = screen
	h.must("send-text", map[string]any{"session": "ap", "window": win, "text": "ping\n"})
	h.waitPane("ap", win, "pong-1", 15*time.Second)
	// The pane program is the host, not a tuios.
	out, err := exec.Command("ps", "-axo", "command=").Output()
	if err != nil {
		t.Fatal(err)
	}
	want := h.exe + " mtmd agent-proto --protocol acp"
	if !strings.Contains(string(out), want) {
		t.Fatalf("no process runs %q", want)
	}
}

func TestLiveUpgrade(t *testing.T) {
	h := newHost(t)
	pid := h.start()
	win := h.newShell("up", "w1")
	time.Sleep(500 * time.Millisecond)
	h.must("send-text", map[string]any{"session": "up", "window": win, "text": "echo before-$((6*7))\n"})
	h.waitPane("up", win, "before-42", 10*time.Second)
	// A protocol pane rides through the upgrade too.
	pwin, _ := h.startProtocolPane("upap")
	h.waitPane("upap", pwin, "acp: ", 15*time.Second)

	before := h.must("hello", map[string]any{})
	// The new image is a second copy of the host binary.
	second := filepath.Join(h.base, "bin", "host2")
	data, err := os.ReadFile(h.exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := h.call("test-handoff", map[string]any{"binary": second}); err != nil {
		t.Fatalf("test-handoff: %v", err)
	}
	var after map[string]any
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		res, err := h.call("hello", map[string]any{})
		if err == nil && res["instance"] != before["instance"] {
			after = res
			break
		}
	}
	if after == nil {
		t.Fatal("the daemon never answered from a new image")
	}
	if int(after["pid"].(float64)) != pid {
		t.Fatalf("pid changed across the exec: %v -> %v", pid, after["pid"])
	}
	argv := argvOf(t, pid)
	if !strings.HasPrefix(argv, second+" mtmd --server "+h.server+" daemon --adopt-fd=") {
		t.Fatalf("the new image did not run through the prefix: argv %q", argv)
	}
	// The shell survived and still takes input; so does the protocol pane.
	h.waitPane("up", win, "before-42", 5*time.Second)
	h.must("send-text", map[string]any{"session": "up", "window": win, "text": "echo after-$((6*7+1))\n"})
	h.waitPane("up", win, "after-43", 10*time.Second)
	h.waitPane("upap", pwin, "acp: ", 5*time.Second)
	h.must("send-text", map[string]any{"session": "upap", "window": pwin, "text": "ping\n"})
	h.waitPane("upap", pwin, "pong-1", 15*time.Second)
}

// A resize changes the pane's PTY, and get-window and list-windows must report
// that size, not the layout's nominal one (mtmd patch, FORK.md).
func TestResizeIsReported(t *testing.T) {
	h := newHost(t)
	h.start()
	win := h.newShell("sz", "w1")
	time.Sleep(300 * time.Millisecond)
	h.must("resize", map[string]any{"session": "sz", "window": win, "width": 100, "height": 30})

	size := func(m map[string]any) [2]int {
		w, _ := m["width"].(float64)
		hh, _ := m["height"].(float64)
		return [2]int{int(w), int(hh)}
	}
	want := [2]int{100, 30}
	if got := size(h.must("get-window", map[string]any{"session": "sz", "window": win})); got != want {
		t.Fatalf("get-window size = %v, want %v", got, want)
	}
	found := false
	for _, w := range h.must("list-windows", map[string]any{"session": "sz"})["windows"].([]any) {
		m := w.(map[string]any)
		if m["window_id"] == win {
			found = true
			if got := size(m); got != want {
				t.Fatalf("list-windows size = %v, want %v", got, want)
			}
		}
	}
	if !found {
		t.Fatal("window missing from list-windows")
	}
}
