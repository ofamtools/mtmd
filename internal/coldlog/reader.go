package coldlog

import (
	"container/list"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	uv "github.com/charmbracelet/ultraviolet"

	"ofam.dev/mtmd/internal/vt"
)

// segment is one file of the log as the index knows it.
type segment struct {
	path   string
	first  uint64
	sealed bool
	blocks []blockRef
	rows   int64
	size   int64
}

func segName(first uint64) string { return fmt.Sprintf("%s%020d%s", segPrefix, first, segSuffix) }

func parseSegName(name string) (uint64, bool) {
	if !strings.HasPrefix(name, segPrefix) || !strings.HasSuffix(name, segSuffix) {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(name, segPrefix), segSuffix), 10, 64)
	return v, err == nil
}

func (s *segment) next() uint64 {
	if len(s.blocks) == 0 {
		return s.first
	}
	return s.blocks[len(s.blocks)-1].end()
}

// index is the set of segments, shared by a Log's writer and its readers.
type index struct {
	dir   string
	mu    sync.RWMutex
	segs  []*segment
	cache *blockCache
}

// Reader gives random access to a log by row number. It is safe for
// concurrent use, also while a Log appends to the same directory.
type Reader struct {
	ix *index
}

// OpenReader opens dir for reading only: no lock, no repair. An unsealed
// segment (a writer is active, or it crashed) is read up to its last good
// block. Call Refresh to see what was written since.
func OpenReader(dir string, cacheBlocks int) (*Reader, error) {
	ix := &index{dir: dir, cache: newBlockCache(cacheBlocks)}
	if err := ix.load(false); err != nil {
		return nil, err
	}
	return &Reader{ix: ix}, nil
}

// Refresh rescans the directory.
func (r *Reader) Refresh() error { return r.ix.load(false) }

// loadSegment reads the index of one segment file. repair truncates a torn
// tail. ok is false when the file is not a readable segment of this version.
func loadSegment(path string, first uint64, repair bool) (seg *segment, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	var h [headerSize]byte
	if st.Size() < headerSize {
		return nil, false, nil
	}
	if _, err := f.ReadAt(h[:], 0); err != nil {
		return nil, false, err
	}
	hf, good := parseHeader(h[:])
	if !good {
		return nil, false, nil
	}
	seg = &segment{path: path, first: hf, size: st.Size()}
	_ = first
	if blocks, _, ok := parseFooter(f, st.Size()); ok {
		seg.blocks, seg.sealed = blocks, true
	} else {
		blocks, end := scanBlocks(f, st.Size(), hf)
		seg.blocks = blocks
		seg.size = end
		if repair && end < st.Size() {
			if err := os.Truncate(path, end); err != nil {
				return nil, false, err
			}
		}
	}
	for _, b := range seg.blocks {
		seg.rows += int64(b.n)
	}
	return seg, true, nil
}

// load rebuilds the segment list from disk. With repair it truncates a torn
// tail and seals every unsealed segment (the caller holds the lock).
func (ix *index) load(repair bool) error {
	ents, err := os.ReadDir(ix.dir)
	if err != nil {
		return err
	}
	type named struct {
		name  string
		first uint64
	}
	var names []named
	for _, e := range ents {
		if f, ok := parseSegName(e.Name()); ok && !e.IsDir() {
			names = append(names, named{e.Name(), f})
		}
	}
	sort.Slice(names, func(i, j int) bool { return names[i].first < names[j].first })
	var segs []*segment
	for _, n := range names {
		path := filepath.Join(ix.dir, n.name)
		s, ok, err := loadSegment(path, n.first, repair)
		if errors.Is(err, fs.ErrNotExist) {
			continue // dropped by retention between the listing and the open
		}
		if err != nil {
			return err
		}
		if !ok || len(s.blocks) == 0 {
			// A file too short for a header, or a segment with no good
			// block, holds nothing. A foreign or newer-version file stays.
			if fi, serr := os.Stat(path); repair && serr == nil && (ok || fi.Size() < headerSize) {
				_ = os.Remove(path)
			}
			continue
		}
		if repair && !s.sealed {
			if err := sealFile(s); err != nil {
				return err
			}
		}
		segs = append(segs, s)
	}
	ix.mu.Lock()
	ix.segs = segs
	ix.mu.Unlock()
	return nil
}

// sealFile appends the footer to an unsealed segment file.
func sealFile(s *segment) error {
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	foot := footer(s.blocks, s.size)
	if _, err := f.Write(foot); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	s.sealed = true
	s.size += int64(len(foot))
	return f.Close()
}

// bounds returns the first row id on disk and the id after the last one.
func (ix *index) bounds() (first, next uint64, ok bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if len(ix.segs) == 0 {
		return 0, 0, false
	}
	return ix.segs[0].first, ix.segs[len(ix.segs)-1].next(), true
}

// First returns the id of the oldest row on disk (or 0 when empty); gaps may
// make some ids from there unreadable.
func (r *Reader) First() uint64 { f, _, _ := r.ix.bounds(); return f }

// Next returns the id after the newest row written to disk.
func (r *Reader) Next() uint64 { _, n, _ := r.ix.bounds(); return n }

// Stats describes a log on disk.
type Stats struct {
	Rows     int64 // rows readable
	Bytes    int64
	Segments int
	First    uint64
	Next     uint64
	Gaps     []Gap
}

// Gap is a run of row ids that were lost.
type Gap struct{ First, N uint64 }

// Stats returns the current extent.
func (r *Reader) Stats() Stats { return r.ix.stats() }

func (ix *index) stats() Stats {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	var st Stats
	st.Segments = len(ix.segs)
	var prev uint64
	have := false
	for _, s := range ix.segs {
		st.Rows += s.rows
		st.Bytes += s.size
		for _, b := range s.blocks {
			if have && b.first > prev {
				st.Gaps = append(st.Gaps, Gap{prev, b.first - prev})
			}
			prev, have = b.end(), true
		}
	}
	if len(ix.segs) > 0 {
		st.First, st.Next = ix.segs[0].first, ix.segs[len(ix.segs)-1].next()
	}
	return st
}

// locate finds the block holding row id.
func (ix *index) locate(id uint64) (path string, b blockRef, err error) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if len(ix.segs) == 0 {
		return "", b, ErrNotYet
	}
	if id < ix.segs[0].first {
		return "", b, ErrTrimmed
	}
	if id >= ix.segs[len(ix.segs)-1].next() {
		return "", b, ErrNotYet
	}
	si := sort.Search(len(ix.segs), func(i int) bool { return ix.segs[i].first > id }) - 1
	s := ix.segs[si]
	bi := sort.Search(len(s.blocks), func(i int) bool { return s.blocks[i].first > id }) - 1
	if bi < 0 || id >= s.blocks[bi].end() {
		return "", b, ErrGap
	}
	return s.path, s.blocks[bi], nil
}

type decoded struct {
	first uint64
	rows  []vt.EncodedRow
}

func (ix *index) block(path string, b blockRef) (*decoded, error) {
	if d := ix.cache.get(b.first); d != nil {
		return d, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrTrimmed
		}
		return nil, err
	}
	defer f.Close()
	payload := make([]byte, b.clen)
	if _, err := f.ReadAt(payload, b.off+blockHdrSize); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if crc32.Checksum(payload, crcTable) != b.crc {
		return nil, ErrCorrupt
	}
	raw, err := gunzip(payload, b.rawLen)
	if err != nil {
		return nil, err
	}
	rows, err := decodeRows(raw, int(b.n))
	if err != nil {
		return nil, err
	}
	d := &decoded{first: b.first, rows: rows}
	ix.cache.put(d)
	return d, nil
}

// Row returns the stored row id. Its Data is shared with the block cache:
// read only. Errors: ErrTrimmed, ErrGap, ErrNotYet, ErrCorrupt.
func (r *Reader) Row(id uint64) (vt.EncodedRow, error) {
	path, b, err := r.ix.locate(id)
	if err != nil {
		return vt.EncodedRow{}, err
	}
	d, err := r.ix.block(path, b)
	if err != nil {
		return vt.EncodedRow{}, err
	}
	return d.rows[id-b.first], nil
}

// Line decodes row id into cells.
func (r *Reader) Line(id uint64) (uv.Line, error) {
	row, err := r.Row(id)
	if err != nil {
		return nil, err
	}
	return vt.DecodeScrollbackRow(row.Data), nil
}

// Text returns the plain text of row id.
func (r *Reader) Text(id uint64) (string, error) {
	row, err := r.Row(id)
	if err != nil {
		return "", err
	}
	return vt.ScrollbackRowText(row.Data), nil
}

// Wrapped reports whether row id carried on to the next one by autowrap.
func (r *Reader) Wrapped(id uint64) (bool, error) {
	row, err := r.Row(id)
	if err != nil {
		return false, err
	}
	return row.Wrapped(), nil
}

// blockCache is a small LRU of decoded blocks keyed by the block's first id.
type blockCache struct {
	mu  sync.Mutex
	max int
	ll  *list.List
	m   map[uint64]*list.Element
}

func newBlockCache(n int) *blockCache {
	if n <= 0 {
		n = 8
	}
	return &blockCache{max: n, ll: list.New(), m: map[uint64]*list.Element{}}
}

func (c *blockCache) get(first uint64) *decoded {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[first]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*decoded)
	}
	return nil
}

func (c *blockCache) put(d *decoded) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[d.first]; ok {
		c.ll.MoveToFront(e)
		return
	}
	c.m[d.first] = c.ll.PushFront(d)
	for c.ll.Len() > c.max {
		last := c.ll.Back()
		delete(c.m, last.Value.(*decoded).first)
		c.ll.Remove(last)
	}
}

func (c *blockCache) drop(first, end uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.m {
		if k >= first && k < end {
			c.ll.Remove(e)
			delete(c.m, k)
		}
	}
}
