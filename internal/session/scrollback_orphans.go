package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"ofam.dev/mtmd/internal/coldlog"
)

// Orphaned cold logs: the logs of panes that are gone. A log is live while a
// pane owns it, and orphaned once its pane closed, its session was killed, or
// a crash left it with no pane (closed.json says which). An orphan is kept for
// daemon.scrollback_grace_days, counted from its closed_at, and the sweep
// below then deletes it. A kept orphan (history-log keep) is never swept; an
// orphan can be adopted onto a new window, which gives its history back.

// orphanSweepEvery is the least time between two sweeps of a running daemon.
const orphanSweepEvery = time.Hour

// liveColdDirs are the cold log directories panes own, whether or not their
// log is open yet.
func (m *Manager) liveColdDirs() map[string]bool {
	out := map[string]bool{}
	for _, s := range m.AllSessions() {
		for _, p := range s.allPTYs() {
			if c := p.cold; c != nil {
				if dir, err := coldDir(c.session(), c.windowID); err == nil {
					out[dir] = true
				}
			}
		}
	}
	return out
}

// sweepColdOrphans deletes the orphaned logs whose grace period has ended
// (the deadline is closed_at plus the setting, so a changed setting applies to
// logs already orphaned), and gives a log that has no marker and no pane, as a
// crash leaves it, a marker stamped now so its grace period starts. It runs
// once at start, after the restore has claimed the logs of the panes that came
// back, and about hourly after that. A kept orphan is spared.
func (d *Daemon) sweepColdOrphans(now time.Time) []coldlog.Purged {
	pol := d.manager.ColdPolicy()
	if !pol.Enabled {
		return nil
	}
	live := d.manager.liveColdDirs()
	purged, err := coldlog.PurgeOrphans(historyRoot(), coldlog.PurgeOptions{
		Now:       func() time.Time { return now },
		GraceDays: pol.GraceDays,
		// With the restore switched off no pane claims its log at start.
		StampUnmarked: !d.disableAutoRestore,
		Live:          func(dir string) bool { return live[dir] },
	})
	if err != nil {
		LogError("Sweeping the orphaned scrollback logs: %v", err)
	}
	for _, p := range purged {
		debugLog("[DEBUG] scrollback log %s purged: closed %s, %d bytes", p.Dir, p.ClosedAt.Format(time.RFC3339), p.Bytes)
	}
	return purged
}

// sweepColdOrphansIfDue is cleanupOnce's call: the sweep, at most once an
// hour.
func (d *Daemon) sweepColdOrphansIfDue(now time.Time) {
	d.orphanSweepMu.Lock()
	due := d.orphanSweepAt.IsZero() || now.Sub(d.orphanSweepAt) >= orphanSweepEvery
	if due {
		d.orphanSweepAt = now
	}
	d.orphanSweepMu.Unlock()
	if due {
		d.sweepColdOrphans(now)
	}
}

// historyLogID is the id of the log of one pane: session/window.
func historyLogID(session, window string) string { return session + "/" + window }

// historyLogDir is the directory of the log an id names.
func historyLogDir(id string) (session, window, dir string, err error) {
	session, window, ok := strings.Cut(id, "/")
	if !ok || session == "" || window == "" {
		return "", "", "", fmt.Errorf("%q is not a history log id (session/window)", id)
	}
	dir, err = coldDir(session, window)
	return session, window, dir, err
}

// historyLogDoc is one log as the history-log verbs report it.
type historyLogDoc struct {
	Log      string `json:"log"`
	Session  string `json:"session"`
	Window   string `json:"window"`
	State    string `json:"state"` // live or orphaned
	Rows     int64  `json:"rows"`
	Bytes    int64  `json:"bytes"`
	FirstRow uint64 `json:"first_row"`
	NextRow  uint64 `json:"next_row"`
	Segments int    `json:"segments"`
	// ClosedAt is when the pane went (Unix ms), 0 for a live log or a crash
	// orphan the sweep has not stamped yet.
	ClosedAt int64 `json:"closed_at,omitempty"`
	// DaysLeft is how long until the sweep deletes an orphan, 0 once it is due.
	// Absent for a live log, a kept one and an unstamped one.
	DaysLeft *float64 `json:"days_left,omitempty"`
	Keep     bool     `json:"keep,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Title    string   `json:"title,omitempty"`
	Cwd      string   `json:"cwd,omitempty"`
	Gaps     int      `json:"gaps,omitempty"`
}

// describeColdLog turns what is on disk under dir into a doc.
func describeColdLog(in coldlog.Info, live map[string]bool, graceDays int, now time.Time) historyLogDoc {
	window := strings.TrimSuffix(filepath.Base(in.Dir), coldExt)
	session := filepath.Base(filepath.Dir(in.Dir))
	doc := historyLogDoc{
		Log: historyLogID(session, window), Session: session, Window: window,
		State: "orphaned", Bytes: in.Bytes, Segments: in.Segments,
	}
	if r, err := coldlog.OpenReader(in.Dir, 1); err == nil {
		st := r.Stats()
		doc.Rows, doc.FirstRow, doc.NextRow, doc.Gaps = st.Rows, st.First, st.Next, len(st.Gaps)
	}
	if in.Orphan {
		m := in.Marker
		doc.Keep, doc.Reason, doc.Title, doc.Cwd = m.Keep, m.Reason, m.Title, m.Cwd
		if !m.ClosedAt.IsZero() {
			doc.ClosedAt = m.ClosedAt.UnixMilli()
			if !m.Keep {
				left := m.ClosedAt.Add(time.Duration(graceDays)*24*time.Hour).Sub(now).Hours() / 24
				left = max(left, 0)
				left = float64(int(left*10+0.5)) / 10
				doc.DaysLeft = &left
			}
		}
	} else if live[in.Dir] || in.Locked {
		doc.State = "live"
	}
	return doc
}

// historyLogDocs lists the logs under the history root, by id.
func (d *Daemon) historyLogDocs(now time.Time) ([]historyLogDoc, error) {
	logs, err := coldlog.FindLogs(historyRoot())
	if err != nil {
		return nil, err
	}
	live := d.manager.liveColdDirs()
	grace := d.manager.ColdPolicy().GraceDays
	out := make([]historyLogDoc, 0, len(logs))
	for _, in := range logs {
		in.Locked = coldlog.Locked(in.Dir)
		out = append(out, describeColdLog(in, live, grace, now))
	}
	slices.SortFunc(out, func(a, b historyLogDoc) int { return strings.Compare(a.Log, b.Log) })
	return out, nil
}

// historyLogDoc finds one log by id.
func (d *Daemon) historyLogDoc(id string, now time.Time) (historyLogDoc, string, error) {
	_, _, dir, err := historyLogDir(id)
	if err != nil {
		return historyLogDoc{}, "", err
	}
	in, ok := coldlog.Stat(dir)
	if !ok {
		return historyLogDoc{}, dir, os.ErrNotExist
	}
	return describeColdLog(in, d.manager.liveColdDirs(), d.manager.ColdPolicy().GraceDays, now), dir, nil
}

var (
	errHistoryLogLive     = errors.New("the log belongs to a live pane")
	errHistoryLogNotFound = errors.New("no such history log")
)

// adoptHistoryLog gives the orphaned log at src to the pane c: the directory
// moves to the pane's own place, the orphan marker goes, and the ring's first
// row takes the id after the log's last row. The pane must have no log of its
// own, because ids and rows cannot be merged. The caller holds the pane's
// terminalMu so no row is evicted meanwhile.
func (c *paneCold) adoptHistoryLog(src string) (rows int64, err error) {
	if c == nil {
		return 0, errors.New("the pane does not keep a cold log")
	}
	in, ok := coldlog.Stat(src)
	if !ok {
		return 0, errHistoryLogNotFound
	}
	if !in.Orphan || in.Locked {
		return 0, errHistoryLogLive
	}
	dst, err := coldDir(c.session(), c.windowID)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.log != nil || c.done {
		return 0, errors.New("the pane already has a history log of its own")
	}
	if filepath.Clean(src) == filepath.Clean(dst) {
		return 0, errors.New("the log already belongs to this pane")
	}
	if _, has := coldlog.Stat(dst); has {
		return 0, errors.New("the pane already has a history log of its own")
	}
	if err := os.RemoveAll(dst); err != nil { // an empty dir a failed open left
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, err
	}
	if err := os.Rename(src, dst); err != nil {
		return 0, err
	}
	if err := coldlog.ClearMarker(dst); err != nil {
		return 0, err
	}
	r, err := coldlog.OpenReader(dst, 1)
	if err != nil {
		return 0, err
	}
	st := r.Stats()
	c.base = st.Next
	c.evicted.Store(0)
	c.skip.Store(0)
	c.failed = false
	c.tailOK.Store(false)
	c.diskRd.Store(nil)
	return st.Rows, nil
}
