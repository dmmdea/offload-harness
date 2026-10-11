package main

// What the command says when the batch ends with jobs unfinished (verdict queued) depends on WHY they are unfinished. The
// text used to be one sentence about a job already on a node that cannot be recalled, which is true of an interrupt and wrong
// for a job queued because no lane or node had a place for it (nothing was sent, so there is nothing to recall).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/mediaremote"
)

func queuedRows(n int) []mediaremote.BatchRow {
	rows := make([]mediaremote.BatchRow, n)
	for i := range rows {
		rows[i] = mediaremote.BatchRow{I: i, Out: "p.png", Verdict: mediaremote.VerdictQueued, Error: "a reason"}
	}
	return rows
}

func TestBatchExitSaysWhyTheJobsAreUnfinished(t *testing.T) {
	// The caller's interrupt: a job already on a node cannot be recalled, and that is why the seed matters.
	err := batchExit(&mediaremote.Batch{Rows: queuedRows(2), Interrupted: true})
	if code, line := exitStatus(err); code != 1 || !strings.Contains(line, "interrupted") || !strings.Contains(line, "cannot be recalled") || !strings.Contains(line, "--resume") {
		t.Errorf("an interrupted batch: exit %d %q, want exit 1 saying it was interrupted, what cannot be recalled, and --resume", code, line)
	}
	// Queued for want of a place, with no interrupt: nothing was sent that could need recalling, and nobody interrupted.
	err = batchExit(&mediaremote.Batch{Rows: queuedRows(2)})
	line := ""
	if err != nil {
		_, line = exitStatus(err)
	}
	if err == nil || strings.Contains(line, "interrupt") || strings.Contains(line, "cannot be recalled") || !strings.Contains(line, "2 of 2 jobs") || !strings.Contains(line, "--resume") {
		t.Errorf("a batch whose jobs found no place: %q, want the count, --resume, and neither an interrupt nor a recall that did not happen", line)
	}
	// A job waited the queue's ceiling and the jobs behind it were not sent: say that, not an interrupt or a recall.
	err = batchExit(&mediaremote.Batch{Rows: queuedRows(3), Ceiling: true})
	line = ""
	if err != nil {
		_, line = exitStatus(err)
	}
	if err == nil || strings.Contains(line, "interrupt") || strings.Contains(line, "cannot be recalled") || !strings.Contains(line, "3 of 3 jobs") || !strings.Contains(line, "waited the queue's 8 h ceiling") || !strings.Contains(line, "were not sent") || !strings.Contains(line, "--resume") {
		t.Errorf("a batch that waited the ceiling: %q, want the count, the ceiling, that the rest were not sent, and --resume", line)
	}
	// Both: the interrupt's words, and the ceiling named too.
	err = batchExit(&mediaremote.Batch{Rows: queuedRows(3), Ceiling: true, Interrupted: true})
	line = ""
	if err != nil {
		_, line = exitStatus(err)
	}
	if err == nil || !strings.Contains(line, "interrupted with 3 of 3 jobs") || !strings.Contains(line, "cannot be recalled") || !strings.Contains(line, "ceiling") {
		t.Errorf("an interrupted batch that had also waited the ceiling: %q, want both causes said", line)
	}
	// A stop with an error keeps the error it stopped with.
	stopped := errors.New("ENOSPC")
	if got := batchExit(&mediaremote.Batch{Rows: queuedRows(1), Err: stopped}); got != stopped {
		t.Errorf("a batch the warm call stopped keeps its error, got %v", got)
	}
}

// A killed batch leaves its rows in <results>.partial. The recovery run (--resume --results the same file) must not truncate
// that record: it is set aside as <results>.partial.prev and the command says so; and a run the batch refuses puts it back.
func TestGenerateImageBatchKeepsTheRecordOfAKilledBatchBesideTheRecoveryRun(t *testing.T) {
	home, cfgPath, _, _ := pngBatchHome(t)
	jobs := filepath.Join(home, "jobs.jsonl")
	writeJobs(t, jobs, `{"prompt":"a red bike","out":`+strconvQuote(filepath.ToSlash(filepath.Join(home, "pics", "one.png")))+`,"seed":5,"width":64,"height":32}`)
	results := filepath.Join(home, "r", "results.jsonl")
	if err := os.MkdirAll(filepath.Dir(results), 0o755); err != nil {
		t.Fatal(err)
	}
	dead := `{"i":0,"out":"x.png","seed":5,"ok":true,"ms":9,"node":"node-a","verdict":"rendered"}` + "\n"
	if err := os.WriteFile(results+".partial", []byte(dead), 0o644); err != nil {
		t.Fatal(err)
	}

	// a refused recovery run leaves the record where it was
	var err error
	captureStdout(t, func() {
		_ = captureStderr(t, func() {
			err = runGenerateImage([]string{"--batch", jobs, "--config", cfgPath, "--family", "nope", "--resume", "--results", results})
		})
	})
	if err == nil {
		t.Fatalf("an unknown family must be refused")
	}
	if raw, _ := os.ReadFile(results + ".partial"); string(raw) != dead {
		t.Errorf("a refused recovery run left the record as %q, want it whole under its own name", raw)
	}
	if _, serr := os.Stat(results + ".partial.prev"); serr == nil {
		t.Errorf("a refused run left a set-aside copy beside the record it put back")
	}

	// the recovery run itself sets it aside and says so
	var stderr string
	captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			err = runGenerateImage([]string{"--batch", jobs, "--config", cfgPath, "--resume", "--results", results, "--json"})
		})
	})
	if err != nil {
		t.Fatalf("the recovery run: %v", err)
	}
	if raw, _ := os.ReadFile(results + ".partial.prev"); string(raw) != dead {
		t.Errorf("the killed batch's record = %q, want it kept whole as .partial.prev", raw)
	}
	if !strings.Contains(stderr, results+".partial.prev") || !strings.Contains(stderr, "kept as") {
		t.Errorf("the command must say where the earlier record went: %q", stderr)
	}
	if rows := readResults(t, results); len(rows) != 1 || rows[0].Verdict != "rendered" {
		t.Errorf("the recovery run's own results = %+v", rows)
	}
}
