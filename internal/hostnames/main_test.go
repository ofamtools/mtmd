package hostnames_test

import (
	"os"
	"testing"

	"ofam.dev/mtmd/internal/testutil"
)

// TestMain isolates the whole test binary from the developer's own XDG
// directories. See testutil.RunIsolated for why this cannot be a per-test
// helper.
//
// This has to be an external test package (hostnames_test, not hostnames):
// testutil imports hostnames, so an in-package test file importing testutil
// back would be a cycle.
func TestMain(m *testing.M) { os.Exit(testutil.RunIsolated(m)) }
