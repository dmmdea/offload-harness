package mcpserver

import (
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
)

// liveSentences indexes every sentence of every description (a tool's own and each parameter's, at any depth) of the full
// surfaces by tool: sentence -> true when it occurs somewhere OTHER than an opening position (the only place the keep-list and
// the dropped list act), false when it only ever opens a description.
func liveSentences(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	for _, cfg := range []func(string) config.Config{fullSurfaceCfg, fleetSurfaceCfg} {
		for _, tool := range listTools(t, cfg("full")) {
			if out[tool.Name] == nil {
				out[tool.Name] = map[string]bool{}
			}
			for _, desc := range descriptionsByPath(tool) {
				sents := splitSentences(desc)
				lead := gateLeadCount(sents)
				for i, s := range sents {
					out[tool.Name][s] = out[tool.Name][s] || i >= lead
				}
			}
		}
	}
	return out
}

// TestKeepListEntriesAreLive: every keep entry names tools that exist and a sentence (and referents) that a live full description
// has, outside an opening position. A reworded description, a renamed tool or a sentence that has become an opening one leaves a
// dead entry, and a dead entry is a red test, never a silent no-op.
func TestKeepListEntriesAreLive(t *testing.T) {
	live := liveSentences(t)
	seen := map[string]bool{}
	for i, e := range compactKeep {
		if len(e.Tools) == 0 || strings.TrimSpace(e.Sentence) == "" || strings.TrimSpace(e.Why) == "" {
			t.Errorf("keep entry %d needs tools, a sentence and a reason: %+v", i, e)
			continue
		}
		for _, tool := range e.Tools {
			sents, ok := live[tool]
			if !ok {
				t.Errorf("keep entry %d (%.60q): %s is not a tool of the full surface", i, e.Sentence, tool)
				continue
			}
			if key := tool + "\x00" + e.Sentence; seen[key] {
				t.Errorf("keep entry %d: %s keeps %.60q twice", i, tool, e.Sentence)
			} else {
				seen[key] = true
			}
			if !sents[e.Sentence] {
				t.Errorf("keep entry %d: %s has no such sentence outside an opening position (reworded?): %.100q", i, tool, e.Sentence)
			}
			for _, w := range e.With {
				if w == e.Sentence {
					t.Errorf("keep entry %d lists its own sentence as its referent", i)
				}
				if _, ok := sents[w]; !ok {
					t.Errorf("keep entry %d: %s has no referent sentence %.100q", i, tool, w)
				}
			}
		}
	}
	if len(compactKeep) < 40 {
		t.Fatalf("only %d keep entries: the list has been emptied", len(compactKeep))
	}
}

// TestDroppedEntriesAreLive: every dropped-with-reason entry is a constraint sentence a live description still has, in the tool
// it names. A stale entry would let a reworded rule (a different sentence) in under an old reason.
func TestDroppedEntriesAreLive(t *testing.T) {
	live := liveSentences(t)
	for i, d := range compactDropped {
		if !gateTrigger.MatchString(d.Sentence) {
			t.Errorf("dropped entry %d carries no constraint word, so it needs no entry: %.100q", i, d.Sentence)
		}
		for _, tool := range d.Tools {
			sents, ok := live[tool]
			if !ok {
				t.Errorf("dropped entry %d: %s is not a tool of the full surface", i, tool)
				continue
			}
			if !sents[d.Sentence] {
				t.Errorf("dropped entry %d: %s has no such sentence outside an opening position (reworded or removed?): %.100q", i, tool, d.Sentence)
			}
		}
	}
}

// TestKeepAndDroppedListsAreDisjoint: a sentence cannot be both kept and dropped in one tool.
func TestKeepAndDroppedListsAreDisjoint(t *testing.T) {
	dropped := droppedIndex(t)
	for i, e := range compactKeep {
		for _, tool := range e.Tools {
			if reason, both := dropped[tool][e.Sentence]; both {
				t.Errorf("keep entry %d: %s both keeps and drops (%q) %.80q", i, tool, reason, e.Sentence)
			}
		}
	}
}

// TestKeepListEntriesSurviveWhereverTheyAppear: a kept sentence (and its referents) is in the compact description at every place
// of the tool where the full description has it, whether or not it carries a constraint word.
func TestKeepListEntriesSurviveWhereverTheyAppear(t *testing.T) {
	compact := map[string]*mcp.Tool{}
	for _, tool := range listTools(t, fullSurfaceCfg("compact")) {
		compact[tool.Name] = tool
	}
	checked := 0
	for _, ft := range listTools(t, fullSurfaceCfg("full")) {
		fd, cd := descriptionsByPath(ft), descriptionsByPath(compact[ft.Name])
		paths := make([]string, 0, len(fd))
		for p := range fd {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, path := range paths {
			sents := splitSentences(fd[path])
			lead := gateLeadCount(sents)
			for i, s := range sents {
				if i < lead || !compactKeepByTool[ft.Name][s] {
					continue
				}
				checked++
				if !strings.Contains(cd[path], s) {
					t.Errorf("%s %s: compact dropped the kept sentence %.100q", ft.Name, path, s)
				}
			}
		}
	}
	if checked < 40 {
		t.Fatalf("only %d kept sentences were found in the compact list: the check is blind", checked)
	}
}
