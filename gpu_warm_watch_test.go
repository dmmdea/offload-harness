package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A warm-back whose health request llama-swap answers 5xx (its own health wait
// ran out before a long cold load finished) watches the seat's state: a load in
// progress is waited out and a ready seat is a warm that succeeded; a seat that
// never started is the failure (2026-09-18 19:5x, the 3-card seat).
func TestWarmWatchesALoadThatOutlastsLlamaSwapsHealthWait(t *testing.T) {
	old, oldEvery := warmWatch, warmWatchEvery
	warmWatch, warmWatchEvery = 2*time.Second, 10*time.Millisecond
	t.Cleanup(func() { warmWatch, warmWatchEvery = old, oldEvery })
	var state atomic.Value // "", "starting", "ready"
	state.Store("starting")
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/upstream/seat/health", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
	})
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		var running []map[string]string
		if s := state.Load().(string); s != "" {
			running = append(running, map[string]string{"model": "seat", "state": s})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"running": running})
	})
	mux.HandleFunc("/upstream/seat/metrics", func(w http.ResponseWriter, r *http.Request) {
		t.Error("the warm watch read the seat's gauge through /upstream; it needs /running only")
		w.WriteHeader(http.StatusTeapot)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	go func() {
		time.Sleep(120 * time.Millisecond)
		state.Store("ready")
	}()
	start := time.Now()
	if err := warmSeat(context.Background(), srv.Client(), srv.URL, "seat"); err != nil {
		t.Fatalf("a load that finishes must be a successful warm: %v", err)
	}
	if took := time.Since(start); took < 100*time.Millisecond || took > time.Second {
		t.Fatalf("the warm must wait for the load, took %s", took)
	}
	// Nothing loading after the 5xx: the failure, at once.
	state.Store("")
	hits.Store(0)
	start = time.Now()
	err := warmSeat(context.Background(), srv.Client(), srv.URL, "seat")
	if err == nil || !strings.Contains(err.Error(), "not loading") {
		t.Fatalf("a 5xx with no load in progress must fail as such, got %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("a seat that is not loading must fail without waiting out the watch")
	}
	// ...and without re-sending the load: a bare 500 is not a reload.
	if n := hits.Load(); n != 1 {
		t.Fatalf("a bare 500 over a cold seat is not retried: want 1 health request, got %d", n)
	}
	// A load that never becomes ready fails at the watch bound.
	state.Store("starting")
	warmWatch = 150 * time.Millisecond
	err = warmSeat(context.Background(), srv.Client(), srv.URL, "seat")
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("a load that never finishes must fail at the watch bound, got %v", err)
	}
}

// llama-swap -watch-config reloads on any config write: the warm's health request, parked in the
// old router, is answered HTTP 500 "<router> is shutting down" and /running speaks for the new,
// cold server. The load was never refused, it was interrupted: the warm re-sends it.
func TestWarmSurvivesAReloadWhoseOldRouterAnswersShuttingDown500(t *testing.T) {
	notes := fastWarmRetry(t, 5*time.Second)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 2}
	srv := reloadingSwap(t, f)
	if err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat"); err != nil {
		t.Fatalf("a warm across a reload must succeed: %v", err)
	}
	if hits, warms := f.healthHits.Load(), f.warms.Load(); hits != 3 || warms != 1 {
		t.Fatalf("two interrupted requests and one that loaded: health requests=%d (want 3) loads=%d (want 1)", hits, warms)
	}
	got := notes.all()
	if len(got) != 1 || !strings.Contains(got[0], "reload") || !strings.Contains(got[0], "seat") || !strings.Contains(got[0], "the seat is cold") {
		t.Fatalf("the retry says so once, naming the seat and the reload: %q", got)
	}
}

// A full restart drops the listener: the health request dies with a transport error. The warm
// re-sends the load to the restarted server instead of failing on the first EOF.
func TestWarmRetriesATransportErrorAcrossARestart(t *testing.T) {
	fastWarmRetry(t, 5*time.Second)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 2, reloadHijack: true}
	srv := reloadingSwap(t, f)
	if err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat"); err != nil {
		t.Fatalf("a warm across a dropped connection must succeed: %v", err)
	}
	if hits, warms := f.healthHits.Load(), f.warms.Load(); hits != 3 || warms != 1 {
		t.Fatalf("health requests=%d (want 3) loads=%d (want 1)", hits, warms)
	}
}

// While the whole server is down /running is as unreachable as the health route; an unreadable
// /running during a recovery is "not up yet", not "wait for a load nobody started".
func TestWarmRetriesThroughAnUnreadableRunningWhileTheServerIsDown(t *testing.T) {
	fastWarmRetry(t, 5*time.Second)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 2, reloadHijack: true, runningDownUntilHit: 3}
	srv := reloadingSwap(t, f)
	start := time.Now()
	if err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat"); err != nil {
		t.Fatalf("a warm across a restart must succeed: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("an unreadable /running must not be waited out like a load (took %s)", took)
	}
	if hits := f.healthHits.Load(); hits != 3 {
		t.Fatalf("health requests=%d (want 3)", hits)
	}
}

// The status set that means "the server is going away or not there yet" is 502, 503 and 504; it
// is retried. A 404 is the server answering that it does not know the seat: not retried, and not
// a warm either (see the next test).
func TestWarmRetriesA502And503ButNotA404(t *testing.T) {
	for _, code := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			fastWarmRetry(t, 5*time.Second)
			f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1, reloadStatus: code, reloadBody: "upstream gone"}
			srv := reloadingSwap(t, f)
			if err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat"); err != nil {
				t.Fatalf("a %d is retried and the retry loads the seat: %v", code, err)
			}
			if hits, warms := f.healthHits.Load(), f.warms.Load(); hits != 2 || warms != 1 {
				t.Fatalf("health requests=%d (want 2) loads=%d (want 1)", hits, warms)
			}
		})
	}
	t.Run("404", func(t *testing.T) {
		fastWarmRetry(t, 5*time.Second)
		f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1, reloadStatus: http.StatusNotFound, reloadBody: "model not found"}
		srv := reloadingSwap(t, f)
		err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat")
		if err == nil || !strings.Contains(err.Error(), "404") {
			t.Fatalf("a 404 over a cold seat is not retried and not a warm, got %v", err)
		}
		if hits := f.healthHits.Load(); hits != 1 {
			t.Fatalf("a 404 is not retried: health requests=%d (want 1)", hits)
		}
	})
}

// A status below 500 used to be "success" whatever it said: a seat renamed or removed by the very
// config edit that reloaded llama-swap answers 404 "model not found", and the warm printed
// "warmed back" and cleared its marker for a seat that never loaded (409 and 429 the same). The
// verdict now matches what the seat is doing: ready is a warm, starting is a load to watch, cold
// is the failure, named with the status and what the server said.
func TestWarmDoesNotTakeA4xxForAWarmSeat(t *testing.T) {
	t.Run("cold", func(t *testing.T) {
		fastWarmRetry(t, 5*time.Second)
		f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1, reloadStatus: http.StatusNotFound, reloadBody: `{"error":"could not find real modelID for seat"}`}
		srv := reloadingSwap(t, f)
		err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat")
		if err == nil {
			t.Fatal("a 404 over a seat that is not loaded is not a warm")
		}
		for _, want := range []string{"status 404", "could not find real modelID", "not loading"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the failure must contain %q, got %v", want, err)
			}
		}
		if f.healthHits.Load() != 1 || f.warms.Load() != 0 {
			t.Errorf("one request, no load: health requests=%d loads=%d", f.healthHits.Load(), f.warms.Load())
		}
	})
	t.Run("ready", func(t *testing.T) {
		fastWarmRetry(t, 5*time.Second)
		f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1, reloadStatus: http.StatusTooManyRequests, reloadBody: "busy"}
		f.loaded.Store(true) // another client loaded it
		srv := reloadingSwap(t, f)
		if err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat"); err != nil {
			t.Fatalf("a 429 over a seat that is loaded and ready is a warm seat: %v", err)
		}
		if f.healthHits.Load() != 1 {
			t.Errorf("health requests=%d (want 1)", f.healthHits.Load())
		}
	})
	t.Run("starting", func(t *testing.T) {
		fastWarmRetry(t, 5*time.Second)
		f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1, reloadStatus: http.StatusConflict, reloadBody: "busy"}
		f.loaded.Store(true)
		f.starting.Store(true)
		srv := reloadingSwap(t, f)
		go func() {
			time.Sleep(100 * time.Millisecond)
			f.starting.Store(false)
		}()
		start := time.Now()
		if err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat"); err != nil {
			t.Fatalf("a load in progress is watched to its end: %v", err)
		}
		if time.Since(start) < 80*time.Millisecond {
			t.Errorf("the warm must wait for the load that was starting, took %s", time.Since(start))
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		fastWarmRetry(t, 5*time.Second)
		f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1, reloadStatus: http.StatusNotFound, reloadBody: "nope", runningDownUntilHit: 1 << 20}
		srv := reloadingSwap(t, f)
		start := time.Now()
		err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat")
		if err == nil || !strings.Contains(err.Error(), "status 404") || !strings.Contains(err.Error(), "could not be read") {
			t.Fatalf("a 404 with no readable seat state is a failure that says so, got %v", err)
		}
		if time.Since(start) > time.Second {
			t.Errorf("a 404 must not wait out the watch for a seat it cannot read, took %s", time.Since(start))
		}
	})
	t.Run("2xx is a warm", func(t *testing.T) {
		fastWarmRetry(t, 5*time.Second)
		f := &warmOrderSwap{drainSwap: &drainSwap{}}
		srv := reloadingSwap(t, f)
		if err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat"); err != nil {
			t.Fatalf("a 200 is the load: %v", err)
		}
	})
}

// The retry class is narrow on purpose. A bare 500 is how llama-swap reports a start that failed
// (the engine exited, the health check timed out): re-sending that load is a second failed load.
// Only a signature of a reload (or a restart) earns the re-send, so a broken seat still fails at
// once, with what the server said.
func TestWarmDoesNotRetryAPlainFiveHundredWithNothingLoading(t *testing.T) {
	fastWarmRetry(t, 5*time.Second)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1, reloadStatus: http.StatusInternalServerError,
		reloadBody: `{"error":{"message":"unspecific error: upstream command exited prematurely but successfully"}}`}
	srv := reloadingSwap(t, f)
	start := time.Now()
	err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat")
	if err == nil || !strings.Contains(err.Error(), "not loading") || !strings.Contains(err.Error(), "exited prematurely") {
		t.Fatalf("a bare 500 over a cold seat fails as not loading, with the server's words: %v", err)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("a bare 500 over a cold seat must fail at once, took %s", took)
	}
	if hits := f.healthHits.Load(); hits != 1 {
		t.Fatalf("a bare 500 is not retried: health requests=%d (want 1)", hits)
	}
}

// Once a reload has been seen the recovery is a window, not a single event: the new server's
// first answer can be a bare 500 with no reload words in it. Inside the grace it is retried too,
// unless its body says the start died (next test).
func TestWarmKeepsRetryingAFiveHundredThatDoesNotSayTheStartDiedInsideTheGrace(t *testing.T) {
	fastWarmRetry(t, 5*time.Second)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 2}
	f.reloadAnswerFn = func(hit int32) (int, string) {
		if hit == 1 {
			return http.StatusInternalServerError, shuttingDownBody
		}
		return http.StatusInternalServerError, `{"error":{"message":"unspecific error: not ready"}}`
	}
	srv := reloadingSwap(t, f)
	if err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat"); err != nil {
		t.Fatalf("a bare 500 right after a reload is part of the recovery: %v", err)
	}
	if hits := f.healthHits.Load(); hits != 3 {
		t.Fatalf("health requests=%d (want 3)", hits)
	}
}

// A FAILED START is answered as a 502 "unable to start process: upstream command exited
// prematurely" (the shape this repo records; the contract path reads the same death marker,
// seatwait.DeathMarker, as a start that died). That is a refusal, not a server going away, so it
// neither opens the recovery window nor is re-sent: a second send is a second engine launch while
// the lease stays held and a successor waits. The status code does not matter, and neither does
// the window.
func TestWarmDoesNotRetryAnAnswerThatSaysTheStartDied(t *testing.T) {
	bodies := map[string]string{
		"unable to start process": "unable to start process: upstream command exited prematurely",
		"engine exited":           `{"error":{"message":"unspecific error: upstream command exited prematurely","src":"llama-swap"}}`,
		"unable, nothing else":    "Unable to start process: listen failed",
	}
	for _, code := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusInternalServerError} {
		for name, body := range bodies {
			t.Run(strconv.Itoa(code)+" "+name, func(t *testing.T) {
				notes := fastWarmRetry(t, 5*time.Second)
				f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1 << 20, reloadStatus: code, reloadBody: body}
				srv := reloadingSwap(t, f)
				start := time.Now()
				err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat")
				if err == nil {
					t.Fatal("a start that died is a failed warm")
				}
				for _, want := range []string{"status " + strconv.Itoa(code), "not loading"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the failure must contain %q, got %v", want, err)
					}
				}
				if sn := warmSnippet(body); !strings.Contains(err.Error(), sn) {
					t.Errorf("the failure carries what the server said (%q): %v", sn, err)
				}
				if strings.Contains(err.Error(), "no recovery within") {
					t.Errorf("no recovery window was opened, so none ran out: %v", err)
				}
				if hits := f.healthHits.Load(); hits != 1 {
					t.Errorf("a failed start is not sent again: health requests=%d (want 1)", hits)
				}
				if took := time.Since(start); took > 500*time.Millisecond {
					t.Errorf("a failed start must fail at once, took %s", took)
				}
				if got := notes.all(); len(got) != 0 {
					t.Errorf("nothing was retried, so nothing is announced: %q", got)
				}
			})
		}
	}
}

// Inside an open window the same answer ends the warm too: a reload came first, then the new
// server's first start died. The window covers a server that is not there, not one that tried
// and failed.
func TestWarmStopsAtAnAnswerThatSaysTheStartDiedEvenInsideTheRecoveryWindow(t *testing.T) {
	fastWarmRetry(t, 5*time.Second)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 3}
	f.reloadAnswerFn = func(hit int32) (int, string) {
		if hit == 1 {
			return http.StatusInternalServerError, shuttingDownBody
		}
		return http.StatusBadGateway, "unable to start process: upstream command exited prematurely"
	}
	srv := reloadingSwap(t, f)
	err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat")
	if err == nil || !strings.Contains(err.Error(), "status 502") || !strings.Contains(err.Error(), "unable to start process") ||
		strings.Contains(err.Error(), "no recovery within") {
		t.Fatalf("the failed start ends the warm, named as such: %v", err)
	}
	if hits := f.healthHits.Load(); hits != 2 {
		t.Fatalf("the load is not re-sent after a start that died: health requests=%d (want 2)", hits)
	}
}

// The seat outranks the body: a start that died for THIS request while another client's request
// is loading the seat is watched, not failed (the contract path reads it the same way). The
// answer did not open a recovery window either, so the old rule for a 5xx still holds: an
// unreadable /running is waited out like the load it may hide.
func TestWarmWatchesASeatAnotherClientIsLoadingEvenWhenTheAnswerSaysTheStartDied(t *testing.T) {
	fastWarmRetry(t, 5*time.Second)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1, reloadStatus: http.StatusBadGateway,
		reloadBody: "unable to start process: upstream command exited prematurely"}
	f.loaded.Store(true)
	f.starting.Store(true)
	var polls atomic.Int32
	f.runningDown = func() bool { return polls.Add(1) <= 3 }
	srv := reloadingSwap(t, f)
	go func() {
		time.Sleep(150 * time.Millisecond)
		f.starting.Store(false)
	}()
	if err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat"); err != nil {
		t.Fatalf("a seat that is loading is a warm in progress: %v", err)
	}
	if hits := f.healthHits.Load(); hits != 1 {
		t.Fatalf("health requests=%d (want 1)", hits)
	}
}

// The recovery window covers a server that is going away or not there yet, not a server that
// answers: after a reload the seat may be GONE from the new config, and a 404 over a cold seat
// ends the warm at once however much of the window is left.
func TestWarmStopsAtA404EvenInsideTheRecoveryWindow(t *testing.T) {
	fastWarmRetry(t, 5*time.Second)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 2}
	f.reloadAnswerFn = func(hit int32) (int, string) {
		if hit == 1 {
			return http.StatusInternalServerError, shuttingDownBody
		}
		return http.StatusNotFound, `{"error":"could not find real modelID for seat"}`
	}
	srv := reloadingSwap(t, f)
	err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat")
	if err == nil || !strings.Contains(err.Error(), "status 404") || strings.Contains(err.Error(), "no recovery within") {
		t.Fatalf("a 404 over a cold seat ends the warm, named as such: %v", err)
	}
	if hits := f.healthHits.Load(); hits != 2 {
		t.Fatalf("the load is not re-sent after a 404: health requests=%d (want 2)", hits)
	}
}

// The grace bounds the recovery. A server that keeps saying it is shutting down must end the warm
// with the not-loading failure and the grace named, never a loop that holds the lease for good.
func TestWarmRetryIsBoundedByTheReloadGrace(t *testing.T) {
	fastWarmRetry(t, 300*time.Millisecond)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1 << 20}
	srv := reloadingSwap(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := warmSeat(ctx, srv.Client(), srv.URL, "seat")
	if err == nil || !strings.Contains(err.Error(), "not loading") || !strings.Contains(err.Error(), "no recovery within") {
		t.Fatalf("a recovery that never comes must end as not loading, with the grace named: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the retry outlived its grace: %s", took)
	}
	if hits := f.healthHits.Load(); hits < 3 {
		t.Fatalf("the grace allows several re-sends, got %d health requests", hits)
	}
}

// A seat that another client is already loading on the new server is WAITED for, not asked again:
// the re-send is for a seat nothing is loading.
func TestWarmWaitsOutALoadThatStartedOnTheNewServerInsteadOfResending(t *testing.T) {
	fastWarmRetry(t, 5*time.Second)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1}
	f.loaded.Store(true)
	f.starting.Store(true) // another client's request is loading it on the new server
	srv := reloadingSwap(t, f)
	go func() {
		time.Sleep(150 * time.Millisecond)
		f.starting.Store(false)
	}()
	start := time.Now()
	if err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat"); err != nil {
		t.Fatalf("a load that finishes is a warm: %v", err)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Errorf("the warm must wait for the load, took %s", time.Since(start))
	}
	if hits, warms := f.healthHits.Load(), f.warms.Load(); hits != 1 || warms != 0 {
		t.Errorf("no second load request while one is starting: health requests=%d (want 1) loads=%d (want 0)", hits, warms)
	}
}

// A request that timed out is a load that outlasted the client, not a reload: it is not sent
// again (the caller's final reading of the seat decides what it was). The two shapes a timeout
// takes are both covered: the client's own deadline, and a transport that gave up waiting for
// the response headers.
func TestWarmDoesNotRetryAClientTimeout(t *testing.T) {
	clients := map[string]*http.Client{
		"client deadline":         {Timeout: 100 * time.Millisecond},
		"response header timeout": {Transport: &http.Transport{ResponseHeaderTimeout: 100 * time.Millisecond}},
	}
	for name, client := range clients {
		t.Run(name, func(t *testing.T) {
			fastWarmRetry(t, 5*time.Second)
			f := &warmOrderSwap{drainSwap: &drainSwap{}, warmHold: 400 * time.Millisecond}
			srv := reloadingSwap(t, f)
			err := warmSeat(testCtx(t), client, srv.URL, "seat")
			if err == nil {
				t.Fatal("a request that timed out is an error")
			}
			var ne interface{ Timeout() bool }
			if !errors.As(err, &ne) || !ne.Timeout() {
				t.Errorf("the error must still be the timeout: %v", err)
			}
			if hits := f.healthHits.Load(); hits != 1 {
				t.Errorf("a timed-out load request is not sent again: health requests=%d (want 1)", hits)
			}
		})
	}
}

// dialTimeout is a transport timeout that is not a context deadline: what a dialer that gave up
// reports (the shape of "dial tcp ...: i/o timeout").
type dialTimeout struct{}

func (dialTimeout) Error() string   { return "dial: i/o timeout" }
func (dialTimeout) Timeout() bool   { return true }
func (dialTimeout) Temporary() bool { return true }

// A transport timeout of that shape is a connection that never came up in time, not a server
// going away mid-reload: it is not retried either.
func TestWarmDoesNotRetryATransportTimeoutThatIsNotAContextDeadline(t *testing.T) {
	fastWarmRetry(t, 300*time.Millisecond)
	var dials atomic.Int32
	client := &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, dialTimeout{}
	}}}
	err := warmSeat(testCtx(t), client, "http://warm.invalid:1", "seat")
	if err == nil || !strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("a dial timeout is an error, got %v", err)
	}
	if n := dials.Load(); n != 1 {
		t.Fatalf("a transport timeout is not retried: dials=%d (want 1)", n)
	}
}

// The back-off ends with the caller: a cancelled warm (a lost lease) returns at once, however
// long the next delay was.
func TestWarmRetryIsCancelledWithItsContext(t *testing.T) {
	fastWarmRetry(t, time.Minute)
	warmRetryFirst, warmRetryMax = 2*time.Second, 2*time.Second
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1 << 20}
	srv := reloadingSwap(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := warmSeat(ctx, srv.Client(), srv.URL, "seat")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled warm returns the cancellation, got %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("a cancelled warm must not sleep out its back-off, took %s", took)
	}
}

// What the server said is in the failure, bounded: the snippet is at most 120 bytes however big
// the body, and only the first 512 bytes of a body are read, so a reload signature further in is
// not seen.
func TestWarmErrorCarriesABoundedSnippetOfTheAnswer(t *testing.T) {
	fastWarmRetry(t, 5*time.Second)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1, reloadStatus: http.StatusInternalServerError, reloadBody: strings.Repeat("x", 4096)}
	srv := reloadingSwap(t, f)
	err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat")
	if err == nil {
		t.Fatal("a bare 500 over a cold seat is a failure")
	}
	if n := strings.Count(err.Error(), "x"); n == 0 || n > 120 {
		t.Errorf("the snippet of the answer must be there and at most 120 bytes, got %d bytes of it: %v", n, err)
	}
	f2 := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1, reloadStatus: http.StatusInternalServerError,
		reloadBody: strings.Repeat("x", 600) + " is shutting down"}
	srv2 := reloadingSwap(t, f2)
	if err := warmSeat(testCtx(t), srv2.Client(), srv2.URL, "seat"); err == nil || f2.healthHits.Load() != 1 {
		t.Errorf("a signature past the first 512 bytes is not read: err=%v health requests=%d (want 1)", err, f2.healthHits.Load())
	}
}

// The gate is asked before EVERY re-send, never before the first request (that check is the
// caller's), and a refusal ends the warm with the sentinel the caller branches on.
func TestWarmAsksItsGateBeforeEachResend(t *testing.T) {
	fastWarmRetry(t, 5*time.Second)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 2}
	srv := reloadingSwap(t, f)
	asked := 0
	if err := warmSeatGated(testCtx(t), srv.Client(), srv.URL, "seat", func() bool { asked++; return true }); err != nil {
		t.Fatalf("an open gate lets the retry through: %v", err)
	}
	if asked != 2 || f.healthHits.Load() != 3 {
		t.Fatalf("two re-sends, two gate checks: asked=%d health requests=%d", asked, f.healthHits.Load())
	}
	f2 := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1 << 20}
	srv2 := reloadingSwap(t, f2)
	asked = 0
	err := warmSeatGated(testCtx(t), srv2.Client(), srv2.URL, "seat", func() bool { asked++; return asked < 2 })
	if !errors.Is(err, errWarmGuardStopped) {
		t.Fatalf("a closed gate ends the warm with the sentinel, got %v", err)
	}
	if asked != 2 || f2.healthHits.Load() != 2 {
		t.Fatalf("the second check refused, so only the first re-send went: asked=%d health requests=%d (want 2 and 2)", asked, f2.healthHits.Load())
	}
}

// A reload does not end the patience a load in progress is owed. After the reload the re-sent
// load outlasts llama-swap's health wait (the 500 comes back while the seat is still starting),
// and one /running poll fails in the middle of it. That poll says nothing about the load: the
// watch keeps waiting for the seat it last saw starting instead of calling the state unknown,
// which would end the warm while the load still lands, under whoever takes the card next.
func TestWarmKeepsWaitingForALoadItSawStartingThroughOneUnreadableRunningAfterAReload(t *testing.T) {
	notes := fastWarmRetry(t, 5*time.Second)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1, healthFails: true, visibleLoad: true, warmHold: 300 * time.Millisecond}
	var polls atomic.Int32
	f.runningDown = func() bool {
		// The second poll that finds the seat starting fails, once; the first one read it fine.
		return f.starting.Load() && polls.Add(1) == 2
	}
	srv := reloadingSwap(t, f)
	if err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat"); err != nil {
		t.Fatalf("a load that finishes is a warm, whatever one poll said in the middle of it: %v", err)
	}
	if polls.Load() < 2 {
		t.Fatalf("the stand-in never failed a poll mid-load (polls while starting=%d): the test proved nothing", polls.Load())
	}
	// One reload answer, then the one load. A watch that gave up on the unreadable poll would
	// have sent the load a third time.
	if hits, warms := f.healthHits.Load(), f.warms.Load(); hits != 2 || warms != 1 {
		t.Fatalf("health requests=%d (want 2) loads=%d (want 1)", hits, warms)
	}
	if got := notes.all(); len(got) != 1 {
		t.Fatalf("the reload was announced once: %q", got)
	}
}

// The other face of that rule: nothing was ever seen starting, so an unreadable /running during a
// recovery is "the server is not up yet", and the warm backs off and re-sends instead of waiting
// out the watch (15 minutes in production) for a load nobody started. The answer here is a 503
// from whatever fronts llama-swap, not a dropped connection, so the attempt carries a status.
func TestWarmDoesNotWaitOutAnUnreadableRunningAfterAReloadAnsweredBy503(t *testing.T) {
	fastWarmRetry(t, 5*time.Second)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 2, reloadStatus: http.StatusServiceUnavailable, reloadBody: "upstream gone", runningDownUntilHit: 3}
	srv := reloadingSwap(t, f)
	start := time.Now()
	if err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat"); err != nil {
		t.Fatalf("a warm across a front end that was down must succeed: %v", err)
	}
	// warmWatch is 2s in this test: a watch that waited for /running would have run it out.
	if took := time.Since(start); took > time.Second {
		t.Fatalf("an unreadable /running with nothing starting must not be waited out like a load (took %s)", took)
	}
	if hits, warms := f.healthHits.Load(), f.warms.Load(); hits != 3 || warms != 1 {
		t.Fatalf("health requests=%d (want 3) loads=%d (want 1)", hits, warms)
	}
}

// The back-off doubles from warmRetryFirst up to warmRetryMax and stays there. Timers are coarse
// on some hosts, so the test reads arrival times with generous scales, and checks the two things
// a wrong back-off changes: no gap is shorter than its delay (a constant first delay fails the
// third gap), and the cap is reached (an uncapped doubling reaches only six requests in the
// grace where the capped one sends about eleven; the floor is eight so a loaded host keeps
// some headroom).
func TestWarmBackoffDoublesUpToItsCap(t *testing.T) {
	fastWarmRetry(t, 1500*time.Millisecond)
	warmRetryFirst, warmRetryMax = 40*time.Millisecond, 160*time.Millisecond
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1 << 20}
	srv := reloadingSwap(t, f)
	err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat")
	if err == nil || !strings.Contains(err.Error(), "no recovery within") {
		t.Fatalf("a recovery that never comes ends with the grace named: %v", err)
	}
	at := f.arrivals()
	if len(at) < 8 {
		t.Fatalf("the capped back-off sends about eleven requests in the grace (an uncapped one six), got %d", len(at))
	}
	want := []time.Duration{40, 80, 160, 160, 160}
	for i, w := range want {
		w *= time.Millisecond
		if gap := at[i+1].Sub(at[i]); gap < w-time.Millisecond {
			t.Errorf("gap %d between requests is %s, shorter than its back-off of %s", i+1, gap, w)
		}
	}
}

// The body of the answer is the only input to the reload classification, and a read of it can
// fail: the old router is shutting down while it writes the 500, and the connection dies mid-body.
// A 500 whose body was cut short by a dropped connection is retried like the reload it is (the
// words that would have said so did not arrive), where a bare 500 over a cold seat is not.
func TestWarmRetriesAFiveHundredWhoseBodyWasCutShort(t *testing.T) {
	fastWarmRetry(t, 5*time.Second)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1, reloadTruncate: true}
	srv := reloadingSwap(t, f)
	if err := warmSeat(testCtx(t), srv.Client(), srv.URL, "seat"); err != nil {
		t.Fatalf("a 500 cut off mid-answer is a reload: %v", err)
	}
	if hits, warms := f.healthHits.Load(), f.warms.Load(); hits != 2 || warms != 1 {
		t.Fatalf("health requests=%d (want 2) loads=%d (want 1)", hits, warms)
	}
}

// When the body cannot be read and the failure is not a dropped connection (here the client's own
// deadline ran out while the answer was being read), nothing is retried and the failure says the
// answer was cut short, with the error, instead of presenting the part that arrived as the whole.
func TestWarmFailureSaysWhenTheAnswerWasCutShort(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/upstream/seat/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "400")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("unspecific error: part"))
		w.(http.Flusher).Flush()
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"running": []map[string]string{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	fastWarmRetry(t, 5*time.Second)
	client := &http.Client{Timeout: 300 * time.Millisecond}
	err := warmSeat(testCtx(t), client, srv.URL, "seat")
	if err == nil {
		t.Fatal("a 500 over a cold seat is a failure")
	}
	for _, want := range []string{"status 500", "unspecific error: part", "the answer was cut short", "not loading"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure must contain %q, got %v", want, err)
		}
	}
	if got := (warmAttempt{status: 500, bodyErr: errors.New("read: boom")}).describe(); !strings.Contains(got, "could not be read: read: boom") {
		t.Errorf("an answer with no body at all says it could not be read: %q", got)
	}
}
