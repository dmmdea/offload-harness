package main

// Coverage for warnMissingGatedModels — 0.72.0 review finding I-2: the function
// had ZERO test references. It is the last line of defence for a specific silent
// failure: llama-swap lists models from the CONFIG, so a gated entry whose weights
// were never downloaded makes `doctor` and `acceptance` PASS while the route fails
// only when someone actually calls it.
//
// Every case asserts the silent arm too. This warning's whole value is that it
// fires ONLY on a real local miss — it is skipped for cross-machine renders, where
// a local absence means nothing, and that skip is itself a silent path worth
// pinning.

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The filenames are the templates' own cmd contract (and install.ps1's $PINNED
// table). Duplicated here deliberately: if a rename lands on one side only, this
// test fails rather than silently checking a path nothing downloads to.
const (
	weight26B    = "gemma-4-26B-A4B-it-qat-UD-Q4_K_XL.gguf"
	weightQ38    = "Qwen3.8-27B-UD-Q4_K_XL.gguf"
	mmprojQ38    = "mmproj-Qwen3.8-27B-F16.gguf"
	weightQ354B  = "Qwen3.5-4B-UD-Q4_K_XL.gguf"
	weightQ359B  = "Qwen3.5-9B-UD-Q4_K_XL.gguf"
	weightQ3827B = "Qwen3.8-27B-UD-IQ3_S.gguf"
	weightMimo9B = "MiMo-V2.6-Distill-Qwen-9B-Q4_K_M.gguf"
	// The RAM-spill seat's weight sits in a SUBDIRECTORY of the models dir: the templates'
	// cmd and install.ps1's pinned `name` both carry it.
	weightQ3635B = "Qwen3.6-35B-A3B/Qwen3.6-35B-A3B-UD-IQ3_XXS.gguf"
	// The second memory-stack embedder: a model AND its multimodal projector, flat in the models dir.
	weightEG2 = "embeddinggemma-2-Q8_0.gguf"
	mmprojEG2 = "mmproj-embeddinggemma-2-Q8_0.gguf"
)

func gatedWarn(t *testing.T, in26B, inQ38, inQ354B, inQ359B, inQ3827B, inMimo9B bool, dir, target string) string {
	t.Helper()
	var buf bytes.Buffer
	warnMissingGatedModelsTo(in26B, inQ38, inQ354B, inQ359B, inQ3827B, inMimo9B, false, false, false, dir, target, &buf)
	return buf.String()
}

// gatedWarnQ3635B drives the RAM-spill seat's gate alone, every other gate off.
func gatedWarnQ3635B(t *testing.T, inQ3635B bool, dir, target string) string {
	t.Helper()
	var buf bytes.Buffer
	warnMissingGatedModelsTo(false, false, false, false, false, false, inQ3635B, false, false, dir, target, &buf)
	return buf.String()
}

// gatedWarnEG2 drives the memory-stack embedder's gate alone, every other gate off. inProjector is the
// tier's projector flag (embeddinggemma2_projector, true unless the tier is a text-only replica).
func gatedWarnEG2(t *testing.T, inEG2, inProjector bool, dir, target string) string {
	t.Helper()
	var buf bytes.Buffer
	warnMissingGatedModelsTo(false, false, false, false, false, false, false, inEG2, inProjector, dir, target, &buf)
	return buf.String()
}

func touchWeight(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWarnMissingGatedModelsNamesOnlyAbsentGatedWeights(t *testing.T) {
	dir := t.TempDir()
	// Q38's pair is PRESENT on disk; Q354B's weight is ABSENT.
	touchWeight(t, dir, weightQ38)
	touchWeight(t, dir, mmprojQ38)

	out := gatedWarn(t, false, true, true, true, false, false, dir, runtime.GOOS)

	if strings.Contains(out, weightQ38) || strings.Contains(out, mmprojQ38) {
		t.Fatalf("a PRESENT gated weight must not be reported missing:\n%s", out)
	}
	if !strings.Contains(out, weightQ354B) || !strings.Contains(out, weightQ359B) {
		t.Fatalf("an ABSENT gated weight must be reported:\n%s", out)
	}
	// The 26B gate was OFF, so its weight is absent-but-not-asked-for. Reporting
	// it would send the operator to download a model this tier never wanted.
	if strings.Contains(out, weight26B) {
		t.Fatalf("an ungated model must never be reported:\n%s", out)
	}
	// The count in the header must match the number of entries listed, or the
	// message misstates the size of the problem (Q354B + Q359B weights are the
	// two absent gated files here).
	if !strings.Contains(out, "2 gated model weight(s)") {
		t.Fatalf("want a count of exactly 2, got:\n%s", out)
	}
}

func TestWarnMissingGatedModelsCountsEachMissingFileIncludingMmproj(t *testing.T) {
	dir := t.TempDir() // nothing on disk at all
	out := gatedWarn(t, true, true, true, true, false, false, dir, runtime.GOOS)

	// 26B weight + Q38 weight + Q38 mmproj + Q354B weight + Q359B weight = 5. The
	// mmproj is a SEPARATE check: a vision entry with its weights but no projector
	// loads and then fails on the first image, so it must be counted on its own.
	if !strings.Contains(out, "5 gated model weight(s)") {
		t.Fatalf("want a count of 5, got:\n%s", out)
	}
	for _, name := range []string{weight26B, weightQ38, mmprojQ38, weightQ354B, weightQ359B} {
		if !strings.Contains(out, name) {
			t.Fatalf("missing weight %q not reported:\n%s", name, out)
		}
	}
}

// Silent arms. Three ways this warning must stay quiet; each is a path that would
// otherwise cry wolf and train the operator to ignore a real miss.
func TestWarnMissingGatedModelsSilentArms(t *testing.T) {
	dir := t.TempDir()
	touchWeight(t, dir, weight26B)
	touchWeight(t, dir, weightQ38)
	touchWeight(t, dir, mmprojQ38)
	touchWeight(t, dir, weightQ354B)
	touchWeight(t, dir, weightQ359B)

	// 1. Everything gated is present => silence.
	if out := gatedWarn(t, true, true, true, true, false, false, dir, runtime.GOOS); strings.TrimSpace(out) != "" {
		t.Fatalf("all gated weights present must be silent, got:\n%s", out)
	}

	// 2. No gates set => silence, even on an empty dir. A tier that asked for
	//    nothing cannot be missing anything.
	if out := gatedWarn(t, false, false, false, false, false, false, t.TempDir(), runtime.GOOS); strings.TrimSpace(out) != "" {
		t.Fatalf("no gates set must be silent, got:\n%s", out)
	}

	// 3. CROSS-MACHINE render => silence. Rendering a config FOR another OS must
	//    not warn about this machine's disk: the weights are expected to live on
	//    the target, and warning here would be a false alarm on every remote
	//    render. This is the one branch whose correctness is invisible in
	//    production, since it produces no output either way.
	otherOS := "linux"
	if runtime.GOOS == "linux" {
		otherOS = "windows"
	}
	if out := gatedWarn(t, true, true, true, true, false, false, t.TempDir(), otherOS); strings.TrimSpace(out) != "" {
		t.Fatalf("cross-machine render must be silent, got:\n%s", out)
	}

	// 4. Empty modelsDir => silence (nothing to resolve a relative path against).
	if out := gatedWarn(t, true, true, true, true, false, false, "", runtime.GOOS); strings.TrimSpace(out) != "" {
		t.Fatalf("empty modelsDir must be silent, got:\n%s", out)
	}
}

// TestWarnMissingGatedModelsCoversThe27BAgentSeat pins the 16GB-class agent seat into
// the same warning the other gated seats get. The gap this closes is the one the
// installer's own comment names: a tier can RENDER a seat whose weights were never
// downloaded, and because llama-swap lists models from the CONFIG, `doctor` and
// `acceptance` both pass while the route fails only when something calls it. The seat
// this covers (ADR 0047) beat the one it replaces 24 of 24 blind, so shipping it
// unserved would be the worst version of that failure.
func TestWarnMissingGatedModelsCoversThe27BAgentSeat(t *testing.T) {
	dir := t.TempDir()
	// Gate ON, weight absent -> it must be named.
	out := gatedWarn(t, false, false, false, false, true, false, dir, runtime.GOOS)
	if !strings.Contains(out, weightQ3827B) {
		t.Errorf("include_qwen38_27b is set and %s is absent, but the warning does not name it:\n%s",
			weightQ3827B, out)
	}
	// Weight present -> silent, so the warning cannot cry wolf on a complete install.
	touchWeight(t, dir, weightQ3827B)
	if out := gatedWarn(t, false, false, false, false, true, false, dir, runtime.GOOS); strings.TrimSpace(out) != "" {
		t.Errorf("every gated weight is present, want silence, got:\n%s", out)
	}
	// Gate OFF -> silent even with the weight absent, or every non-16GB tier would warn.
	if out := gatedWarn(t, false, false, false, false, false, false, t.TempDir(), runtime.GOOS); strings.TrimSpace(out) != "" {
		t.Errorf("include_qwen38_27b is unset, want silence, got:\n%s", out)
	}
}

// TestWarnMissingGatedModelsCoversTheMimoSeat is TestWarnMissingGatedModelsCoversThe27BAgentSeat's
// mirror for mimo-9b-agent: the second 8GB-class agent seat must get the same
// last-line-of-defence coverage the first one (qwen3.5-9b-agent) already has —
// both entries can render into the roster while the GGUF sits undownloaded, and
// llama-swap's CONFIG-sourced /v1/models makes doctor/acceptance blind to that.
func TestWarnMissingGatedModelsCoversTheMimoSeat(t *testing.T) {
	dir := t.TempDir()
	// Gate ON, weight absent -> it must be named.
	out := gatedWarn(t, false, false, false, false, false, true, dir, runtime.GOOS)
	if !strings.Contains(out, weightMimo9B) {
		t.Errorf("include_mimo_9b is set and %s is absent, but the warning does not name it:\n%s",
			weightMimo9B, out)
	}
	// Weight present -> silent, so the warning cannot cry wolf on a complete install.
	touchWeight(t, dir, weightMimo9B)
	if out := gatedWarn(t, false, false, false, false, false, true, dir, runtime.GOOS); strings.TrimSpace(out) != "" {
		t.Errorf("every gated weight is present, want silence, got:\n%s", out)
	}
	// Gate OFF -> silent even with the weight absent, or every tier without the seat would warn.
	if out := gatedWarn(t, false, false, false, false, false, false, t.TempDir(), runtime.GOOS); strings.TrimSpace(out) != "" {
		t.Errorf("include_mimo_9b is unset, want silence, got:\n%s", out)
	}
	// Both 8GB-class agent seats gated on and absent -> both are named, distinctly.
	both := gatedWarn(t, false, false, false, true, false, true, t.TempDir(), runtime.GOOS)
	if !strings.Contains(both, weightQ359B) || !strings.Contains(both, weightMimo9B) {
		t.Errorf("both qwen3.5-9b-agent and mimo-9b-agent gated and absent must both be named:\n%s", both)
	}
	if !strings.Contains(both, "2 gated model weight(s)") {
		t.Errorf("want a count of exactly 2, got:\n%s", both)
	}
}

// TestWarnMissingGatedModelsCoversTheQwen3635BSpillSeat pins the RAM-spill agent seat into the
// same last-line-of-defence warning the other gated seats get, INCLUDING the subdirectory its
// weight lives in: a render that stats a flat Qwen3.6-35B-A3B-UD-IQ3_XXS.gguf would warn
// forever on a box that fetched it where the template reads it. The gate it is handed is the
// post-RAM-gate value, so a `min` box (seat not rendered) is told nothing.
func TestWarnMissingGatedModelsCoversTheQwen3635BSpillSeat(t *testing.T) {
	dir := t.TempDir()
	// Gate ON, weight absent -> it must be named, with its subdirectory.
	out := gatedWarnQ3635B(t, true, dir, runtime.GOOS)
	if !strings.Contains(out, filepath.Join(dir, filepath.FromSlash(weightQ3635B))) {
		t.Errorf("the spill seat is gated on and %s is absent, but the warning does not name that path:\n%s", weightQ3635B, out)
	}
	if !strings.Contains(out, "1 gated model weight(s)") {
		t.Errorf("want a count of exactly 1, got:\n%s", out)
	}
	// The weight FLAT in the models dir is not the file the template reads: still absent.
	touchWeight(t, dir, filepath.Base(weightQ3635B))
	if out := gatedWarnQ3635B(t, true, dir, runtime.GOOS); !strings.Contains(out, filepath.FromSlash(weightQ3635B)) {
		t.Errorf("a flat copy of the GGUF must not satisfy the check (the template reads the subdirectory):\n%s", out)
	}
	// Weight in its subdirectory -> silent.
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(weightQ3635B)), 0o755); err != nil {
		t.Fatal(err)
	}
	touchWeight(t, dir, filepath.FromSlash(weightQ3635B))
	if out := gatedWarnQ3635B(t, true, dir, runtime.GOOS); strings.TrimSpace(out) != "" {
		t.Errorf("every gated weight is present, want silence, got:\n%s", out)
	}
	// Gate OFF (the tier does not carry it, or the box is `min`) -> silent with the weight absent.
	if out := gatedWarnQ3635B(t, false, t.TempDir(), runtime.GOOS); strings.TrimSpace(out) != "" {
		t.Errorf("the spill seat is not rendered, want silence, got:\n%s", out)
	}
}

// TestWarnMissingGatedModelsNamesBothEmbeddingGemma2Files pins the stack member into the same
// last-line-of-defence warning the other gated entries get, and requires BOTH files: a model with no
// projector loads and then fails on the first image embed, so the projector is counted on its own.
func TestWarnMissingGatedModelsNamesBothEmbeddingGemma2Files(t *testing.T) {
	dir := t.TempDir()
	out := gatedWarnEG2(t, true, true, dir, runtime.GOOS)
	for _, name := range []string{weightEG2, mmprojEG2} {
		if !strings.Contains(out, name) {
			t.Errorf("include_embeddinggemma2 is set and %s is absent, but the warning does not name it:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "2 gated model weight(s)") {
		t.Errorf("want a count of exactly 2 (model + projector), got:\n%s", out)
	}
	// the model present, the projector absent -> only the projector is named
	touchWeight(t, dir, weightEG2)
	out = gatedWarnEG2(t, true, true, dir, runtime.GOOS)
	if strings.Contains(out, "embeddinggemma2 (model)") || !strings.Contains(out, "embeddinggemma2 (mmproj)") || !strings.Contains(out, "1 gated model weight(s)") {
		t.Errorf("a missing projector alone must be reported on its own, got:\n%s", out)
	}
	touchWeight(t, dir, mmprojEG2)
	if out := gatedWarnEG2(t, true, true, dir, runtime.GOOS); strings.TrimSpace(out) != "" {
		t.Errorf("both files present, want silence, got:\n%s", out)
	}
	if out := gatedWarnEG2(t, false, true, t.TempDir(), runtime.GOOS); strings.TrimSpace(out) != "" {
		t.Errorf("a tier without the stack member must not be told to fetch its files, got:\n%s", out)
	}
}

// TestWarnMissingGatedModelsNamesOnlyTheEmbeddingGemma2ModelForATextOnlyTier: a replica's entry starts
// without --mmproj and the installer never fetches the projector, so a missing projector is not a
// warning there (it would fire forever), while a missing model still is.
func TestWarnMissingGatedModelsNamesOnlyTheEmbeddingGemma2ModelForATextOnlyTier(t *testing.T) {
	dir := t.TempDir()
	out := gatedWarnEG2(t, true, false, dir, runtime.GOOS)
	if !strings.Contains(out, weightEG2) {
		t.Errorf("a text-only tier is missing the model and the warning does not name it:\n%s", out)
	}
	if strings.Contains(out, mmprojEG2) || strings.Contains(out, "embeddinggemma2 (mmproj)") {
		t.Errorf("a text-only tier is told to fetch a projector it never downloads:\n%s", out)
	}
	if !strings.Contains(out, "1 gated model weight(s)") {
		t.Errorf("want a count of exactly 1 (the model), got:\n%s", out)
	}
	touchWeight(t, dir, weightEG2)
	if out := gatedWarnEG2(t, true, false, dir, runtime.GOOS); strings.TrimSpace(out) != "" {
		t.Errorf("the model alone satisfies a text-only tier, want silence, got:\n%s", out)
	}
}
