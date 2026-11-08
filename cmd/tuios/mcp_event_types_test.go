package main

import (
	"slices"
	"testing"

	"ofam.dev/mtmd/internal/mcp"
	"ofam.dev/mtmd/internal/session"
)

// TestMCPEventsAcceptEverySubscribeType: tuios_events offered twelve of the
// daemon's event types, so an agent could not wait for a command to finish or
// a prompt to appear. The MCP package does not import the daemon, so its copy
// of the list is held to the daemon's here.
func TestMCPEventsAcceptEverySubscribeType(t *testing.T) {
	got := slices.Sorted(slices.Values(mcp.EventTypes))
	want := slices.Sorted(slices.Values(session.EventTypeNames))
	if !slices.Equal(got, want) {
		t.Errorf("tuios_events accepts %v\nsubscribe accepts %v", got, want)
	}
}
