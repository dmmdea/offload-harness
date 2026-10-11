package fleetnode

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The seat's prefill rate is published beside its decode rate (register F10): the average the
// store keeps and the slowest recent solo sample the stall bound is sized from, so a reader of
// /fleet/health can check the number a "silent N s of M s allowed" line rests on. A seat with
// no prefill measurement publishes neither key (never a zero that reads as a measured rate),
// and the node that has none is unchanged.
func TestHealthPublishesThePrefillRateTheStallBoundRestsOn(t *testing.T) {
	cfg := agentCfg()
	cfg.StateDir = t.TempDir()
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	store := filepath.Join(cfg.StateDir, "seat-rates.json")

	read := func() (SeatRateHealth, string) {
		s.seatRateAt = time.Time{} // expire the health cache
		rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
		var h struct {
			SeatRate *SeatRateHealth `json:"seat_rate"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
			t.Fatal(err)
		}
		if h.SeatRate == nil {
			t.Fatalf("seat_rate absent: %s", rec.Body.String())
		}
		raw, _ := json.Marshal(h.SeatRate)
		return *h.SeatRate, string(raw)
	}

	// No prefill measurement yet: the decode sample publishes alone, and neither prefill key appears.
	if err := os.WriteFile(store, []byte(`{"seats":{"offload-e4b":{"tok_s":25,"cold_load_sec":34,"samples":3}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, raw := read(); strings.Contains(raw, "prefill") {
		t.Fatalf("an unmeasured prefill must publish no key: %s", raw)
	}

	// Measured: the average and the slowest of the window are both published.
	if err := os.WriteFile(store, []byte(`{"seats":{"offload-e4b":{"tok_s":25,"cold_load_sec":34,"samples":3,"prefill_tok_s":300,"prefill_rates":[310,290,120,310,290],"prefill_slow_tok_s":120}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ := read()
	if got.PrefillTokS != 300 || got.PrefillSlowTokS != 120 {
		t.Fatalf("seat_rate prefill = %.0f avg / %.0f slow, want 300 / 120", got.PrefillTokS, got.PrefillSlowTokS)
	}
}
