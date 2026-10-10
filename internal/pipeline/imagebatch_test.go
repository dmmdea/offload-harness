package pipeline

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestNormalizeImageBatch_SeedsOutsAndJSONL(t *testing.T) {
	jobs := []ImageBatchJob{
		{Prompt: "a red bike", Seed: 7, Out: `D:\x\bike.png`},
		{Prompt: "a green apple"}, // no seed, no out
	}
	norm, jsonl := normalizeImageBatch(jobs, `D:\media`)
	if norm[0].Seed != 7 || norm[0].Out != `D:\x\bike.png` {
		t.Fatalf("explicit seed/out must be preserved: %+v", norm[0])
	}
	if norm[1].Seed <= 0 {
		t.Fatalf("missing seed must be minted, got %d", norm[1].Seed)
	}
	if !strings.HasPrefix(norm[1].Out, `D:\media`) || !strings.HasSuffix(norm[1].Out, ".png") {
		t.Fatalf("missing out must default under MediaDir, got %q", norm[1].Out)
	}
	lines := strings.Split(strings.TrimSpace(jsonl), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 JSONL lines, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"prompt":"a red bike"`) || !strings.Contains(lines[0], `"seed":7`) {
		t.Fatalf("line 0 malformed: %s", lines[0])
	}
}

func TestParseBatchResults_OrderAndFailures(t *testing.T) {
	norm := []ImageBatchJob{{Prompt: "a", Seed: 1, Out: "a.png"}, {Prompt: "b", Seed: 2, Out: "b.png"}, {Prompt: "c", Seed: 3, Out: "c.png"}}
	raw := `{"i":0,"out":"a.png","seed":1,"ok":true,"ms":25000}
{"i":1,"out":"b.png","seed":2,"ok":false,"ms":900,"error":"comfy-render exited 1"}`
	items := parseBatchResults([]byte(raw), norm)
	if len(items) != 3 {
		t.Fatalf("every job gets an item, got %d", len(items))
	}
	if !items[0].OK || items[0].Ms != 25000 {
		t.Fatalf("item 0 wrong: %+v", items[0])
	}
	if items[1].OK || items[1].Error == "" {
		t.Fatalf("item 1 must carry the failure: %+v", items[1])
	}
	if items[2].OK || items[2].Error != "no result recorded (batch aborted?)" {
		t.Fatalf("missing line => explicit not-run item: %+v", items[2])
	}
}

// Review fix #1: two jobs differing ONLY in negative must not share a default out
// path (the second would silently overwrite the first).
func TestNormalizeImageBatch_NegativeInDefaultOutHash(t *testing.T) {
	jobs := []ImageBatchJob{
		{Prompt: "same", Seed: 7, Width: 512, Height: 512, Negative: "people"},
		{Prompt: "same", Seed: 7, Width: 512, Height: 512, Negative: "text"},
	}
	norm, _ := normalizeImageBatch(jobs, "M")
	if norm[0].Out == norm[1].Out {
		t.Fatalf("different negatives must yield different default outs, both %q", norm[0].Out)
	}
}

// Review fix #2: ledger ErrClass parity — a job with no result line inherits the
// batch-level cause; a job with its own error is classified from that error.
func TestBatchErrClass(t *testing.T) {
	timeoutErr := fmt.Errorf("gpugen: comfy-generate.mjs failed: context deadline exceeded (killed)")
	if got := batchErrClass("no result recorded (batch aborted?)", timeoutErr); got != "timeout" {
		t.Fatalf("aborted job should classify from the batch error: got %q", got)
	}
	if got := batchErrClass("connect ECONNREFUSED 127.0.0.1:8188", timeoutErr); got != "conn_refused" {
		t.Fatalf("a job's own error wins over the batch error: got %q", got)
	}
	if got := batchErrClass("CUDA out of memory", nil); got != "oom" {
		t.Fatalf("oom classification: got %q", got)
	}
}

// 2026-10-09: a batch stopped on a full drive and its items were recorded with err_class "other".
// A full volume is its own class, read from the errno token (the render helpers' rule), never from
// prose, for the image lanes (this file's classifyErr) as for the rest (gpugen.ClassifyErr).
func TestBatchErrClassRecordsAFullDiskAsDiskFull(t *testing.T) {
	stopped := fmt.Errorf("gpugen: comfy-generate.mjs failed: exit status 1 (IMAGE BATCH FAILED: the disk is full at job 2/4, writing renders/b.png (comfy-render exited 1: ENOSPC: no space left on device, write); 2 jobs not run, recorded as such)")
	for name, got := range map[string]string{
		"the failing job's own row":      batchErrClass("comfy-render exited 1: ENOSPC: no space left on device, write (writing renders/b.png)", nil),
		"a not-run row":                  batchErrClass("not run: the disk is full at job 2/4, writing renders/b.png (comfy-render exited 1: ENOSPC: no space left on device, write)", nil),
		"ComfyUI's own errno":            batchErrClass("comfy-render exited 1: ComfyUI exec error: OSError: [Errno 28] No space left on device", nil),
		"a job with no row, batch error": batchErrClass("no result recorded (batch aborted?)", stopped),
		"a Go write that failed":         classifyErr(&os.PathError{Op: "write", Path: "jobs.jsonl", Err: syscall.ENOSPC}),
		"the batch error itself":         classifyErr(stopped),
	} {
		if got != "disk_full" {
			t.Errorf("%s: err_class = %q, want disk_full", name, got)
		}
	}
	// prose alone is not a full disk: a prompt echoed in an exec error must not be recorded as one
	for _, msg := range []string{"no space left on device", `ComfyUI exec error: {"current_inputs":{"text":["a monitor reading: no space left on device"]}}`} {
		if got := classifyErr(fmt.Errorf("%s", msg)); got != "other" {
			t.Errorf("classifyErr(%q) = %q, want other", msg, got)
		}
	}
}
