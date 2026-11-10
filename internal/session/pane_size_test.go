package session

import (
	"testing"
	"time"

	"ofam.dev/mtmd/internal/config"
)

// A verb-created pane must start like its session's already displayed panes,
// even when the session itself still has its headless 80x24 placeholder.
func TestNewPaneInheritsDisplayedContentSize(t *testing.T) {
	_, sess := newTestDaemonSession(t)
	first, err := sess.AddDaemonWindowWith(NewWindowOptions{Focus: true, Command: []string{"cat"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.GetPTY(first.PTYID).Resize(151, 37); err != nil {
		t.Fatal(err)
	}
	second, err := sess.AddDaemonWindowWith(NewWindowOptions{Command: []string{"cat"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := sess.GetPTY(second.PTYID).Size(); w != 151 || h != 37 {
		t.Fatalf("unfocused, unplaced pane starts at %dx%d; want displayed content size 151x37", w, h)
	}
	if !second.Unplaced || sess.GetState().FocusedWindowID != first.ID {
		t.Fatal("inheriting a size changed placement or focus")
	}
}

func TestNewPaneHonorsExplicitContentSize(t *testing.T) {
	_, sess := newTestDaemonSession(t)
	// Explicit content dimensions must not be capped by a nominal session box.
	explicit, err := sess.AddDaemonWindowWith(NewWindowOptions{Cols: 190, Rows: 51, Command: []string{"cat"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := sess.GetPTY(explicit.PTYID).Size(); w != 190 || h != 51 {
		t.Fatalf("explicit pane starts at %dx%d; want 190x51", w, h)
	}
}

func TestNewPaneKeepsLastDisplayedSizeWithoutPeers(t *testing.T) {
	_, sess := newTestDaemonSession(t)
	first, err := sess.AddDaemonWindowWith(NewWindowOptions{Focus: true, Command: []string{"cat"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.GetPTY(first.PTYID).Resize(143, 43); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.CloseDaemonWindow(first.ID); err != nil {
		t.Fatal(err)
	}
	next, err := sess.AddDaemonWindowWith(NewWindowOptions{Command: []string{"cat"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := sess.GetPTY(next.PTYID).Size(); w != 143 || h != 43 {
		t.Fatalf("pane without peers starts at %dx%d; want last displayed size 143x43", w, h)
	}
}

func TestNewPaneSizeOptOutAndSessionOverride(t *testing.T) {
	off := false
	d := NewDaemon(&DaemonConfig{NewWindowInheritSize: &off})
	sess, err := d.manager.CreateSession("size-options", &SessionConfig{}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.manager.Shutdown)
	first, err := sess.AddDaemonWindowWith(NewWindowOptions{Focus: true, Command: []string{"cat"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.GetPTY(first.PTYID).Resize(151, 37); err != nil {
		t.Fatal(err)
	}
	check := func(w, h int) {
		t.Helper()
		win, err := sess.AddDaemonWindowWith(NewWindowOptions{Command: []string{"cat"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cw, ch := sess.GetPTY(win.PTYID).Size(); cw != w || ch != h {
			t.Fatalf("new pane is %dx%d; want %dx%d", cw, ch, w, h)
		}
	}
	check(78, 22)
	// File reload reaches sessions that already exist.
	d.applyUserConfig(config.DefaultConfig(), false)
	check(151, 37)
	// Session overrides use the registry's boolean syntax and take precedence.
	sess.SetOption(optionInheritPaneSize, "off")
	check(78, 22)
	sess.SetOption(optionInheritPaneSize, "on")
	check(151, 37)
}

// Exercise the real stream boundary, not SetViewerLease in isolation: opening a
// fit stream already establishes whether the session has attached TUI clients.
func TestPaneFitStreamFollowsShownArea(t *testing.T) {
	d, socket := startTestDaemon(t)
	sess := makeSessionWithWindow(t, d, "fit-area")
	win := sess.GetState().Windows[0]
	c, err := DialVerbClientAt(socket, "")
	if err != nil {
		t.Fatal(err)
	}
	stream, _, err := c.OpenPaneStream(map[string]any{"session": "fit-area", "window": win.ID, "fit": true, "lease_cols": 151, "lease_rows": 37})
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	defer stream.Close()
	pty := sess.GetPTY(win.PTYID)
	want := func(w, h int) {
		t.Helper()
		end := time.Now().Add(2 * time.Second)
		for time.Now().Before(end) {
			if cw, ch := pty.Size(); cw == w && ch == h {
				return
			}
			time.Sleep(time.Millisecond)
		}
		cw, ch := pty.Size()
		t.Fatalf("shown pane is %dx%d; want %dx%d", cw, ch, w, h)
	}
	want(151, 37)
	if err := stream.Lease(197, 53); err != nil {
		t.Fatal(err)
	}
	want(197, 53)
	if err := stream.Lease(67, 19); err != nil {
		t.Fatal(err)
	}
	want(67, 19)
}
