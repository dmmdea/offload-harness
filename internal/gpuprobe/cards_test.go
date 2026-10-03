package gpuprobe

import (
	"strings"
	"testing"
)

// Synthetic three-card box: two same-model cards and one faster display card. The
// UUIDs are invented; the shape (index 1 is the display card) is what matters.
func syntheticDevices() []Device {
	return []Device{
		{Index: 0, UUID: "GPU-aaaa0000-0000-0000-0000-000000000001", Name: "Test Card A", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
		{Index: 1, UUID: "GPU-bbbb0000-0000-0000-0000-000000000002", Name: "Test Card B", TotalGiB: 16, FreeGiB: 12, UtilKnown: true, UtilPct: 3, DisplayActive: true},
		{Index: 2, UUID: "GPU-cccc0000-0000-0000-0000-000000000003", Name: "Test Card A", TotalGiB: 16, FreeGiB: 16, UtilKnown: true},
	}
}

func TestBuildCardsMarksDisplayByTheSharedRule(t *testing.T) {
	cards, _ := BuildCards(syntheticDevices(), "")
	if len(cards) != 3 {
		t.Fatalf("want 3 cards, got %d", len(cards))
	}
	if cards[0].Display || !cards[1].Display || cards[2].Display {
		t.Fatalf("display flags: %+v", cards)
	}
	if cards[1].NvidiaIndex != 1 || cards[1].VRAMTotalGiB != 16 || cards[1].Name != "Test Card B" {
		t.Fatalf("fields not carried: %+v", cards[1])
	}
	if got := cards[0].LeaseID(); got != "gpu-aaaa0000-0000-0000-0000-000000000001" {
		t.Fatalf("lease id must be the lower-cased uuid, got %q", got)
	}
}

// A box whose only card is its display card still has a work card: DisplayCardUUIDs
// excludes nothing there, and the card table must agree with it.
func TestBuildCardsSingleDisplayCardIsNotExcluded(t *testing.T) {
	d := syntheticDevices()[1:2]
	cards, _ := BuildCards(d, "")
	if cards[0].Display {
		t.Fatal("a box with only a display card must not exclude it")
	}
	if cards[0].ComfyOrder != 0 {
		t.Fatalf("one card has one possible order, got %d", cards[0].ComfyOrder)
	}
}

// nvidia-smi gives no FASTEST_FIRST order. Without an operator-supplied order the
// table says so (-1) rather than guessing; with one it is exact.
func TestBuildCardsComfyOrderIsUnknownUnlessGiven(t *testing.T) {
	cards, warn := BuildCards(syntheticDevices(), "")
	if warn != "" {
		t.Fatalf("no spec is not a warning: %q", warn)
	}
	for _, c := range cards {
		if c.ComfyOrder != -1 {
			t.Fatalf("order must be unknown (-1) without a spec, card %d has %d", c.NvidiaIndex, c.ComfyOrder)
		}
	}
	cards, warn = BuildCards(syntheticDevices(), "1, 2 ,gpu-aaaa0000")
	if warn != "" {
		t.Fatalf("a full spec must parse: %q", warn)
	}
	want := map[int]int{1: 0, 2: 1, 0: 2} // nvidia index -> comfy order
	for _, c := range cards {
		if c.ComfyOrder != want[c.NvidiaIndex] {
			t.Fatalf("card %d: comfy order %d, want %d", c.NvidiaIndex, c.ComfyOrder, want[c.NvidiaIndex])
		}
	}
}

func TestBuildCardsBadComfyOrderSpecWarnsAndLeavesAllUnknown(t *testing.T) {
	for _, spec := range []string{"0,0,1", "0,1", "0,1,9", "0,1,GPU-zzzz"} {
		cards, warn := BuildCards(syntheticDevices(), spec)
		if warn == "" {
			t.Errorf("spec %q must warn (it does not name every card exactly once)", spec)
		}
		for _, c := range cards {
			if c.ComfyOrder != -1 {
				t.Errorf("spec %q: card %d got order %d from a bad spec", spec, c.NvidiaIndex, c.ComfyOrder)
			}
		}
	}
}

func TestResolveCardsByIndexOrUUIDPrefix(t *testing.T) {
	cards, _ := BuildCards(syntheticDevices(), "")
	got, err := ResolveCards(cards, []string{"2", "GPU-aaaa", "bbbb0000"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].NvidiaIndex != 2 || got[1].NvidiaIndex != 0 || got[2].NvidiaIndex != 1 {
		t.Fatalf("resolution order/identity wrong: %+v", got)
	}
	for _, bad := range []string{"7", "GPU-nope", "", "GPU-", "GPU-aa", "bb"} { // a short prefix is refused even when it would match one card
		if _, err := ResolveCards(cards, []string{bad}); err == nil {
			t.Errorf("key %q must not resolve", bad)
		}
	}
	// A prefix that matches several cards is ambiguous, never a silent first match.
	twins, _ := BuildCards([]Device{
		{Index: 0, UUID: "GPU-dddd0000-0000", Name: "T", TotalGiB: 16, FreeGiB: 16},
		{Index: 1, UUID: "GPU-dddd1111-0000", Name: "T", TotalGiB: 16, FreeGiB: 16},
	}, "")
	if _, err := ResolveCards(twins, []string{"gpu-dddd"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("a prefix that matches several cards must be refused as ambiguous, got %v", err)
	}
}

func TestResolveCardsRefusesDuplicates(t *testing.T) {
	cards, _ := BuildCards(syntheticDevices(), "")
	if _, err := ResolveCards(cards, []string{"0", "GPU-aaaa0000"}); err == nil {
		t.Fatal("the same card named twice must be an error, not a silent dedupe")
	}
}

func TestCardByComfyOrder(t *testing.T) {
	cards, _ := BuildCards(syntheticDevices(), "1,2,0")
	c, ok := CardByComfyOrder(cards, 2)
	if !ok || c.NvidiaIndex != 0 {
		t.Fatalf("comfy 2 should be nvidia 0, got %+v %v", c, ok)
	}
	if _, ok := CardByComfyOrder(cards, 3); ok {
		t.Fatal("an order no card carries must not resolve")
	}
	unknown, _ := BuildCards(syntheticDevices(), "")
	if _, ok := CardByComfyOrder(unknown, 0); ok {
		t.Fatal("with the order unknown nothing may resolve")
	}
}
