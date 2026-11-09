package session

import (
	"strings"

	"ofam.dev/mtmd/internal/integration"
	"ofam.dev/mtmd/internal/procinfo"
)

// AgentHintEnv is the environment variable a wrapper sets to name the harness
// it runs. It is for the agents process detection cannot see: one in a
// container, in a VM, over ssh, or under a build tool such as go run. Nothing
// in the pane's process tree is the agent there, so the one who started the
// wrapper says what it is:
//
//	TUIOS_AGENT=claude-code docker run -it sandbox claude
//
// Only the foreground process group leader's own environment is read, and only
// this one variable of it. A value that names no manifest is ignored.
const AgentHintEnv = "TUIOS_AGENT"

// readAgentHintEnv reads TUIOS_AGENT (or MTM_AGENT, read first) from a process's environment. The kernel
// gives it only for a process of the same user, and a refusal is an absent
// hint, never an error.
func readAgentHintEnv(pid int) (string, bool) {
	for _, n := range integration.AgentHintEnvs {
		if v, ok := procinfo.EnvVar(pid, n); ok && strings.TrimSpace(v) != "" {
			return v, true
		}
	}
	return "", false
}

// agentHint returns the harness a process's TUIOS_AGENT names, trimmed and
// lowercased, or "" when it names none.
func agentHint(read func(pid int) (string, bool), pid int) string {
	if read == nil || pid <= 1 {
		return ""
	}
	v, ok := read(pid)
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(v))
}
