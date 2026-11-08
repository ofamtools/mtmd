package session

import (
	"slices"
	"time"

	"ofam.dev/mtmd/internal/config"
	"ofam.dev/mtmd/internal/memtrim"
)

// Tiering keeps a pane's history ring as deep as the pane's use asks. A pane
// in use (hot) keeps appearance.scrollback_lines rows in memory. A pane that
// is not (cold) keeps daemon.scrollback_cold_ring_rows, and the rows that do
// not fit go to the pane's cold log through the evict hook, so nothing is
// lost and the older history is read from disk. A pane that turns hot again
// gets its configured depth back; the ring is not refilled from disk, so it
// grows again only with new output.
//
// Which signals count as use is data: daemon.scrollback_hot_when names them,
// and tierSignals maps each name to its test. Only a pane with a cold log
// (disk history on, an emulator that reports evictions) is ever shrunk, since
// shrinking any other would lose the rows.

// TierPolicy is the resolved daemon.scrollback_hot_* and
// scrollback_cold_ring_rows settings.
type TierPolicy struct {
	// Signals is the set of signals, bits indexed by tierSignalNames, that make
	// a pane hot. None means every pane is cold.
	Signals uint8
	// Idle is how long a pane may print nothing and still count as hot by
	// "output".
	Idle time.Duration
	// ColdRows is the ring depth of a cold pane, at least the floor.
	ColdRows int
}

// ResolveTierPolicy turns the settings into a policy. A nil list means every
// signal (a config that never named them); an empty one means none; a name
// that is not a signal is ignored.
func ResolveTierPolicy(hotWhen []string, idleSeconds, coldRows int) TierPolicy {
	p := TierPolicy{Idle: time.Duration(idleSeconds) * time.Second, ColdRows: coldRows}
	if idleSeconds < 0 {
		p.Idle = 0
	}
	if coldRows <= 0 {
		p.ColdRows = config.ScrollbackColdRingRowsDefault
	}
	p.ColdRows = max(p.ColdRows, config.ScrollbackColdRingRowsFloor)
	if hotWhen == nil {
		hotWhen = config.ScrollbackHotSignals
	}
	for _, name := range hotWhen {
		if i := slices.Index(tierSignalNames, name); i >= 0 {
			p.Signals |= 1 << i
		}
	}
	return p
}

// tierCtx is what the signals read for one pane in one sweep.
type tierCtx struct {
	now   time.Time
	idle  time.Duration
	pty   *PTY
	sess  *Session
	state *SessionState
	win   *WindowState
}

// tierSignalNames are the names daemon.scrollback_hot_when accepts; tierSignals
// is their tests, in the same order.
var tierSignalNames = config.ScrollbackHotSignals

// tierSignal is one reason a pane is hot. A timed one lapses by itself, so the
// sweep has to come back to see it lapse.
type tierSignal struct {
	holds func(*tierCtx) bool
	timed bool
}

var tierSignals = [...]tierSignal{
	// displayed: on screen for a client: its window is not minimized, is on the
	// workspace the session shows, and a TUI client shows the session.
	{holds: func(c *tierCtx) bool {
		return c.win != nil && !c.win.Minimized && c.win.Workspace == c.state.CurrentWorkspace && c.sess.tuiClients.Load() > 0
	}},
	// focused
	{holds: func(c *tierCtx) bool {
		return c.win != nil && c.state.FocusedWindowID == c.win.ID
	}},
	// output: printed within the idle time
	{timed: true, holds: func(c *tierCtx) bool {
		last := c.pty.LastOutput()
		return last > 0 && c.now.Sub(time.Unix(0, last)) < c.idle
	}},
}

// tierStep is the most rows one shrink hands to the log at once. The log's
// queue holds 16384 rows and drops what does not fit, so a pane with a
// million-row ring is brought down in steps, each flushed before the next.
const tierStep = 8192

// holding reports whether any signal of the policy holds for the pane, and
// whether one that lapses by itself does.
func (p TierPolicy) holding(c *tierCtx) (hot, timed bool) {
	for i, sig := range tierSignals {
		if p.Signals&(1<<i) != 0 && sig.holds(c) {
			hot = true
			timed = timed || sig.timed
		}
	}
	return hot, timed
}

// tierScrollback is the sweep: it sets every pane's ring depth by its use. It
// reports whether it should run again without further activity, which is so
// while a pane is hot only because it printed lately.
func (d *Daemon) tierScrollback(now time.Time) (again bool) {
	pol := d.manager.ColdPolicy()
	if !pol.Enabled {
		return false
	}
	shrunk := false
	for _, sess := range d.manager.AllSessions() {
		state := sess.GetState()
		if state == nil {
			continue
		}
		for _, id := range sess.ListPTYIDs() {
			p := sess.GetPTY(id)
			if p == nil || !p.cold.usable() {
				continue
			}
			c := &tierCtx{now: now, idle: pol.Tier.Idle, pty: p, sess: sess, state: state}
			for i := range state.Windows {
				if state.Windows[i].PTYID == id {
					c.win = &state.Windows[i]
					break
				}
			}
			hot, timed := pol.Tier.holding(c)
			again = again || timed
			if p.setRingDepth(hot, pol.Tier.ColdRows) {
				shrunk = true
			}
		}
	}
	if shrunk {
		// The rows a shrink let go of are garbage until a collection and a
		// trim hand the pages back; RSS only falls after it.
		memtrim.Request()
	}
	return again
}

// setRingDepth brings the pane's ring to the depth its use asks and reports
// whether it shrank. A hot pane gets the configured depth; a cold one the
// smaller of that and coldRows.
func (p *PTY) setRingDepth(hot bool, coldRows int) (shrunk bool) {
	full := 0
	if s := p.owner(); s != nil {
		full = s.scrollbackLines()
	}
	if full <= 0 {
		return false
	}
	want := full
	if !hot {
		want = min(full, coldRows)
	}
	cur := int(p.ringRows.Load())
	if cur == 0 {
		cur = full
	}
	if want == cur {
		return false
	}
	if want > cur {
		p.terminalMu.Lock()
		p.terminal.SetScrollbackMaxLines(want)
		p.terminalMu.Unlock()
		p.ringRows.Store(int64(want))
		return false
	}
	for cur > want {
		p.terminalMu.Lock()
		next := want
		if n := p.terminal.ScrollbackLen(); n > want {
			next = max(want, n-tierStep)
		}
		p.terminal.SetScrollbackMaxLines(next)
		p.terminalMu.Unlock()
		p.cold.flush()
		cur = next
		p.ringRows.Store(int64(cur))
	}
	return true
}
