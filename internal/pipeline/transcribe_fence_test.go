package pipeline

// transcribe_fence_test.go pins register C-89 at the call site: a transcription that finds
// the card held waits gpu_wait_ms, like every other GPU door, and then defers as CAPACITY
// ("gpu busy: ...", error class gpu_busy), the shape the vision tier and the agent doors
// already use. It used to wait the client's own timeout (stt_request_timeout_sec, 1,800 s,
// the limit at which the MCP client aborts an idle call) and then fail as an unclassed
// "transcribe call failed: gpu-lease timeout ...", which no delegator can re-place.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

func TestTranscribeUnderAHeldCardDefersAsCapacityWithinGPUWait(t *testing.T) {
	ffmpeg := lookFFmpeg()
	if ffmpeg == "" {
		t.Skip("ffmpeg not on PATH; this test needs a real convert to reach the STT call")
	}
	wav := makeSilentWav(t, ffmpeg)
	isolateSwapKeepSet(t)

	// A render holds the card. The process-wide gate is armed at a temp directory (never
	// the machine's), so this test does not run in parallel with another that reads it.
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	if err := modelaffinity.SetGPULease("", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(modelaffinity.DisarmGPULease)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "video render", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()

	// whisper is not resident, so the fence holds the request; anything that reaches
	// /upstream would load it onto the render's card.
	var upstream atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"test-stt"}]}`))
		case r.URL.Path == "/running":
			_, _ = w.Write([]byte(`{"running":[]}`))
		case strings.HasPrefix(r.URL.Path, "/upstream/"):
			upstream.Add(1)
			_, _ = w.Write([]byte(`{"text":"hi","segments":[{"id":0,"start":0,"end":1,"text":"hi"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := gateCfg(t)
	cfg.MediaDir = t.TempDir()
	cfg.FFmpegPath = ffmpeg
	cfg.Endpoint = srv.URL
	cfg.GPUWaitMs = 300          // the injected, small gpu wait
	cfg.STTRequestTimeoutSec = 4 // the old bound: far above it, so waiting it out is visible
	p := gatePipeline(t, cfg, nil)

	start := time.Now()
	res := p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: wav})
	elapsed := time.Since(start)

	if res.OK || !res.Deferred {
		t.Fatalf("want a deferred result under a held card, got OK=%v Deferred=%v data=%s", res.OK, res.Deferred, res.Data)
	}
	if elapsed > 3*time.Second {
		t.Errorf("deferred after %s: the call waited out the client's 4s timeout instead of the 300ms gpu wait", elapsed)
	}
	if res.DeferClass != core.DeferClassCapacity {
		t.Errorf("defer class = %q, want %q: a held card is capacity, which the delegator re-places", res.DeferClass, core.DeferClassCapacity)
	}
	if res.Meta.ErrClass != "gpu_busy" {
		t.Errorf("error class = %q, want gpu_busy", res.Meta.ErrClass)
	}
	if !strings.HasPrefix(res.Reason, "gpu busy:") || !strings.Contains(res.Reason, "video render") {
		t.Errorf("reason = %q, want \"gpu busy: ...\" naming the holder", res.Reason)
	}
	if got := upstream.Load(); got != 0 {
		t.Errorf("%d request(s) reached /upstream under a media lease — each one loads whisper onto the render's card", got)
	}
}
