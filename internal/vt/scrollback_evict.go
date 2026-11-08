package vt

import (
	"encoding/binary"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
)

// ScrollbackFormatVersion is the version of the line encoding EncodedRow.Data
// holds and DecodeScrollbackRow reads. A reader of stored rows should refuse a
// version it was not written for.
const ScrollbackFormatVersion = 1

// EncodedRow is a scrollback line as the ring stores it, handed to the hook of
// SetOnEvict. Data is valid only during the call: copy it to keep it.
type EncodedRow struct {
	// Data is the line's encoding: self-contained, so DecodeScrollbackRow and
	// ScrollbackRowText need no ring. A colour of a type the emulator does not
	// produce, which the ring keeps in a table, is written as RGB.
	Data []byte
	// Width is the width the line was written at.
	Width int
	// Flags holds the row flags of the line; see Wrapped.
	Flags uint8
}

// Wrapped reports whether the line carried on to the next one by autowrap.
func (r EncodedRow) Wrapped() bool { return rowFlag(r.Flags)&rowWrapped != 0 }

// SetOnEvict sets fn to be called, in order and exactly once per line, with
// each line that leaves the old end of the ring: when a push overwrites the
// oldest line of a full ring (called just before it is overwritten), and for
// each line SetMaxLines drops when it shrinks the ring (oldest first, before
// the ring changes). It is called under the caller's emulator lock, so it must
// not block or call back into the emulator.
//
// Clear, Discard and a reflow taking back the newest lines are not evictions.
// A nil fn removes the hook; with none set a push costs one nil check.
func (sb *Scrollback) SetOnEvict(fn func(EncodedRow)) {
	sb.onEvict = fn
}

// evict reports the line in ring slot slot to the hook.
func (sb *Scrollback) evict(slot int) {
	data := sb.lines[slot]
	if len(sb.colors) > 0 {
		// The line may name a colour held in the ring's table. Rewrite it
		// with every such colour as RGB so the bytes stand alone.
		line := sb.decodeLine(data)
		n := len(line)
		for n > 0 && isBlankCell(&line[n-1]) {
			n--
		}
		sb.rgbOnly = true
		data = sb.encodeLine(nil, line[:n], len(line))
		sb.rgbOnly = false
	}
	w, _ := lineWidth(data)
	sb.onEvict(EncodedRow{Data: data, Width: w, Flags: uint8(sb.wraps[slot])})
}

// DecodeScrollbackRow decodes the Data of an EncodedRow into a line of its
// width, the same cells Scrollback.Line returns for it. A truncated record
// decodes as far as it can and the rest of the line is blank; nil when even
// the width is unreadable.
func DecodeScrollbackRow(data []byte) uv.Line {
	return new(Scrollback).decodeLine(data)
}

// ScrollbackRowText returns the plain text of an encoded row, as
// DecodeScrollbackRow(data).String() would, without decoding cells.
func ScrollbackRowText(data []byte) string {
	if _, n := binary.Uvarint(data); n <= 0 {
		return ""
	}
	var b strings.Builder
	new(Scrollback).appendLineText(&b, data)
	return b.String()
}
