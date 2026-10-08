package pipeline

// One video_watch call is ONE call (plan D12-D15, package h3): its per-window
// rows are INNER rows of the call's own row (ParentJobID = the call's job id),
// the savings counters read it once with each token counted once, and a held
// GPU stops the sweep after the first window instead of re-waiting the gate for
// every remaining window (12 x 90 s = 18 min on 2026-10-03).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// The test binary doubles as a fake ffmpeg/ffprobe: when it is started under a
// name whose stem is "ffmpeg" or "ffprobe" (fakeFFmpeg links/copies it so), it
// answers like the tool instead of running tests. That keeps the window loop
// testable with no real ffmpeg, so the test runs wherever the suite does.
func init() {
	stem := strings.ToLower(strings.TrimSuffix(filepath.Base(os.Args[0]), filepath.Ext(os.Args[0])))
	switch stem {
	case "ffprobe":
		os.Stdout.WriteString(os.Getenv("LO_FAKE_VIDEO_SECONDS") + "\n")
		os.Exit(0)
	case "ffmpeg":
		// the last argument is the frame pattern: <dir>/frame_%03d.jpg
		pattern := os.Args[len(os.Args)-1]
		out := strings.Replace(pattern, "%03d", "001", 1)
		_ = os.WriteFile(out, fakePNG, 0o644)
		os.Exit(0)
	}
}

var fakePNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53, 0xDE,
	0x00, 0x00, 0x00, 0x0C, 0x49, 0x44, 0x41, 0x54,
	0x08, 0xD7, 0x63, 0xF8, 0xCF, 0xC0, 0x00, 0x00, 0x00, 0x03, 0x00, 0x01,
	0x18, 0xDD, 0x8D, 0xB4,
	0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
}

// fakeFFmpeg returns the path of an "ffmpeg" next to an "ffprobe" (both the
// test binary) and a dummy video file, with the probe reporting seconds.
func fakeFFmpeg(t *testing.T, seconds string) (ffmpeg, video string) {
	t.Helper()
	dir := t.TempDir()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		dst := filepath.Join(dir, name+ext)
		if err := os.Link(os.Args[0], dst); err != nil {
			b, rerr := os.ReadFile(os.Args[0])
			if rerr != nil {
				t.Fatal(rerr)
			}
			if werr := os.WriteFile(dst, b, 0o755); werr != nil {
				t.Fatal(werr)
			}
		}
	}
	t.Setenv("LO_FAKE_VIDEO_SECONDS", seconds)
	video = filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(video, []byte("not a real video"), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "ffmpeg"+ext), video
}

// watchPipeline builds a Pipeline whose ledger is the returned path, over srv.
func watchPipeline(t *testing.T, srv *httptest.Server, ffmpeg string) (*Pipeline, string) {
	t.Helper()
	cfg := baseVisionCfg(srv, "fake-vlm")
	cfg.FFmpegPath = ffmpeg
	lpath := filepath.Join(t.TempDir(), "ledger.jsonl")
	led, err := ledger.Open(lpath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { led.Close() })
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	return New(cfg, client, nil, led), lpath
}

func watchReq(video string) core.Request {
	return core.Request{
		Task:   core.TaskVideoWatch,
		Video:  video,
		Door:   "offload_video_watch",
		Params: map[string]any{"question": "what happens?"},
	}
}

// answeringServer answers every vision window with winTokens prompt tokens and
// the text-seat synthesis with synthTokens (each completion is 8 tokens).
func answeringServer(winTokens, synthTokens int, windowCalls *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"image_url"`) {
			atomic.AddInt32(windowCalls, 1)
			_, _ = w.Write(fakeChat{content: "a cat walks", finishReason: "stop", promptTokens: winTokens}.marshal())
			return
		}
		_, _ = w.Write(fakeChat{content: "A cat walks through the clip.", finishReason: "stop", promptTokens: synthTokens}.marshal())
	}))
}

// writeHeldLease plants a live lease at path (same shape holdGPULock writes),
// for a test that must take the card in the middle of a call.
func writeHeldLease(path string) error {
	if err := os.MkdirAll(path, 0o777); err != nil {
		return err
	}
	now := time.Now()
	b, err := json.Marshal(&gpulease.Meta{
		Epoch:        1,
		Class:        gpulease.ClassMedia,
		Holder:       gpulease.Holder{PID: os.Getpid()},
		Reason:       "test generation job",
		AcquiredAtMs: now.UnixMilli(),
		RenewedAtMs:  now.UnixMilli(),
		ExpiresAtMs:  now.Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(path, "meta.json"), b, 0o666)
}

// rowStr is the one-line form of a ledger row in a failure message: the columns
// these tests are about, not the whole 70-field struct.
func rowStr(e ledger.Entry) string {
	return fmt.Sprintf("{task=%s model=%s job=%q parent=%q call=%q deferred=%v err=%q tokens_in=%d tokens_out=%d cards=%d}",
		e.Task, e.ModelTier, e.JobID, e.ParentJobID, e.CallID, e.Deferred, e.ErrClass, e.TokensIn, e.TokensOut, e.CardsTokens)
}

func rowsStr(rows []ledger.Entry) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = rowStr(r)
	}
	return strings.Join(parts, " ")
}

func readRows(t *testing.T, path string) []ledger.Entry {
	t.Helper()
	rows, err := ledger.ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// D12: three windows + a synthesis are one call. The summary row carries the
// minted job id and every window row names it as its parent; the savings
// summary counts ONE call, each token once; the cards figure sums the work.
func TestVideoWatchWindowRowsAreInnerRowsOfTheCall(t *testing.T) {
	var windowCalls int32
	srv := answeringServer(100, 30, &windowCalls)
	defer srv.Close()
	ffmpeg, video := fakeFFmpeg(t, "24") // 3 windows of 8 s
	p, lpath := watchPipeline(t, srv, ffmpeg)

	res := p.Run(context.Background(), watchReq(video))
	if !res.OK {
		t.Fatalf("video_watch deferred: %s", res.Reason)
	}
	if windowCalls != 3 {
		t.Fatalf("vision windows served = %d, want 3", windowCalls)
	}
	// The caller still sees the WHOLE call's tokens: 3x100 + 30 in, 3x8 + 8 out.
	if res.Meta.TokensIn != 330 || res.Meta.TokensOut != 32 {
		t.Fatalf("caller-facing tokens in/out = %d/%d, want 330/32", res.Meta.TokensIn, res.Meta.TokensOut)
	}

	rows := readRows(t, lpath)
	if len(rows) != 4 {
		t.Fatalf("ledger rows = %d, want 3 windows + 1 summary", len(rows))
	}
	var summary *ledger.Entry
	inner := 0
	for i := range rows {
		r := rows[i]
		if r.JobID != "" {
			if summary != nil {
				t.Fatal("two rows carry a job id: the call has one row of its own")
			}
			summary = &rows[i]
			continue
		}
		inner++
		if r.ParentJobID == "" {
			t.Fatalf("window row %d names no parent: it would be carded and counted as a call of its own", i)
		}
	}
	if summary == nil || inner != 3 {
		t.Fatalf("want one summary row and 3 inner rows, got summary=%v inner=%d", summary != nil, inner)
	}
	for _, r := range rows {
		if r.JobID == "" && r.ParentJobID != summary.JobID {
			t.Fatalf("window parent %q, want the summary's job id %q", r.ParentJobID, summary.JobID)
		}
	}
	if res.Meta.JobID != summary.JobID {
		t.Fatalf("caller-facing job id %q != the summary row's %q", res.Meta.JobID, summary.JobID)
	}

	s, err := ledger.SummarizeFile(lpath, 0, ledger.Prices{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Calls != 1 || s.Completed != 1 || s.ByTask["video_watch"] != 1 {
		t.Fatalf("one video_watch call counted as calls=%d completed=%d by_task=%v", s.Calls, s.Completed, s.ByTask)
	}
	if s.TokensSaved != 330 {
		t.Fatalf("tokens saved = %d, want 330 (3 x 100 window prompts + 30 synthesis prompt, each once)", s.TokensSaved)
	}
	if s.TokensOut != 32 {
		t.Fatalf("tokens out = %d, want 32 (3 x 8 window + 8 synthesis, each once)", s.TokensOut)
	}
	// The card work of the call: the summary row alone carries it (inner rows
	// record 0), and it must still include the window prompts.
	if summary.CardsTokens != 330+32 {
		t.Fatalf("summary cards_tokens = %d, want 362 (all prompt + completion work of the call)", summary.CardsTokens)
	}
}

// D13: a card held by a render stops the sweep after the FIRST window. The
// remaining windows are reported deferred with a short skipped note, write no
// row, and the call returns after ONE wait of the gate.
func TestVideoWatchStopsAfterFirstGPUBusyWindow(t *testing.T) {
	var windowCalls int32
	srv := answeringServer(100, 30, &windowCalls)
	defer srv.Close()
	ffmpeg, video := fakeFFmpeg(t, "40") // 5 windows
	p, lpath := watchPipeline(t, srv, ffmpeg)
	p.gpuLockPath = holdGPULock(t)
	p.visionGPUWait = 500 * time.Millisecond
	p.visionGPUPoll = 10 * time.Millisecond

	begin := time.Now()
	res := p.Run(context.Background(), watchReq(video))
	elapsed := time.Since(begin)
	// One wait of the gate (+ the ffmpeg stand-in's process start-up) is well
	// under 3.5 waits; five windows re-waiting would be 2.5 s before any overhead.
	// The row count below is the exact proof; this bound is the wall-clock one.
	if elapsed >= 7*p.visionGPUWait/2 {
		t.Fatalf("call took %v: the sweep re-waited the gate for later windows (5 windows x %v would be %v)", elapsed, p.visionGPUWait, 5*p.visionGPUWait)
	}
	if windowCalls != 0 {
		t.Fatalf("the vision seat was called %d times while the card was held", windowCalls)
	}
	if res.OK || !res.Deferred {
		t.Fatalf("want one defer, got OK=%v deferred=%v", res.OK, res.Deferred)
	}
	if res.Meta.ErrClass != "gpu_busy" {
		t.Fatalf("call err_class = %q, want gpu_busy (the defer class of the single-window defer)", res.Meta.ErrClass)
	}
	rows := readRows(t, lpath)
	if len(rows) != 2 {
		t.Fatalf("ledger rows = %d, want 1 window row + 1 call row (skipped windows write none)", len(rows))
	}
	win, call := rows[0], rows[1]
	if win.ParentJobID == "" || win.ErrClass != "gpu_busy" || !win.Deferred {
		t.Fatalf("window row = %s, want a deferred gpu_busy inner row", rowStr(win))
	}
	if call.JobID != win.ParentJobID || call.ErrClass != "gpu_busy" || !call.Deferred {
		t.Fatalf("call row = %s, want the parent (job id %q) deferred with err_class gpu_busy", rowStr(call), win.ParentJobID)
	}
}

// D13, the other half: windows already answered before the card was taken keep
// today's partial-result behaviour, and the unattempted tail is listed skipped.
func TestVideoWatchPartialResultKeepsAnsweredWindowsWhenTheCardIsTaken(t *testing.T) {
	var windowCalls int32
	srv := answeringServer(100, 30, &windowCalls)
	defer srv.Close()
	ffmpeg, video := fakeFFmpeg(t, "40") // 5 windows
	p, lpath := watchPipeline(t, srv, ffmpeg)
	lock := filepath.Join(t.TempDir(), "lease") // absent: the card is free
	p.gpuLockPath = lock
	p.visionGPUWait = 60 * time.Millisecond
	p.visionGPUPoll = 10 * time.Millisecond
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"image_url"`) {
			if atomic.AddInt32(&windowCalls, 1) == 2 {
				// A render takes the card while the second window is answered.
				_ = writeHeldLease(lock)
			}
			_, _ = w.Write(fakeChat{content: "a cat walks", finishReason: "stop", promptTokens: 100}.marshal())
			return
		}
		_, _ = w.Write(fakeChat{content: "A cat walks.", finishReason: "stop", promptTokens: 30}.marshal())
	}))
	defer srv2.Close()
	p.client = llamaclient.New(srv2.URL, p.cfg.CompletionPath, "", 10*time.Second)

	res := p.Run(context.Background(), watchReq(video))
	if !res.OK {
		t.Fatalf("two answered windows must still return a result, got defer: %s", res.Reason)
	}
	var out struct {
		Total    int `json:"windows_total"`
		Deferred int `json:"windows_deferred"`
		Windows  []struct {
			Deferred bool   `json:"deferred"`
			Reason   string `json:"reason"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != 5 || out.Deferred != 3 {
		t.Fatalf("windows total/deferred = %d/%d, want 5/3", out.Total, out.Deferred)
	}
	if out.Windows[2].Reason == "" || !strings.HasPrefix(out.Windows[3].Reason, "skipped: ") || !strings.HasPrefix(out.Windows[4].Reason, "skipped: ") {
		t.Fatalf("tail windows must carry a short skipped note: %+v", out.Windows)
	}
	rows := readRows(t, lpath)
	// 2 answered windows + 1 busy window + the call's row; the 2 skipped write none.
	if len(rows) != 4 {
		t.Fatalf("ledger rows = %d, want 4", len(rows))
	}
}

// D14: an all-deferred call whose windows deferred for a non-gpu reason still
// reports the common err_class, and quotes the MOST RECENT window's reason, not
// the first's (the age in a gpu-busy reason grows; the first one is stale).
func TestVideoWatchAllDeferredSummaryCarriesCommonErrClassAndLastReason(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusBadRequest) // http_4xx-style failure: not a 5xx, no retry
		_, _ = w.Write([]byte(`{"error":{"message":"bad window ` + string(rune('0'+c)) + `"}}`))
	}))
	defer srv.Close()
	ffmpeg, video := fakeFFmpeg(t, "24") // 3 windows
	p, lpath := watchPipeline(t, srv, ffmpeg)

	res := p.Run(context.Background(), watchReq(video))
	if res.OK || !res.Deferred {
		t.Fatalf("want an all-windows defer, got OK=%v", res.OK)
	}
	rows := readRows(t, lpath)
	if len(rows) != 4 {
		t.Fatalf("ledger rows = %d, want 3 windows + 1 call row", len(rows))
	}
	call := rows[3]
	if call.JobID == "" || call.ErrClass == "" || call.ErrClass != rows[0].ErrClass || call.ErrClass != rows[2].ErrClass {
		t.Fatalf("call row err_class %q must equal the windows' common class %q/%q", call.ErrClass, rows[0].ErrClass, rows[2].ErrClass)
	}
	if !strings.Contains(call.Reason, "bad window 3") || strings.Contains(call.Reason, "bad window 1") {
		t.Fatalf("call reason %q must quote the most recent window (3), not the first", call.Reason)
	}
}

// D14, mixed classes: no single class describes the call, so none is claimed.
func TestVideoWatchAllDeferredWithMixedClassesClaimsNone(t *testing.T) {
	if got := commonErrClass([]string{"gpu_busy", "gpu_busy", ""}); got != "" {
		t.Fatalf("mixed classes -> %q, want none", got)
	}
	if got := commonErrClass([]string{"gpu_busy", "gpu_busy"}); got != "gpu_busy" {
		t.Fatalf("common class -> %q, want gpu_busy", got)
	}
	if got := commonErrClass(nil); got != "" {
		t.Fatalf("no windows -> %q, want none", got)
	}
}
