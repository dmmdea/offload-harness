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

// gateTrigger marks a sentence as one that may carry a constraint: a prohibition, an obligation, an inability, a trust boundary, a
// restriction or an opt-in. It is a deliberate superset of the words the compact profile was first reviewed against (never, refuse,
// only, must, do not, cannot, required): it adds every contraction ("can't", "won't"), plain "not" (which also covers "do not",
// "may not" and the upper-case "NOT"), "without", "opt-in" and the words the first lexical list kept (trusted, untrusted,
// unattended, sandbox, safety, advisory, read-only, forbidden, irreversible, destructive, denied), so nothing that list kept and
// nothing a review found it missing can go unclassified. It is written out here on purpose and is NOT the production lead-cut guard
// (leadGuard in toolsprofile.go): narrowing production's regexp cannot narrow this gate.
var gateTrigger = regexp.MustCompile(`(?i)\b(never|refus\w*|only|must|not|\w+n't|cannot|required|denied|trusted|untrusted|unattended|sandbox\w*|safety|advisory|read-only|forbidden|irreversible|destructive|opt-in|without|no (?:delete|shell|run|network|cloud|write))\b`)

// droppedSentence records a constraint-word sentence the compact profile deliberately drops, in one tool, and why that is safe.
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

// TestEveryConstraintSentenceIsKeptOrDroppedWithReason is the review gate of the compact profile. It reads the LIVE full
// descriptions (every tool of the full surface, and each parameter at any depth), cuts them into sentences with the production
// splitter, and for every sentence that carries a constraint word (gateTrigger) outside the opening sentence(s) it requires one of
// two things: the sentence is in the COMPACT description of that same tool and parameter, verbatim, or it is on the dropped list
// (toolsprofile_dropped_test.go) with a reason. A description that gains a new rule therefore turns this test red until someone
// has read the sentence and put it on one list or the other. The check reads the compact OUTPUT, so a keep-list entry that does
// not match what the splitter produces is caught too.
//
// An opening sentence is kept by rule, and the cut may shorten it at a clause boundary only when the part it drops carries no
// constraint word: that half is checked below with gateTrigger, independently of the production guard.
func TestEveryConstraintSentenceIsKeptOrDroppedWithReason(t *testing.T) {
	dropped := droppedIndex(t)
	var kept, droppedSeen, leadsChecked, leadsCut int
	var unclassified []string
	for _, surface := range []struct {
		name string
		cfg  func(string) config.Config
	}{{"full surface", fullSurfaceCfg}, {"fleet accelerators", fleetSurfaceCfg}} {
		compact := map[string]*mcp.Tool{}
		for _, tool := range listTools(t, surface.cfg("compact")) {
			compact[tool.Name] = tool
		}
		for _, ft := range listTools(t, surface.cfg("full")) {
			ct, ok := compact[ft.Name]
			if !ok {
				t.Errorf("%s: %s is in the full list and not in the compact one", surface.name, ft.Name)
				continue
			}
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
						leadsChecked++
						if strings.Contains(cd[path], s) {
							continue
						}
						leadsCut++
						tail := s[commonPrefixLen(s, cd[path]):]
						if gateTrigger.MatchString(tail) {
							t.Errorf("%s %s: the opening sentence was cut and the dropped tail carries a constraint word: %q", ft.Name, path, tail)
						}
						continue
					}
					if i < lead {
						continue
					}
					switch _, isDropped := dropped[ft.Name][s]; {
					case strings.Contains(cd[path], s):
						kept++
					case isDropped:
						droppedSeen++
					default:
						unclassified = append(unclassified, fmt.Sprintf("%s %s sentence %d: %s", ft.Name, path, i, s))
					}
				}
			}
		}
	}
	for _, u := range unclassified {
		t.Errorf("a constraint sentence is neither in the compact description nor on the dropped list: %s", u)
	}
	if kept < 40 || droppedSeen < 40 || leadsChecked < 40 {
		t.Fatalf("the gate saw %d kept, %d dropped and %d opening sentences with a constraint word: it is blind (leads cut: %d)", kept, droppedSeen, leadsChecked, leadsCut)
	}
}
