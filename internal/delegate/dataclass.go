// dataclass.go is the data-class rule of placement (register SF-40): a fleet node the operator declared
// "public" in delegate_remote_classes is one whose admin is not the operator (a node shared into the tailnet by
// another account), so it may be placed public-data work only. Everything a contract holds - its goal, its
// context documents, the diff a review reads - is readable by that admin, and on 2026-09-27 a private handoff
// plan was dispatched to such a node as a context document.
//
// The rule is one predicate in the placement gate (eligibilityVerdict), so every route that places work reads
// it: the idle-local shortcut, the joint auto/remote deal, the spread deal, the capacity wait, a retry and a
// replacement. This file holds the predicate and the words the three narrations use (the gate's one-word
// verdict, noEligibleRemote's defer reason, the spread deal's slot reason).

package delegate

import (
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
)

// smokeDoor is the door fleet-smoke stamps on its probe contract (fleet_smoke_cmd.go). Its context is a fixed
// token string, so it is public by construction, and a public node must stay testable.
const smokeDoor = "cli:fleet-smoke"

// publicWork reports whether c is public-data work: every byte a node would read in it came from the public web
// or was made up by the harness, never from the operator's files or prompts. The research door builds one
// digest contract per page the guarded fetcher read from the public web (research.ValidateURL refuses a tailnet,
// private or loopback host), and fleet-smoke's probe is a constant. Nothing else qualifies, and that is
// deliberate and fail-closed:
//
//   - By the time a contract reaches placement, context_paths have been read and inlined as context documents
//     (PrepareContract), so a file and an inline document look the same here, and a goal typed by a model can
//     quote anything. Deciding "private" from the presence of a context document would let a goal-only contract
//     carry a pasted plan to a public node. So the discriminator is the DOOR, which every surface stamps itself
//     after intake (agent_delegate writes its own name into the struct it builds, research stamps after Prepare,
//     the CLI verbs stamp theirs): a caller cannot claim research by naming it in a subtask.
//   - An unstamped contract (Door empty) is private.
func publicWork(c core.AgentContract) bool {
	return researchDoors[c.Door] || c.Door == smokeDoor
}

// privateWork is !publicWork with the words for it: what makes c private, as "door agent_delegate, 2 context
// document(s)".
func privateWork(c core.AgentContract) (private bool, what string) {
	if publicWork(c) {
		return false, ""
	}
	door := strings.TrimSpace(c.Door)
	if door == "" {
		what = "no door stamped"
	} else {
		what = "door " + door
	}
	if n := len(c.Context); n > 0 {
		what += fmt.Sprintf(", %d context document(s)", n)
	}
	return true, what
}

// publicVerdict is the gate's word and detail for a public node and a private contract. It is the one place the
// words are written, so the one-word verdict line and the aggregate defer say the same thing.
func publicVerdict(c core.AgentContract) (word, detail string) {
	_, what := privateWork(c)
	return "private", "public node (delegate_remote_classes) takes public-data work only; this is not (" + what + ")"
}

// publicHeld splits views into the agent-lane nodes this contract may NOT go to because they are declared public
// and the contract is private, and the rest. The held nodes are named by laneID. A node that does not offer the
// agent lane is not "held": the gate refuses it for the lane first and it carries no class story.
func publicHeld(st Subtask, views []NodeView) (held []string, rest []NodeView) {
	private, _ := privateWork(st.Contract)
	for _, v := range views {
		if private && v.AgentEnabled && v.PublicOnly {
			held = append(held, laneID(v))
			continue
		}
		rest = append(rest, v)
	}
	return held, rest
}

// publicHeldSentence is the clause that names the rule in a defer reason. It says which nodes, why, and the
// two ways out, so a caller pinned route "remote" is never left with the generic "no eligible remote".
func publicHeldSentence(st Subtask, held []string) string {
	_, what := privateWork(st.Contract)
	return fmt.Sprintf("the data-class rule (delegate_remote_classes) excludes %d remote(s) declared public (%s): they take public-data work only, and this contract is not (%s); route it local, or list a private-ok node in delegate_remotes",
		len(held), strings.Join(held, ", "), what)
}
