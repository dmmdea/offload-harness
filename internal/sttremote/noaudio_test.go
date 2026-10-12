package sttremote

// F21 on the remote lane: a file with no audio stream used to be sent to a node anyway (the Opus
// conversion failed, so "the original file" was uploaded) and the node failed on it with ffmpeg's words.
// It is now refused here, in the same words and classes the local door files, and no node is contacted.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/winexec"
)

func TestAFileWithNoAudioStreamIsNeverSentToANode(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH; the conversion under test is a real one")
	}
	silent := filepath.Join(t.TempDir(), "silent.mp4")
	if out, err := winexec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "color=c=black:s=64x64:d=1", "-pix_fmt", "yuv420p", silent).CombinedOutput(); err != nil {
		t.Skipf("could not synthesise a video with no audio track: %v (%s)", err, out)
	}
	var contacted atomic.Int32
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted.Add(1)
		http.Error(w, "must not be contacted", http.StatusTeapot)
	}))
	defer node.Close()
	cfg := config.Default()
	cfg.DelegateRemotes = []string{node.URL}
	cfg.FleetAuthToken = "tok"
	cfg.FFmpegPath = ffmpeg

	res, err := callWith(context.Background(), cfg, core.Request{Task: core.TaskTranscribe, Audio: silent}, core.NopAttribution{})
	if err != nil {
		t.Fatalf("a bad input is a deferred result, not a placement error: %v", err)
	}
	if res.OK || !res.Deferred || res.Reason != "the input has no audio stream" {
		t.Errorf("want the plain deferred sentence, got OK=%v Deferred=%v reason=%q", res.OK, res.Deferred, res.Reason)
	}
	if res.Meta.ErrClass != core.ErrClassBadInput || res.DeferClass != core.DeferClassContract {
		t.Errorf("err_class %q defer_class %q, want %q and %q", res.Meta.ErrClass, res.DeferClass, core.ErrClassBadInput, core.DeferClassContract)
	}
	if n := contacted.Load(); n != 0 {
		t.Errorf("a file that cannot be transcribed anywhere was offered to a node (%d request(s))", n)
	}
}
