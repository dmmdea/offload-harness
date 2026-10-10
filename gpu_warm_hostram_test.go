package main

// G4 of the P0 plan: the warm-back of a seat is a load, and it is admitted like one.
//
// A media lane that takes a card unloads the agent seat; the wrapper warms it back before it releases the lease.
// That reload put the seat's resident set (a 35B-class MoE with spilled experts, a vLLM pair seat with staged KV
// cache) onto a host nothing had asked: the warm path had no host-RAM check, and the seat's footprint was declared
// nowhere (0 for text and seat leases). Now the warm puts the seat's host footprint (agent_seat_host_ram_gib, else
// the fail-closed hostneed.DefaultSeatHostGiB) to the same admission a lease grant applies, over every lease but the
// one being released, and never runs while another lane that declared host RAM is live. A refused warm stays owed:
// a cold seat costs one on-demand load, an overloaded host costs every caller.

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// useWarmHost stands a host in: 100 GiB physical, the given commit, the default 8 GiB headroom.
func useWarmHost(t *testing.T, commit float64) {
	t.Helper()
	restore := gpuprobe.UseHostMemoryReader(func() (gpuprobe.HostMemory, bool) {
		return gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 100 - commit/2, CommitUsedGiB: commit, CommitLimitGiB: 160}, true
	})
	t.Cleanup(restore)
}

// statesSeatFootprint adds agent_seat_host_ram_gib to the fixture's config file.
func statesSeatFootprint(t *testing.T, cfgPath string, gib float64) {
	t.Helper()
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.TrimSuffix(strings.TrimSpace(string(b)), "}") + `, "agent_seat_host_ram_gib": ` + strconv.FormatFloat(gib, 'f', -1, 64) + `}`
	if err := os.WriteFile(cfgPath, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pinSeatToCardA makes the seat sit on card A alone, so a lease on card C is "off the seat's cards" and the
// other-lease rule has nothing to say: what is asked in the tests below is the host rule.
func pinSeatToCardA(t *testing.T) {
	t.Helper()
	modelaffinity.SetSeatPins(func(model string) ([]string, bool) {
		if model == "seat" {
			return []string{"gpu-aaaa0000"}, true
		}
		return nil, false
	})
	t.Cleanup(func() { modelaffinity.SetSeatPins(nil) })
	t.Cleanup(modelaffinity.SetCardTableReader(func(context.Context, string) ([]gpuprobe.Card, string, error) {
		return statusCards(), "", nil
	}))
}

// A seat nobody sized is not assumed small: the fail-closed default (21 GiB, never 0) is put to the host, and a
// host that cannot take it keeps the seat cold, says why with the grant's own numbers, and leaves the warm owed.
func TestAWarmBackIsRefusedWhenTheHostCannotTakeTheSeatAndStaysOwed(t *testing.T) {
	cfgPath, m, warms := warmCardFixture(t)
	useWarmHost(t, 85) // 85 + 21 > 100 - 8
	a := acquireCard(t, m, "lane", "gpu-aaaa0000-x")
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, a), &out)
	if n := warms.Load(); n != 0 {
		t.Fatalf("the seat was loaded onto a host that cannot take it (%d warm(s)): %s", n, out.String())
	}
	if m.SeatWarmOwed() != "seat" {
		t.Errorf("a refused warm stays owed, owed=%q", m.SeatWarmOwed())
	}
	for _, want := range []string{"NOT warming seat back", "the host cannot take the seat's 21.0 GiB (seat default)", "committed 85.0 of 100.0 GiB physical", "the warm stays owed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the refusal must say %q:\n%s", want, out.String())
		}
	}
}

// The same seat on a host with room is warmed, and the debt is paid.
func TestAWarmBackIsAdmittedWhenTheHostHasRoomForTheSeat(t *testing.T) {
	cfgPath, m, warms := warmCardFixture(t)
	useWarmHost(t, 40)
	a := acquireCard(t, m, "lane", "gpu-aaaa0000-x")
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, a), &out)
	if n := warms.Load(); n != 1 || m.SeatWarmOwed() != "" {
		t.Fatalf("a host with room warms the seat: warms=%d owed=%q: %s", n, m.SeatWarmOwed(), out.String())
	}
}

// A node that measured its seat states it, and the figure replaces the default in both directions.
func TestAWarmBackUsesTheNodesOwnSeatFootprint(t *testing.T) {
	for _, tc := range []struct {
		name      string
		gib       float64
		wantWarms int32
	}{
		{"a small seat fits where the default would not", 2, 1}, // 85 + 2 = 87 <= 92
		{"a big seat does not fit where a smaller one would", 12, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, m, warms := warmCardFixture(t)
			statesSeatFootprint(t, cfgPath, tc.gib)
			useWarmHost(t, 85)
			a := acquireCard(t, m, "lane", "gpu-aaaa0000-x")
			if err := m.MarkSeatWarmOwed("seat"); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, a), &out)
			if n := warms.Load(); n != tc.wantWarms {
				t.Fatalf("warms = %d, want %d: %s", n, tc.wantWarms, out.String())
			}
			if tc.wantWarms == 0 && !strings.Contains(out.String(), "the host cannot take the seat's "+strconv.FormatFloat(tc.gib, 'f', 1, 64)+" GiB (seat)") {
				t.Errorf("the refusal names the configured footprint:\n%s", out.String())
			}
		})
	}
}

// The lease being released is left out of the admission: its command has exited and it loads nothing more, so its
// declared need (40 GiB here, with none of it held by any process) must not count as still to come, or the warm its
// own release makes room for would be refused by the lane that just finished.
func TestAWarmBackDoesNotCountTheLeaseItIsReleasing(t *testing.T) {
	cfgPath, m, warms := warmCardFixture(t)
	useWarmHost(t, 40)
	a, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "the lane that just finished", Devices: []string{"gpu-aaaa0000-x"}, TTL: time.Hour, HostRAMGiB: 40})
	if err != nil {
		t.Fatalf("40 committed + 40 declared fits a 100 GiB host: %v", err)
	}
	t.Cleanup(func() { _ = a.Release() })
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, a), &out) // 40 + 21 = 61 <= 92; counting a's 40 would make 101
	if n := warms.Load(); n != 1 {
		t.Fatalf("the finished lane's own declared need was counted against its seat's warm (%d warm(s)): %s", n, out.String())
	}
}

// A seat is not loaded while another lane that declared host RAM is live, whatever the numbers say: the lane's own
// growth is in no counter yet. The lane is on a card the seat does not use, so the other-lease rule is silent and
// this is the rule under test; once the lane is gone the warm runs.
func TestAWarmBackNeverRunsWhileALaneThatDeclaredHostRAMIsLive(t *testing.T) {
	cfgPath, m, warms := warmCardFixture(t)
	useWarmHost(t, 10)          // a roomy host: the numeric admission alone would say yes
	cfg := loadCfgPath(cfgPath) // arms the seat pins from the config; the pins below go on after it
	pinSeatToCardA(t)
	a := acquireCard(t, m, "seat's card", "gpu-aaaa0000-x")
	lane, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "krea2 lane", Devices: []string{"gpu-cccc0000-x"}, TTL: time.Hour, HostRAMGiB: 30})
	if err != nil {
		t.Fatalf("30 declared on a roomy host: %v", err)
	}
	t.Cleanup(func() { _ = lane.Release() })
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warmBackGuarded(cfg, leaseWarmGuard(m, a), &out)
	if n := warms.Load(); n != 0 {
		t.Fatalf("the seat was loaded while a lane that declared host RAM was live (%d warm(s)): %s", n, out.String())
	}
	if !strings.Contains(out.String(), "a lane that declared 30.0 GiB of host RAM is still live") || !strings.Contains(out.String(), "a seat is not loaded while a lane streams") {
		t.Errorf("the refusal must name the lane:\n%s", out.String())
	}
	if m.SeatWarmOwed() != "seat" {
		t.Errorf("the warm stays owed, owed=%q", m.SeatWarmOwed())
	}

	_ = lane.Release()
	out.Reset()
	warmBackGuarded(cfg, leaseWarmGuard(m, a), &out)
	if n := warms.Load(); n != 1 {
		t.Fatalf("once the lane is gone the warm runs (%d warm(s)): %s", n, out.String())
	}
}

// `gpu release --warm-seat` with no epoch is "whatever is held": with one live lease that lease is the one being
// released, and its declared need is left out like any releasing lease's.
func TestReleaseWarmSeatWithNoEpochLeavesOutTheOneLiveLease(t *testing.T) {
	cfgPath, m, warms := warmCardFixture(t)
	useWarmHost(t, 40)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "detached lane", Devices: []string{"gpu-aaaa0000-x"}, TTL: time.Hour, HostRAMGiB: 40})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), releaseWarmGuard(m, 0), &out)
	if n := warms.Load(); n != 1 {
		t.Fatalf("release --warm-seat over the one live lease must warm (%d warm(s)): %s", n, out.String())
	}
}
