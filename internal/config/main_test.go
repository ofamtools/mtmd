package config

import (
	"os"
	"testing"

	"ofam.dev/mtmd/internal/testutil"
)

// TestMain isolates the whole test binary from the developer's own XDG
// directories. See testutil.RunIsolated for why this cannot be a per-test
// helper.
func TestMain(m *testing.M) { os.Exit(testutil.RunIsolated(m)) }
