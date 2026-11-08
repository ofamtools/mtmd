package session

import "ofam.dev/mtmd/internal/vt"

// scrollbackEvictor is an emulator that reports each scrollback line it lets
// go of from its old end (vt.Scrollback.SetOnEvict). The pure Go emulator is
// one; libghostty-vt keeps its history inside the library and is not.
type scrollbackEvictor interface {
	SetScrollbackEvictHook(fn func(vt.EncodedRow))
}

// setScrollbackEvictHook installs fn on term and reports whether term can
// report evicted lines; nil removes the hook. The caller holds the pane's
// terminalMu, and fn runs under it, so it must only copy the row and return.
func setScrollbackEvictHook(term vt.Terminal, fn func(vt.EncodedRow)) bool {
	e, ok := term.(scrollbackEvictor)
	if ok {
		e.SetScrollbackEvictHook(fn)
	}
	return ok
}
