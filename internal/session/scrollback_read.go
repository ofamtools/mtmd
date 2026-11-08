package session

import (
	"errors"
	"fmt"
	"image/color"
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"ofam.dev/mtmd/internal/coldlog"
	"ofam.dev/mtmd/internal/vt"
)

// Reading history back from disk (scrollback-disk lane M5).
//
// A pane whose ring has dropped rows keeps them in its cold log (see
// scrollback_cold.go). Row ids are global: the log holds ids [first, end), the
// ring holds end, end+1, ... (end is the count of rows ever evicted), and the
// screen follows the ring. The readers here (capture-pane, search-panes) join
// the log and the ring into one history, as the ring alone would have held it.
//
// A reader takes a coldView under the pane's terminalMu, in the same hold that
// reads the ring, so the two halves meet at the same row. The log's rows are
// immutable, so they are read after the lock is let go: a slow disk never holds
// the pane's output back.

// coldView is the part of a pane's history that lives only on disk: ids
// [first, end). The zero value has no rows.
type coldView struct {
	rd         *coldlog.Reader
	first, end uint64
}

// rows is how many row ids the view spans (gaps included).
func (v coldView) rows() int {
	if v.rd == nil || v.end <= v.first {
		return 0
	}
	return int(v.end - v.first)
}

// view flushes the log and returns what is on disk now. The caller holds the
// pane's terminalMu (read or write), so no row is evicted meanwhile and end is
// the id of the ring's oldest row.
func (c *paneCold) view() coldView {
	if c == nil {
		return coldView{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.log == nil {
		return coldView{}
	}
	c.log.Flush()
	rd := c.log.Reader()
	return coldView{rd: rd, first: rd.First(), end: rd.Next()}
}

// coldGapText is the line that stands for a run of n rows the log lost.
func coldGapText(n uint64) string {
	if n == 1 {
		return "[scrollback: 1 row missing from the disk log]"
	}
	return fmt.Sprintf("[scrollback: %d rows missing from the disk log]", n)
}

// coldLines reads a view's rows as lines. A run of lost rows (coldlog.ErrGap)
// is one marker line. With unwrap, soft-wrapped rows join into one line the way
// unwrapRows joins them.
type coldLines struct {
	coldView
	gaps   []coldlog.Gap
	ansi   bool
	unwrap bool
	width  int
}

func newColdLines(v coldView, ansiOut, unwrap bool, width int) *coldLines {
	return &coldLines{coldView: v, gaps: v.rd.Stats().Gaps, ansi: ansiOut, unwrap: unwrap, width: width}
}

// gapAt returns how many ids from id on are lost, 0 when id is readable.
func (c *coldLines) gapAt(id uint64) uint64 {
	i := sort.Search(len(c.gaps), func(i int) bool { return c.gaps[i].First+c.gaps[i].N > id })
	if i < len(c.gaps) && c.gaps[i].First <= id {
		return c.gaps[i].First + c.gaps[i].N - id
	}
	return 0
}

// rowText renders one stored row the way the ring renders its own.
func (c *coldLines) rowText(r vt.EncodedRow) string {
	if c.ansi {
		return vt.StripSixelMarkers(vt.DecodeScrollbackRow(r.Data).Render())
	}
	return vt.StripSixelMarkers(vt.ScrollbackRowText(r.Data))
}

// walk visits the lines made of rows [from, to). visit gets the id of the
// line's first row, the id after its last, its text (only when text is set)
// and open: the line's last row soft-wrapped and is the last row of the log,
// so the line carries on into the ring. visit returns false to stop.
func (c *coldLines) walk(from, to uint64, text bool, visit func(start, next uint64, text string, open bool) bool) {
	to = min(to, c.end)
	from = max(from, c.first)
	var cur strings.Builder
	for id := from; id < to; {
		if n := c.gapAt(id); n > 0 {
			next := min(id+n, to)
			if !visit(id, next, coldGapText(next-id), false) {
				return
			}
			id = next
			continue
		}
		start := id
		cur.Reset()
		for id < to {
			row, err := c.rd.Row(id)
			if err != nil {
				if id == start && errors.Is(err, coldlog.ErrTrimmed) {
					// Retention took it while we read: not part of the history.
					id++
					start = id
					continue
				}
				if id == start {
					// Unreadable block: a marker, like a gap.
					id++
					cur.WriteString(coldGapText(1))
				}
				break
			}
			var t string
			if text {
				t = c.rowText(row)
				cur.WriteString(t)
			}
			id++
			if !c.unwrap || !row.Wrapped() {
				break
			}
			if id < to && c.gapAt(id) > 0 {
				break // a lost run follows: the line ends here
			}
			if text {
				fill := c.width - ansi.StringWidth(t)
				if row.Padded() {
					fill--
				}
				if fill > 0 {
					cur.WriteString(strings.Repeat(" ", fill))
				}
			}
			if id == to {
				visit(start, id, cur.String(), true)
				return
			}
		}
		if id == start {
			continue // nothing was read (trimmed away)
		}
		if !visit(start, id, cur.String(), false) {
			return
		}
	}
}

// index lists where each line starts, and the open line's text. starts has
// one entry per line; open says the last line carries on into the ring.
type coldIndex struct {
	starts []uint64
	open   bool
}

func (c *coldLines) index() coldIndex {
	var ix coldIndex
	c.walk(c.first, c.end, false, func(start, _ uint64, _ string, open bool) bool {
		ix.starts = append(ix.starts, start)
		ix.open = open
		return true
	})
	return ix
}

// captureSel is the part of a capture the caller will keep, so the reader can
// leave the rest of the log unread.
type captureSel struct {
	start, end, lines int // 1-based inclusive region, or the last lines
	trim              bool
}

// captureWithCold is the recent capture (history, then screen) with the rows
// the ring no longer holds read back from the pane's cold log in front of
// them. ok is false when the pane has no such rows; the caller then takes the
// ring-only capture. lead is how many lines of the whole capture are left out
// at the front, because sel does not need them: the caller shifts a start/end
// region by it. The content equals what a pane whose ring held everything
// would give for the same sel.
func (p *PTY) captureWithCold(ansiOut, unwrap bool, sel captureSel, palette *[16]color.Color) (content string, meta paneMeta, lead int, ok bool) {
	if sel.end > 0 && sel.end < sel.start {
		return "", paneMeta{}, 0, false
	}
	p.terminalMu.RLock()
	if p.terminal == nil {
		p.terminalMu.RUnlock()
		return "", paneMeta{}, 0, false
	}
	v := p.cold.view()
	if v.rows() == 0 {
		p.terminalMu.RUnlock()
		return "", paneMeta{}, 0, false
	}
	ring := p.captureContent(true, ansiOut)
	if unwrap {
		ring = unwrapCapture(p.terminal, ring, true)
	}
	meta = p.metaLocked()
	width := p.terminal.Width()
	// While the alternate screen is up and the ring is empty, the ring's first
	// row is the alternate screen's: the last cold row does not wrap into it.
	seam := unwrap && !(p.terminal.ScrollbackLen() == 0 && p.terminal.ActiveScreenIsAlt())
	p.terminalMu.RUnlock()

	meta.HistoryRows += v.rows()
	meta.ColdRows = v.rows()
	cl := newColdLines(v, ansiOut, unwrap, width)
	k := captureLineCount(ring)

	ranged := sel.start > 0 || sel.end > 0
	tail := !ranged && sel.lines > 0 && k > 0
	full := !ranged && !tail

	var ix coldIndex
	if !full {
		ix = cl.index()
	}
	var out strings.Builder
	var openText string
	open := false
	if full {
		// One pass, no index: render every line.
		cl.walk(v.first, v.end, true, func(_, _ uint64, t string, o bool) bool {
			if o && seam {
				openText, open = t, true
				return true
			}
			out.WriteString(t)
			out.WriteByte('\n')
			return true
		})
	} else {
		lc := len(ix.starts)
		if ix.open && seam {
			open = true
			lc--
		}
		a, b := 0, lc // complete cold lines to render: [a, b)
		switch {
		case ranged:
			a = max(sel.start-1, 0)
			if sel.end > 0 {
				b = min(sel.end, lc)
			}
		case tail:
			a = max(lc-(sel.lines-k), 0)
		}
		if a > b {
			a = b
		}
		lead = a
		endID := func(i int) uint64 {
			if i+1 < len(ix.starts) {
				return ix.starts[i+1]
			}
			return v.end
		}
		if b > a {
			cl.walk(ix.starts[a], endID(b-1), true, func(_, _ uint64, t string, _ bool) bool {
				out.WriteString(t)
				out.WriteByte('\n')
				return true
			})
		}
		if open {
			cl.walk(ix.starts[lc], v.end, true, func(_, _ uint64, t string, _ bool) bool {
				openText = t
				return true
			})
		}
		// A region that ends inside the cold lines does not reach the ring.
		if ranged && sel.end > 0 && sel.end <= lc {
			res := out.String()
			if !strings.HasSuffix(ring, "\n") {
				res = strings.TrimSuffix(res, "\n")
			}
			return p.finishCold(res, palette), meta, lead, true
		}
	}
	out.WriteString(openText)
	out.WriteString(ring)
	return p.finishCold(out.String(), palette), meta, lead, true
}

// finishCold resolves the colours of a composed capture when asked.
func (p *PTY) finishCold(content string, palette *[16]color.Color) string {
	if palette != nil {
		return ResolveSGR(content, *palette)
	}
	return content
}
