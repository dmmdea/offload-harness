package gpulease

import (
	"reflect"
	"testing"
)

// The pure half of the consumers' question (plan P4): "does THIS lease sit on the cards
// THIS seat would use?". Everything below is Info and card-table arithmetic: no directory,
// no clock, no process table.

const (
	idCard0 = "gpu-aaaa0000"
	idCard1 = "gpu-bbbb0000" // the display card in allocCards
	idCard2 = "gpu-cccc0000"
)

func mediaOn(epoch uint64, devs ...string) Info {
	return Info{Held: true, Class: ClassMedia, Epoch: epoch, Epochs: []uint64{epoch}, Devices: devs}
}

func TestInfoTouchesDeclaredDevices(t *testing.T) {
	lease := mediaOn(7, idCard2)
	cases := []struct {
		name string
		ids  []string
		want bool
	}{
		{"the held card", []string{idCard2}, true},
		{"another card", []string{idCard0}, false},
		{"a set that includes the held card", []string{idCard0, idCard2}, true},
		{"a set that misses it", []string{idCard0, idCard1}, false},
		// An unknown pin is every card: the answer that keeps today's behaviour.
		{"unknown pin (nil) is every card", nil, true},
	}
	for _, c := range cases {
		if got := lease.Touches(c.ids); got != c.want {
			t.Errorf("%s: Touches(%v) = %v, want %v", c.name, c.ids, got, c.want)
		}
	}
	if (Info{}).Touches([]string{idCard2}) {
		t.Error("an Info that holds nothing touches nothing")
	}
}

func TestWholeNodeLeaseTouchesEveryCard(t *testing.T) {
	whole := Info{Held: true, Class: ClassMedia, Epoch: 3, Epochs: []uint64{3}}
	for _, ids := range [][]string{{idCard0}, {idCard1}, {idCard2}, {idCard0, idCard2}, nil} {
		if !whole.Touches(ids) {
			t.Errorf("a whole-node lease must touch %v", ids)
		}
	}
	if got := whole.EffectiveDevices(); len(got) != 0 {
		t.Errorf("a whole-node lease with no inference has no effective device set, got %v", got)
	}
}

func TestInferredScopeIsWhatConsumersRead(t *testing.T) {
	legacy := Info{Held: true, Class: ClassMedia, Epoch: 1205, Epochs: []uint64{1205},
		Inferred: []string{idCard2}, Scope: ScopeInferred}
	if got := legacy.EffectiveDevices(); !reflect.DeepEqual(got, []string{idCard2}) {
		t.Fatalf("EffectiveDevices = %v, want the inferred set", got)
	}
	if !legacy.Touches([]string{idCard2}) || legacy.Touches([]string{idCard0}) {
		t.Fatal("an inferred scope must fence its cards and leave the others free")
	}
	// The declared devices stay the declared devices: the P3 card table and the allocator
	// read Info.Devices and must never see an inferred set as a claim.
	if len(legacy.Devices) != 0 {
		t.Fatalf("inference must not rewrite Info.Devices, got %v", legacy.Devices)
	}
	// A declared set wins over a stale inferred one.
	declared := Info{Held: true, Epoch: 8, Devices: []string{idCard0}, Inferred: []string{idCard2}}
	if got := declared.EffectiveDevices(); !reflect.DeepEqual(got, []string{idCard0}) {
		t.Fatalf("declared devices outrank an inference, got %v", got)
	}
}

func TestInfoForKeepsOnlyTouchingLeases(t *testing.T) {
	a, b := mediaOn(7, idCard2), mediaOn(9, idCard0)
	both := a
	both.Epochs = []uint64{7, 9}
	both.Leases = []Info{a, b}

	got := both.For([]string{idCard0})
	if !got.Held || got.Epoch != 9 || len(got.Leases) != 0 {
		t.Fatalf("For(card0) = %+v, want the card-0 lease alone", got)
	}
	got = both.For([]string{idCard1})
	if got.Held {
		t.Fatalf("For(card1) = %+v, want nothing: no live lease sits on that card", got)
	}
	got = both.For([]string{idCard0, idCard2})
	if len(got.Each()) != 2 || got.Epoch != 7 {
		t.Fatalf("For(card0,card2) = %+v, want both leases, lowest epoch first", got)
	}
	if !got.HoldsEpoch(7) || !got.HoldsEpoch(9) {
		t.Fatalf("a narrowed Info must still answer HoldsEpoch for the leases it kept: %+v", got)
	}
	// Unknown pin: nothing is narrowed away.
	if same := both.For(nil); !reflect.DeepEqual(same, both) {
		t.Fatalf("For(nil) must be the identity, got %+v", same)
	}
	if got := mediaOn(7, idCard2).For([]string{idCard0}); got.Held {
		t.Fatalf("a single lease on another card narrows to nothing, got %+v", got)
	}
	// An unknown pin leaves even a single lease's Info exactly as it was (its Epochs list
	// still names every live epoch): nothing is rebuilt on a doubt.
	single := mediaOn(7, idCard2)
	single.Epochs = []uint64{7, 9}
	if same := single.For(nil); !reflect.DeepEqual(same, single) {
		t.Fatalf("For(nil) must not rebuild a single lease's Info, got %+v want %+v", same, single)
	}
}

func TestResolvePinsSpeaksPCIOrderAndUUIDs(t *testing.T) {
	cards := allocCards()
	ids, ok := ResolvePins([]string{"0", "2"}, cards)
	if !ok || !reflect.DeepEqual(ids, []string{idCard0, idCard2}) {
		t.Fatalf("pins 0,2 = %v, %v; want card0,card2 (the tier's gpu_env is PCI order)", ids, ok)
	}
	// A display device pinned by UUID prefix resolves too.
	ids, ok = ResolvePins([]string{"GPU-bbbb"}, cards)
	if !ok || !reflect.DeepEqual(ids, []string{idCard1}) {
		t.Fatalf("UUID prefix pin = %v, %v", ids, ok)
	}
	// Anything that cannot be turned into cards is UNKNOWN, never a guess and never empty-
	// means-nothing: the caller reads it as every card.
	for name, pins := range map[string][]string{
		"no pin":             nil,
		"an index no card":   {"7"},
		"an ambiguous key":   {"GPU-"},
		"a card named twice": {"0", "0"},
		"a blank entry":      {"0", ""},
	} {
		if ids, ok := ResolvePins(pins, cards); ok || len(ids) != 0 {
			t.Errorf("%s: ResolvePins(%v) = %v, %v; want unresolved", name, pins, ids, ok)
		}
	}
	if ids, ok := ResolvePins([]string{"0"}, nil); ok || len(ids) != 0 {
		t.Errorf("with no card table nothing resolves, got %v %v", ids, ok)
	}
}

func TestWithLegacyNoteMarksOnlyLegacyWholeNodeLeases(t *testing.T) {
	legacy := Info{Held: true, Class: ClassMedia, Epoch: 5, Epochs: []uint64{5}, Legacy: true}
	stamped := Info{Held: true, Class: ClassMedia, Epoch: 6, Epochs: []uint64{6}}
	declared := Info{Held: true, Class: ClassMedia, Epoch: 7, Epochs: []uint64{7}, Devices: []string{idCard0}, Scope: ScopeDeclared}

	if got := legacy.WithLegacyNote("off"); got.ScopeWhy != "off" || got.ScopeKind() != ScopeWholeNode {
		t.Fatalf("a legacy lease carries the note and stays whole-node, got %q %s", got.ScopeWhy, got.ScopeKind())
	}
	if got := stamped.WithLegacyNote("off"); got.ScopeWhy != "" {
		t.Fatalf("a record this binary wrote is not noted, got %q", got.ScopeWhy)
	}
	if got := declared.WithLegacyNote("off"); got.ScopeWhy != "" || got.ScopeKind() != ScopeDeclared {
		t.Fatalf("a declared lease is not noted, got %q", got.ScopeWhy)
	}

	several := Info{Held: true, Epoch: 5, Epochs: []uint64{5, 6, 7}, Legacy: true, Leases: []Info{legacy, stamped, declared}}
	got := several.WithLegacyNote("off")
	if len(got.Leases) != 3 || got.Leases[0].ScopeWhy != "off" || got.Leases[1].ScopeWhy != "" || got.Leases[2].ScopeWhy != "" {
		t.Fatalf("only the legacy member of several is noted, got %+v", got.Leases)
	}
	if !reflect.DeepEqual(got.Epochs, []uint64{5, 6, 7}) {
		t.Fatalf("the epochs are kept, got %v", got.Epochs)
	}
}
