package session

const optionInheritPaneSize = "appearance.new_window_inherit_size"

// inheritedPaneSize supplies content cells, never a window's bordered box.
// A focused peer is the best measurement for a not-yet-placed pane. If it has
// gone, retain the last area any view announced rather than the session's
// nominal box (which a stream viewer does not change).
func (s *Session) inheritedPaneSize() (int, int) {
	if _, ok := s.GetOption(optionInheritPaneSize); ok {
		if on, _ := (sessionOptions{sess: s}).getBool(optionInheritPaneSize); !on {
			return 0, 0
		}
	} else if s.config != nil && s.config.InheritSize != nil && !s.config.InheritSize() {
		return 0, 0
	}
	s.stateMu.RLock()
	ptyID := ""
	if s.state != nil {
		if win, ok := findWindowState(s.state, s.state.FocusedWindowID); ok {
			ptyID = win.PTYID
		}
	}
	s.stateMu.RUnlock()
	if p := s.GetPTY(ptyID); p != nil {
		if w, h := p.Size(); w > 0 && h > 0 {
			return w, h
		}
	}
	s.sizeMu.RLock()
	defer s.sizeMu.RUnlock()
	return s.paneWidth, s.paneHeight
}

// noteShownSize records the applied content size for the next unplaced pane.
// All per-pane size announcements, direct and leased, pass through here.
func (p *PTY) noteShownSize(width, height int) {
	if width <= 0 || height <= 0 {
		return
	}
	if s := p.owner(); s != nil {
		s.sizeMu.Lock()
		s.paneWidth, s.paneHeight = width, height
		s.sizeMu.Unlock()
	}
}
