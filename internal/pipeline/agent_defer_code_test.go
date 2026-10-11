package pipeline

// F22 follow-up (2026-10-10): the agent door's deferred rows carried a free-text reason and no err_class, so
// "output failed schema", "structured re-pack skipped" and "step budget exhausted" (about 60 % of the
// deferred non-delegate rows of a 48 hour window) all coded as "other". The door knows the class (core.DeferClass*)
// when it defers; the row now carries it as its reason code. And a transcription whose ffmpeg could not be run
// is the stack's, not the caller's file.

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

func TestAgentDoorDeferRowCarriesItsClassAsItsReasonCode(t *testing.T) {
	restore := compressLiveness(t, 60*time.Second, 30*time.Second, 2)
	defer restore()
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(int64) string {
			time.Sleep(2500 * time.Millisecond) // past the 2 s ceiling: a budget defer
			return doneChat("too late")
		},
		repack: func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	contract := testContract()
	contract.TimeoutSec = 1
	p := contentionTestPipeline(t, srv.URL, 30)
	ledgerPath := filepath.Join(t.TempDir(), "ledger.jsonl")
	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	p.led = led
	defer func() { p.led = nil; _ = led.Close() }()

	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
	if !wire.Deferred || wire.DeferClass != core.DeferClassBudget {
		t.Fatalf("the scenario must be a budget defer: %+v", wire)
	}
	rows, err := ledger.ReadAll(ledgerPath)
	if err != nil || len(rows) != 1 {
		t.Fatalf("one ledger row: %v %+v", err, rows)
	}
	row := rows[0]
	if !row.Deferred || row.ReasonCode != ledger.ReasonBudget {
		t.Errorf("the deferred agent row must carry the class %q as its reason code, got %q (reason %q)", core.DeferClassBudget, row.ReasonCode, row.Reason)
	}
	// The words alone would not have said so: the class did the work.
	if got := ledger.DeferCode(row.ErrClass, row.Reason); got != ledger.ReasonOther {
		t.Errorf("the reason %q classifies as %q from its words; this test is meant to show the class carries it", row.Reason, got)
	}
}

// Every class the wire vocabulary has (core/agentwire.go) maps to a member of the ledger's closed set: a class
// added there and not in the ledger would file its rows as "other" again.
func TestEveryCoreDeferClassHasALedgerReasonCode(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "core", "agentwire.go"))
	if err != nil {
		t.Fatal(err)
	}
	classes := regexp.MustCompile(`(?m)^\s*DeferClass\w+\s*=\s*"(\w+)"`).FindAllSubmatch(src, -1)
	if len(classes) < 7 {
		t.Fatalf("found %d DeferClass constants in core/agentwire.go, want at least 7: the pin no longer reads them", len(classes))
	}
	for _, m := range classes {
		class := string(m[1])
		code := ledger.CodeForDeferClass(class)
		if code == "" || !ledger.IsReasonCode(code) {
			t.Errorf("core defer class %q has no reason code in the ledger (got %q): add it to ledger.deferClassCodes", class, code)
		}
	}
}

func TestTranscribeWithNoFFmpegIsFiledAsTheStacksFaultNotTheCallers(t *testing.T) {
	audio := filepath.Join(t.TempDir(), "talk.wav")
	if err := os.WriteFile(audio, []byte("RIFFjunk"), 0o644); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(t.TempDir(), "ledger.jsonl")
	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := gateCfg(t)
	cfg.MediaDir = t.TempDir()
	cfg.FFmpegPath = filepath.Join(t.TempDir(), "no-such-ffmpeg")
	p := New(cfg, llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 0), gateCache(t), led)

	res := p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: audio, Door: "offload_transcribe"})
	_ = led.Close()
	if res.OK || !res.Deferred {
		t.Fatalf("want a deferred result, got OK=%v Deferred=%v", res.OK, res.Deferred)
	}
	if res.Meta.ErrClass != core.ErrClassFFmpegUnavailable {
		t.Errorf("err_class = %q, want %q", res.Meta.ErrClass, core.ErrClassFFmpegUnavailable)
	}
	rows, err := ledger.ReadAll(ledgerPath)
	if err != nil || len(rows) != 1 {
		t.Fatalf("one ledger row: %v %+v", err, rows)
	}
	if rows[0].ReasonCode != ledger.ReasonConfig {
		t.Errorf("an ffmpeg that cannot be run is the box's config: reason_code = %q, want %q (row %+v)", rows[0].ReasonCode, ledger.ReasonConfig, rows[0])
	}
}
