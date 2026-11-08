package coldlog

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"ofam.dev/mtmd/internal/vt"
)

// corpus makes n real encoded rows: plain, styled, wide, link and wrapped.
func corpus(tb testing.TB, n int) []vt.EncodedRow {
	tb.Helper()
	e := vt.NewEmulator(20, 4)
	e.SetScrollbackMaxLines(8)
	var rows []vt.EncodedRow
	e.SetScrollbackEvictHook(func(r vt.EncodedRow) {
		r.Data = append([]byte(nil), r.Data...)
		rows = append(rows, r)
	})
	var b strings.Builder
	for i := 0; len(rows) < n && i < n*2+50; i++ {
		switch i % 5 {
		case 0:
			fmt.Fprintf(&b, "plain row %06d\r\n", i)
		case 1:
			fmt.Fprintf(&b, "\x1b[1;31mred\x1b[0m \x1b[38;2;10;20;30mtrue %06d\x1b[0m\r\n", i)
		case 2:
			fmt.Fprintf(&b, "日本語 wide %05d\r\n", i)
		case 3:
			fmt.Fprintf(&b, "\x1b]8;;http://x/%d\x1b\\link %d\x1b]8;;\x1b\\\r\n", i, i)
		case 4:
			fmt.Fprintf(&b, "this one wraps well past twenty columns %06d\r\n", i)
		}
		if i%50 == 49 {
			e.Write([]byte(b.String()))
			b.Reset()
		}
	}
	e.Write([]byte(b.String()))
	if len(rows) < n {
		tb.Fatalf("corpus: got %d rows, want %d", len(rows), n)
	}
	return rows[:n]
}

func small(o Options) Options {
	if o.BlockRows == 0 {
		o.BlockRows = 16
	}
	if o.SegmentRows == 0 {
		o.SegmentRows = 64
	}
	if o.FlushInterval == 0 {
		o.FlushInterval = time.Hour
	}
	return o
}

func openLog(t testing.TB, dir string, o Options) *Log {
	t.Helper()
	l, err := OpenLog(dir, small(o))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func checkRow(t testing.TB, r *Reader, id uint64, want vt.EncodedRow) {
	t.Helper()
	got, err := r.Row(id)
	if err != nil {
		t.Fatalf("row %d: %v", id, err)
	}
	if got.Width != want.Width || got.Flags != want.Flags || string(got.Data) != string(want.Data) {
		t.Fatalf("row %d differs: got %+v want %+v", id, got, want)
	}
	if got.Wrapped() != want.Wrapped() {
		t.Fatalf("row %d wrap flag", id)
	}
	txt, _ := r.Text(id)
	if txt != vt.ScrollbackRowText(want.Data) {
		t.Fatalf("row %d text %q != %q", id, txt, vt.ScrollbackRowText(want.Data))
	}
	line, _ := r.Line(id)
	if line.String() != vt.DecodeScrollbackRow(want.Data).String() {
		t.Fatalf("row %d line differs", id)
	}
}

func TestRoundTrip(t *testing.T) {
	rows := corpus(t, 500)
	var wrapped, wide int
	for _, r := range rows {
		if r.Wrapped() {
			wrapped++
		}
		if strings.Contains(vt.ScrollbackRowText(r.Data), "日") {
			wide++
		}
	}
	if wrapped == 0 || wide == 0 {
		t.Fatalf("corpus lacks wrapped (%d) or wide (%d) rows", wrapped, wide)
	}
	dir := t.TempDir()
	l := openLog(t, dir, Options{})
	for _, r := range rows {
		l.Append(r)
		r.Data[0] ^= 0xff // the hook's Data is only valid during the call
		r.Data[0] ^= 0xff
	}
	l.Flush()
	rd := l.Reader()
	if rd.Next() != 500 || rd.First() != 0 {
		t.Fatalf("extent %d..%d", rd.First(), rd.Next())
	}
	for i, r := range rows {
		checkRow(t, rd, uint64(i), r)
	}
	if _, err := rd.Row(500); !errors.Is(err, ErrNotYet) {
		t.Fatalf("row 500: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := OpenReader(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	st := r2.Stats()
	if st.Rows != 500 || st.Segments < 7 || len(st.Gaps) != 0 {
		t.Fatalf("stats %+v", st)
	}
	for i := 499; i >= 0; i -= 7 { // backwards, cache of 2 blocks
		checkRow(t, r2, uint64(i), rows[i])
	}
}

func TestAppendCopiesData(t *testing.T) {
	rows := corpus(t, 40)
	l := openLog(t, t.TempDir(), Options{})
	defer l.Close()
	buf := make([]byte, 0, 256)
	for _, r := range rows {
		buf = append(buf[:0], r.Data...)
		l.Append(vt.EncodedRow{Data: buf, Width: r.Width, Flags: r.Flags})
		for i := range buf {
			buf[i] = 0xAA
		}
	}
	l.Flush()
	for i, r := range rows {
		checkRow(t, l.Reader(), uint64(i), r)
	}
}

// snapshot copies a live log's files, as a crash would leave them.
func snapshot(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	ents, _ := os.ReadDir(src)
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dst, e.Name()), b, 0o600)
	}
	os.Remove(filepath.Join(dst, lockName))
	return dst
}

func TestTornTailRecovery(t *testing.T) {
	rows := corpus(t, 300)
	src := t.TempDir()
	l := openLog(t, src, Options{SegmentRows: 100000})
	for _, r := range rows[:200] {
		l.Append(r)
	}
	l.Flush() // 12 full blocks of 16 plus one of 8 on disk, unsealed
	crash := snapshot(t, src)
	l.Close()

	files, _ := filepath.Glob(filepath.Join(crash, "seg-*"))
	if len(files) != 1 {
		t.Fatalf("segments: %v", files)
	}
	fi, _ := os.Stat(files[0])
	// Cut into the middle of the last block (8 rows, a few dozen bytes).
	if err := os.Truncate(files[0], fi.Size()-9); err != nil {
		t.Fatal(err)
	}
	// A reader alone sees the good prefix and does not modify the file.
	rd, err := OpenReader(crash, 4)
	if err != nil {
		t.Fatal(err)
	}
	if rd.Next() != 192 {
		t.Fatalf("reader next = %d, want 192", rd.Next())
	}
	if now, _ := os.Stat(files[0]); now.Size() != fi.Size()-9 {
		t.Fatal("reader repaired the file")
	}
	// Opening for writing truncates the tail, seals, and continues after it.
	l2 := openLog(t, crash, Options{SegmentRows: 100000})
	if got := l2.Reader().Next(); got != 192 {
		t.Fatalf("recovered next = %d, want 192", got)
	}
	for i := 0; i < 192; i++ {
		checkRow(t, l2.Reader(), uint64(i), rows[i])
	}
	for _, r := range rows[192:300] {
		l2.Append(r)
	}
	l2.Close()
	rd, _ = OpenReader(crash, 4)
	if rd.Next() != 300 {
		t.Fatalf("after append next = %d", rd.Next())
	}
	for i := 0; i < 300; i++ {
		checkRow(t, rd, uint64(i), rows[i])
	}
	// Garbage bytes after the last block are cut too.
	files, _ = filepath.Glob(filepath.Join(crash, "seg-*"))
	for _, f := range files {
		if fh, err := os.OpenFile(f, os.O_APPEND|os.O_WRONLY, 0); err == nil {
			fh.Write([]byte("garbage"))
			fh.Close()
		}
	}
	l3 := openLog(t, crash, Options{})
	defer l3.Close()
	if l3.Reader().Next() != 300 {
		t.Fatalf("after garbage next = %d", l3.Reader().Next())
	}
}

func TestEmptySegmentRemoved(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, segName(0)), putHeader(0), 0o600)
	os.WriteFile(filepath.Join(dir, segName(5)), []byte("xx"), 0o600)
	l := openLog(t, dir, Options{})
	defer l.Close()
	if l.Reader().Next() != 0 {
		t.Fatal("next != 0")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "seg-*")); len(m) != 0 {
		t.Fatalf("left over: %v", m)
	}
}

func TestHelperKill9(t *testing.T) {
	dir := os.Getenv("COLDLOG_KILL9_DIR")
	if dir == "" {
		t.Skip("helper process")
	}
	rows := corpus(t, 105)
	l, err := OpenLog(dir, small(Options{BlockRows: 10, SegmentRows: 40}))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows[:100] {
		l.Append(r)
	}
	l.Flush()
	for _, r := range rows[100:] {
		l.Append(r) // never flushed: the open block
	}
	fmt.Println("ready")
	time.Sleep(time.Minute)
}

func TestKill9LosesOnlyOpenBlock(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperKill9$")
	cmd.Env = append(os.Environ(), "COLDLOG_KILL9_DIR="+dir)
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	sc := bufio.NewScanner(out)
	ready := make(chan bool, 1)
	go func() {
		for sc.Scan() {
			if sc.Text() == "ready" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("helper exited early")
		}
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		t.Fatal("helper timeout")
	}
	cmd.Process.Signal(syscall.SIGKILL)
	<-done

	// The dead process's lock is gone with it.
	rows := corpus(t, 105)
	l := openLog(t, dir, Options{BlockRows: 10, SegmentRows: 40})
	defer l.Close()
	if n := l.Reader().Next(); n != 100 {
		t.Fatalf("recovered %d rows, want exactly the 100 flushed", n)
	}
	for i := 0; i < 100; i++ {
		checkRow(t, l.Reader(), uint64(i), rows[i])
	}
	l.Append(rows[100])
	l.Flush()
	checkRow(t, l.Reader(), 100, rows[100])
}

func TestLockExcludesSecondWriter(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, Options{})
	if _, err := OpenLog(dir, Options{}); !errors.Is(err, ErrLocked) {
		t.Fatalf("second open: %v", err)
	}
	if !Locked(dir) {
		t.Fatal("Locked = false while open")
	}
	b, _ := os.ReadFile(filepath.Join(dir, lockName))
	if strings.TrimSpace(string(b)) != fmt.Sprint(os.Getpid()) {
		t.Fatalf("lock holds %q", b)
	}
	l.Close()
	if Locked(dir) {
		t.Fatal("Locked after close")
	}
	l2, err := OpenLog(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	l2.Close()
}

func TestRowCap(t *testing.T) {
	rows := corpus(t, 1000)
	l := openLog(t, t.TempDir(), Options{SegmentRows: 64, BlockRows: 16, MaxRows: 300})
	for _, r := range rows {
		l.Append(r)
	}
	l.Flush()
	st := l.Stats()
	// Oldest whole segments go; the open one stays, so up to one segment over.
	if st.Rows > 300+64 || st.Rows < 300-64 {
		t.Fatalf("rows kept = %d", st.Rows)
	}
	if st.Next != 1000 || st.First == 0 || st.First%64 != 0 {
		t.Fatalf("extent %d..%d", st.First, st.Next)
	}
	if _, err := l.Reader().Row(0); !errors.Is(err, ErrTrimmed) {
		t.Fatalf("row 0: %v", err)
	}
	checkRow(t, l.Reader(), 999, rows[999])
	checkRow(t, l.Reader(), st.First, rows[st.First])
	l.Close()
}

func TestByteCap(t *testing.T) {
	rows := corpus(t, 1000)
	probe := openLog(t, t.TempDir(), Options{})
	for _, r := range rows {
		probe.Append(r)
	}
	probe.Close()
	full := probe.Stats().Bytes

	l := openLog(t, t.TempDir(), Options{MaxBytes: full / 3})
	for _, r := range rows {
		l.Append(r)
	}
	l.Flush()
	st := l.Stats()
	if st.Bytes > full/3+full/10 || st.Bytes == 0 {
		t.Fatalf("bytes %d, cap %d, full %d", st.Bytes, full/3, full)
	}
	if st.First == 0 {
		t.Fatal("nothing trimmed")
	}
	l.Close()
	// The cap also applies when a log is reopened with a smaller cap.
	l = openLog(t, l.dir, Options{MaxBytes: full / 6})
	defer l.Close()
	if l.Stats().Bytes > full/6+full/10 {
		t.Fatalf("reopen bytes %d", l.Stats().Bytes)
	}
}

// fill makes a closed log of n rows under root/name and returns its dir.
func fill(t *testing.T, root, name string, n int, rows []vt.EncodedRow) string {
	t.Helper()
	dir := filepath.Join(root, name)
	l := openLog(t, dir, Options{})
	for i := range n {
		l.Append(rows[i%len(rows)])
	}
	l.Close()
	return dir
}

func TestTotalCapOrphansFirst(t *testing.T) {
	rows := corpus(t, 300)
	root := t.TempDir()
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	oldO := fill(t, root, "s/old.cold", 300, rows)
	newO := fill(t, root, "s/new.cold", 300, rows)
	keepO := fill(t, root, "s/keep.cold", 300, rows)
	bigLive := fill(t, root, "s/big.cold", 600, rows)
	smallLive := fill(t, root, "s/small.cold", 100, rows)
	for dir, m := range map[string]Marker{
		oldO: {ClosedAt: t0}, newO: {ClosedAt: t0.Add(time.Hour)}, keepO: {ClosedAt: t0.Add(-time.Hour), Keep: true},
	} {
		if err := WriteMarker(dir, m); err != nil {
			t.Fatal(err)
		}
	}
	size := func(dir string) int64 { in, _ := Stat(dir); return in.Bytes }
	all := size(oldO) + size(newO) + size(keepO) + size(bigLive) + size(smallLive)

	// Just under the total: only the oldest-closed orphan goes (not the kept one).
	rem, err := EnforceTotal(root, all-size(oldO)/2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rem) != 1 || rem[0].Dir != oldO || !rem[0].Whole {
		t.Fatalf("removed %+v", rem)
	}
	if _, err := os.Stat(oldO); !os.IsNotExist(err) {
		t.Fatal("old orphan still there")
	}

	// Well under: the other orphan goes too, kept orphan stays, then the live
	// log with the most bytes loses oldest segments (big, not small).
	bigBefore, smallBefore := size(bigLive), size(smallLive)
	target := size(keepO) + size(smallLive) + size(bigLive)/2
	rem, err = EnforceTotal(root, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rem[0].Dir != newO || !rem[0].Whole {
		t.Fatalf("first victim %+v", rem[0])
	}
	if _, err := os.Stat(keepO); err != nil {
		t.Fatal("kept orphan was removed")
	}
	if size(smallLive) != smallBefore {
		t.Fatal("small live log trimmed before the big one")
	}
	if size(bigLive) >= bigBefore || len(rem) < 2 || rem[1].Whole {
		t.Fatalf("big live not trimmed by segments: %+v", rem)
	}
	var total int64
	logs, _ := FindLogs(root)
	for _, in := range logs {
		total += in.Bytes
	}
	if total > target {
		t.Fatalf("total %d over %d", total, target)
	}
	// What is left of the big log still reads from its first remaining row.
	rd, _ := OpenReader(bigLive, 2)
	if rd.First() == 0 || rd.Next() != 600 {
		t.Fatalf("big extent %d..%d", rd.First(), rd.Next())
	}
}

func TestTotalCapUsesLiveLogAndSkipsLocked(t *testing.T) {
	rows := corpus(t, 300)
	root := t.TempDir()
	dir := filepath.Join(root, "s/a.cold")
	l := openLog(t, dir, Options{SegmentRows: 32})
	for i := range 300 {
		l.Append(rows[i])
	}
	l.Flush()
	// A locked log without a Trimmer (another process owns it) is skipped.
	rem, err := EnforceTotal(root, 1, nil)
	if err != nil || len(rem) != 0 {
		t.Fatalf("locked log trimmed: %v %v", rem, err)
	}
	rem, err = EnforceTotal(root, 1, func(d string) Trimmer {
		if d == dir {
			return l
		}
		return nil
	})
	if err != nil || len(rem) == 0 {
		t.Fatalf("live trim: %v %v", rem, err)
	}
	st := l.Stats()
	if st.Segments != 1 || st.First == 0 {
		t.Fatalf("stats %+v (only the open segment should be left)", st)
	}
	checkRow(t, l.Reader(), 299, rows[299])
	l.Close()
}

// stall is a File whose Write blocks until released.
type stall struct {
	File
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *stall) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.File.Write(p)
}

func TestFullQueueDropsWithGap(t *testing.T) {
	rows := corpus(t, 200)
	entered, release := make(chan struct{}), make(chan struct{})
	create := func(p string) (File, error) {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		return &stall{File: f, entered: entered, release: release}, err
	}
	l := openLog(t, t.TempDir(), Options{QueueRows: 10, BlockRows: 4, Create: create})
	l.Append(rows[0])
	l.Append(rows[1])
	l.Append(rows[2])
	l.Append(rows[3]) // block full: the writer enters the stalled header write
	<-entered
	start := time.Now()
	for i := 4; i < 100; i++ {
		l.Append(rows[i]) // must not block
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Append blocked on a stalled disk")
	}
	dropped := l.Dropped()
	if dropped == 0 || dropped > 90 {
		t.Fatalf("dropped = %d", dropped)
	}
	close(release)
	l.Flush()
	for i := 100; i < 120; i++ { // rows after the gap keep their ids
		l.Append(rows[i])
		if i%5 == 4 {
			l.Flush() // keep the queue from filling again
		}
	}
	l.Flush()
	dropped = l.Dropped()
	rd := l.Reader()
	if rd.Next() != 120 {
		t.Fatalf("next = %d, want 120 (gap ids count)", rd.Next())
	}
	var gaps, ok int
	for i := range 120 {
		row, err := rd.Row(uint64(i))
		switch {
		case errors.Is(err, ErrGap):
			gaps++
		case err != nil:
			t.Fatalf("row %d: %v", i, err)
		default:
			ok++
			if string(row.Data) != string(rows[i].Data) {
				t.Fatalf("row %d holds another row's data", i)
			}
		}
	}
	if uint64(gaps) != dropped || ok != 120-gaps {
		t.Fatalf("gaps %d dropped %d ok %d", gaps, dropped, ok)
	}
	var sum uint64
	for _, g := range rd.Stats().Gaps {
		sum += g.N
	}
	if sum != dropped {
		t.Fatalf("recorded gaps cover %d, dropped %d", sum, dropped)
	}
	l.Close()
}

// failAfter fails writes once n bytes went through.
type failAfter struct {
	File
	left int
}

func (f *failAfter) Write(p []byte) (int, error) {
	if len(p) > f.left {
		return 0, syscall.ENOSPC
	}
	f.left -= len(p)
	return f.File.Write(p)
}

func TestDiskErrorDisablesLog(t *testing.T) {
	rows := corpus(t, 300)
	var reported []error
	var mu sync.Mutex
	create := func(p string) (File, error) {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		return &failAfter{File: f, left: 600}, err
	}
	l := openLog(t, t.TempDir(), Options{Create: create, OnError: func(err error) { mu.Lock(); reported = append(reported, err); mu.Unlock() }})
	for _, r := range rows {
		l.Append(r)
	}
	l.Flush()
	if !errors.Is(l.Err(), syscall.ENOSPC) {
		t.Fatalf("Err = %v", l.Err())
	}
	l.Append(rows[0]) // no panic, no block
	l.Flush()
	if err := l.Close(); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Close = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 1 {
		t.Fatalf("OnError called %d times", len(reported))
	}
	if l.Dropped() == 0 {
		t.Fatal("rows after the error not counted as dropped")
	}
	// Whatever reached the disk before the error is still readable.
	rd, err := OpenReader(l.dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	for id := rd.First(); id < rd.Next(); id++ {
		checkRow(t, rd, id, rows[id])
	}
}

func TestGraceAndKeepPurge(t *testing.T) {
	rows := corpus(t, 40)
	root := t.TempDir()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	mk := func(name string, closedDaysAgo float64, keep bool) string {
		dir := filepath.Join(root, "sess", name+".cold")
		l := openLog(t, dir, Options{})
		for _, r := range rows {
			l.Append(r)
		}
		err := l.CloseOrphan(Marker{ClosedAt: now.Add(-time.Duration(closedDaysAgo * 24 * float64(time.Hour))), Reason: "window-closed", Session: "sess", Window: name, Keep: keep}, now)
		if err != nil {
			t.Fatal(err)
		}
		return dir
	}
	old := mk("old", 4, false)
	edge := mk("edge", 2.9, false)
	kept := mk("kept", 30, true)
	live := openLog(t, filepath.Join(root, "sess", "live.cold"), Options{})
	live.Append(rows[0])
	live.Flush()
	defer live.Close()

	m, err := ReadMarker(old)
	if err != nil || m.Rows != 40 || m.Bytes == 0 || m.Window != "old" {
		t.Fatalf("marker %+v %v", m, err)
	}

	clock := now
	opt := PurgeOptions{Now: func() time.Time { return clock }, GraceDays: 3}
	dry := opt
	dry.DryRun = true
	got, err := PurgeOrphans(root, dry)
	if err != nil || len(got) != 1 || got[0].Dir != old {
		t.Fatalf("dry run: %+v %v", got, err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("dry run deleted")
	}
	got, _ = PurgeOrphans(root, opt)
	if len(got) != 1 || got[0].Dir != old {
		t.Fatalf("purge: %+v", got)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old not purged")
	}
	// Two days later the edge one's grace has ended; kept and live never.
	clock = now.Add(48 * time.Hour)
	got, _ = PurgeOrphans(root, opt)
	if len(got) != 1 || got[0].Dir != edge {
		t.Fatalf("second purge: %+v", got)
	}
	clock = now.Add(1000 * 24 * time.Hour)
	got, _ = PurgeOrphans(root, opt)
	if len(got) != 0 {
		t.Fatalf("kept/live purged: %+v", got)
	}
	if err := SetKeep(kept, false); err != nil {
		t.Fatal(err)
	}
	got, _ = PurgeOrphans(root, opt)
	if len(got) != 1 || got[0].Dir != kept {
		t.Fatalf("after unkeep: %+v", got)
	}
	// Grace 0 purges every unkept orphan at once; a live log is untouched.
	l2 := openLog(t, filepath.Join(root, "sess", "x.cold"), Options{})
	l2.Append(rows[0])
	l2.CloseOrphan(Marker{}, clock)
	got, _ = PurgeOrphans(root, PurgeOptions{Now: opt.Now})
	if len(got) != 1 || got[0].Bytes == 0 {
		t.Fatalf("grace 0: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(root, "sess", "live.cold")); err != nil {
		t.Fatal("live log purged")
	}
}

func TestStampUnmarkedAndReopenClearsMarker(t *testing.T) {
	rows := corpus(t, 10)
	root := t.TempDir()
	dir := fill(t, root, "s/a.cold", 10, rows)
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	opt := PurgeOptions{Now: func() time.Time { return now }, GraceDays: 3, StampUnmarked: true}
	if got, _ := PurgeOrphans(root, opt); len(got) != 0 {
		t.Fatalf("purged on first sight: %+v", got)
	}
	m, err := ReadMarker(dir)
	if err != nil || !m.ClosedAt.Equal(now) || m.Reason != "unmarked" {
		t.Fatalf("stamp: %+v %v", m, err)
	}
	opt.Now = func() time.Time { return now.Add(4 * 24 * time.Hour) }
	// A pane comes back under the old id before the sweep: the log is live.
	l := openLog(t, dir, Options{})
	if _, err := ReadMarker(dir); !os.IsNotExist(err) {
		t.Fatal("marker survived a writer opening the log")
	}
	if got, _ := PurgeOrphans(root, opt); len(got) != 0 {
		t.Fatalf("live log purged: %+v", got)
	}
	l.Close()
}

func TestConcurrentReadWhileAppending(t *testing.T) {
	rows := corpus(t, 400)
	l := openLog(t, t.TempDir(), Options{SegmentRows: 48, MaxRows: 250, FlushInterval: 5 * time.Millisecond})
	rd := l.Reader()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	errc := make(chan error, 4)
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				first, next := rd.First(), rd.Next()
				for id := next; id > first && id+64 > next; id-- {
					row, err := rd.Row(id - 1)
					switch {
					case err == nil:
						if string(row.Data) != string(rows[(id-1)%400].Data) {
							errc <- fmt.Errorf("row %d wrong data", id-1)
							return
						}
					case errors.Is(err, ErrTrimmed), errors.Is(err, ErrNotYet), errors.Is(err, ErrGap):
					default:
						errc <- fmt.Errorf("row %d: %w", id-1, err)
						return
					}
				}
			}
		}()
	}
	for round := 0; round < 5; round++ {
		for i := range rows {
			l.Append(rows[i])
		}
		time.Sleep(10 * time.Millisecond)
	}
	l.Flush()
	close(stop)
	wg.Wait()
	select {
	case err := <-errc:
		t.Fatal(err)
	default:
	}
	l.Close()
}

func TestClosedLogIgnoresAppend(t *testing.T) {
	rows := corpus(t, 5)
	l := openLog(t, t.TempDir(), Options{})
	l.Close()
	l.Append(rows[0])
	l.Flush()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if l.Dropped() != 1 {
		t.Fatalf("dropped = %d", l.Dropped())
	}
}

func TestCorruptBlockDetected(t *testing.T) {
	rows := corpus(t, 64)
	dir := t.TempDir()
	l := openLog(t, dir, Options{SegmentRows: 1000})
	for _, r := range rows {
		l.Append(r)
	}
	l.Close()
	files, _ := filepath.Glob(filepath.Join(dir, "seg-*"))
	b, _ := os.ReadFile(files[0])
	b[headerSize+blockHdrSize+5] ^= 0xff // inside the first payload
	os.WriteFile(files[0], b, 0o600)
	rd, err := OpenReader(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Row(0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("row 0: %v", err)
	}
	if _, err := rd.Row(40); err != nil {
		t.Fatalf("row 40 in an intact block: %v", err)
	}
}

func BenchmarkWriter(b *testing.B) { benchWriter(b, 0) }

func BenchmarkWriterBestSpeed(b *testing.B) { benchWriter(b, 1) }

func benchWriter(b *testing.B, level int) {
	rows := corpus(b, 4096)
	var bytesIn int
	for _, r := range rows {
		bytesIn += len(r.Data)
	}
	l, err := OpenLog(filepath.Join(b.TempDir(), "x.cold"), Options{SegmentRows: 4096, GzipLevel: level})
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Append(rows[i%len(rows)])
		if i%8192 == 8191 {
			l.Flush() // keep the queue from filling: measure the writer, not the drops
		}
	}
	l.Flush()
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "rows/s")
	b.ReportMetric(float64(bytesIn)/float64(len(rows)), "rawB/row")
	b.ReportMetric(float64(l.Stats().Bytes)/float64(b.N), "diskB/row")
	l.Close()
}

func BenchmarkAppendOnly(b *testing.B) {
	rows := corpus(b, 4096)
	l, err := OpenLog(filepath.Join(b.TempDir(), "x.cold"), Options{QueueRows: 1 << 30})
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Append(rows[i%len(rows)])
		if i%65536 == 65535 {
			b.StopTimer()
			l.Flush()
			b.StartTimer()
		}
	}
}

func BenchmarkReadWarmAndCold(b *testing.B) {
	rows := corpus(b, 4096)
	dir := filepath.Join(b.TempDir(), "x.cold")
	l, _ := OpenLog(dir, Options{})
	for i := 0; i < 20000; i++ {
		l.Append(rows[i%len(rows)])
		if i%8192 == 8191 {
			l.Flush()
		}
	}
	l.Close()
	rd, _ := OpenReader(dir, 8)
	b.Run("warm", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			rd.Row(uint64(1000 + i%200))
		}
	})
	b.Run("cold", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			rd.Row(uint64((i * 257) % 20000))
		}
	})
}

// FuzzSegmentBytes feeds arbitrary bytes as a segment file: opening, scanning
// and reading must never panic.
func FuzzSegmentBytes(f *testing.F) {
	rows := corpus(f, 40)
	dir := f.TempDir()
	l, _ := OpenLog(dir, small(Options{}))
	for _, r := range rows {
		l.Append(r)
	}
	l.Close()
	files, _ := filepath.Glob(filepath.Join(dir, "seg-*"))
	good, _ := os.ReadFile(files[0])
	f.Add(good)
	f.Add(good[:len(good)/2])
	f.Add(putHeader(0))
	f.Fuzz(func(t *testing.T, b []byte) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, segName(0)), b, 0o600)
		rd, err := OpenReader(d, 2)
		if err != nil {
			return
		}
		for id := rd.First(); id < rd.Next() && id < 1000; id++ {
			rd.Row(id)
		}
		if w, err := OpenLog(d, small(Options{})); err == nil {
			w.Close()
		}
	})
}

// Skip makes the next ids a gap and Next reports the id the next row gets,
// counting rows queued and gaps queued: what a restored pane needs to line
// the log's ids up with its ring.
func TestSkipAndNext(t *testing.T) {
	dir := t.TempDir()
	rows := corpus(t, 30)
	l, err := OpenLog(dir, small(Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if n := l.Next(); n != 0 {
		t.Fatalf("empty log: next %d", n)
	}
	l.Skip(100)
	if n := l.Next(); n != 100 {
		t.Fatalf("after skipping 100: next %d", n)
	}
	for _, r := range rows[:10] {
		l.Append(r)
	}
	l.Skip(5)
	if n := l.Next(); n != 115 {
		t.Fatalf("next %d, want 115", n)
	}
	for _, r := range rows[10:12] {
		l.Append(r)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenReader(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	st := r.Stats()
	if st.First != 100 || st.Next != 117 || st.Rows != 12 || len(st.Gaps) != 1 || st.Gaps[0] != (Gap{110, 5}) {
		t.Fatalf("stats %+v", st)
	}
	if got, err := r.Row(115); err != nil || string(got.Data) != string(rows[10].Data) {
		t.Fatalf("row 115: %v", err)
	}
	// Reopening continues after the last row.
	l2, err := OpenLog(dir, small(Options{}))
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if n := l2.Next(); n != 117 {
		t.Fatalf("reopened: next %d", n)
	}
}
