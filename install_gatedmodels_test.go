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
	weight26B   = "gemma-4-26B-A4B-it-qat-UD-Q4_K_XL.gguf"
	weightQ38   = "Qwen3.8-27B-UD-Q4_K_XL.gguf"
	mmprojQ38   = "mmproj-Qwen3.8-27B-F16.gguf"
	weightQ354B = "Qwen3.5-4B-UD-Q4_K_XL.gguf"
	weightQ359B = "Qwen3.5-9B-UD-Q4_K_XL.gguf"
)

func gatedWarn(t *testing.T, in26B, inQ38, inQ354B, inQ359B bool, dir, target string) string {
	t.Helper()
	var buf bytes.Buffer
	warnMissingGatedModelsTo(in26B, inQ38, inQ354B, inQ359B, dir, target, &buf)
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

	out := gatedWarn(t, false, true, true, true, dir, runtime.GOOS)

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
	out := gatedWarn(t, true, true, true, true, dir, runtime.GOOS)

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
	if out := gatedWarn(t, true, true, true, true, dir, runtime.GOOS); strings.TrimSpace(out) != "" {
		t.Fatalf("all gated weights present must be silent, got:\n%s", out)
	}

	// 2. No gates set => silence, even on an empty dir. A tier that asked for
	//    nothing cannot be missing anything.
	if out := gatedWarn(t, false, false, false, false, t.TempDir(), runtime.GOOS); strings.TrimSpace(out) != "" {
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
	if out := gatedWarn(t, true, true, true, true, t.TempDir(), otherOS); strings.TrimSpace(out) != "" {
		t.Fatalf("cross-machine render must be silent, got:\n%s", out)
	}

	// 4. Empty modelsDir => silence (nothing to resolve a relative path against).
	if out := gatedWarn(t, true, true, true, true, "", runtime.GOOS); strings.TrimSpace(out) != "" {
		t.Fatalf("empty modelsDir must be silent, got:\n%s", out)
	}
}
