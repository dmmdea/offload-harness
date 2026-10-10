package ledger

// F22 (2026-10-09): some deferred ledger rows carried no reason_code (the review lane's local "gpu busy"
// at 08:06, the older rows of 10-08; on one node every deferred row, on another 21 of 100). The code was
// settled for task agent_delegate alone, so every other door's defer was a free-text reason and, on the
// media doors, an err_class. Record is the one path every writer takes, so it settles the code of every
// deferred row; these tests pin that for the shape each writer produces, and walk the writers.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// deferShapes are rows shaped like what each writer of a deferred row produces today: the pipeline's
// recordDefer for each door, the review lane's local defer, the media doors' err_classes, RecordAccel, the
// remote lane's row (internal/pairworkloads/remote.go) and a fleet node's executor row.
var deferShapes = []struct {
	name string
	row  Entry
	want string
}{
	{"transcribe, no audio stream (F21)", Entry{Task: "transcribe", ErrClass: "bad_input", Reason: "the input has no audio stream"}, ReasonBadInput},
	{"transcribe, audio convert failed", Entry{Task: "transcribe", Reason: "audio convert: audioio: ffmpeg failed: exit status 183 (Invalid data)"}, ReasonBadInput},
	{"transcribe, card held", Entry{Task: "transcribe", ErrClass: "gpu_busy", Reason: "gpu busy: generation job holds the lock (...)"}, ReasonNodeBusy},
	{"transcribe, no model", Entry{Task: "transcribe", Reason: "no stt model configured"}, ReasonConfig},
	{"transcribe, no speech", Entry{Task: "transcribe", Reason: "empty transcript (no speech detected)"}, ReasonAbstention},
	{"transcribe, call failed", Entry{Task: "transcribe", ErrClass: "timeout", Reason: "transcribe call failed: context deadline exceeded"}, ReasonInfrastructure},
	{"the review lane's local defer (08:06)", Entry{Task: "agent", Door: "offload_review_diff", Reason: "gpu busy: media render holds the cards; no new run is admitted on this box ..."}, ReasonNodeBusy},
	{"a fleet node's executor row", Entry{Task: "agent", Door: "fleet", Reason: "seat busy: the seat is at its run cap"}, ReasonNodeBusy},
	{"image, left a place in line", Entry{Task: "generate_image", ErrClass: "gpu_queued", Reason: "gpu queued: card(s) held by media; your place in line is #1"}, ReasonNodeBusy},
	{"image, card table unreadable", Entry{Task: "generate_image", ErrClass: "gpu_lease_unavailable", Reason: "the card table: nvidia-smi: context deadline exceeded"}, ReasonInfrastructure},
	{"image, no route", Entry{Task: "generate_image", Reason: "no image-gen route configured"}, ReasonConfig},
	{"image batch, full disk", Entry{Task: "generate_image", ErrClass: "disk_full", Reason: "batch job failed: ENOSPC: no space left on device"}, ReasonInfrastructure},
	{"video, over the token cap", Entry{Task: "generate_video", ErrClass: "token_cap_exceeded", Reason: "character animation refused: TOKEN_CAP_EXCEEDED"}, ReasonContract},
	{"video, black clip", Entry{Task: "generate_video", ErrClass: "black_clip", Reason: "output gate: black_clip"}, ReasonFailedVerification},
	{"video, no binding", Entry{Task: "generate_video", ErrClass: "not_configured", Reason: "no video binding"}, ReasonConfig},
	{"vision, no model", Entry{Task: "vqa", Reason: "no vision model configured"}, ReasonConfig},
	{"vision, empty output", Entry{Task: "vqa", Reason: "empty vision output"}, ReasonAbstention},
	{"vision, truncated", Entry{Task: "vqa", Reason: "vision output truncated"}, ReasonAbstention},
	{"vision, image would not load", Entry{Task: "vqa", Reason: "image load: open x.png: no such file"}, ReasonBadInput},
	{"video, frames could not be sampled", Entry{Task: "video_describe", Reason: "frame sampling: videoio: video \"x.mp4\": GetFileAttributesEx"}, ReasonBadInput},
	{"svg, no kind", Entry{Task: "generate_svg", Reason: "generate_svg: missing kind"}, ReasonBadInput},
	{"svg, write failed", Entry{Task: "generate_svg", Reason: "generate_svg: write: access denied"}, ReasonInfrastructure},
	{"compose, slot busy", Entry{Task: "compose_video", ErrClass: "compose_busy", Reason: "compose busy"}, ReasonNodeBusy},
	{"the remote lane's row, queued on a node", Entry{Task: "generate_image", Deferred: true, ErrClass: "gpu_queued", Node: "n1", CardByCaller: true, Reason: "gpu queued: ..."}, ReasonNodeBusy},
	{"the remote lane's row, no reason given", Entry{Task: "generate_image", Node: "n1", CardByCaller: true, Reason: "deferred"}, ReasonOther},
	{"a cascade defer that names nothing", Entry{Task: "classify", Reason: "confidence 0.41 below the gate"}, ReasonOther},
	{"an accelerator call that failed", Entry{Task: "classify_image", ModelTier: "coral-edgetpu", Reason: "sidecar answered 500"}, ReasonOther},
	{"a err_class the table does not know, with a reason that names nothing", Entry{Task: "ocr", ErrClass: "never_heard_of_it", Reason: "x"}, ReasonOther},
	{"nothing at all", Entry{Task: "summarize"}, ReasonOther},
}

func TestEveryDeferredRowCarriesAReasonCodeFromTheClosedSet(t *testing.T) {
	l, p := openLedger(t)
	for _, c := range deferShapes {
		e := c.row
		e.Deferred = true
		if err := l.Record(e); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	got, err := ReadAll(p)
	if err != nil || len(got) != len(deferShapes) {
		t.Fatalf("rows = %d (%v), want %d", len(got), err, len(deferShapes))
	}
	for i, c := range deferShapes {
		if !IsReasonCode(got[i].ReasonCode) {
			t.Errorf("%s: reason_code %q is not in the closed set", c.name, got[i].ReasonCode)
		}
		if got[i].ReasonCode != c.want {
			t.Errorf("%s: reason_code = %q, want %q", c.name, got[i].ReasonCode, c.want)
		}
	}
}

func TestARowThatDidNotDeferStaysWithoutACodeUnlessItIsADelegateRow(t *testing.T) {
	l, p := openLedger(t)
	rows := []Entry{
		{Task: "summarize", TokensIn: 10, TokensOut: 2},
		{Task: "generate_image", ErrClass: "gpu_busy"}, // an err_class on a row that was not a defer
		{Task: "agent_delegate", JobID: "agd-1"},
	}
	for _, e := range rows {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := ReadAll(p)
	for _, i := range []int{0, 1} {
		raw, _ := json.Marshal(got[i])
		if strings.Contains(string(raw), "reason_code") {
			t.Errorf("a row that did not defer gained a reason_code: %s", raw)
		}
	}
	if got[2].ReasonCode != ReasonOK {
		t.Errorf("a completed delegate row still says ok, got %q", got[2].ReasonCode)
	}
}

func TestAWritersOwnCodeWinsOverTheClassificationAndATypoDoesNot(t *testing.T) {
	l, p := openLedger(t)
	for _, e := range []Entry{
		{Task: "generate_image", Deferred: true, ErrClass: "gpu_busy", ReasonCode: ReasonQueueDeadline},
		{Task: "generate_image", Deferred: true, ErrClass: "gpu_busy", ReasonCode: "queue_deadlne"},
	} {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := ReadAll(p)
	if got[0].ReasonCode != ReasonQueueDeadline {
		t.Errorf("a code the writer set from the closed set is kept: %q", got[0].ReasonCode)
	}
	if got[1].ReasonCode != ReasonNodeBusy {
		t.Errorf("a code outside the set is replaced by the row's classification, got %q", got[1].ReasonCode)
	}
}

func TestADocumentedCodeIsAMemberAndTheFleetNodeDocListsEveryMember(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "FLEET-NODE.md"))
	if err != nil {
		t.Fatal(err)
	}
	var row string
	for _, ln := range strings.Split(strings.ReplaceAll(string(doc), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(ln, "| `reason_code` |") {
			row = ln
		}
	}
	if row == "" {
		t.Fatal("docs/FLEET-NODE.md has no `reason_code` row")
	}
	// The enumeration is the part before "Present on every": a member mentioned only in the explanation after
	// it (how rows are classified) does not count as listed.
	enum := row
	if i := strings.Index(row, "Present on every"); i > 0 {
		enum = row[:i]
	}
	for _, c := range ReasonCodes() {
		if !strings.Contains(enum, "`"+c+"`") {
			t.Errorf("the closed-set enumeration in the reason_code row of docs/FLEET-NODE.md does not list `%s`", c)
		}
	}
}

// writerFiles are the files that build a ledger.Entry or call Record on a ledger, and what each writes. A new
// writer fails TestEveryWriterOfALedgerRowIsKnown until it is listed here with its case in deferShapes: every
// writer funnels into Record, which settles the code, so what the list buys is that nobody adds a writer
// whose deferred rows were never looked at (F22).
var writerFiles = map[string]string{
	"internal/delegate/run.go":                 "agent_delegate rows; the code is the writer's own (reasonCodeFor)",
	"internal/delegate/telemetry.go":           "the dispatch marker (phase started)",
	"internal/pairworkloads/remote.go":         "the remote lane's row for a call a node ran; deferred when the node deferred or the dispatch failed",
	"internal/pipeline/pipeline.go":            "every door: entryFrom/callRowFrom (record, recordDefer, the composite rows) and RecordAccel",
	"internal/pipeline/pipeline_videowatch.go": "video_watch's whole-call row",
	"internal/shadow/label.go":                 "shadow and correctness-label rows; never deferred",
}

func TestEveryWriterOfALedgerRowIsKnown(t *testing.T) {
	root := filepath.Join("..", "..")
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "tools", "testdata", "ledger":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				if sel, ok := x.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Entry" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "ledger" {
						found[filepath.ToSlash(rel(root, path))] = true
					}
				}
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Record" {
					if recv, ok := sel.X.(*ast.SelectorExpr); ok && recv.Sel.Name == "led" {
						found[filepath.ToSlash(rel(root, path))] = true
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var got, want []string
	for f := range found {
		got = append(got, f)
	}
	for f := range writerFiles {
		want = append(want, f)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the files that write ledger rows changed.\n  found: %v\n  known: %v\nA new writer must be added to writerFiles with a row shaped like its output in deferShapes; Record settles the reason_code of its deferred rows, and this keeps anyone from adding one unlooked-at", got, want)
	}
}

func rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return r
}
