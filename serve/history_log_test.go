package serve_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// logsOf lists the history logs of the daemon, by id.
func (h *host) logsOf(state string) map[string]map[string]any {
	h.t.Helper()
	res := h.must("list-history-logs", map[string]any{"state": state})
	out := map[string]map[string]any{}
	for _, l := range res["logs"].([]any) {
		m := l.(map[string]any)
		out[m["log"].(string)] = m
	}
	return out
}

// waitLog waits for the live log of a window to hold at least rows rows.
func (h *host) waitLog(id string, rows float64) map[string]any {
	h.t.Helper()
	var last map[string]any
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if last = h.logsOf("all")[id]; last != nil && last["rows"].(float64) >= rows {
			return last
		}
	}
	_ = filepath.Walk(h.base, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.HasSuffix(p, ".log") {
			b, _ := os.ReadFile(p)
			h.t.Logf("%s:\n%s", p, b)
		}
		return nil
	})
	h.t.Fatalf("log %s never held %v rows; last %v", id, rows, last)
	return nil
}

// flood prints rows lines in the window, 5,000 at a time so the log's writer
// keeps up (a log drops rows, and records the gap, when output outruns it), and
// waits for the last one.
func (h *host) flood(sess, win string, rows int) {
	h.t.Helper()
	h.must("send-text", map[string]any{"session": sess, "window": win, "text": "i=0; while [ $i -lt " + itoa(rows) + " ]; do seq $((i+1)) $((i+5000)); i=$((i+5000)); sleep 0.25; done; echo done-" + itoa(rows) + "\n"})
	h.waitPane(sess, win, "done-"+itoa(rows), 60*time.Second)
}

func itoa(n int) string {
	var b [20]byte
	i := len(b)
	for ; n > 0; n /= 10 {
		i--
		b[i] = byte('0' + n%10)
	}
	return string(b[i:])
}

// The history-log verbs over the daemon's socket: list, show, keep, purge with
// its dry run and its confirm token, and adopt, from the window's side and the
// log's.
func TestHistoryLogVerbs(t *testing.T) {
	h := newHost(t)
	h.start()
	gone := h.newShell("hl", "gone")
	time.Sleep(500 * time.Millisecond)
	h.flood("hl", gone, 40000)
	id := "hl/" + gone
	live := h.waitLog(id, 25000)
	if live["state"] != "live" {
		t.Fatalf("a pane's log is %v", live["state"])
	}
	if shown := h.must("show-history-log", map[string]any{"log": id}); shown["window"] != gone || shown["session"] != "hl" {
		t.Fatalf("show: %v", shown)
	}
	// A live log cannot be kept, purged or adopted.
	for _, c := range []struct {
		verb   string
		params map[string]any
	}{
		{"keep-history-log", map[string]any{"log": id}},
		{"purge-history-logs", map[string]any{"log": id, "dry_run": true}},
	} {
		if _, err := h.call(c.verb, c.params); err == nil {
			t.Fatalf("%s accepted a live log", c.verb)
		}
	}

	h.must("close-window", map[string]any{"session": "hl", "window": gone})
	orphan := h.logsOf("orphaned")[id]
	if orphan == nil || orphan["reason"] != "window-closed" || orphan["days_left"].(float64) < 2.9 {
		t.Fatalf("after the close: %v", orphan)
	}
	rows := orphan["rows"].(float64)

	// Keep, then a dry run that skips it.
	h.must("keep-history-log", map[string]any{"log": id})
	dry := h.must("purge-history-logs", map[string]any{"all_orphaned": true, "dry_run": true})
	if len(dry["purged"].([]any)) != 0 {
		t.Fatalf("a kept log would be purged: %v", dry)
	}
	h.must("unkeep-history-log", map[string]any{"log": id})
	dry = h.must("purge-history-logs", map[string]any{"all_orphaned": true, "dry_run": true})
	token, _ := dry["confirm"].(string)
	if dry["dry_run"] != true || len(dry["purged"].([]any)) != 1 || token == "" || h.logsOf("all")[id] == nil {
		t.Fatalf("dry run: %v", dry)
	}

	// A new window takes the log over, from either side.
	fresh := h.must("new-window", map[string]any{"session": "hl", "name": "fresh", "focus": false, "command": []string{"sh"}})["window_id"].(string)
	time.Sleep(500 * time.Millisecond)
	if _, err := h.call("adopt-history-log", map[string]any{"log": "hl/nothing", "session": "hl", "window": fresh}); err == nil {
		t.Fatal("adopted a log that does not exist")
	}
	res := h.must("adopt-history", map[string]any{"session": "hl", "window": fresh, "log": id})
	newID := "hl/" + fresh
	if res["log"] != newID || res["from"] != id || res["rows"].(float64) != rows {
		t.Fatalf("adopt: %v", res)
	}
	if all := h.logsOf("all"); all[id] != nil || all[newID] == nil || all[newID]["state"] != "live" {
		t.Fatalf("after the adopt: %v", all)
	}
	// Its new window's output follows the old rows.
	h.flood("hl", fresh, 30000)
	if after := h.waitLog(newID, rows+10000); after["gaps"] != nil {
		t.Fatalf("gaps in the adopted log: %v", after)
	}

	// Purge: no token, nothing goes; the token from a dry run does it.
	h.must("close-window", map[string]any{"session": "hl", "window": fresh})
	if _, err := h.call("purge-history-logs", map[string]any{"all_orphaned": true}); err == nil || !strings.Contains(err.Error(), "confirm_required") {
		t.Fatalf("purge without a token: %v", err)
	}
	if h.logsOf("orphaned")[newID] == nil {
		t.Fatal("deleted without a token")
	}
	dry = h.must("purge-history-logs", map[string]any{"all_orphaned": true, "dry_run": true})
	done := h.must("purge-history-logs", map[string]any{"all_orphaned": true, "confirm": dry["confirm"]})
	if len(done["purged"].([]any)) != 1 || len(h.logsOf("all")) != 0 {
		t.Fatalf("purge: %v, left %v", done, h.logsOf("all"))
	}
}

// A clean daemon stop and start brings the pane back with its whole history:
// the log is the same, and what the new daemon holds in memory starts where
// the log ends.
func TestHistoryLogSurvivesADaemonRestart(t *testing.T) {
	h := newHost(t)
	pid := h.start()
	win := h.newShell("rs", "w1")
	time.Sleep(500 * time.Millisecond)
	h.flood("rs", win, 60000)
	id := "rs/" + win
	before := h.waitLog(id, 40000)
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 200 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatal("the daemon did not stop")
	}
	h.start()
	after := h.waitLog(id, before["rows"].(float64))
	if after["state"] != "live" || after["window"] != win {
		t.Fatalf("after the restart: %v", after)
	}
	// The log kept what was in memory too: it ends where the saved history starts.
	if after["next_row"].(float64) < 60000-6000-300 {
		t.Fatalf("log ends at %v", after["next_row"])
	}
	// And it carries on from there.
	h.flood("rs", win, 30000)
	later := h.waitLog(id, after["rows"].(float64)+10000)
	if later["gaps"] != nil {
		t.Fatalf("gaps after the restart: %v", later)
	}
}

// A live upgrade carries 1000 rows of each ring. The rows older than that that
// no log held yet go to the log first.
func TestHistoryLogSurvivesALiveUpgrade(t *testing.T) {
	h := newHost(t)
	h.start()
	win := h.newShell("up", "w1")
	time.Sleep(500 * time.Millisecond)
	// 15,000 rows: 5,000 in the log, 10,000 in the ring.
	h.flood("up", win, 15000)
	id := "up/" + win
	h.waitLog(id, 1000)
	before := h.must("hello", map[string]any{})
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
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if res, err := h.call("hello", map[string]any{}); err == nil && res["instance"] != before["instance"] {
			break
		}
	}
	// 15,000 rows, the screen and the command's own line, less the 1,000 the
	// successor holds.
	log := h.waitLog(id, 10000)
	if next := log["next_row"].(float64); next < 15000-1000-100 || next > 15000-1000+100 || log["gaps"] != nil {
		t.Fatalf("after the upgrade: %v", log)
	}
	// The successor keeps 1,000 carried rows; 15,000 more push 6,000 out.
	h.flood("up", win, 15000)
	if later := h.waitLog(id, 19000); later["gaps"] != nil {
		t.Fatalf("gaps after the upgrade: %v", later)
	}
}
