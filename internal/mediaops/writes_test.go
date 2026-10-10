package mediaops

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every place this package writes a file, or starts a child that does, is accounted for. The failure
// class this guards is one idiom repeated wherever an output is made: hand the destination path to a
// tool (or to os.WriteFile) and let it truncate the target before it writes a byte, so a full drive
// leaves a zero-byte file that every exists() check reads as finished (2026-10-09; the render helpers
// have the same list in render/output-writers.test.mjs). A new write that is not listed fails here:
// if it delivers an output, route it through deliverFile / deliverFrames (deliver.go); if it does not,
// list it below with the reason. Comments are not code and are not counted.

var writePrimitives = []string{
	"os.WriteFile(", "os.Create(", "os.CreateTemp(", "os.OpenFile(", "os.Mkdir(", "os.MkdirAll(", "os.MkdirTemp(",
	"os.Rename(", "renameFile(", "io.Copy(", ".WriteString(", "ioutil.", "runCapture(", "exec.Command(",
}

var allowedWrites = map[string]struct {
	counts map[string]int
	why    string
}{
	"deliver.go": {map[string]int{"os.Mkdir(": 1, "renameFile(": 1},
		"the staging directory of an extract_frames run (inside the destination, removed on every path out) and the one rename that delivers a staged file or frame"},
	"exec.go": {map[string]int{"runCapture(": 1, "exec.Command(": 1},
		"runCapture's own definition (the one place a child is started) and the taskkill that ends a timed-out process tree"},
	"gimp.go": {map[string]int{"os.CreateTemp(": 1, ".WriteString(": 1},
		"the .scm batch script GIMP loads (WriteScriptFile): an input of the engine, in the temp directory"},
	"run.go": {map[string]int{"os.CreateTemp(": 1, "os.MkdirAll(": 1, "os.MkdirTemp(": 1, ".WriteString(": 1, "runCapture(": 4},
		"concat's list file (an input); extract_frames' destination directory (created, never written into by the engine); GIMP's private work directory " +
			"(its raster is read by the PIL worker and removed, never delivered); and the four engine runs: probe (writes nothing), GIMP (the private raster), " +
			"the PIL worker (delivers through render/atomic_out.py) and the ffmpeg op (staged: deliverFile / deliverFrames)"},
}

func stripGoComments(src string) string {
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, "")
	return regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(src, "")
}

func TestEveryDirectWriteInMediaopsIsAccountedFor(t *testing.T) {
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var drift []string
	seen := map[string]bool{}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		src := stripGoComments(string(raw))
		found := map[string]int{}
		for _, p := range writePrimitives {
			if n := strings.Count(src, p); n > 0 {
				found[p] = n
			}
		}
		seen[name] = true
		want := allowedWrites[name].counts
		if fmt.Sprint(sortedCounts(found)) != fmt.Sprint(sortedCounts(want)) {
			drift = append(drift, fmt.Sprintf("%s: found %v, listed %v", name, sortedCounts(found), sortedCounts(want)))
		}
	}
	for name := range allowedWrites {
		if !seen[name] {
			drift = append(drift, name+": listed but the file is gone")
		}
	}
	if len(drift) > 0 {
		t.Fatalf("a write that is not listed (or a listed one that moved):\n  %s\n"+
			"If it delivers an output (a clip, a frame, an image a caller reads), route it through deliverFile / deliverFrames in deliver.go: "+
			"a tool handed the destination truncates it first, so a full drive leaves a 0-byte file that looks finished.\n"+
			"If it is not an output, list it in allowedWrites with the reason.", strings.Join(drift, "\n  "))
	}
}

func sortedCounts(m map[string]int) []string {
	var out []string
	for k, v := range m {
		out = append(out, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(out)
	return out
}
