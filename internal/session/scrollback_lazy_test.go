package session

import (
	"strings"
	"testing"
	"time"

	"ofam.dev/mtmd/internal/coldlog"
)

// The cold log is never read eagerly. A restart adopts it by its index only, an
// attach or a promotion to hot reads none of it, and older rows are read only
// when a capture or search asks for them, in blocks bounded by the request.
func TestColdLogIsNotReadByRestoreAttachOrPromotion(t *testing.T) {
	d1 := coldTestDaemon(t, coldTestPolicy(), 20000)
	sess, pty := coldTestPane(t, d1, "lazy", "win-1")
	sess.UpdateState(&SessionState{Name: "lazy", Width: 80, Height: 24,
		Windows: []WindowState{{ID: "win-1", PTYID: pty.ID, Width: 42, Height: 22}}})
	const total = 200000
	flood(pty, 0, total)
	d1.manager.Shutdown()

	// Restore.
	base := coldlog.BlockReads()
	d2 := coldTestDaemonHere(t, coldTestPolicy(), 20000)
	d2.restoreAllSessions()
	sess2 := d2.manager.GetSession("lazy")
	if sess2 == nil {
		t.Fatal("the session did not come back")
	}
	pty2 := sess2.GetPTY(sess2.GetState().Windows[0].PTYID)
	if pty2 == nil || pty2.cold == nil {
		t.Fatal("no restored pane with a log")
	}
	if n := coldlog.BlockReads() - base; n != 0 {
		t.Fatalf("restore read %d blocks of the log", n)
	}
	if got := ringLen(pty2); got > 6000 {
		t.Fatalf("restored ring holds %d rows: only the saved tail comes back", got)
	}

	// Attach: the usual small snapshot, the listing metadata, a subscription.
	pty2.GetTerminalState(500, 0)
	pty2.GetTerminalStatePacked(500, 0)
	pty2.terminalMu.RLock()
	_ = pty2.metaLocked()
	pty2.terminalMu.RUnlock()
	pty2.SubscribeFromSnapshot("client-1", 0)
	if n := coldlog.BlockReads() - base; n != 0 {
		t.Fatalf("attach read %d blocks of the log", n)
	}

	// Demote, then promote: the ring regrows only from new output.
	now := time.Now()
	tierState(sess2, "win-1", pty2, false, false)
	quiet(pty2, now)
	d2.tierScrollback(now)
	shrunk := ringLen(pty2)
	tierState(sess2, "win-1", pty2, true, false)
	d2.tierScrollback(now)
	if n := coldlog.BlockReads() - base; n != 0 {
		t.Fatalf("demotion and promotion read %d blocks of the log", n)
	}
	if got := ringLen(pty2); got > shrunk {
		t.Fatalf("promotion refilled the ring from %d to %d rows", shrunk, got)
	}

	// An explicit capture of a window reads only the blocks of that window.
	res := captureVerb(t, d2, "lazy", map[string]any{"source": "recent", "start": 1000, "end": 1199})
	if c, _ := res["content"].(string); strings.Count(c, "\n") < 150 {
		t.Fatalf("capture returned %q", c)
	}
	n := coldlog.BlockReads() - base
	if n < 1 || n > 3 {
		t.Fatalf("a 200-line capture read %d blocks (256 rows each), want 1 to 3", n)
	}
	// The same lines again come from the cache.
	captureVerb(t, d2, "lazy", map[string]any{"source": "recent", "start": 1000, "end": 1199})
	if m := coldlog.BlockReads() - base; m != n {
		t.Fatalf("repeating the capture read %d more blocks", m-n)
	}
	// The tail of the history: lines from the end.
	before := coldlog.BlockReads()
	captureVerb(t, d2, "lazy", map[string]any{"source": "recent", "lines": 100})
	if m := coldlog.BlockReads() - before; m > 2 {
		t.Fatalf("a 100-line tail capture read %d blocks", m)
	}
}
