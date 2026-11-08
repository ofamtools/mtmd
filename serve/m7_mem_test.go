package serve_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"os/signal"
)

// Lane scrollback-m7 measurements. The daemon is a real subprocess (this test
// binary running itself as the host, see TestMain). When M7_DUMP_DIR is set in
// its environment the daemon process writes its Go memory statistics and a heap
// profile on SIGUSR1, so the test can compare the Go view with the OS view.
func init() {
	dir := os.Getenv("M7_DUMP_DIR")
	if dir == "" || os.Getenv(hostEnv) != "1" {
		return
	}
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		n := 0
		for range ch {
			n++
			if os.Getenv("M7_GC") == "1" {
				runtime.GC() // profile and stats then show live data only
			}
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			b, _ := json.Marshal(map[string]uint64{
				"heap_alloc": m.HeapAlloc, "heap_inuse": m.HeapInuse, "heap_idle": m.HeapIdle,
				"heap_released": m.HeapReleased, "heap_sys": m.HeapSys, "sys": m.Sys,
				"stack_inuse": m.StackInuse, "mspan_inuse": m.MSpanInuse, "gc_sys": m.GCSys,
				"other_sys": m.OtherSys, "num_gc": uint64(m.NumGC),
			})
			_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("mem-%d.json", n)), b, 0o644)
			if f, err := os.Create(filepath.Join(dir, fmt.Sprintf("heap-%d.pb", n))); err == nil {
				_ = pprof.Lookup("heap").WriteTo(f, 0)
				_ = f.Close()
			}
		}
	}()
}

// m7Footprint is the OS's idea of the process's memory in MB: footprint(1) on
// macOS, VmRSS and the private dirty total from smaps_rollup on Linux.
func m7Footprint(pid int) string {
	if runtime.GOOS == "darwin" {
		b, err := exec.Command("footprint", "-p", strconv.Itoa(pid)).CombinedOutput()
		if err != nil {
			return "footprint: " + err.Error()
		}
		for _, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, "Footprint:") {
				return strings.TrimSpace(l)
			}
		}
		return "?"
	}
	var rss, pd string
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "VmRSS:") {
				rss = strings.Join(strings.Fields(l)[1:], " ")
			}
		}
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid)); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "Private_Dirty:") {
				pd = strings.Join(strings.Fields(l)[1:], " ")
			}
		}
	}
	return "VmRSS " + rss + ", Private_Dirty " + pd
}

func TestM7Footprint(t *testing.T) {
	if os.Getenv("M7_MEM") != "1" {
		t.Skip("M7_MEM not set")
	}
	panes, rows := 10, 100000
	if n, err := strconv.Atoi(os.Getenv("M7_PANES")); err == nil {
		panes = n
	}
	h := newHost(t)
	dump := filepath.Join(h.base, "dump")
	_ = os.MkdirAll(dump, 0o700)
	h.env = append(h.env, "M7_DUMP_DIR="+dump)
	if v := os.Getenv("M7_GODEBUG"); v != "" {
		h.env = append(h.env, "GODEBUG="+v)
	}
	_ = os.MkdirAll(filepath.Join(h.base, "config", "tuios"), 0o700)
	cfg := fmt.Sprintf("[appearance]\nscrollback_lines = %d\n[daemon]\nscrollback_hot_when = [\"output\"]\nscrollback_hot_idle_seconds = 15\nscrollback_cold_ring_rows = 5000\n", rows)
	_ = os.WriteFile(filepath.Join(h.base, "config", "tuios", "config.toml"), []byte(cfg), 0o600)
	pid := h.start()
	sample := 0
	point := func(label string) {
		sample++
		_ = syscall.Kill(pid, syscall.SIGUSR1)
		time.Sleep(700 * time.Millisecond)
		b, _ := os.ReadFile(filepath.Join(dump, fmt.Sprintf("mem-%d.json", sample)))
		var m map[string]uint64
		_ = json.Unmarshal(b, &m)
		mb := func(k string) uint64 { return m[k] >> 20 }
		t.Logf("M7 %-28s | %s | go: inuse=%d idle=%d released=%d sys=%d alloc=%d MiB (heap profile heap-%d.pb)",
			label, m7Footprint(pid), mb("heap_inuse"), mb("heap_idle"), mb("heap_released"), mb("sys"), mb("heap_alloc"), sample)
		if keep := os.Getenv("M7_KEEP_DIR"); keep != "" {
			_ = os.MkdirAll(keep, 0o755)
			if data, err := os.ReadFile(filepath.Join(dump, fmt.Sprintf("heap-%d.pb", sample))); err == nil {
				_ = os.WriteFile(filepath.Join(keep, fmt.Sprintf("heap-%s.pb", strings.ReplaceAll(label, " ", "_"))), data, 0o644)
			}
		}
	}
	var wins []string
	for i := range panes {
		wins = append(wins, h.newShell(fmt.Sprintf("p%d", i), "w"))
	}
	time.Sleep(time.Second)
	point("empty, panes open")
	flood := `awk 'BEGIN{for(i=0;i<` + strconv.Itoa(rows) + `;i++) printf "\033[3%dm%07d\033[0m the quick brown fox %d jumps\r\n", i%7+1, i, i*7919}'; echo FLOOD-$((6*7))` + "\n"
	for i, w := range wins {
		h.must("send-text", map[string]any{"session": fmt.Sprintf("p%d", i), "window": w, "text": flood})
	}
	for i, w := range wins {
		h.waitPane(fmt.Sprintf("p%d", i), w, "FLOOD-42", 300*time.Second)
	}
	time.Sleep(3 * time.Second)
	point("all hot, flooded")
	// Keep pane 0 busy-ish (hot): print a line every 5 s so it stays hot while the others go cold.
	h.must("send-text", map[string]any{"session": "p0", "window": wins[0], "text": "while true; do echo tick; sleep 5; done\n"})
	var rowsOf func() string = func() string {
		var parts []string
		for i, w := range wins {
			r, _ := h.call("get-window", map[string]any{"session": fmt.Sprintf("p%d", i), "window": w})
			parts = append(parts, fmt.Sprint(r["history_rows"]))
		}
		return strings.Join(parts, ",")
	}
	t.Logf("M7 history_rows before: %s", rowsOf())
	for _, s := range []int{25, 45, 75} {
		time.Sleep(time.Duration(map[int]int{25: 25, 45: 20, 75: 30}[s]) * time.Second)
		point(fmt.Sprintf("demoted +%ds", s))
	}
	t.Logf("M7 history_rows after: %s", rowsOf())
	_ = debug.SetGCPercent
}
