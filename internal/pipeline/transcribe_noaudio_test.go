package pipeline

// F21 (2026-10-09): transcribing a file with no audio stream deferred with ffmpeg's raw text. The door now
// says "the input has no audio stream", classes the defer as the caller's (contract), files the call's
// err_class as bad_input, and the ledger row carries the reason code bad_input (F22).

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/winexec"
)

func TestTranscribeOfAFileWithNoAudioStreamSaysSoAndIsFiledAsABadInput(t *testing.T) {
	ffmpeg := lookFFmpeg()
	if ffmpeg == "" {
		t.Skip("ffmpeg not on PATH; the conversion under test is a real one")
	}
	silent := filepath.Join(t.TempDir(), "silent.mp4")
	if out, err := winexec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "color=c=black:s=64x64:d=1", "-pix_fmt", "yuv420p", silent).CombinedOutput(); err != nil {
		t.Skipf("could not synthesise a video with no audio track: %v (%s)", err, out)
	}
	ledgerPath := filepath.Join(t.TempDir(), "ledger.jsonl")
	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := gateCfg(t)
	cfg.MediaDir = t.TempDir()
	cfg.FFmpegPath = ffmpeg
	p := New(cfg, llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 0), gateCache(t), led)

	res := p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: silent, Door: "offload_transcribe"})
	_ = led.Close()
	if res.OK || !res.Deferred {
		t.Fatalf("want a deferred result, got OK=%v Deferred=%v", res.OK, res.Deferred)
	}
	if res.Reason != "the input has no audio stream" {
		t.Errorf("reason = %q, want the plain sentence (not ffmpeg's text)", res.Reason)
	}
	if res.Meta.ErrClass != core.ErrClassBadInput {
		t.Errorf("err_class = %q, want %q", res.Meta.ErrClass, core.ErrClassBadInput)
	}
	if res.DeferClass != core.DeferClassContract {
		t.Errorf("defer_class = %q, want %q: the caller's file is what is wrong, not the stack", res.DeferClass, core.DeferClassContract)
	}
	rows, err := ledger.ReadAll(ledgerPath)
	if err != nil || len(rows) != 1 {
		t.Fatalf("one ledger row: %v %+v", err, rows)
	}
	row := rows[0]
	if !row.Deferred || row.Reason != "the input has no audio stream" || row.ErrClass != core.ErrClassBadInput || row.ReasonCode != ledger.ReasonBadInput {
		t.Errorf("the row must carry the sentence, the err_class and the reason code bad_input: %+v", row)
	}
}

// Every core.ErrClass* is a member of the ledger's err_class table: a new congestion class that nobody
// added there would file as "other" and fragment the register this closed set exists to keep (F22).
func TestEveryCoreErrClassHasALedgerReasonCode(t *testing.T) {
	for _, c := range []string{
		core.ErrClassGPUBusy, core.ErrClassGPUQueued, core.ErrClassComposeBusy, core.ErrClassGPULeaseUnavailable, core.ErrClassBadInput, core.ErrClassFFmpegUnavailable,
		errClassGPUBusy, errClassLaneGone,
	} {
		if got := ledger.DeferCode(c, ""); got == ledger.ReasonOther {
			t.Errorf("err_class %q files as %q: add it to the ledger's table", c, got)
		}
	}
}
