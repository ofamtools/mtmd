package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ofam.dev/mtmd/internal/coldlog"
	"ofam.dev/mtmd/internal/config"
	"ofam.dev/mtmd/internal/vt"
)

// The cold log is the on-disk half of a pane's scrollback: each row the
// emulator lets go of from the old end of its history is appended to a
// coldlog.Log in <state>/scrollback/<session>/<window>.cold/. Row ids in the
// log are global: the number of rows the pane has evicted before it.

const (
	coldExt = ".cold"

	// Why a log was closed, as written to its closed.json.
	coldWindowClosed  = "window-closed"
	coldSessionKilled = "session-killed"
)

// ColdPolicy is what the daemon does with the rows that leave a pane's memory:
// the resolved daemon.scrollback_disk settings. The zero value writes nothing.
type ColdPolicy struct {
	// Enabled writes the evicted rows to a log.
	Enabled bool
	// Rows is the most rows one log keeps, oldest segments dropped first; 0 = no limit.
	Rows int64
	// Bytes is the most one log takes on disk.
	Bytes int64
	// TotalBytes is the most all logs take together; 0 = no limit.
	TotalBytes int64
	// GraceDays is how long a closed pane's log is kept; 0 deletes it at close.
	GraceDays int
	// Tier is how the pane's memory ring follows its use. See scrollback_tier.go.
	Tier TierPolicy
}

// ResolveColdPolicy turns the config's settings into a policy. nil enabled
// means on; a negative or (for the byte limits) zero bound means its default.
func ResolveColdPolicy(enabled *bool, rows, mbPane, mbTotal, graceDays int) ColdPolicy {
	p := ColdPolicy{Enabled: enabled == nil || *enabled, Rows: int64(rows), Bytes: int64(mbPane) << 20, TotalBytes: int64(mbTotal) << 20, GraceDays: graceDays}
	if rows < 0 {
		p.Rows = config.ScrollbackDiskRowsPerPaneDefault
	}
	if mbPane <= 0 {
		p.Bytes = int64(config.ScrollbackDiskMBPerPaneDefault) << 20
	}
	if mbTotal <= 0 {
		p.TotalBytes = int64(config.ScrollbackDiskMBTotalDefault) << 20
	}
	p.GraceDays = min(max(graceDays, 0), 365)
	return p
}

// SetColdPolicy sets what every session made from now on does with the rows
// that leave its panes' memory.
func (m *Manager) SetColdPolicy(p ColdPolicy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cold = &p
}

// ColdPolicy is what SetColdPolicy last set, off when nothing did.
func (m *Manager) ColdPolicy() ColdPolicy {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cold == nil {
		return ColdPolicy{}
	}
	return *m.cold
}

// coldPolicy is the session's ColdPolicy, off when none was stamped.
func (s *Session) coldPolicy() ColdPolicy {
	if s.config == nil || s.config.cold == nil {
		return ColdPolicy{}
	}
	return *s.config.cold
}

// coldDir is where one pane's log lives.
func coldDir(sessionName, windowID string) (string, error) {
	if !validHistoryWindowID(windowID) {
		return "", errors.New("bad window id")
	}
	dir, err := historyDir(sessionName)
	if err != nil {
		return "", err
	}
	return stateJoin(dir, windowID, coldExt)
}

func isColdDir(name string) bool { return strings.HasSuffix(name, coldExt) }

// paneCold is one pane's cold log. The log is opened at the first evicted
// row, so a pane that never fills its history leaves no files.
type paneCold struct {
	pol      ColdPolicy
	windowID string
	// session names the session that owns the pane now; it changes when the
	// pane moves.
	session func() string

	mu     sync.RWMutex
	log    *coldlog.Log
	failed bool // the log could not be opened or broke: the pane is as without one
	done   bool // closed: no more rows are taken
}

func newPaneCold(pol ColdPolicy, windowID string, session func() string) *paneCold {
	return &paneCold{pol: pol, windowID: windowID, session: session}
}

// append is the evict hook. It runs under the pane's terminalMu, so it only
// copies the row (Log.Append never blocks); the open at the first row is the
// one exception.
func (c *paneCold) append(r vt.EncodedRow) {
	c.mu.RLock()
	l := c.log
	c.mu.RUnlock()
	if l != nil {
		l.Append(r)
		return
	}
	c.mu.Lock()
	if c.log == nil && !c.failed && !c.done {
		c.open()
	}
	l = c.log
	c.mu.Unlock()
	if l != nil {
		l.Append(r)
	}
}

// open opens the log. The caller holds c.mu.
func (c *paneCold) open() {
	dir, err := coldDir(c.session(), c.windowID)
	if err == nil {
		c.log, err = coldlog.OpenLog(dir, coldlog.Options{
			MaxRows:  c.pol.Rows,
			MaxBytes: c.pol.Bytes,
			OnError: func(err error) {
				LogError("Scrollback log of window %s stopped, its history stays in memory only: %v", shortID(c.windowID), err)
			},
		})
	}
	if err != nil {
		c.failed = true
		LogError("Scrollback log of window %s could not be opened, its history stays in memory only: %v", shortID(c.windowID), err)
	}
}

// flush returns once every row evicted so far is in the log's file.
func (c *paneCold) flush() {
	if c == nil {
		return
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.log != nil {
		c.log.Flush()
	}
}

// usable reports whether rows the pane lets go of reach the log: it is open
// or can be, and has not stopped. A log that has not is no place to put rows
// on purpose.
func (c *paneCold) usable() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return !c.failed && !c.done && (c.log == nil || !c.log.Disabled())
}

// DropOldestSegment makes a pane's log a coldlog.Trimmer.
func (c *paneCold) DropOldestSegment() (int64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.log == nil {
		return 0, false
	}
	return c.log.DropOldestSegment()
}

// dir is the open log's directory, "" when none is open.
func (c *paneCold) dir() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.log == nil {
		return ""
	}
	return c.log.Dir()
}

// hold seals and closes the log and keeps rows from opening it again until
// release is called, so the log's directory can be moved. A row evicted
// meanwhile waits for release (the hook runs under the pane's terminalMu, and
// a move is one rename), and then opens the log wherever the pane lives.
func (c *paneCold) hold() (release func()) {
	if c == nil {
		return func() {}
	}
	c.mu.Lock()
	if c.log != nil {
		_ = c.log.Close()
		c.log = nil
	}
	return c.mu.Unlock
}

// clear deletes the log at once (clear-history). The next row starts a new one.
func (c *paneCold) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var dir string
	if c.log != nil {
		dir = c.log.Dir()
		_ = c.log.Close()
		c.log = nil
	} else if d, err := coldDir(c.session(), c.windowID); err == nil {
		dir = d
	}
	if dir != "" {
		if err := os.RemoveAll(dir); err != nil {
			LogError("Removing the scrollback log of window %s failed: %v", shortID(c.windowID), err)
		}
	}
	c.failed = false
}

// close seals the log and takes no more rows. With a reason the pane is gone
// and the log is left as an orphan to be kept for the grace period; with none
// the daemon is stopping and the pane carries on in the next process.
func (c *paneCold) close(reason, title, cwd string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.done = true
	l := c.log
	c.log = nil
	if l == nil {
		return
	}
	if reason == "" {
		if err := l.Close(); err != nil {
			LogError("Closing the scrollback log of window %s: %v", shortID(c.windowID), err)
		}
		return
	}
	if c.pol.GraceDays <= 0 {
		_ = l.Close()
		_ = os.RemoveAll(l.Dir())
		return
	}
	m := coldlog.Marker{Reason: reason, Session: c.session(), Window: c.windowID, Title: title, Cwd: cwd}
	if err := l.CloseOrphan(m, time.Now()); err != nil {
		LogError("Closing the scrollback log of window %s: %v", shortID(c.windowID), err)
	}
}

// installCold gives the pane's emulator a hook that writes the rows it evicts
// to the pane's cold log, when the policy asks for one and the emulator can
// report them (libghostty cannot: its history stays inside the library).
func (p *PTY) installCold(pol ColdPolicy, windowID string) {
	if !pol.Enabled || !validHistoryWindowID(windowID) {
		return
	}
	c := newPaneCold(pol, windowID,
		func() string {
			if s := p.owner(); s != nil {
				return s.Name()
			}
			return ""
		})
	p.terminalMu.Lock()
	ok := p.terminal != nil && setScrollbackEvictHook(p.terminal, c.append)
	p.terminalMu.Unlock()
	if ok {
		p.cold = c
	}
}

// coldPanes are the live logs of the manager's panes, by directory.
func (m *Manager) coldPanes() map[string]*paneCold {
	out := map[string]*paneCold{}
	for _, s := range m.AllSessions() {
		for _, p := range s.allPTYs() {
			if c := p.cold; c != nil {
				if dir := c.dir(); dir != "" {
					out[dir] = c
				}
			}
		}
	}
	return out
}

// enforceColdTotal brings the logs under the total cap: closed ones go first,
// then the live log with the most on disk drops its oldest segment.
func (d *Daemon) enforceColdTotal() {
	pol := d.manager.ColdPolicy()
	if !pol.Enabled || pol.TotalBytes <= 0 {
		return
	}
	live := d.manager.coldPanes()
	removed, err := coldlog.EnforceTotal(historyRoot(), pol.TotalBytes, func(dir string) coldlog.Trimmer {
		if c, ok := live[dir]; ok {
			return c
		}
		return nil
	})
	if err != nil {
		LogError("Enforcing the scrollback disk cap: %v", err)
	}
	for _, r := range removed {
		debugLog("[DEBUG] scrollback disk cap removed %d bytes of %s (whole=%v)", r.Bytes, r.Dir, r.Whole)
	}
}

// dropColdWhenOff deletes the cold logs once daemon.scrollback_disk is set to
// false, so turning it off also takes the text off the disk. Called once at
// start, before any pane exists.
func (d *Daemon) dropColdWhenOff() {
	if d.manager.ColdPolicy().Enabled {
		return
	}
	entries, err := os.ReadDir(historyRoot())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub, err := os.ReadDir(filepath.Join(historyRoot(), e.Name()))
		if err != nil {
			continue
		}
		for _, f := range sub {
			if f.IsDir() && isColdDir(f.Name()) {
				_ = os.RemoveAll(filepath.Join(historyRoot(), e.Name(), f.Name()))
			}
		}
	}
}

// holdCold holds every pane log of the session (see paneCold.hold) and
// returns the release.
func (s *Session) holdCold() (release func()) {
	var releases []func()
	for _, p := range s.allPTYs() {
		releases = append(releases, p.cold.hold())
	}
	return func() {
		for _, r := range releases {
			r()
		}
	}
}

// moveWindowCold moves the pane's log from the session it left to the one it
// joined. The row ids stay: they belong to the pane, not to its place.
func moveWindowCold(p *PTY, src, dst *Session, windowID string) {
	if p == nil || p.cold == nil {
		return
	}
	release := p.cold.hold()
	defer release()
	from, err := coldDir(src.Name(), windowID)
	if err != nil {
		return
	}
	to, err := coldDir(dst.Name(), windowID)
	if err != nil {
		return
	}
	if _, err := os.Stat(from); err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return
	}
	if err := os.Rename(from, to); err != nil {
		LogError("Moving the scrollback log of pane %s to session %q failed: %v", shortID(windowID), dst.Name(), err)
	}
}
