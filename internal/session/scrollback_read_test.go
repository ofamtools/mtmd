package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ofam.dev/mtmd/internal/coldlog"
)

// readerCorpus is what both panes of an equivalence test are fed: plain,
// styled, wide, wrapped (the panes are 40 columns wide) and blank rows.
func readerCorpus(n int) []string {
	chunks := make([]string, 0, n/50+1)
	var b strings.Builder
	for i := range n {
		switch i % 9 {
		case 0:
			fmt.Fprintf(&b, "\x1b[1;31mred %05d\x1b[0m plain\r\n", i)
		case 1:
			fmt.Fprintf(&b, "wide %05d 漢字かな\r\n", i)
		case 2:
			// Longer than the pane: wraps into three rows.
			fmt.Fprintf(&b, "wrap %05d %s\r\n", i, strings.Repeat("abcdefghij", 8))
		case 3:
			// A wide character that does not fit the last column wraps early.
			fmt.Fprintf(&b, "%s漢tail %05d\r\n", strings.Repeat("p", 39), i)
		case 4:
			b.WriteString("\r\n")
		case 5:
			fmt.Fprintf(&b, "\x1b[38;2;10;200;30mtrue %05d\x1b[48;5;17m bg\x1b[0m\r\n", i)
		default:
			fmt.Fprintf(&b, "line %05d\r\n", i)
		}
		if i%50 == 49 {
			chunks = append(chunks, b.String())
			b.Reset()
		}
	}
	return append(chunks, b.String())
}

func captureVerb(t *testing.T, d *Daemon, session string, params map[string]any) map[string]any {
	t.Helper()
	params["session"] = session
	raw, _ := json.Marshal(params)
	out, verr := d.verbCapturePane(&connState{}, raw)
	if verr != nil {
		t.Fatalf("capture-pane %v: %v", params, verr)
	}
	return out.(map[string]any)
}

// readerPair is a pane whose ring holds 300 rows and logs the rest to disk,
// and a pane with no log and a ring that holds everything, fed the same bytes.
func readerPair(t *testing.T, rows int) (small, big *Daemon, sp, bp *PTY) {
	t.Helper()
	small = coldTestDaemon(t, coldTestPolicy(), 300)
	_, sp = coldTestPane(t, small, "small", "win-s")
	big = coldTestDaemonHere(t, ColdPolicy{}, 100000)
	_, bp = coldTestPane(t, big, "big", "win-b")
	for _, c := range readerCorpus(rows) {
		writePane(sp, c)
		writePane(bp, c)
		sp.cold.flush()
	}
	if sp.cold.view().rows() == 0 {
		t.Fatal("nothing reached the log")
	}
	return
}

func TestCaptureJoinsDiskAndRingLikeABigRing(t *testing.T) {
	small, big, sp, _ := readerPair(t, 4000)
	cold := sp.cold.view().rows()

	variants := []map[string]any{
		{},
		{"styled": true},
		{"unwrap": true},
		{"styled": true, "unwrap": true},
		{"resolved": true, "unwrap": true},
		{"trim": true},
	}
	selections := []map[string]any{
		{},
		{"lines": 40},
		{"lines": 3000},
		{"lines": 100000},
		{"start": 3, "end": 40},
		{"start": 100, "end": 100},
		{"start": 5},
		{"end": 7},
		{"start": cold - 20, "end": cold + 30},
		{"start": cold + 1, "end": cold + 15},
		{"start": cold, "end": cold},
		{"start": 2500, "end": 99999},
	}
	for _, v := range variants {
		for _, sel := range selections {
			params := map[string]any{"source": "recent"}
			for k, x := range v {
				params[k] = x
			}
			for k, x := range sel {
				params[k] = x
			}
			p1, p2 := map[string]any{}, map[string]any{}
			for k, x := range params {
				p1[k], p2[k] = x, x
			}
			got := captureVerb(t, small, "small", p1)
			want := captureVerb(t, big, "big", p2)
			name := fmt.Sprintf("%v %v", v, sel)
			gc, wc := got["content"].(string), want["content"].(string)
			if gc != wc {
				i := 0
				for i < len(gc) && i < len(wc) && gc[i] == wc[i] {
					i++
				}
				lo := max(i-60, 0)
				t.Errorf("%s: content differs at byte %d (len %d vs %d)\n got: %q\nwant: %q", name, i, len(gc), len(wc),
					gc[lo:min(i+60, len(gc))], wc[lo:min(i+60, len(wc))])
				continue
			}
			if got["history_rows"] != want["history_rows"] {
				t.Errorf("%s: history_rows %v want %v", name, got["history_rows"], want["history_rows"])
			}
			if got["lines_omitted"] != want["lines_omitted"] {
				t.Errorf("%s: lines_omitted %v want %v", name, got["lines_omitted"], want["lines_omitted"])
			}
		}
	}
	if got := captureVerb(t, small, "small", map[string]any{"source": "recent"}); got["history_rows_disk"] != cold {
		t.Errorf("history_rows_disk %v, want %d", got["history_rows_disk"], cold)
	}
}

func TestCaptureVisibleIgnoresTheDisk(t *testing.T) {
	small, big, _, _ := readerPair(t, 1500)
	a := captureVerb(t, small, "small", map[string]any{"source": "visible"})
	b := captureVerb(t, big, "big", map[string]any{"source": "visible"})
	if a["content"] != b["content"] {
		t.Fatal("a visible capture differs")
	}
	if _, ok := a["history_rows_disk"]; ok {
		t.Error("a visible capture reported disk rows")
	}
}

func TestCaptureWithoutALogIsTheRingCapture(t *testing.T) {
	d := coldTestDaemon(t, ColdPolicy{}, 300)
	_, p := coldTestPane(t, d, "off", "win-o")
	for _, c := range readerCorpus(800) {
		writePane(p, c)
	}
	if _, _, _, ok := p.captureWithCold(false, false, captureSel{}, nil); ok {
		t.Fatal("a pane with no log read the disk")
	}
}

// A hole in the log (rows the writer's queue dropped) reads as one marker line
// per run, in place of the lost rows.
func TestColdLinesMarkAGap(t *testing.T) {
	dir := t.TempDir()
	l, err := coldlog.OpenLog(dir, coldlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Two logs' worth of rows in separate segments, then lose the middle one.
	small, _, sp, _ := readerPair(t, 10000)
	_ = small
	src := sp.cold.view()
	for id := src.first; id < src.end; id++ {
		r, err := src.rd.Row(id)
		if err != nil {
			t.Fatal(err)
		}
		l.Append(r)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	segs, _ := filepath.Glob(filepath.Join(dir, "seg-*"))
	if len(segs) < 3 {
		t.Fatalf("want 3 segments to lose the middle one, have %d", len(segs))
	}
	if err := os.Remove(segs[1]); err != nil {
		t.Fatal(err)
	}
	rd, err := coldlog.OpenReader(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	gaps := rd.Stats().Gaps
	if len(gaps) != 1 {
		t.Fatalf("gaps %v", gaps)
	}
	cl := newColdLines(coldView{rd: rd, first: rd.First(), end: rd.Next()}, false, false, 40)
	var lines []string
	cl.walk(cl.first, cl.end, true, func(start, next uint64, text string, _ bool) bool {
		lines = append(lines, text)
		return true
	})
	want := coldGapText(gaps[0].N)
	markers := 0
	for _, s := range lines {
		if s == want {
			markers++
		}
	}
	if markers != 1 {
		t.Fatalf("%d marker lines %q in %d lines", markers, want, len(lines))
	}
	if int64(len(lines)) != int64(cl.end-cl.first)-int64(gaps[0].N)+1 {
		t.Errorf("%d lines for %d ids with a %d-row gap", len(lines), cl.end-cl.first, gaps[0].N)
	}
	// The index agrees with the walk.
	if ix := cl.index(); len(ix.starts) != len(lines) {
		t.Errorf("index %d lines, walk %d", len(ix.starts), len(lines))
	}
}

func searchVerb(t *testing.T, d *Daemon, query string) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"query": query})
	out, verr := d.verbSearchPanes(&connState{}, raw)
	if verr != nil {
		t.Fatalf("search-panes: %v", verr)
	}
	return out.(map[string]any)
}

func TestSearchFindsARowOnlyTheDiskHolds(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 300)
	_, p := coldTestPane(t, d, "find", "win-f")
	writePane(p, "needle-on-disk-only\r\n")
	for _, c := range readerCorpus(4000) {
		writePane(p, c)
	}
	p.cold.flush()
	if strings.Contains(p.CaptureContent(true, false), "needle-on-disk-only") {
		t.Fatal("the marker is still in the ring")
	}
	res := searchVerb(t, d, "needle-on-disk-only")
	ms := res["matches"].([]map[string]any)
	if len(ms) != 1 || ms[0]["text"] != "needle-on-disk-only" || ms[0]["row"] != 0 {
		t.Fatalf("matches %v", ms)
	}
	if res["complete"] != true || res["truncated"] != false {
		t.Errorf("complete %v truncated %v", res["complete"], res["truncated"])
	}
	// Newest first: a text that is in many rows answers from the end.
	ms = searchVerb(t, d, "line 0")["matches"].([]map[string]any)
	if len(ms) < 2 || ms[0]["row"].(int) < ms[1]["row"].(int) {
		t.Fatalf("not newest first: %v", ms[:min(2, len(ms))])
	}
	// age_rows counts from the bottom across the disk rows too.
	if age := res["matches"].([]map[string]any)[0]["age_rows"].(int); age < 4000 {
		t.Errorf("age_rows %d", age)
	}
}

func TestSearchBudgetRunsOutOnDisk(t *testing.T) {
	d := coldTestDaemon(t, coldTestPolicy(), 300)
	_, p := coldTestPane(t, d, "slow", "win-w")
	writePane(p, "needle-on-disk-only\r\n")
	for _, c := range readerCorpus(4000) {
		writePane(p, c)
	}
	p.cold.flush()
	old := searchPaneBudget
	searchPaneBudget = time.Nanosecond
	t.Cleanup(func() { searchPaneBudget = old })
	res := searchVerb(t, d, "needle-on-disk-only")
	if res["complete"] != false || res["truncated"] != true {
		t.Fatalf("complete %v truncated %v", res["complete"], res["truncated"])
	}
	if ms := res["matches"].([]map[string]any); len(ms) != 0 {
		t.Fatalf("found %v with no time", ms)
	}
	// What the budget did reach is kept, newest first: the screen is read
	// before the clock is looked at.
	searchPaneBudget = old
	if res := searchVerb(t, d, "needle-on-disk-only"); res["complete"] != true {
		t.Fatalf("with the budget back: %v", res)
	}
}

// Wherever the ring begins, even in the middle of a wrapped line, the two
// halves join into the same lines a big ring gives.
func TestCaptureSeamAtEveryOffset(t *testing.T) {
	for n := 1500; n < 1512; n++ {
		small, big, sp, _ := readerPair(t, n)
		cold := sp.cold.view().rows()
		for _, params := range []map[string]any{
			{"unwrap": true},
			{"unwrap": true, "styled": true, "lines": 300},
			{"unwrap": true, "start": cold - 3, "end": cold + 5},
			{"start": cold, "end": cold + 2},
		} {
			p1, p2 := map[string]any{"source": "recent"}, map[string]any{"source": "recent"}
			for k, x := range params {
				p1[k], p2[k] = x, x
			}
			got := captureVerb(t, small, "small", p1)["content"]
			want := captureVerb(t, big, "big", p2)["content"]
			if got != want {
				t.Errorf("rows %d %v: differs (len %d vs %d)", n, params, len(got.(string)), len(want.(string)))
			}
		}
	}
}
