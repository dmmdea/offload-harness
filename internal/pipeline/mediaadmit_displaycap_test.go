package pipeline

// The media admission reads the display card's cap through the one input every allocator caller shares
// (gpualloc.BuildInput). Two facts about the door itself are pinned here, without starting a runner:
// it forces nothing on a live table, and it keeps the screen's card closed on a table it could not
// refresh (F24), because the cap is judged on the VRAM in use now and a stale table misstates that.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpualloc"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// pairSeated puts the pair seat on the two non-display cards of the admission fixture, so the display
// card is the one with nothing to evict and sorts first whenever the allocator lets it in.
func pairSeated() map[string]gpulease.ResidentInfo {
	seat := gpulease.ResidentInfo{Seats: []string{"agent-pool"}, CostGiB: 15}
	return map[string]gpulease.ResidentInfo{leaseIDOf(admitUUIDA): seat, leaseIDOf(admitUUIDC): seat}
}

func pickOne(t *testing.T, in gpulease.AllocInput) (gpulease.Allocation, error) {
	t.Helper()
	in.Min, in.Max = 1, 1
	in.Resident = pairSeated()
	return gpulease.Allocate(in)
}

func TestMediaAdmitDoesNotForceTheDisplayCardClosed(t *testing.T) {
	f := newAdmitFixture(t, nil) // the display card holds 2 GiB of 16
	reads := &tableReads{cards: f.cards, at: time.Now()}
	in, err := f.p.mediaAllocInput(context.Background(), f.m, gpualloc.Need{VRAMGiB: 2}, reads)
	if err != nil {
		t.Fatal(err)
	}
	if reads.stood() {
		t.Fatal("setup: the table was read, so the call is not placed from a stored one")
	}
	if in.DisplayCapPct != 60 {
		t.Fatalf("the door must hand the allocator the configured cap (the default 60), got %d", in.DisplayCapPct)
	}
	a, err := pickOne(t, in)
	if err != nil || len(a.Devices) != 1 || a.Devices[0] != leaseIDOf(admitUUIDB) {
		t.Fatalf("a call that declares 2 GiB stays under the cap on the screen's card, which has no seat to evict: %v %v (skipped %+v)", a.Devices, err, a.Skipped)
	}

	// The operator's switch: a cap of 0 in the config closes the card through the same door.
	zero := 0
	g := newAdmitFixture(t, func(c *config.Config) { c.GPUDisplayCapPct = &zero })
	in, err = g.p.mediaAllocInput(context.Background(), g.m, gpualloc.Need{VRAMGiB: 2}, &tableReads{cards: g.cards, at: time.Now()})
	if err != nil || in.DisplayCapPct != 0 {
		t.Fatalf("gpu_display_cap_pct 0 reaches the allocator: cap %d err %v", in.DisplayCapPct, err)
	}
	if a, err = pickOne(t, in); err != nil || a.Devices[0] == leaseIDOf(admitUUIDB) {
		t.Fatalf("with the card closed by the operator the call goes elsewhere: %v %v", a.Devices, err)
	}
}

func TestAPlacementFromAStaleTableKeepsTheDisplayCardClosed(t *testing.T) {
	f := newAdmitFixture(t, nil)
	f.p.alloc.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		return nil, "", errors.New("nvidia-smi: not answering")
	}
	reads := &tableReads{cards: f.cards, at: time.Now()}
	in, err := f.p.mediaAllocInput(context.Background(), f.m, gpualloc.Need{VRAMGiB: 2}, reads)
	if err != nil {
		t.Fatalf("a table that cannot be re-read is not a reason to fail the call: %v", err)
	}
	if !reads.stood() {
		t.Fatal("setup: the re-read failed, so the call goes on from the table it has")
	}
	// The operator's cap is what the config says (the default 60 here): the closure is the table's doing, so
	// the input must not claim the operator closed the card.
	if in.DisplayCapPct != 60 {
		t.Errorf("the configured cap is not changed by a table that could not be re-read, got %d", in.DisplayCapPct)
	}
	a, err := pickOne(t, in)
	if err != nil || a.Devices[0] == leaseIDOf(admitUUIDB) {
		t.Fatalf("the cap is judged on VRAM in use now, which a stale table misstates: the call is placed off the screen's card: %v %v", a.Devices, err)
	}
	var shown *gpulease.Skip
	for i, s := range a.Skipped {
		if s.ID == leaseIDOf(admitUUIDB) && s.Reason == gpulease.ReasonDisplay {
			shown = &a.Skipped[i]
		}
	}
	if shown == nil {
		t.Fatalf("the screen's card is skipped as %q: %+v", gpulease.ReasonDisplay, a.Skipped)
	}
	// The skip names the cause that is true: the table, not a gpu_display_cap_pct of 0 the operator never wrote.
	if strings.Contains(shown.Detail, "gpu_display_cap_pct is 0") || !strings.Contains(shown.Detail, "could not be re-read") {
		t.Errorf("the skip must say the card table could not be re-read, and not that the key is 0: %q", shown.Detail)
	}
}

// The pipeline lives as long as the MCP server and never reloads its config, so the operator's switch
// (gpu_display_cap_pct 0, written after the server started) must reach the media door through the file the
// config was read from: the input every pick and every grant-time check builds carries what the file says now.
func TestTheMediaDoorSeesTheOperatorsSwitchWrittenAfterTheServerStarted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"gpu_display_cap_pct": 60}`)
	f := newAdmitFixture(t, func(c *config.Config) { *c = c.WithSource(path) })
	capNow := func() int {
		t.Helper()
		in, err := f.p.mediaAllocInput(context.Background(), f.m, gpualloc.Need{VRAMGiB: 2}, &tableReads{cards: f.cards, at: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		return in.DisplayCapPct
	}
	if got := capNow(); got != 60 {
		t.Fatalf("the file says 60, got %d", got)
	}
	write(`{"gpu_display_cap_pct": 0}`)
	if got := capNow(); got != 0 {
		t.Fatalf("the operator wrote 0 while the server ran: the next call's input must carry it, got %d", got)
	}
}
