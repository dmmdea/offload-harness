package placement

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// Which leases stand between a contract and its seats (plan P7). The delegator reads it for
// the local seat, the fleet node reads it at dispatch, and the delegator reads the remote's
// version of it from health rows: one rule over one chain of seats.

const (
	cardA = "gpu-aaaa0000"
	cardB = "gpu-bbbb0000"
	cardC = "gpu-cccc0000"
)

func threeCards() []gpuprobe.Card {
	return []gpuprobe.Card{
		{UUID: "GPU-AAAA0000", NvidiaIndex: 0, ComfyOrder: -1},
		{UUID: "GPU-BBBB0000", NvidiaIndex: 1, Display: true, ComfyOrder: -1},
		{UUID: "GPU-CCCC0000", NvidiaIndex: 2, ComfyOrder: -1},
	}
}

func armCards(t *testing.T) {
	t.Helper()
	t.Cleanup(modelaffinity.SetCardTableReader(func(context.Context, string) ([]gpuprobe.Card, string, error) {
		return threeCards(), "", nil
	}))
}

func heldOn(epoch uint64, class gpulease.Class, devs ...string) gpulease.Info {
	return gpulease.Info{Held: true, Class: class, Epoch: epoch, Epochs: []uint64{epoch}, Devices: devs}
}

func together(ls ...gpulease.Info) gpulease.Info {
	out := ls[0]
	out.Leases = ls
	out.Epochs = nil
	for _, l := range ls {
		out.Epochs = append(out.Epochs, l.Epoch)
	}
	return out
}

func agentContract() core.AgentContract {
	return core.AgentContract{Goal: "summarize the file", OutputSchema: []byte(`{"properties":{"a":{"type":"string"}}}`)}
}

// The flagship's agent chain is the triple seat (all three cards) and then the single layer's
// agent seat (card 0). A lease holds the contract only when it holds EVERY seat of the chain.
func TestLeasesAgainstContractIsTheChainReadingUnderAPredicate(t *testing.T) {
	armCards(t)
	cfg := config.FlagshipFixture()
	media := heldOn(1, gpulease.ClassMedia, cardC)
	text := heldOn(2, gpulease.ClassText, cardA)
	both := together(media, text)
	c := agentContract()

	// No predicate: every lease counts. The triple seat is held by both, the single seat by the
	// text lease on card 0: the contract is held, and the leases named are the first seat's.
	all := LeasesAgainstContract(cfg, both, c, nil)
	if !all.Held || len(all.Leases) != 2 {
		t.Fatalf("both seats are held by some lease; the first seat's leases are named: %+v", all)
	}

	// Only media leases count: the triple seat is held, the single seat (card 0) is not.
	if got := LeasesAgainstContract(cfg, both, c, func(l gpulease.Info) bool { return l.Class == gpulease.ClassMedia }); got.Held {
		t.Fatalf("the single seat on card 0 is free of every media lease, so nothing holds the contract: %+v", got)
	}

	// Only text leases count: the text lease on card 0 holds the triple seat and the single
	// seat alike, so the contract is held, by that lease alone.
	got := LeasesAgainstContract(cfg, both, c, func(l gpulease.Info) bool { return l.Class == gpulease.ClassText })
	if !got.Held || got.Epoch != 2 || len(got.Leases) != 0 {
		t.Fatalf("a text lease on card 0 holds every seat of the chain: %+v", got)
	}
}

// The reading is whole-node wherever the chain cannot be built: a plain box, a contract that
// asks for the long seat, a contract naming a layer the box does not declare.
func TestLeasesAgainstContractIsWholeNodeWhereNothingCanBeNarrowed(t *testing.T) {
	armCards(t)
	lease := heldOn(1, gpulease.ClassMedia, cardC)
	c := agentContract()
	if got := LeasesAgainstContract(config.Default(), lease, c, nil); !got.Held {
		t.Fatal("a box with no layers has no seats to narrow by: the lease holds the contract")
	}
	long := c
	long.ContextClass = core.ContextClassLong
	if got := LeasesAgainstContract(config.FlagshipFixture(), lease, long, nil); !got.Held {
		t.Fatal("a long-context contract runs on a seat the chain does not describe: nothing narrows on a guess")
	}
	named := c
	named.Layer = "no-such-layer"
	if got := LeasesAgainstContract(config.FlagshipFixture(), lease, named, nil); !got.Held {
		t.Fatal("a layer the box does not declare has no chain: the whole-node reading")
	}
}

// A node publishes each seat's cards as lease ids, resolved where the card table is. The
// delegator cannot resolve a bare index on another box without guessing the index space.
func TestWithDeviceIDsResolvesEverySeatPin(t *testing.T) {
	rows := RowsFromConfig(config.FlagshipFixture(), admitting().live())
	got := WithDeviceIDs(rows, threeCards())
	var triple, single LayerRow
	for _, r := range got {
		switch r.Name {
		case "triple":
			triple = r
		case "single":
			single = r
		}
	}
	if want := []string{cardA, cardB, cardC}; !reflect.DeepEqual(triple.Seats[0].DeviceIDs, want) {
		t.Fatalf("triple agent seat ids = %v, want %v (nvidia index order)", triple.Seats[0].DeviceIDs, want)
	}
	for _, s := range single.Seats {
		switch s.Role {
		case "agent":
			if !reflect.DeepEqual(s.DeviceIDs, []string{cardA}) {
				t.Fatalf("single agent seat (pin 0) ids = %v", s.DeviceIDs)
			}
		case "ocr":
			if !reflect.DeepEqual(s.DeviceIDs, []string{cardC}) {
				t.Fatalf("single ocr seat (pin 2) ids = %v", s.DeviceIDs)
			}
		}
	}
}

// A pin the card table cannot place is left unresolved (unknown is every card), and no card
// table at all leaves the rows exactly as they were.
func TestWithDeviceIDsLeavesUnresolvedPinsAlone(t *testing.T) {
	rows := RowsFromConfig(config.FlagshipFixture(), admitting().live())
	for i := range rows {
		for j := range rows[i].Seats {
			if rows[i].Seats[j].Role == "agent" && rows[i].Name == "single" {
				rows[i].Seats[j].Device = "7" // no such card
			}
		}
	}
	got := WithDeviceIDs(rows, threeCards())
	for _, r := range got {
		if r.Name != "single" {
			continue
		}
		for _, s := range r.Seats {
			if s.Role == "agent" && len(s.DeviceIDs) != 0 {
				t.Fatalf("a pin naming no card must publish no ids: %v", s.DeviceIDs)
			}
		}
	}
	plain := WithDeviceIDs(RowsFromConfig(config.FlagshipFixture(), admitting().live()), nil)
	for _, r := range plain {
		for _, s := range r.Seats {
			if len(s.DeviceIDs) != 0 {
				t.Fatalf("no card table, no ids: %+v", s)
			}
		}
	}
}

// What a delegator derives from another node's health: the cards of each seat in the chain.
func TestRemoteSeatCardsReadsTheChainOffTheRows(t *testing.T) {
	rows := WithDeviceIDs(RowsFromConfig(config.FlagshipFixture(), admitting().live()), threeCards())
	seats, ok := RemoteSeatCards(rows, threeCards(), agentContract(), EstimateTokens(agentContract()))
	if !ok || len(seats) != 2 {
		t.Fatalf("seats = %v ok = %v, want the triple seat then the single layer's agent seat", seats, ok)
	}
	if !reflect.DeepEqual(seats[0], []string{cardA, cardB, cardC}) || !reflect.DeepEqual(seats[1], []string{cardA}) {
		t.Fatalf("chain cards = %v", seats)
	}

	// A named layer is that layer's seat alone.
	named := agentContract()
	named.Layer = "single"
	seats, ok = RemoteSeatCards(rows, threeCards(), named, EstimateTokens(named))
	if !ok || len(seats) != 1 || !reflect.DeepEqual(seats[0], []string{cardA}) {
		t.Fatalf("named layer chain = %v ok = %v", seats, ok)
	}

	// A node that published no ids is resolved against its own card table, then left unknown.
	bare := RowsFromConfig(config.FlagshipFixture(), admitting().live())
	seats, ok = RemoteSeatCards(bare, threeCards(), named, EstimateTokens(named))
	if !ok || len(seats) != 1 || !reflect.DeepEqual(seats[0], []string{cardA}) {
		t.Fatalf("pins resolved against the node's card table = %v ok = %v", seats, ok)
	}
	seats, ok = RemoteSeatCards(bare, nil, named, EstimateTokens(named))
	if !ok || len(seats) != 1 || len(seats[0]) != 0 {
		t.Fatalf("no ids and no card table: the seat's cards are unknown (empty), got %v ok = %v", seats, ok)
	}

	// A node that wrote its ids in capitals is read as lower-cased lease ids.
	loud := WithDeviceIDs(RowsFromConfig(config.FlagshipFixture(), admitting().live()), threeCards())
	for i := range loud {
		for j := range loud[i].Seats {
			for k, id := range loud[i].Seats[j].DeviceIDs {
				loud[i].Seats[j].DeviceIDs[k] = strings.ToUpper(id)
			}
		}
	}
	seats, ok = RemoteSeatCards(loud, nil, named, EstimateTokens(named))
	if !ok || len(seats) != 1 || !reflect.DeepEqual(seats[0], []string{cardA}) {
		t.Fatalf("published ids are compared as lower-cased lease ids: %v ok = %v", seats, ok)
	}

	// No rows: the node is not composite, nothing to read.
	if _, ok := RemoteSeatCards(nil, threeCards(), agentContract(), 100); ok {
		t.Fatal("a node with no layer rows has no chain")
	}
	// A long-context contract runs on a seat the chain does not describe.
	long := agentContract()
	long.ContextClass = core.ContextClassLong
	if _, ok := RemoteSeatCards(rows, threeCards(), long, 100); ok {
		t.Fatal("a long-context contract has no chain to read")
	}
}
