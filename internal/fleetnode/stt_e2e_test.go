// An EXTERNAL test package (the pipeline imports fleetnode): the stt upload door end to end, over HTTP,
// through the real pipeline, with a stand-in for llama-swap's whisper upstream that aborts an inference
// an unload finds in flight, the way the real proxy does ("matrix: model unloaded").
package fleetnode_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/sttclient"
	"github.com/dmmdea/offload-harness/internal/sttremote"
)

// The test binary doubles as a fake ffmpeg: started under a name whose stem is "ffmpeg" it writes a
// non-empty file to its last argument, so the pipeline's convert step needs no real ffmpeg.
func init() {
	stem := strings.ToLower(strings.TrimSuffix(filepath.Base(os.Args[0]), filepath.Ext(os.Args[0])))
	if stem == "ffmpeg" {
		_ = os.WriteFile(os.Args[len(os.Args)-1], []byte("RIFFfake"), 0o644)
		os.Exit(0)
	}
}

// whisperSwap stands in for llama-swap on the routes a transcription and its unload use.
type whisperSwap struct {
	srv                   *httptest.Server
	hold                  time.Duration
	mu                    sync.Mutex
	inflight, peak, total int
	unloads               int
	aborted               int
	abort                 chan struct{}
}

func newWhisperSwap(t *testing.T, hold time.Duration) *whisperSwap {
	t.Helper()
	w := &whisperSwap{hold: hold, abort: make(chan struct{})}
	w.srv = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *whisperSwap) serve(rw http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/models":
		_, _ = rw.Write([]byte(`{"object":"list","data":[{"id":"whisper-stt"}]}`))
	case r.URL.Path == "/running":
		_, _ = rw.Write([]byte(`{"running":[{"model":"whisper-stt","state":"ready","ttl":300}]}`))
	case strings.HasPrefix(r.URL.Path, "/api/models/unload/"):
		w.mu.Lock()
		w.unloads++
		if w.inflight > 0 {
			close(w.abort)
			w.abort = make(chan struct{})
		}
		w.mu.Unlock()
	case strings.HasPrefix(r.URL.Path, "/upstream/"):
		w.mu.Lock()
		w.inflight++
		w.total++
		if w.inflight > w.peak {
			w.peak = w.inflight
		}
		abort := w.abort
		w.mu.Unlock()
		aborted := false
		select {
		case <-time.After(w.hold):
		case <-abort:
			aborted = true
		}
		w.mu.Lock()
		w.inflight--
		if aborted {
			w.aborted++
		}
		w.mu.Unlock()
		if aborted {
			http.Error(rw, `{"error":"unspecific error: matrix: model unloaded","src":"llama-swap"}`, http.StatusInternalServerError)
			return
		}
		_, _ = rw.Write([]byte(`{"language":"english","duration":2,"text":"hello there","segments":[{"id":0,"start":0,"end":1,"text":"hello"},{"id":1,"start":1,"end":2,"text":"there"}]}`))
	default:
		http.NotFound(rw, r)
	}
}

func (w *whisperSwap) stats() (peak, total, unloads, aborted int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.peak, w.total, w.unloads, w.aborted
}

func fakeFFmpegBinary(t *testing.T) string {
	t.Helper()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	dst := filepath.Join(t.TempDir(), "ffmpeg"+ext)
	if err := os.Link(os.Args[0], dst); err != nil {
		b, rerr := os.ReadFile(os.Args[0])
		if rerr != nil {
			t.Fatal(rerr)
		}
		if werr := os.WriteFile(dst, b, 0o755); werr != nil {
			t.Fatal(werr)
		}
	}
	return dst
}

// A burst of uploads through the real pipeline against a whisper that aborts what an unload finds in
// flight: every job finishes with its transcript, none fails "model unloaded", whisper never runs two
// at once, the model is freed ONCE after the last job (the gate's waiters keep it loaded for the
// jobs behind them), and no upload is left on disk.
//
// What this does and does not pin: the whisper client's process-wide mutex and its idle-only unload
// (register C-91) already keep one process from unloading a model under its own call, so removing the
// node's stt gate does not fail the "never fails" and "one at a time" lines here (the gate's order and cap
// are pinned in stt_upload_test.go). The single unload is the gate's doing: without sttclient.Queued each
// job would free the model before the next one reached the whisper client, and this test counts four.
func TestSTTUploadBurstThroughTheRealPipelineNeverFailsAndUnloadsOnce(t *testing.T) {
	swap := newWhisperSwap(t, 120*time.Millisecond)
	home := t.TempDir()
	cfg := config.Default()
	cfg.Home = home
	cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
	cfg.MediaDir = filepath.Join(home, "media")
	cfg.FFmpegPath = fakeFFmpegBinary(t)
	cfg.Endpoint = swap.srv.URL
	cfg.STTModel = "whisper-stt"
	cfg.STTUnloadAfter = true
	cfg.FleetAuthToken = "tok"
	cfg.FleetSTTMaxConcurrent = 1

	jobs := fleetnode.NewJobs(time.Hour, cfg.FleetConcurrencyLimit())
	t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })
	s := fleetnode.New(pipeline.New(cfg, nil, nil, nil), jobs, fleetnode.Options{
		NodeID: "testnode",
		Snapshot: func() (fleetnode.Snapshot, bool) {
			return fleetnode.Snapshot{TotalGiB: 16, FreeGiB: 12, At: time.Now()}, true
		},
		Footprints: func() []fleetnode.FootprintEntry { return nil },
		GpuVendor:  "nvidia",
		GpuArch:    "ampere",
		Cfg:        cfg,
	})
	node := httptest.NewServer(s.Handler())
	defer node.Close()

	do := func(method, path string, body []byte) (int, map[string]any) {
		req, _ := http.NewRequest(method, node.URL+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}

	const n = 4
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range ids {
		ids[i] = "stt-burst-" + string(rune('a'+i))
		body, _ := json.Marshal(map[string]any{"job_id": ids[i], "audio_b64": base64.StdEncoding.EncodeToString([]byte("OggS-" + ids[i])), "audio_ext": "ogg"})
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, m := do(http.MethodPost, "/fleet/stt", body); code != http.StatusAccepted {
				t.Errorf("dispatch %v: %d %v", ids[i], code, m)
			}
		}()
	}
	wg.Wait()

	for _, id := range ids {
		var m map[string]any
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			_, m = do(http.MethodGet, "/fleet/jobs/"+id, nil)
			if st, _ := m["state"].(string); st == "done" || st == "error" {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if m["state"] != "done" {
			t.Fatalf("%s ended %v: %v", id, m["state"], m)
		}
		var res struct {
			OK       bool            `json:"ok"`
			Deferred bool            `json:"deferred"`
			Reason   string          `json:"reason"`
			Result   json.RawMessage `json:"result"`
		}
		raw, _ := json.Marshal(m["data"])
		if err := json.Unmarshal(raw, &res); err != nil || !res.OK || res.Deferred {
			t.Fatalf("%s: result = %s (err %v), want a finished transcription (reason %q)", id, raw, err, res.Reason)
		}
		if !strings.Contains(string(res.Result), `"gist":"hello there"`) {
			t.Errorf("%s: transcript = %s", id, res.Result)
		}
	}
	peak, total, unloads, aborted := swap.stats()
	if aborted != 0 {
		t.Errorf("%d inference(s) were unloaded from under their job: the burst failed 'model unloaded'", aborted)
	}
	if peak != 1 || total != n {
		t.Errorf("whisper saw peak concurrency %d over %d request(s), want 1 over %d", peak, total, n)
	}
	if unloads != 1 {
		t.Errorf("%d unloads for a burst of %d jobs, want exactly one after the last (a job waiting at the gate keeps the model loaded)", unloads, n)
	}
	if left, _ := os.ReadDir(filepath.Join(cfg.MediaDir, ".stt-upload")); len(left) != 0 {
		t.Errorf("the private upload directory still holds %d file(s)", len(left))
	}
}

// nodeOverRealPipeline serves a real fleetnode.Server over a real pipeline whose whisper is swap.
func nodeOverRealPipeline(t *testing.T, swap *whisperSwap, mutate func(*config.Config)) (url string, cfg config.Config) {
	t.Helper()
	home := t.TempDir()
	cfg = config.Default()
	cfg.Home = home
	cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
	cfg.MediaDir = filepath.Join(home, "media")
	cfg.FFmpegPath = fakeFFmpegBinary(t)
	cfg.Endpoint = swap.srv.URL
	cfg.STTModel = "whisper-stt"
	cfg.FleetAuthToken = "tok"
	if mutate != nil {
		mutate(&cfg)
	}
	jobs := fleetnode.NewJobs(time.Hour, cfg.FleetConcurrencyLimit())
	t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })
	s := fleetnode.New(pipeline.New(cfg, nil, nil, nil), jobs, fleetnode.Options{
		NodeID: "node-e2e",
		Snapshot: func() (fleetnode.Snapshot, bool) {
			return fleetnode.Snapshot{TotalGiB: 16, FreeGiB: 12, At: time.Now()}, true
		},
		Footprints: func() []fleetnode.FootprintEntry { return nil },
		GpuVendor:  "nvidia",
		GpuArch:    "ampere",
		Cfg:        cfg,
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv.URL, cfg
}

type nopLocal struct{}

func (nopLocal) Run(_ context.Context, _ core.Request) core.Result {
	return core.Result{OK: false, Deferred: true, Reason: "the local runner must not run on route remote"}
}

// The asker (sttremote) against a REAL node over the real pipeline, so neither side is tested only
// against a fake of the other: the payload the asker builds is the one the node's door decodes, the
// result the real pipeline returns is the one the asker materializes, and a transcript the node cut at
// stt_max_inline_segments is completed from the node's own /fleet/media before the asker writes files.
func TestSTTRemoteAgainstARealNodeWritesLocalOutputs(t *testing.T) {
	for name, inline := range map[string]int{"whole transcript inline": 0, "node truncated its inline list": 1} {
		t.Run(name, func(t *testing.T) {
			swap := newWhisperSwap(t, 10*time.Millisecond)
			nodeURL, nodeCfg := nodeOverRealPipeline(t, swap, func(c *config.Config) { c.STTMaxInlineSegments = inline })

			asker := config.Default()
			asker.DelegateRemotes = []string{nodeURL}
			asker.FleetAuthToken = "tok"
			asker.MediaDir = filepath.Join(t.TempDir(), "asker-media")
			asker.FFmpegPath = fakeFFmpegBinary(t)
			src := filepath.Join(t.TempDir(), "interview.m4a")
			if err := os.WriteFile(src, []byte("not really audio"), 0o644); err != nil {
				t.Fatal(err)
			}

			res := sttremote.Run(context.Background(), asker, nopLocal{}, core.Request{
				Task: core.TaskTranscribe, Door: "offload_transcribe", Audio: src, Params: map[string]any{},
			}, "remote")
			if !res.OK || res.Deferred {
				t.Fatalf("result = %+v", res)
			}
			if res.Meta.Node != "node-e2e" || res.Meta.Placement != "remote: forced" || res.Meta.Model != "whisper-stt" {
				t.Fatalf("meta = %+v", res.Meta)
			}
			var out struct {
				Language    string              `json:"language"`
				NumSegments int                 `json:"num_segments"`
				Segments    []sttclient.Segment `json:"segments"`
				SRT         string              `json:"srt_path"`
				TXT         string              `json:"text_path"`
				JSON        string              `json:"json_path"`
			}
			if err := json.Unmarshal(res.Data, &out); err != nil {
				t.Fatal(err)
			}
			if out.NumSegments != 2 || len(out.Segments) == 0 {
				t.Fatalf("data = %s", res.Data)
			}
			for _, p := range []string{out.SRT, out.TXT, out.JSON} {
				if filepath.Dir(p) != asker.MediaDir {
					t.Errorf("%s is not under the asker's media_dir %s", p, asker.MediaDir)
				}
				if strings.HasPrefix(p, nodeCfg.MediaDir) {
					t.Errorf("%s names a path on the node", p)
				}
			}
			srt, _ := os.ReadFile(out.SRT)
			want := sttclient.SRT([]sttclient.Segment{{ID: 0, Start: 0, End: 1, Text: "hello"}, {ID: 1, Start: 1, End: 2, Text: "there"}})
			if string(srt) != want {
				t.Errorf("srt = %q, want %q: the asker's file must hold the node's WHOLE transcript", srt, want)
			}
			txt, _ := os.ReadFile(out.TXT)
			if string(txt) != "hello there" {
				t.Errorf("txt = %q", txt)
			}
			if left, _ := os.ReadDir(filepath.Join(nodeCfg.MediaDir, ".stt-upload")); len(left) != 0 {
				t.Errorf("the node kept %d upload file(s)", len(left))
			}
		})
	}
}
