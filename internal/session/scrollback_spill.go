package session

import (
	"ofam.dev/mtmd/internal/vt"
)

// Spilling the ring into the cold log.
//
// The cold log holds the rows the pane's ring let go of. When the ring itself
// is about to go (the daemon stops and restarts, a handoff carries only the
// newest rows, the pane is closed), the rows of the ring that the next owner
// will not hold are written to the log first, so no row falls between the log
// and what comes back. The rows stay in the ring too, and are counted in
// paneCold.skip so that evicting them later (a handoff that was abandoned)
// does not write them a second time.

// scrollbackRowSource is an emulator that hands out its scrollback rows as the
// evict hook would have: the pure Go emulator is one, libghostty is not.
type scrollbackRowSource interface {
	ScrollbackLen() int
	ScrollbackEncodedRow(index int) (vt.EncodedRow, bool)
}

// spillChunk is how many rows go to the log's queue between flushes, well
// under the queue's size so a large ring is not dropped as a flood.
const spillChunk = 2048

// noteTail records the id of the first row of the history file just saved,
// ok false when there is none. A restart restores the ring from that file.
func (c *paneCold) noteTail(first uint64, ok bool) {
	if c == nil {
		return
	}
	c.tail.Store(first)
	c.tailOK.Store(ok)
}

// hasLog reports whether the pane has a log, open or on disk.
func (c *paneCold) hasLog() bool {
	c.mu.RLock()
	open := c.log != nil
	c.mu.RUnlock()
	return open || c.diskNext() > 0
}

// spillCold writes the rows of the pane's ring older than the id target
// picks to its log. target is given the ids of the ring's first row and one
// past its last; it reports where the log should end and whether to write at
// all. Nothing is written for a pane without a log hook or a ring that
// cannot give its rows out.
func (p *PTY) spillCold(target func(start, end uint64) (uint64, bool)) {
	c := p.cold
	if c == nil {
		return
	}
	p.terminalMu.Lock()
	defer p.terminalMu.Unlock()
	src, ok := p.terminal.(scrollbackRowSource)
	if p.terminal == nil || !ok {
		return
	}
	start := c.ringStart()
	end := start + uint64(src.ScrollbackLen())
	to, ok := target(start, end)
	if !ok || to <= start {
		return
	}
	to = min(to, end)
	l := c.openLog()
	if l == nil {
		return
	}
	next := l.Next()
	if next < start {
		l.Skip(start - next)
		next = l.Next()
	}
	for id := next; id < to; id++ {
		row, ok := src.ScrollbackEncodedRow(int(id - start))
		if !ok {
			break
		}
		l.Append(row)
		if (id-next+1)%spillChunk == 0 {
			l.Flush()
		}
	}
	// The log now holds the ring's first rows: skip them when they leave it.
	c.skip.Store(int64(l.Next() - start))
}

// spillColdAtClose is closeAs's step before the emulator goes. A pane that is
// gone for good (reason) leaves its log whole, with the rows still in its ring,
// for the grace period. A daemon that is stopping writes the rows older than
// the history file it saved last, so the file and the log meet.
func (p *PTY) spillColdAtClose(reason string) {
	c := p.cold
	if c == nil {
		return
	}
	if reason != "" {
		if c.pol.GraceDays <= 0 || !c.hasLog() {
			return
		}
		p.spillCold(func(_, end uint64) (uint64, bool) { return end, true })
		return
	}
	s := p.owner()
	if s == nil || !s.historyPolicy().Enabled || !c.tailOK.Load() {
		return
	}
	tail := c.tail.Load()
	p.spillCold(func(_, _ uint64) (uint64, bool) { return tail, true })
}

// spillColdForHandoff writes the ring rows the successor will not carry: the
// snapshot holds the newest handoffHistoryRows.
func (p *PTY) spillColdForHandoff() {
	p.spillCold(func(start, end uint64) (uint64, bool) {
		return end - min(end-start, handoffHistoryRows), true
	})
}
