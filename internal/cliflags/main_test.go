package cliflags

import (
	"os"
	"testing"

	"ofam.dev/mtmd/internal/testutil"
)

// TestMain isolates the whole test binary from the developer's own XDG
// directories, as every test package here does. See testutil.RunIsolated.
func TestMain(m *testing.M) { os.Exit(testutil.RunIsolated(m)) }
