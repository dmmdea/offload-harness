package delegate

import (
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
)

// CheckRosterRemotes is the rule for the doors where a MODEL names the remotes of a call (the MCP
// agent_delegate tool): the list may only NARROW the configured fleet, never add a node to it. Every
// entry must be one of cfg.DelegateRemotes, and a box that configures none accepts none.
//
// Fleet membership is configuration: the operator lists the nodes this box delegates to in
// delegate_remotes, and a node that is not listed is not part of the fleet. A call's own list already
// REPLACED the roster rather than merging with it (so one node can be targeted deliberately), and before
// this rule the only check on an entry was its SHAPE (netguard.TailnetURL: loopback, the tailnet's
// address range or zone, a dotless name). Any tailnet host therefore passed, and was dialled, with the
// fleet's bearer token on its health read; the operator's standing rule that a box never delegates to
// the standalone machine, and is never a delegator for it, held by habit alone. A caller-named URL
// outside the roster is now refused with a message that says why, before anything is dialled.
//
// An entry is compared as the roster spells it, without a trailing slash and without regard to case
// ("http://Node-A:18811/" is "http://node-a:18811"); a node the roster lists by address is not
// reachable under another name for it, because the roster is what the operator vetted.
//
// The operator's own CLI verbs (`delegate --remote`, `fleet-smoke --remote`) keep naming any tailnet
// node, which is how a node that has not joined the roster yet is smoke-tested: the engine applies this
// rule only when RunOptions.RosterOnly says the list came from a model.
func CheckRosterRemotes(cfg config.Config, named []string) error {
	_, err := CanonicalRosterRemotes(cfg, named)
	return err
}

// CanonicalRosterRemotes is CheckRosterRemotes that also hands back what it accepted: each entry
// spelled as the roster spells it, in the order the caller named them, a node named twice (or in two
// spellings of one base) kept once.
//
// The respelling is not cosmetic. The check compares entries loosely (case, a trailing slash), but
// everything after it keys a node by the string it was given: the process-wide in-flight gate
// (processGate) counts open dispatches per dial base, and "http://Node-A:18811/" and "http://node-a:18811"
// would be two bases for one node, each admitted up to the node's ceiling, so a model that spelled the
// roster's node differently could hold twice the in-flight limit the node's admission was sized for.
// With the roster's own spelling, one node is one key. An empty named list returns nil (the roster is
// used as configured).
func CanonicalRosterRemotes(cfg config.Config, named []string) ([]string, error) {
	if len(named) == 0 {
		return nil, nil
	}
	if len(cfg.DelegateRemotes) == 0 {
		return nil, fmt.Errorf("remotes: this box has no delegate_remotes, so a call cannot name a node (%s): a call's remotes list can only narrow the configured fleet, never add to it; add the node to delegate_remotes in the config and restart the server, or omit remotes", strings.Join(named, ", "))
	}
	known := make(map[string]string, len(cfg.DelegateRemotes))
	for _, base := range cfg.DelegateRemotes {
		if _, dup := known[rosterKey(base)]; !dup {
			known[rosterKey(base)] = base
		}
	}
	var outside []string
	canonical := make([]string, 0, len(named))
	seen := make(map[string]bool, len(named))
	for _, base := range named {
		spelled, ok := known[rosterKey(base)]
		if !ok {
			outside = append(outside, fmt.Sprintf("%q", base))
			continue
		}
		if !seen[spelled] {
			seen[spelled] = true
			canonical = append(canonical, spelled)
		}
	}
	if len(outside) == 0 {
		return canonical, nil
	}
	return nil, fmt.Errorf("remotes: %s not in this box's delegate_remotes (%s): a call's remotes list can only narrow the configured fleet, never add a node; add it to delegate_remotes and restart the server, or drop it from the call", describeOutside(outside), strings.Join(cfg.DelegateRemotes, ", "))
}

func describeOutside(quoted []string) string {
	if len(quoted) == 1 {
		return quoted[0] + " is"
	}
	return strings.Join(quoted, ", ") + " are"
}

// rosterKey is a node base reduced to what identifies it for this check: lower case, without surrounding
// space or a trailing slash. Nothing cleverer is wanted: the roster is what the operator wrote, a caller
// has to spell the node as the roster does, and a malformed entry can never match a well-formed one
// because both sides go through the same plain normalisation.
func rosterKey(base string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(base), "/"))
}
