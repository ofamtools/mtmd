package coldlog

import (
	"compress/gzip"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"ofam.dev/mtmd/internal/vt"
)

// File is what the writer needs of a segment file; Options.Create swaps it
// (tests inject failures).
type File interface {
	io.Writer
	Sync() error
	Close() error
}

// Options tunes a Log. Zero values take the defaults in parentheses.
type Options struct {
	QueueRows     int           // rows waiting for the writer before rows are dropped (16384)
	BlockRows     int           // rows per block (256)
	SegmentRows   int           // rows after which a segment is sealed (4096)
	SegmentBytes  int64         // raw bytes after which a segment is sealed (8 MiB)
	FlushInterval time.Duration // longest a row waits in the open block (1s)
	GzipLevel     int           // gzip.DefaultCompression
	CacheBlocks   int           // decoded blocks kept for reads (8)
	MaxRows       int64         // per-log row cap, oldest whole segment first; 0 = none
	MaxBytes      int64         // per-log byte cap; 0 = none
	StartRow      uint64        // id of the first row of an empty log (0)
	// OnError is called once, from the writer goroutine, when a disk error
	// disables the log.
	OnError func(error)
	// Create opens a new segment file for writing (os.OpenFile by default).
	Create func(path string) (File, error)
}

func (o *Options) defaults() {
	if o.QueueRows <= 0 {
		o.QueueRows = 16384
	}
	if o.BlockRows <= 0 {
		o.BlockRows = 256
	}
	if o.BlockRows > maxBlockRows {
		o.BlockRows = maxBlockRows
	}
	if o.SegmentRows <= 0 {
		o.SegmentRows = 4096
	}
	if o.SegmentBytes <= 0 {
		o.SegmentBytes = 8 << 20
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = time.Second
	}
	if o.GzipLevel == 0 {
		o.GzipLevel = gzip.DefaultCompression
	}
	if o.Create == nil {
		o.Create = func(p string) (File, error) { return os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) }
	}
}

const (
	kindRow = iota
	kindFlush
	kindClose
)

type item struct {
	kind  uint8
	flags uint8
	width int32
	gap   uint64 // rows dropped just before this item
	data  []byte
	done  chan struct{}
}

const slabSize = 64 << 10

// Log is the writer (and a reader) of one pane's cold log. Append is for a
// single producer, the emulator hook, and never blocks. Everything else may be
// called from any goroutine.
type Log struct {
	opt  Options
	dir  string
	lock *dirLock
	ix   *index

	mu      sync.Mutex // guards the producer side below
	q       []item
	pending uint64 // rows dropped since the last queued item
	slab    []byte
	closed  bool
	wake    chan struct{}
	finish  chan struct{} // closed when the goroutine exits

	disabled atomic.Bool
	errv     atomic.Pointer[error]
	dropped  atomic.Uint64
	appended atomic.Uint64

	// Writer-goroutine state.
	comp    *compressor
	f       File
	cur     *segment // open segment, also in ix.segs
	curRaw  int64
	raw     []byte // the open block's row records
	blkRows int
	blkID   uint64 // id of the open block's first row
	nextID  uint64
	blkAge  time.Time
}

// OpenLog opens (creating it) the log in dir for writing: it takes the
// sole-writer lock (ErrLocked when held), truncates a torn tail, seals the
// segments a crash left open, clears an orphan marker and applies retention.
func OpenLog(dir string, opt Options) (*Log, error) {
	opt.defaults()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lk, err := lockDir(dir)
	if err != nil {
		return nil, err
	}
	l := &Log{opt: opt, dir: dir, lock: lk, ix: &index{dir: dir, cache: newBlockCache(opt.CacheBlocks)}}
	if l.comp, err = newCompressor(opt.GzipLevel); err != nil {
		lk.release()
		return nil, err
	}
	if err := l.ix.load(true); err != nil {
		lk.release()
		return nil, err
	}
	if _, next, ok := l.ix.bounds(); ok {
		l.nextID = next
	} else {
		l.nextID = opt.StartRow
	}
	_ = ClearMarker(dir) // a log with a writer is live again
	l.retain()
	l.wake = make(chan struct{}, 1)
	l.finish = make(chan struct{})
	go l.run()
	return l, nil
}

// Dir returns the log's directory.
func (l *Log) Dir() string { return l.dir }

// Reader returns the log's reader, sharing the writer's index and cache.
func (l *Log) Reader() *Reader { return &Reader{ix: l.ix} }

// Err returns the error that disabled the log, or nil.
func (l *Log) Err() error {
	if p := l.errv.Load(); p != nil {
		return *p
	}
	return nil
}

// Dropped returns the number of rows lost to a full queue or a disabled log.
func (l *Log) Dropped() uint64 { return l.dropped.Load() }

// Append queues a copy of r for the writer. It never blocks and never fails:
// with the queue full, or the log disabled, the row is counted as dropped and
// leaves a gap of one id.
func (l *Log) Append(r vt.EncodedRow) {
	l.mu.Lock()
	if l.closed || l.disabled.Load() {
		l.mu.Unlock()
		l.dropped.Add(1)
		return
	}
	if len(l.q) >= l.opt.QueueRows {
		l.pending++
		l.mu.Unlock()
		l.dropped.Add(1)
		return
	}
	n := len(r.Data)
	if cap(l.slab)-len(l.slab) < n {
		l.slab = make([]byte, 0, max(slabSize, n))
	}
	off := len(l.slab)
	l.slab = append(l.slab, r.Data...)
	l.q = append(l.q, item{kind: kindRow, flags: r.Flags, width: int32(r.Width), gap: l.pending, data: l.slab[off : off+n : off+n]})
	l.pending = 0
	first := len(l.q) == 1
	l.mu.Unlock()
	l.appended.Add(1)
	if first {
		select {
		case l.wake <- struct{}{}:
		default:
		}
	}
}

// control queues a flush or close marker (never dropped) and waits for it.
func (l *Log) control(kind uint8) {
	done := make(chan struct{})
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		if kind == kindClose {
			<-l.finish
		}
		return
	}
	if kind == kindClose {
		l.closed = true
	}
	l.q = append(l.q, item{kind: kind, gap: l.pending, done: done})
	l.pending = 0
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
	<-done
}

// Flush returns once every row appended before it is in the segment file (the
// open block is written out short).
func (l *Log) Flush() { l.control(kindFlush) }

// Close flushes, seals the open segment, releases the lock and stops the
// writer. The log's files stay; see CloseOrphan to leave an orphan marker.
func (l *Log) Close() error {
	l.control(kindClose)
	<-l.finish
	return l.Err()
}

// run is the writer goroutine.
func (l *Log) run() {
	defer close(l.finish)
	tick := time.NewTicker(l.opt.FlushInterval / 2)
	defer tick.Stop()
	var batch []item
	for {
		select {
		case <-l.wake:
		case <-tick.C:
			if l.blkRows > 0 && time.Since(l.blkAge) >= l.opt.FlushInterval {
				l.flushBlock()
			}
		}
		l.mu.Lock()
		batch, l.q = l.q, batch[:0]
		l.mu.Unlock()
		for i := range batch {
			it := &batch[i]
			if l.handle(it) {
				l.drainRest(batch[i+1:])
				return
			}
			batch[i] = item{}
		}
	}
}

// drainRest finishes controls queued behind a close so nobody waits forever.
func (l *Log) drainRest(rest []item) {
	for _, it := range rest {
		if it.done != nil {
			close(it.done)
		}
	}
	l.mu.Lock()
	rest, l.q = l.q, nil
	l.mu.Unlock()
	for _, it := range rest {
		if it.done != nil {
			close(it.done)
		}
	}
}

// handle processes one item and reports whether the goroutine should exit.
func (l *Log) handle(it *item) (exit bool) {
	if it.gap > 0 {
		l.flushBlock()
		l.nextID += it.gap
	}
	switch it.kind {
	case kindRow:
		if l.disabled.Load() {
			l.nextID++
			l.dropped.Add(1)
			return false
		}
		if l.blkRows == 0 {
			l.blkID = l.nextID
			l.blkAge = time.Now()
		}
		l.raw = appendRow(l.raw, vt.EncodedRow{Data: it.data, Width: int(it.width), Flags: it.flags})
		l.blkRows++
		l.nextID++
		if l.blkRows >= l.opt.BlockRows {
			l.flushBlock()
		}
	case kindFlush:
		l.flushBlock()
		close(it.done)
	case kindClose:
		l.flushBlock()
		if l.f != nil {
			l.seal()
		}
		l.lock.release()
		close(it.done)
		return true
	}
	return false
}

// fail disables the log after a disk error and reports it once.
func (l *Log) fail(err error) {
	if l.disabled.Swap(true) {
		return
	}
	l.errv.Store(&err)
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
	l.raw, l.blkRows = l.raw[:0], 0
	if l.opt.OnError != nil {
		l.opt.OnError(err)
	}
}

// flushBlock writes the open block, rotating the segment first when it is full.
func (l *Log) flushBlock() {
	if l.blkRows == 0 {
		return
	}
	if l.disabled.Load() {
		l.raw, l.blkRows = l.raw[:0], 0
		return
	}
	if l.f != nil && (l.cur.rows >= int64(l.opt.SegmentRows) || l.curRaw >= l.opt.SegmentBytes) {
		l.seal()
		if l.disabled.Load() {
			return
		}
	}
	payload, err := l.comp.compress(l.raw)
	if err != nil {
		l.fail(err)
		return
	}
	b := blockRef{clen: uint32(len(payload)), crc: crc32.Checksum(payload, crcTable), first: l.blkID, n: uint32(l.blkRows), rawLen: uint32(len(l.raw))}
	if l.f == nil {
		if err := l.startSegment(l.blkID); err != nil {
			l.fail(err)
			return
		}
	}
	b.off = l.cur.size
	buf := make([]byte, blockHdrSize, blockHdrSize+len(payload))
	putBlockHdr(buf, b)
	buf = append(buf, payload...)
	if _, err := l.f.Write(buf); err != nil {
		l.fail(err)
		return
	}
	l.ix.mu.Lock()
	l.cur.blocks = append(l.cur.blocks, b)
	l.cur.rows += int64(b.n)
	l.cur.size += int64(len(buf))
	l.ix.mu.Unlock()
	l.curRaw += int64(len(l.raw))
	l.raw, l.blkRows = l.raw[:0], 0
}

func (l *Log) startSegment(first uint64) error {
	path := filepath.Join(l.dir, segName(first))
	f, err := l.opt.Create(path)
	if err != nil {
		return err
	}
	h := putHeader(first)
	if _, err := f.Write(h); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	l.f = f
	l.cur = &segment{path: path, first: first, size: headerSize}
	l.curRaw = 0
	l.ix.mu.Lock()
	l.ix.segs = append(l.ix.segs, l.cur)
	l.ix.mu.Unlock()
	return nil
}

// seal writes the footer of the open segment, closes it and applies retention.
func (l *Log) seal() {
	seg := l.cur
	l.ix.mu.RLock()
	foot := footer(seg.blocks, seg.size)
	l.ix.mu.RUnlock()
	_, err := l.f.Write(foot)
	if err == nil {
		err = l.f.Sync()
	}
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f, l.cur = nil, nil
	if err != nil {
		l.f = nil
		l.fail(err)
		return
	}
	l.ix.mu.Lock()
	seg.sealed = true
	seg.size += int64(len(foot))
	l.ix.mu.Unlock()
	l.retain()
}

// retain drops the oldest sealed segments while a per-log cap is exceeded.
func (l *Log) retain() {
	for {
		st := l.ix.stats()
		over := (l.opt.MaxRows > 0 && st.Rows > l.opt.MaxRows) || (l.opt.MaxBytes > 0 && st.Bytes > l.opt.MaxBytes)
		if !over {
			return
		}
		if _, ok := l.DropOldestSegment(); !ok {
			return
		}
	}
}

// DropOldestSegment deletes the oldest sealed segment (never the open one)
// and returns the bytes freed. The total-cap enforcer calls it on live logs.
func (l *Log) DropOldestSegment() (int64, bool) {
	l.ix.mu.Lock()
	if len(l.ix.segs) == 0 || !l.ix.segs[0].sealed {
		l.ix.mu.Unlock()
		return 0, false
	}
	s := l.ix.segs[0]
	l.ix.segs = l.ix.segs[1:]
	l.ix.mu.Unlock()
	l.ix.cache.drop(s.first, s.next())
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, false
	}
	return s.size, true
}

// Stats returns the log's extent on disk (rows still in the open block or the
// queue are not counted).
func (l *Log) Stats() Stats { return l.ix.stats() }
