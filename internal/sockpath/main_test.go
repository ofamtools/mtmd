package sockpath

import (
	"os"
	"testing"

	"ofam.dev/mtmd/internal/testutil"
)

// TestMain isolates the test binary from the developer's own XDG directories.
// See testutil.RunIsolated.
func TestMain(m *testing.M) { os.Exit(testutil.RunIsolated(m)) }
