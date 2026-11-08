package serve_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"ofam.dev/mtmd/internal/coldlog"
	"ofam.dev/mtmd/internal/vt"
)

// Lane scrollback-m7 benchmarks and soak, against a real daemon subprocess.
// Gated by M7_SOAK=1. Results are logged as "M7 ..." lines.

func m7Config(h *host, toml string) {
	_ = os.MkdirAll(filepath.Join(h.base, "config", "tuios"), 0o700)
	_ = os.WriteFile(filepath.Join(h.base, "config", "tuios", "config.toml"), []byte(toml), 0o600)
}

func m7Gate(t *testing.T) {
	if os.Getenv("M7_SOAK") != "1" {
		t.Skip("M7_SOAK not set")
	}
}

// m7Rows is a shell command that prints n numbered rows as fast as it can.
func m7Rows(n int, colour bool) string {
	if colour {
		return `awk 'BEGIN{for(i=0;i<` + strconv.Itoa(n) + `;i++) printf "\033[3%dm%07d\033[0m the quick brown fox %d jumps\r\n", i%7+1, i, i*7919}'`
	}
	return `awk 'BEGIN{for(i=0;i<` + strconv.Itoa(n) + `;i++) printf "R%07d the quick brown fox jumps\r\n", i}'`
}

func (h *host) run(sess, win, cmd, done string, d time.Duration) time.Duration {
	h.t.Helper()
	t0 := time.Now()
	h.must("send-text", map[string]any{"session": sess, "window": win, "text": cmd + "; echo " + done + "-$((6*7))\n"})
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(25 * time.Millisecond) {
		if strings.Contains(h.capture(sess, win), done+"-42") {
			return time.Since(t0)
		}
	}
	h.t.Fatalf("%s never finished", done)
	return 0
}

// coldDirOf finds the pane's log directory on disk.
func (h *host) coldDirOf(win string) string {
	var found string
	_ = filepath.Walk(filepath.Join(h.base, "state"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.IsDir() && filepath.Base(p) == win+".cold" {
			found = p
		}
		return nil
	})
	return found
}

func TestM7Throughput(t *testing.T) {
	m7Gate(t)
	const rows = 1500000
	// The rows come from a file, so the shell and awk are not what is timed.
	corpus := filepath.Join(os.TempDir(), "m7-rows.txt")
	if out, err := exec.Command("sh", "-c", strings.Replace(m7Rows(rows, true), "\r\n", "\n", 1)+" > "+corpus).CombinedOutput(); err != nil {
		t.Fatalf("corpus: %v %s", err, out)
	}
	t.Cleanup(func() { _ = os.Remove(corpus) })
	res := map[string][]float64{}
	for _, rep := range []int{1, 2, 3} {
		for _, disk := range []bool{false, true} {
			name := map[bool]string{false: "off", true: "on"}[disk]
			t.Run(fmt.Sprintf("%s-%d", name, rep), func(t *testing.T) {
				h := newHost(t)
				m7Config(h, fmt.Sprintf("[appearance]\nscrollback_lines = 10000\n[daemon]\nscrollback_disk = %v\n", disk))
				pid := h.start()
				win := h.newShell("tp", "w")
				time.Sleep(500 * time.Millisecond)
				// A warm-up run, then the timed one.
				h.run("tp", win, "head -n 200000 "+corpus, "WARM", 120*time.Second)
				d := h.run("tp", win, "cat "+corpus, "FLOOD", 300*time.Second)
				res[name] = append(res[name], float64(rows)/d.Seconds()/1000)
				t.Logf("M7 throughput disk=%s rep=%d: %.2fs, %.0f k rows/s, %s", name, rep, d.Seconds(), float64(rows)/d.Seconds()/1000, m7Footprint(pid))
				if disk {
					time.Sleep(3 * time.Second) // let the writer finish
					for _, l := range h.logsOf("all") {
						t.Logf("M7 writer: log rows=%v next_row=%v gaps=%v bytes=%v (flood %d rows after warm-up 200000; ring 10000)", l["rows"], l["next_row"], l["gaps"], l["bytes"], rows)
					}
				}
			})
		}
	}
	for _, k := range []string{"off", "on"} {
		sort.Float64s(res[k])
		t.Logf("M7 throughput summary disk=%s: median %.0f k rows/s (%v)", k, res[k][1], res[k])
	}
	t.Logf("M7 slowdown with the hook: %.1f%%", (1-res["on"][1]/res["off"][1])*100)
}

func TestM7ColdRead(t *testing.T) {
	m7Gate(t)
	h := newHost(t)
	m7Config(h, "[appearance]\nscrollback_lines = 10000\n")
	h.start()
	win := h.newShell("cr", "w")
	time.Sleep(500 * time.Millisecond)
	h.run("cr", win, m7Rows(1500000, false), "FLOOD", 300*time.Second)
	time.Sleep(3 * time.Second)
	h.waitLog("cr/"+win, 1400000)
	var lat []float64
	for i := 0; i < 30; i++ {
		start := 1 + (i*47123)%1300000
		t0 := time.Now()
		res := h.must("capture-pane", map[string]any{"session": "cr", "window": win, "source": "recent", "start": start, "end": start + 199})
		lat = append(lat, float64(time.Since(t0).Microseconds())/1000)
		c, _ := res["content"].(string)
		if !strings.Contains(c, "R") || strings.Count(c, "\n") < 150 {
			t.Fatalf("capture at %d short: %q", start, c[:min(len(c), 80)])
		}
	}
	sort.Float64s(lat)
	t.Logf("M7 cold read 200 lines (deep, uncached blocks): min %.1f ms median %.1f ms p90 %.1f ms max %.1f ms", lat[0], lat[len(lat)/2], lat[len(lat)*9/10], lat[len(lat)-1])
	// The same window again hits the block cache.
	t0 := time.Now()
	h.must("capture-pane", map[string]any{"session": "cr", "window": win, "source": "recent", "start": 1000, "end": 1199})
	t.Logf("M7 first line window again (cached or not): %.1f ms", float64(time.Since(t0).Microseconds())/1000)
	// Search for a row only the disk holds, oldest end.
	t0 = time.Now()
	sr := h.must("search-panes", map[string]any{"query": "R0000042 "})
	t.Logf("M7 search-panes (row 42 of 1.5M, newest first): %.0f ms, complete=%v truncated=%v", float64(time.Since(t0).Milliseconds()), sr["complete"], sr["truncated"])
	t0 = time.Now()
	sr = h.must("search-panes", map[string]any{"query": "R1499000 "})
	t.Logf("M7 search-panes (row 1499000, near the ring): %.0f ms, complete=%v", float64(time.Since(t0).Milliseconds()), sr["complete"])
	res := h.must("capture-pane", map[string]any{"session": "cr", "window": win, "source": "recent"})
	c, _ := res["content"].(string)
	t0 = time.Now()
	h.must("capture-pane", map[string]any{"session": "cr", "window": win, "source": "recent"})
	t.Logf("M7 whole-history capture: %d bytes, %.0f ms", len(c), float64(time.Since(t0).Milliseconds()))
}

func TestM7DiskFull(t *testing.T) {
	m7Gate(t)
	if os.Getuid() != 0 || !strings.Contains(runtimeGOOS(), "linux") {
		t.Skip("needs root on Linux to mount a tiny tmpfs")
	}
	h := newHost(t)
	state := filepath.Join(h.base, "state")
	if out, err := exec.Command("mount", "-t", "tmpfs", "-o", "size=3m", "tmpfs", state).CombinedOutput(); err != nil {
		t.Skipf("mount: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("umount", "-l", state).Run() })
	m7Config(h, "[appearance]\nscrollback_lines = 10000\n")
	h.start()
	win := h.newShell("df", "w")
	other := h.must("new-window", map[string]any{"session": "df", "name": "o", "focus": false, "command": []string{"sh"}})["window_id"].(string)
	time.Sleep(500 * time.Millisecond)
	d := h.run("df", win, m7Rows(3000000, true), "FLOOD", 300*time.Second)
	t.Logf("M7 disk-full: flood of 3M rows finished in %.1fs", d.Seconds())
	time.Sleep(2 * time.Second)
	for _, l := range h.logsOf("all") {
		t.Logf("M7 disk-full log: %v", l)
	}
	// The pane and the daemon work: more output, another pane, the ring.
	h.run("df", win, "echo after-the-disk-filled", "AFTER", 30*time.Second)
	h.run("df", other, "echo second-pane-ok", "OTHER", 30*time.Second)
	res := h.must("get-window", map[string]any{"session": "df", "window": win})
	t.Logf("M7 disk-full: history_rows in memory %v; df: %s", res["history_rows"], func() string {
		b, _ := exec.Command("df", "-h", state).Output()
		return strings.ReplaceAll(string(b), "\n", " | ")
	}())
	if c := h.capture("df", win); !strings.Contains(c, "AFTER-42") {
		t.Fatal("pane stopped working")
	}
}

func runtimeGOOS() string { return strings.ToLower(osName()) }

func osName() string {
	b, _ := exec.Command("uname", "-s").Output()
	return strings.TrimSpace(string(b))
}

// readCold reads every row of a cold log directory and checks the numbered
// sequence R%07d is contiguous from the first row; it returns the counts.
func readCold(t *testing.T, dir string, prefix string) (rows, gaps int, first, next uint64) {
	t.Helper()
	r, err := coldlog.OpenReader(dir, 4)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	st := r.Stats()
	first, next = st.First, st.Next
	for _, g := range st.Gaps {
		gaps += int(g.N)
	}
	var prev = -1
	for id := first; id < next; id++ {
		row, err := r.Row(id)
		if err != nil {
			continue // a gap
		}
		txt := vt.ScrollbackRowText(row.Data)
		if !strings.HasPrefix(txt, prefix) || len(strings.Fields(txt)[0]) != 8 {
			continue // the shell's echo of the command, before the numbers
		}
		rows++
		n, _ := strconv.Atoi(strings.Fields(txt)[0][len(prefix):])
		if prev >= 0 && n != prev+1 {
			t.Fatalf("row %d holds %d after %d", id, n, prev)
		}
		prev = n
	}
	return rows, gaps, first, next
}

func TestM7Kill9DuringFlood(t *testing.T) {
	m7Gate(t)
	h := newHost(t)
	m7Config(h, "[appearance]\nscrollback_lines = 10000\n")
	pid := h.start()
	win := h.newShell("k9", "w")
	time.Sleep(500 * time.Millisecond)
	// One continuous number sequence, 4M rows.
	h.must("send-text", map[string]any{"session": "k9", "window": win, "text": m7Rows(4000000, false) + "\n"})
	time.Sleep(4 * time.Second)
	dir := h.coldDirOf(win)
	if dir == "" {
		t.Fatal("no log directory")
	}
	// Rows evicted so far, from the screen: the last number printed.
	last := func() int {
		c := h.capture("k9", win)
		n := -1
		for _, f := range strings.Fields(c) {
			if strings.HasPrefix(f, "R") && len(f) == 8 {
				if v, err := strconv.Atoi(f[1:]); err == nil && v > n {
					n = v
				}
			}
		}
		return n
	}
	before := last()
	_ = syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(500 * time.Millisecond)
	h.start()
	time.Sleep(2 * time.Second)
	rows, gaps, first, next := readCold(t, dir, "R")
	t.Logf("M7 kill -9 during flood: last row seen on screen about R%07d; after restart the log holds %d rows (ids %d..%d), %d in gaps, contiguous numbers, readable", before, rows, first, next, gaps)
	t.Logf("M7 kill -9: rows evicted at the time of the kill were at least %d (screen number minus ring 10000 minus screen 24 minus the poll lag); on disk %d", before-10024, next)
	// The pane's log is an orphan now and the daemon takes new work.
	win2 := h.newShell("k9b", "w")
	time.Sleep(500 * time.Millisecond)
	h.run("k9b", win2, "echo alive", "ALIVE", 30*time.Second)
	for id, l := range h.logsOf("all") {
		t.Logf("M7 kill -9 log %s: state=%v rows=%v gaps=%v reason=%v", id, l["state"], l["rows"], l["gaps"], l["reason"])
	}
}

func TestM7HandoffUnderFlood(t *testing.T) {
	m7Gate(t)
	h := newHost(t)
	m7Config(h, "[appearance]\nscrollback_lines = 10000\n")
	h.start()
	win := h.newShell("ho", "w")
	time.Sleep(500 * time.Millisecond)
	before := h.must("hello", map[string]any{})
	second := filepath.Join(h.base, "bin", "host2")
	data, _ := os.ReadFile(h.exe)
	_ = os.WriteFile(second, data, 0o755)
	// 100 bursts of 5000 numbered rows with a short pause: about 12 s.
	h.must("send-text", map[string]any{"session": "ho", "window": win, "text": `i=0; while [ $i -lt 500000 ]; do awk -v s=$i 'BEGIN{for(j=0;j<5000;j++) printf "R%07d the quick brown fox jumps\r\n", s+j}'; i=$((i+5000)); sleep 0.1; done; echo HFLOOD-$((6*7))` + "\n"})
	time.Sleep(3 * time.Second)
	t0 := time.Now()
	if _, err := h.call("test-handoff", map[string]any{"binary": second}); err != nil {
		t.Fatalf("test-handoff: %v", err)
	}
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if res, err := h.call("hello", map[string]any{}); err == nil && res["instance"] != before["instance"] {
			break
		}
	}
	t.Logf("M7 handoff under flood: successor answered after %.1fs", time.Since(t0).Seconds())
	h.waitPane("ho", win, "HFLOOD-42", 120*time.Second)
	time.Sleep(3 * time.Second)
	dir := h.coldDirOf(win)
	rows, gaps, first, next := readCold(t, dir, "R")
	t.Logf("M7 handoff under flood: log holds %d rows (ids %d..%d), %d in gaps, numbers contiguous", rows, first, next, gaps)
	res := h.must("capture-pane", map[string]any{"session": "ho", "window": win, "source": "recent"})
	c, _ := res["content"].(string)
	var seq []int
	for _, ln := range strings.Split(c, "\n") {
		if strings.HasPrefix(ln, "R") && len(ln) > 8 {
			if v, err := strconv.Atoi(ln[1:8]); err == nil {
				seq = append(seq, v)
			}
		}
	}
	breaks, missing := 0, 0
	for i := 1; i < len(seq); i++ {
		if seq[i] != seq[i-1]+1 {
			breaks++
			missing += seq[i] - seq[i-1] - 1
		}
	}
	t.Logf("M7 handoff under flood: whole capture (disk+ring) %d numbered lines, first R%07d last R%07d, %d breaks, %d missing numbers; history_rows_disk=%v history_rows=%v", len(seq), seq[0], seq[len(seq)-1], breaks, missing, res["history_rows_disk"], res["history_rows"])
}

func TestM7GracePurgeAfterThreeDays(t *testing.T) {
	m7Gate(t)
	h := newHost(t)
	m7Config(h, "[appearance]\nscrollback_lines = 10000\n[daemon]\nscrollback_grace_days = 3\n")
	pid := h.start()
	win := h.newShell("gp", "w")
	time.Sleep(500 * time.Millisecond)
	h.run("gp", win, m7Rows(100000, false), "F", 60*time.Second)
	id := "gp/" + win
	h.waitLog(id, 50000)
	h.must("close-window", map[string]any{"session": "gp", "window": win})
	dir := h.coldDirOf(win)
	var marker string
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.HasSuffix(p, ".json") {
			marker = p
		}
		return nil
	})
	if marker == "" {
		t.Fatalf("no marker in %s", dir)
	}
	b, _ := os.ReadFile(marker)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	set := func(ago time.Duration) {
		m["closed_at"] = time.Now().Add(-ago).UTC().Format(time.RFC3339Nano)
		nb, _ := json.Marshal(m)
		_ = os.WriteFile(marker, nb, 0o600)
	}
	restart := func(pid int) int {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		for i := 0; i < 200 && syscall.Kill(pid, 0) == nil; i++ {
			time.Sleep(50 * time.Millisecond)
		}
		return h.start()
	}
	// 2 days: survives the sweep at start; 3 days and a bit: purged.
	set(48 * time.Hour)
	pid = restart(pid)
	time.Sleep(2 * time.Second)
	l := h.logsOf("all")[id]
	t.Logf("M7 grace: closed 2 days ago, after a restart: %v", l != nil)
	if l == nil {
		t.Fatal("a log inside its grace was purged")
	}
	set(73 * time.Hour)
	restart(pid)
	time.Sleep(2 * time.Second)
	_, err := os.Stat(dir)
	t.Logf("M7 grace: closed 73 hours ago, after a restart: log listed=%v, directory exists=%v", h.logsOf("all")[id] != nil, err == nil)
	if h.logsOf("all")[id] != nil || err == nil {
		t.Fatal("a log past its grace was not purged")
	}
}

// Restore with a 1M-row log on disk costs what a restore without a log costs.
func TestM7RestoreCost(t *testing.T) {
	m7Gate(t)
	h := newHost(t)
	m7Config(h, "[appearance]\nscrollback_lines = 10000\n")
	pid := h.start()
	win := h.newShell("rc", "w")
	time.Sleep(500 * time.Millisecond)
	h.run("rc", win, m7Rows(1100000, false), "FLOOD", 300*time.Second)
	time.Sleep(3 * time.Second)
	h.waitLog("rc/"+win, 1000000)
	dir := h.coldDirOf(win)
	stop := func(pid int) {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		for i := 0; i < 400 && syscall.Kill(pid, 0) == nil; i++ {
			time.Sleep(50 * time.Millisecond)
		}
	}
	stop(pid)
	backup := dir + ".bak"
	if err := os.Rename(dir, backup); err != nil {
		t.Fatal(err)
	}
	restart := func(label string) {
		reads := coldlog.BlockReads()
		t0 := time.Now()
		out, err := h.cmd("start-server").CombinedOutput()
		if err != nil {
			t.Fatalf("start-server: %v %s", err, out)
		}
		hello := h.must("hello", map[string]any{})
		up := time.Since(t0)
		newPid := int(hello["pid"].(float64))
		// The pane is back with its tail.
		h.waitPane("rc", win, "R", 30*time.Second)
		ready := time.Since(t0)
		time.Sleep(3 * time.Second)
		res := h.must("get-window", map[string]any{"session": "rc", "window": win})
		t.Logf("M7 restore %s: daemon answers after %.0f ms, pane readable after %.0f ms; history_rows=%v; %s", label, float64(up.Milliseconds()), float64(ready.Milliseconds()), res["history_rows"], m7Footprint(newPid))
		_ = reads
		stop(newPid)
	}
	// Without a log (the saved tail only), then with the 1M-row log back.
	restart("without a log")
	_ = os.RemoveAll(dir)
	if err := os.Rename(backup, dir); err != nil {
		t.Fatal(err)
	}
	restart("with a 1M-row log")
	restart("with a 1M-row log, second restart")
}
