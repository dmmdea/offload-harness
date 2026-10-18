package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// useHostReading installs this reading on BOTH doors of the host-RAM rule: the card allocator's seam (buildAllocInput) and the
// lease grant's (gpuprobe), which must see one host.
func useHostReading(t *testing.T, mem gpuprobe.HostMemory) {
	t.Helper()
	read := func() (gpuprobe.HostMemory, bool) { return mem, true }
	restore := gpuprobe.UseHostMemoryReader(read)
	old := hostMemoryFn
	hostMemoryFn = read
	t.Cleanup(func() { restore(); hostMemoryFn = old })
}

// `--cards` is allocated from the figures the verb resolved, so the commit a reserve STATES has to reach the allocator, or the
// pre-filter judges another figure than the grant. 60 GiB touched of 90 available with the commit stated at 5 fits the commit limit
// on 140 committed of a 160 GiB limit (145 <= 152); the 60 declared once is 200 against the 92 GiB physical line.
func TestReserveCardsHandsTheStatedCommitToTheAllocator(t *testing.T) {
	host := gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 90, CommitUsedGiB: 140, CommitLimitGiB: 160}

	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	useHostReading(t, host)
	err := runGPUReserve(reserveWithCmd(cfg, []string{"--class", "media", "--cards", "1", "--wait", "0", "--ram", "60"}))
	var short *gpulease.ErrHostRAM
	var none *gpulease.NoCardsError
	if err == nil || !(errors.As(err, &short) || errors.As(err, &none)) || !strings.Contains(err.Error(), "committed 140.0 of 100.0 GiB physical") {
		t.Fatalf("--ram 60 states no commit: want the refusal on the physical line, got %v", err)
	}
	if l := m.Leases(); len(l) != 0 {
		t.Fatalf("a refused reserve holds nothing, got %+v", l)
	}

	t.Setenv("LO_HELPER_SLEEP_MS", "0")
	t.Setenv("LO_HELPER_ENV_OUT", t.TempDir()+"/env.txt")
	if err := runGPUReserve(reserveWithCmd(cfg, []string{"--class", "media", "--cards", "1", "--wait", "0", "--ram", "60", "--ram-commit", "5"})); err != nil {
		t.Fatalf("a commit stated at 5 fits the commit limit, so the pre-filter and the grant admit it: %v", err)
	}
}
