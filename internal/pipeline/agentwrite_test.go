// The delegation WRITE door (register D-06), node side. Every test here is
// about a refusal or about what the caller GETS BACK — never about the harness
// applying anything, because it never does.
package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/writedoor"
)

// writeCallChat is an assistant turn calling write_file with the given path and
// content — the shape a seat that actually uses the door produces.
func writeCallChat(id, path, content string) string {
	args, _ := json.Marshal(map[string]string{"path": path, "content": content})
	inner, _ := json.Marshal(string(args))
	return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"` + id +
		`","type":"function","function":{"name":"write_file","arguments":` + string(inner) +
		`}}]},"finish_reason":"tool_calls"}]}`
}

// editCallChat is an assistant turn calling edit_file.
func editCallChat(id, path, oldStr, newStr string) string {
	args, _ := json.Marshal(map[string]string{"path": path, "old_string": oldStr, "new_string": newStr})
	inner, _ := json.Marshal(string(args))
	return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"` + id +
		`","type":"function","function":{"name":"edit_file","arguments":` + string(inner) +
		`}}]},"finish_reason":"tool_calls"}]}`
}

// writeDoorPipeline is agentTestPipeline with the node's write opt-in set.
func writeDoorPipeline(t *testing.T, base string, allowWrite bool) *Pipeline {
	t.Helper()
	cfg := config.Config{
		Endpoint:        base,
		Model:           "workhorse",
		AgentModel:      agentTestSeat,
		FleetNodeID:     "node-t",
		Temperature:     0.1,
		AgentAllowWrite: allowWrite,
	}
	return New(cfg, llamaclient.New(base, "", cfg.Model, 30*time.Second), nil, nil)
}

func writeContract(root string) core.AgentContract {
	c := testContract()
	c.Goal = "fix the off-by-one in util.go and add a test case"
	c.Context = []core.ContextDoc{{Name: "util.go", Text: "package util\n\nfunc Last(n int) int {\n\treturn n\n}\n"}}
	c.WriteRoot = root
	c.MaxSteps = 6
	return c
}

// --- The config gate -------------------------------------------------------

// TestWriteDoorRefusedWhenTheNodeHasNotOptedIn is the DEFAULT-OFF guarantee: a
// node whose agent_allow_write is false must refuse a write contract outright,
// by class, with no diff and no seat call — not run it read-only and hand back
// a green result whose write set is silently empty.
func TestWriteDoorRefusedWhenTheNodeHasNotOptedIn(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(int64) string {
			t.Error("the seat was called on a node that has not opted into the write door")
			return doneChat("x")
		},
		repack: func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	res := writeDoorPipeline(t, srv.URL, false).Run(context.Background(), agentTestRequest(t, writeContract(".")))
	wire := decodeWire(t, res)

	if !wire.Deferred {
		t.Fatal("a write contract on a node without agent_allow_write must DEFER, not run")
	}
	if wire.DeferClass != core.DeferClassWrite {
		t.Errorf("defer_class = %q, want %q — the class is what tells a delegator to re-place rather than to page an operator", wire.DeferClass, core.DeferClassWrite)
	}
	if !strings.Contains(wire.Reason, "agent_allow_write") {
		t.Errorf("reason %q does not name the key an operator must set", wire.Reason)
	}
	if wire.Diff != "" || len(wire.DiffFiles) != 0 {
		t.Errorf("a refused door published a write set: diff=%q files=%v", wire.Diff, wire.DiffFiles)
	}
}

// TestReadOnlyContractIsUnchangedByTheDoor: a contract with no write_root must
// build the read-only loop it always did, on an opted-in node as much as on any
// other — and publish no write fields at all, so a reader can tell "no door"
// from "door open, nothing written".
func TestReadOnlyContractIsUnchangedByTheDoor(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, writeDoorPipeline(t, srv.URL, true).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if wire.Diff != "" || len(wire.DiffFiles) != 0 || wire.WriteNote != "" {
		t.Errorf("a read-only contract published write-door fields: diff=%q files=%v note=%q", wire.Diff, wire.DiffFiles, wire.WriteNote)
	}
}

// --- What the caller gets back ---------------------------------------------

// TestWriteDoorReturnsAnAppliableDiffAndTouchedPaths is the door working: the
// seat edits the materialized copy, and the CALLER gets a unified diff naming
// exactly what changed. The diff is the deliverable; nothing is applied.
func TestWriteDoorReturnsAnAppliableDiffAndTouchedPaths(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(n int64) string {
			switch n {
			case 1:
				return editCallChat("e1", "util.go", "\treturn n\n", "\treturn n - 1\n")
			case 2:
				return writeCallChat("w1", "util_test.go", "package util\n\nfunc TestLast(t *testing.T) {}\n")
			default:
				return doneChat("Fixed the off-by-one and added a test.")
			}
		},
		repack: func(int64) string { return `{"answer":"fixed"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, writeDoorPipeline(t, srv.URL, true).Run(context.Background(), agentTestRequest(t, writeContract("."))))
	if wire.Deferred {
		t.Fatalf("deferred: %s (note %q)", wire.Reason, wire.WriteNote)
	}
	want := []string{"util.go", "util_test.go"}
	if len(wire.DiffFiles) != len(want) {
		t.Fatalf("diff_files = %v, want %v", wire.DiffFiles, want)
	}
	for i, w := range want {
		if wire.DiffFiles[i] != w {
			t.Errorf("diff_files[%d] = %q, want %q (sorted, slash-separated)", i, wire.DiffFiles[i], w)
		}
	}
	for _, frag := range []string{"diff --git a/util.go b/util.go", "-\treturn n", "+\treturn n - 1", "new file mode 100644"} {
		if !strings.Contains(wire.Diff, frag) {
			t.Errorf("diff is missing %q:\n%s", frag, wire.Diff)
		}
	}
	if !strings.Contains(wire.WriteNote, "NOT applied") {
		t.Errorf("write_note %q must say the harness did not apply the patch — that is the door's whole safety story", wire.WriteNote)
	}
}

// TestWriteDoorReportsAnEmptyWriteSet: a seat that talks instead of writing is
// the single most likely 4B failure. It must be legible as exactly that, not as
// an absent door.
func TestWriteDoorReportsAnEmptyWriteSet(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("I would change util.go to subtract one.") },
		repack:    func(int64) string { return `{"answer":"described"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, writeDoorPipeline(t, srv.URL, true).Run(context.Background(), agentTestRequest(t, writeContract("."))))
	if wire.Diff != "" || len(wire.DiffFiles) != 0 {
		t.Fatalf("nothing was written but a write set was published: %v", wire.DiffFiles)
	}
	if !strings.Contains(wire.WriteNote, "wrote nothing") {
		t.Errorf("write_note = %q, want it to say the seat wrote nothing", wire.WriteNote)
	}
}

// --- Confinement (cross-platform half; the OS-specific escapes live in the
// _linux and _windows files beside this one) --------------------------------

// TestOpenWriteDoorRefusesEveryEscapingRoot: the shapes whose damage is done
// before os.Root can see them — the caller believes it scoped the write to one
// directory and it did not.
func TestOpenWriteDoorRefusesEveryEscapingRoot(t *testing.T) {
	read := t.TempDir()
	roots := []string{"..", "../sibling", "sub/../../out", "/etc", "C:/Windows", "sub/.git", "sub/nul", "sub/trailing ", "sub/trailing."}
	for _, root := range roots {
		t.Run(root, func(t *testing.T) {
			c := writeContract(root)
			if verr := c.Validate(); verr != nil {
				return // refused at the contract, which is the earliest door
			}
			if _, derr := openWriteDoor(read, c); derr == nil {
				t.Fatalf("write_root %q was accepted; it must be refused at the contract or at the door", root)
			}
		})
	}
}

// TestWriteToolsCannotLeaveTheWriteRoot proves the confinement that matters at
// run time: the tools are rooted at write_root, so a path climbing out of it is
// refused and the file outside is untouched.
func TestWriteToolsCannotLeaveTheWriteRoot(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(n int64) string {
			if n == 1 {
				return writeCallChat("w1", "../escaped.txt", "this must never land")
			}
			return doneChat("done")
		},
		repack: func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	req := agentTestRequest(t, writeContract("work"))
	ctxDir := req.Params["context_dir"].(string)
	wire := decodeWire(t, writeDoorPipeline(t, srv.URL, true).Run(context.Background(), req))

	if _, err := os.Stat(filepath.Join(ctxDir, "escaped.txt")); err == nil {
		t.Fatal("a write climbed OUT of write_root and landed in the read root")
	}
	if len(wire.DiffFiles) != 0 {
		t.Errorf("the escaping write appears in the write set: %v", wire.DiffFiles)
	}
}

// --- Caps ------------------------------------------------------------------

// TestCloseWriteDoorRefusesAnOversizeWriteSetWithoutPublishingADiff: the
// post-run half of the cap. No diff is published — a truncated or partial patch
// applies as silent damage, which is worse than no patch at all.
func TestCloseWriteDoorRefusesAnOversizeWriteSetWithoutPublishingADiff(t *testing.T) {
	read := t.TempDir()
	door, err := openWriteDoor(read, writeContract("."))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= core.AgentWriteMaxFiles; i++ {
		name := "f" + string(rune('a'+i)) + ".txt"
		if werr := os.WriteFile(filepath.Join(door.root, name), []byte("x\n"), 0o644); werr != nil {
			t.Fatal(werr)
		}
	}
	diff, files, _, cerr := closeWriteDoor(door)
	if cerr == nil {
		t.Fatalf("a %d-file write set passed the %d-file cap", core.AgentWriteMaxFiles+1, core.AgentWriteMaxFiles)
	}
	if diff != "" || len(files) != 0 {
		t.Errorf("a capped-out door published a write set anyway: %d files, %d bytes of diff", len(files), len(diff))
	}
}

// TestCloseWriteDoorRefusesAnOversizeDiff: the byte cap, tested at the diff
// rather than at the write — one file can hold an unreviewable patch.
func TestCloseWriteDoorRefusesAnOversizeDiff(t *testing.T) {
	read := t.TempDir()
	door, err := openWriteDoor(read, writeContract("."))
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("a line of text that is long enough to add up\n", core.AgentWriteDiffMaxBytes/40)
	if werr := os.WriteFile(filepath.Join(door.root, "big.txt"), []byte(big), 0o644); werr != nil {
		t.Fatal(werr)
	}
	diff, files, _, cerr := closeWriteDoor(door)
	if cerr == nil {
		t.Fatalf("a %d-byte write rendered a diff inside the %d-byte cap", len(big), core.AgentWriteDiffMaxBytes)
	}
	if diff != "" || len(files) != 0 {
		t.Errorf("a capped-out door published a write set anyway: %d files, %d bytes of diff", len(files), len(diff))
	}
}

// TestWriteDoorSnapshotSeesAnUnaskedWrite is the property the cap rests on: the
// write set is taken from the TREE, so a file the seat wrote without being
// asked to is in the diff too.
func TestWriteDoorSnapshotSeesAnUnaskedWrite(t *testing.T) {
	read := t.TempDir()
	door, err := openWriteDoor(read, writeContract("."))
	if err != nil {
		t.Fatal(err)
	}
	if werr := os.WriteFile(filepath.Join(door.root, "unasked.txt"), []byte("surprise\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	_, files, _, cerr := closeWriteDoor(door)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if len(files) != 1 || files[0] != "unasked.txt" {
		t.Fatalf("write set = %v, want [unasked.txt] — a tree snapshot must see a write nobody asked for", files)
	}
	if _, serr := writedoor.Take(door.root); serr != nil {
		t.Fatal(serr)
	}
}

// --- shared helper for the OS-specific confinement tests -------------------

// doorTools is the write tool set as the door builds it, exposed to the
// platform-tagged tests beside this file so each OS proves the confinement
// against its own escape shape (a Linux symlink, a Windows junction) through
// the SAME tools a real run uses.
type doorTools struct{ tools []agent.Tool }

func buildWriteToolsForTest(d *writeDoor) (doorTools, error) {
	pol := agent.NewPolicy(true, nil).WithWritePosture(true, false)
	ts, err := agent.WriteToolsLimited(d.root, pol, d.limit)
	return doorTools{tools: ts}, err
}

// write invokes write_file exactly as the loop would.
func (dt doorTools) write(t *testing.T, args string) (string, error) {
	t.Helper()
	for _, tool := range dt.tools {
		if tool.Name == "write_file" {
			return tool.Exec(context.Background(), args)
		}
	}
	t.Fatal("write_file is not in the door's tool set")
	return "", nil
}
