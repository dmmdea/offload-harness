package delegate

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// Register F48 / C23: a delegation the caller pinned for measurement is a measurement, not a card on the
// operator's PAIR dashboard. The control shows the same frame IS sent for a pin made for another reason, so the
// test cannot pass by emitting nothing at all.
func TestAMeasurementPinnedDelegationOpensNoPairCard(t *testing.T) {
	pairAppDir(t)
	c := &pairCapture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.PairWorkloadsEnabled = true
	cfg.PairWorkloadsEndpoint = srv.URL
	mk := func(pin pinCall) *runner {
		return &runner{cfg: cfg, pair: pairworkloads.New(pairworkloads.FromConfig(cfg)), pin: pin}
	}
	inFlight := func() PlacedResult { return PlacedResult{pairModel: "seat-m", pairEngine: "engine-e", pairCreated: 1} }

	r := mk(pinCall{asked: "local", reason: PinPrivacy})
	pr := inFlight()
	r.pairTerminal("agd-real", &pr)
	r.pair.Wait()
	if n := len(c.snapshot()); n != 1 {
		t.Fatalf("control: %d frame(s) for a privacy pin, want 1", n)
	}

	r = mk(pinCall{asked: "local", reason: PinMeasurement})
	pr = inFlight()
	r.pairTerminal("agd-measure", &pr)
	pr = PlacedResult{}
	r.pairInflight(&pr, "agd-measure", "", nil, "seat-m", "queued", true)
	r.pair.Wait()
	waitABit()
	if n := len(c.snapshot()); n != 1 {
		t.Fatalf("a measurement-pinned delegation opened PAIR cards: %d frame(s) in all, want the control's 1", n)
	}
}

// The card a delegator does not open must not reappear on the serving node: a box whose own PAIR emitter is
// off (a thin client) tells the node to card its jobs, and a node that does would open the card of a call
// pinned for measurement as "Requested from fleet:<asker>". The control shows the same box still asks for any
// other pin, and an unpinned call.
func TestAMeasurementPinnedDispatchAsksNoNodeToCard(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node, url := acceptingNode(t, "node-a", "zorblax", nil)
	t.Setenv("OFFLOAD_PAIR_APPDIR", t.TempDir()) // no PAIR on this box: its emitter is off
	cfg := testCfg(t)

	dispatch := func(opts *RunOptions) string {
		t.Helper()
		if _, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{url}, opts); err != nil || sum.Succeeded != 1 {
			t.Fatalf("Run: err=%v summary=%+v", err, sum)
		}
		got, _ := node.lastPairCard.Load().(string)
		return got
	}
	if got := dispatch(&RunOptions{PinReason: PinOperator, PinNeedsReason: true}); got != core.PairCardNode {
		t.Fatalf("control: a pin made for another reason must still ask the node to card: %q", got)
	}
	if got := dispatch(nil); got != core.PairCardNode {
		t.Fatalf("control: an unpinned call must still ask the node to card: %q", got)
	}
	if got := dispatch(&RunOptions{PinReason: PinMeasurement, PinNeedsReason: true}); got != "" {
		t.Fatalf("a call pinned for measurement asked the node to open its card: %s = %q", core.PairCardHeader, got)
	}
}
