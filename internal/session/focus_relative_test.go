package session

import "testing"

// focus-window --relative takes previous as well as prev (D2 of the soak
// report, found by mtm sending the word its own check accepts).
func TestFocusWindowRelativeAcceptsPrevious(t *testing.T) {
	d, sp := startTestDaemon(t)
	sess := makeSessionWithWindow(t, d, "work")
	if _, err := sess.AddDaemonWindow("Second", nil); err != nil {
		t.Fatal(err)
	}
	c := dialVerb(t, sp)
	ids := []string{sess.GetState().Windows[0].ID, sess.GetState().Windows[1].ID}
	focused := func() string { return sess.GetState().FocusedWindowID }

	result(t, c.call(t, `{"id":1,"verb":"focus-window","params":{"session":"work","window":"`+ids[0]+`"}}`))
	for _, word := range []string{"previous", "prev"} {
		result(t, c.call(t, `{"id":2,"verb":"focus-window","params":{"session":"work","relative":"next"}}`))
		start := focused()
		result(t, c.call(t, `{"id":3,"verb":"focus-window","params":{"session":"work","relative":"`+word+`"}}`))
		if focused() == start {
			t.Fatalf("relative %q did not move the focus off %s", word, start)
		}
	}
	mustRefuse(t, c.call(t, `{"id":4,"verb":"focus-window","params":{"session":"work","relative":"sideways"}}`), ErrVerbInvalidParams, "an unknown relative move")
}
