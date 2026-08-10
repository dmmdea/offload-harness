package main

// CLI-surface coverage for the opt-in prompt refiner's two generate-image
// fixes: --refine=false must propagate onto batch jobs (job-level wins), and
// the space form "--refine false" must error loudly instead of silently
// dropping every later flag (the flag package stops at the first non-flag).

import (
	"flag"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/pipeline"
)

func TestApplyBatchRefineDefault(t *testing.T) {
	on, off := true, false
	jobs := []pipeline.ImageBatchJob{
		{Prompt: "a"},               // no per-job value: takes the CLI default
		{Prompt: "b", Refine: &on},  // job-level true wins
		{Prompt: "c", Refine: &off}, // job-level false stays
	}
	// --refine (default true) is inert: nothing is written.
	applyBatchRefineDefault(jobs, true)
	if jobs[0].Refine != nil {
		t.Errorf("refine=true must write nothing, got %v", *jobs[0].Refine)
	}
	// --refine=false fills only the unset job.
	applyBatchRefineDefault(jobs, false)
	if jobs[0].Refine == nil || *jobs[0].Refine {
		t.Errorf("unset job must default to refine:false, got %v", jobs[0].Refine)
	}
	if jobs[1].Refine == nil || !*jobs[1].Refine {
		t.Errorf("job-level refine:true must win over the CLI flag, got %v", jobs[1].Refine)
	}
	if jobs[2].Refine == nil || *jobs[2].Refine {
		t.Errorf("job-level refine:false must stay, got %v", jobs[2].Refine)
	}
}

func TestLeftoverArgErr(t *testing.T) {
	// Space form: "--refine false" leaves "false" (and every later flag) as
	// unparsed leftovers — reject loudly, naming the =form.
	fs := flag.NewFlagSet("generate-image", flag.ContinueOnError)
	fs.Bool("refine", true, "")
	fs.Int("width", 0, "")
	if err := fs.Parse([]string{"--refine", "false", "--width", "500"}); err != nil {
		t.Fatal(err)
	}
	err := leftoverArgErr(fs, "generate-image")
	if err == nil || !strings.Contains(err.Error(), "--refine=false") || !strings.Contains(err.Error(), `"false"`) {
		t.Fatalf("space-form boolean must error naming the =form, got %v", err)
	}
	// The =form parses clean: no leftovers, no error.
	fs2 := flag.NewFlagSet("generate-image", flag.ContinueOnError)
	fs2.Bool("refine", true, "")
	fs2.Int("width", 0, "")
	if err := fs2.Parse([]string{"--refine=false", "--width", "500"}); err != nil {
		t.Fatal(err)
	}
	if err := leftoverArgErr(fs2, "generate-image"); err != nil {
		t.Fatalf("=form must pass, got %v", err)
	}
}
