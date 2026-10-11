package pipeline

// The result envelope of a run_graph call is the runner's note to that one call. When the caller names none the harness names one
// for itself, reads it once, and has no further use for it, so it must not outlive the run. A name made of the graph's path left one
// file in the media dir for every call that hands a graph file of its own, and three callers do exactly that: a graph ticket (its
// copy of the graph is body-<id>-graph.json, new for every ticket), the MCP door (an inline graph_json becomes a new temp file on
// every call) and a fleet node (a temp file for every job). The operator is strict about leftover files: none may grow with the
// number of calls.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// envelopeRunnerSrc stands in for comfy-run-graph.mjs: it writes the result envelope it is asked for (--result) and ends the way
// ENVELOPE_MODE says. ok: an envelope with outputs and exit 0. defer: a typed defer in the envelope and exit 0 (the runner's way of
// handing a handled failure back as data). crash: the envelope written, then a non-zero exit, as a runner that fails after its note.
const envelopeRunnerSrc = `import {writeFileSync} from "node:fs";
const ri = process.argv.indexOf("--result");
const mode = process.env.ENVELOPE_MODE || "ok";
if (ri > 0) {
  const body = mode === "defer" ? {deferred: true, code: "MODEL_MISSING", ref: "x", detail: "a typed defer"} : {outputs: {}, image_path: ""};
  writeFileSync(process.argv[ri + 1], JSON.stringify(body));
}
if (mode === "crash") process.exit(3);
`

// envelopeFixture is a pipeline whose run-graph route is the stand-in above.
func envelopeFixture(t *testing.T, mode string) *admitFixture {
	t.Helper()
	f := newAdmitFixture(t, nil)
	script := filepath.Join(f.dir, "envelope-runner.mjs")
	if err := os.WriteFile(script, []byte(envelopeRunnerSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	f.p.cfg.RunGraphScript = script
	t.Setenv("ENVELOPE_MODE", mode)
	return f
}

// graphFileOf writes a graph file of its own in dir, as a ticket's copy or an inline graph's temp file is.
func graphFileOf(t *testing.T, dir string, n int) string {
	t.Helper()
	gp := filepath.Join(dir, fmt.Sprintf("body-mq-%012d-graph.json", n))
	if err := os.WriteFile(gp, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return gp
}

// envelopesIn lists the run-graph result envelopes in dir.
func envelopesIn(t *testing.T, dir string) []string {
	t.Helper()
	got, err := filepath.Glob(filepath.Join(dir, "run-graph-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Three runs, each handed a graph file of its own, leave no envelope in the media dir: whether the run succeeded, ended in a typed
// defer, or failed after it had written its note.
func TestARunGraphLeavesNoEnvelopeBehind(t *testing.T) {
	for _, mode := range []string{"ok", "defer", "crash"} {
		t.Run(mode, func(t *testing.T) {
			f := envelopeFixture(t, mode)
			copies := t.TempDir() // where a ticket keeps its copies
			for i := 0; i < 3; i++ {
				res := f.await(f.start(core.TaskRunGraph, "", map[string]any{"graph_path": graphFileOf(t, copies, i), "out_dir": f.dir}))
				if mode == "ok" && !res.OK {
					t.Fatalf("control: the run succeeds: %+v", res)
				}
				if mode != "ok" && (res.OK || !res.Deferred) {
					t.Fatalf("control: a %s run is deferred to the caller: %+v", mode, res)
				}
			}
			if left := envelopesIn(t, f.dir); len(left) != 0 {
				t.Errorf("3 runs left %d result envelopes in the media dir: %v", len(left), left)
			}
		})
	}
}

// A caller that names result_path owns that file: the run writes it there and keeps it, and the media dir gets none of its own.
func TestARunGraphKeepsTheEnvelopeACallerNamed(t *testing.T) {
	f := envelopeFixture(t, "ok")
	mine := filepath.Join(t.TempDir(), "mine.json")
	res := f.await(f.start(core.TaskRunGraph, "", map[string]any{"graph_path": graphFileOf(t, t.TempDir(), 1), "out_dir": f.dir, "result_path": mine}))
	if !res.OK {
		t.Fatalf("control: the run succeeds: %+v", res)
	}
	if b, err := os.ReadFile(mine); err != nil || len(b) == 0 {
		t.Errorf("the envelope the caller named is written there and kept: %q %v", b, err)
	}
	if left := envelopesIn(t, f.dir); len(left) != 0 {
		t.Errorf("a caller that names its envelope leaves none in the media dir: %v", left)
	}
}

// The envelope of a call is that call's alone: no two runs get the same name, so the removal of one run's note can never take
// another's (runs that declare disjoint devices can be in flight on one node together).
func TestRunGraphEnvelopeNamesAreOneToACall(t *testing.T) {
	dir := t.TempDir()
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		p := runGraphEnvelopePath(dir)
		if filepath.Dir(p) != dir || !strings.HasPrefix(filepath.Base(p), "run-graph-") || !strings.HasSuffix(p, ".json") {
			t.Fatalf("an envelope is a run-graph-*.json in the media dir: %s", p)
		}
		if seen[p] {
			t.Fatalf("two calls were given the same envelope %s", p)
		}
		seen[p] = true
	}
}
