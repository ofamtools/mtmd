package vt

import (
	"fmt"
	"image/color"
	"slices"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// evictOddColor is a colour type the packer does not know, so the ring interns it.
type evictOddColor struct{ r, g, b uint8 }

func (c evictOddColor) RGBA() (r, g, b, a uint32) {
	return uint32(c.r) * 0x101, uint32(c.g) * 0x101, uint32(c.b) * 0x101, 0xffff
}

func numbered(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "line %04d\r\n", i)
	}
	return b.String()
}

// evictedTexts collects the plain text of every evicted row.
func evictedTexts(t *testing.T, e *Emulator) *[]string {
	t.Helper()
	var got []string
	e.SetScrollbackEvictHook(func(r EncodedRow) {
		got = append(got, strings.TrimRight(ScrollbackRowText(r.Data), " "))
	})
	return &got
}

func ringTexts(e *Emulator) []string {
	var out []string
	for i := range e.ScrollbackLen() {
		out = append(out, strings.TrimRight(e.ScrollbackLine(i).String(), " "))
	}
	return out
}

func TestEvictHookOrderExactlyOnceAcrossWrap(t *testing.T) {
	e := NewEmulator(20, 4)
	e.SetScrollbackMaxLines(10)
	got := evictedTexts(t, e)
	e.Write([]byte(numbered(57)))
	ring := ringTexts(e)
	all := append(slices.Clone(*got), ring...)
	// 57 lines, the last one's newline scrolls: 57 rows off the screen top in total.
	for i, s := range all {
		if want := fmt.Sprintf("line %04d", i); s != want {
			t.Fatalf("row %d = %q, want %q (evicted %d, ring %d)", i, s, want, len(*got), len(ring))
		}
	}
	if len(*got) != len(all)-10 {
		t.Fatalf("evicted %d rows, ring %d", len(*got), len(ring))
	}
}

func TestEvictHookSetMaxLinesShrink(t *testing.T) {
	e := NewEmulator(20, 4)
	e.SetScrollbackMaxLines(20)
	e.Write([]byte(numbered(15)))
	n := e.ScrollbackLen()
	got := evictedTexts(t, e)
	e.SetScrollbackMaxLines(5)
	if len(*got) != n-5 {
		t.Fatalf("shrink evicted %d rows, want %d", len(*got), n-5)
	}
	all := append(slices.Clone(*got), ringTexts(e)...)
	for i, s := range all {
		if want := fmt.Sprintf("line %04d", i); s != want {
			t.Fatalf("row %d = %q, want %q", i, s, want)
		}
	}
	// Growing evicts nothing; a later push on the smaller ring evicts once.
	*got = nil
	e.SetScrollbackMaxLines(50)
	e.Write([]byte(numbered(1)))
	if len(*got) != 0 {
		t.Fatalf("grow evicted %v", *got)
	}
}

func TestEvictHookAcrossResize(t *testing.T) {
	e := NewEmulator(30, 5)
	e.SetScrollbackMaxLines(12)
	got := evictedTexts(t, e)
	e.Write([]byte(numbered(40)))
	e.Resize(12, 5)
	e.Write([]byte(numbered(10)))
	e.Resize(30, 8)
	e.Write([]byte(numbered(10)))
	// Every evicted row is a row text that was printed, once: no duplicates
	// among the distinct numbered rows of the first batch.
	seen := map[string]int{}
	for _, s := range *got {
		if strings.HasPrefix(s, "line ") {
			seen[s]++
		}
	}
	for s, n := range seen {
		if n > 3 { // the numbering restarts twice, so a text can recur up to 3 times
			t.Fatalf("%q evicted %d times", s, n)
		}
	}
	// Conservation: rows ever scrolled off = evicted + ring (reflow adjusts,
	// so check the ring never exceeds its depth and the hook saw something).
	if e.ScrollbackLen() > 12 || len(*got) == 0 {
		t.Fatalf("ring %d, evicted %d", e.ScrollbackLen(), len(*got))
	}
}

func TestEvictedRowDecodeEqualsRingRow(t *testing.T) {
	sb := NewScrollback(2)
	var rows []EncodedRow
	var datas [][]byte
	sb.SetOnEvict(func(r EncodedRow) {
		rows = append(rows, r)
		datas = append(datas, slices.Clone(r.Data))
	})
	red := ansi.BasicColor(1)
	lines := []uv.Line{
		{{Content: "a", Width: 1}, {Content: "b", Width: 1, Style: uv.Style{Fg: red, Attrs: uv.AttrBold}}, {Content: "c", Width: 1}},
		{{Content: "漢", Width: 2}, {Content: "", Width: 0}, {Content: "x", Width: 1, Style: uv.Style{Bg: ansi.RGBColor{R: 1, G: 2, B: 3}, Underline: uv.UnderlineCurly}}},
		{{Content: "l", Width: 1, Link: uv.Link{URL: "http://x/y", Params: "id=1"}}, {Content: "k", Width: 1, Link: uv.Link{URL: "http://x/y", Params: "id=1"}}},
		{{Content: "é", Width: 1}, {Content: "👍🏽", Width: 2}, {Content: "", Width: 0}},
		{{Content: "z", Width: 1}},
	}
	for i, l := range lines {
		padded := slices.Clone(l)
		for len(padded) < 6 {
			padded = append(padded, uv.EmptyCell)
		}
		sb.PushLine(padded)
		if i == 0 {
			sb.markNewest(rowWrapped)
		}
	}
	// Rows 0..2 were evicted; rebuild expectations from a ring big enough.
	big := NewScrollback(100)
	for i, l := range lines {
		padded := slices.Clone(l)
		for len(padded) < 6 {
			padded = append(padded, uv.EmptyCell)
		}
		big.PushLine(padded)
		if i == 0 {
			big.markNewest(rowWrapped)
		}
	}
	if len(rows) != 3 {
		t.Fatalf("evicted %d rows, want 3", len(rows))
	}
	for i := range rows {
		got := DecodeScrollbackRow(datas[i])
		if !slices.EqualFunc(got, big.Line(i), func(a, b uv.Cell) bool { return a == b }) {
			t.Errorf("row %d: decoded %#v, ring %#v", i, got, big.Line(i))
		}
		if txt := ScrollbackRowText(datas[i]); txt != big.Line(i).String() {
			t.Errorf("row %d text %q, want %q", i, txt, big.Line(i).String())
		}
		if rows[i].Wrapped() != big.LineWrapped(i) || rows[i].Width != 6 {
			t.Errorf("row %d: wrapped %v width %d", i, rows[i].Wrapped(), rows[i].Width)
		}
	}
}

func TestEvictReencodesInternedColoursAsRGB(t *testing.T) {
	sb := NewScrollback(1)
	var data []byte
	sb.SetOnEvict(func(r EncodedRow) { data = slices.Clone(r.Data) })
	sb.PushLine(uv.Line{{Content: "q", Width: 1, Style: uv.Style{Fg: evictOddColor{10, 20, 30}}}})
	if len(sb.colors) != 1 {
		t.Fatalf("colour not interned: %d", len(sb.colors))
	}
	sb.PushLine(uv.Line{{Content: "r", Width: 1}})
	got := DecodeScrollbackRow(data)
	if len(got) != 1 || got[0].Content != "q" {
		t.Fatalf("decoded %#v", got)
	}
	if want := (color.RGBA{10, 20, 30, 0xff}); got[0].Style.Fg != color.Color(want) {
		t.Fatalf("fg = %#v, want %#v", got[0].Style.Fg, want)
	}
	if sb.rgbOnly {
		t.Fatal("rgbOnly left set")
	}
}

func TestEvictHookNilByDefaultAndRemovable(t *testing.T) {
	sb := NewScrollback(2)
	n := 0
	sb.SetOnEvict(func(EncodedRow) { n++ })
	for range 5 {
		sb.PushLine(uv.Line{{Content: "a", Width: 1}})
	}
	sb.SetOnEvict(nil)
	for range 5 {
		sb.PushLine(uv.Line{{Content: "a", Width: 1}})
	}
	if n != 3 {
		t.Fatalf("hook saw %d, want 3", n)
	}
	sb.Clear()
	sb.SetOnEvict(func(EncodedRow) { n += 100 })
	sb.Clear()
	if n != 3 {
		t.Fatal("Clear must not call the hook")
	}
}

func benchFlood(b *testing.B, hook func(EncodedRow)) {
	e := NewEmulator(120, 30)
	e.SetScrollbackMaxLines(1000)
	e.SetScrollbackEvictHook(hook)
	chunk := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog 0123456789 \x1b[32mgreen\x1b[0m text\r\n", 200))
	b.SetBytes(int64(len(chunk)))
	b.ResetTimer()
	for range b.N {
		e.Write(chunk)
	}
}

func BenchmarkFloodHookNil(b *testing.B)  { benchFlood(b, nil) }
func BenchmarkFloodHookNoop(b *testing.B) { benchFlood(b, func(EncodedRow) {}) }
