package main

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// heldHint adds "--ram-commit <GiB> states the commit (lower it ...)" to a host-RAM refusal only when the refusal rests on a commit
// the operator STATED. A commit nobody stated is the --ram figure held to physical RAM, and telling the operator to lower a commit
// they never gave sends them to a flag that does not change the outcome.
func TestTheHostRAMRefusalOffersToLowerTheCommitOnlyWhenOneWasStated(t *testing.T) {
	const hint = "--ram-commit <GiB> states the commit"
	for _, tc := range []struct {
		name     string
		check    gpuprobe.HostRAMCheck
		wantHint bool
	}{
		{"a commit nobody stated, refused on the commit", gpuprobe.HostRAMCheck{CommitShort: true, CommitStated: false}, false},
		{"a stated commit, refused on the commit", gpuprobe.HostRAMCheck{CommitShort: true, CommitStated: true}, true},
		{"a stated commit, refused on the working set alone", gpuprobe.HostRAMCheck{TouchShort: true, CommitStated: true}, false},
		{"a stated commit that can never fit", gpuprobe.HostRAMCheck{CommitShort: true, CommitStated: true, Impossible: true}, false},
	} {
		tc.check.Why = "waiting for host RAM: the sentence of the condition that refused"
		for _, wait := range []time.Duration{0, 5 * time.Second} {
			got := heldHint(&gpulease.ErrHostRAM{HostRAMCheck: tc.check}, wait).Error()
			if has := strings.Contains(got, hint); has != tc.wantHint {
				t.Errorf("%s, --wait %s: hint present = %v, want %v: %s", tc.name, wait, has, tc.wantHint, got)
			}
		}
	}
}
