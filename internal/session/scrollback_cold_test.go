package session

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ofam.dev/mtmd/internal/coldlog"
	"ofam.dev/mtmd/internal/vt"
)

// coldTestDaemon is a daemon whose panes keep ring rows of history and log
// the rest to disk.
func coldTestDaemon(t *testing.T, pol ColdPolicy, ring int) *Daemon {
	t.Helper()
	t.Cleanup(useResurrectionDir(t.TempDir()))
	return coldTestDaemonHere(t, pol, ring)
}

// coldTestDaemonHere is a daemon over the state folder already in use.
func coldTestDaemonHere(t *testing.T, pol ColdPolicy, ring int) *Daemon {
	t.Helper()
	d := NewDaemon(&DaemonConfig{History: ResolveHistoryPolicy(nil, 0, 0), Cold: pol, ScrollbackLines: ring})
	t.Cleanup(d.manager.Shutdown)
	return d
}

func coldTestPolicy() ColdPolicy {
	return ResolveColdPolicy(nil, 0, 0, 0, 3)
}

// coldTestSpawn starts a pane that prints nothing of its own, so the rows in
// its history are only the ones the test wrote.
func coldTestSpawn(t *testing.T, sess *Session, window string) *PTY {
	t.Helper()
	pty, err := sess.createPTY(40, 20, ptySpawn{windowID: window, command: []string{"sleep", "3600"}})
	if err != nil {
		t.Fatal(err)
	}
	return pty
}

func coldTestPane(t *testing.T, d *Daemon, session, window string) (*Session, *PTY) {
	t.Helper()
	sess, err := d.manager.CreateSession(session, &SessionConfig{}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	pty := coldTestSpawn(t, sess, window)
	sess.UpdateState(&SessionState{Name: session, Windows: []WindowState{{ID: window, PTYID: pty.ID}}})
	return sess, pty
}

// flood prints rows lines "L<first+i>" and waits for the log to take what
// the pane has let go of, so a fast flood does not outrun the writer's queue.
func flood(p *PTY, first, n int) {
	const chunk = 4000
	for i := 0; i < n; i += chunk {
		var b strings.Builder
		for j := i; j < min(i+chunk, n); j++ {
			fmt.Fprintf(&b, "L%07d\r\n", first+j)
		}
		writePane(p, b.String())
		p.cold.flush()
	}
}

func readCold(t *testing.T, dir string) *coldlog.Reader {
	t.Helper()
	r, err := coldlog.OpenReader(dir, 8)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func checkRows(t *testing.T, r *coldlog.Reader, from, to uint64, line func(id uint64) string) {
	t.Helper()
	for id := from; id < to; id++ {
		got, err := r.Text(id)
		if err != nil {
			t.Fatalf("row %d: %v", id, err)
		}
		if strings.TrimRight(got, " ") != line(id) {
			t.Fatalf("row %d = %q, want %q", id, got, line(id))
		}
	}
}

func rowName(id uint64) string { return fmt.Sprintf("L%07d", id) }

func TestColdFloodLeavesEvictedRowsReadable(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 10000)
	_, pty := coldTestPane(t, d, "flood", "win-1")

	const total = 200000
	flood(pty, 0, total)

	dir := mustPath(coldDir("flood", "win-1"))
	r := readCold(t, dir)
	// The ring keeps 10000 rows and the screen 20 (one of them the cursor's).
	ring := 10000
	want := uint64(total + 1 - 20 - ring)
	if n := r.Next(); n < want-2 || n > want+2 {
		t.Fatalf("log holds %d rows, want about %d", n, want)
	}
	if r.First() != 0 {
		t.Fatalf("first row %d", r.First())
	}
	checkRows(t, r, 0, r.Next(), rowName)
	if dropped := pty.cold.log.Dropped(); dropped != 0 {
		t.Fatalf("%d rows dropped", dropped)
	}
	// Private to the owner.
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("log dir: %v %v", fi, err)
	}
}

func TestColdOffWritesNothing(t *testing.T) {
	off := false
	d := coldTestDaemon(t, ResolveColdPolicy(&off, 0, 0, 0, 3), 1000)
	_, pty := coldTestPane(t, d, "off", "win-1")
	if pty.cold != nil {
		t.Fatal("a disabled policy installed a hook")
	}
	flood(pty, 0, 5000)
	if _, err := os.Stat(mustPath(coldDir("off", "win-1"))); !os.IsNotExist(err) {
		t.Fatalf("a cold dir exists: %v", err)
	}
}

func TestColdPaneThatNeverEvictsLeavesNoFiles(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 1000)
	_, pty := coldTestPane(t, d, "quiet", "win-1")
	writePane(pty, "hello\r\n")
	if _, err := os.Stat(mustPath(coldDir("quiet", "win-1"))); !os.IsNotExist(err) {
		t.Fatalf("a cold dir exists: %v", err)
	}
}

func TestColdWithoutEvictHookKeepsTodaysBehaviour(t *testing.T) {
	p := &PTY{terminal: noEvictor{}}
	p.installCold(coldTestPolicy(), "win-1", nil)
	if p.cold != nil {
		t.Fatal("a terminal that cannot report evictions got a log")
	}
}

func TestColdCloseWritesMarkerAndKeepsTheLog(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 1000)
	sess, pty := coldTestPane(t, d, "closer", "win-1")
	flood(pty, 0, 8000)
	dir := mustPath(coldDir("closer", "win-1"))

	if err := sess.ClosePTY(pty.ID); err != nil {
		t.Fatal(err)
	}
	m, err := coldlog.ReadMarker(dir)
	if err != nil {
		t.Fatalf("no closed.json: %v", err)
	}
	if m.Reason != coldWindowClosed || m.Session != "closer" || m.Window != "win-1" || m.Rows < 6000 || m.ClosedAt.IsZero() {
		t.Errorf("marker %+v", m)
	}
	checkRows(t, readCold(t, dir), 0, uint64(m.Rows), rowName)

	// Removing the session's resurrection state and history files keeps it.
	RemoveResurrectionState("closer")
	cleanOrphanHistory()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the log went with the session's state: %v", err)
	}
}

func TestColdSessionKillWritesMarker(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 1000)
	_, pty := coldTestPane(t, d, "doomed", "win-1")
	flood(pty, 0, 5000)
	dir := mustPath(coldDir("doomed", "win-1"))
	if err := d.manager.DeleteSession("doomed"); err != nil {
		t.Fatal(err)
	}
	m, err := coldlog.ReadMarker(dir)
	if err != nil || m.Reason != coldSessionKilled {
		t.Fatalf("marker %+v, %v", m, err)
	}
	if _, err := os.Stat(testHistoryDir("doomed")); err != nil {
		t.Errorf("the session's history folder with the log went: %v", err)
	}
}

func TestColdDaemonStopSealsWithoutMarker(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 1000)
	_, pty := coldTestPane(t, d, "stopper", "win-1")
	flood(pty, 0, 5000)
	dir := mustPath(coldDir("stopper", "win-1"))
	d.manager.Shutdown()
	if _, err := coldlog.ReadMarker(dir); !os.IsNotExist(err) {
		t.Fatalf("a stopping daemon marked the log closed: %v", err)
	}
	if coldlog.Locked(dir) {
		t.Fatal("the log is still locked")
	}
	r := readCold(t, dir)
	if r.Next() < 3000 {
		t.Fatalf("log holds %d rows", r.Next())
	}
	checkRows(t, r, 0, r.Next(), rowName)
}

func TestColdGraceZeroDeletesAtClose(t *testing.T) {
	d := coldTestDaemon(t, ResolveColdPolicy(nil, 0, 0, 0, 0), 1000)
	sess, pty := coldTestPane(t, d, "nograce", "win-1")
	flood(pty, 0, 5000)
	dir := mustPath(coldDir("nograce", "win-1"))
	if err := sess.ClosePTY(pty.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the log is still there: %v", err)
	}
}

func TestColdClearHistoryDeletesTheLog(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 1000)
	_, pty := coldTestPane(t, d, "clearer", "win-1")
	flood(pty, 0, 6000)
	dir := mustPath(coldDir("clearer", "win-1"))
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pty.ClearHistory(ctx, false); err != nil {
		t.Fatal(err)
	}
	// The sequence is applied by the pane's writer: wait for it.
	ack := make(chan struct{})
	pty.vtWriteChan <- vtChunk{ack: ack}
	<-ack
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("clear-history left the log: %v", err)
	}
	// History after the clear starts a new log: ids from zero, and none of
	// the rows the clear removed (the screen's own rows were not cleared and
	// are the first to scroll off).
	flood(pty, 100000, 6000)
	r := readCold(t, dir)
	first, err := r.Text(0)
	if err != nil || strings.TrimRight(first, " ") < rowName(5900) {
		t.Fatalf("row 0 of the new log = %q, %v", first, err)
	}
	last, err := r.Text(r.Next() - 1)
	if err != nil || !strings.HasPrefix(last, "L01") {
		t.Fatalf("last row of the new log = %q, %v", last, err)
	}
}

func TestColdSessionRenameKeepsTheLog(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 1000)
	_, pty := coldTestPane(t, d, "before", "win-1")
	flood(pty, 0, 5000)
	n := readCold(t, mustPath(coldDir("before", "win-1"))).Next()
	if _, err := d.manager.RenameSession("before", "after"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mustPath(coldDir("before", "win-1"))); !os.IsNotExist(err) {
		t.Fatalf("the old dir is still there: %v", err)
	}
	flood(pty, 5000, 5000)
	dir := mustPath(coldDir("after", "win-1"))
	r := readCold(t, dir)
	if r.Next() <= n {
		t.Fatalf("rows after the rename did not reach the log: %d then %d", n, r.Next())
	}
	checkRows(t, r, 0, r.Next(), rowName)
	if _, err := os.Stat(mustPath(coldDir("before", "win-1"))); !os.IsNotExist(err) {
		t.Fatalf("the old dir came back: %v", err)
	}
}

// A handoff flushes each pane's log and the next image opens the same
// directory with the old process's lock gone: the files are copied after the
// flush, as the exec leaves them, and the successor continues the ids.
func TestColdHandoffKeepsTheRows(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 1000)
	_, pty := coldTestPane(t, d, "upgrade", "win-1")
	// Rows that sit in the open block when the handoff starts.
	var b strings.Builder
	for i := range 3500 {
		fmt.Fprintf(&b, "%s\r\n", rowName(uint64(i)))
	}
	writePane(pty, b.String())
	pty.cold.flush() // what prepare does after the quiesce

	dir := mustPath(coldDir("upgrade", "win-1"))
	snap := strings.TrimSuffix(dir, coldExt) + "-snap" + coldExt
	copyTree(t, dir, snap)
	before := readCold(t, snap).Next()
	if before == 0 {
		t.Fatal("nothing was flushed")
	}

	// The old image goes; the new one finds the files as they were.
	d.manager.Shutdown()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(snap, dir); err != nil {
		t.Fatal(err)
	}

	next := coldTestDaemonHere(t, coldTestPolicy(), 1000)
	_, pty2 := coldTestPane(t, next, "upgrade", "win-1")
	flood(pty2, int(before)+1000, 4000)
	r := readCold(t, dir)
	if r.Next() <= before {
		t.Fatalf("the successor's rows did not follow: %d then %d", before, r.Next())
	}
	checkRows(t, r, 0, before, rowName)
	if got, _ := r.Text(before); strings.TrimRight(got, " ") != rowName(uint64(before)+1000) {
		t.Fatalf("first row of the successor = %q", got)
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(to, 0o700); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		src, err := os.Open(filepath.Join(from, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		dst, err := os.OpenFile(filepath.Join(to, e.Name()), os.O_WRONLY|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(dst, src); err != nil {
			t.Fatal(err)
		}
		src.Close()
		dst.Close()
	}
}

func TestColdTotalCapDropsClosedLogsFirst(t *testing.T) {
	pol := coldTestPolicy()
	d := coldTestDaemon(t, pol, 1000)
	sess, old := coldTestPane(t, d, "capped", "win-old")
	flood(old, 0, 8000)
	if err := sess.ClosePTY(old.ID); err != nil {
		t.Fatal(err)
	}
	oldDir := mustPath(coldDir("capped", "win-old"))
	live := coldTestSpawn(t, sess, "win-live")
	flood(live, 0, 8000)
	liveDir := mustPath(coldDir("capped", "win-live"))

	pol.TotalBytes = 1 // below everything
	d.manager.SetColdPolicy(pol)
	d.enforceColdTotal()
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("the closed log survived the cap: %v", err)
	}
	if _, err := os.Stat(liveDir); err != nil {
		t.Errorf("the live log went: %v", err)
	}
}

func TestColdDropWhenOffAtStart(t *testing.T) {
	dir := t.TempDir()
	defer useResurrectionDir(dir)()
	cold := mustPath(coldDir("s", "w"))
	if err := os.MkdirAll(cold, 0o700); err != nil {
		t.Fatal(err)
	}
	hist := testHistoryPath("s", "w")
	if err := os.WriteFile(hist, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	off := false
	d := NewDaemon(&DaemonConfig{Cold: ResolveColdPolicy(&off, 0, 0, 0, 3)})
	defer d.manager.Shutdown()
	d.dropColdWhenOff()
	if _, err := os.Stat(cold); !os.IsNotExist(err) {
		t.Errorf("the cold dir survived: %v", err)
	}
	if _, err := os.Stat(hist); err != nil {
		t.Errorf("the .hist.gz went with it: %v", err)
	}
}

func TestResolveColdPolicy(t *testing.T) {
	p := ResolveColdPolicy(nil, 2000000, 128, 2048, 3)
	want := ColdPolicy{Enabled: true, Rows: 2000000, Bytes: 128 << 20, TotalBytes: 2048 << 20, GraceDays: 3}
	if p != want {
		t.Errorf("got %+v, want %+v", p, want)
	}
	if z := ResolveColdPolicy(nil, 0, 0, 0, 999); z.Rows != 0 || z.Bytes != 128<<20 || z.TotalBytes != 2048<<20 || z.GraceDays != 365 {
		t.Errorf("zero values: %+v", z)
	}
	if (ColdPolicy{}).Enabled {
		t.Error("the zero policy is on")
	}
}

var _ vt.Terminal = noEvictor{}
