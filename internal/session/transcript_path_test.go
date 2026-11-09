package session

import "testing"

// get-window and get-agent-state name the transcript file a pane's agent
// conversation is read from, and say nothing when none is joined (D4).
func TestTranscriptPathIsReported(t *testing.T) {
	d, sp := startTestDaemon(t)
	sess := makeSessionWithWindow(t, d, "work")
	c := dialVerb(t, sp)
	win := sess.GetState().Windows[0].ID

	for _, verb := range []string{"get-window", "get-agent-state"} {
		got := result(t, c.call(t, `{"id":1,"verb":"`+verb+`","params":{"session":"work","window":"`+win+`"}}`))
		if _, has := got["transcript_path"]; has {
			t.Fatalf("%s reported a transcript path before any join: %v", verb, got["transcript_path"])
		}
	}

	sess.SetTranscriptWatcher(newFakeWatch())
	path := writeTranscript(t, t.TempDir(), "a.jsonl", assistantRec(t, "tool_use", "/p", "2.1.222"))
	if err := sess.JoinAgentTranscript(win, "claude-code", path, true); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"get-window", "get-agent-state"} {
		got := result(t, c.call(t, `{"id":2,"verb":"`+verb+`","params":{"session":"work","window":"`+win+`"}}`))
		if got["transcript_path"] != path {
			t.Fatalf("%s transcript_path = %v, want %s", verb, got["transcript_path"], path)
		}
	}
}
