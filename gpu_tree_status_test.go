package main

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// `gpu status` on a tree-orphan says the wrapper is gone, what still runs, why the claim is kept
// and the command that frees it; a lease with a wrapper says none of it (plan P10).
func TestStatusSaysTheWrapperIsGoneAndWhatStillRuns(t *testing.T) {
	h := &gpuactivity.Holder{Epoch: 41, PID: 5555, OwnerState: "unknown", TreeOrphan: true, TreePIDs: []int{9001, 9002},
		TreeWhy: "a process the wrapper recorded is still running (pid 9001, pid 9002)"}
	got := strings.Join(ownershipStatusLines(h, gpulease.Info{Epochs: []uint64{41}}), "\n")
	for _, want := range []string{
		"wrapper: pid 5555 is GONE", "TREE-ORPHAN", "pid 9001", "pid 9002",
		"the claim is kept until they exit", "a process the wrapper recorded is still running",
		"gpu release --epoch 41", "the rest of the job keeps running",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q:\n%s", want, got)
		}
	}
	healthy := &gpuactivity.Holder{Epoch: 41, PID: 5555, Alive: true, OwnerState: "alive"}
	if got := strings.Join(ownershipStatusLines(healthy, gpulease.Info{Epochs: []uint64{41}}), "\n"); strings.Contains(got, "wrapper:") || strings.Contains(got, "TREE-ORPHAN") {
		t.Errorf("a lease with a live wrapper says nothing about one:\n%s", got)
	}
}
