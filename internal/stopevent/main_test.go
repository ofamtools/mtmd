package stopevent

import (
	"os"
	"testing"

	"ofam.dev/mtmd/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunIsolated(m))
}
