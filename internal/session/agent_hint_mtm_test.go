package session

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// MTM_AGENT names the harness as TUIOS_AGENT does, and is read first.
func TestReadAgentHintEnvReadsMTMName(t *testing.T) {
	if os.Getenv("TUIOS_HINT_TEST_CHILD") == "1" {
		time.Sleep(5 * time.Second)
		return
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no process environment reader on this platform")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestReadAgentHintEnvReadsMTMName$")
	cmd.Env = append(os.Environ(), "TUIOS_HINT_TEST_CHILD=1", "MTM_AGENT=Claude-Code ", "TUIOS_AGENT=codex")
	if err := cmd.Start(); err != nil {
		t.Skipf("start child: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := agentHint(readAgentHintEnv, cmd.Process.Pid); got == "claude-code" {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("agentHint = %q, want claude-code (MTM_AGENT first)", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
