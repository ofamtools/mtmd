package config

import (
	"fmt"
	"slices"
	"strings"
)

// Which of the person's acts a process in a pane may do.
//
//	[agents.permissions]
//	person_verbs = ["agent-transcript", "dismiss-attention", "attach-presence"]
//
// Some verbs act as the person: they read or clear the Inbox, answer for the
// person, or change what the person trusts. From outside every pane they
// need the person's proof (an attach or presence nonce, or a call from a
// terminal outside tuios). A process inside a pane has no such proof, so the
// daemon refuses it these acts unless the act is named in person_verbs. A
// named act runs for the pane without a nonce, as the respond grant answers a
// prompt without one.
//
// Unset, person_verbs is the default: every act PersonVerbTable marks
// PaneDefault. An empty list names none, which refuses every one of them to
// a pane. Names outside PersonVerbTable are dropped with a warning.
//
// It is file-plane config like the rest of [agents.permissions]: a change to
// the file applies only where it narrows; a widening waits for tuios config
// apply from outside tuios, or a daemon restart. It is not a set-option path,
// because set-option is a verb any pane may call.

// PersonVerb is one act of the person a pane's process may be allowed.
type PersonVerb struct {
	// Name is the verb, or for an act that is one form of a verb, the verb
	// and what it acts on ("set-option agents.enabled").
	Name string
	// PaneDefault is whether a pane may do it when person_verbs is unset.
	PaneDefault bool
}

// PersonVerbTable is every act a pane's process is refused unless
// person_verbs names it, and the default for each. The consent steps (an
// answer to an approval or a question, trusting a plugin, applying a wider
// config, letting work leave the machine) default to the person only; the
// rest default to allowed. To change a default, change its PaneDefault.
var PersonVerbTable = []PersonVerb{
	{"agent-transcript", true},
	{"dismiss-attention", true},
	{"mark-attention", true},
	{"reply-approval", false},
	{"answer-ask", false},
	{"release-agent-message", true},
	{"register-push", true},
	{"list-push", true},
	{"remove-push", true},
	{"attach-presence", true},
	{"paste-image", true},
	{"reply-history", true},
	{"add-reply-history", true},
	{"set-option agents.enabled", true},
	{"plugin.approve", false},
	{"plugin.enable", false},
	{"plugin.disable", false},
	{"plugin.restart", false},
	{"plugin.link", false},
	{"plugin.unlink", false},
	{"adopt-history-log", true},
	{"purge-history-logs", true},
	{"apply-config", false},
	{"ship-push", false},
	{"ship-pr", false},
	{"tml-input", true},
	{"tml-place", true},
}

// PersonVerbNames is every name in PersonVerbTable, in its order.
func PersonVerbNames() []string {
	out := make([]string, 0, len(PersonVerbTable))
	for _, v := range PersonVerbTable {
		out = append(out, v.Name)
	}
	return out
}

// DefaultPanePersonVerbs is what a pane may do when person_verbs is unset.
func DefaultPanePersonVerbs() []string {
	out := []string{}
	for _, v := range PersonVerbTable {
		if v.PaneDefault {
			out = append(out, v.Name)
		}
	}
	return out
}

// ResolvedPersonVerbs reads person_verbs: nil is the default, and names
// outside PersonVerbTable are dropped. The result is never nil, and it is in
// table order without repeats.
func (c PermissionsConfig) ResolvedPersonVerbs() []string {
	if c.PersonVerbs == nil {
		return DefaultPanePersonVerbs()
	}
	valid, _ := CanonicalPersonVerbs(c.PersonVerbs)
	return valid
}

// CanonicalPersonVerbs returns the names in names that PersonVerbTable has,
// trimmed, without repeats and in table order, and the names it dropped.
func CanonicalPersonVerbs(names []string) (valid, unknown []string) {
	known := PersonVerbNames()
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if slices.Contains(known, n) {
			seen[n] = true
		} else if n != "" {
			unknown = append(unknown, n)
		}
	}
	valid = []string{}
	for _, n := range known {
		if seen[n] {
			valid = append(valid, n)
		}
	}
	return valid, unknown
}

// validatePersonVerbs warns about a name person_verbs holds that no act has.
// It is dropped, which fails toward less authority.
func validatePersonVerbs(cfg *UserConfig, result *ValidationResult) {
	if _, unknown := CanonicalPersonVerbs(cfg.Agents.Permissions.PersonVerbs); len(unknown) > 0 {
		result.Warnings = append(result.Warnings, ValidationError{
			Field:   "agents.permissions",
			Key:     "person_verbs",
			Message: fmt.Sprintf("unknown verb %s (allowed: %s); dropped", strings.Join(unknown, ", "), strings.Join(PersonVerbNames(), ", ")),
		})
	}
}
