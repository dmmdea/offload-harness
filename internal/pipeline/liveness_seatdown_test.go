package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"syscall"
	"testing"

	"github.com/dmmdea/offload-harness/internal/seatload"
)

// The engine probe carries what the seat-down verdicts need (ADR 0066): the
// engine's own request gauges, and — as evidence, never as a guess — whether
// llama-swap lists the seat at all.
func TestEngineActivityProbeReportsTheLoadAndAGoneSeat(t *testing.T) {
	e := &engineSwap{}
	srv := e.server(t)
	defer srv.Close()
	probe := engineActivityProbe(srv.URL, "agent-pool", seatLoadProbe(srv.URL, "agent-pool"))
	ctx := context.Background()

	ready, err := probe(ctx)
	if err != nil || ready.NotLoaded || ready.Loading || ready.Refused {
		t.Fatalf("ready seat: %+v %v", ready, err)
	}
	if ready.Running != 3 || ready.Waiting != 2 || ready.Load() != 5 {
		t.Fatalf("gauges = %d running, %d waiting, load %d, want 3/2/5 (the engine's own request gauges)", ready.Running, ready.Waiting, ready.Load())
	}

	// llama-swap no longer lists the seat and nothing is loading: NOT LOADED —
	// not an error (that is llama-swap's answer) and not an idle engine.
	e.state.Store("")
	gone, err := probe(ctx)
	if err != nil || !gone.NotLoaded || gone.Loading || gone.Fingerprint != "" {
		t.Fatalf("an unlisted seat must read as not loaded, without a fingerprint and without an error: %+v %v", gone, err)
	}
}

// The seat's own address refusing the connection while llama-swap still lists the
// seat is a dead engine: the reading says so (Refused) beside the error. Nothing
// listens where the seat was.
func TestEngineActivityProbeFlagsARefusedEngine(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // nothing listens on this address any more
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"seat-x"}]}`))
	})
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"running": []map[string]string{{"model": "seat-x", "state": "ready", "proxy": deadURL + "/seat"}}})
	})
	swap := httptest.NewServer(mux)
	defer swap.Close()

	rd, err := engineActivityProbe(swap.URL, "seat-x", nil)(context.Background())
	if err == nil {
		t.Fatalf("a dead engine answered: %+v", rd)
	}
	if !rd.Refused {
		t.Fatalf("reading = %+v (err %v): a connection refused at the seat's own address must be flagged Refused", rd, err)
	}
}

// llama-swap's OWN address refusing the connection (the service is down) is not
// the seat's engine refusing: act.Loaded is what says llama-swap answered and
// listed the seat. Reading it as a dead engine would send the run into a
// ten-minute wait for a seat that no read can ever confirm.
func TestEngineActivityProbeDoesNotBlameTheEngineWhenLlamaSwapIsDown(t *testing.T) {
	swap := httptest.NewServer(http.NotFoundHandler())
	url := swap.URL
	swap.Close() // llama-swap itself is gone
	rd, err := engineActivityProbe(url, "seat-x", nil)(context.Background())
	if err == nil {
		t.Fatalf("a dead llama-swap answered: %+v", rd)
	}
	if rd.Refused {
		t.Fatalf("reading = %+v (err %v): llama-swap's own refusal must not be flagged as the seat's engine refusing", rd, err)
	}
}

// connRefused: a refusal, on either platform's wording — and nothing else.
func TestConnRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"the syscall error under a url.Error", &url.Error{Op: "Get", URL: "http://192.0.2.1:1/metrics", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}, true},
		{"the linux wording", errors.New("dial tcp 192.0.2.1:18797: connect: connection refused"), true},
		{"the windows wording", errors.New("dial tcp 192.0.2.1:18797: connectex: No connection could be made because the target machine actively refused it."), true},
		{"wrapped by the activity reader", fmt.Errorf("seat activity: %w", errors.New("Get \"http://192.0.2.1:1/metrics\": dial tcp: connection refused")), true},
		{"a reset is not a refusal", errors.New("read tcp 192.0.2.1:1->192.0.2.2:2: connection reset by peer"), false},
		{"a timeout", context.DeadlineExceeded, false},
		{"a seat this box cannot reach (loopback-bound on another machine)", fmt.Errorf("seat activity at http://x: %w: connection refused", seatload.ErrRemoteSeatUnreachable), false},
		{"nil", nil, false},
	} {
		if got := connRefused(tc.err); got != tc.want {
			t.Errorf("%s: connRefused(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
