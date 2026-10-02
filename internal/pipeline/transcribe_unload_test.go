package pipeline

// transcribe_unload_test.go pins the call-site half of the second round of register C-91's
// review: the zero-always-warm unload that follows a transcription is best-effort, and its
// error used to be thrown away. A model that stayed loaded because its unload failed then
// left no trace anywhere, and the only thing that would free it was llama-swap's ttl.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestAFailedZeroAlwaysWarmUnloadIsLoggedAndDoesNotFailTheCall: llama-swap answers the unload
// with a 500. The transcription it follows is still delivered, and the log says which model
// stays loaded and why.
func TestAFailedZeroAlwaysWarmUnloadIsLoggedAndDoesNotFailTheCall(t *testing.T) {
	ffmpeg := lookFFmpeg()
	if ffmpeg == "" {
		t.Skip("ffmpeg not on PATH; this test needs a real convert to reach the STT call")
	}
	isolateSwapKeepSet(t)
	cfg := gateCfg(t)
	cfg.MediaDir = t.TempDir()
	cfg.FFmpegPath = ffmpeg
	fake := newSwapStandIn(t, cfg.STTModel, time.Millisecond)
	fake.unloadStatus = http.StatusInternalServerError
	close(fake.hold) // the one inference is not to be held
	cfg.Endpoint = fake.srv.URL
	if !cfg.STTUnloadAfter {
		t.Fatal("zero-always-warm must be the default for this test to mean anything")
	}
	p := gatePipeline(t, cfg, nil)
	logs := captureLog(t)

	res := p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: makeSilentWav(t, ffmpeg)})

	if !res.OK {
		t.Fatalf("the transcription must not fail because its unload did: %q", res.Reason)
	}
	fake.mu.Lock()
	unloads := len(fake.unloads)
	fake.mu.Unlock()
	if unloads != 1 {
		t.Fatalf("%d unload(s) reached the stand-in, want the one that fails", unloads)
	}
	out := logs.String()
	// The model is pinned by the line's own wording: the error underneath it names the model
	// too, and a line that dropped the model could still pass on that.
	for _, want := range []string{"zero-always-warm unload after a " + cfg.STTModel + " call failed", "500"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log %q lacks %q: a failed unload must leave a trace naming the model and the error", out, want)
		}
	}
}
