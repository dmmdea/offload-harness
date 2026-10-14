package mcpserver

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
)

// gateTrigger marks a sentence as one that carries a rule word. The gate no longer depends on it to find the sentences it reads
// (every non-opening sentence must be kept or on the dropped list, see TestEveryConstraintSentenceIsKeptOrDroppedWithReason), so it is
// the SECOND check: a sentence on the dropped list that carries one of these words needs an explicit reason (checkDroppedRows), not
// only a reason class, and the clause-boundary cut of an opening sentence may not drop a tail that carries one.
//
// Three families. A constraint: a prohibition, an obligation, an inability, a trust boundary, a restriction or an opt-in (never,
// refuse*, only, must, not, any contraction, cannot, required, without, opt-in, the sandbox family, no delete|shell|run|network|cloud|write).
// An instruction in the second person that is not phrased as a prohibition (always, should, avoid*, make sure, prohibit*, ensure*) and
// the deferral protocol (defer*). And data leaving the box: a sentence that says the call is forwarded (forward*), that the input
// travels with the job (travel*), that it runs on a fleet node (fleet node, fleet nodes), that it goes over the tailnet or is sent on
// (tailnet, sent), or that a cloud model sits behind a local service (cloud). It is written out here on purpose and is NOT the
// production lead-cut guard (leadGuard in toolsprofile.go): narrowing production's regexp cannot narrow this gate.
var gateTrigger = regexp.MustCompile(`(?i)\b(never|refus\w*|only|must|not|\w+n't|cannot|required|denied|trusted|untrusted|unattended|sandbox\w*|safety|advisory|read-only|forbidden|irreversible|destructive|opt-in|without|no (?:delete|shell|run|network|cloud|write)|forward\w*|travel\w*|fleet nodes?|tailnet|sent|cloud|defer\w*|always|should|avoid\w*|make sure|prohibit\w*|ensure\w*)\b`)

// acceptanceRule marks the sentences that teach how to write acceptance: the lint that flags acceptance which verifies less than it looks
// (PARROT-PASSABLE, SHAPE-ONLY, "passes garbage") and the remedy that says where to anchor a check. They are never
// dropped, in any tool: a caller that does not see them writes a lone nonempty: check that passes garbage.
var acceptanceRule = regexp.MustCompile(`(?i)\bparrot-passable\b|\bshape-only\b|\bpasses garbage\b|\banchor >=1\b`)

// droppedSentence records a sentence the compact profile deliberately drops, in the tools named, with the class of reason that makes
// that safe (a key of dropClasses) and, for a sentence that carries a rule word, an explicit reason of its own.
type droppedSentence struct {
	Tools    []string
	Sentence string
	Class    string
	Reason   string
}

// minExplicitReason is the shortest reason that can be explicit: a class name or a few words restate the class, they do not say why
// this sentence is safe to drop.
const minExplicitReason = 30

// checkDroppedRows returns one problem per defect of the dropped list: a row with no tool or sentence, a class that is not in
// dropClasses, a sentence listed twice for one tool, and a sentence that carries a
// rule word (gateTrigger) whose reason is missing, short or only a class description.
func checkDroppedRows(rows []droppedSentence) []string {
	var problems []string
	seen := map[string]bool{}
	for i, d := range rows {
		if len(d.Tools) == 0 || strings.TrimSpace(d.Sentence) == "" {
			problems = append(problems, fmt.Sprintf("dropped row %d names no tool or no sentence: %+v", i, d))
			continue
		}
		if _, ok := dropClasses[d.Class]; !ok {
			problems = append(problems, fmt.Sprintf("dropped row %d (%.60q) has the reason class %q, which is not one of the declared classes", i, d.Sentence, d.Class))
		}
		if gateTrigger.MatchString(d.Sentence) {
			reason := strings.TrimSpace(d.Reason)
			switch {
			case len(reason) < minExplicitReason:
				problems = append(problems, fmt.Sprintf("dropped row %d carries a rule word and needs an explicit reason of at least %d characters, has %q: %.80q", i, minExplicitReason, reason, d.Sentence))
			case isClassText(reason):
				problems = append(problems, fmt.Sprintf("dropped row %d carries a rule word and its reason only restates its class: %.80q", i, d.Sentence))
			}
		}
		for _, tool := range d.Tools {
			key := tool + "\x00" + d.Sentence
			if seen[key] {
				problems = append(problems, fmt.Sprintf("%s: the sentence is on the dropped list twice: %.80q", tool, d.Sentence))
			}
			seen[key] = true
		}
	}
	return problems
}

func isClassText(reason string) bool {
	for _, text := range dropClasses {
		if reason == text {
			return true
		}
	}
	return false
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

// droppedIndex flattens the dropped list to tool -> sentence -> row and fails the test for every defect checkDroppedRows reports.
func droppedIndex(t *testing.T) map[string]map[string]droppedSentence {
	t.Helper()
	for _, p := range checkDroppedRows(compactDropped) {
		t.Error(p)
	}
	idx := map[string]map[string]droppedSentence{}
	for _, d := range compactDropped {
		for _, tool := range d.Tools {
			if idx[tool] == nil {
				idx[tool] = map[string]droppedSentence{}
			}
			idx[tool][d.Sentence] = d
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

// gateTool classifies every sentence of one tool, comparing its full and compact forms: dropped is the tool's dropped list
// (sentence -> row). An opening sentence is kept by rule (and, when the cut shortened it, the part it dropped may carry no rule word).
// Every other sentence, whatever words it holds, must be in the COMPACT description of the same tool and parameter, verbatim, or on
// the dropped list.
func gateTool(ft, ct *mcp.Tool, dropped map[string]droppedSentence) gateResult {
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
			if i == 0 {
				if !gateTrigger.MatchString(s) {
					continue
				}
				res.leads++
				if strings.Contains(cd[path], s) {
					continue
				}
				res.leadsCut++
				if tail := s[commonPrefixLen(s, cd[path]):]; gateTrigger.MatchString(tail) {
					res.problems = append(res.problems, fmt.Sprintf("%s %s: the opening sentence was cut and the dropped tail carries a rule word: %q", ft.Name, path, tail))
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
				res.problems = append(res.problems, fmt.Sprintf("a sentence is neither in the compact description nor on the dropped list: %s %s sentence %d: %s", ft.Name, path, i, s))
			}
		}
	}
	return res
}

// compactForGate is what a client receives for tool under the compact profile: compactTool's result with its schema decoded the way a
// client decodes it (compactTool returns the schema as raw JSON, which descriptionsByPath cannot walk).
func compactForGate(t *testing.T, tool *mcp.Tool) *mcp.Tool {
	t.Helper()
	c := compactTool(tool)
	if c.InputSchema == nil {
		return c
	}
	b, err := json.Marshal(c.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatal(err)
	}
	c.InputSchema = schema
	return c
}

// gateSurfaceProblems runs the gate over one surface and returns every problem and the totals. A tool that is in the full list and
// not in the compact one is a problem too.
func gateSurfaceProblems(t *testing.T, surface gateSurface, dropped map[string]map[string]droppedSentence, mutate func(*mcp.Tool) *mcp.Tool) (gateResult, []string) {
	t.Helper()
	var total gateResult
	var problems []string
	for _, ft := range listTools(t, surface.cfg("full")) {
		if mutate != nil {
			ft = mutate(ft)
		}
		r := gateTool(ft, compactForGate(t, ft), dropped[ft.Name])
		total.kept += r.kept
		total.dropped += r.dropped
		total.leads += r.leads
		total.leadsCut += r.leadsCut
		problems = append(problems, r.problems...)
	}
	return total, problems
}

// TestEveryConstraintSentenceIsKeptOrDroppedWithReason is the review gate of the compact profile. It reads the LIVE full
// descriptions exactly as tools/list emits them (every tool of every surface in gateSurfaces: each optional lane on, the three
// accelerators together as local and as fleet devices, and each device alone as a local and as a fleet accelerator, so the tool
// names the shared-name rule hides on a combined box are read too, and a forwarder is read with the note appended to it), and each
// parameter at any depth. It cuts them into sentences with the production splitter, and for EVERY sentence outside the opening
// sentence(s) it requires one of two things: the sentence is in the COMPACT description of that same tool and parameter, verbatim, or
// it is on the dropped list (toolsprofile_dropped_test.go) with a reason class. The words a sentence holds decide nothing: a rule
// reworded as "Always ...", "You should ..." or "Make sure ..." is as unclassified as a rule with "never" in it, and so is a plain
// sentence. A description that gains or rewords a sentence therefore turns this test red until someone has read it and put it on one
// list or the other. The check reads the compact OUTPUT, so a keep-list entry that does not match what the splitter produces is caught
// too.
//
// An opening sentence is kept by rule, and the cut may shorten it at a clause boundary only when the part it drops carries no
// rule word: that half is checked in gateTool with gateTrigger, independently of the production guard.
func TestEveryConstraintSentenceIsKeptOrDroppedWithReason(t *testing.T) {
	dropped := droppedIndex(t)
	var total gateResult
	for _, surface := range gateSurfaces() {
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
		r, problems := gateSurfaceProblems(t, surface, dropped, nil)
		total.kept += r.kept
		total.dropped += r.dropped
		total.leads += r.leads
		total.leadsCut += r.leadsCut
		for _, p := range problems {
			t.Errorf("%s: %s", surface.name, p)
		}
	}
	if total.kept < 100 || total.dropped < 1000 || total.leads < 40 {
		t.Fatalf("the gate saw %d kept, %d dropped and %d opening sentences with a rule word: it is blind (leads cut: %d)", total.kept, total.dropped, total.leads, total.leadsCut)
	}
}

// TestGateFiresOnANewConstraintSentence proves the gate is not vacuous: a tool whose description gains a sentence is reported until
// that sentence is kept or dropped with a reason, a reworded sentence is reported again, and a sentence with no rule word at all is
// reported the same way.
func TestGateFiresOnANewConstraintSentence(t *testing.T) {
	const name = "synthetic_gate_tool"
	const rule = "A caller may pass the token only once and must never share it."
	const plain = "Plain description follows."
	full := &mcp.Tool{Name: name, Description: "Do the first useful thing for the caller here. " + rule + " " + plain}
	row := func(s string) droppedSentence {
		return droppedSentence{Tools: []string{name}, Sentence: s, Class: dropDescr}
	}

	if r := gateTool(full, compactTool(full), nil); len(r.problems) != 2 || !strings.Contains(r.problems[0], rule) || !strings.Contains(r.problems[1], plain) {
		t.Fatalf("a new rule and a plain sentence were not each reported exactly once: %v", r.problems)
	}
	both := map[string]droppedSentence{rule: row(rule), plain: row(plain)}
	if r := gateTool(full, compactTool(full), both); len(r.problems) != 0 || r.dropped != 2 {
		t.Fatalf("dropped-with-reason sentences were still reported: %+v", r)
	}
	compactKeepByTool[name] = map[string]bool{rule: true}
	t.Cleanup(func() { delete(compactKeepByTool, name) })
	if r := gateTool(full, compactTool(full), map[string]droppedSentence{plain: row(plain)}); len(r.problems) != 0 || r.kept != 1 || r.dropped != 1 {
		t.Fatalf("a kept rule was reported: %+v", r)
	}
	// A keep entry that no longer matches the description (the sentence was reworded) keeps nothing, and the gate sees it.
	reworded := &mcp.Tool{Name: name, Description: "Do the first useful thing for the caller here. A caller may pass the token only once and must never share it, ever. " + plain}
	if r := gateTool(reworded, compactTool(reworded), map[string]droppedSentence{plain: row(plain)}); len(r.problems) != 1 {
		t.Fatalf("a reworded rule slipped past a stale keep entry: %+v", r)
	}
}

// unclassifiedProbes are sentences a caller could be left without: four rules the first gate (a keyword list) did not see, because
// they are phrased with words it did not list, and one sentence that holds no rule word at all. Every one of them, put into any
// description, turns the gate red until it is kept or dropped.
var unclassifiedProbes = []string{
	"Always pass the token twice before sharing it.",
	"You should send the audio as mp3 first.",
	"Avoid sharing the token with anyone.",
	"Make sure the key is rotated after use.",
	"The file is written next to the input.",
}

// withToolDescription returns a copy of tool whose description is f(description).
func withToolDescription(tool *mcp.Tool, f func(string) string) *mcp.Tool {
	c := *tool
	c.Description = f(tool.Description)
	return &c
}

// addSentence appends a sentence to a description, closing the last sentence first when it has no full stop, so the probe is a sentence
// of its own and not the tail of the one before it.
func addSentence(desc, sentence string) string {
	desc = strings.TrimRight(desc, " ")
	if !strings.HasSuffix(desc, ".") && !strings.HasSuffix(desc, "!") && !strings.HasSuffix(desc, "?") {
		desc += "."
	}
	return desc + " " + sentence
}

// withParamDescription returns a copy of tool whose parameter at the schema path (a property name, then "properties" names or
// "items" as needed, the way paramDescription reads it) has the description f(description). The schema is copied through JSON, so the
// registry's value is never touched.
func withParamDescription(t *testing.T, tool *mcp.Tool, f func(string) string, path ...string) *mcp.Tool {
	t.Helper()
	b, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatal(err)
	}
	cur := schema
	cur, _ = cur["properties"].(map[string]any)
	for _, k := range path {
		next, ok := cur[k].(map[string]any)
		if !ok {
			t.Fatalf("%s: no %q on the path %v", tool.Name, k, path)
		}
		cur = next
	}
	desc, _ := cur["description"].(string)
	cur["description"] = f(desc)
	c := *tool
	c.InputSchema = schema
	return &c
}

// TestGateReportsEveryUnclassifiedProbeInALiveDescription: each probe, inserted after the opening sentence of a live tool's
// description and at the end of a live parameter's, adds exactly one problem to the gate over the full surface, and the problem names
// the probe and the tool. The probes with a rule word the first gate lacked kill a gate that goes back to the old word list; the probe
// with no rule word kills a gate that reads only sentences with a rule word.
func TestGateReportsEveryUnclassifiedProbeInALiveDescription(t *testing.T) {
	dropped := droppedIndex(t)
	surface := gateSurfaces()[0]
	_, base := gateSurfaceProblems(t, surface, dropped, nil)
	for _, probe := range unclassifiedProbes {
		for _, site := range []struct {
			name   string
			mutate func(*mcp.Tool) *mcp.Tool
		}{
			{"tool, after the opening sentence", func(tool *mcp.Tool) *mcp.Tool {
				if tool.Name != "offload_summarize" {
					return tool
				}
				return withToolDescription(tool, func(d string) string {
					sents := splitSentences(d)
					return strings.Join(append([]string{sents[0], probe}, sents[1:]...), " ")
				})
			}},
			{"tool, at the end", func(tool *mcp.Tool) *mcp.Tool {
				if tool.Name != "offload_vqa" {
					return tool
				}
				return withToolDescription(tool, func(d string) string { return addSentence(d, probe) })
			}},
			{"parameter, after the opening sentence", func(tool *mcp.Tool) *mcp.Tool {
				if tool.Name != "offload_transcribe" {
					return tool
				}
				return withParamDescription(t, tool, func(d string) string {
					sents := splitSentences(d)
					return strings.Join(append([]string{sents[0], probe}, sents[1:]...), " ")
				}, "route")
			}},
		} {
			_, got := gateSurfaceProblems(t, surface, dropped, site.mutate)
			if len(got) != len(base)+1 {
				t.Errorf("%q (%s): the gate reported %d problems, want %d (the probe is not reported exactly once)", probe, site.name, len(got), len(base)+1)
				continue
			}
			found := false
			for _, p := range got {
				if strings.Contains(p, probe) {
					found = true
				}
			}
			if !found {
				t.Errorf("%q (%s): no problem names the probe: %v", probe, site.name, got)
			}
		}
	}
}

// TestDroppedRowsNeedAReasonClassAndARuleWordsNeedAnExplicitReason: the dropped list is reviewable row by row. A row with no class or
// an undeclared class is a defect; a row whose sentence carries a rule word needs a reason of its own, and the probes with a rule
// word the first gate lacked prove the second check reads the words.
func TestDroppedRowsNeedAReasonClassAndARuleWordsNeedAnExplicitReason(t *testing.T) {
	for _, p := range checkDroppedRows(compactDropped) {
		t.Error(p)
	}
	if len(compactDropped) < 150 {
		t.Fatalf("only %d dropped rows: the list has been emptied", len(compactDropped))
	}
	const goodReason = "this sentence is safe to drop because the opening sentence and the schema already say it"
	rowFor := func(s, class, reason string) []droppedSentence {
		return []droppedSentence{{Tools: []string{"synthetic_tool"}, Sentence: s, Class: class, Reason: reason}}
	}
	for _, probe := range unclassifiedProbes {
		wantProblems := 0 // a rule word makes the missing or class-only reason a defect; a plain sentence needs no reason of its own
		if gateTrigger.MatchString(probe) {
			wantProblems = 1
		}
		if got := checkDroppedRows(rowFor(probe, dropDescr, "")); len(got) != wantProblems {
			t.Errorf("%q: a row with no explicit reason gave %v, want %d problem(s)", probe, got, wantProblems)
		}
		if got := checkDroppedRows(rowFor(probe, dropDescr, dropClasses[dropDescr])); len(got) != wantProblems {
			t.Errorf("%q: a row whose reason only restates its class gave %v, want %d problem(s)", probe, got, wantProblems)
		}
		if got := checkDroppedRows(rowFor(probe, dropDescr, goodReason)); len(got) != 0 {
			t.Errorf("%q: a row with an explicit reason was reported: %v", probe, got)
		}
		if got := checkDroppedRows(rowFor(probe, "", goodReason)); len(got) != 1 {
			t.Errorf("%q: a row with no class gave %v", probe, got)
		}
		if got := checkDroppedRows(rowFor(probe, "made-up", goodReason)); len(got) != 1 {
			t.Errorf("%q: a row with an undeclared class gave %v", probe, got)
		}
	}
	twice := append(rowFor("A plain sentence here.", dropDescr, ""), rowFor("A plain sentence here.", dropDescr, "")...)
	if got := checkDroppedRows(twice); len(got) != 1 {
		t.Errorf("a sentence listed twice for one tool gave %v", got)
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
	for _, w := range []string{"never", "refuse", "refused", "refuses", "only", "must", "not", "do not", "may not", "don't", "can't", "won't", "cannot", "required", "denied", "trusted", "untrusted", "unattended", "sandbox", "sandboxed", "safety", "advisory", "read-only", "forbidden", "irreversible", "destructive", "opt-in", "without", "no delete", "no shell", "no run", "no network", "no cloud", "no write", "NOT", "ONLY", "forward", "forwarded", "forwards", "forwarding", "travel", "travels", "travelled", "tailnet", "sent", "cloud", "fleet node", "fleet nodes", "defer", "defers", "deferred", "deferred:true", "deferring", "deferral", "deferrals", "always", "Always", "should", "avoid", "avoids", "avoided", "make sure", "Make sure", "prohibited", "prohibits", "prohibition", "ensure", "ensures", "ensured"} {
		probe := "A sentence that says " + w + " here."
		if !gateTrigger.MatchString(probe) {
			t.Errorf("the gate does not classify by %q", w)
		}
		if !leadGuard.MatchString(probe) {
			t.Errorf("the lead guard does not protect %q, which the gate classifies by", w)
		}
	}
}
