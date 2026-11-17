package session

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"ofam.dev/mtmd/internal/config"
)

// personVerbPaneCaller is a connection from a process in window of a pane:
// connFromPane says so, and the grant table places it in window.
func personVerbPaneCaller(t *testing.T, d *Daemon, window string) *connState {
	t.Helper()
	cs := &connState{clientID: "pane-caller", peerPID: os.Getpid(), done: make(chan struct{})}
	cs.fromPaneOnce.Do(func() {})
	cs.fromPane = true
	d.setApprovalPeer(func(*connState) (bool, string) { return true, window })
	t.Cleanup(func() { d.setApprovalPeer(nil) })
	return cs
}

// TestPersonVerbsFollowTheSetting: every act of config.PersonVerbTable runs
// for a pane's process when person_verbs names it, and is refused as the
// person's when it does not. A row per act calls the verb the way a pane
// would, without a nonce, and says whether the person check refused it.
func TestPersonVerbsFollowTheSetting(t *testing.T) {
	d, _ := startTestDaemon(t)
	sess, a, b := twoWindowSession(t, d, "pv")
	cs := personVerbPaneCaller(t, d, b)

	d.tml.mu.Lock()
	if d.tml.surfaces == nil {
		d.tml.surfaces = map[string]*tmlSurface{}
	}
	d.tml.surfaces["pv-surface"] = &tmlSurface{id: "pv-surface", sessionID: sess.ID}
	d.tml.mu.Unlock()

	notHuman := func(_ any, err *verbError) bool { return err != nil && err.Code == ErrVerbNotHuman }
	forbidden := func(_ any, err *verbError) bool { return err != nil && err.Code == ErrVerbForbidden }
	call := func(h func(*Daemon, *connState, json.RawMessage) (any, *verbError), params string, refused func(any, *verbError) bool) func() bool {
		return func() bool { return refused(h(d, cs, json.RawMessage(params))) }
	}
	plugin := func(method string) func() bool {
		return func() bool { return d.herdrPluginTrust(cs, method) != nil }
	}
	ship := func(verb string) func() bool {
		return func() bool {
			o := shipOutbound{sess: sess, target: sess.GetState().Windows[0]}
			p := shipPushParams{Wait: 1}
			p.Confirm = o.token(verb, p)
			by, _ := d.shipOutboundGate(cs, verb, "tuios "+verb, o, p)
			return by != "person_verbs"
		}
	}
	rows := map[string]func() bool{
		"agent-transcript":          call((*Daemon).verbAgentTranscript, `{"session":"pv","window":"`+a+`"}`, notHuman),
		"dismiss-attention":         call((*Daemon).verbDismissAttention, `{"id":"none"}`, notHuman),
		"mark-attention":            call((*Daemon).verbMarkAttention, `{"action":"wake","id":"none"}`, notHuman),
		"reply-approval":            call((*Daemon).verbReplyApproval, `{"decision":"`+ApprovalDeny+`","request_id":"none"}`, notHuman),
		"answer-ask":                call((*Daemon).verbAnswerAsk, `{"request_id":"none","answer":"yes"}`, notHuman),
		"release-agent-message":     call((*Daemon).verbReleaseAgentMessage, `{"session":"pv","id":1}`, notHuman),
		"register-push":             call((*Daemon).verbRegisterPush, `{}`, notHuman),
		"list-push":                 call((*Daemon).verbListPush, `{}`, notHuman),
		"remove-push":               call((*Daemon).verbRemovePush, `{}`, notHuman),
		"attach-presence":           call((*Daemon).verbAttachPresence, `{}`, forbidden),
		"paste-image":               call((*Daemon).verbPasteImage, `{"session":"pv","window":"`+a+`"}`, notHuman),
		"reply-history":             call((*Daemon).verbReplyHistory, `{"target":"t"}`, notHuman),
		"add-reply-history":         call((*Daemon).verbAddReplyHistory, `{"target":"t"}`, notHuman),
		"set-option agents.enabled": call((*Daemon).verbSetOption, `{"session":"pv","key":"agents.enabled","value":"true"}`, forbidden),
		"plugin.approve":            plugin("plugin.approve"),
		"plugin.enable":             plugin("plugin.enable"),
		"plugin.disable":            plugin("plugin.disable"),
		"plugin.restart":            plugin("plugin.restart"),
		"plugin.link":               plugin("plugin.link"),
		"plugin.unlink":             plugin("plugin.unlink"),
		"adopt-history-log":         call((*Daemon).verbAdoptHistoryLog, `{}`, forbidden),
		"purge-history-logs":        call((*Daemon).verbPurgeHistoryLogs, `{"log":"none"}`, forbidden),
		"apply-config":              call((*Daemon).verbApplyConfig, `{}`, forbidden),
		"ship-push":                 ship("ship-push"),
		"ship-pr":                   ship("ship-pr"),
		"tml-input":                 call((*Daemon).verbTMLInput, `{"surface":"pv-surface"}`, notHuman),
		"tml-place":                 call((*Daemon).verbTMLPlace, `{"surface":"pv-surface","target":"`+TMLTargetPane+`"}`, notHuman),
	}
	for _, v := range config.PersonVerbTable {
		refused, ok := rows[v.Name]
		if !ok {
			t.Errorf("%s is in PersonVerbTable and has no row here", v.Name)
			continue
		}
		t.Run(v.Name, func(t *testing.T) {
			d.SetPersonVerbs([]string{v.Name})
			if refused() {
				t.Errorf("named in person_verbs, %s was refused to a pane", v.Name)
			}
			d.SetPersonVerbs([]string{})
			if !refused() {
				t.Errorf("left out of person_verbs, %s ran for a pane", v.Name)
			}
		})
	}
}

// TestPersonVerbsDefault: unset, person_verbs lets a pane do every act but
// the consent steps, and a file change narrows it at once but widens it only
// for the person (tuios config apply) or a restart.
func TestPersonVerbsDefault(t *testing.T) {
	personOnly := []string{"reply-approval", "answer-ask", "plugin.approve", "plugin.enable", "plugin.disable", "plugin.restart", "plugin.link", "plugin.unlink", "apply-config", "ship-push", "ship-pr"}
	got := config.PermissionsConfig{}.ResolvedPersonVerbs()
	for _, name := range config.PersonVerbNames() {
		if want := !slices.Contains(personOnly, name); slices.Contains(got, name) != want {
			t.Errorf("default: pane may %s = %v, want %v", name, !want, want)
		}
	}
	if none := (config.PermissionsConfig{PersonVerbs: []string{}}).ResolvedPersonVerbs(); len(none) != 0 {
		t.Errorf("person_verbs = [] gives %v, want none", none)
	}

	d := NewDaemon(&DaemonConfig{Version: "test", DisableAutoRestore: true})
	if in := d.personVerbsInForce(); strings.Join(in, ",") != strings.Join(got, ",") {
		t.Errorf("a daemon with no config holds %v, want the default %v", in, got)
	}
	d.SetPersonVerbs([]string{"agent-transcript"})
	if !d.applyPersonVerbs([]string{"agent-transcript", "reply-approval"}, false) {
		t.Error("a file change that widens person_verbs did not wait")
	}
	if in := d.personVerbsInForce(); slices.Contains(in, "reply-approval") {
		t.Errorf("a file change widened person_verbs: %v", in)
	}
	if d.applyPersonVerbs([]string{"agent-transcript", "reply-approval"}, true) || !slices.Contains(d.personVerbsInForce(), "reply-approval") {
		t.Errorf("the person's apply did not widen person_verbs: %v", d.personVerbsInForce())
	}
}
