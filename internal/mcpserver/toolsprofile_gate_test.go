package mcpserver

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
)

// gateTrigger marks a sentence as one that may carry a rule a caller must not be without. Two families. A constraint: a
// prohibition, an obligation, an inability, a trust boundary, a restriction or an opt-in. It is a deliberate superset of the words
// the compact profile was first reviewed against (never, refuse, only, must, do not, cannot, required): it adds every contraction
// ("can't", "won't"), plain "not" (which also covers "do not", "may not" and the upper-case "NOT"), "without", "opt-in" and the
// words the first lexical list kept (trusted, untrusted, unattended, sandbox, safety, advisory, read-only, forbidden, irreversible,
// destructive, denied). And data leaving the box: a sentence that says the call is forwarded (forward*), that the input travels
// with the job (travel*), that it runs on a fleet node (fleet node, fleet nodes), that it goes over the tailnet or is sent on
// (tailnet, sent), or that a cloud model sits behind a local
// service (cloud). Those carry no prohibition word and say what happens to the caller's bytes, which is a privacy rule
// all the same. It is written out here on purpose and is NOT the production lead-cut guard (leadGuard in toolsprofile.go):
// narrowing production's regexp cannot narrow this gate.
var gateTrigger = regexp.MustCompile(`(?i)\b(never|refus\w*|only|must|not|\w+n't|cannot|required|denied|trusted|untrusted|unattended|sandbox\w*|safety|advisory|read-only|forbidden|irreversible|destructive|opt-in|without|no (?:delete|shell|run|network|cloud|write)|forward\w*|travel\w*|fleet nodes?|tailnet|sent|cloud)\b`)

// droppedSentence records a constraint-word sentence the compact profile deliberately drops, in the tools named, and why that is
// safe.
type droppedSentence struct {
	Tools    []string
	Sentence string
	Reason   string
}

// fleetSurfaceCfg is the same optional surfaces as fullSurfaceCfg with the three accelerators listed as FLEET accelerators, whose
// descriptions carry the "[FLEET: ...]" suffix.
func fleetSurfaceCfg(profile string) config.Config {
	c := browseCfg()
	c.AgentDelegationEnabled = true
	c.FleetAccelerators = []string{"hailo-8l", "coral-edgetpu", "rknpu"}
	c.ToolsSchemaProfile = profile
	return c
}

const toolDescKey = "(tool)"

// descriptionsByPath returns every description string of a tool keyed by where it lives: toolDescKey for the tool's own, and the
// schema path ("#/properties/pin_reason") for each parameter at any depth.
func descriptionsByPath(tool *mcp.Tool) map[string]string {
	out := map[string]string{toolDescKey: tool.Description}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch n := v.(type) {
		case map[string]any:
			for k, x := range n {
				if s, ok := x.(string); ok && k == "description" {
					out[path] = s
					continue
				}
				walk(path+"/"+k, x)
			}
		case []any:
			for i, x := range n {
				walk(fmt.Sprintf("%s/%d", path, i), x)
			}
		}
	}
	walk("#", tool.InputSchema)
	return out
}

// gateLeadCount is how many opening sentences the cut keeps unconditionally: the first, and the next while the run is shorter than
// compactLeadMin.
func gateLeadCount(sents []string) int {
	n, acc := 0, 0
	for _, s := range sents {
		if acc >= compactLeadMin {
			break
		}
		acc += len(s)
		n++
	}
	return n
}

// droppedIndex flattens the dropped list to tool -> sentence -> reason and refuses a malformed entry: no tool, no reason, a
// sentence listed twice for one tool.
func droppedIndex(t *testing.T) map[string]map[string]string {
	t.Helper()
	idx := map[string]map[string]string{}
	for i, d := range compactDropped {
		if len(d.Tools) == 0 || strings.TrimSpace(d.Sentence) == "" {
			t.Errorf("dropped entry %d names no tool or no sentence: %+v", i, d)
			continue
		}
		if strings.TrimSpace(d.Reason) == "" {
			t.Errorf("dropped entry %d (%.60q) carries no reason", i, d.Sentence)
		}
		for _, tool := range d.Tools {
			if idx[tool] == nil {
				idx[tool] = map[string]string{}
			}
			if _, dup := idx[tool][d.Sentence]; dup {
				t.Errorf("%s: the sentence is on the dropped list twice: %.80q", tool, d.Sentence)
			}
			idx[tool][d.Sentence] = d.Reason
		}
	}
	return idx
}

// commonPrefixLen is the length of the longest shared prefix of a and b.
func commonPrefixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// gateResult is what one tool contributes to the review gate.
type gateResult struct {
	kept, dropped, leads, leadsCut int
	problems                       []string
}

// gateTool classifies every constraint sentence of one tool, comparing its full and compact forms: dropped is the tool's
// dropped-with-reason index (sentence -> reason).
func gateTool(ft, ct *mcp.Tool, dropped map[string]string) gateResult {
	var res gateResult
	fd, cd := descriptionsByPath(ft), descriptionsByPath(ct)
	paths := make([]string, 0, len(fd))
	for p := range fd {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		sents := splitSentences(fd[path])
		lead := gateLeadCount(sents)
		for i, s := range sents {
			if !gateTrigger.MatchString(s) {
				continue
			}
			if i == 0 {
				res.leads++
				if strings.Contains(cd[path], s) {
					continue
				}
				res.leadsCut++
				if tail := s[commonPrefixLen(s, cd[path]):]; gateTrigger.MatchString(tail) {
					res.problems = append(res.problems, fmt.Sprintf("%s %s: the opening sentence was cut and the dropped tail carries a constraint word: %q", ft.Name, path, tail))
				}
				continue
			}
			if i < lead {
				continue
			}
			_, isDropped := dropped[s]
			switch {
			case strings.Contains(cd[path], s):
				res.kept++
			case isDropped:
				res.dropped++
			default:
				res.problems = append(res.problems, fmt.Sprintf("a constraint sentence is neither in the compact description nor on the dropped list: %s %s sentence %d: %s", ft.Name, path, i, s))
			}
		}
	}
	return res
}

// TestEveryConstraintSentenceIsKeptOrDroppedWithReason is the review gate of the compact profile. It reads the LIVE full
// descriptions exactly as tools/list emits them (every tool of every surface in gateSurfaces: each optional lane on, the three
// accelerators together as local and as fleet devices, and each device alone as a local and as a fleet accelerator, so the tool
// names the shared-name rule hides on a combined box are read too, and a forwarder is read with the note appended to it), and each
// parameter at any depth. It cuts them into sentences with the production splitter, and for every sentence that carries a
// constraint word or says the data leaves the box (gateTrigger) outside the opening sentence(s) it requires one of two things:
// the sentence is in the COMPACT description of that same tool and parameter, verbatim, or it is on the dropped list
// (toolsprofile_dropped_test.go) with a reason. A description that gains a new rule therefore turns this test red until someone
// has read the sentence and put it on one list or the other. The check reads the compact OUTPUT, so a keep-list entry that does
// not match what the splitter produces is caught too.
//
// An opening sentence is kept by rule, and the cut may shorten it at a clause boundary only when the part it drops carries no
// trigger word: that half is checked in gateTool with gateTrigger, independently of the production guard.
func TestEveryConstraintSentenceIsKeptOrDroppedWithReason(t *testing.T) {
	dropped := droppedIndex(t)
	var total gateResult
	for _, surface := range gateSurfaces() {
		compact := map[string]*mcp.Tool{}
		for _, tool := range listTools(t, surface.cfg("compact")) {
			compact[tool.Name] = tool
		}
		fullTools := listTools(t, surface.cfg("full"))
		switch {
		case surface.device != "":
			// A single-device surface must really register its device's table, or the pass over it is vacuous.
			requireSurfaceRegistersItsDevice(t, surface, fullTools)
		case surface.name == "fleet accelerators":
			// The variant must really register the forwarders, or the pass over it is vacuous: every accelerator tool carries
			// the "[FLEET: ...]" suffix there, and the surface is as large as the full one.
			fleetTagged := 0
			for _, ft := range fullTools {
				if strings.Contains(ft.Description, "[FLEET:") {
					fleetTagged++
				}
			}
			if len(fullTools) < 44 || fleetTagged < 10 {
				t.Fatalf("the fleet-accelerator surface has %d tools and %d with a [FLEET: ...] suffix: it no longer registers the forwarders", len(fullTools), fleetTagged)
			}
		}
		for _, ft := range fullTools {
			ct, ok := compact[ft.Name]
			if !ok {
				t.Errorf("%s: %s is in the full list and not in the compact one", surface.name, ft.Name)
				continue
			}
			r := gateTool(ft, ct, dropped[ft.Name])
			total.kept += r.kept
			total.dropped += r.dropped
			total.leads += r.leads
			total.leadsCut += r.leadsCut
			for _, p := range r.problems {
				t.Errorf("%s: %s", surface.name, p)
			}
		}
	}
	if total.kept < 40 || total.dropped < 40 || total.leads < 40 {
		t.Fatalf("the gate saw %d kept, %d dropped and %d opening sentences with a trigger word: it is blind (leads cut: %d)", total.kept, total.dropped, total.leads, total.leadsCut)
	}
}

// TestGateFiresOnANewConstraintSentence proves the gate is not vacuous: a tool whose description gains a sentence with a constraint
// word is reported until that sentence is kept or dropped with a reason, and a reworded sentence is reported again.
func TestGateFiresOnANewConstraintSentence(t *testing.T) {
	const name = "synthetic_gate_tool"
	const rule = "A caller may pass the token only once and must never share it."
	full := &mcp.Tool{Name: name, Description: "Do the first useful thing for the caller here. " + rule + " Plain description follows."}

	if r := gateTool(full, compactTool(full), nil); len(r.problems) != 1 || !strings.Contains(r.problems[0], rule) {
		t.Fatalf("a new rule was not reported exactly once: %v", r.problems)
	}
	if r := gateTool(full, compactTool(full), map[string]string{rule: "reviewed"}); len(r.problems) != 0 || r.dropped != 1 {
		t.Fatalf("a dropped-with-reason rule was still reported: %+v", r)
	}
	compactKeepByTool[name] = map[string]bool{rule: true}
	t.Cleanup(func() { delete(compactKeepByTool, name) })
	if r := gateTool(full, compactTool(full), nil); len(r.problems) != 0 || r.kept != 1 {
		t.Fatalf("a kept rule was reported: %+v", r)
	}
	// A keep entry that no longer matches the description (the sentence was reworded) keeps nothing, and the gate sees it.
	reworded := &mcp.Tool{Name: name, Description: "Do the first useful thing for the caller here. A caller may pass the token only once and must never share it, ever. Plain description follows."}
	if r := gateTool(reworded, compactTool(reworded), nil); len(r.problems) != 1 {
		t.Fatalf("a reworded rule slipped past a stale keep entry: %+v", r)
	}
}

// TestLeadCutNeverDropsAConstraintWord: a long opening sentence is shortened only where the dropped tail is free of the gate's
// words. The tail is checked with gateTrigger, which production does not use.
func TestLeadCutNeverDropsAConstraintWord(t *testing.T) {
	lead := "Do the first useful thing for the caller right here; " + strings.Repeat("then explain with plain words at some length ", 8) + "and why you must never skip it."
	if got := compactTool(&mcp.Tool{Name: "synthetic_lead_tool", Description: lead + " Next."}).Description; got != lead {
		t.Fatalf("the cut dropped a constraint clause from a long opening sentence: %q", got)
	}
	long := "Do the first useful thing for the caller right here and keep going; " + strings.Repeat("then continue with plain words that carry no rule at all ", 8) + "and stop."
	cut := compactTool(&mcp.Tool{Name: "synthetic_lead_tool", Description: long + " Next."}).Description
	if len(cut) >= len(long) || gateTrigger.MatchString(long[commonPrefixLen(long, cut):]) {
		t.Fatalf("a clean tail was not cut, or the cut tail carries a constraint word: %q", cut)
	}
}

// TestLeadGuardCoversTheGate: every word the gate classifies by is a word production's lead guard refuses to cut. Narrowing the
// guard turns this red.
func TestLeadGuardCoversTheGate(t *testing.T) {
	for _, w := range []string{"never", "refuse", "refused", "refuses", "only", "must", "not", "do not", "may not", "don't", "can't", "won't", "cannot", "required", "denied", "trusted", "untrusted", "unattended", "sandbox", "sandboxed", "safety", "advisory", "read-only", "forbidden", "irreversible", "destructive", "opt-in", "without", "no delete", "no shell", "no run", "no network", "no cloud", "no write", "NOT", "ONLY", "forward", "forwarded", "forwards", "forwarding", "travel", "travels", "travelled", "tailnet", "sent", "cloud", "fleet node", "fleet nodes", "defer", "defers", "deferred", "deferred:true", "deferring", "deferral", "deferrals"} {
		probe := "A sentence that says " + w + " here."
		if !gateTrigger.MatchString(probe) {
			t.Errorf("the gate does not classify by %q", w)
		}
		if !leadGuard.MatchString(probe) {
			t.Errorf("the lead guard does not protect %q, which the gate classifies by", w)
		}
	}
}
