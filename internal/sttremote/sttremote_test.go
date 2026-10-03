package sttremote

import (
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
	"github.com/dmmdea/offload-harness/internal/sttclient"
)

// The test binary doubles as a fake ffmpeg: started under a name whose stem is "ffmpeg" it writes
// LO_FAKE_OUT_TEXT to its last argument (LO_FAKE_FAIL=1 makes it exit 1 instead), so the asker's
// Opus conversion is testable with no real ffmpeg.
func init() {
	stem := strings.ToLower(strings.TrimSuffix(filepath.Base(os.Args[0]), filepath.Ext(os.Args[0])))
	if stem != "ffmpeg" {
		return
	}
	if os.Getenv("LO_FAKE_FAIL") == "1" {
		os.Stderr.WriteString("fake ffmpeg: no libopus")
		os.Exit(1)
	}
	_ = os.WriteFile(os.Args[len(os.Args)-1], []byte(os.Getenv("LO_FAKE_OUT_TEXT")), 0o644)
	os.Exit(0)
}

const fakeOpus = "OggS-fake-opus-bytes"

// fakeFFmpeg returns an "ffmpeg" (the test binary) whose conversion writes fakeOpus, or fails.
func fakeFFmpeg(t *testing.T, fail bool) string {
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
	t.Setenv("LO_FAKE_OUT_TEXT", fakeOpus)
	if fail {
		t.Setenv("LO_FAKE_FAIL", "1")
	} else {
		t.Setenv("LO_FAKE_FAIL", "")
	}
	return dst
}

var sampleSegments = []sttclient.Segment{
	{ID: 0, Start: 0, End: 2.5, Text: " Hello there."},
	{ID: 1, Start: 2.5, End: 5.0, Text: " This is a test."},
	{ID: 2, Start: 5.0, End: 8.25, Text: " Goodbye."},
}

// nodeDir is a path on the SERVING node's disk, as the node's result carries them.
const nodeDir = "/srv/node-media/"

// sttData is the node's transcribeResult JSON. truncated=true inlines only the first segment, the
// rest living in the node's .segments.json (jsonPath).
func sttData(segs []sttclient.Segment, truncated bool, jsonPath string) json.RawMessage {
	inline := segs
	if truncated {
		inline = segs[:1]
	}
	b, _ := json.Marshal(map[string]any{
		"language": "en", "duration_sec": 8.25, "num_segments": len(segs), "gist": "Hello there. This is a test. Goodbye.",
		"segments": inline, "segments_truncated": truncated,
		"srt_path": nodeDir + "up-1.srt", "text_path": nodeDir + "up-1.txt", "json_path": jsonPath,
	})
	return b
}

func okResult(segs []sttclient.Segment, truncated bool) core.Result {
	return core.Result{OK: true, Data: sttData(segs, truncated, nodeDir+"up-1.segments.json"), Meta: core.Meta{Model: "whisper-stt", LatencyMs: 4321}}
}

// fakeNode is a fleet node that advertises the stt upload door (or not), records the upload it
// receives and answers the job with a core.Result.
type fakeNode struct {
	node   string
	tasks  []string
	hq     *bool // health stt_hq; nil = not published
	capMB  int   // health stt_upload_max_mb; 0 = not published
	leased bool
	result core.Result
	refuse int // non-zero: answer the dispatch with this status
	// media serves GET /fleet/media/{name}; a name it lacks is a 404.
	media map[string][]byte

	mu           sync.Mutex
	payload      map[string]any
	auth         string
	hdr          http.Header
	runningFirst bool
	stayRunning  bool // every poll answers running: a job that never finishes
	jobStatus    int  // non-zero: answer every job poll with this HTTP status
	polls        int
	mediaReqs    []string
	mediaAuth    []string
	srv          *httptest.Server
}

func newFakeNode(t *testing.T, node string, tasks []string, res core.Result) *fakeNode {
	f := &fakeNode{node: node, tasks: tasks, result: res, media: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) {
		h := map[string]any{"node_id": node, "supported_task_types": tasks, "queue_depth": 0}
		if f.hq != nil {
			h["stt_hq"] = *f.hq
		}
		if f.capMB != 0 {
			h["stt_upload_max_mb"] = f.capMB
		}
		if f.leased {
			h["lease"] = map[string]any{"held": true, "class": "text", "busy": true}
		}
		_ = json.NewEncoder(w).Encode(h)
	})
	mux.HandleFunc("POST /fleet/stt", func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		f.payload = p
		f.auth = r.Header.Get("Authorization")
		f.hdr = r.Header.Clone()
		f.mu.Unlock()
		if f.refuse != 0 {
			w.WriteHeader(f.refuse)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "error": "refused for the test"})
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"job_id": p["job_id"], "status": "accepted"})
	})
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.polls++
		first := f.polls == 1
		f.mu.Unlock()
		if f.jobStatus != 0 {
			http.Error(w, "job poll refused for the test", f.jobStatus)
			return
		}
		if f.stayRunning || f.runningFirst && first {
			_ = json.NewEncoder(w).Encode(map[string]any{"job_id": r.PathValue("id"), "state": "running"})
			return
		}
		data, _ := json.Marshal(f.result)
		_ = json.NewEncoder(w).Encode(map[string]any{"job_id": r.PathValue("id"), "state": "done", "data": json.RawMessage(data)})
	})
	mux.HandleFunc("GET /fleet/media/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.mediaReqs = append(f.mediaReqs, r.PathValue("name"))
		f.mediaAuth = append(f.mediaAuth, r.Header.Get("Authorization"))
		b, ok := f.media[r.PathValue("name")]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNode) dispatched() (map[string]any, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.payload, f.auth
}

func (f *fakeNode) dispatchHeader() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hdr.Clone()
}

func (f *fakeNode) uploaded(t *testing.T) []byte {
	t.Helper()
	p, _ := f.dispatched()
	if p == nil {
		t.Fatal("the node received no upload")
	}
	b, err := base64.StdEncoding.DecodeString(p["audio_b64"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var sttTasks = []string{"stt", "stt-upload"}

// localRunner records whether the in-process path ran.
type localRunner struct {
	mu   sync.Mutex
	runs []core.Request
	res  core.Result
}

func (l *localRunner) Run(ctx context.Context, req core.Request) core.Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runs = append(l.runs, req)
	return l.res
}

func (l *localRunner) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.runs)
}

// setBusy replaces the auto route's trigger and records the hq flag it was asked about.
func setBusy(t *testing.T, busy bool) *bool {
	t.Helper()
	asked := new(bool)
	prev := localBusy
	localBusy = func(ctx context.Context, cfg config.Config, hq bool) bool { *asked = hq; return busy }
	t.Cleanup(func() { localBusy = prev })
	return asked
}

// srcAudio writes a source recording and returns its path.
func srcAudio(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "interview.m4a")
	if err := os.WriteFile(p, []byte("not-really-m4a-audio-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func sttReq(path string, params map[string]any) core.Request {
	if params == nil {
		params = map[string]any{}
	}
	return core.Request{Task: core.TaskTranscribe, Door: "offload_transcribe", Audio: path, Params: params}
}

// askerCfg is the asking box: it lists the node, holds the token, has a media dir and a fake ffmpeg.
func askerCfg(t *testing.T, f *fakeNode, ffmpeg string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.DelegateRemotes = []string{f.srv.URL}
	cfg.FleetAuthToken = "tok"
	cfg.MediaDir = t.TempDir()
	cfg.FFmpegPath = ffmpeg
	cfg.STTModel = "whisper-stt"
	return cfg
}

var localOK = core.Result{OK: true, Data: json.RawMessage(`{"language":"en","gist":"local"}`), Meta: core.Meta{Model: "whisper-stt"}}

// "" and "local" run in-process with no stamp and never touch the wire: byte-identical to before the
// route existed.
func TestLocalRouteIsByteIdenticalAndNeverTouchesTheWire(t *testing.T) {
	setBusy(t, true) // even a busy card: local is local
	node := newFakeNode(t, "node-c", sttTasks, okResult(sampleSegments, false))
	cfg := askerCfg(t, node, fakeFFmpeg(t, false))
	local := &localRunner{res: localOK}
	for _, route := range []string{"", "local", "LOCAL"} {
		res := Run(context.Background(), cfg, local, sttReq(srcAudio(t), nil), route)
		if !res.OK || string(res.Data) != string(localOK.Data) || res.Meta.Placement != "" || res.Meta.Node != "" || res.Reason != "" {
			t.Fatalf("route %q: %+v, want the local result unstamped", route, res)
		}
	}
	if p, _ := node.dispatched(); p != nil {
		t.Fatalf("local route dispatched to the node: %v", p)
	}
	if local.count() != 3 {
		t.Fatalf("local ran %d times, want 3", local.count())
	}
}

// D19: a local call that defers gpu_busy while fleet nodes are configured says a node could take it.
// Only that defer, only the local route, only with remotes: everything else is untouched.
func TestLocalGPUBusyDeferHintsAtTheFleetRoute(t *testing.T) {
	busy := core.Deferf("gpu busy: a media job holds the GPU", "", core.Meta{ErrClass: "gpu_busy"})
	busy.DeferClass = core.DeferClassCapacity
	with := config.Default()
	with.DelegateRemotes = []string{"http://127.0.0.1:1"}
	without := config.Default()

	res := Run(context.Background(), with, &localRunner{res: busy}, sttReq("a.wav", nil), "local")
	if !strings.HasPrefix(res.Reason, busy.Reason) || !strings.Contains(res.Reason, `route "auto" or "remote"`) || !strings.Contains(res.Reason, "fleet node") {
		t.Fatalf("reason = %q, want the original text ending with a hint at route auto or remote", res.Reason)
	}
	if res.DeferClass != core.DeferClassCapacity || res.Meta.ErrClass != "gpu_busy" || !res.Deferred {
		t.Fatalf("the hint changed the defer's class or error class: %+v", res)
	}
	if res := Run(context.Background(), with, &localRunner{res: busy}, sttReq("a.wav", nil), ""); !strings.Contains(res.Reason, `route "auto" or "remote"`) {
		t.Errorf("the default (empty) route is local and gets the hint too: %q", res.Reason)
	}
	if res := Run(context.Background(), without, &localRunner{res: busy}, sttReq("a.wav", nil), "local"); res.Reason != busy.Reason {
		t.Errorf("no delegate_remotes: reason %q, want it untouched (there is no fleet to name)", res.Reason)
	}
	other := core.Deferf("empty transcript (no speech detected)", "", core.Meta{})
	if res := Run(context.Background(), with, &localRunner{res: other}, sttReq("a.wav", nil), "local"); res.Reason != other.Reason {
		t.Errorf("a defer that is not gpu_busy got a hint: %q", res.Reason)
	}
	fail := core.Deferf("transcribe call failed: boom", "", core.Meta{ErrClass: "timeout"})
	if res := Run(context.Background(), with, &localRunner{res: fail}, sttReq("a.wav", nil), "local"); res.Reason != fail.Reason {
		t.Errorf("another error class got a hint: %q", res.Reason)
	}
}

// route remote: the caller's audio is converted to Opus, shipped with the bearer, and the node's
// transcript comes back with files written HERE under media_dir: the result's paths are local.
func TestRemoteUploadsOpusAndWritesLocalOutputs(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-c-ampere16", sttTasks, okResult(sampleSegments, false))
	cfg := askerCfg(t, node, fakeFFmpeg(t, false))
	local := &localRunner{}
	src := srcAudio(t)

	res := Run(context.Background(), cfg, local, sttReq(src, map[string]any{"language": "es"}), "remote")
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	if local.count() != 0 {
		t.Fatal("route remote must never run the local seat")
	}
	p, auth := node.dispatched()
	if auth != "Bearer tok" {
		t.Errorf("Authorization = %q, want the fleet bearer", auth)
	}
	if got := node.uploaded(t); string(got) != fakeOpus {
		t.Errorf("the node received %q, want the Opus conversion %q", got, fakeOpus)
	}
	if p["audio_ext"] != "ogg" || p["language"] != "es" || !strings.HasPrefix(p["job_id"].(string), "stt-") {
		t.Errorf("payload = %v, want ogg, language es and an stt- job id", p)
	}
	if _, has := p["hq"]; has {
		t.Errorf("hq sent although not asked: %v", p)
	}
	if res.Meta.Node != "node-c-ampere16" || res.Meta.Placement != "remote: forced" || res.Meta.Model != "whisper-stt" {
		t.Fatalf("meta = %+v, want node + placement stamped over the node's meta", res.Meta)
	}

	var out struct {
		Language          string              `json:"language"`
		NumSegments       int                 `json:"num_segments"`
		Segments          []sttclient.Segment `json:"segments"`
		SegmentsTruncated bool                `json:"segments_truncated"`
		SRT               string              `json:"srt_path"`
		TXT               string              `json:"text_path"`
		JSON              string              `json:"json_path"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Language != "en" || out.NumSegments != 3 || len(out.Segments) != 3 || out.SegmentsTruncated {
		t.Fatalf("data = %+v", out)
	}
	for ext, path := range map[string]string{".srt": out.SRT, ".txt": out.TXT, ".segments.json": out.JSON} {
		if filepath.Dir(path) != cfg.MediaDir || !strings.HasPrefix(filepath.Base(path), "interview-") || !strings.HasSuffix(path, ext) {
			t.Errorf("%s path %q: want a file under this box's media_dir named after the source", ext, path)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s: %v", ext, err)
		}
		if strings.HasPrefix(path, nodeDir) {
			t.Errorf("%s path is the NODE's path", ext)
		}
	}
	srt, _ := os.ReadFile(out.SRT)
	if string(srt) != sttclient.SRT(sampleSegments) {
		t.Errorf("srt = %q, want %q", srt, sttclient.SRT(sampleSegments))
	}
	txt, _ := os.ReadFile(out.TXT)
	if string(txt) != "Hello there. This is a test. Goodbye." {
		t.Errorf("txt = %q", txt)
	}
	var js []sttclient.Segment
	raw, _ := os.ReadFile(out.JSON)
	if err := json.Unmarshal(raw, &js); err != nil || len(js) != 3 || js[2].Text != " Goodbye." {
		t.Errorf("segments.json = %s (err %v)", raw, err)
	}
}

// The inline segment list follows THIS box's stt_max_inline_segments, as a local call's does; the
// files always hold every segment.
func TestRemoteInlineSegmentsFollowTheAskersCap(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-c", sttTasks, okResult(sampleSegments, false))
	cfg := askerCfg(t, node, fakeFFmpeg(t, false))
	cfg.STTMaxInlineSegments = 2
	res := Run(context.Background(), cfg, &localRunner{}, sttReq(srcAudio(t), nil), "remote")
	var out struct {
		Segments  []sttclient.Segment `json:"segments"`
		Truncated bool                `json:"segments_truncated"`
		JSON      string              `json:"json_path"`
		Num       int                 `json:"num_segments"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil || len(out.Segments) != 2 || !out.Truncated || out.Num != 3 {
		t.Fatalf("data = %s (err %v), want 2 inline, truncated, 3 in all", res.Data, err)
	}
	var js []sttclient.Segment
	raw, _ := os.ReadFile(out.JSON)
	if json.Unmarshal(raw, &js) != nil || len(js) != 3 {
		t.Fatalf("segments.json holds %d segments, want all 3", len(js))
	}
}

// A node that inlined only part of the transcript: the rest is fetched from its /fleet/media (by the
// basename of the path in the result, however the node spells paths) and the local files are complete.
func TestRemoteFetchesTheFullSegmentListWhenTheNodeTruncated(t *testing.T) {
	setBusy(t, false)
	for name, jsonPath := range map[string]string{"posix": nodeDir + "up-1.segments.json", "windows": `C:\node\media\up-1.segments.json`} {
		t.Run(name, func(t *testing.T) {
			res := core.Result{OK: true, Data: sttData(sampleSegments, true, jsonPath), Meta: core.Meta{Model: "whisper-stt"}}
			node := newFakeNode(t, "node-c", sttTasks, res)
			full, _ := json.MarshalIndent(sampleSegments, "", "  ")
			node.media["up-1.segments.json"] = full
			cfg := askerCfg(t, node, fakeFFmpeg(t, false))
			got := Run(context.Background(), cfg, &localRunner{}, sttReq(srcAudio(t), nil), "remote")
			if !got.OK {
				t.Fatalf("result = %+v", got)
			}
			var out struct {
				SRT string `json:"srt_path"`
				TXT string `json:"text_path"`
			}
			_ = json.Unmarshal(got.Data, &out)
			srt, _ := os.ReadFile(out.SRT)
			if string(srt) != sttclient.SRT(sampleSegments) {
				t.Errorf("srt = %q, want all three segments", srt)
			}
			txt, _ := os.ReadFile(out.TXT)
			if !strings.Contains(string(txt), "Goodbye.") {
				t.Errorf("txt = %q, want the whole transcript", txt)
			}
			node.mu.Lock()
			defer node.mu.Unlock()
			if len(node.mediaReqs) != 1 || node.mediaReqs[0] != "up-1.segments.json" || node.mediaAuth[0] != "Bearer tok" {
				t.Errorf("media fetches = %v (auth %v), want one fetch of the basename with the bearer", node.mediaReqs, node.mediaAuth)
			}
		})
	}
}

// A truncated answer whose full list cannot be fetched is a deferred result naming the node, never a
// partial transcript presented as the whole.
func TestRemoteTruncatedAnswerWithAFailedFetchDefers(t *testing.T) {
	setBusy(t, false)
	res := core.Result{OK: true, Data: sttData(sampleSegments, true, nodeDir+"missing.segments.json"), Meta: core.Meta{Model: "whisper-stt"}}
	node := newFakeNode(t, "node-c", sttTasks, res)
	cfg := askerCfg(t, node, fakeFFmpeg(t, false))
	got := Run(context.Background(), cfg, &localRunner{}, sttReq(srcAudio(t), nil), "remote")
	if !got.Deferred || got.DeferClass != core.DeferClassInfrastructure || !strings.Contains(got.Reason, "node-c") || !strings.Contains(got.Reason, "segment") {
		t.Fatalf("result = %+v, want an infrastructure defer naming the node and the segment list", got)
	}
	if left, _ := filepath.Glob(filepath.Join(cfg.MediaDir, "*")); len(left) != 0 {
		t.Errorf("a failed answer left files behind: %v", left)
	}
	// A fetched list that disagrees with the result's own count is refused too.
	node2 := newFakeNode(t, "node-d", sttTasks, core.Result{OK: true, Data: sttData(sampleSegments, true, nodeDir+"short.segments.json"), Meta: core.Meta{Model: "whisper-stt"}})
	short, _ := json.Marshal(sampleSegments[:2])
	node2.media["short.segments.json"] = short
	got = Run(context.Background(), askerCfg(t, node2, fakeFFmpeg(t, false)), &localRunner{}, sttReq(srcAudio(t), nil), "remote")
	if !got.Deferred || !strings.Contains(got.Reason, "2") || !strings.Contains(got.Reason, "3") {
		t.Fatalf("result = %+v, want a defer that names the 2 vs 3 mismatch", got)
	}
}

// The node's answer is checked before it is believed: no segments, or timestamps that run backwards,
// is a defer, not a transcript.
func TestRemoteAnswerIsValidated(t *testing.T) {
	setBusy(t, false)
	backwards := []sttclient.Segment{{ID: 0, Start: 5, End: 6, Text: "b"}, {ID: 1, Start: 1, End: 2, Text: "a"}}
	inverted := []sttclient.Segment{{ID: 0, Start: 5, End: 4, Text: "x"}}
	for name, segs := range map[string][]sttclient.Segment{"none": nil, "backwards": backwards, "end before start": inverted} {
		t.Run(name, func(t *testing.T) {
			res := core.Result{OK: true, Data: sttData(segs, false, nodeDir+"up-1.segments.json"), Meta: core.Meta{Model: "whisper-stt"}}
			node := newFakeNode(t, "node-c", sttTasks, res)
			cfg := askerCfg(t, node, fakeFFmpeg(t, false))
			got := Run(context.Background(), cfg, &localRunner{}, sttReq(srcAudio(t), nil), "remote")
			if !got.Deferred || got.DeferClass != core.DeferClassInfrastructure || !strings.Contains(got.Reason, "node-c") {
				t.Fatalf("result = %+v, want an infrastructure defer naming the node", got)
			}
		})
	}
	// An unreadable job result is a defer too.
	node := newFakeNode(t, "node-c", sttTasks, core.Result{OK: true, Data: json.RawMessage(`"not an object"`)})
	got := Run(context.Background(), askerCfg(t, node, fakeFFmpeg(t, false)), &localRunner{}, sttReq(srcAudio(t), nil), "remote")
	if !got.Deferred || got.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("result = %+v, want a defer for an unreadable answer", got)
	}
}

// A node that deferred (its whisper was held) hands back the defer with its class and error class
// intact, stamped with where it ran: the caller branches on them as for a local defer.
func TestRemoteNodeDeferKeepsItsClasses(t *testing.T) {
	setBusy(t, false)
	d := core.Deferf("gpu busy: a render holds the node's card", "", core.Meta{ErrClass: "gpu_busy", Model: "whisper-stt"})
	d.DeferClass = core.DeferClassCapacity
	node := newFakeNode(t, "node-c", sttTasks, d)
	got := Run(context.Background(), askerCfg(t, node, fakeFFmpeg(t, false)), &localRunner{}, sttReq(srcAudio(t), nil), "remote")
	if !got.Deferred || got.DeferClass != core.DeferClassCapacity || got.Meta.ErrClass != "gpu_busy" || got.Meta.Node != "node-c" || got.Meta.Placement != "remote: forced" {
		t.Fatalf("result = %+v, want the node's defer verbatim, stamped", got)
	}
}

// When conversion is impossible the original file is sent, if it fits the node's cap; the extension
// is the original's, so the node's ffmpeg sees what it was given.
func TestRemoteFallsBackToTheOriginalFileWhenConversionFails(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-c", sttTasks, okResult(sampleSegments, false))
	cfg := askerCfg(t, node, fakeFFmpeg(t, true))
	got := Run(context.Background(), cfg, &localRunner{}, sttReq(srcAudio(t), nil), "remote")
	if !got.OK {
		t.Fatalf("result = %+v", got)
	}
	p, _ := node.dispatched()
	if string(node.uploaded(t)) != "not-really-m4a-audio-bytes" || p["audio_ext"] != "m4a" {
		t.Errorf("uploaded %q as %v, want the original file with its own extension", node.uploaded(t), p["audio_ext"])
	}
}

func TestRemoteOriginalOverTheNodesCapIsACapacityDefer(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-c", sttTasks, okResult(sampleSegments, false))
	node.capMB = 1
	cfg := askerCfg(t, node, fakeFFmpeg(t, true))
	src := filepath.Join(t.TempDir(), "long.wav")
	if err := os.WriteFile(src, make([]byte, 1<<20+1), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Run(context.Background(), cfg, &localRunner{}, sttReq(src, nil), "remote")
	if !got.Deferred || got.DeferClass != core.DeferClassCapacity || !strings.Contains(got.Reason, "node-c") {
		t.Fatalf("result = %+v, want a capacity defer naming the node's cap", got)
	}
	if p, _ := node.dispatched(); p != nil {
		t.Fatal("a file over the node's cap was sent anyway")
	}
}

// hq asks for a node with an hq model, and only that one is sent the job.
func TestRemoteHQPicksANodeWithAnHQModel(t *testing.T) {
	asked := setBusy(t, false)
	yes, no := true, false
	std := newFakeNode(t, "node-std", sttTasks, okResult(sampleSegments, false))
	std.hq = &no
	hqn := newFakeNode(t, "node-hq", sttTasks, okResult(sampleSegments, false))
	hqn.hq = &yes
	cfg := askerCfg(t, std, fakeFFmpeg(t, false))
	cfg.DelegateRemotes = []string{std.srv.URL, hqn.srv.URL}
	got := Run(context.Background(), cfg, &localRunner{}, sttReq(srcAudio(t), map[string]any{"hq": true}), "remote")
	if !got.OK || got.Meta.Node != "node-hq" {
		t.Fatalf("result = %+v, want the hq node", got)
	}
	if p, _ := hqn.dispatched(); p["hq"] != true {
		t.Errorf("hq = %v, want it forwarded", p["hq"])
	}
	if p, _ := std.dispatched(); p != nil {
		t.Error("the node without an hq model was sent an hq job")
	}
	// Only a node without hq: a capacity defer naming why.
	cfg.DelegateRemotes = []string{std.srv.URL}
	got = Run(context.Background(), cfg, &localRunner{}, sttReq(srcAudio(t), map[string]any{"hq": true}), "remote")
	if !got.Deferred || got.DeferClass != core.DeferClassCapacity || !strings.Contains(got.Reason, "hq") {
		t.Fatalf("result = %+v, want a capacity defer that names hq", got)
	}
	// The auto trigger asks about the model the call will use.
	asked = setBusy(t, true)
	Run(context.Background(), cfg, &localRunner{res: localOK}, sttReq(srcAudio(t), map[string]any{"hq": true}), "auto")
	if !*asked {
		t.Error("the auto probe was not told the call is hq")
	}
}

// The asker's own stt_language applies to a remote call that names none, as it does locally.
func TestRemoteForwardsTheAskersDefaultLanguage(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-c", sttTasks, okResult(sampleSegments, false))
	cfg := askerCfg(t, node, fakeFFmpeg(t, false))
	cfg.STTLanguage = "es"
	Run(context.Background(), cfg, &localRunner{}, sttReq(srcAudio(t), nil), "remote")
	if p, _ := node.dispatched(); p["language"] != "es" {
		t.Errorf("language = %v, want the asker's stt_language", p["language"])
	}
	cfg.STTLanguage = "auto"
	Run(context.Background(), cfg, &localRunner{}, sttReq(srcAudio(t), nil), "remote")
	if p, _ := node.dispatched(); p["language"] != "auto" {
		t.Errorf("language = %v, want auto sent as such (the node must detect instead of applying its own default)", p["language"])
	}
	cfg.STTLanguage = ""
	Run(context.Background(), cfg, &localRunner{}, sttReq(srcAudio(t), nil), "remote")
	if p, _ := node.dispatched(); p["language"] != nil {
		t.Errorf("language = %v, want none when the asker names none (the node's own default applies)", p["language"])
	}
}

// Placement defers: a node that lists only the legacy path-taking stt cannot be sent bytes; a leased
// card is skipped; no remotes at all is a config defer. The error names every miss.
func TestRemotePlacementDefers(t *testing.T) {
	setBusy(t, false)
	legacy := newFakeNode(t, "node-old", []string{"stt"}, okResult(sampleSegments, false))
	leased := newFakeNode(t, "node-leased", sttTasks, okResult(sampleSegments, false))
	leased.leased = true
	cfg := askerCfg(t, legacy, fakeFFmpeg(t, false))
	cfg.DelegateRemotes = []string{legacy.srv.URL, leased.srv.URL, "http://127.0.0.1:1"}
	got := Run(context.Background(), cfg, &localRunner{}, sttReq(srcAudio(t), nil), "remote")
	if !got.Deferred || got.DeferClass != core.DeferClassCapacity || got.Meta.Placement != "remote: forced" {
		t.Fatalf("result = %+v", got)
	}
	for _, want := range []string{"node-old", "upload", "node-leased", "leased", "127.0.0.1:1"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason %q does not name %q", got.Reason, want)
		}
	}
	if p, _ := legacy.dispatched(); p != nil {
		t.Error("a legacy-only node was sent an upload")
	}
	cfg.DelegateRemotes = nil
	got = Run(context.Background(), cfg, &localRunner{}, sttReq(srcAudio(t), nil), "remote")
	if !got.Deferred || got.DeferClass != core.DeferClassConfig {
		t.Fatalf("no remotes: %+v, want a config defer", got)
	}
}

// A dispatch the node refuses: 503/429 is capacity, anything else infrastructure.
func TestRemoteRefusedDispatchClasses(t *testing.T) {
	setBusy(t, false)
	for status, want := range map[int]string{503: core.DeferClassCapacity, 429: core.DeferClassCapacity, 401: core.DeferClassInfrastructure, 400: core.DeferClassInfrastructure} {
		node := newFakeNode(t, "node-c", sttTasks, okResult(sampleSegments, false))
		node.refuse = status
		got := Run(context.Background(), askerCfg(t, node, fakeFFmpeg(t, false)), &localRunner{}, sttReq(srcAudio(t), nil), "remote")
		if !got.Deferred || got.DeferClass != want {
			t.Errorf("status %d: %+v, want class %s", status, got, want)
		}
	}
}

// A missing source is a deferred result (the shape the local convert gives), never a panic or an
// upload.
func TestRemoteMissingAudioDefersWithoutDispatch(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-c", sttTasks, okResult(sampleSegments, false))
	got := Run(context.Background(), askerCfg(t, node, fakeFFmpeg(t, false)), &localRunner{}, sttReq(filepath.Join(t.TempDir(), "nope.wav"), nil), "remote")
	if !got.Deferred || !strings.Contains(got.Reason, "audio") {
		t.Fatalf("result = %+v", got)
	}
	if p, _ := node.dispatched(); p != nil {
		t.Fatal("dispatched without a file")
	}
}

func TestUnrecognizedRouteIsAContractDefer(t *testing.T) {
	got := Run(context.Background(), config.Default(), &localRunner{}, sttReq("a.wav", nil), "everywhere")
	if !got.Deferred || got.DeferClass != core.DeferClassContract || !strings.Contains(got.Reason, "everywhere") {
		t.Fatalf("result = %+v", got)
	}
	for in, want := range map[string]string{"": RouteLocal, " Auto ": RouteAuto, "REMOTE": RouteRemote} {
		if r, ok := NormalizeRoute(in); !ok || r != want {
			t.Errorf("NormalizeRoute(%q) = %q, %v, want %q", in, r, ok, want)
		}
	}
}

// auto: an idle local whisper runs the work and the fleet is never contacted.
func TestAutoRunsLocalWhenWhisperIsNotBlocked(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-c", sttTasks, okResult(sampleSegments, false))
	cfg := askerCfg(t, node, fakeFFmpeg(t, false))
	local := &localRunner{res: localOK}
	got := Run(context.Background(), cfg, local, sttReq(srcAudio(t), nil), "auto")
	if !got.OK || got.Meta.Placement != "local: gpu idle" || local.count() != 1 {
		t.Fatalf("result = %+v (local ran %d)", got, local.count())
	}
	if p, _ := node.dispatched(); p != nil {
		t.Fatal("auto contacted a node although local whisper was free")
	}
}

// auto with a blocked local whisper spills to a node; the transcript comes back with local files.
func TestAutoSpillsWhenWhisperWouldBlock(t *testing.T) {
	setBusy(t, true)
	node := newFakeNode(t, "node-c", sttTasks, okResult(sampleSegments, false))
	cfg := askerCfg(t, node, fakeFFmpeg(t, false))
	local := &localRunner{res: localOK}
	got := Run(context.Background(), cfg, local, sttReq(srcAudio(t), nil), "auto")
	if !got.OK || got.Meta.Placement != "remote: local gpu busy" || got.Meta.Node != "node-c" || local.count() != 0 {
		t.Fatalf("result = %+v (local ran %d)", got, local.count())
	}
}

// auto with a blocked local whisper and no usable node still runs local (queued-local beats
// ineligible-remote), with the reason in the placement and NO fleet hint (a node was tried).
func TestAutoFallsBackToLocalWhenNoNodeIsEligible(t *testing.T) {
	setBusy(t, true)
	legacy := newFakeNode(t, "node-old", []string{"stt"}, okResult(sampleSegments, false))
	cfg := askerCfg(t, legacy, fakeFFmpeg(t, false))
	busy := core.Deferf("gpu busy: held", "", core.Meta{ErrClass: "gpu_busy"})
	local := &localRunner{res: busy}
	got := Run(context.Background(), cfg, local, sttReq(srcAudio(t), nil), "auto")
	if local.count() != 1 || !strings.HasPrefix(got.Meta.Placement, "local: gpu busy, ") || !strings.Contains(got.Meta.Placement, "node-old") {
		t.Fatalf("result = %+v (local ran %d), want the local run with the placement reason", got, local.count())
	}
	if got.Reason != busy.Reason {
		t.Errorf("reason = %q, want it untouched: the fleet was already tried", got.Reason)
	}
}

// A call whose context is already cancelled ends in a defer, not a hang.
func TestRemoteWaitEndsWhenTheContextDoes(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-c", sttTasks, core.Result{OK: true})
	node.runningFirst = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := askerCfg(t, node, fakeFFmpeg(t, false))
	got := Run(ctx, cfg, &localRunner{}, sttReq(srcAudio(t), nil), "remote")
	if !got.Deferred {
		t.Fatalf("result = %+v, want a defer for a cancelled call", got)
	}
}

// A node that never finishes: the wait ends with the caller's deadline and says the job was still
// running; a node that denies holding the job (evicted, restarted) ends it at once.
func TestRemoteWaitEndsOnTheDeadlineAndOnAnEvictedJob(t *testing.T) {
	setBusy(t, false)
	slow := newFakeNode(t, "node-slow", sttTasks, okResult(sampleSegments, false))
	slow.stayRunning = true
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	got := Run(ctx, askerCfg(t, slow, fakeFFmpeg(t, false)), &localRunner{}, sttReq(srcAudio(t), nil), "remote")
	if !got.Deferred || got.DeferClass != core.DeferClassInfrastructure || !strings.Contains(got.Reason, "node-slow") || !strings.Contains(got.Reason, "still running") {
		t.Fatalf("result = %+v, want an infrastructure defer naming the node whose job was still running", got)
	}
	gone := newFakeNode(t, "node-gone", sttTasks, okResult(sampleSegments, false))
	gone.jobStatus = http.StatusNotFound
	start := time.Now()
	got = Run(context.Background(), askerCfg(t, gone, fakeFFmpeg(t, false)), &localRunner{}, sttReq(srcAudio(t), nil), "remote")
	if !got.Deferred || !strings.Contains(got.Reason, "denies holding") {
		t.Fatalf("result = %+v, want the evicted job named", got)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("an evicted job took %s to give up", time.Since(start))
	}
	flaky := newFakeNode(t, "node-flaky", sttTasks, okResult(sampleSegments, false))
	flaky.jobStatus = http.StatusInternalServerError
	got = Run(context.Background(), askerCfg(t, flaky, fakeFFmpeg(t, false)), &localRunner{}, sttReq(srcAudio(t), nil), "remote")
	if !got.Deferred || !strings.Contains(got.Reason, "consecutive poll failures") {
		t.Fatalf("result = %+v, want the poll-failure cap named", got)
	}
}

// The client dials only loopback and the tailnet (never-cloud, ADR 0001).
func TestRemoteRefusesAnOffTailnetNodeAtTheDialGate(t *testing.T) {
	setBusy(t, false)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{"http://192.0.2.10:9"}
	cfg.FFmpegPath = fakeFFmpeg(t, false)
	got := Run(context.Background(), cfg, &localRunner{}, sttReq(srcAudio(t), nil), "remote")
	if !got.Deferred || !strings.Contains(got.Reason, "tailnet guard") {
		t.Fatalf("result = %+v, want the dial gate's refusal in the reason", got)
	}
}

// The wait for a node is sized from the audio: a short clip does not get the whole 30 minutes, a long
// one is clamped to stt_request_timeout_sec, an unknown length gets the cap.
func TestBudgetScalesWithDuration(t *testing.T) {
	cfg := config.Default() // stt_request_timeout_sec 1800
	cases := []struct {
		durSec float64
		want   string
	}{{0, "30m0s"}, {10, "3m5s"}, {600, "8m0s"}, {7200, "30m0s"}}
	for _, c := range cases {
		if got := budgetFor(cfg, c.durSec).String(); got != c.want {
			t.Errorf("budgetFor(%vs) = %s, want %s", c.durSec, got, c.want)
		}
	}
	cfg.STTRequestTimeoutSec = 600
	if got := budgetFor(cfg, 7200).String(); got != "10m0s" {
		t.Errorf("a smaller stt_request_timeout_sec must clamp the budget, got %s", got)
	}
	cfg.STTRequestTimeoutSec = 0
	if got := budgetFor(cfg, 0).String(); got != "30m0s" {
		t.Errorf("an unset timeout reads as the 1800 s default, got %s", got)
	}
}

// Delivering the body is sized from it: the 20 s the small lanes get cannot carry 40 MB.
func TestDispatchTimeoutScalesWithTheBody(t *testing.T) {
	if got := dispatchTimeoutFor(0).Seconds(); got != 30 {
		t.Errorf("an empty body gets %vs, want 30", got)
	}
	if small, big := dispatchTimeoutFor(1<<20), dispatchTimeoutFor(40<<20); big <= small || big.Seconds() < 120 {
		t.Errorf("40 MiB gets %s and 1 MiB %s: the budget must grow with the body", big, small)
	}
}

// The segments file name the node reports is only ever used as a bare name.
func TestNodeFileBaseHandlesBothSeparators(t *testing.T) {
	for in, want := range map[string]string{
		"/srv/media/a.segments.json":     "a.segments.json",
		`C:\media\a.segments.json`:       "a.segments.json",
		`C:\media/mixed\a.segments.json`: "a.segments.json",
		"plain.json":                     "plain.json",
		"":                               "",
		"/trailing/":                     "",
	} {
		if got := nodeFileBase(in); got != want {
			t.Errorf("nodeFileBase(%q) = %q, want %q", in, got, want)
		}
	}
}

// Output stems are sanitized like the pipeline's, and distinct uploads of one name do not collide.
func TestOutputStemIsSanitizedAndContentAddressed(t *testing.T) {
	a := outputBase("/media", `C:\in\a:b*c?.m4a`, []byte("one"), "whisper-stt")
	b := outputBase("/media", `C:\in\a:b*c?.m4a`, []byte("two"), "whisper-stt")
	if a == b {
		t.Fatalf("two different recordings share the output stem %s", a)
	}
	if strings.ContainsAny(filepath.Base(a), `:*?"<>|`) || !strings.HasPrefix(filepath.Base(a), "a_b_c_-") {
		t.Errorf("stem %q is not sanitized", filepath.Base(a))
	}
	if again := outputBase("/media", `C:\in\a:b*c?.m4a`, []byte("one"), "whisper-stt"); again != a {
		t.Errorf("the same recording and model must keep one stem: %s vs %s", again, a)
	}
	if other := outputBase("/media", `C:\in\a:b*c?.m4a`, []byte("one"), "whisper-hq"); other == a {
		t.Error("the model is part of the identity")
	}
}

// The original file's extension reaches the node only when it is one its door accepts.
func TestCleanExt(t *testing.T) {
	for in, want := range map[string]string{
		"a.FLAC": "flac", "dir/a.m4a": "m4a", "noext": "audio", "a.": "audio",
		"a.waytoolongext": "audio", "a.w@v": "audio", `C:\x\a.mp3`: "mp3",
	} {
		if got := cleanExt(in); got != want {
			t.Errorf("cleanExt(%q) = %q, want %q", in, got, want)
		}
	}
}
