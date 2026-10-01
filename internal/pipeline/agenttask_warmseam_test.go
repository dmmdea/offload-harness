package pipeline

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// The warm-up core answers both decisions in one pass: a refusal that is positive
// evidence the seat's process did not start (register C-76) is reported through the
// status AND is neither an attempted nor a confirmed load (register C-66), so the
// run defers on it without the coherence probe or the cold-load store hearing of a
// load that never happened.
func TestWarmSeatWithReportsTheRefusalAndTheLoadTogether(t *testing.T) {
	fake := &agentFake{
		rosterIDs:      []string{agentTestSeat},
		running:        func(int64) string { return `{"running":[]}` }, // the seat never reads ready
		upstreamStatus: func(int64) int { return http.StatusInternalServerError },
		upstreamBody:   func(int64) string { return "upstream command exited prematurely" },
	}
	srv := fake.server(t)
	defer srv.Close()

	var st warmStatus
	_, note, attempted, loaded := warmSeatWith(context.Background(), srv.URL, agentTestSeat, 30*time.Second, &st)
	if st.Refused != http.StatusInternalServerError || attempted || loaded {
		t.Fatalf("refused=%d attempted=%v loaded=%v (note %q), want the refusal reported and no load attempted or confirmed", st.Refused, attempted, loaded, note)
	}

	// A 200 is both: the load was attempted and confirmed, and there is no refusal.
	ok := &agentFake{
		rosterIDs: []string{agentTestSeat},
		running: func(n int64) string {
			if n == 1 {
				return `{"running":[]}`
			}
			return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"x"}]}`
		},
		upstreamModels: func(int64) string { return `{"object":"list","data":[{"id":"` + agentTestSeat + `"}]}` },
	}
	srv2 := ok.server(t)
	defer srv2.Close()
	var st2 warmStatus
	_, _, attempted, loaded = warmSeatWith(context.Background(), srv2.URL, agentTestSeat, 30*time.Second, &st2)
	if st2.Refused != 0 || !attempted || !loaded {
		t.Fatalf("refused=%d attempted=%v loaded=%v, want a confirmed load and no refusal", st2.Refused, attempted, loaded)
	}
}
