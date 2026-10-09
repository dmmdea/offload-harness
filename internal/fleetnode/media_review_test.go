package fleetnode

// Review round for the media-job work (register CT-50): each test here fails without the guard it names.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetqueue"
	"github.com/dmmdea/offload-harness/internal/mediacap"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

// stubMediaRoutes swaps the route derivation for the test and restores it after.
func stubMediaRoutes(t *testing.T, fn func(config.Config) []mediacap.Route) {
	t.Helper()
	prev := mediaRoutesFn
	mediaRoutesFn = fn
	ResetMediaRoutesCache()
	t.Cleanup(func() { mediaRoutesFn = prev; ResetMediaRoutesCache() })
}

// routesWith derives every media route NOT CONFIGURED except those named, which are in the given state.
func routesWith(states map[string]mediacap.State) func(config.Config) []mediacap.Route {
	return func(config.Config) []mediacap.Route {
		var out []mediacap.Route
		for _, n := range []string{"generate_video", "animate_character", "run_graph", "generate_audio:voice", "generate_audio:voice:endpoint", "generate_audio:music"} {
			st, ok := states[n]
			if !ok {
				st = mediacap.NotConfigured
			}
			out = append(out, mediacap.Route{Name: n, Engine: "test", State: st})
		}
		return out
	}
}

// ---- F1, F2: the pull path ------------------------------------------------------------------------

// The claim names the tasks this node can run NOW, by the predicate health and admission use: a weight
// that goes missing stops the claim for that task, and one that returns starts it, with no restart.
func TestClaimAdvertisesTheMediaTasksTheNodeCanRunRightNow(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	stubMediaRoutes(t, func(config.Config) []mediacap.Route {
		st := mediacap.BoundButMissing
		if up.Load() {
			st = mediacap.Configured
		}
		return []mediacap.Route{{Name: "generate_video", State: st}}
	})
	cfg := mediaJobCfg(t)
	s, _ := newTestServer(t, cfg, &fakeRunner{}, &Options{NodeID: "testnode", Snapshot: goodSnapshot, LoopbackListener: true})

	var mu sync.Mutex
	var claims [][]string
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			Tasks []string `json:"task_types"`
		}
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		claims = append(claims, m.Tasks)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer holder.Close()
	client := &http.Client{Timeout: 5 * time.Second, Transport: netguard.SafeTransport(nil)}

	s.claimOne(context.Background(), client, holder.URL, "testnode", cfg)
	up.Store(false)
	ResetMediaRoutesCache()
	s.claimOne(context.Background(), client, holder.URL, "testnode", cfg)
	up.Store(true)
	ResetMediaRoutesCache()
	s.claimOne(context.Background(), client, holder.URL, "testnode", cfg)

	mu.Lock()
	got := append([][]string(nil), claims...)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("holder saw %d claims, want 3", len(got))
	}
	if !slices.Contains(got[0], "video-gen") || slices.Contains(got[1], "video-gen") || !slices.Contains(got[2], "video-gen") {
		t.Fatalf("claims must follow the live route verdict (up, down, up): %v", got)
	}
}

// A media job pulled from the holder is acked with the same artifacts a pushed one carries, so the holder
// stores the verified result too.
func TestPulledMediaJobIsAckedWithArtifacts(t *testing.T) {
	cfg := mediaJobCfg(t)
	out := filepath.Join(cfg.MediaDir, "render-1.png")
	runner := &fakeRunner{fn: func(context.Context, core.Request) core.Result {
		if err := os.WriteFile(out, []byte("PIXELS"), 0o644); err != nil {
			t.Error(err)
		}
		data, _ := json.Marshal(map[string]any{"image_path": out})
		return core.Result{OK: true, Data: data}
	}}
	s, _ := newTestServer(t, cfg, runner, &Options{NodeID: "testnode", Snapshot: goodSnapshot, LoopbackListener: true})

	var mu sync.Mutex
	var acks []map[string]any
	served := false
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fleet/queue/claim":
			mu.Lock()
			first := !served
			served = true
			mu.Unlock()
			if first {
				_ = json.NewEncoder(w).Encode(fleetqueue.Job{ID: "pulled-media", TaskType: "image-gen", Payload: json.RawMessage(`{"prompt":"p"}`)})
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "/fleet/queue/ack":
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			mu.Lock()
			acks = append(acks, m)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer holder.Close()
	client := &http.Client{Timeout: 5 * time.Second, Transport: netguard.SafeTransport(nil)}
	if id, ok := s.claimOne(context.Background(), client, holder.URL, "testnode", cfg); !ok || id != "pulled-media" {
		t.Fatalf("claimOne = %q, %v", id, ok)
	}
	var ack map[string]any
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		mu.Lock()
		if len(acks) > 0 {
			ack = acks[0]
		}
		mu.Unlock()
		if ack != nil {
			break
		}
	}
	if ack == nil {
		t.Fatal("the holder was never acked")
	}
	result, _ := ack["result"].(map[string]any)
	arts, _ := result["artifacts"].([]any)
	if len(arts) != 1 {
		t.Fatalf("the ack must carry the artifacts of the output inside media_dir: %v", ack)
	}
	sum := sha256.Sum256([]byte("PIXELS"))
	a, _ := arts[0].(map[string]any)
	if a["name"] != "render-1.png" || a["sha256"] != hex.EncodeToString(sum[:]) || a["bytes"] != float64(6) {
		t.Fatalf("artifact %v", a)
	}
}

// ---- F3: the request body is not pinned by a running job ------------------------------------------

// While a media job is in flight (here: its runner is blocked) nothing the node keeps references the
// request body: the admission closure captures the job id and the task type, and BuildRequest's extracted
// directory is the only copy of the bundle. A closure over the whole envelope pins the base64 bundle for
// the job's entire life, and this fails by about the body's size.
func TestAnInFlightMediaJobDoesNotPinItsRequestBody(t *testing.T) {
	cfg := mediaJobCfg(t)
	cfg.FleetMediaInputsMaxMB = 64
	started, release := make(chan struct{}), make(chan struct{})
	runner := &fakeRunner{fn: func(context.Context, core.Request) core.Result {
		close(started)
		<-release
		return core.Result{OK: true, Data: json.RawMessage(`{"video_path":"clip.mp4"}`)}
	}}
	s, jobs := newTestServer(t, cfg, runner, nil)

	heap := func() uint64 {
		var ms runtime.MemStats
		for i := 0; i < 3; i++ {
			runtime.GC()
		}
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	before := heap()
	const payloadBytes = 24 << 20
	func() {
		big := append(append([]byte{}, mjPNG...), []byte(incompressible(payloadBytes))...)
		body := string(mediaPayload("video-gen", `{"prompt":"p"}`, packFiles(t, map[string][]byte{"still.png": big}), map[string]string{"still": "still.png"},
			func(p *MediaJobPayload) { p.JobID = "mj-pin" }))
		if len(body) < payloadBytes {
			t.Fatalf("test body is only %d bytes", len(body))
		}
		if rec := do(t, s, "POST", MediaJobPath, body, bearer()); rec.Code != 202 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the job never started")
	}
	after := heap()
	close(release)
	waitDone(t, jobs, "mj-pin")
	if after > before && after-before > 10<<20 {
		t.Fatalf("a running media job pins %d MiB of heap after its handler returned: the closure keeps the request body", (after-before)>>20)
	}
}

// ---- F4: file fields in any casing ----------------------------------------------------------------

func TestMediaJobFileFieldsAreRefusedAsNodePathsInAnyCasing(t *testing.T) {
	cfg := mediaJobCfg(t)
	refused := map[string][]byte{
		"Still":               mediaPayload("video-gen", `{"prompt":"p","Still":"/etc/passwd"}`, nil, nil, nil),
		"STILL":               mediaPayload("video-gen", `{"prompt":"p","STILL":"/etc/passwd"}`, nil, nil, nil),
		"sTiLl":               mediaPayload("video-gen", `{"prompt":"p","sTiLl":"C:/Windows/win.ini"}`, nil, nil, nil),
		"DRIVER with ref":     mediaPayload("animate", `{"prompt":"p","DRIVER":"/etc/passwd"}`, packFiles(t, map[string][]byte{"ref.png": mjPNG}), map[string]string{"ref": "ref.png"}, nil),
		"Ref with driver":     mediaPayload("animate", `{"prompt":"p","Ref":"/etc/passwd"}`, packFiles(t, map[string][]byte{"driver.mp4": mjMP4}), map[string]string{"driver": "driver.mp4"}, nil),
		"Clone":               mediaPayload("audio-gen", `{"text":"hola","Clone":"/etc/passwd"}`, nil, nil, nil),
		"CLONE beside a file": mediaPayload("audio-gen", `{"text":"hola","CLONE":"/etc/passwd"}`, packFiles(t, map[string][]byte{"c.wav": mjWAV}), nil, nil),
	}
	for name, payload := range refused {
		req, cleanup, err := BuildRequest(context.Background(), cfg, true, MediaJobTask, payload)
		if err == nil {
			cleanup()
			t.Errorf("%s: a node path in a differently-cased file field was admitted: %+v", name, req.Params)
			continue
		}
		if !strings.Contains(err.Error(), "names a path on this node") && !strings.Contains(err.Error(), "a bundle needs inputs") {
			t.Errorf("%s: refused for the wrong reason: %v", name, err)
		}
	}
	// A file that IS shipped replaces any spelling of its field: no other casing survives to the builder.
	req, cleanup, err := BuildRequest(context.Background(), cfg, true, MediaJobTask, mediaPayload("video-gen", `{"prompt":"p","STILL":"/etc/passwd","Still":"/etc/hosts"}`,
		packFiles(t, map[string][]byte{"still.png": mjPNG}), map[string]string{"still": "still.png"}, nil))
	if err != nil {
		t.Fatalf("a shipped still with stray casings must be accepted (the shipped file wins): %v", err)
	}
	defer cleanup()
	still, _ := req.Params["still"].(string)
	if !strings.Contains(filepath.ToSlash(still), "/"+mediaInputsDir+"/in-") {
		t.Fatalf("the builder read %q, want the extracted file", still)
	}
	if left := leftoverInputDirs(t, cfg); len(left) != 1 {
		t.Fatalf("only the accepted job may hold a directory: %v", left)
	}
}

// ---- F17: the inner task allowlist ----------------------------------------------------------------

// A task this node serves but the door does not carry is refused BY THE ALLOWLIST (stt is configured here,
// so the configured check alone would let it through; so would the nested media-job).
func TestMediaJobCarriesOnlyItsFiveTasks(t *testing.T) {
	cfg := mediaJobCfg(t)
	cfg.STTModel = "whisper-test"
	if !taskConfiguredFor(cfg, "stt", true) || !taskConfiguredFor(cfg, MediaJobTask, true) {
		t.Fatal("test premise: stt and media-job are both served by this node")
	}
	for _, task := range []string{"stt", MediaJobTask, "compose-video", "agent", "accel", "vision", "text", "bogus"} {
		_, cleanup, err := BuildRequest(context.Background(), cfg, true, MediaJobTask, mediaPayload(task, `{"audio":"a.wav"}`, nil, nil, nil))
		if err == nil {
			cleanup()
			t.Errorf("task %q was carried by the media-job door", task)
			continue
		}
		if !strings.Contains(err.Error(), "is not one of image-gen, video-gen, animate, audio-gen, run-graph") {
			t.Errorf("task %q: refused for the wrong reason (want the allowlist): %v", task, err)
		}
	}
	for _, task := range mediaJobTasks {
		inner := map[string]string{"image-gen": `{"prompt":"p"}`, "video-gen": `{"prompt":"p"}`, "audio-gen": `{"text":"t"}`,
			"run-graph": `{"graph":{"1":{"class_type":"X"}}}`, "animate": `{"prompt":"p"}`}[task]
		var payload []byte
		if task == "animate" {
			payload = mediaPayload(task, inner, packFiles(t, map[string][]byte{"ref.png": mjPNG, "driver.mp4": mjMP4}), map[string]string{"ref": "ref.png", "driver": "driver.mp4"}, nil)
		} else {
			payload = mediaPayload(task, inner, nil, nil, nil)
		}
		if _, cleanup, err := BuildRequest(context.Background(), cfg, true, MediaJobTask, payload); err != nil {
			t.Errorf("carried task %q refused: %v", task, err)
		} else {
			cleanup()
		}
	}
}

// ---- F11: a bound task whose route is not ready is a 503 that names the route ---------------------

func TestABoundTaskWithANotReadyRouteIs503NamingTheRouteAnUnboundOneStays400(t *testing.T) {
	stubMediaRoutes(t, routesWith(map[string]mediacap.State{"generate_video": mediacap.BoundButMissing, "animate_character": mediacap.Configured}))
	cfg := mediaJobCfg(t)
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)

	rec := do(t, s, "POST", "/fleet/dispatch", `{"job_id":"v-1","task_type":"video-gen","payload":{"prompt":"p"}}`, nil)
	if rec.Code != 503 {
		t.Fatalf("bound-but-missing video-gen: status %d, want 503 (re-placeable): %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"video-gen", "generate_video", "BOUND-BUT-MISSING", "not ready"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the refusal must name %q: %s", want, rec.Body.String())
		}
	}
	if strings.Contains(rec.Body.String(), "unsupported task_type") {
		t.Errorf("the generic unsupported-task text is the 400's: %s", rec.Body.String())
	}
	var nre *routeNotReadyError
	if _, cleanup, err := BuildRequest(context.Background(), cfg, true, "video-gen", json.RawMessage(`{"prompt":"p"}`)); err == nil {
		cleanup()
		t.Fatal("admitted")
	} else if !errors.As(err, &nre) {
		t.Fatalf("want a *routeNotReadyError, got %T: %v", err, err)
	}

	// Not bound at all keeps the plain 400.
	unbound := mediaJobCfg(t)
	unbound.VideoGenScript = ""
	s2, _ := newTestServer(t, unbound, &fakeRunner{}, nil)
	rec = do(t, s2, "POST", "/fleet/dispatch", `{"job_id":"v-2","task_type":"video-gen","payload":{"prompt":"p"}}`, nil)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "unsupported task_type") {
		t.Fatalf("an unbound task: status %d %s, want the 400 unsupported task_type", rec.Code, rec.Body.String())
	}
	// A ready route is unaffected, and a bad payload on it is still a 400.
	if rec := do(t, s, "POST", "/fleet/dispatch", `{"job_id":"a-1","task_type":"animate","payload":{"prompt":"p"}}`, nil); rec.Code != 400 {
		t.Fatalf("animate is ready here; a payload with no ref is a 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// The media-job door: an inner task whose route is not ready is the same 503; one that is ready is not.
	rec = do(t, s, "POST", MediaJobPath, string(mediaPayload("video-gen", `{"prompt":"p"}`, packFiles(t, map[string][]byte{"s.png": mjPNG}),
		map[string]string{"still": "s.png"}, func(p *MediaJobPayload) { p.JobID = "mj-nr" })), bearer())
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "generate_video") || !strings.Contains(rec.Body.String(), "BOUND-BUT-MISSING") {
		t.Fatalf("media-job of a not-ready inner task: status %d %s, want 503 naming the route", rec.Code, rec.Body.String())
	}
	if left := leftoverInputDirs(t, cfg); len(left) != 0 {
		t.Fatalf("a refused media job left directories: %v", left)
	}

	// When every task the door carries is bound-but-not-ready, the door itself answers 503, not the
	// unsupported-task 400 a re-placement would never survive.
	stubMediaRoutes(t, routesWith(map[string]mediacap.State{"generate_video": mediacap.BoundButMissing, "animate_character": mediacap.BoundButMissing,
		"run_graph": mediacap.BoundButMissing, "generate_audio:voice": mediacap.BoundButMissing}))
	onlyMedia := mediaJobCfg(t)
	onlyMedia.ImageGenScript = ""
	s3, _ := newTestServer(t, onlyMedia, &fakeRunner{}, nil)
	rec = do(t, s3, "POST", MediaJobPath, string(mediaPayload("run-graph", `{"graph":{"1":{"class_type":"X"}}}`, nil, nil, nil)), bearer())
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "run_graph") {
		t.Fatalf("every inner task not ready: status %d %s, want 503", rec.Code, rec.Body.String())
	}
}

// ---- F12: deadline-extension failures are reported, once per route --------------------------------

func TestMediaJobReportsADeadlineItCannotExtendOncePerRoute(t *testing.T) {
	writeDeadlineUnsupported.Delete("the media-job door")
	writeDeadlineUnsupported.Delete("the media-job door (read)")
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	s, _ := newTestServer(t, mediaJobCfg(t), &fakeRunner{}, nil)
	for i := 0; i < 3; i++ {
		// httptest's recorder carries no deadlines: the writer cannot extend them.
		if rec := do(t, s, "POST", MediaJobPath, `{}`, bearer()); rec.Code != 400 {
			t.Fatalf("status %d", rec.Code)
		}
	}
	logged := buf.String()
	for _, want := range []string{"media-job door runs under the blanket read timeout", "media-job door runs under the blanket write timeout"} {
		if n := strings.Count(logged, want); n != 1 {
			t.Errorf("%q logged %d times over three requests, want exactly once per route:\n%s", want, n, logged)
		}
	}

	// A real failure to extend is an event about the request, logged each time.
	buf.Reset()
	s.setReadDeadline = func(http.ResponseWriter, time.Time) error { return errors.New("boom-read") }
	s.setWriteDeadline = func(http.ResponseWriter, time.Time) error { return errors.New("boom-write") }
	do(t, s, "POST", MediaJobPath, `{}`, bearer())
	if !strings.Contains(buf.String(), "boom-read") || !strings.Contains(buf.String(), "boom-write") {
		t.Errorf("a failed extension must be logged with its cause:\n%s", buf.String())
	}
}

// ---- F14: the verdicts are read once per request --------------------------------------------------

// A config that cannot be keyed is never cached, so every read of the derivation is visible: one health
// request and one admission each read it exactly once, however many tasks they judge.
func TestMediaVerdictsAreReadOncePerHealthRequestAndPerAdmission(t *testing.T) {
	var calls atomic.Int32
	stubMediaRoutes(t, func(cfg config.Config) []mediacap.Route {
		calls.Add(1)
		return bindingOnlyRoutes(cfg)
	})
	cfg := mediaJobCfg(t)
	cfg.ImageGenCFG = math.Inf(1) // json cannot encode it: no cache key
	if _, ok := mediacap.KeyOf(cfg); ok {
		t.Fatal("test premise: this config must not be keyable")
	}
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	calls.Store(0)
	if rec := do(t, s, "GET", "/fleet/health", "", nil); rec.Code != 200 {
		t.Fatalf("health status %d", rec.Code)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("one health request read the derivation %d times, want 1", n)
	}
	calls.Store(0)
	rec := do(t, s, "POST", MediaJobPath, string(mediaPayload("video-gen", `{"prompt":"p"}`, packFiles(t, map[string][]byte{"s.png": mjPNG}),
		map[string]string{"still": "s.png"}, func(p *MediaJobPayload) { p.JobID = "mj-once" })), bearer())
	if rec.Code != 202 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("one media-job admission read the derivation %d times, want 1", n)
	}
}

// The node encodes its config once, at construction: its cache key is computed there and health and
// admission look verdicts up with it.
func TestTheNodeComputesItsMediaCacheKeyOnce(t *testing.T) {
	cfg := mediaJobCfg(t)
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	want, ok := mediacap.KeyOf(cfg)
	if !ok || !s.mediaKeySet || !s.mediaKeyOK || s.mediaKey != want {
		t.Fatalf("key set=%v ok=%v equal=%v", s.mediaKeySet, s.mediaKeyOK, s.mediaKey == want)
	}
	v := s.mediaView()
	if !v.haveKey || v.key != want {
		t.Fatal("a request's view must start from the node's key, not encode the config again")
	}
}

// ---- F23: each media task is gated on ITS route ---------------------------------------------------

func TestEachMediaTaskIsAdvertisedOnlyForItsOwnRoute(t *testing.T) {
	cfg := mediaJobCfg(t)
	cfg.MusicGenScript, cfg.TTSEndpoint = "render/comfy-music.mjs", "http://192.0.2.9:8000"
	for _, tc := range []struct {
		route string
		task  string
	}{
		{"generate_video", "video-gen"}, {"animate_character", "animate"}, {"run_graph", "run-graph"},
		{"generate_audio:voice", "audio-gen"}, {"generate_audio:voice:endpoint", "audio-gen"}, {"generate_audio:music", "audio-gen"},
	} {
		stubMediaRoutes(t, routesWith(map[string]mediacap.State{tc.route: mediacap.Configured}))
		got := SupportedTasksFor(cfg, true)
		for _, task := range []string{"video-gen", "animate", "run-graph", "audio-gen"} {
			if want := task == tc.task; slices.Contains(got, task) != want {
				t.Errorf("only %s CONFIGURED: %s advertised=%v, want %v (tasks %v)", tc.route, task, !want, want, got)
			}
		}
	}
}

// ---- F24: the orphan sweep's age rule -------------------------------------------------------------

func TestSweepKeepsADirectoryYoungerThanTheLongestTimeoutPlusAnHour(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name         string
		cfg          config.Config
		age          time.Duration
		wantSwept    bool
		wantLongest  time.Duration
		wantBoundary time.Duration
	}{
		// Unequal timeouts: the video wall (2 h) sets the bound, 3 h.
		{"unequal, inside the longest", config.Config{ImageGenTimeoutSec: 60, VideoGenTimeoutSec: 7200, AnimateGenTimeoutSec: 60, AudioGenTimeoutSec: 60}, 150 * time.Minute, false, 2 * time.Hour, 3 * time.Hour},
		{"unequal, past the longest", config.Config{ImageGenTimeoutSec: 60, VideoGenTimeoutSec: 7200, AnimateGenTimeoutSec: 60, AudioGenTimeoutSec: 60}, 190 * time.Minute, true, 2 * time.Hour, 3 * time.Hour},
		// The longest need not be the video one.
		{"audio longest, inside", config.Config{ImageGenTimeoutSec: 60, VideoGenTimeoutSec: 60, AnimateGenTimeoutSec: 60, AudioGenTimeoutSec: 3600}, 100 * time.Minute, false, time.Hour, 2 * time.Hour},
		{"audio longest, past", config.Config{ImageGenTimeoutSec: 60, VideoGenTimeoutSec: 60, AnimateGenTimeoutSec: 60, AudioGenTimeoutSec: 3600}, 130 * time.Minute, true, time.Hour, 2 * time.Hour},
		// Unset keys fall back to the runners' defaults; animate's 30 min is the longest, so 1 h 30 min.
		{"defaults, inside", config.Config{}, 80 * time.Minute, false, 30 * time.Minute, 90 * time.Minute},
		{"defaults, past", config.Config{}, 100 * time.Minute, true, 30 * time.Minute, 90 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.MediaDir = t.TempDir()
			if got := longestMediaTimeout(cfg); got != tc.wantLongest {
				t.Fatalf("longestMediaTimeout = %v, want %v", got, tc.wantLongest)
			}
			dir := filepath.Join(cfg.MediaDir, mediaInputsDir, "in-x")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			when := now.Add(-tc.age)
			if err := os.Chtimes(dir, when, when); err != nil {
				t.Fatal(err)
			}
			n, err := SweepOrphanedInputDirs(cfg, now)
			if err != nil {
				t.Fatal(err)
			}
			if (n == 1) != tc.wantSwept {
				t.Fatalf("a directory %v old (bound %v): swept=%d, want swept=%v", tc.age, tc.wantBoundary, n, tc.wantSwept)
			}
		})
	}
}

// ---- the lean decode of the body ------------------------------------------------------------------

// The door reads the bundle straight out of the body: an escaped slash in the string is the one case that
// takes a copy, and both give the same bundle; the head decode keeps the strict decoder's refusals.
func TestMediaJobBodyDecodeAcceptsEscapedBase64AndRefusesUnknownKeys(t *testing.T) {
	bundle := packFiles(t, map[string][]byte{"still.png": mjPNG})
	enc := base64.StdEncoding.EncodeToString(bundle)
	if !strings.Contains(enc, "/") {
		enc = base64.StdEncoding.EncodeToString(append(bundle, bytes.Repeat([]byte{0xff, 0xff, 0xfe}, 40)...)) // force a '/' into the text
	}
	for name, lit := range map[string]string{"plain": enc, "escaped slash": strings.ReplaceAll(enc, "/", `\/`)} {
		var w mediaJobWire
		if err := json.Unmarshal([]byte(`{"bundle":"`+lit+`"}`), &w); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := base64.StdEncoding.EncodeToString(w.Bundle.raw); got != enc {
			t.Errorf("%s: decoded bundle differs", name)
		}
	}
	for name, body := range map[string]string{
		"unknown key":       `{"job_id":"x","bogus":1}`,
		"unknown, cased":    `{"job_id":"x","Bogus":1}`,
		"bundle not string": `{"job_id":"x","bundle":5}`,
		"not an object":     `[1]`,
	} {
		if _, err := decodeMediaJobHead([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if h, err := decodeMediaJobHead([]byte(`{"JOB_ID":"x","bundle":null,"payload":{"a":1}}`)); err != nil || h.JobID != "x" {
		t.Errorf("a field matches without regard to case, as the strict decoder matched it: %+v %v", h, err)
	}
	// And the body reader hands back every byte whether or not Content-Length was sent.
	for _, cl := range []int64{-1, 11} {
		r := httptest.NewRequest("POST", "/", strings.NewReader("hello world"))
		r.ContentLength = cl
		got, err := readMediaJobBody(r, 1<<20)
		if err != nil || string(got) != "hello world" {
			t.Errorf("Content-Length %d: %q %v", cl, got, err)
		}
	}
	var maxErr *http.MaxBytesError
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", 100)))
	r.Body = http.MaxBytesReader(w, r.Body, 10)
	if _, err := readMediaJobBody(r, 10); !errors.As(err, &maxErr) {
		t.Errorf("an over-limit body must surface the typed MaxBytesError, got %v", err)
	}
}

// ---- R1: the body lands in one buffer of exactly Content-Length bytes -----------------------------

// bytes.Buffer.ReadFrom wants 512 free bytes before each read, so a buffer sized CL+1 regrew (to about
// twice its size) when the last reads left less than that. Sizes around every 512 boundary must come back
// in a buffer with no slack and no regrowth, and a body that outruns its declared length is refused.
func TestReadMediaJobBodyAllocatesExactlyContentLength(t *testing.T) {
	for _, cl := range []int{1, 510, 511, 512, 513, 1023, 1024, 1025, 4095, 4096, 4097, 70000} {
		src := bytes.Repeat([]byte{'x'}, cl)
		r := httptest.NewRequest("POST", "/", smallReads{bytes.NewReader(src)})
		r.ContentLength = int64(cl)
		got, err := readMediaJobBody(r, 1<<20)
		if err != nil || !bytes.Equal(got, src) {
			t.Fatalf("Content-Length %d: err %v, %d bytes back", cl, err, len(got))
		}
		if cap(got) != cl {
			t.Errorf("Content-Length %d: buffer capacity %d, want exactly %d (a regrowth)", cl, cap(got), cl)
		}
	}
	// Longer than it declared: refused as too large, the door's 413.
	r := httptest.NewRequest("POST", "/", strings.NewReader("0123456789"))
	r.ContentLength = 4
	var maxErr *http.MaxBytesError
	if _, err := readMediaJobBody(r, 1<<20); !errors.As(err, &maxErr) {
		t.Errorf("a body longer than its Content-Length must be refused as too large, got %v", err)
	}
	// Shorter than it declared: an error, not a short buffer.
	r = httptest.NewRequest("POST", "/", strings.NewReader("0123"))
	r.ContentLength = 10
	if _, err := readMediaJobBody(r, 1<<20); err == nil {
		t.Error("a body shorter than its Content-Length was accepted")
	}
}

// smallReads returns at most 509 bytes per Read, the pattern that leaves fewer than 512 free bytes at the
// end of a buffer.
type smallReads struct{ r *bytes.Reader }

func (o smallReads) Read(p []byte) (int, error) {
	if len(p) > 509 {
		p = p[:509]
	}
	return o.r.Read(p)
}

// ---- R9: the compose-project door reports a deadline it cannot extend ------------------------------

func TestComposeProjectReportsADeadlineItCannotExtend(t *testing.T) {
	writeDeadlineUnsupported.Delete("the compose-project door")
	writeDeadlineUnsupported.Delete("the compose-project door (read)")
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	s, _ := newTestServer(t, projectCfg(), &fakeRunner{}, nil)
	for i := 0; i < 3; i++ {
		if rec := do(t, s, "POST", ComposeProjectPath, "{}", bearer()); rec.Code != 400 {
			t.Fatalf("status %d", rec.Code)
		}
	}
	for _, want := range []string{"compose-project door runs under the blanket read timeout", "compose-project door runs under the blanket write timeout"} {
		if n := strings.Count(buf.String(), want); n != 1 {
			t.Errorf("%q logged %d times over three requests, want exactly once:\n%s", want, n, buf.String())
		}
	}
	buf.Reset()
	s.setReadDeadline = func(http.ResponseWriter, time.Time) error { return errors.New("boom-read") }
	s.setWriteDeadline = func(http.ResponseWriter, time.Time) error { return errors.New("boom-write") }
	do(t, s, "POST", ComposeProjectPath, "{}", bearer())
	if !strings.Contains(buf.String(), "boom-read") || !strings.Contains(buf.String(), "boom-write") {
		t.Errorf("a failed extension must be logged with its cause:\n%s", buf.String())
	}
}
