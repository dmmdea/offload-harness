package pipeline

// F58 (a): askRAM, the host RAM a call declares against its cards, was sized on the LARGEST card of the whole table
// (ids = nil) even when the call's plan names its cards (a pin, a pool, a declared device). On a box whose cards differ
// that disagrees with the lease the call then takes, which declares on THESE cards: a krea2 render pinned to a 16 GiB
// card declares its 32.8 GiB of weights, while sized against a 48 GiB card on the same box it declared 0. askRAM
// decides whether a waiter that waits only on host RAM holds its cards against the call (a call that declares nothing
// passes it, G6), so the admission's pre-filter and the lane probe both counted the wrong callers ahead. A plan that
// does not name its cards (the whole node, an allocator pick) keeps the largest card: the smallest answer, which can
// only err toward "nothing in the way".

import (
	"context"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// mixedCards is a 16 GiB card A, the 16 GiB screen's card B and a 48 GiB card C.
func mixedCards() []gpuprobe.Card {
	devs := []gpuprobe.Device{
		{Index: 0, UUID: admitUUIDA, Name: "A", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
		{Index: 1, UUID: admitUUIDB, Name: "B", TotalGiB: 16, FreeGiB: 14, UtilKnown: true, DisplayAttached: true},
		{Index: 2, UUID: admitUUIDC, Name: "C", TotalGiB: 48, FreeGiB: 47, UtilKnown: true},
	}
	cards, _ := gpuprobe.BuildCards(devs, admitOrder)
	return cards
}

func TestAskRAMIsSizedOnTheCardsAPlanNames(t *testing.T) {
	cfg := config.Default()
	cfg.ComfyDir = t.TempDir() // no model files here: the documented per-family sizes stand in, whatever this machine has installed
	krea2Binding(&cfg)
	need := imageNeed(cfg, "")
	p := &Pipeline{cfg: cfg}
	cards := mixedCards()
	A, C := leaseIDOf(admitUUIDA), leaseIDOf(admitUUIDC)
	ctx := context.Background()

	cases := []struct {
		name string
		plan mediaPlan
		want float64
	}{
		{"pinned to the 16 GiB card: its weights do not fit it, so it declares them", mediaPlan{ids: []string{A}}, 32.8},
		{"pinned to the 48 GiB card: they fit, so it declares nothing", mediaPlan{ids: []string{C}}, 0},
		{"a pool of both: the lease declares on the largest of THEM, which fits", mediaPlan{ids: []string{A, C}}, 0},
		{"the allocator chooses: the smallest answer, the largest card on the box", mediaPlan{auto: true}, 0},
		{"the whole node: the same smallest answer", mediaPlan{whole: true}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := p.askRAMFor(ctx, need, tc.plan, cards)
			if got != tc.want {
				t.Fatalf("askRAMFor = %.1f GiB, want %.1f", got, tc.want)
			}
		})
	}
}

// The lane probe uses it: a call pinned to the 16 GiB card is held back by a waiter that waits only on host RAM for
// that card, because the call declares RAM too. Sized on the 48 GiB card it declared none and counted nobody ahead.
func TestTheLaneProbeCountsAHostRAMWaiterAheadOfACallPinnedToASmallCard(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinAKrea2})
	mixed := mixedCards()
	f.p.alloc.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { return mixed, "", nil }
	startHostRAMWaiter(t, f, admitUUIDA)

	v := f.p.MediaLaneFree(context.Background(), laneReq(nil))
	if v.Free {
		t.Fatalf("a krea2 render pinned to the 16 GiB card cannot be served ahead of the waiter for it: %+v", v)
	}
	if v.Ahead != 1 {
		t.Errorf("the waiter that waits only on host RAM holds its card against a call that declares RAM, so it is ahead: Ahead=%d (%+v)", v.Ahead, v)
	}
}
