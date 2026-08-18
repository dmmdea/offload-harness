// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package measure

import "testing"

// Real nvidia-smi output shape from this box (--format=csv,noheader,nounits).
const smiFixture = `GPU-1111aaaa-2222-3333-4444-555566667777, NVIDIA GeForce RTX 5060 Ti, 721, 16311
GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000, NVIDIA GeForce RTX 5070 Ti, 1607, 16303`

func TestParseGPUCSVKeysByUUID(t *testing.T) {
	roles := map[string]string{
		"GPU-1111aaaa-2222-3333-4444-555566667777": "utility-card",
		"GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000": "fast-card",
	}
	gpus, err := parseGPUCSV(smiFixture, roles)
	if err != nil {
		t.Fatalf("parseGPUCSV: %v", err)
	}
	if len(gpus) != 2 {
		t.Fatalf("got %d GPUs, want 2", len(gpus))
	}
	byUUID := map[string]GPU{}
	for _, g := range gpus {
		byUUID[g.UUID] = g
	}
	util := byUUID["GPU-1111aaaa-2222-3333-4444-555566667777"]
	if util.Name != "NVIDIA GeForce RTX 5060 Ti" || util.UsedMiB != 721 || util.TotalMiB != 16311 {
		t.Errorf("utility card parsed as %+v", util)
	}
	if util.Role != "utility-card" || util.Label() != "utility-card" {
		t.Errorf("role label = %q/%q", util.Role, util.Label())
	}
	fast := byUUID["GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000"]
	if fast.UsedMiB != 1607 || fast.Role != "fast-card" {
		t.Errorf("fast card parsed as %+v", fast)
	}
}

func TestLabelFallsBackToNameAndShortUUID(t *testing.T) {
	gpus, err := parseGPUCSV(smiFixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range gpus {
		if g.Role != "" {
			t.Fatalf("unconfigured role should stay empty, got %q", g.Role)
		}
		want := g.Name + " (" + ShortUUID(g.UUID) + ")"
		if g.Label() != want {
			t.Errorf("Label() = %q, want %q", g.Label(), want)
		}
	}
	if got := ShortUUID("GPU-1111aaaa-2222-3333-4444-555566667777"); got != "1111aaaa" {
		t.Errorf("ShortUUID = %q", got)
	}
}

func TestDeltasReportAllThreeQuantities(t *testing.T) {
	before, err := parseGPUCSV(smiFixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	after := make([]GPU, len(before))
	copy(after, before)
	after[0].UsedMiB += 2500
	ds := Deltas(before, after)
	if len(ds) != 2 {
		t.Fatalf("got %d deltas, want 2", len(ds))
	}
	var moved Delta
	for _, d := range ds {
		if d.UUID == after[0].UUID {
			moved = d
		}
	}
	if moved.DeltaMiB != 2500 {
		t.Errorf("delta = %d, want 2500", moved.DeltaMiB)
	}
	if moved.BaselineMiB == 0 || moved.AfterMiB == 0 {
		t.Errorf("baseline/after must both be reported, got %+v", moved)
	}
	if moved.AfterMiB-moved.BaselineMiB != moved.DeltaMiB {
		t.Errorf("delta is not after-baseline: %+v", moved)
	}
	if TotalDelta(ds) != 2500 {
		t.Errorf("TotalDelta = %d, want 2500", TotalDelta(ds))
	}
}

// A card that appears in only one sample is dropped, never reported with an
// invented endpoint.
func TestDeltasDropUnpairedCards(t *testing.T) {
	before, _ := parseGPUCSV(smiFixture, nil)
	after := []GPU{before[0], {UUID: "GPU-new", UsedMiB: 500, TotalMiB: 8000}}
	ds := Deltas(before, after)
	if len(ds) != 1 || ds[0].UUID != before[0].UUID {
		t.Fatalf("unpaired card leaked into deltas: %+v", ds)
	}
}

func TestParseGPUCSVRejectsEmpty(t *testing.T) {
	if _, err := parseGPUCSV("", nil); err == nil {
		t.Fatal("expected an error for empty nvidia-smi output")
	}
}
