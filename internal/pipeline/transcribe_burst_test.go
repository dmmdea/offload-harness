package pipeline

// transcribe_burst_test.go pins register C-91 at the call site: runTranscribe sends the
// zero-always-warm unload after a transcription, and sent it after EVERY one — so a call
// that finished while another was in line unloaded whisper from under the next call's
// inference (llama-swap answers that call "matrix: model unloaded"). The client-side
// invariants are pinned in internal/sttclient; this proves the pipeline asks for the
// idle-only unload, with the real pipeline, the real ffmpeg convert and a llama-swap
// stand-in that aborts what an unload finds in flight.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/sttclient"
)

// isolateSwapKeepSet points the llama-swap keep-set loader at nothing, so a test never
// picks up the config of the box it runs on (a keep-set member's unload is refused before
// anything is sent, which would read as "zero unloads" for the wrong reason).
func isolateSwapKeepSet(t *testing.T) {
	t.Helper()
	t.Setenv("LLAMASWAP_YAML", filepath.Join(t.TempDir(), "absent.yaml"))
	t.Setenv("LLAMASWAP_KEEP_SET", "")
	t.Setenv("LLAMASWAP_CONFIG", "")
}

// swapStandIn is a llama-swap stand-in for one whisper model: the first inference is held
// until the test lets it go, later ones take `later`, and an unload that finds an inference
// in flight aborts it with the 500 the real proxy answers.
type swapStandIn struct {
	srv    *httptest.Server
	hold   chan struct{} // closed by the test to let the first inference finish
	firstN chan struct{} // closed when the first inference arrives
	later  time.Duration
	// unloadStatus, when set, is the HTTP status an unload answers instead of 200: the unload
	// still counts as sent, it just fails.
	unloadStatus int

	mu       sync.Mutex
	arrived  int
	inflight int
	served   int
	aborted  int
	abort    chan struct{}
	events   []string // "inference" / "unload", in arrival order
	unloads  [][2]int // {in flight, served} at each unload's arrival
}

func newSwapStandIn(t *testing.T, model string, later time.Duration) *swapStandIn {
	t.Helper()
	s := &swapStandIn{hold: make(chan struct{}), firstN: make(chan struct{}), later: later, abort: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"` + model + `"}]}`))
		case r.URL.Path == "/running":
			_, _ = w.Write([]byte(`{"running":[{"model":"` + model + `","state":"ready","ttl":300}]}`))
		case strings.HasPrefix(r.URL.Path, "/api/models/unload/"):
			s.mu.Lock()
			s.events = append(s.events, "unload")
			s.unloads = append(s.unloads, [2]int{s.inflight, s.served})
			if s.inflight > 0 {
				close(s.abort)
				s.abort = make(chan struct{})
			}
			s.mu.Unlock()
			if s.unloadStatus != 0 {
				http.Error(w, "unload failed", s.unloadStatus)
			}
		case strings.HasPrefix(r.URL.Path, "/upstream/"):
			s.inference(w)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *swapStandIn) inference(w http.ResponseWriter) {
	s.mu.Lock()
	s.arrived++
	first := s.arrived == 1
	s.inflight++
	s.events = append(s.events, "inference")
	abort := s.abort
	s.mu.Unlock()

	var wait <-chan time.Time
	var released <-chan struct{}
	if first {
		close(s.firstN)
		released = s.hold
	} else {
		wait = time.After(s.later)
	}
	aborted := false
	select {
	case <-wait:
	case <-released:
	case <-abort:
		aborted = true
	}

	s.mu.Lock()
	s.inflight--
	s.served++
	if aborted {
		s.aborted++
	}
	s.mu.Unlock()
	if aborted {
		http.Error(w, `{"error":"unspecific error: matrix: model unloaded","src":"llama-swap"}`, http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte(`{"language":"english","duration":1,"text":"hello","segments":[{"id":0,"start":0,"end":1,"text":"hello"}]}`))
}

// TestConcurrentTranscribesUnloadWhisperOnceAfterTheLastCall: call A is on the upstream
// and call B is in line behind it. When A finishes, the old pipeline unloaded whisper at
// once, on top of B's inference; now A leaves the unload to B, the last call out.
func TestConcurrentTranscribesUnloadWhisperOnceAfterTheLastCall(t *testing.T) {
	ffmpeg := lookFFmpeg()
	if ffmpeg == "" {
		t.Skip("ffmpeg not on PATH; this test needs a real convert to reach the STT call")
	}
	isolateSwapKeepSet(t)
	cfg := gateCfg(t)
	cfg.MediaDir = t.TempDir()
	cfg.FFmpegPath = ffmpeg
	fake := newSwapStandIn(t, cfg.STTModel, 150*time.Millisecond)
	cfg.Endpoint = fake.srv.URL
	p := gatePipeline(t, cfg, nil)
	if !cfg.STTUnloadAfter {
		t.Fatal("zero-always-warm must be the default for this test to mean anything")
	}

	run := func(wav string, out *core.Result, wg *sync.WaitGroup) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			*out = p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: wav})
		}()
	}
	var wg sync.WaitGroup
	var a, b core.Result
	run(makeSilentWav(t, ffmpeg), &a, &wg)
	select {
	case <-fake.firstN:
	case <-time.After(30 * time.Second):
		t.Fatal("call A never reached the upstream")
	}
	run(makeSilentWav(t, ffmpeg), &b, &wg)
	// B is in line once it has been converted and counted by the client: wait for that
	// instead of sleeping, so A cannot finish before B is waiting.
	for deadline := time.Now().Add(30 * time.Second); sttclient.Pending() < 2; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			close(fake.hold)
			wg.Wait()
			t.Fatalf("call B never got in line behind A (pending = %d)", sttclient.Pending())
		}
	}
	close(fake.hold)
	wg.Wait()

	for name, res := range map[string]core.Result{"A": a, "B": b} {
		if !res.OK {
			t.Errorf("call %s did not complete: %q", name, res.Reason)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.aborted != 0 {
		t.Errorf("%d inference(s) were aborted by an unload that landed on them", fake.aborted)
	}
	if len(fake.unloads) != 1 {
		t.Fatalf("%d unload(s) sent for two calls, want exactly 1 — the last call out", len(fake.unloads))
	}
	if u := fake.unloads[0]; u[0] != 0 || u[1] != 2 {
		t.Errorf("the unload arrived with %d inference(s) in flight and %d of 2 finished: it belongs after the last of them", u[0], u[1])
	}
	if last := fake.events[len(fake.events)-1]; last != "unload" {
		t.Errorf("the last request the upstream saw was %q, want the unload (events: %v)", last, fake.events)
	}
}
