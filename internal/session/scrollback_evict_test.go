package session

import (
	"strings"
	"testing"

	"ofam.dev/mtmd/internal/vt"
)

func TestSetScrollbackEvictHookOnPureEmulator(t *testing.T) {
	t.Parallel()
	term := vt.NewEmulator(20, 3)
	term.SetScrollbackMaxLines(5)
	var got []string
	if !setScrollbackEvictHook(term, func(r vt.EncodedRow) { got = append(got, vt.ScrollbackRowText(r.Data)) }) {
		t.Fatal("the pure emulator should accept the hook")
	}
	term.Write([]byte(strings.Repeat("row\r\n", 20)))
	if len(got) == 0 || got[0] != "row" {
		t.Fatalf("evicted %q", got)
	}
	if setScrollbackEvictHook(noEvictor{}, nil) {
		t.Fatal("a terminal without the method must report false")
	}
}

type noEvictor struct{ vt.Terminal }
