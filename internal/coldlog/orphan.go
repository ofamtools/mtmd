package coldlog

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"ofam.dev/mtmd/internal/atomicfile"
)

const markerName = "closed.json"

// Marker is closed.json: it says a log has no pane any more. A log with a
// marker is an orphan, kept until its grace period ends.
type Marker struct {
	ClosedAt time.Time `json:"closed_at"`
	Reason   string    `json:"reason,omitempty"` // window-closed, session-killed, ...
	Session  string    `json:"session,omitempty"`
	Window   string    `json:"window,omitempty"`
	Title    string    `json:"title,omitempty"`
	Cwd      string    `json:"cwd,omitempty"`
	Rows     int64     `json:"rows,omitempty"`
	Bytes    int64     `json:"bytes,omitempty"`
	// Keep exempts the orphan from the grace purge and from the total cap.
	Keep bool `json:"keep,omitempty"`
}

// ReadMarker returns dir's marker; fs.ErrNotExist when the log is live.
func ReadMarker(dir string) (Marker, error) {
	var m Marker
	b, err := os.ReadFile(filepath.Join(dir, markerName))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// WriteMarker writes dir's marker atomically.
func WriteMarker(dir string, m Marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(dir, markerName), append(b, '\n'), 0o600, false)
}

// ClearMarker removes the marker (the log is live again); no error when absent.
func ClearMarker(dir string) error {
	err := os.Remove(filepath.Join(dir, markerName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// SetKeep sets or clears the keep flag of an orphan.
func SetKeep(dir string, keep bool) error {
	m, err := ReadMarker(dir)
	if err != nil {
		return err
	}
	m.Keep = keep
	return WriteMarker(dir, m)
}

// CloseOrphan closes the log and leaves an orphan marker; Rows and Bytes are
// filled in from the log, ClosedAt from now when zero.
func (l *Log) CloseOrphan(m Marker, now time.Time) error {
	cerr := l.Close()
	st := l.Stats()
	m.Rows, m.Bytes = st.Rows, st.Bytes
	if m.ClosedAt.IsZero() {
		m.ClosedAt = now
	}
	if err := WriteMarker(l.dir, m); err != nil {
		return err
	}
	return cerr
}

// Info describes one log directory found under a root.
type Info struct {
	Dir      string
	Bytes    int64
	Segments int
	Orphan   bool
	Marker   Marker // valid when Orphan
	Locked   bool   // a writer holds it right now
}

// isLogDir reports whether dir holds a log, with its size.
func statDir(dir string) (Info, bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return Info{}, false
	}
	in := Info{Dir: dir}
	found := false
	for _, e := range ents {
		if _, ok := parseSegName(e.Name()); ok && !e.IsDir() {
			found = true
			in.Segments++
			if fi, err := e.Info(); err == nil {
				in.Bytes += fi.Size()
			}
		}
		if e.Name() == markerName {
			found = true
		}
	}
	if !found {
		return Info{}, false
	}
	if m, err := ReadMarker(dir); err == nil {
		in.Orphan, in.Marker = true, m
	}
	return in, true
}

// Stat describes the log in dir; false when dir is not a log.
func Stat(dir string) (Info, bool) {
	in, ok := statDir(dir)
	if ok {
		in.Locked = Locked(dir)
	}
	return in, ok
}

// FindLogs lists the log directories under root (not descending into one).
func FindLogs(root string) ([]Info, error) {
	var out []Info
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if in, ok := statDir(path); ok {
			out = append(out, in)
			return filepath.SkipDir
		}
		return nil
	})
	return out, err
}

// PurgeOptions configure PurgeOrphans.
type PurgeOptions struct {
	Now       func() time.Time // time.Now when nil
	GraceDays int              // orphans closed longer ago than this are purged; 0 = all
	DryRun    bool             // report, delete nothing
	// StampUnmarked gives a log with no marker and no writer a closed_at of
	// now (first sight); the grace period then runs from there.
	StampUnmarked bool
	// Live, when set, reports directories that a pane owns even though no
	// writer holds their lock (not opened yet); they are left alone.
	Live func(dir string) bool
}

// Purged is one log PurgeOrphans removed (or would, with DryRun).
type Purged struct {
	Dir      string
	ClosedAt time.Time
	Bytes    int64
}

// PurgeOrphans deletes the orphaned logs under root whose grace period has
// ended, skipping kept ones and live ones. It is the daemon's housekeeping
// sweep; the caller decides how often to run it.
func PurgeOrphans(root string, opt PurgeOptions) ([]Purged, error) {
	now := time.Now
	if opt.Now != nil {
		now = opt.Now
	}
	logs, err := FindLogs(root)
	if err != nil {
		return nil, err
	}
	grace := time.Duration(opt.GraceDays) * 24 * time.Hour
	var out []Purged
	for _, in := range logs {
		if opt.Live != nil && opt.Live(in.Dir) {
			continue
		}
		if Locked(in.Dir) {
			continue
		}
		if !in.Orphan {
			if opt.StampUnmarked && !opt.DryRun {
				_ = WriteMarker(in.Dir, Marker{ClosedAt: now(), Reason: "unmarked", Bytes: in.Bytes})
			}
			if !opt.StampUnmarked || opt.GraceDays > 0 {
				continue
			}
			in.Marker = Marker{ClosedAt: now()}
		}
		if in.Marker.Keep || now().Before(in.Marker.ClosedAt.Add(grace)) {
			continue
		}
		if !opt.DryRun {
			if err := os.RemoveAll(in.Dir); err != nil {
				return out, err
			}
		}
		out = append(out, Purged{in.Dir, in.Marker.ClosedAt, in.Bytes})
	}
	return out, nil
}

// Trimmer is a live log that can give up its oldest segment.
type Trimmer interface {
	DropOldestSegment() (int64, bool)
}

// Removed is one thing EnforceTotal deleted.
type Removed struct {
	Dir   string
	Whole bool // the whole log (an orphan); otherwise one segment
	Bytes int64
}

// EnforceTotal brings the logs under root to at most maxBytes (0 = no cap).
// Victims go in one order: orphans whole, oldest-closed first (kept orphans
// are never victims), then the live log with the most bytes loses its oldest
// segment, repeatedly. live maps a directory to the Log this process writes
// it with; a directory with no Log and no lock is trimmed on disk, one locked
// by another process is left alone.
func EnforceTotal(root string, maxBytes int64, live func(dir string) Trimmer) ([]Removed, error) {
	if maxBytes <= 0 {
		return nil, nil
	}
	logs, err := FindLogs(root)
	if err != nil {
		return nil, err
	}
	var total int64
	for _, in := range logs {
		total += in.Bytes
	}
	var out []Removed
	if total <= maxBytes {
		return out, nil
	}
	var orphans, liveLogs []Info
	for _, in := range logs {
		switch {
		case in.Orphan && in.Marker.Keep:
		case in.Orphan:
			if !Locked(in.Dir) {
				orphans = append(orphans, in)
			}
		default:
			liveLogs = append(liveLogs, in)
		}
	}
	sort.SliceStable(orphans, func(i, j int) bool { return orphans[i].Marker.ClosedAt.Before(orphans[j].Marker.ClosedAt) })
	for _, in := range orphans {
		if total <= maxBytes {
			return out, nil
		}
		if err := os.RemoveAll(in.Dir); err != nil {
			return out, err
		}
		total -= in.Bytes
		out = append(out, Removed{in.Dir, true, in.Bytes})
	}
	for total > maxBytes && len(liveLogs) > 0 {
		sort.SliceStable(liveLogs, func(i, j int) bool { return liveLogs[i].Bytes > liveLogs[j].Bytes })
		in := liveLogs[0]
		freed, ok := int64(0), false
		var tr Trimmer
		if live != nil {
			tr = live(in.Dir)
		}
		switch {
		case tr != nil:
			freed, ok = tr.DropOldestSegment()
		case !Locked(in.Dir):
			freed, ok = dropOldestOnDisk(in.Dir)
		}
		if !ok {
			liveLogs = liveLogs[1:]
			continue
		}
		total -= freed
		liveLogs[0].Bytes -= freed
		out = append(out, Removed{in.Dir, false, freed})
	}
	return out, nil
}

// dropOldestOnDisk removes the oldest segment file of a log nobody writes,
// keeping at least one.
func dropOldestOnDisk(dir string) (int64, bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, false
	}
	var segs []fs.DirEntry
	for _, e := range ents {
		if _, ok := parseSegName(e.Name()); ok && !e.IsDir() {
			segs = append(segs, e)
		}
	}
	if len(segs) < 2 {
		return 0, false
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].Name() < segs[j].Name() })
	fi, err := segs[0].Info()
	if err != nil {
		return 0, false
	}
	if err := os.Remove(filepath.Join(dir, segs[0].Name())); err != nil {
		return 0, false
	}
	return fi.Size(), true
}
