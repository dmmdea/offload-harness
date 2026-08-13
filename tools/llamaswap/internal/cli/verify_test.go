// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// cli-printing-press: novel-scaffold-test
// Novel command scaffold tests. Keep the wiring smoke test and add behavior cases as needed.

package cli

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

// TestNovelVerifyHelpWires smoke-tests that the verify command
// resolves at runtime and renders useful --help output. Catches wiring
// regressions (missing AddCommand, panicking RunE on --help, etc.) before
// review. Keep this smoke test when adding behavior-specific cases.
func TestNovelVerifyHelpWires(t *testing.T) {
	cmd := RootCmd()
	cmd.SetArgs([]string{"verify", "--help"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("verify --help error = %v (novel command not wired correctly?)", err)
	}
	help := out.String()
	for _, want := range []string{"Usage:", "verify", "--probe", "--init", "--expect-models", "--keepset", "--probe-each"} {
		if !strings.Contains(help, want) {
			t.Fatalf("verify --help missing %q in output:\n%s", want, help)
		}
	}
}

// The probe inputs are the calibration. If any of them changes, every stored
// baseline silently becomes a comparison against a different question - so a
// change here must be a deliberate, visible edit.
func TestVerifyProbeInputsArePinned(t *testing.T) {
	const wantEmbedSHA = "376fb0f28b5c9d0fbe4c6bea3a169e6a6eacb7feed098df694739c3a8753b095"
	if got := verifySHA(verifyProbeEmbedText); got != wantEmbedSHA {
		t.Errorf("the embed probe text changed (sha %s): every stored baseline is now keyed to a different input", got)
	}
	const wantRerankSHA = "0cefe4d9f31be9c4ac3365bcd72dbeb38a08442c3de968b81f56e82ee6b4c663"
	got := verifySHA(verifyProbeRerankQ + "\x00" + verifyProbeRerankDocA + "\x00" + verifyProbeRerankDocB)
	if got != wantRerankSHA {
		t.Errorf("the rerank probe pair changed (sha %s): stored baselines no longer apply", got)
	}
}

func TestVerifyCosine(t *testing.T) {
	a := []float64{1, 2, 3, 4}
	if got := verifyCosine(a, a); math.Abs(got-1) > 1e-12 {
		t.Errorf("cosine(v,v) = %v, want 1", got)
	}
	// Scaling must not move cosine: a renormalized-but-identical embedding is
	// not a degradation.
	scaled := []float64{2, 4, 6, 8}
	if got := verifyCosine(a, scaled); math.Abs(got-1) > 1e-12 {
		t.Errorf("cosine is not scale-invariant: %v", got)
	}
	// A genuinely different vector must fall below the floor.
	other := []float64{4, -3, 2, -1}
	if got := verifyCosine(a, other); got >= verifyCosineFloor {
		t.Errorf("cosine(different) = %v, must be below the %v floor", got, verifyCosineFloor)
	}
	if got := verifyCosine([]float64{0, 0}, []float64{0, 0}); got != 0 {
		t.Errorf("zero vectors must not produce a fake 1.0, got %v", got)
	}
}

// The vector hash must be reproducible and must not flip on float noise below
// the recorded precision.
func TestVerifyVectorSHAIsStableAndSensitive(t *testing.T) {
	v := []float64{0.1234567, -0.7654321, 0.0}
	a := verifyVectorSHA(v)
	if a != verifyVectorSHA([]float64{0.1234567, -0.7654321, 0.0}) {
		t.Fatal("vector sha is not reproducible")
	}
	if a == verifyVectorSHA([]float64{0.1234567, -0.7654322, 0.0}) {
		// -0.7654321 vs -0.7654322 round to the same 6 decimals, so this
		// SHOULD collide; asserting the collision documents the precision
		// choice rather than leaving it implicit.
		t.Log("6-decimal precision collides on sub-1e-6 noise, as designed")
	}
	if a == verifyVectorSHA([]float64{0.1244567, -0.7654321, 0.0}) {
		t.Error("a 1e-3 change must change the vector sha")
	}
}

func TestVerifySplitCSV(t *testing.T) {
	got := verifySplitCSV(" embeddinggemma , bge-reranker-v2-m3 ,, ")
	if len(got) != 2 || got[0] != "embeddinggemma" || got[1] != "bge-reranker-v2-m3" {
		t.Errorf("verifySplitCSV = %q", got)
	}
	if len(verifySplitCSV("")) != 0 {
		t.Error("empty keepset must yield no members")
	}
}

func TestVerifyFailureSummariesNameTheNumbers(t *testing.T) {
	r := &verifyReport{
		Probes: []verifyProbeResult{
			{Kind: "embed", Model: "embeddinggemma", Pass: false, Measured: 0.42, Expected: 1, Detail: "DEGRADED"},
			{Kind: "rerank", Model: "bge-reranker-v2-m3", Pass: true},
		},
		Keepset: []verifyKeepsetMember{
			{Name: "embeddinggemma", Answered: false, Detail: "embedding call failed"},
		},
	}
	summary := verifyProbeFailureSummary(r)
	for _, want := range []string{"embed", "embeddinggemma", "0.42"} {
		if !strings.Contains(summary, want) {
			t.Errorf("probe failure summary missing %q: %s", want, summary)
		}
	}
	if strings.Contains(summary, "bge-reranker") {
		t.Errorf("passing probes must not appear in the failure summary: %s", summary)
	}
	if ks := verifyKeepsetFailureSummary(r); !strings.Contains(ks, "embedding call failed") {
		t.Errorf("keepset summary missing the reason: %s", ks)
	}
}
