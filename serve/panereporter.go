package serve

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"ofam.dev/mtmd/internal/agentproto"
	"ofam.dev/mtmd/internal/session"
)

// agentProtoHoldMax bounds the wait on one Inbox hold. The daemon ends every
// hold within 300 seconds; this only guards against one that stops answering.
const agentProtoHoldMax = 305 * time.Second

// paneReporter reports the session's state for the pane it runs in, and its
// metadata and activity.
//
// State reports are made on the pane's loop and wait for the daemon, in
// order. Metadata and activity come from the session's feed goroutine and go
// on a connection of their own, so a slow call of theirs never holds a state
// report, and through it the pane's input and render loop.
type paneReporter struct {
	session, window, harness string
	dial                     func() (*session.VerbClient, error)

	mu     sync.Mutex
	client *session.VerbClient

	// feedMu guards feedClient and activity, used only by the feed.
	feedMu     sync.Mutex
	feedClient *session.VerbClient
	// activity is whether the daemon's set-agent-state takes activity:
	// unknown until it has been asked, once.
	activity activitySupport
}

// activitySupport is what the daemon was found to take.
type activitySupport int

const (
	activityUnknown activitySupport = iota
	activityYes
	activityNo
)

// protoMetaSource is the set-agent-meta source a protocol pane writes under.
const protoMetaSource = "protocol"

// activityIfState is the if_state an activity report carries: its state
// part applies only to a pane whose state is none.
const activityIfState = "none"

func (r *paneReporter) close() {
	r.mu.Lock()
	if r.client != nil {
		_ = r.client.Close()
		r.client = nil
	}
	r.mu.Unlock()
	r.feedMu.Lock()
	if r.feedClient != nil {
		_ = r.feedClient.Close()
		r.feedClient = nil
	}
	r.feedMu.Unlock()
}

// Report sends set-agent-state on a connection kept for reports, dialled
// again after a failure.
func (r *paneReporter) Report(ctx context.Context, state, kind, message, ifState string) error {
	params := map[string]any{"session": r.session, "window": r.window, "state": state, "harness": r.harness}
	for k, v := range map[string]string{"kind": kind, "message": message, "if_state": ifState} {
		if v != "" {
			params[k] = v
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client == nil {
		c, err := r.dial()
		if err != nil {
			return err
		}
		r.client = c
	}
	timeout := time.Until(deadlineOr(ctx, 2*time.Second))
	if _, err := r.client.CallWithTimeout("set-agent-state", params, timeout); err != nil {
		_ = r.client.Close()
		r.client = nil
		return err
	}
	return nil
}

// feedCall runs one verb on the feed's connection, dialled again after a
// failure. Callers hold r.feedMu.
func (r *paneReporter) feedCall(ctx context.Context, verb string, params map[string]any) (json.RawMessage, error) {
	if r.feedClient == nil {
		c, err := r.dial()
		if err != nil {
			return nil, err
		}
		r.feedClient = c
	}
	timeout := time.Until(deadlineOr(ctx, 2*time.Second))
	raw, err := r.feedClient.CallWithTimeout(verb, params, timeout)
	if err != nil {
		if _, ok := errors.AsType[*session.VerbCallError](err); !ok {
			// The connection itself failed: dial again next time.
			_ = r.feedClient.Close()
			r.feedClient = nil
		}
		return nil, err
	}
	return raw, nil
}

// SetMeta writes the agent's model, context use, cost and plan progress to
// the pane's metadata, under the protocol source. set-agent-meta is scoped to
// the caller's own pane.
func (r *paneReporter) SetMeta(ctx context.Context, tokens map[string]string) error {
	r.feedMu.Lock()
	defer r.feedMu.Unlock()
	_, err := r.feedCall(ctx, "set-agent-meta", map[string]any{
		"session": r.session,
		"window":  r.window,
		"tokens":  tokens,
		"source":  protoMetaSource,
	})
	return err
}

// ReportActivity sends one activity with set-agent-state. set-agent-state
// needs a state, and this one's is the state the pane last reported, with
// if_state none: the daemon records the activity and refuses the state part
// on every pane that has a state, so the call restamps nothing and pushes no
// state. Only a pane whose state was lost mid-turn (none) gets it back. A
// daemon whose set-agent-state does not list activity is sent nothing: an
// older daemon would ignore the field and apply the report.
func (r *paneReporter) ReportActivity(ctx context.Context, a agentproto.Activity) error {
	r.feedMu.Lock()
	defer r.feedMu.Unlock()
	if r.activity == activityUnknown {
		raw, err := r.feedCall(ctx, "list-verbs", map[string]any{"verb": "set-agent-state"})
		if _, ok := errors.AsType[*session.VerbCallError](err); ok {
			// A daemon that answers but cannot say is taken not to.
			r.activity = activityNo
			return nil
		}
		if err != nil {
			return err
		}
		r.activity = activityNo
		if listsParam(raw, "set-agent-state", "activity") {
			r.activity = activityYes
		}
	}
	if r.activity != activityYes {
		return nil
	}
	act := map[string]any{"event": a.Event}
	for k, v := range map[string]string{"tool": a.Tool, "target": a.Target, "text": a.Text} {
		if v != "" {
			act[k] = v
		}
	}
	if a.OK != nil {
		act["ok"] = *a.OK
	}
	state := a.State
	if state == "" {
		state = "working"
	}
	_, err := r.feedCall(ctx, "set-agent-state", map[string]any{
		"session":  r.session,
		"window":   r.window,
		"state":    state,
		"if_state": activityIfState,
		"harness":  r.harness,
		"activity": act,
	})
	return err
}

// listsParam reports whether a list-verbs answer lists param for verb.
func listsParam(raw json.RawMessage, verb, param string) bool {
	var res struct {
		Verbs []struct {
			Verb   string `json:"verb"`
			Params []struct {
				Name string `json:"name"`
			} `json:"params"`
		} `json:"verbs"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return false
	}
	for _, v := range res.Verbs {
		if v.Verb != verb {
			continue
		}
		for _, p := range v.Params {
			if p.Name == param {
				return true
			}
		}
	}
	return false
}

// Hold calls request-approval on a connection of its own, which is closed
// when ctx ends: the daemon reads that as the caller gone and ends the hold
// with no decision.
func (r *paneReporter) Hold(ctx context.Context, line string, options []string) (string, string, error) {
	client, err := r.dial()
	if err != nil {
		return "", "", err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		_ = client.Close()
	}()
	raw, err := client.CallWithTimeout("request-approval", map[string]any{
		"session": r.session,
		"window":  r.window,
		"harness": r.harness,
		"options": options,
		"summary": line,
	}, agentProtoHoldMax)
	if err != nil {
		return "", "", err
	}
	if ctx.Err() != nil {
		return "", "", ctx.Err()
	}
	var res struct {
		Decision   string `json:"decision"`
		AnsweredBy string `json:"answered_by"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", "", err
	}
	if res.Decision == "" {
		return "", "", errors.New("no decision")
	}
	return res.Decision, res.AnsweredBy, nil
}

// deadlineOr is ctx's deadline, or now plus d when it has none.
func deadlineOr(ctx context.Context, d time.Duration) time.Time {
	if dl, ok := ctx.Deadline(); ok {
		return dl
	}
	return time.Now().Add(d)
}
