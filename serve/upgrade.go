package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"ofam.dev/mtmd/internal/selfcmd"
	"ofam.dev/mtmd/internal/session"
)

// The live upgrade a host asks for: "[--server NAME] daemon upgrade [--binary
// PATH] [--or-restart] [--json]". It is upstream tuios's `tuios daemon
// upgrade` (cmd/tuios/daemon_upgrade.go, commits be0ce78e6 and a362b00d5),
// without cobra, the installers' owner check and the pane dialog: the client
// half of the daemon-upgrade verb (internal/session/handoff_verb.go).
//
// The binary is run once first as "BINARY <SelfCommand words> daemon
// handoff-accepts", the daemon runs it again before it pauses anything, and
// the new image runs as "BINARY <SelfCommand words> [--server NAME] daemon
// --adopt-fd=N": a host's own name, never a tuios.

// upgradeWait bounds how long an upgrade waits for the new image to answer.
// A handoff of a busy daemon takes under a second.
const upgradeWait = 60 * time.Second

// restartBudget bounds each step of an --or-restart restart.
const restartBudget = 60 * time.Second

// What daemon upgrade did, as its JSON names it.
const (
	UpgradeActionUpgraded  = "upgraded"
	UpgradeActionRestarted = "restarted"
	UpgradeActionStarted   = "started"
	UpgradeActionFallback  = "fallback"
	UpgradeActionRefused   = "refused"
	UpgradeActionFailed    = "failed"
)

// UpgradeOutcome is what daemon upgrade reports, one JSON object with --json.
type UpgradeOutcome struct {
	// Action is one of the UpgradeAction names: upgraded live, restarted or
	// started by --or-restart, fallback to the old binary, refused with
	// nothing changed, or failed.
	Action string `json:"action"`
	PID    int    `json:"pid"`
	From   string `json:"from"`
	To     string `json:"to"`
	Binary string `json:"binary"`
	Panes  int    `json:"panes"`
	// PanesKept says every pane's program still runs: true after a live
	// upgrade and after a fallback to the old binary, false after a restart.
	PanesKept bool    `json:"panes_kept"`
	Seconds   float64 `json:"seconds"`
	Fallback  bool    `json:"fallback"`
	Instance  string  `json:"instance"`
	// Handoff is what the handoff measured, from the new image's hello.
	Handoff *session.HandoffStats `json:"handoff,omitempty"`
	// NotLive says the live upgrade never started (nothing was changed by
	// it): the daemon predates live upgrades, refused, or none runs.
	NotLive bool `json:"not_live,omitempty"`
	// Reason is why the daemon was not upgraded live.
	Reason string `json:"reason,omitempty"`
	// Error is the failure, when the command failed.
	Error string `json:"error,omitempty"`
}

// notLiveError is a live upgrade that never started: nothing was changed, and
// a running daemon runs as it did. --or-restart restarts the daemon instead.
type notLiveError struct {
	reason  string
	running bool
	from    string
	pid     int
}

func (e *notLiveError) Error() string { return e.reason }

// upgradeArgs are the words after "daemon upgrade".
type upgradeArgs struct {
	binary    string
	orRestart bool
	asJSON    bool
}

func parseUpgradeArgs(words []string) (upgradeArgs, error) {
	var u upgradeArgs
	for i := 0; i < len(words); i++ {
		a := words[i]
		switch {
		case a == "--or-restart":
			u.orRestart = true
		case a == "--json":
			u.asJSON = true
		case a == "--binary" && i+1 < len(words):
			i++
			u.binary = words[i]
		case strings.HasPrefix(a, "--binary="):
			u.binary = strings.TrimPrefix(a, "--binary=")
		default:
			return u, fmt.Errorf("daemon upgrade: unknown argument %s (usage: daemon upgrade [--binary PATH] [--or-restart] [--json])", a)
		}
	}
	return u, nil
}

// upgrade is "daemon upgrade".
func (r *run) upgrade(words []string, out io.Writer) int {
	u, err := parseUpgradeArgs(words)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	say := func(format string, a ...any) {
		if !u.asJSON {
			fmt.Fprintf(out, format, a...)
		}
	}
	o, err := r.upgradeDaemon(say, u.binary, u.orRestart)
	if u.asJSON {
		if err != nil && o.Error == "" {
			o.Error = err.Error()
		}
		data, _ := json.Marshal(o)
		fmt.Fprintln(out, string(data))
	} else if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	if err != nil {
		return 1
	}
	return 0
}

// upgradeDaemon upgrades the daemon live, and with orRestart restarts it when
// a live upgrade cannot start.
func (r *run) upgradeDaemon(say func(string, ...any), binary string, orRestart bool) (UpgradeOutcome, error) {
	if binary == "" {
		exe, err := selfcmd.Exe()
		if err != nil {
			return UpgradeOutcome{Action: UpgradeActionFailed}, fmt.Errorf("find this program: %w", err)
		}
		binary = exe
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		return UpgradeOutcome{Action: UpgradeActionFailed}, err
	}
	o, err := r.upgradeLive(say, binary)
	var notLive *notLiveError
	if !errors.As(err, &notLive) {
		return o, err
	}
	if !orRestart {
		return UpgradeOutcome{Action: UpgradeActionRefused, Binary: binary, From: notLive.from, PID: notLive.pid,
			PanesKept: notLive.running, NotLive: true, Reason: notLive.reason}, err
	}
	say("Not upgraded live: %s.\n", notLive.reason)
	return r.restartDaemonWith(say, binary, notLive)
}

type helloAnswer struct {
	PID      int                   `json:"pid"`
	Instance string                `json:"instance"`
	Version  string                `json:"daemon_version"`
	Upgrade  *session.HandoffStats `json:"upgrade"`
}

func (r *run) hello(c *session.VerbClient, timeout time.Duration) (helloAnswer, error) {
	var h helloAnswer
	raw, err := c.CallWithTimeout("hello", map[string]any{"client": "mtmd", "version": r.version, "protocol": session.VerbProtocolVersion}, timeout)
	if err != nil {
		return h, err
	}
	return h, json.Unmarshal(raw, &h)
}

// upgradeLive is the live upgrade itself. A *notLiveError means nothing was
// changed.
func (r *run) upgradeLive(say func(string, ...any), binary string) (UpgradeOutcome, error) {
	o := UpgradeOutcome{Action: UpgradeActionFailed, Binary: binary}
	if runtime.GOOS == "windows" {
		return o, &notLiveError{reason: "live upgrades are not supported on Windows", running: session.IsDaemonRunning()}
	}
	client, err := session.DialVerbClientAs(r.version)
	if err != nil {
		var mismatch *session.ProtocolMismatchError
		if errors.As(err, &mismatch) {
			return o, &notLiveError{
				reason:  fmt.Sprintf("the daemon (%s, pid %d) speaks a control protocol this build does not", orUnknown(mismatch.DaemonVersion), mismatch.DaemonPID),
				running: true, from: mismatch.DaemonVersion, pid: mismatch.DaemonPID,
			}
		}
		if !session.IsDaemonRunning() {
			return o, &notLiveError{reason: "no daemon is running"}
		}
		return o, err
	}
	before, err := r.hello(client, 10*time.Second)
	if err != nil {
		_ = client.Close()
		return o, err
	}
	o.From, o.PID = before.Version, before.PID

	// The same run the daemon makes, made here first: it says what the
	// binary is, and it pays a new file's first exec before the daemon
	// pauses anything.
	acc, err := handoffAcceptsOf(binary)
	if err != nil {
		_ = client.Close()
		return o, &notLiveError{reason: fmt.Sprintf("%s cannot take the daemon over: %v", binary, err), running: true, from: before.Version, pid: before.PID}
	}
	say("Checking %s (%s) accepts this daemon's state (handoff v%d)... ", binary, acc.Version, session.HandoffVersion)

	started := time.Now()
	raw, err := client.CallWithTimeout("daemon-upgrade", map[string]any{"binary": binary}, upgradeWait)
	_ = client.Close()
	if err != nil {
		say("refused\n")
		var verr *session.VerbCallError
		if errors.As(err, &verr) && verr.Code == session.ErrVerbUnknownVerb {
			return o, &notLiveError{reason: fmt.Sprintf("the daemon (%s, pid %d) predates live upgrades", before.Version, before.PID), running: true, from: before.Version, pid: before.PID}
		}
		if errors.As(err, &verr) {
			return o, &notLiveError{reason: "the daemon refused: " + strings.TrimSuffix(verr.Message, "."), running: true, from: before.Version, pid: before.PID}
		}
		return o, err
	}
	var accepted struct {
		Panes         int    `json:"panes"`
		ManifestBytes int64  `json:"manifest_bytes"`
		To            string `json:"to"`
		From          string `json:"from"`
	}
	_ = json.Unmarshal(raw, &accepted)
	say("ok\n")
	say("Paused %d %s, saved their screens (%d bytes).\n", accepted.Panes, plural(accepted.Panes, "pane", "panes"), accepted.ManifestBytes)
	say("Starting %s in place (pid %d kept)...\n", orUnknown(accepted.To), before.PID)

	after, err := r.awaitUpgradedDaemon(before.Instance, upgradeWait)
	if err != nil {
		// The exec failed and the old image carried on with every pane, or
		// nothing answers.
		o.PanesKept = after.Instance == before.Instance
		return o, err
	}
	o = UpgradeOutcome{
		Action: UpgradeActionUpgraded,
		PID:    after.PID, From: cmpOr(accepted.From, before.Version), To: after.Version, Binary: binary,
		Panes: accepted.Panes, PanesKept: true, Seconds: time.Since(started).Seconds(),
		Instance: after.Instance, Handoff: after.Upgrade,
	}
	if after.Upgrade != nil && after.Upgrade.Fallback {
		o.Action, o.Fallback = UpgradeActionFallback, true
		return o, fmt.Errorf("%s started and could not take the panes over, so the daemon went back to %s. Every program is still running; the daemon log says what failed", binary, o.To)
	}
	if after.PID != before.PID {
		o.Action, o.PanesKept = UpgradeActionFailed, false
		return o, fmt.Errorf("the daemon's pid changed from %d to %d: it was restarted, not upgraded", before.PID, after.PID)
	}
	say("Upgraded in %.2f s. %d %s and their programs kept.\n", o.Seconds, o.Panes, plural(o.Panes, "pane", "panes"))
	return o, nil
}

// awaitUpgradedDaemon waits for a hello from an image other than old. Between
// the old image dropping its clients and the new one serving, a call waits in
// the socket's backlog. An answer from the old image means the exec failed and
// the daemon carried on.
func (r *run) awaitUpgradedDaemon(old string, wait time.Duration) (helloAnswer, error) {
	deadline := time.Now().Add(wait)
	var last error
	for time.Now().Before(deadline) {
		client, err := session.DialVerbClientAs(r.version)
		if err == nil {
			var h helloAnswer
			h, err = r.hello(client, time.Until(deadline))
			_ = client.Close()
			if err == nil {
				if h.Instance != old {
					return h, nil
				}
				// The old image answers only before it drops its clients or
				// after an exec that failed; give the exec its moment.
				last = errors.New("the daemon could not exec the new binary and carried on as it was; every program is still running, and the daemon log says why")
				time.Sleep(200 * time.Millisecond)
				continue
			}
		}
		last = err
		time.Sleep(50 * time.Millisecond)
	}
	return helloAnswer{Instance: old}, fmt.Errorf("the daemon did not answer as a new image within %s of the upgrade: %v", wait, last)
}

// handoffAcceptsOf runs binary's handoff-accepts through the host's prefix.
func handoffAcceptsOf(binary string) (session.HandoffAccepts, error) {
	var acc session.HandoffAccepts
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	argv := selfcmd.Argv(binary, "daemon", "handoff-accepts")
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.Output()
	if err != nil {
		return acc, fmt.Errorf("it did not answer handoff-accepts (%w)", err)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &acc); err != nil {
		return acc, errors.New("it answered handoff-accepts with something that is not its answer")
	}
	if session.HandoffVersion < acc.Handoff[0] || session.HandoffVersion > acc.Handoff[1] {
		return acc, fmt.Errorf("it reads handoff v%d to v%d, and this build writes v%d", acc.Handoff[0], acc.Handoff[1], session.HandoffVersion)
	}
	return acc, nil
}

// restartDaemonWith puts the daemon on binary the way a restart does: it stops
// the daemon (SIGTERM, which saves every session), and starts binary's
// daemon, which restores them with their layout and new shells. The programs
// in the panes end. The start runs as binary through the host's prefix, with
// this process's server, so the daemon that comes back serves the same
// socket.
func (r *run) restartDaemonWith(say func(string, ...any), binary string, why *notLiveError) (UpgradeOutcome, error) {
	o := UpgradeOutcome{Action: UpgradeActionFailed, Binary: binary, From: why.from, Reason: why.reason, NotLive: true}
	if why.running {
		pid := why.pid
		if pid <= 0 {
			pid = session.GetDaemonPID()
		}
		if pid <= 0 {
			return o, errors.New("the daemon's pid is not known, so it was not restarted")
		}
		say("Restarting the daemon with %s, which ends the programs in its panes...\n", binary)
		if err := stopPID(pid); err != nil {
			return o, fmt.Errorf("stop the daemon (pid %d): %w", pid, err)
		}
		deadline := time.Now().Add(restartBudget)
		for session.IsDaemonRunning() && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if session.IsDaemonRunning() {
			return o, errors.New("the daemon did not stop, so it still runs the old build")
		}
	} else {
		say("Starting the daemon with %s...\n", binary)
	}
	var words []string
	if r.p.serverSet {
		words = append(words, "--server", r.p.server)
	}
	argv := selfcmd.Argv(binary, append(words, "start-server")...)
	ctx, cancel := context.WithTimeout(context.Background(), restartBudget)
	defer cancel()
	if out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput(); err != nil {
		return o, fmt.Errorf("the daemon did not start: %v %s", err, strings.TrimSpace(string(out)))
	}
	o.Action = UpgradeActionRestarted
	if !why.running {
		o.Action = UpgradeActionStarted
	}
	if client, err := session.DialVerbClientAs(r.version); err == nil {
		if h, err := r.hello(client, 10*time.Second); err == nil {
			o.PID, o.Instance, o.To = h.PID, h.Instance, h.Version
		}
		_ = client.Close()
	}
	say("%s. The daemon runs %s (pid %d).\n", strings.ToUpper(o.Action[:1])+o.Action[1:], orUnknown(o.To), o.PID)
	return o, nil
}

func orUnknown(s string) string { return cmpOr(s, "unknown version") }

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
