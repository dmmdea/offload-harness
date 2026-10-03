package pipeline

// The inner-row rule for every path that writes more than one ledger row per
// call (plan D12, package h3; the video_watch case lives in
// pipeline_videowatch_cards_test.go). One call is one row of its own (JobID) plus
// inner rows (ParentJobID = that id): no counter reads the inner rows as calls
// and PAIR cards only the call's own row.
//
// Paths covered here, with the recon's F9 letters:
//   (b) video_describe's context-overflow retries
//   (c) the text cascade's escalating attempts
//   (d) extract_image's ocr + extract sub-calls
//   (e) inpaint_image's auto-text vqa sub-call
// RunImageBatch rows (f) are one per item by design and are left alone.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

func ledgerPipeline(t *testing.T, cfg config.Config, srv *httptest.Server) (*Pipeline, string) {
	t.Helper()
	lpath := filepath.Join(t.TempDir(), "ledger.jsonl")
	led, err := ledger.Open(lpath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { led.Close() })
	return New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, led), lpath
}

// assertOneCall checks the ledger holds exactly one call row (a JobID, no
// parent) and that every other row names it as parent, then returns the call
// row and the inner rows.
func assertOneCall(t *testing.T, rows []ledger.Entry, wantInner int) (ledger.Entry, []ledger.Entry) {
	t.Helper()
	var call *ledger.Entry
	var inner []ledger.Entry
	for i := range rows {
		if rows[i].JobID != "" && rows[i].ParentJobID == "" {
			if call != nil {
				t.Fatalf("two call rows in %s", rowsStr(rows))
			}
			call = &rows[i]
			continue
		}
		inner = append(inner, rows[i])
	}
	if call == nil {
		t.Fatalf("no row of its own carries the call's job id: %s", rowsStr(rows))
	}
	if len(inner) != wantInner {
		t.Fatalf("inner rows = %d, want %d: %s", len(inner), wantInner, rowsStr(rows))
	}
	for _, r := range inner {
		if r.ParentJobID != call.JobID {
			t.Fatalf("inner row parent %q, want the call's job id %q", r.ParentJobID, call.JobID)
		}
	}
	return *call, inner
}

// (b) video_describe: a clip too big for the context is retried at half the
// frame width. The overflowed attempt is a step of the call (inner); the final
// attempt is the call.
func TestVideoDescribeOverflowRetryRowsAreInnerRows(t *testing.T) {
	var calls int32
	overflow := int32(1) // image requests that answer "context overflow" before one succeeds
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if atomic.AddInt32(&overflow, -1) >= 0 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"request (9999 tokens) exceeds the available context size (8192 tokens)"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeChat{content: "a cat", finishReason: "stop", promptTokens: 100}.marshal())
	}))
	defer srv.Close()
	ffmpeg, video := fakeFFmpeg(t, "10")
	cfg := baseVisionCfg(srv, "fake-vlm")
	cfg.FFmpegPath, cfg.VideoFrameWidth = ffmpeg, 512
	p, lpath := ledgerPipeline(t, cfg, srv)

	res := p.Run(context.Background(), core.Request{Task: core.TaskVideoDescribe, Video: video, Door: "offload_video_describe", Params: map[string]any{"question": "what?"}})
	if !res.OK {
		t.Fatalf("deferred: %s", res.Reason)
	}
	call, inner := assertOneCall(t, readRows(t, lpath), 1)
	if call.Deferred || !inner[0].Deferred {
		t.Fatalf("the final attempt is the call (ok); the overflowed attempt is the inner row (deferred): call=%s inner=%s", rowStr(call), rowStr(inner[0]))
	}
	s, err := ledger.SummarizeFile(lpath, 0, ledger.Prices{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Calls != 1 || s.Completed != 1 || s.Deferred != 0 {
		t.Fatalf("one video_describe call counted as calls=%d completed=%d deferred=%d", s.Calls, s.Completed, s.Deferred)
	}
}

// (b) every attempt overflowing: the last one is the floor (width 256), so it is
// the call's row — the call has a row of its own to carry the defer.
func TestVideoDescribeOverflowAtTheFloorStillHasACallRow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"request exceeds the available context size"}}`))
	}))
	defer srv.Close()
	ffmpeg, video := fakeFFmpeg(t, "10")
	cfg := baseVisionCfg(srv, "fake-vlm")
	cfg.FFmpegPath, cfg.VideoFrameWidth = ffmpeg, 512
	p, lpath := ledgerPipeline(t, cfg, srv)

	res := p.Run(context.Background(), core.Request{Task: core.TaskVideoDescribe, Video: video, Params: map[string]any{"question": "what?"}})
	if res.OK || !res.Deferred {
		t.Fatalf("want a defer, got OK=%v", res.OK)
	}
	call, _ := assertOneCall(t, readRows(t, lpath), 1) // 512 -> 256: one inner row, then the floor
	if !call.Deferred {
		t.Fatalf("the floor attempt's defer is the call's row: %s", rowStr(call))
	}
	s, _ := ledger.SummarizeFile(lpath, 0, ledger.Prices{})
	if s.Calls != 1 || s.Deferred != 1 {
		t.Fatalf("calls=%d deferred=%d, want 1/1", s.Calls, s.Deferred)
	}
}

// (b) the common case stays byte-identical: a call that never overflowed writes
// ONE plain row with neither a job id nor a parent.
func TestVideoDescribeWithoutOverflowWritesOnePlainRow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeChat{content: "a cat", finishReason: "stop", promptTokens: 100}.marshal())
	}))
	defer srv.Close()
	ffmpeg, video := fakeFFmpeg(t, "10")
	cfg := baseVisionCfg(srv, "fake-vlm")
	cfg.FFmpegPath, cfg.VideoFrameWidth = ffmpeg, 512
	p, lpath := ledgerPipeline(t, cfg, srv)

	if res := p.Run(context.Background(), core.Request{Task: core.TaskVideoDescribe, Video: video, Params: map[string]any{"question": "what?"}}); !res.OK {
		t.Fatalf("deferred: %s", res.Reason)
	}
	rows := readRows(t, lpath)
	if len(rows) != 1 || rows[0].JobID != "" || rows[0].ParentJobID != "" {
		t.Fatalf("want one plain row, got %s", rowsStr(rows))
	}
}

// cascadeServer: the entry tier answers with a thin decision margin (so the
// gate climbs); the escalation tier answers per escReply.
func cascadeServer(entryModel, escModel string, escReply fakeChat) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch body.Model {
		case entryModel:
			_, _ = w.Write(fakeChat{content: `{"decision":"yes","reason":"likely"}`, finishReason: "stop", promptTokens: 100, logprobs: tokenizeDecision("likely")}.marshal())
		case escModel:
			_, _ = w.Write(escReply.marshal())
		default:
			http.Error(w, "unexpected model "+body.Model, http.StatusBadRequest)
		}
	}))
}

func cascadeCfg(srv *httptest.Server, entryModel, escModel string) config.Config {
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.Model = escModel
	cfg.TriageModel = entryModel
	cfg.EscalationModel, cfg.ReasoningModel = "", ""
	cfg.MaxRetries = 0
	cfg.ThresholdsPath, cfg.RouterWeightsPath, cfg.TierOverridesPath, cfg.ConfHeadLabelsPath = "", "", "", ""
	return cfg
}

var escTriageReq = core.Request{
	Task:   core.TaskTriage,
	Input:  "The customer reports the invoice was charged twice and wants a refund processed today.",
	Params: map[string]any{"question": "Is this a billing issue?"},
}

// (c) a margin escalation: the escalating attempt's row is an inner row of the
// call; the tier that answered is the call.
func TestCascadeEscalatingAttemptIsAnInnerRow(t *testing.T) {
	const entryModel, escModel = "fake-e2b", "fake-e4b"
	srv := cascadeServer(entryModel, escModel, fakeChat{content: `{"decision":"yes","reason":"confirmed"}`, finishReason: "stop", promptTokens: 120})
	defer srv.Close()
	p, lpath := ledgerPipeline(t, cascadeCfg(srv, entryModel, escModel), srv)

	if res := p.Run(context.Background(), escTriageReq); !res.OK {
		t.Fatalf("cascade must succeed on the escalation tier: %+v", res)
	}
	call, inner := assertOneCall(t, readRows(t, lpath), 1)
	if call.ModelTier != escModel || inner[0].ModelTier != entryModel {
		t.Fatalf("the answering tier is the call (%s), the climbing tier the inner row (%s): call=%q inner=%q", escModel, entryModel, call.ModelTier, inner[0].ModelTier)
	}
	if inner[0].EscSource != string(core.EscMargin) {
		t.Fatalf("the inner row keeps what D-127 recorded on it: esc_source = %q", inner[0].EscSource)
	}
	s, err := ledger.SummarizeFile(lpath, 0, ledger.Prices{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Calls != 1 || s.Completed != 1 {
		t.Fatalf("one escalated call counted as calls=%d completed=%d", s.Calls, s.Completed)
	}
}

// (c) the whole ladder failing: the escalating attempt's row stays inner, and the
// final defer — formerly a SECOND row for that last attempt — is the call.
func TestCascadeAllTiersFailingKeepsOneCallRow(t *testing.T) {
	const entryModel, escModel = "fake-e2b", "fake-e4b"
	srv := cascadeServer(entryModel, escModel, fakeChat{content: "not json at all", finishReason: "stop", promptTokens: 120})
	defer srv.Close()
	p, lpath := ledgerPipeline(t, cascadeCfg(srv, entryModel, escModel), srv)

	res := p.Run(context.Background(), escTriageReq)
	if res.OK || !res.Deferred {
		t.Fatalf("want a defer, got OK=%v", res.OK)
	}
	call, _ := assertOneCall(t, readRows(t, lpath), 1)
	if !call.Deferred {
		t.Fatalf("the final defer is the call's row: %s", rowStr(call))
	}
	s, _ := ledger.SummarizeFile(lpath, 0, ledger.Prices{})
	if s.Calls != 1 || s.Deferred != 1 {
		t.Fatalf("calls=%d deferred=%d, want 1/1", s.Calls, s.Deferred)
	}
}

// (c) a call that answers at the entry tier writes one plain row.
func TestCascadeWithoutEscalationWritesOnePlainRow(t *testing.T) {
	const entryModel, escModel = "fake-e2b", "fake-e4b"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeChat{content: `{"decision":"yes","reason":"clear"}`, finishReason: "stop", promptTokens: 100}.marshal())
	}))
	defer srv.Close()
	p, lpath := ledgerPipeline(t, cascadeCfg(srv, entryModel, escModel), srv)

	if res := p.Run(context.Background(), escTriageReq); !res.OK {
		t.Fatalf("deferred: %s", res.Reason)
	}
	rows := readRows(t, lpath)
	if len(rows) != 1 || rows[0].JobID != "" || rows[0].ParentJobID != "" {
		t.Fatalf("want one plain row, got %s", rowsStr(rows))
	}
}

// (d) extract_image is a composite: the ocr and extract sub-calls are inner rows
// of one row the composite writes for itself. Its tokens: output is the whole
// call's; the prompts stay on the inner rows (their savings); the cards figure
// is the work of both.
func TestExtractImageSubCallsAreInnerRowsOfOneCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"image_url"`) {
			_, _ = w.Write(fakeChat{content: "Invoice #A-204\nTOTAL 4999", finishReason: "stop", promptTokens: 50}.marshal())
			return
		}
		_, _ = w.Write(fakeChat{content: `{"invoice_no":"A-204","total":4999}`, finishReason: "stop", promptTokens: 60}.marshal())
	}))
	defer srv.Close()
	cfg := baseVisionCfg(srv, "fake-vlm")
	cfg.Model = "fake-e4b"
	p, lpath := ledgerPipeline(t, cfg, srv)

	res := p.Run(context.Background(), core.Request{Task: core.TaskExtractImage, Image: minimalPNGDataURI(), Door: "offload_extract_image", Params: map[string]any{"schema": etestSchema()}})
	if !res.OK {
		t.Fatalf("deferred: %s", res.Reason)
	}
	call, inner := assertOneCall(t, readRows(t, lpath), 2)
	if call.Task != "extract_image" || call.Door != "offload_extract_image" || call.Deferred {
		t.Fatalf("composite row = %s", rowStr(call))
	}
	tasks := map[string]bool{inner[0].Task: true, inner[1].Task: true}
	if !tasks["ocr"] || !tasks["extract"] {
		t.Fatalf("inner rows must be the ocr and extract sub-calls, got %v", tasks)
	}
	s, err := ledger.SummarizeFile(lpath, 0, ledger.Prices{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Calls != 1 || s.ByTask["extract_image"] != 1 || s.Completed != 1 {
		t.Fatalf("one extract_image call counted as calls=%d by_task=%v completed=%d", s.Calls, s.ByTask, s.Completed)
	}
	if s.TokensSaved != 50+60 {
		t.Fatalf("tokens saved = %d, want 110 (the two sub-call prompts, each once)", s.TokensSaved)
	}
	if s.TokensOut != 16 {
		t.Fatalf("tokens out = %d, want 16 (8 + 8, each once)", s.TokensOut)
	}
	if call.CardsTokens != (50+8)+(60+8) {
		t.Fatalf("composite cards_tokens = %d, want 126 (the work of both sub-calls)", call.CardsTokens)
	}
}

// (d) when the OCR sub-call defers the extract never runs; the composite still
// writes its row, or the OCR row would be an orphan with no card.
func TestExtractImageOCRDeferStillHasACallRow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeChat{content: "   ", finishReason: "stop", promptTokens: 50}.marshal())
	}))
	defer srv.Close()
	cfg := baseVisionCfg(srv, "fake-vlm")
	p, lpath := ledgerPipeline(t, cfg, srv)

	res := p.Run(context.Background(), core.Request{Task: core.TaskExtractImage, Image: minimalPNGDataURI(), Params: map[string]any{"schema": etestSchema()}})
	if res.OK {
		t.Fatal("want the OCR defer to propagate")
	}
	call, inner := assertOneCall(t, readRows(t, lpath), 1)
	if !call.Deferred || call.Task != "extract_image" || inner[0].Task != "ocr" {
		t.Fatalf("call=%s inner=%s", rowStr(call), rowStr(inner[0]))
	}
	s, _ := ledger.SummarizeFile(lpath, 0, ledger.Prices{})
	if s.Calls != 1 || s.Deferred != 1 {
		t.Fatalf("calls=%d deferred=%d, want 1/1", s.Calls, s.Deferred)
	}
}

// writePNGFile writes the 1x1 test PNG to dir and returns its path.
func writePNGFile(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "in.png")
	if err := os.WriteFile(p, fakePNG, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// (e) inpaint auto-text: the vqa box detector is a sub-call of the inpaint call;
// its row is an inner row of the inpaint row (which every exit writes).
func TestInpaintAutoTextDetectorRowIsAnInnerRow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeChat{content: "  ", finishReason: "stop", promptTokens: 40}.marshal()) // empty: the detector defers
	}))
	defer srv.Close()
	cfg := baseVisionCfg(srv, "fake-vlm")
	cfg.InpaintScript, cfg.InpaintCkpt = "inpaint.mjs", "ckpt.safetensors"
	cfg.MediaDir = t.TempDir()
	p, lpath := ledgerPipeline(t, cfg, srv)
	img := writePNGFile(t, t.TempDir())

	res := p.Run(context.Background(), core.Request{Task: core.TaskInpaintImage, Input: "clean", Door: "offload_inpaint_image",
		Params: map[string]any{"image": img, "auto_text": true}})
	if res.OK || !strings.Contains(res.Reason, "auto text localization failed") {
		t.Fatalf("want the auto-text defer, got OK=%v reason=%q", res.OK, res.Reason)
	}
	call, inner := assertOneCall(t, readRows(t, lpath), 1)
	if call.Task != "inpaint_image" || inner[0].Task != "vqa" || !call.Deferred {
		t.Fatalf("call=%s inner=%s", rowStr(call), rowStr(inner[0]))
	}
	s, _ := ledger.SummarizeFile(lpath, 0, ledger.Prices{})
	if s.Calls != 1 || s.ByTask["inpaint_image"] != 1 || s.ByTask["vqa"] != 0 {
		t.Fatalf("calls=%d by_task=%v: the detector must not count as a call", s.Calls, s.ByTask)
	}
}

// (e) an inpaint call that uses no auto-text keeps its plain single row.
func TestInpaintWithoutAutoTextWritesOnePlainRow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := baseVisionCfg(srv, "fake-vlm")
	cfg.InpaintScript, cfg.InpaintCkpt = "inpaint.mjs", "ckpt.safetensors"
	p, lpath := ledgerPipeline(t, cfg, srv)
	p.Run(context.Background(), core.Request{Task: core.TaskInpaintImage, Input: "clean", Params: map[string]any{"image": "x.png"}})
	rows := readRows(t, lpath)
	if len(rows) != 1 || rows[0].JobID != "" || rows[0].ParentJobID != "" {
		t.Fatalf("want one plain row, got %s", rowsStr(rows))
	}
}

// D15: Run stamps the id Begin returned on the call's Meta, so the call's own
// ledger row names the card it closes.
func TestRunStampsTheCallIDOnTheCallsRow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := baseVisionCfg(srv, "fake-vlm")
	cfg.InpaintScript, cfg.InpaintCkpt = "", "" // the call defers at once, writing its row
	p, lpath := ledgerPipeline(t, cfg, srv)
	p.SetCallTracker(&recTracker{})

	p.Run(context.Background(), core.Request{Task: core.TaskInpaintImage, Input: "clean", Params: map[string]any{"image": "x.png"}})
	rows := readRows(t, lpath)
	if len(rows) != 1 || rows[0].CallID != "call-test-1" {
		t.Fatalf("the row must carry the call id Begin returned, got %s", rowsStr(rows))
	}
}

// A request cannot mark its own row inner over the wire: ParentJobID on a
// core.Request is in-process only.
func TestRequestParentJobIDIsNeverOnTheWire(t *testing.T) {
	b, err := json.Marshal(core.Request{Task: core.TaskVQA, ParentJobID: "forged"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "forged") {
		t.Fatalf("Request.ParentJobID leaked into JSON: %s", b)
	}
	var r core.Request
	if err := json.Unmarshal([]byte(`{"task":"vqa","parent_job_id":"forged"}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.ParentJobID != "" {
		t.Fatalf("a wire request set its own ParentJobID: %q", r.ParentJobID)
	}
}
