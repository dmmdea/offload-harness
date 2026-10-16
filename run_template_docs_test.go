package main

// How ComfyUI finds an embedding, and what the template-run checks cannot see.
//
// ComfyUI's embedding lookup is not "each directory of the class in its order". expand_directory_list (comfy/sd1_clip.py) puts
// every embeddings root and every subdirectory below it into one Python set(), and load_embed takes the first directory of
// list(set) that holds the name, so which directory answers is a matter of hash order and differs from one process to the next.
// The first wording of the docs and of the catalog's comments said "each directory of the class in its order ... the first file
// found" and "no base-name match (the file ComfyUI opens is the one at that path)": both are false against ComfyUI 0.38.0, and
// the tests that pinned them pinned the false premise. This refuses that wording where the run-template prose lives, and requires
// the true statement beside it.
//
// The second half is a limit that was stated nowhere while "an override never retargets a file or names an embedding" was said
// without limit on eight surfaces: both the override check and the runner's comparison read the string VALUES of the graph, so an
// embedding that string nodes assemble while the graph runs (a concatenation of two overrides, say) is neither refused nor compared
// with the manifest. The fragment, the MCP server page and the manifest-satisfaction flow state it.
//
// A fragment is changelog text from the day it is written, so CHANGELOG.md is read with its fragments spliced in (docText).

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	embeddingSetOrderMarker = "into a Python `set`"
	embeddingConcatMarker   = "assembled while the graph runs by string nodes"
)

func TestTheRunTemplateDocsStateHowComfyUIFindsAnEmbeddingAndWhatTheChecksCannotSee(t *testing.T) {
	for _, problem := range embeddingDocProblems(".") {
		t.Error(problem)
	}
}

// embeddingDocProblems is that check over the files under root, as a list, so a test can run it on a root of its own.
func embeddingDocProblems(root string) []string {
	retired := map[string][]string{
		"CHANGELOG.md":                                  {"each directory of the class in its order", "no base-name match", "the file ComfyUI opens is the one at that path"},
		"docs/systems/mcp-server.md":                    {"each directory of the class in its order", "no base-name match"},
		"docs/flows/run-graph-manifest-satisfaction.md": {"each directory of the class in its order", "no base-name match"},
		"render/templates-catalog.mjs":                  {"each directory of the class in turn", "the one at that path", "loads the first file it finds"},
		"render/template-run.mjs":                       {"each directory of the class in its order", "no base-name match"},
	}
	required := map[string][]string{
		"CHANGELOG.md":                                  {embeddingSetOrderMarker, embeddingConcatMarker},
		"docs/systems/mcp-server.md":                    {embeddingSetOrderMarker, embeddingConcatMarker},
		"docs/flows/run-graph-manifest-satisfaction.md": {embeddingConcatMarker},
	}
	var problems []string
	read := func(name string) (string, bool) {
		raw, err := docText(root, name)
		if err != nil {
			problems = append(problems, err.Error())
			return "", false
		}
		// A quote wraps across lines, and a code comment carries its // markers across the wrap.
		return strings.Join(strings.Fields(strings.ReplaceAll(raw, "//", " ")), " "), true
	}
	for _, name := range embeddingDocFiles(retired) {
		text, ok := read(name)
		if !ok {
			continue
		}
		for _, phrase := range retired[name] {
			if strings.Contains(text, phrase) {
				problems = append(problems, fmt.Sprintf("%s still says %q: ComfyUI 0.38.0 searches the embeddings roots and every subfolder through a set, so the directory that answers is chosen by hash order, and a file in a subfolder answers a bare name", filepath.ToSlash(name), phrase))
			}
		}
	}
	for _, name := range embeddingDocFiles(required) {
		text, ok := read(name)
		if !ok {
			continue
		}
		for _, phrase := range required[name] {
			if !strings.Contains(text, phrase) {
				problems = append(problems, fmt.Sprintf("%s does not say %q: it must state how ComfyUI finds an embedding (a set of directories, picked by hash order) and the limit that an embedding assembled by string nodes while the graph runs is not seen", filepath.ToSlash(name), phrase))
			}
		}
	}
	return problems
}

func embeddingDocFiles(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The check reads the tree it is given: a tree that says the retired thing and lacks the limit is refused, one that says the true thing is clean.
func TestTheEmbeddingDocCheckSeesBothHalves(t *testing.T) {
	files := func(setOrder, concat, retired string) map[string]string {
		prose := "Embeddings are looked up " + setOrder + ". " + concat + ". " + retired + "\n"
		return map[string]string{
			"CHANGELOG.md":                                  "# Changelog\n\n## [Unreleased]\n\n## [0.1.0] - 2026-01-01 - x\n\n- " + prose,
			"docs/systems/mcp-server.md":                    prose,
			"docs/flows/run-graph-manifest-satisfaction.md": prose,
			"render/templates-catalog.mjs":                  "// nothing\n",
			"render/template-run.mjs":                       "// nothing\n",
		}
	}
	clean := t.TempDir()
	writeFixtureTree(t, clean, files("by putting every directory "+embeddingSetOrderMarker, "An embedding "+embeddingConcatMarker+" is not seen", "Nothing else."))
	if p := embeddingDocProblems(clean); len(p) != 0 {
		t.Fatalf("the control tree is not clean: %v", p)
	}
	bad := t.TempDir()
	writeFixtureTree(t, bad, files("in each directory", "Nothing about strings", "It tries each directory of the class in its order, the first file found."))
	got := strings.Join(embeddingDocProblems(bad), "\n")
	for _, want := range []string{"still says \"each directory of the class in its order\"", "does not say \"" + embeddingSetOrderMarker + "\"", "does not say \"" + embeddingConcatMarker + "\""} {
		if !strings.Contains(got, want) {
			t.Errorf("the check missed %s:\n%s", want, got)
		}
	}
}
