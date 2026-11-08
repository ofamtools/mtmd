package coldlog

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"ofam.dev/mtmd/internal/vt"
)

const (
	magic         = "MTMCOLD1"
	footMagic     = "MTMCFOOT"
	fileVersion   = 1
	headerSize    = 24
	blockHdrSize  = 24
	footEntrySize = 32
	trailerSize   = 24
	maxBlockBytes = 64 << 20
	maxBlockRows  = 1 << 16
	segPrefix     = "seg-"
	segSuffix     = ".cold"
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Errors returned by reads.
var (
	// ErrTrimmed: the row was dropped by retention (it is older than First).
	ErrTrimmed = errors.New("coldlog: row trimmed")
	// ErrGap: the row was dropped because the writer's queue was full.
	ErrGap = errors.New("coldlog: row lost in a gap")
	// ErrNotYet: the row is newer than anything on disk.
	ErrNotYet = errors.New("coldlog: row not written yet")
	// ErrCorrupt: a block failed its checksum or did not decode.
	ErrCorrupt = errors.New("coldlog: corrupt block")
)

// blockRef locates one block in a segment.
type blockRef struct {
	off    int64
	clen   uint32
	crc    uint32
	first  uint64
	n      uint32
	rawLen uint32
}

func (b blockRef) end() uint64 { return b.first + uint64(b.n) }

func putHeader(first uint64) []byte {
	h := make([]byte, headerSize)
	copy(h, magic)
	binary.LittleEndian.PutUint32(h[8:], fileVersion)
	binary.LittleEndian.PutUint32(h[12:], vt.ScrollbackFormatVersion)
	binary.LittleEndian.PutUint64(h[16:], first)
	return h
}

// parseHeader returns the first row id; ok is false for a foreign or newer
// file, readable false when only the vt format version differs.
func parseHeader(h []byte) (first uint64, ok bool) {
	if len(h) < headerSize || string(h[:8]) != magic {
		return 0, false
	}
	if binary.LittleEndian.Uint32(h[8:]) != fileVersion || binary.LittleEndian.Uint32(h[12:]) != vt.ScrollbackFormatVersion {
		return 0, false
	}
	return binary.LittleEndian.Uint64(h[16:]), true
}

func putBlockHdr(dst []byte, b blockRef) {
	binary.LittleEndian.PutUint32(dst[0:], b.clen)
	binary.LittleEndian.PutUint32(dst[4:], b.crc)
	binary.LittleEndian.PutUint64(dst[8:], b.first)
	binary.LittleEndian.PutUint32(dst[16:], b.n)
	binary.LittleEndian.PutUint32(dst[20:], b.rawLen)
}

func getBlockHdr(src []byte) blockRef {
	return blockRef{
		clen:   binary.LittleEndian.Uint32(src[0:]),
		crc:    binary.LittleEndian.Uint32(src[4:]),
		first:  binary.LittleEndian.Uint64(src[8:]),
		n:      binary.LittleEndian.Uint32(src[16:]),
		rawLen: binary.LittleEndian.Uint32(src[20:]),
	}
}

func (b blockRef) sane() bool {
	return b.n > 0 && b.n <= maxBlockRows && b.clen > 0 && b.clen <= maxBlockBytes && b.rawLen <= maxBlockBytes
}

// appendRow appends the record of one row to dst.
func appendRow(dst []byte, r vt.EncodedRow) []byte {
	dst = binary.AppendUvarint(dst, uint64(r.Width))
	dst = append(dst, r.Flags)
	dst = binary.AppendUvarint(dst, uint64(len(r.Data)))
	return append(dst, r.Data...)
}

// decodeRows parses n row records out of raw; the rows' Data alias raw.
func decodeRows(raw []byte, n int) ([]vt.EncodedRow, error) {
	rows := make([]vt.EncodedRow, 0, n)
	for len(rows) < n {
		w, k := binary.Uvarint(raw)
		if k <= 0 || len(raw) < k+1 {
			return nil, ErrCorrupt
		}
		raw = raw[k:]
		flags := raw[0]
		raw = raw[1:]
		l, k := binary.Uvarint(raw)
		if k <= 0 || uint64(len(raw)-k) < l {
			return nil, ErrCorrupt
		}
		rows = append(rows, vt.EncodedRow{Data: raw[k : k+int(l) : k+int(l)], Width: int(w), Flags: flags})
		raw = raw[k+int(l):]
	}
	return rows, nil
}

// compressor reuses one gzip writer and buffer.
type compressor struct {
	zw  *gzip.Writer
	buf bytes.Buffer
}

func newCompressor(level int) (*compressor, error) {
	c := &compressor{}
	zw, err := gzip.NewWriterLevel(&c.buf, level)
	if err != nil {
		return nil, err
	}
	c.zw = zw
	return c, nil
}

func (c *compressor) compress(raw []byte) ([]byte, error) {
	c.buf.Reset()
	c.zw.Reset(&c.buf)
	if _, err := c.zw.Write(raw); err != nil {
		return nil, err
	}
	if err := c.zw.Close(); err != nil {
		return nil, err
	}
	return c.buf.Bytes(), nil
}

func gunzip(payload []byte, rawLen uint32) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	defer zr.Close()
	raw := make([]byte, 0, rawLen)
	b := bytes.NewBuffer(raw)
	if _, err := io.Copy(b, io.LimitReader(zr, maxBlockBytes+1)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if uint32(b.Len()) != rawLen {
		return nil, ErrCorrupt
	}
	return b.Bytes(), nil
}

// footer encodes the table and trailer for blocks (offsets included).
func footer(blocks []blockRef, tableOff int64) []byte {
	out := make([]byte, 0, len(blocks)*footEntrySize+trailerSize)
	for _, b := range blocks {
		var e [footEntrySize]byte
		binary.LittleEndian.PutUint64(e[0:], uint64(b.off))
		putBlockHdr(e[8:], b)
		out = append(out, e[:]...)
	}
	crc := crc32.Checksum(out, crcTable)
	var t [trailerSize]byte
	binary.LittleEndian.PutUint64(t[0:], uint64(tableOff))
	binary.LittleEndian.PutUint32(t[8:], uint32(len(blocks)))
	binary.LittleEndian.PutUint32(t[12:], crc)
	copy(t[16:], footMagic)
	return append(out, t[:]...)
}

// parseFooter reads the trailer and table from a whole file's bytes via
// readAt; ok is false when there is no valid footer.
func parseFooter(r io.ReaderAt, size int64) (blocks []blockRef, tableOff int64, ok bool) {
	if size < headerSize+trailerSize {
		return nil, 0, false
	}
	var t [trailerSize]byte
	if _, err := r.ReadAt(t[:], size-trailerSize); err != nil || string(t[16:]) != footMagic {
		return nil, 0, false
	}
	tableOff = int64(binary.LittleEndian.Uint64(t[0:]))
	n := int64(binary.LittleEndian.Uint32(t[8:]))
	crc := binary.LittleEndian.Uint32(t[12:])
	if tableOff < headerSize || tableOff+n*footEntrySize != size-trailerSize {
		return nil, 0, false
	}
	tab := make([]byte, n*footEntrySize)
	if _, err := r.ReadAt(tab, tableOff); err != nil || crc32.Checksum(tab, crcTable) != crc {
		return nil, 0, false
	}
	blocks = make([]blockRef, n)
	for i := range blocks {
		e := tab[i*footEntrySize:]
		b := getBlockHdr(e[8:])
		b.off = int64(binary.LittleEndian.Uint64(e[0:]))
		if !b.sane() {
			return nil, 0, false
		}
		blocks[i] = b
	}
	return blocks, tableOff, true
}

// scanBlocks walks the blocks of an unsealed segment from the header on and
// returns the valid ones and the offset just past the last. It stops at the
// first short, insane, out-of-order or checksum-failing block.
func scanBlocks(r io.ReaderAt, size int64, first uint64) (blocks []blockRef, end int64) {
	pos := int64(headerSize)
	next := first
	var hdr [blockHdrSize]byte
	for pos+blockHdrSize <= size {
		if _, err := r.ReadAt(hdr[:], pos); err != nil {
			break
		}
		b := getBlockHdr(hdr[:])
		if !b.sane() || b.first < next || pos+blockHdrSize+int64(b.clen) > size {
			break
		}
		payload := make([]byte, b.clen)
		if _, err := r.ReadAt(payload, pos+blockHdrSize); err != nil || crc32.Checksum(payload, crcTable) != b.crc {
			break
		}
		b.off = pos
		blocks = append(blocks, b)
		pos += blockHdrSize + int64(b.clen)
		next = b.end()
	}
	return blocks, pos
}
