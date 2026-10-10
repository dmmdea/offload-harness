package gpulease

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// allocCards is a synthetic 3-card box: index 1 is the display card.
func allocCards() []gpuprobe.Card {
	return []gpuprobe.Card{
		{UUID: "GPU-aaaa0000", NvidiaIndex: 0, Name: "T", VRAMTotalGiB: 16, VRAMFreeGiB: 15.5, ComfyOrder: -1},
		{UUID: "GPU-bbbb0000", NvidiaIndex: 1, Name: "T", VRAMTotalGiB: 16, VRAMFreeGiB: 12, Display: true, ComfyOrder: -1},
		{UUID: "GPU-cccc0000", NvidiaIndex: 2, Name: "T", VRAMTotalGiB: 16, VRAMFreeGiB: 15.9, ComfyOrder: -1},
	}
}

// roomyHost is a host with room for anything these tests ask of it: 256 GiB physical, 40 committed.
var roomyHost = gpuprobe.HostMemory{PhysicalGiB: 256, AvailableGiB: 200, CommitUsedGiB: 40, CommitLimitGiB: 400}

func baseInput() AllocInput {
	return AllocInput{Cards: allocCards(), Min: 1, Max: 1, HostMemOK: true, HostMem: roomyHost}
}

func skipReason(a Allocation, id string) string {
	for _, s := range a.Skipped {
		if s.ID == id {
			return s.Reason
		}
	}
	return ""
}

func TestAllocatorSkipsDisplayClaimedForeignAndQuarantined(t *testing.T) {
	in := baseInput()
	in.Min, in.Max = 1, 3
	in.Claimed = map[string]bool{"gpu-aaaa0000": true}
	in.Quarantined = map[string]bool{"gpu-cccc0000": true}
	in.ForeignBusy = map[string]string{"gpu-cccc0000": "a foreign process"}
	_, err := Allocate(in)
	var none *NoCardsError
	if !errors.As(err, &none) {
		t.Fatalf("every card is unavailable for a different reason: want NoCardsError, got %v", err)
	}
	want := map[string]string{
		"gpu-aaaa0000": ReasonClaimed,
		"gpu-bbbb0000": ReasonDisplay,
		"gpu-cccc0000": ReasonQuarantined, // quarantine outranks foreign-busy: it is the stronger, durable fact
	}
	for id, reason := range want {
		var got string
		for _, s := range none.Skipped {
			if s.ID == id {
				got = s.Reason
			}
		}
		if got != reason {
			t.Errorf("card %s: skipped as %q, want %q (all: %+v)", id, got, reason, none.Skipped)
		}
	}
	// A foreign-busy card on its own is reported as such, and is never taken.
	in2 := baseInput()
	in2.Min, in2.Max = 1, 3
	in2.ForeignBusy = map[string]string{"gpu-cccc0000": "pid 4242"}
	a, err := Allocate(in2)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Devices) != 1 || a.Devices[0] != "gpu-aaaa0000" {
		t.Fatalf("only card 0 is allocatable (1 display, 2 foreign-busy), got %v", a.Devices)
	}
	if r := skipReason(a, "gpu-cccc0000"); r != ReasonForeignBusy {
		t.Errorf("foreign card reason %q", r)
	}
	if r := skipReason(a, "gpu-bbbb0000"); r != ReasonDisplay {
		t.Errorf("display card reason %q", r)
	}
}

func TestAllocatorDisplayCardIsAllocatableOnlyWhenTheCallerSaysSo(t *testing.T) {
	in := baseInput()
	in.Min, in.Max = 3, 3
	in.Claimed = nil
	if _, err := Allocate(in); err == nil {
		t.Fatal("the display card must never be auto-assigned by default (I6)")
	}
	in.AllowDisplay = true
	a, err := Allocate(in)
	if err != nil || len(a.Devices) != 3 {
		t.Fatalf("with the operator away all three are allocatable: %v %v", a.Devices, err)
	}
}

func TestAllocatorSkipsWhenVRAMDoesNotFit(t *testing.T) {
	in := baseInput()
	in.Min, in.Max = 1, 3
	in.FootprintGiB = 15.8
	a, err := Allocate(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Devices) != 1 || a.Devices[0] != "gpu-cccc0000" {
		t.Fatalf("only card 2 has 15.8 GiB free, got %v", a.Devices)
	}
	if r := skipReason(a, "gpu-aaaa0000"); r != ReasonVRAM {
		t.Errorf("card 0 reason %q", r)
	}
}

func TestAllocatorSkipsWhenHostRamHeadroomLow(t *testing.T) {
	in := baseInput()
	in.HostMem = gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 10, CommitUsedGiB: 90, CommitLimitGiB: 160}
	in.HostNeedGiB = 8
	in.HostHeadroomGiB = 4 // 90 + 8 = 98 > 100 - 4
	_, err := Allocate(in)
	var none *NoCardsError
	if !errors.As(err, &none) {
		t.Fatalf("host RAM headroom is low: want NoCardsError, got %v", err)
	}
	if none.HostReason == "" || !strings.Contains(none.HostReason, "host RAM") || none.HostImpossible {
		t.Fatalf("the refusal must name host RAM and be a wait, got %q impossible=%v", none.HostReason, none.HostImpossible)
	}
	// The cards are free and only the host is short, so waiting can help: the cards stay listed.
	if len(none.Waitable) == 0 {
		t.Fatal("a host shortage must leave the cards waitable")
	}
	// Plenty of RAM: the same request allocates.
	in.HostMem.CommitUsedGiB = 40
	if a, err := Allocate(in); err != nil || len(a.Devices) != 1 {
		t.Fatalf("with RAM to spare: %v %v", a.Devices, err)
	}
	// The part of leases already granted that has not loaded counts, as it does at the grant.
	in.HostPendingGiB = 60
	if _, err := Allocate(in); err == nil {
		t.Fatal("what leases already granted have yet to load must count against the host")
	}
	in.HostPendingGiB = 0
	// A need no state of this host admits is impossible: no card makes it fit.
	in.HostNeedGiB = 99
	_, err = Allocate(in)
	if !errors.As(err, &none) || !none.HostImpossible {
		t.Fatalf("a need above physical RAM less the headroom is impossible, got %v", err)
	}
	in.HostNeedGiB = 8
	// Unknown host memory with a declared need fails closed (on a platform with a reader).
	if gpuprobe.HostMemorySupported {
		in.HostMemOK = false
		if _, err := Allocate(in); err == nil {
			t.Fatal("an unreadable host memory reading with a declared need must refuse")
		}
	}
	// A job that declares no need is not turned away by a host that is over.
	in.HostMemOK, in.HostNeedGiB = true, 0
	in.HostMem.CommitUsedGiB = 400
	if a, err := Allocate(in); err != nil || len(a.Devices) != 1 {
		t.Fatalf("a job that declares no host RAM adds none, got %v %v", a.Devices, err)
	}
}

func TestAllocatorPrefersCardWithNoResidentSeat(t *testing.T) {
	in := baseInput()
	in.Min, in.Max = 1, 1
	in.AllowDisplay = true
	in.Resident = map[string]ResidentInfo{
		"gpu-aaaa0000": {Seats: []string{"seat-a"}, CostGiB: 10},
		"gpu-bbbb0000": {Seats: []string{"seat-b"}, CostGiB: 3},
	}
	a, err := Allocate(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Devices) != 1 || a.Devices[0] != "gpu-cccc0000" {
		t.Fatalf("the only card with no resident seat is card 2, got %v", a.Devices)
	}
	// Both residents: the cheaper eviction goes first, then the lowest id.
	in.Resident["gpu-cccc0000"] = ResidentInfo{Seats: []string{"seat-c"}, CostGiB: 12}
	a, _ = Allocate(in)
	if a.Devices[0] != "gpu-bbbb0000" {
		t.Fatalf("least eviction cost is card 1 (3 GiB), got %v", a.Devices)
	}
	in.Resident["gpu-aaaa0000"] = ResidentInfo{Seats: []string{"seat-a"}, CostGiB: 3}
	a, _ = Allocate(in)
	if a.Devices[0] != "gpu-aaaa0000" {
		t.Fatalf("equal cost breaks on the lowest id, got %v", a.Devices)
	}
}

func TestAllocatorTakesBetweenMinAndMax(t *testing.T) {
	in := baseInput()
	in.AllowDisplay = true
	in.Min, in.Max = 2, 3
	in.Claimed = map[string]bool{"gpu-bbbb0000": true}
	a, err := Allocate(in)
	if err != nil || len(a.Devices) != 2 {
		t.Fatalf("2 free cards satisfy 2..3: %v %v", a.Devices, err)
	}
	in.Min = 3
	if _, err := Allocate(in); err == nil {
		t.Fatal("min 3 with one card claimed must not allocate")
	}
}

func TestAllocatorRefusesABadRequest(t *testing.T) {
	for _, c := range []struct{ min, max int }{{0, 1}, {2, 1}, {-1, 1}} {
		in := baseInput()
		in.Min, in.Max = c.min, c.max
		if _, err := Allocate(in); err == nil || errors.As(err, new(*NoCardsError)) {
			t.Errorf("min %d max %d must be a request error, not NoCards: %v", c.min, c.max, err)
		}
	}
}

func TestWholeNodeLeaseClaimsEveryCard(t *testing.T) {
	in := baseInput()
	in.WholeNodeHeld = true
	_, err := Allocate(in)
	var none *NoCardsError
	if !errors.As(err, &none) {
		t.Fatalf("a whole-node lease holds every card: %v", err)
	}
	for _, s := range none.Skipped {
		if s.ID != "gpu-bbbb0000" && s.Reason != ReasonClaimed {
			t.Errorf("card %s: %q", s.ID, s.Reason)
		}
	}
}

// quarantine.<id> is the sidecar plan P12 writes for a card whose old tree could not be
// killed. P3 only READS it, so the allocator already skips such a card.
func TestQuarantinedCardsReadsTheSidecars(t *testing.T) {
	m, _ := newTestManager(t)
	if got := m.QuarantinedCards(); len(got) != 0 {
		t.Fatalf("nothing quarantined yet: %v", got)
	}
	if err := os.MkdirAll(m.leaseDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"quarantine.gpu-cccc0000", "quarantine.GPU-DDDD0000", "meta.json", "quarantine."} {
		if err := os.WriteFile(filepath.Join(m.leaseDir(), n), []byte("{}"), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	got := m.QuarantinedCards()
	if !got["gpu-cccc0000"] || !got["gpu-dddd0000"] || len(got) != 2 {
		t.Fatalf("want the two named cards, lower-cased: %v", got)
	}
}
