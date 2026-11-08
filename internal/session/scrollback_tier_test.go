package session

import (
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"
)

func tierTestPolicy(hotWhen []string) ColdPolicy {
	p := coldTestPolicy()
	p.Tier = ResolveTierPolicy(hotWhen, 300, 5000)
	return p
}

// tierState says what the session shows: whether the pane's window is
// focused or minimized.
func tierState(sess *Session, window string, pty *PTY, focused, minimized bool) {
	st := &SessionState{Name: sess.Name(), Windows: []WindowState{{ID: window, PTYID: pty.ID, Minimized: minimized}}}
	if focused {
		st.FocusedWindowID = window
	}
	sess.UpdateState(st)
}

func ringLen(p *PTY) int {
	p.terminalMu.Lock()
	defer p.terminalMu.Unlock()
	return p.terminal.ScrollbackLen()
}

func ringText(p *PTY, i int) string {
	p.terminalMu.Lock()
	defer p.terminalMu.Unlock()
	return strings.TrimRight(p.terminal.ScrollbackLine(i).String(), " ")
}

// quiet makes the pane's last output long ago.
func quiet(p *PTY, now time.Time) { p.lastOutput.Store(now.Add(-time.Hour).UnixNano()) }

func TestTierIdleUndisplayedPaneDemotesAndKeepsEveryRow(t *testing.T) {
	d := coldTestDaemon(t, tierTestPolicy(nil), 30000)
	sess, pty := coldTestPane(t, d, "tier", "win-1")
	tierState(sess, "win-1", pty, false, false)
	const total = 40000
	flood(pty, 0, total)
	now := time.Now()
	quiet(pty, now)

	if got := ringLen(pty); got != 30000 {
		t.Fatalf("hot ring = %d", got)
	}
	d.tierScrollback(now)
	if got := ringLen(pty); got != 5000 {
		t.Fatalf("cold ring = %d, want 5000", got)
	}
	pty.cold.flush()

	// The log and the ring together are the whole history, in order, no gap.
	r := readCold(t, mustPath(coldDir("tier", "win-1")))
	if r.First() != 0 {
		t.Fatalf("first row %d", r.First())
	}
	checkRows(t, r, 0, r.Next(), rowName)
	if dropped := pty.cold.log.Dropped(); dropped != 0 {
		t.Fatalf("%d rows dropped", dropped)
	}
	if got, want := ringText(pty, 0), rowName(r.Next()); got != want {
		t.Fatalf("ring starts at %q, the log ends before %q", got, want)
	}
	if got, want := ringText(pty, 4999), rowName(r.Next()+4999); got != want {
		t.Fatalf("ring ends at %q, want %q", got, want)
	}
}

func TestTierHotSignalsKeepTheRing(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		setup func(sess *Session, pty *PTY)
		hot   bool
	}{
		{"nothing holds", func(s *Session, p *PTY) {}, false},
		{"focused", func(s *Session, p *PTY) { tierState(s, "win-1", p, true, false) }, true},
		{"printed lately", func(s *Session, p *PTY) { p.lastOutput.Store(now.Add(-10 * time.Second).UnixNano()) }, true},
		{"displayed", func(s *Session, p *PTY) { s.tuiClients.Store(1) }, true},
		{"displayed but minimized", func(s *Session, p *PTY) { s.tuiClients.Store(1); tierState(s, "win-1", p, false, true) }, false},
		{"printed too long ago", func(s *Session, p *PTY) { p.lastOutput.Store(now.Add(-6 * time.Minute).UnixNano()) }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := coldTestDaemon(t, tierTestPolicy(nil), 8000)
			sess, pty := coldTestPane(t, d, "tier", "win-1")
			tierState(sess, "win-1", pty, false, false)
			flood(pty, 0, 10000)
			quiet(pty, now)
			c.setup(sess, pty)
			d.tierScrollback(now)
			want := 5000
			if c.hot {
				want = 8000
			}
			if got := ringLen(pty); got != want {
				t.Fatalf("ring = %d, want %d", got, want)
			}
		})
	}
}

func TestTierRemovedSignalStopsCounting(t *testing.T) {
	now := time.Now()
	d := coldTestDaemon(t, tierTestPolicy([]string{"displayed", "output"}), 8000)
	sess, pty := coldTestPane(t, d, "tier", "win-1")
	tierState(sess, "win-1", pty, true, false) // focused, but "focused" is not listed
	flood(pty, 0, 10000)
	quiet(pty, now)
	d.tierScrollback(now)
	if got := ringLen(pty); got != 5000 {
		t.Fatalf("ring = %d, want 5000", got)
	}

	d2 := coldTestDaemon(t, tierTestPolicy([]string{}), 8000)
	sess2, pty2 := coldTestPane(t, d2, "tier2", "win-1")
	tierState(sess2, "win-1", pty2, true, false)
	sess2.tuiClients.Store(1)
	flood(pty2, 0, 10000)
	pty2.lastOutput.Store(now.UnixNano())
	d2.tierScrollback(now)
	if got := ringLen(pty2); got != 5000 {
		t.Fatalf("empty list: ring = %d, want 5000 (always cold)", got)
	}
}

func TestTierPromotionRestoresTheMax(t *testing.T) {
	now := time.Now()
	d := coldTestDaemon(t, tierTestPolicy(nil), 30000)
	sess, pty := coldTestPane(t, d, "tier", "win-1")
	tierState(sess, "win-1", pty, false, false)
	flood(pty, 0, 40000)
	quiet(pty, now)
	d.tierScrollback(now)
	if ringLen(pty) != 5000 {
		t.Fatal("did not demote")
	}

	tierState(sess, "win-1", pty, true, false)
	d.tierScrollback(now)
	if got := pty.ringRows.Load(); got != 30000 {
		t.Fatalf("depth after promotion = %d", got)
	}
	if got := ringLen(pty); got != 5000 {
		t.Fatalf("promotion refilled the ring: %d", got)
	}
	// New output grows the ring again, to the configured depth and no
	// further, and what leaves it follows the rows already in the log.
	flood(pty, 40000, 40000)
	if got := ringLen(pty); got != 30000 {
		t.Fatalf("ring after new output = %d, want 30000", got)
	}
	pty.cold.flush()
	r := readCold(t, mustPath(coldDir("tier", "win-1")))
	checkRows(t, r, 0, r.Next(), rowName)
	if got, want := ringText(pty, 0), rowName(r.Next()); got != want {
		t.Fatalf("ring starts at %q, log ends before %q", got, want)
	}
}

func TestTierMarksSurviveTheShrink(t *testing.T) {
	now := time.Now()
	d := coldTestDaemon(t, tierTestPolicy(nil), 30000)
	sess, pty := coldTestPane(t, d, "tier", "win-1")
	tierState(sess, "win-1", pty, false, false)
	writePane(pty, "\x1b]133;A\x07OLD-PROMPT\r\n")
	flood(pty, 0, 20000)
	writePane(pty, "\x1b]133;A\x07NEW-PROMPT\r\n")
	flood(pty, 20000, 2000)
	quiet(pty, now)

	markedRows := func() []string {
		var out []string
		for _, m := range pty.marks.Markers() {
			pty.terminalMu.Lock()
			n := pty.terminal.ScrollbackLen()
			var s string
			if m.AbsLine < n {
				s = strings.TrimRight(pty.terminal.ScrollbackLine(m.AbsLine).String(), " ")
			}
			pty.terminalMu.Unlock()
			out = append(out, s)
		}
		return out
	}
	if got := markedRows(); !reflect.DeepEqual(got, []string{"OLD-PROMPT", "NEW-PROMPT"}) {
		t.Fatalf("before: marks resolve to %q", got)
	}
	d.tierScrollback(now)
	if ringLen(pty) != 5000 {
		t.Fatal("did not demote")
	}
	// The mark in the rows the ring gave up is forgotten, as it is when the
	// ring evicts; the one still in the ring resolves to its own row.
	if got := markedRows(); !reflect.DeepEqual(got, []string{"NEW-PROMPT"}) {
		t.Fatalf("after: marks resolve to %q", got)
	}
}

func TestTierLeavesAPaneWithoutALogAlone(t *testing.T) {
	now := time.Now()
	off := false
	pol := ResolveColdPolicy(&off, 0, 0, 0, 3)
	pol.Tier = ResolveTierPolicy(nil, 300, 5000)
	d := coldTestDaemon(t, pol, 8000)
	sess, pty := coldTestPane(t, d, "tier", "win-1")
	tierState(sess, "win-1", pty, false, false)
	writePane(pty, strings.Repeat("x\r\n", 10000))
	quiet(pty, now)
	d.tierScrollback(now)
	if got := ringLen(pty); got != 8000 {
		t.Fatalf("a pane with no log lost rows: ring %d", got)
	}
}

func TestTierSweepAsksForAnotherLookWhileOutputCountsDown(t *testing.T) {
	now := time.Now()
	d := coldTestDaemon(t, tierTestPolicy(nil), 8000)
	sess, pty := coldTestPane(t, d, "tier", "win-1")
	tierState(sess, "win-1", pty, false, false)
	flood(pty, 0, 10000)
	pty.lastOutput.Store(now.Add(-time.Minute).UnixNano())
	if !d.tierScrollback(now) {
		t.Fatal("a pane hot by output alone needs a later sweep")
	}
	if d.tierScrollback(now.Add(10 * time.Minute)) {
		t.Fatal("the sweep still wants another look after the pane went cold")
	}
	if got := ringLen(pty); got != 5000 {
		t.Fatalf("ring = %d", got)
	}
}

func TestResolveTierPolicy(t *testing.T) {
	p := ResolveTierPolicy(nil, 300, 0)
	if p.Signals != 0b111 || p.Idle != 300*time.Second || p.ColdRows != 10000 {
		t.Errorf("defaults: %+v", p)
	}
	if p := ResolveTierPolicy([]string{}, 300, 10); p.Signals != 0 || p.ColdRows != 5000 {
		t.Errorf("empty list / floor: %+v", p)
	}
	if p := ResolveTierPolicy([]string{"focused", "nope", "focused"}, 0, 20000); p.Signals != 0b010 || p.ColdRows != 20000 {
		t.Errorf("names: %+v", p)
	}
}

// TestTierFootprint is a measuring run, not a check: with MTM_TIER_FOOTPRINT=1
// it fills ten panes with 100k rows each, demotes nine, and prints the
// process's footprint (macOS footprint(1), else the Go heap) before and after.
func TestTierFootprint(t *testing.T) {
	if os.Getenv("MTM_TIER_FOOTPRINT") == "" {
		t.Skip("set MTM_TIER_FOOTPRINT=1 to measure")
	}
	panes := 10
	if n, err := strconv.Atoi(os.Getenv("MTM_TIER_PANES")); err == nil {
		panes = n
	}
	const rows = 100000
	d := coldTestDaemon(t, tierTestPolicy(nil), rows)
	measure := func(label string) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		out := "(no footprint)"
		if b, err := exec.Command("footprint", "-p", strconv.Itoa(os.Getpid())).CombinedOutput(); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if strings.Contains(l, "Footprint:") {
					out = strings.TrimSpace(l)
					break
				}
			}
		}
		t.Logf("%s: heapInuse=%d MiB heapSys=%d MiB heapReleased=%d MiB; footprint %s", label, m.HeapInuse>>20, m.HeapSys>>20, m.HeapReleased>>20, out)
	}
	debug.FreeOSMemory()
	measure("empty")
	var ptys []*PTY
	for i := range panes {
		sess, pty := coldTestPane(t, d, fmt.Sprintf("fp%d", i), "win-1")
		tierState(sess, "win-1", pty, i == 0, i != 0)
		// Full-width coloured rows, which is what a build log or an agent's
		// transcript fills a ring with.
		for n := 0; n < rows; n += 4000 {
			var b strings.Builder
			for j := n; j < min(n+4000, rows); j++ {
				fmt.Fprintf(&b, "\x1b[3%dm%07d\x1b[0m the quick brown fox %d jumps\r\n", j%7+1, j, j*7919)
			}
			writePane(pty, b.String())
			pty.cold.flush()
		}
		ptys = append(ptys, pty)
	}
	now := time.Now()
	for _, p := range ptys {
		quiet(p, now)
	}
	debug.FreeOSMemory()
	measure("all hot")
	d.tierScrollback(now)
	measure("after demote, before trim")
	time.Sleep(6 * time.Second) // memtrim settles for two seconds
	debug.FreeOSMemory()
	measure("after trim")
	time.Sleep(time.Second)
	debug.FreeOSMemory()
	measure("10 s later")
	if b, err := exec.Command("footprint", "-p", strconv.Itoa(os.Getpid())).CombinedOutput(); err == nil {
		t.Logf("footprint detail after trim:\n%s", b)
	}
	for i, p := range ptys {
		want := 5000
		if i == 0 {
			want = rows
		}
		if got := ringLen(p); got != want && !(i == 0 && got > rows-100) {
			t.Errorf("pane %d ring = %d, want %d", i, got, want)
		}
	}
}
