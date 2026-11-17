package session

import (
	"log"
	"slices"

	"ofam.dev/mtmd/internal/config"
)

// The person's acts a pane's process may do: [agents.permissions]
// person_verbs (config/person_verbs.go).
//
// A verb that acts as the person proves it with a nonce (human_sender.go) or
// a call from outside every pane (mayActAsHuman). A process in a pane has
// neither. It may still do an act that person_verbs names, without a nonce,
// as the respond grant answers a prompt without one. Every such verb asks the
// one question here, with its own name; no verb has a rule of its own.
//
// What person_verbs never lifts: a call over a link (the link policy of
// [hosts] decides those), a pane of another machine hosted here (paneOnly), a
// plugin's own connection, and a connection that restricted itself.

// SetPersonVerbs replaces the acts a pane's process may do. Nil is the
// default, config.DefaultPanePersonVerbs.
func (d *Daemon) SetPersonVerbs(names []string) {
	if names == nil {
		names = config.DefaultPanePersonVerbs()
	}
	names = slices.Clone(names)
	d.personVerbs.Store(&names)
}

// personVerbsInForce is the list the daemon checks now.
func (d *Daemon) personVerbsInForce() []string {
	if p := d.personVerbs.Load(); p != nil {
		return *p
	}
	return config.DefaultPanePersonVerbs()
}

// applyPersonVerbs applies person_verbs read from config.toml. From a file
// change (byPerson false) only what narrows applies: a name the list in
// force lacks waits for tuios config apply or a restart, like a wider
// [agents.permissions] grants. It reports whether something waits.
func (d *Daemon) applyPersonVerbs(names []string, byPerson bool) bool {
	if byPerson {
		d.SetPersonVerbs(names)
		return false
	}
	now := d.personVerbsInForce()
	kept := []string{}
	wait := false
	for _, n := range names {
		if slices.Contains(now, n) {
			kept = append(kept, n)
		} else {
			wait = true
		}
	}
	if wait {
		log.Printf("[agents.permissions] person_verbs in the file gives panes more than before. The narrower list stays in force until tuios config apply from outside tuios, or a daemon restart.")
	}
	d.SetPersonVerbs(kept)
	return wait
}

// panePersonVerb reports whether cs is a process in a pane of this daemon
// that person_verbs lets do verb as the person.
func (d *Daemon) panePersonVerb(verb string, cs *connState) bool {
	if cs == nil || cs.viaLink || cs.paneOnly || cs.pluginBound.Load() != nil || cs.scope.Load() != nil {
		return false
	}
	if !d.connFromPane(cs) {
		return false
	}
	return slices.Contains(d.personVerbsInForce(), verb)
}

// mayActAsPersonFor is mayActAsHuman for verb: the person, or a pane that
// person_verbs lets do verb.
func (d *Daemon) mayActAsPersonFor(verb string, cs *connState) bool {
	return d.mayActAsHuman(cs) || d.panePersonVerb(verb, cs)
}

// personHeld is humanNonceHeld for verb: a nonce sender may use, or a pane
// that person_verbs lets do verb without one.
func (d *Daemon) personHeld(verb, nonce string, sender *connState) bool {
	return d.humanNonceHeld(nonce, sender) || d.panePersonVerb(verb, sender)
}

// personFor is humanNonceFor for verb. For a pane person_verbs lets do verb,
// the id it returns, for answered_by, is the pane's connection's client id.
func (d *Daemon) personFor(verb, nonce, sessionID string, sender *connState) (string, bool) {
	if by, ok := d.humanNonceFor(nonce, sessionID, sender); ok {
		return by, true
	}
	if d.panePersonVerb(verb, sender) {
		return sender.clientID, true
	}
	return "", false
}

// personAttach is verifyHumanNonce for verb: an attach to sessionID, or a
// pane person_verbs lets do verb.
func (d *Daemon) personAttach(verb, nonce, sessionID string, sender *connState) bool {
	if nonce != "" && d.verifyHumanNonce(nonce, sessionID, sender) {
		return true
	}
	return d.panePersonVerb(verb, sender)
}
