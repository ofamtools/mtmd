package session

import (
	"encoding/json"
	"errors"
	"os"
	"time"

	"ofam.dev/mtmd/internal/coldlog"
)

// The history-log verbs: the cold logs of panes (scrollback_cold.go), live or
// orphaned, as a noun with its own verbs. A log is named by an id,
// session/window, which is also where it lives on disk.
//
//	list-history-logs     what logs exist, with size, state and days left
//	show-history-log      one log
//	keep-history-log      exempt an orphan from the grace period
//	unkeep-history-log    put it back under the grace period
//	adopt-history-log     give an orphan to a window (also adopt-history,
//	                      the same call from the window's side)
//	purge-history-logs    delete orphans now, one or many
//
// The names read verb-noun, like the other verbs of the protocol; the plan's
// history-log.list and pane.adopt-history are these.
//
// Who may call: reading, keep and unkeep are open to a restricted
// connection (an agent's), since nothing is lost by them. adopt-history-log
// moves a log onto a pane, and purge-history-logs deletes: both are for the
// person only (a local process outside every pane, on a connection it did not
// restrict), and an agent is refused. purge-history-logs with dry_run lists
// what it would delete and is open to everyone; the person deletes by passing
// the confirm token it printed.

func (d *Daemon) historyLogParams(params json.RawMessage, v any) *verbError {
	return decodeParams(params, v)
}

func historyLogError(err error) *verbError {
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, errHistoryLogNotFound):
		return newVerbError(ErrVerbInvalidParams, "no such history log")
	case errors.Is(err, errHistoryLogLive):
		return newVerbError(ErrVerbInvalidParams, err.Error()+": only an orphaned log can be changed this way")
	}
	return newVerbError(ErrVerbInvalidParams, err.Error())
}

func (d *Daemon) requirePerson(cs *connState, verb string) *verbError {
	if d.shipCallerIsPerson(cs) {
		return nil
	}
	return hintedVerbError(ErrVerbForbidden, verb+" is for the person: an agent, a restricted connection or a call over a link is refused", &VerbHint{
		Detail: "Nothing was changed. Ask the person to run it, or use purge-history-logs with dry_run to show what it would delete.",
	})
}

func (d *Daemon) verbListHistoryLogs(cs *connState, params json.RawMessage) (any, *verbError) {
	var p struct {
		State   string `json:"state"`
		Session string `json:"session"`
	}
	if verr := decodeParams(params, &p); verr != nil {
		return nil, verr
	}
	switch p.State {
	case "", "all", "live", "orphaned":
	default:
		return nil, invalidParam("state", "state must be live, orphaned or all", "live", "orphaned", "all")
	}
	docs, err := d.historyLogDocs(time.Now())
	if err != nil {
		return nil, newVerbError(ErrVerbInternal, "list-history-logs: "+err.Error())
	}
	out := make([]historyLogDoc, 0, len(docs))
	var bytes int64
	for _, doc := range docs {
		if p.State != "" && p.State != "all" && doc.State != p.State {
			continue
		}
		if p.Session != "" && doc.Session != p.Session {
			continue
		}
		bytes += doc.Bytes
		out = append(out, doc)
	}
	return map[string]any{"logs": out, "grace_days": d.manager.ColdPolicy().GraceDays, "total_bytes": bytes}, nil
}

func (d *Daemon) verbShowHistoryLog(cs *connState, params json.RawMessage) (any, *verbError) {
	var p struct {
		Log string `json:"log"`
	}
	if verr := decodeParams(params, &p); verr != nil {
		return nil, verr
	}
	doc, _, err := d.historyLogDoc(p.Log, time.Now())
	if err != nil {
		return nil, historyLogError(err)
	}
	return doc, nil
}

func (d *Daemon) verbKeepHistoryLog(cs *connState, params json.RawMessage) (any, *verbError) {
	return d.setHistoryLogKeep(params, true)
}

func (d *Daemon) verbUnkeepHistoryLog(cs *connState, params json.RawMessage) (any, *verbError) {
	return d.setHistoryLogKeep(params, false)
}

func (d *Daemon) setHistoryLogKeep(params json.RawMessage, keep bool) (any, *verbError) {
	var p struct {
		Log string `json:"log"`
	}
	if verr := decodeParams(params, &p); verr != nil {
		return nil, verr
	}
	now := time.Now()
	doc, dir, err := d.historyLogDoc(p.Log, now)
	if err != nil {
		return nil, historyLogError(err)
	}
	if doc.State == "live" {
		return nil, historyLogError(errHistoryLogLive)
	}
	if _, merr := coldlog.ReadMarker(dir); merr != nil {
		// A crash orphan nobody has stamped yet: stamping it is what keeps it.
		if !keep {
			return doc, nil
		}
		if err := coldlog.WriteMarker(dir, coldlog.Marker{ClosedAt: now, Reason: "unmarked", Keep: true}); err != nil {
			return nil, newVerbError(ErrVerbInternal, err.Error())
		}
	} else if err := coldlog.SetKeep(dir, keep); err != nil {
		return nil, newVerbError(ErrVerbInternal, err.Error())
	}
	doc, _, err = d.historyLogDoc(p.Log, now)
	if err != nil {
		return nil, historyLogError(err)
	}
	return doc, nil
}

// verbAdoptHistoryLog is adopt-history-log and adopt-history: one handler,
// the other noun as a parameter.
func (d *Daemon) verbAdoptHistoryLog(cs *connState, params json.RawMessage) (any, *verbError) {
	var p struct {
		Log     string `json:"log"`
		Session string `json:"session"`
		Window  string `json:"window"`
	}
	if verr := decodeParams(params, &p); verr != nil {
		return nil, verr
	}
	if verr := d.requirePerson(cs, "adopt-history-log"); verr != nil {
		return nil, verr
	}
	if p.Log == "" {
		return nil, invalidParam("log", "log is required: the id of an orphaned log, from list-history-logs")
	}
	sess, verr := d.resolveVerbSession(cs, p.Session)
	if verr != nil {
		return nil, verr
	}
	pty, err := d.resolvePTYForTarget(sess, p.Window)
	if err != nil {
		return nil, mapResolveErr(err, sess)
	}
	if pty.cold == nil {
		return nil, newVerbError(ErrVerbInvalidParams, "the pane keeps no cold log: daemon.scrollback_disk is off, or its emulator cannot report evicted rows")
	}
	_, _, src, err := historyLogDir(p.Log)
	if err != nil {
		return nil, historyLogError(err)
	}
	pty.terminalMu.Lock()
	rows, err := pty.cold.adoptHistoryLog(src)
	pty.terminalMu.Unlock()
	if err != nil {
		return nil, historyLogError(err)
	}
	return map[string]any{
		"log":    historyLogID(sess.Name(), pty.cold.windowID),
		"from":   p.Log,
		"window": pty.cold.windowID,
		"rows":   rows,
	}, nil
}

func (d *Daemon) verbPurgeHistoryLogs(cs *connState, params json.RawMessage) (any, *verbError) {
	var p struct {
		Log           string `json:"log"`
		OlderThanDays *int   `json:"older_than_days"`
		AllOrphaned   bool   `json:"all_orphaned"`
		DryRun        bool   `json:"dry_run"`
		Confirm       string `json:"confirm"`
	}
	if verr := decodeParams(params, &p); verr != nil {
		return nil, verr
	}
	chosen := 0
	if p.Log != "" {
		chosen++
	}
	if p.OlderThanDays != nil {
		chosen++
	}
	if p.AllOrphaned {
		chosen++
	}
	if chosen != 1 {
		return nil, invalidParam("log", "name what to purge with exactly one of log, older_than_days or all_orphaned")
	}
	if p.OlderThanDays != nil && *p.OlderThanDays < 0 {
		return nil, invalidParam("older_than_days", "older_than_days cannot be negative")
	}
	if !p.DryRun {
		if verr := d.requirePerson(cs, "purge-history-logs"); verr != nil {
			return nil, verr
		}
	}
	now := time.Now()
	docs, err := d.historyLogDocs(now)
	if err != nil {
		return nil, newVerbError(ErrVerbInternal, "purge-history-logs: "+err.Error())
	}
	targets := []historyLogDoc{}
	for _, doc := range docs {
		if doc.State != "orphaned" {
			continue
		}
		switch {
		case p.Log != "":
			if doc.Log == p.Log {
				if doc.Keep {
					return nil, hintedVerbError(ErrVerbInvalidParams, "the log is kept; unkeep it first", &VerbHint{Verb: "unkeep-history-log", Param: "log"})
				}
				targets = append(targets, doc)
			}
		case p.OlderThanDays != nil:
			if !doc.Keep && doc.ClosedAt > 0 && now.Sub(time.UnixMilli(doc.ClosedAt)) > time.Duration(*p.OlderThanDays)*24*time.Hour {
				targets = append(targets, doc)
			}
		default:
			if !doc.Keep {
				targets = append(targets, doc)
			}
		}
	}
	if p.Log != "" && len(targets) == 0 {
		if _, _, err := d.historyLogDoc(p.Log, now); err != nil {
			return nil, historyLogError(err)
		}
		return nil, historyLogError(errHistoryLogLive)
	}
	keys := make([]string, 0, len(targets))
	var bytes int64
	for _, t := range targets {
		keys = append(keys, t.Log)
		bytes += t.Bytes
	}
	token := SelectionToken(keys)
	if p.DryRun {
		return map[string]any{"dry_run": true, "purged": targets, "bytes": bytes, "confirm": token}, nil
	}
	if len(targets) > 0 && p.Confirm != token {
		msg := "purge-history-logs deletes " + itoa(len(targets)) + " log" + map[bool]string{true: "", false: "s"}[len(targets) == 1] + " and was called without a confirm token; nothing was deleted"
		if p.Confirm != "" {
			msg = "the logs that would be deleted changed since the token was issued; nothing was deleted"
		}
		return nil, hintedVerbError(ErrVerbConfirmRequired, msg, &VerbHint{
			Param:     "confirm",
			Available: keys,
			Confirm:   token,
			Detail:    "Check the logs in available (dry_run shows their size and age), then call again with confirm set to the token in confirm.",
		})
	}
	purged := make([]historyLogDoc, 0, len(targets))
	for _, t := range targets {
		_, _, dir, err := historyLogDir(t.Log)
		if err != nil {
			continue
		}
		// Still an orphan nobody writes, since the look.
		if in, ok := coldlog.Stat(dir); !ok || !in.Orphan || in.Locked || in.Marker.Keep {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return nil, newVerbError(ErrVerbInternal, "purge-history-logs: "+err.Error())
		}
		purged = append(purged, t)
	}
	return map[string]any{"dry_run": false, "purged": purged, "bytes": bytes}, nil
}

// historyLogVerbs are the verbs of this file.
func historyLogVerbs() map[string]verbEntry {
	logParam := verbParam{Name: "log", Type: "string", Required: true, Description: "The id of the log: the session name and the window id, joined by a slash, as list-history-logs gives it."}
	docReturns := []verbParam{
		{Name: "log", Type: "string", Description: "The id of the log, session/window."},
		{Name: "session", Type: "string", Description: "The session the log lives under."},
		{Name: "window", Type: "string", Description: "The window id the log belongs to."},
		{Name: "state", Type: "string", Description: "live while a pane owns the log, orphaned once its pane is gone.", Accepted: []string{"live", "orphaned"}},
		{Name: "rows", Type: "int", Description: "How many rows the log holds."},
		{Name: "bytes", Type: "int", Description: "What the log takes on disk."},
		{Name: "first_row", Type: "int", Description: "The id of its oldest row. The pane's rows count from 0; a log that dropped its oldest segments starts later."},
		{Name: "next_row", Type: "int", Description: "One past its newest row: the id the next row evicted from the pane's memory gets."},
		{Name: "segments", Type: "int", Description: "How many files the log is in."},
		{Name: "closed_at", Type: "int", Description: "When the pane went, Unix ms. Absent for a live log, and for a log a crash left that the sweep has not stamped."},
		{Name: "days_left", Type: "float", Description: "Days until the sweep deletes an orphan, 0 once it is due. Absent for a live or kept log."},
		{Name: "keep", Type: "bool", Description: "True when the orphan is exempt from the grace period."},
		{Name: "reason", Type: "string", Description: "Why the pane went: window-closed, session-killed or unmarked (a crash)."},
		{Name: "title", Type: "string", Description: "The pane's title when it went."},
		{Name: "cwd", Type: "string", Description: "The pane's folder when it went."},
		{Name: "gaps", Type: "int", Description: "How many runs of lost rows the log has."},
	}
	return map[string]verbEntry{
		"list-history-logs": {
			description: "List the cold scrollback logs on this machine: the rows panes let go of from memory, kept on disk. A log is live while its pane exists and orphaned after its window closed or its session was killed; an orphan is deleted daemon.scrollback_grace_days after it closed unless it is kept.",
			params: []verbParam{
				{Name: "state", Type: "string", Description: "Which logs to list.", Accepted: []string{"live", "orphaned", "all"}, Default: "all"},
				{Name: "session", Type: "string", Description: "List only the logs under this session name."},
			},
			returns: []verbParam{
				{Name: "logs", Type: "[]object", Description: "One entry per log, by id: log, session, window, state, rows, bytes, first_row, next_row, segments, closed_at, days_left, keep, reason, title, cwd, gaps."},
				{Name: "grace_days", Type: "int", Description: "The grace period an orphan is kept for, daemon.scrollback_grace_days."},
				{Name: "total_bytes", Type: "int", Description: "What the listed logs take on disk."},
			},
			examples: []string{
				`{"id":1,"verb":"list-history-logs"}`,
				`{"id":1,"verb":"list-history-logs","params":{"state":"orphaned"}}`,
			},
			handler: (*Daemon).verbListHistoryLogs,
		},
		"show-history-log": {
			description: "Describe one cold scrollback log.",
			params:      []verbParam{logParam},
			returns:     docReturns,
			examples:    []string{`{"id":1,"verb":"show-history-log","params":{"log":"work/3f2a"}}`},
			handler:     (*Daemon).verbShowHistoryLog,
		},
		"keep-history-log": {
			description: "Exempt an orphaned log from the grace period, so the sweep never deletes it. It still counts against the total size cap's accounting but is the last thing the cap removes. A live log is kept while its pane lives and is refused.",
			params:      []verbParam{logParam},
			returns:     docReturns,
			examples:    []string{`{"id":1,"verb":"keep-history-log","params":{"log":"work/3f2a"}}`},
			handler:     (*Daemon).verbKeepHistoryLog,
		},
		"unkeep-history-log": {
			description: "Put a kept orphan back under the grace period: it is deleted when closed_at plus the grace period has passed, which may be at the next sweep.",
			params:      []verbParam{logParam},
			returns:     docReturns,
			examples:    []string{`{"id":1,"verb":"unkeep-history-log","params":{"log":"work/3f2a"}}`},
			handler:     (*Daemon).verbUnkeepHistoryLog,
		},
		"adopt-history-log": {
			description: "Give an orphaned log to a window: the log moves to the window's own place and the window's later rows continue it. The window must have no log of its own, which a pane that has not yet let rows go from memory does not. For the person only.",
			params: []verbParam{
				logParam,
				sessionParam,
				windowParam,
			},
			returns: []verbParam{
				{Name: "log", Type: "string", Description: "The id the log has now."},
				{Name: "from", Type: "string", Description: "The id it had."},
				{Name: "window", Type: "string", Description: "The window id that owns it."},
				{Name: "rows", Type: "int", Description: "How many rows came with it."},
			},
			examples: []string{`{"id":1,"verb":"adopt-history-log","params":{"log":"work/3f2a","session":"work","window":"build"}}`},
			handler:  (*Daemon).verbAdoptHistoryLog,
		},
		"adopt-history": {
			description: "Make a window take over an orphaned log: adopt-history-log, called from the window's side. The same call with the same result.",
			params: []verbParam{
				sessionParam,
				windowParam,
				logParam,
			},
			returns: []verbParam{
				{Name: "log", Type: "string", Description: "The id the log has now."},
				{Name: "from", Type: "string", Description: "The id it had."},
				{Name: "window", Type: "string", Description: "The window id that owns it."},
				{Name: "rows", Type: "int", Description: "How many rows came with it."},
			},
			examples: []string{`{"id":1,"verb":"adopt-history","params":{"window":"build","log":"work/3f2a"}}`},
			handler:  (*Daemon).verbAdoptHistoryLog,
		},
		"purge-history-logs": {
			description: "Delete orphaned logs now, without waiting for the grace period: one log, the orphans closed more than N days ago, or all of them. Kept orphans and live logs are never deleted by older_than_days or all_orphaned. With dry_run it deletes nothing and lists what it would, with the token that confirms the deletion; that form is open to every caller. Without dry_run it is for the person, and needs that token.",
			params: []verbParam{
				{Name: "log", Type: "string", Description: "Purge this one orphan, by id. Refused while it is kept."},
				{Name: "older_than_days", Type: "int", Description: "Purge the orphans that closed more than this many days ago. 0 is every stamped orphan."},
				{Name: "all_orphaned", Type: "bool", Description: "Purge every orphan.", Default: "false"},
				{Name: "dry_run", Type: "bool", Description: "Delete nothing; list what would go.", Default: "false"},
				{Name: "confirm", Type: "string", Description: "The token from a dry_run or a confirm_required hint, for exactly the logs that would be deleted. The call fails with confirm_required again if the set changed."},
			},
			returns: []verbParam{
				{Name: "dry_run", Type: "bool", Description: "Whether nothing was deleted."},
				{Name: "purged", Type: "[]object", Description: "The logs deleted, or with dry_run the logs that would be, each as list-history-logs gives it."},
				{Name: "bytes", Type: "int", Description: "What they took on disk."},
				{Name: "confirm", Type: "string", Description: "With dry_run: the token the deletion takes."},
			},
			examples: []string{
				`{"id":1,"verb":"purge-history-logs","params":{"all_orphaned":true,"dry_run":true}}`,
				`{"id":1,"verb":"purge-history-logs","params":{"older_than_days":7,"confirm":"<from dry_run>"}}`,
			},
			handler: (*Daemon).verbPurgeHistoryLogs,
		},
	}
}
