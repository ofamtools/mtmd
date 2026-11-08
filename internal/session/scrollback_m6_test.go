package session

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"ofam.dev/mtmd/internal/coldlog"
	"ofam.dev/mtmd/internal/vt"
)

// A restored pane runs a shell whose prompt may land in front of a row.
var logRow = regexp.MustCompile(`(?:^|\s)L(\d{7})$`)

// coldRows are the numbers of the L rows of the log in dir, in order, and how
// many rows the log holds in all.
func coldRows(t *testing.T, dir string) (nums []int, total uint64) {
	t.Helper()
	r := readCold(t, dir)
	for id := r.First(); id < r.Next(); id++ {
		txt, err := r.Text(id)
		if err != nil {
			t.Fatalf("row %d: %v", id, err)
		}
		if m := logRow.FindStringSubmatch(strings.TrimRight(txt, " ")); m != nil {
			n, _ := strconv.Atoi(m[1])
			nums = append(nums, n)
		}
	}
	return nums, r.Next()
}

func requireSequence(t *testing.T, nums []int, from, to int) {
	t.Helper()
	if len(nums) < to-from {
		t.Fatalf("the log holds %d of the %d rows L%d..L%d (first %v)", len(nums), to-from, from, to-1, nums[:min(5, len(nums))])
	}
	for i, n := range nums {
		if n != from+i {
			t.Fatalf("log row %d is L%d, want L%d (a row is missing or twice)", i, n, from+i)
		}
	}
}

// A pane with far more history than its ring comes back from a daemon restart
// with all of it: the log continues under the same window id, the restored
// ring starts where the log ends, and the rows written after the restart
// follow without a gap or a repeat.
func TestColdRestartKeepsTheFullHistory(t *testing.T) {
	d1 := coldTestDaemon(t, coldTestPolicy(), 10000)
	sess, pty := coldTestPane(t, d1, "restart", "win-1")
	sess.UpdateState(&SessionState{Name: "restart", Width: 80, Height: 24,
		Windows: []WindowState{{ID: "win-1", PTYID: pty.ID, Width: 42, Height: 22}}})
	const total = 300000
	flood(pty, 0, total)
	dir := mustPath(coldDir("restart", "win-1"))

	d1.manager.Shutdown() // saves the session and its history, spills, seals

	// Everything older than the saved history file is in the log.
	nums, next := coldRows(t, dir)
	h := loadHistory("restart")["win-1"]
	if h == nil || !h.HasColdTail {
		t.Fatalf("no history file with a cold tail: %+v", h)
	}
	if h.ColdTail != next {
		t.Fatalf("the history file starts at row %d, the log ends at %d", h.ColdTail, next)
	}
	requireSequence(t, nums, 0, len(nums))
	if len(nums) < total-10000 {
		t.Fatalf("log holds %d rows", len(nums))
	}

	d2 := coldTestDaemonHere(t, coldTestPolicy(), 10000)
	d2.restoreAllSessions()
	sess2 := d2.manager.GetSession("restart")
	if sess2 == nil {
		t.Fatal("the session did not come back")
	}
	pty2 := sess2.GetPTY(sess2.GetState().Windows[0].PTYID)
	if pty2 == nil || pty2.cold == nil {
		t.Fatal("no restored pane with a log")
	}
	if got := pty2.cold.ringStart(); got != next {
		t.Fatalf("the restored ring starts at row %d, the log ends at %d", got, next)
	}
	// The ring's oldest row is the log's next one.
	pty2.terminalMu.RLock()
	first := strings.TrimRight(pty2.terminal.ScrollbackLine(0).String(), " ")
	pty2.terminalMu.RUnlock()
	if first != rowName(next) {
		t.Fatalf("restored ring starts with %q, want %q", first, rowName(next))
	}

	// New output pushes the restored rows out into the same log, in order.
	flood(pty2, total, 25000)
	nums, _ = coldRows(t, dir)
	requireSequence(t, nums, 0, len(nums))
	if len(nums) < total {
		t.Fatalf("after the restart the log holds %d of the %d old rows", len(nums), total)
	}
	if got := pty2.cold.log.Dropped(); got != 0 {
		t.Fatalf("%d rows dropped", got)
	}
}

// A history file older than the log (a crash after more rows were logged) and
// a log shorter than the file (rows lost) both leave the ids aligned: the
// first is skipped, the second recorded as a gap.
func TestColdRestartReconcilesALogThatDisagreesWithTheFile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tail   uint64 // the file's first row id
		logged int    // rows the log holds
	}{
		{"log ahead of the file", 1000, 1600},
		{"log behind the file", 1000, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := coldTestDaemon(t, coldTestPolicy(), 1000)
			sess, err := d.manager.CreateSession("recon", &SessionConfig{}, 80, 24)
			if err != nil {
				t.Fatal(err)
			}
			dir := mustPath(coldDir("recon", "win-1"))
			l, err := coldlog.OpenLog(dir, coldlog.Options{})
			if err != nil {
				t.Fatal(err)
			}
			feeder := coldTestRows(t, tc.logged)
			for _, r := range feeder {
				l.Append(r)
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}

			pty := coldTestSpawn(t, sess, "win-1")
			pty.cold.setStart(tc.tail)
			// Rows L(tail).. are in the restored ring; evict 2000 of them.
			flood(pty, int(tc.tail), 3500)
			r := readCold(t, dir)
			st := r.Stats()
			if tc.logged < int(tc.tail) {
				if len(st.Gaps) != 1 || st.Gaps[0].First != uint64(tc.logged) || st.Gaps[0].N != tc.tail-uint64(tc.logged) {
					t.Fatalf("gaps %+v, want one of %d rows from %d", st.Gaps, tc.tail-uint64(tc.logged), tc.logged)
				}
			}
			// Row ids are the ring's own: the id of a row is its position in all
			// the rows the pane let go of, from the file's first row on.
			last, err := r.Text(r.Next() - 1)
			if err != nil {
				t.Fatal(err)
			}
			evicted := uint64(pty.cold.evicted.Load())
			if want := tc.tail + evicted; r.Next() != want {
				t.Fatalf("log ends at %d, want %d (last row %q)", r.Next(), want, last)
			}
		})
	}
}

// coldTestRows is n encoded rows "L0000000".., as the ring would hand them out.
func coldTestRows(t *testing.T, n int) []vt.EncodedRow {
	t.Helper()
	term := vt.NewWithScrollback(40, 5, n+10)
	var b strings.Builder
	for i := range n + 5 {
		b.WriteString(rowName(uint64(i)) + "\r\n")
	}
	_, _ = term.Write([]byte(b.String()))
	src := term.(scrollbackRowSource)
	var out []vt.EncodedRow
	for i := range n {
		row, ok := src.ScrollbackEncodedRow(i)
		if !ok {
			t.Fatalf("no row %d", i)
		}
		out = append(out, vt.EncodedRow{Data: append([]byte(nil), row.Data...), Width: row.Width, Flags: row.Flags})
	}
	return out
}

// A handoff carries only the newest 1000 rows of each ring. Rows older than
// that and newer than the log's end are written to the log first, so the log
// and the carried ring meet; an abandoned handoff does not write them twice.
func TestColdHandoffLosesNoRows(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 20000)
	_, pty := coldTestPane(t, d, "handoff", "win-1")
	// 12,000 rows, none evicted: the log does not exist yet.
	writePane(pty, manyRows(0, 12000))
	dir := mustPath(coldDir("handoff", "win-1"))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("a log exists before any eviction: %v", err)
	}

	pty.spillColdForHandoff()
	pty.cold.flush()
	nums, next := coldRows(t, dir)

	pty.terminalMu.RLock()
	ringLen := pty.terminal.ScrollbackLen()
	carriedFrom := ringLen - handoffHistoryRows
	if next != uint64(carriedFrom) {
		pty.terminalMu.RUnlock()
		t.Fatalf("the log ends at row %d, the carried rows start at ring row %d", next, carriedFrom)
	}
	firstCarried := strings.TrimRight(pty.terminal.ScrollbackLine(carriedFrom).String(), " ")
	pty.terminalMu.RUnlock()
	if firstCarried != rowName(next) {
		t.Fatalf("the carried rows start with %q, the log ends before %q", firstCarried, rowName(next))
	}
	requireSequence(t, nums, 0, int(next))

	// The handoff is abandoned: the pane carries on, and the rows it evicts
	// later are the ones already in the log.
	writePane(pty, manyRows(12000, 30000))
	pty.cold.flush()
	nums, _ = coldRows(t, dir)
	requireSequence(t, nums, 0, len(nums))
	if len(nums) < 30000+12000-20000-100 {
		t.Fatalf("log holds %d rows", len(nums))
	}
}

func manyRows(from, n int) string {
	var b strings.Builder
	for i := from; i < from+n; i++ {
		b.WriteString(rowName(uint64(i)) + "\r\n")
	}
	return b.String()
}

// closedLog makes a pane, floods it, closes its window, and returns the log's
// dir and the window id.
func closedLog(t *testing.T, d *Daemon, session, window string) string {
	t.Helper()
	sess, pty := coldTestPane(t, d, session, window)
	flood(pty, 0, 8000)
	if err := sess.ClosePTY(pty.ID); err != nil {
		t.Fatal(err)
	}
	return mustPath(coldDir(session, window))
}

func setClosedAt(t *testing.T, dir string, at time.Time) {
	t.Helper()
	m, err := coldlog.ReadMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.ClosedAt = at
	if err := coldlog.WriteMarker(dir, m); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// The log of a closed pane outlives it by the grace period, counted from the
// close. The edge is exact, and the setting applies to logs already orphaned.
func TestColdGraceSweepAtTheEdge(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 1000)
	dir := closedLog(t, d, "grace", "win-1")
	closed := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	setClosedAt(t, dir, closed)
	day := 24 * time.Hour

	d.sweepColdOrphans(closed.Add(3*day - time.Second))
	if !exists(dir) {
		t.Fatal("purged a second early")
	}
	// A longer grace period applies to the orphan already there.
	d.manager.SetColdPolicy(ResolveColdPolicy(nil, 0, 0, 0, 10))
	d.sweepColdOrphans(closed.Add(9 * day))
	if !exists(dir) {
		t.Fatal("purged before the changed setting allowed")
	}
	d.manager.SetColdPolicy(coldTestPolicy())
	if got := d.sweepColdOrphans(closed.Add(3 * day)); len(got) != 1 || exists(dir) {
		t.Fatalf("not purged at the edge: %v", got)
	}
}

func TestColdKeepSurvivesTheSweepUntilUnkept(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 1000)
	dir := closedLog(t, d, "kept", "win-1")
	closed := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	setClosedAt(t, dir, closed)

	if _, verr := d.verbKeepHistoryLog(nil, json.RawMessage(`{"log":"kept/win-1"}`)); verr != nil {
		t.Fatal(verr)
	}
	d.sweepColdOrphans(closed.Add(400 * 24 * time.Hour))
	if !exists(dir) {
		t.Fatal("a kept orphan was purged")
	}
	if _, verr := d.verbUnkeepHistoryLog(nil, json.RawMessage(`{"log":"kept/win-1"}`)); verr != nil {
		t.Fatal(verr)
	}
	d.sweepColdOrphans(closed.Add(400 * 24 * time.Hour))
	if exists(dir) {
		t.Fatal("an unkept orphan past its grace period survived")
	}
}

func TestColdSweepIsHourly(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 1000)
	dir := closedLog(t, d, "hourly", "win-1")
	closed := time.Now().Add(-10 * 24 * time.Hour)
	setClosedAt(t, dir, closed)
	start := time.Now()
	d.orphanSweepAt = start
	d.sweepColdOrphansIfDue(start.Add(30 * time.Minute))
	if !exists(dir) {
		t.Fatal("swept again within the hour")
	}
	d.sweepColdOrphansIfDue(start.Add(61 * time.Minute))
	if exists(dir) {
		t.Fatal("not swept after the hour")
	}
}

// A log with no marker and no pane is what a crash leaves. The sweep gives it
// a marker so its grace period starts, and spares the logs of live panes,
// including one whose log is not open yet.
func TestColdCrashOrphanIsStamped(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 1000)
	_, pty := coldTestPane(t, d, "live", "win-live")
	_ = pty
	liveDir := mustPath(coldDir("live", "win-live"))
	if err := os.MkdirAll(liveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Both dirs hold a log; the second has no pane.
	crashDir := mustPath(coldDir("crashed", "win-1"))
	for _, dir := range []string{liveDir, crashDir} {
		l, err := coldlog.OpenLog(dir, coldlog.Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range coldTestRows(t, 50) {
			l.Append(r)
		}
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	d.sweepColdOrphans(now)
	if _, err := coldlog.ReadMarker(liveDir); !os.IsNotExist(err) {
		t.Fatalf("a live pane's log was stamped: %v", err)
	}
	m, err := coldlog.ReadMarker(crashDir)
	if err != nil || !m.ClosedAt.Equal(now) {
		t.Fatalf("crash orphan marker %+v, %v", m, err)
	}
	d.sweepColdOrphans(now.Add(3*24*time.Hour - time.Second))
	if !exists(crashDir) {
		t.Fatal("purged early")
	}
	d.sweepColdOrphans(now.Add(3 * 24 * time.Hour))
	if exists(crashDir) || !exists(liveDir) {
		t.Fatalf("crash orphan gone: %v, live log kept: %v", !exists(crashDir), exists(liveDir))
	}
}

func TestColdGraceZeroPurgesAtTheSweep(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 1000)
	dir := closedLog(t, d, "zero", "win-1")
	d.manager.SetColdPolicy(ResolveColdPolicy(nil, 0, 0, 0, 0))
	d.sweepColdOrphans(time.Now())
	if exists(dir) {
		t.Fatal("grace 0 kept the orphan")
	}
}

func historyLogCall(t *testing.T, d *Daemon, cs *connState, verb string, params string) (map[string]any, *verbError) {
	t.Helper()
	entry, ok := verbRegistry()[verb]
	if !ok {
		t.Fatalf("no verb %s", verb)
	}
	res, verr := entry.handler(d, cs, json.RawMessage(params))
	if verr != nil {
		return nil, verr
	}
	b, _ := json.Marshal(res)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m, nil
}

func TestHistoryLogPurgeDryRunAndConfirm(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 1000)
	dirA := closedLog(t, d, "pa", "win-1")
	dirB := closedLog(t, d, "pb", "win-1")
	setClosedAt(t, dirA, time.Now().Add(-5*24*time.Hour))
	setClosedAt(t, dirB, time.Now().Add(-1*24*time.Hour))

	res, verr := historyLogCall(t, d, nil, "purge-history-logs", `{"older_than_days":2,"dry_run":true}`)
	if verr != nil {
		t.Fatal(verr)
	}
	purged := res["purged"].([]any)
	if len(purged) != 1 || res["dry_run"] != true || !exists(dirA) {
		t.Fatalf("dry run: %v", res)
	}
	token := res["confirm"].(string)

	// Without the token nothing goes, and the refusal carries the token.
	if _, verr := historyLogCall(t, d, nil, "purge-history-logs", `{"older_than_days":2}`); verr == nil || verr.Code != ErrVerbConfirmRequired || verr.Hint.Confirm != token {
		t.Fatalf("purge without a token: %+v", verr)
	}
	if _, verr := historyLogCall(t, d, nil, "purge-history-logs", `{"older_than_days":2,"confirm":"nope"}`); verr == nil || verr.Code != ErrVerbConfirmRequired {
		t.Fatalf("purge with a wrong token: %+v", verr)
	}
	if !exists(dirA) {
		t.Fatal("deleted without the token")
	}
	res, verr = historyLogCall(t, d, nil, "purge-history-logs", `{"older_than_days":2,"confirm":"`+token+`"}`)
	if verr != nil || exists(dirA) || !exists(dirB) {
		t.Fatalf("purge: %v %v, A there %v, B there %v", res, verr, exists(dirA), exists(dirB))
	}
	// Selectors are exclusive.
	if _, verr := historyLogCall(t, d, nil, "purge-history-logs", `{"log":"pb/win-1","all_orphaned":true}`); verr == nil || verr.Code != ErrVerbInvalidParams {
		t.Fatalf("two selectors: %+v", verr)
	}
}

// An agent reads and keeps; it does not delete or adopt. A restricted
// connection is one that gave up authority on itself.
func TestHistoryLogRightsOfARestrictedCaller(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 1000)
	dir := closedLog(t, d, "agent", "win-1")
	cs := &connState{}
	cs.scope.Store(&connScope{readOnly: true})

	if _, verr := historyLogCall(t, d, cs, "list-history-logs", `{}`); verr != nil {
		t.Fatal(verr)
	}
	if _, verr := historyLogCall(t, d, cs, "keep-history-log", `{"log":"agent/win-1"}`); verr != nil {
		t.Fatal(verr)
	}
	if res, verr := historyLogCall(t, d, cs, "purge-history-logs", `{"all_orphaned":true,"dry_run":true}`); verr != nil || res["dry_run"] != true {
		t.Fatalf("dry run for an agent: %v %v", res, verr)
	}
	if _, verr := historyLogCall(t, d, cs, "unkeep-history-log", `{"log":"agent/win-1"}`); verr != nil {
		t.Fatal(verr)
	}
	// Even with the right token.
	res, _ := historyLogCall(t, d, nil, "purge-history-logs", `{"all_orphaned":true,"dry_run":true}`)
	_, verr := historyLogCall(t, d, cs, "purge-history-logs", `{"all_orphaned":true,"confirm":"`+res["confirm"].(string)+`"}`)
	if verr == nil || verr.Code != ErrVerbForbidden || !exists(dir) {
		t.Fatalf("an agent purged: %+v", verr)
	}
	if _, verr := historyLogCall(t, d, cs, "adopt-history-log", `{"log":"agent/win-1"}`); verr == nil || verr.Code != ErrVerbForbidden {
		t.Fatalf("an agent adopted: %+v", verr)
	}
}

// An orphan goes onto a new window: the files move, the marker goes, the
// window's rows continue the log, and the old id is gone.
func TestHistoryLogAdoptOntoANewWindow(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 1000)
	old := closedLog(t, d, "work", "win-old")
	before, _ := coldRows(t, old)

	sess, pty := coldTestPane(t, d, "work2", "win-new")
	_ = sess
	res, verr := historyLogCall(t, d, nil, "adopt-history-log", `{"log":"work/win-old","session":"work2","window":"win-new"}`)
	if verr != nil {
		t.Fatal(verr)
	}
	if res["log"] != "work2/win-new" || res["from"] != "work/win-old" || int(res["rows"].(float64)) != len(before) {
		t.Fatalf("adopt: %v", res)
	}
	dst := mustPath(coldDir("work2", "win-new"))
	if exists(old) || !exists(dst) {
		t.Fatalf("old dir there: %v, new dir there: %v", exists(old), exists(dst))
	}
	if _, err := coldlog.ReadMarker(dst); !os.IsNotExist(err) {
		t.Fatalf("the marker stayed: %v", err)
	}
	// The sweep leaves it: its pane is live though no row has opened the log.
	d.sweepColdOrphans(time.Now().Add(100 * 24 * time.Hour))
	if !exists(dst) {
		t.Fatal("the adopted log was swept")
	}

	flood(pty, 100000, 6000)
	nums, _ := coldRows(t, dst)
	if len(nums) <= len(before) {
		t.Fatalf("the new window's rows did not follow: %d then %d", len(before), len(nums))
	}
	for i := range before {
		if nums[i] != before[i] {
			t.Fatalf("row %d changed: %d, was %d", i, nums[i], before[i])
		}
	}
	// The same call from the window's side, shared handler.
	old2 := closedLog(t, d, "work3", "win-x")
	_, pty3 := coldTestPane(t, d, "work4", "win-y")
	_ = pty3
	if _, verr := historyLogCall(t, d, nil, "adopt-history", `{"session":"work4","window":"win-y","log":"work3/win-x"}`); verr != nil || exists(old2) {
		t.Fatalf("adopt-history: %v", verr)
	}
	// A window with a log of its own is refused, and a live log cannot be adopted.
	if _, verr := historyLogCall(t, d, nil, "adopt-history-log", `{"log":"work2/win-new","session":"work4","window":"win-y"}`); verr == nil {
		t.Fatal("adopted a live log")
	}
	old3 := closedLog(t, d, "work5", "win-z")
	if _, verr := historyLogCall(t, d, nil, "adopt-history-log", `{"log":"work5/win-z","session":"work2","window":"win-new"}`); verr == nil || !exists(old3) {
		t.Fatalf("adopted onto a window with a log: %+v", verr)
	}
}

func TestHistoryLogListShowKeep(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 1000)
	_, live := coldTestPane(t, d, "ls", "win-live")
	flood(live, 0, 5000)
	dir := closedLog(t, d, "ls2", "win-gone")
	setClosedAt(t, dir, time.Now().Add(-24*time.Hour))

	res, verr := historyLogCall(t, d, nil, "list-history-logs", `{}`)
	if verr != nil {
		t.Fatal(verr)
	}
	logs := res["logs"].([]any)
	if len(logs) != 2 || int(res["grace_days"].(float64)) != 3 {
		t.Fatalf("list: %v", res)
	}
	byID := map[string]map[string]any{}
	for _, l := range logs {
		m := l.(map[string]any)
		byID[m["log"].(string)] = m
	}
	if byID["ls/win-live"]["state"] != "live" || byID["ls2/win-gone"]["state"] != "orphaned" {
		t.Fatalf("states: %v", byID)
	}
	if dl := byID["ls2/win-gone"]["days_left"].(float64); dl < 1.9 || dl > 2.1 {
		t.Fatalf("days_left %v", dl)
	}
	res, _ = historyLogCall(t, d, nil, "list-history-logs", `{"state":"orphaned"}`)
	if len(res["logs"].([]any)) != 1 {
		t.Fatalf("orphaned only: %v", res)
	}
	if _, verr := historyLogCall(t, d, nil, "keep-history-log", `{"log":"ls/win-live"}`); verr == nil {
		t.Fatal("kept a live log")
	}
	res, verr = historyLogCall(t, d, nil, "keep-history-log", `{"log":"ls2/win-gone"}`)
	if verr != nil || res["keep"] != true || res["days_left"] != nil {
		t.Fatalf("keep: %v %v", res, verr)
	}
	if _, verr := historyLogCall(t, d, nil, "show-history-log", `{"log":"nope/none"}`); verr == nil {
		t.Fatal("showed a log that does not exist")
	}
}
