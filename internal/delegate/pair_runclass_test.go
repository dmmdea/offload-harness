package delegate

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
