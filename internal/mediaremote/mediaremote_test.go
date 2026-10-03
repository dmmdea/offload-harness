package mediaremote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

func init() { pollEvery = 10 * time.Millisecond }

var (
	png = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 24)...)
	mp4 = append([]byte("\x00\x00\x00\x18ftypmp42"), make([]byte, 24)...)
	wav = append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, 24)...)
)

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// seen is one request the fake node received.
type seen struct {
	method, path string
	auth         string
	body         []byte
}

// node is a REAL fleet node server (doors, auth, job store, media serving) behind httptest, with a runner
// standing in for its pipeline and a request log in front of it.
type node struct {
	srv    *httptest.Server
	media  string
	mu     sync.Mutex
	log    []seen
	runner *nodeRunner
	// tamper, when set, rewrites the body GET /fleet/media serves.
	tamper func(name string, b []byte) []byte
}

// nodeRunner writes the outputs a render of each task would and reports them the way the pipeline does.
type nodeRunner struct {
	mu      sync.Mutex
	media   string
	reqs    []core.Request
	inputs  map[string][]byte // field -> bytes the pipeline read from the extracted file
	deferAs string
}

func (n *nodeRunner) Run(_ context.Context, req core.Request) core.Result {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.reqs = append(n.reqs, req)
	n.inputs = map[string][]byte{}
	for _, k := range []string{"still", "ref", "driver", "clone"} {
		if p, ok := req.Params[k].(string); ok {
			b, _ := os.ReadFile(p)
			n.inputs[k] = b
		}
	}
	if n.deferAs != "" {
		return core.Deferf(n.deferAs, "", core.Meta{})
	}
	write := func(name, body string) string {
		p := filepath.Join(n.media, name)
		_ = os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	var data any
	switch req.Task {
	case core.TaskGenerateImage:
		data = map[string]any{"image_path": write("render-1.png", "IMG"), "width": 64, "seed": 9007199254740993}
	case core.TaskGenerateVideo:
		data = map[string]any{"video_path": write("video-1.mp4", "VIDEO"), "seed": 5}
	case core.TaskAnimateCharacter:
		data = map[string]any{"video_path": write("animate-1.mp4", "ANIM")}
	case core.TaskGenerateAudio:
		data = map[string]any{"audio_path": write("audio-1.wav", "AUDIO"), "kind": "voice"}
	case core.TaskRunGraph:
		a, b := write("graph-a.png", "GA"), write("graph-b.mp4", "GB")
		data = map[string]any{
			"image_path": a,
			"outputs": map[string]any{
				"9":  []map[string]any{{"path": a, "type": "output", "kind": "image"}},
				"12": []map[string]any{{"path": b, "type": "output", "kind": "video"}},
			},
		}
	}
	raw, _ := json.Marshal(data)
	return core.Result{OK: true, Data: raw}
}

func (n *nodeRunner) last() core.Request {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.reqs[len(n.reqs)-1]
}

type nodeOpts struct {
	mediaInputs bool
	cfg         func(*config.Config)
	routes      func(config.Config) []mediacap.Route
}

// bindOnly is the derivation a fixture node runs on: a route is CONFIGURED when its script is bound.
func bindOnly(cfg config.Config) []mediacap.Route {
	row := func(name string, bound bool) mediacap.Route {
		st := mediacap.NotConfigured
		if bound {
			st = mediacap.Configured
		}
		return mediacap.Route{Name: name, Engine: "test", State: st}
	}
	return []mediacap.Route{
		row("generate_video", cfg.VideoGenScript != ""), row("animate_character", cfg.AnimateGenScript != ""),
		row("generate_audio:voice", cfg.VoiceGenScript != ""), row("generate_audio:music", cfg.MusicGenScript != ""),
		row("run_graph", cfg.RunGraphScript != ""), row("generate_audio:voice:endpoint", cfg.TTSEndpoint != ""),
	}
}

func startNode(t *testing.T, o nodeOpts) *node {
	t.Helper()
	routes := o.routes
	if routes == nil {
		routes = bindOnly
	}
	t.Cleanup(fleetnode.SetMediaRoutesSourceForTest(routes))
	media := t.TempDir()
	cfg := config.Config{
		MediaDir: media, FleetAuthToken: "tok", FleetMediaInputs: o.mediaInputs,
		ImageGenScript: "render/comfy-generate.mjs", VideoGenScript: "render/comfy-video.mjs",
		AnimateGenScript: "render/comfy-animate.mjs", VoiceGenScript: "render/tts.mjs",
		MusicGenScript: "render/comfy-music.mjs", RunGraphScript: "render/comfy-run-graph.mjs",
	}
	if o.cfg != nil {
		o.cfg(&cfg)
	}
	r := &nodeRunner{media: media}
	jobs := fleetnode.NewJobs(time.Hour, cfg.FleetConcurrencyLimit())
	t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })
	s := fleetnode.New(r, jobs, fleetnode.Options{
		NodeID: "render-node", Cfg: cfg, GpuVendor: "nvidia", GpuArch: "ampere",
		Snapshot: func() (fleetnode.Snapshot, bool) {
			return fleetnode.Snapshot{TotalGiB: 16, FreeGiB: 12, At: time.Now()}, true
		},
		Footprints: func() []fleetnode.FootprintEntry { return nil },
	})
	n := &node{runner: r, media: media}
	h := s.Handler()
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body []byte
		if req.Method == http.MethodPost {
			body, _ = io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewReader(body))
		}
		n.mu.Lock()
		n.log = append(n.log, seen{req.Method, req.URL.Path, req.Header.Get("Authorization"), body})
		tamper := n.tamper
		n.mu.Unlock()
		if tamper != nil && strings.HasPrefix(req.URL.Path, "/fleet/media/") {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			out := tamper(strings.TrimPrefix(req.URL.Path, "/fleet/media/"), rec.Body.Bytes())
			w.WriteHeader(rec.Code)
			_, _ = w.Write(out)
			return
		}
		h.ServeHTTP(w, req)
	}))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *node) requests() []seen {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]seen(nil), n.log...)
}

func (n *node) posts() []seen {
	var out []seen
	for _, r := range n.requests() {
		if r.method == http.MethodPost {
			out = append(out, r)
		}
	}
	return out
}

func clientCfg(t *testing.T, nodes ...*node) config.Config {
	cfg := config.Config{MediaDir: t.TempDir(), FleetAuthToken: "tok"}
	for _, n := range nodes {
		cfg.DelegateRemotes = append(cfg.DelegateRemotes, n.srv.URL)
	}
	return cfg
}

func writeFile(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func video(params map[string]any) core.Request {
	return core.Request{Task: core.TaskGenerateVideo, Input: "a slow pan", Params: params}
}

func decode(t *testing.T, res core.Result) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(res.Data, &m); err != nil {
		t.Fatalf("result data %s: %v", res.Data, err)
	}
	return m
}

func noTempLeft(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("a partial download was left behind: %s", e.Name())
		}
	}
}

// ---- routing ---------------------------------------------------------------------------------------

type recordingRunner struct {
	mu   sync.Mutex
	reqs []core.Request
}

func (r *recordingRunner) Run(_ context.Context, req core.Request) core.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	return core.Result{OK: true, Data: json.RawMessage(`{"local":true}`)}
}

func (r *recordingRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

// localGraphCfg is a client with a REAL run-graph script on disk: mediacap derives its route CONFIGURED,
// so run_graph has a local lane. Every other route is unbound.
func localGraphCfg(t *testing.T, n *node) config.Config {
	cfg := clientCfg(t, n)
	cfg.RunGraphScript = writeFile(t, t.TempDir(), "comfy-run-graph.mjs", []byte("//"))
	return cfg
}

func graphReq(t *testing.T) core.Request {
	return core.Request{Task: core.TaskRunGraph, Params: map[string]any{
		"graph_path": writeFile(t, t.TempDir(), "g.json", []byte(`{"1":{"class_type":"KSampler"}}`)),
	}}
}

func TestAutoWithALocalLaneRunsLocalAndMakesNoHTTPCall(t *testing.T) {
	n := startNode(t, nodeOpts{})
	cfg := localGraphCfg(t, n)
	local := &recordingRunner{}
	res := Run(context.Background(), cfg, local, graphReq(t), "", nil)
	if !res.OK || local.count() != 1 {
		t.Fatalf("auto with a local lane must call the runner once: runs=%d res=%+v", local.count(), res)
	}
	if string(res.Data) != `{"local":true}` || res.Meta.Node != "" || res.Meta.Placement != "" {
		t.Fatalf("the local result must come back untouched: %+v", res)
	}
	if got := n.requests(); len(got) != 0 {
		t.Fatalf("auto with a local lane touched the network: %+v", got)
	}
	// The same request with an explicit auto, and with local, is the same call.
	for _, route := range []string{"auto", "local", "LOCAL"} {
		if Run(context.Background(), cfg, local, graphReq(t), route, nil); len(n.requests()) != 0 {
			t.Fatalf("route %q reached the network", route)
		}
	}
}

func TestLocalRouteNeverLeavesTheMachineEvenWithNoLane(t *testing.T) {
	n := startNode(t, nodeOpts{})
	cfg := clientCfg(t, n) // no local lane at all
	local := &recordingRunner{}
	if res := Run(context.Background(), cfg, local, video(nil), "local", nil); !res.OK || local.count() != 1 {
		t.Fatalf("local must run the runner: %+v", res)
	}
	if len(n.requests()) != 0 {
		t.Fatal("route local reached the network")
	}
}

// A single box (no delegate_remotes) keeps today's behaviour under auto: the runner's own answer, whatever
// it is, with no placement attempt.
func TestAutoWithNoLaneAndNoFleetKeepsTheLocalCall(t *testing.T) {
	cfg := config.Config{MediaDir: t.TempDir()}
	local := &recordingRunner{}
	if res := Run(context.Background(), cfg, local, video(nil), "auto", nil); !res.OK || local.count() != 1 {
		t.Fatalf("with nowhere else to go, auto must run the runner: %+v", res)
	}
}

func TestAnUnknownRouteIsRefusedBeforeAnything(t *testing.T) {
	local := &recordingRunner{}
	res := Run(context.Background(), config.Config{}, local, video(nil), "sideways", nil)
	if res.OK || !res.Deferred || res.DeferClass != core.DeferClassContract || local.count() != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestAutoWithNoLaneGoesToTheFleetAndSaysWhy(t *testing.T) {
	n := startNode(t, nodeOpts{})
	cfg := clientCfg(t, n)
	local := &recordingRunner{}
	res := Run(context.Background(), cfg, local, core.Request{Task: core.TaskGenerateImage, Input: "a red door"}, "auto", nil)
	if !res.OK || local.count() != 0 {
		t.Fatalf("auto with no lane must run on the node: %+v", res)
	}
	if res.Meta.Placement != "remote: no image lane on this machine" || res.Meta.Node != "render-node" {
		t.Fatalf("placement %q node %q", res.Meta.Placement, res.Meta.Node)
	}
	forced := Run(context.Background(), cfg, local, core.Request{Task: core.TaskGenerateImage, Input: "a red door"}, "remote", nil)
	if !forced.OK || forced.Meta.Placement != "remote: forced" {
		t.Fatalf("%+v", forced)
	}
}

// ---- the wire --------------------------------------------------------------------------------------

func TestAStillTravelsAsAMediaJobBundleWhoseHashMatches(t *testing.T) {
	n := startNode(t, nodeOpts{mediaInputs: true})
	cfg := clientCfg(t, n)
	still := writeFile(t, t.TempDir(), "My Photo.PNG", png)
	out := filepath.Join(t.TempDir(), "sub", "clip.mp4")
	res := Run(context.Background(), cfg, &recordingRunner{}, video(map[string]any{
		"still": still, "out": out, "frames": 33, "seed": 7, "fast": true, "reserve_vram": "1.5", "negative": "blur",
	}), "remote", nil)
	if !res.OK {
		t.Fatalf("%+v", res)
	}

	posts := n.posts()
	if len(posts) != 1 || posts[0].path != "/fleet/media-job" {
		t.Fatalf("a job with a still must go through /fleet/media-job, posts: %+v", posts)
	}
	if posts[0].auth != "Bearer tok" {
		t.Errorf("the media-job door needs the fleet bearer, got %q", posts[0].auth)
	}
	var wire struct {
		JobID        string            `json:"job_id"`
		TaskType     string            `json:"task_type"`
		Payload      map[string]any    `json:"payload"`
		Bundle       string            `json:"bundle"`
		BundleSHA256 string            `json:"bundle_sha256"`
		Inputs       map[string]string `json:"inputs"`
	}
	if err := json.Unmarshal(posts[0].body, &wire); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(wire.Bundle)
	if err != nil || wire.BundleSHA256 != sha(raw) {
		t.Fatalf("bundle_sha256 %q does not match the bundle (%v)", wire.BundleSHA256, err)
	}
	if wire.TaskType != "video-gen" || wire.Inputs["still"] != "still.png" {
		t.Fatalf("task %q inputs %v", wire.TaskType, wire.Inputs)
	}
	if _, has := wire.Payload["still"]; has {
		t.Errorf("a local path must never travel: payload %v", wire.Payload)
	}
	if _, has := wire.Payload["out"]; has {
		t.Errorf("out must never travel: payload %v", wire.Payload)
	}
	// The wire carries the exact field names and number shapes the node's builders decode.
	if wire.Payload["prompt"] != "a slow pan" || wire.Payload["frames"] != float64(33) || wire.Payload["seed"] != float64(7) ||
		wire.Payload["fast"] != true || wire.Payload["reserve_vram"] != 1.5 || wire.Payload["negative"] != "blur" {
		t.Fatalf("payload %v", wire.Payload)
	}
	if got := n.runner.last(); got.Task != core.TaskGenerateVideo || got.Params["frames"] != 33 {
		t.Fatalf("the node's pipeline ran %+v", got)
	}
	if !bytes.Equal(n.runner.inputs["still"], png) {
		t.Fatal("the node's pipeline did not read the still's bytes")
	}

	// The clip arrived at the caller's out, verified.
	m := decode(t, res)
	if m["video_path"] != out {
		t.Fatalf("video_path %v, want %s", m["video_path"], out)
	}
	if b, _ := os.ReadFile(out); string(b) != "VIDEO" {
		t.Fatalf("the clip at out holds %q", b)
	}
	if m["node"] != "render-node" || m["remote_job_id"] == "" || m["remote_job_id"] != wire.JobID {
		t.Errorf("node %v remote_job_id %v (job %s)", m["node"], m["remote_job_id"], wire.JobID)
	}
	if _, un := m["unverified"]; un {
		t.Errorf("the node published artifacts, so the result must not be marked unverified: %v", m)
	}
	if res.Meta.Node != "render-node" || res.Meta.Placement != "remote: forced" {
		t.Errorf("meta %+v", res.Meta)
	}
	noTempLeft(t, filepath.Dir(out))
}

func TestAJobWithNoInputGoesThroughDispatchAndNeverSendsOut(t *testing.T) {
	n := startNode(t, nodeOpts{mediaInputs: true})
	cfg := clientCfg(t, n)
	req := core.Request{Task: core.TaskGenerateImage, Input: "a red door", Params: map[string]any{
		"out": filepath.Join(t.TempDir(), "door.png"), "width": 512, "height": 768, "steps": 20, "seed": 3,
		"negative": "text", "family": "", "transparent": true,
	}}
	res := Run(context.Background(), cfg, &recordingRunner{}, req, "remote", nil)
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	posts := n.posts()
	if len(posts) != 1 || posts[0].path != "/fleet/dispatch" {
		t.Fatalf("no input file: the job must use /fleet/dispatch, posts %+v", posts)
	}
	var wire struct {
		TaskType string         `json:"task_type"`
		Payload  map[string]any `json:"payload"`
	}
	_ = json.Unmarshal(posts[0].body, &wire)
	if wire.TaskType != "image-gen" || wire.Payload["prompt"] != "a red door" || wire.Payload["width"] != float64(512) || wire.Payload["transparent"] != true {
		t.Fatalf("wire %+v", wire)
	}
	if _, has := wire.Payload["out"]; has {
		t.Fatal("out reached the wire")
	}
	if _, has := wire.Payload["family"]; has {
		t.Error("an empty family must be omitted")
	}
	m := decode(t, res)
	if p, _ := m["image_path"].(string); p != req.Params["out"] {
		t.Fatalf("image_path %v, want the caller's out", m["image_path"])
	}
	if !bytes.Contains(res.Data, []byte("9007199254740993")) {
		t.Fatalf("a large number in the node's result lost digits: %s", res.Data)
	}
}

func TestAnimateAndAudioShipTheirFiles(t *testing.T) {
	n := startNode(t, nodeOpts{mediaInputs: true})
	cfg := clientCfg(t, n)
	dir := t.TempDir()
	res := Run(context.Background(), cfg, &recordingRunner{}, core.Request{Task: core.TaskAnimateCharacter, Input: "a fox", Params: map[string]any{
		"ref": writeFile(t, dir, "fox.png", png), "driver": writeFile(t, dir, "dance.mp4", mp4), "pose_strength": "0.8", "reserve_vram": 2.0,
	}}, "remote", nil)
	if !res.OK {
		t.Fatalf("animate: %+v", res)
	}
	if !bytes.Equal(n.runner.inputs["ref"], png) || !bytes.Equal(n.runner.inputs["driver"], mp4) {
		t.Fatal("the node did not read both files")
	}
	if r := n.runner.last(); r.Params["pose_strength"] != "0.8" || r.Params["reserve_vram"] != "2" {
		t.Fatalf("animate params %v", r.Params)
	}
	res = Run(context.Background(), cfg, &recordingRunner{}, core.Request{Task: core.TaskGenerateAudio, Input: "hola", Params: map[string]any{
		"clone": writeFile(t, dir, "voice.wav", wav), "lang": "es", "seconds": 10,
	}}, "remote", nil)
	if !res.OK {
		t.Fatalf("audio: %+v", res)
	}
	if !bytes.Equal(n.runner.inputs["clone"], wav) {
		t.Fatal("the node did not read the clone sample")
	}
	for _, p := range n.posts() {
		if p.path != "/fleet/media-job" {
			t.Errorf("a job with input files used %s", p.path)
		}
	}
}

func TestRunGraphCarriesTheGraphInlineAndFetchesEveryOutput(t *testing.T) {
	n := startNode(t, nodeOpts{})
	cfg := clientCfg(t, n)
	dir := t.TempDir()
	graph := writeFile(t, dir, "g.json", []byte(`{"1":{"class_type":"KSampler"}}`))
	manifest := writeFile(t, dir, "m.json", []byte(`{"models":[]}`))
	out := filepath.Join(t.TempDir(), "main.png")
	res := Run(context.Background(), cfg, &recordingRunner{}, core.Request{Task: core.TaskRunGraph, Params: map[string]any{
		"graph_path": graph, "manifest_path": manifest, "out_dir": "/etc", "reserve_vram": "0.5", "out": out,
	}}, "remote", nil)
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	posts := n.posts()
	if len(posts) != 1 || posts[0].path != "/fleet/dispatch" {
		t.Fatalf("run-graph carries its graph inline over /fleet/dispatch: %+v", posts)
	}
	var wire struct {
		Payload map[string]json.RawMessage `json:"payload"`
	}
	_ = json.Unmarshal(posts[0].body, &wire)
	if string(wire.Payload["graph"]) != `{"1":{"class_type":"KSampler"}}` || string(wire.Payload["manifest"]) != `{"models":[]}` {
		t.Fatalf("payload %s", posts[0].body)
	}
	if _, has := wire.Payload["out_dir"]; has {
		t.Error("out_dir must never travel")
	}
	m := decode(t, res)
	if m["image_path"] != out {
		t.Fatalf("the primary output (image_path) goes to out: %v", m["image_path"])
	}
	outs, _ := m["outputs"].(map[string]any)
	var paths []string
	for _, files := range outs {
		for _, f := range files.([]any) {
			paths = append(paths, f.(map[string]any)["path"].(string))
		}
	}
	if len(paths) != 2 {
		t.Fatalf("outputs %v", outs)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("an output path was not rewritten to a local file: %s", p)
		}
		if strings.HasPrefix(p, n.media) {
			t.Errorf("output path still names the node's file: %s", p)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(cfg.MediaDir, "graph-b.mp4")); string(b) != "GB" {
		t.Fatalf("the secondary output belongs in media_dir: %q", b)
	}
	if b, _ := os.ReadFile(out); string(b) != "GA" {
		t.Fatalf("the primary output holds %q", b)
	}
}

// ---- verification ----------------------------------------------------------------------------------

func TestAShaMismatchDefersAndLeavesNoFile(t *testing.T) {
	n := startNode(t, nodeOpts{})
	n.tamper = func(name string, b []byte) []byte {
		if strings.HasPrefix(name, "graph-b") {
			return []byte("EVIL")
		}
		return b
	}
	cfg := clientCfg(t, n)
	out := filepath.Join(t.TempDir(), "main.png")
	res := Run(context.Background(), cfg, &recordingRunner{}, core.Request{Task: core.TaskRunGraph, Params: map[string]any{
		"graph_path": writeFile(t, t.TempDir(), "g.json", []byte(`{"1":{"class_type":"X"}}`)), "out": out,
	}}, "remote", nil)
	if res.OK || !res.Deferred || res.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("a hash mismatch must defer as infrastructure: %+v", res)
	}
	if !strings.Contains(res.Reason, "sha256") || !strings.Contains(res.Reason, "graph-b.mp4") {
		t.Fatalf("the defer must name the file and the hashes: %s", res.Reason)
	}
	for _, p := range []string{out, out + ".part", filepath.Join(cfg.MediaDir, "graph-b.mp4"), filepath.Join(cfg.MediaDir, "graph-b.mp4.part")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s was left behind after a mismatch (%v)", p, err)
		}
	}
	// A file the caller already had at out is not overwritten by a download that fails verification.
	if err := os.WriteFile(out, []byte("MINE"), 0o644); err != nil {
		t.Fatal(err)
	}
	Run(context.Background(), cfg, &recordingRunner{}, core.Request{Task: core.TaskRunGraph, Params: map[string]any{
		"graph_path": writeFile(t, t.TempDir(), "g.json", []byte(`{"1":{"class_type":"X"}}`)), "out": out,
	}}, "remote", nil)
	if b, _ := os.ReadFile(out); string(b) != "MINE" {
		t.Fatalf("a failed verification replaced the caller's file with %q", b)
	}
}

// An older node publishes no artifacts: the file still arrives, marked unverified.
func TestANodeWithoutArtifactsYieldsAnUnverifiedResult(t *testing.T) {
	media := t.TempDir()
	_ = os.WriteFile(filepath.Join(media, "x.png"), []byte("PNGDATA"), 0o644)
	data := fmt.Sprintf(`{"image_path":%q}`, filepath.Join(media, "x.png"))
	fake := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, jobData: data, media: media})
	cfg := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{fake.URL}}
	res := Run(context.Background(), cfg, &recordingRunner{}, core.Request{Task: core.TaskGenerateImage, Input: "p"}, "remote", nil)
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	if m := decode(t, res); m["unverified"] != true {
		t.Fatalf("no artifacts from the node: the result must say unverified: %v", m)
	}
	if b, _ := os.ReadFile(filepath.Join(cfg.MediaDir, "x.png")); string(b) != "PNGDATA" {
		t.Fatalf("the image was not fetched: %q", b)
	}
}

func TestNodeNameRefusesAnythingButAPlainName(t *testing.T) {
	for _, bad := range []string{"..", ".", "/", "C:", `C:\x`, "a:b", "a\x00b"} {
		if _, err := nodeName(bad); err == nil && bad != `C:\x` {
			t.Errorf("nodeName(%q) accepted", bad)
		}
	}
	for in, want := range map[string]string{"/srv/media/x.png": "x.png", `C:\media\y.mp4`: "y.mp4", "z.wav": "z.wav"} {
		if got, err := nodeName(in); err != nil || got != want {
			t.Errorf("nodeName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// A hostile result path cannot become a file outside media_dir.
	fake := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, jobData: `{"image_path":"../../../etc/passwd"}`, media: t.TempDir()})
	cfg := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{fake.URL}}
	res := Run(context.Background(), cfg, &recordingRunner{}, core.Request{Task: core.TaskGenerateImage, Input: "p"}, "remote", nil)
	if res.OK {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(filepath.Join(cfg.MediaDir, "passwd")); err == nil {
		// the guard takes the base name, so the file is a plain name in media_dir at worst
		t.Log("the traversal collapsed to a plain name inside media_dir")
	}
}

// ---- placement -------------------------------------------------------------------------------------

type fakeOpts struct {
	tasks   []string
	routes  string // raw JSON for media_routes, "" = absent (an older node)
	lease   string // raw JSON for lease, "" = none
	queue   int
	running int
	status  int    // dispatch status, 0 = 202
	jobData string // done job data
	media   string
}

// fakeNode is a hand-written node: health with exactly the fields given, dispatch with a chosen status.
func fakeNode(t *testing.T, o fakeOpts) *httptest.Server {
	t.Helper()
	tasks, _ := json.Marshal(o.tasks)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/fleet/health":
			extra := ""
			if o.routes != "" {
				extra += `,"media_routes":` + o.routes
			}
			if o.lease != "" {
				extra += `,"lease":` + o.lease
			}
			fmt.Fprintf(w, `{"node_id":"fake-%d","schema_version":1,"supported_task_types":%s,"queue_depth":%d,"jobs_running":%d%s}`,
				time.Now().UnixNano()%1000, tasks, o.queue+o.running, o.running, extra)
		case strings.HasPrefix(r.URL.Path, "/fleet/dispatch"), strings.HasPrefix(r.URL.Path, "/fleet/media-job"):
			if o.status != 0 {
				w.WriteHeader(o.status)
				fmt.Fprint(w, `{"status":"error","error":"fake refusal"}`)
				return
			}
			w.WriteHeader(202)
			fmt.Fprint(w, `{"status":"accepted"}`)
		case strings.HasPrefix(r.URL.Path, "/fleet/jobs/"):
			fmt.Fprintf(w, `{"state":"done","data":%s}`, o.jobData)
		case strings.HasPrefix(r.URL.Path, "/fleet/media/"):
			http.ServeFile(w, r, filepath.Join(o.media, strings.TrimPrefix(r.URL.Path, "/fleet/media/")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestA503OrA429IsACapacityDeferAnda400IsAContractDefer(t *testing.T) {
	for status, class := range map[int]string{503: core.DeferClassCapacity, 429: core.DeferClassCapacity, 400: core.DeferClassContract,
		413: core.DeferClassContract, 403: core.DeferClassConfig, 500: core.DeferClassInfrastructure} {
		fake := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, status: status})
		cfg := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{fake.URL}}
		res := Run(context.Background(), cfg, &recordingRunner{}, core.Request{Task: core.TaskGenerateImage, Input: "p"}, "remote", nil)
		if res.OK || res.DeferClass != class {
			t.Errorf("status %d: class %q, want %q (%s)", status, res.DeferClass, class, res.Reason)
		}
	}
}

func TestANodeWithoutTheRouteIsSkippedAndNamed(t *testing.T) {
	routes := `[{"route":"generate_video","engine":"comfyui","state":"BOUND-BUT-MISSING"}]`
	missing := fakeNode(t, fakeOpts{tasks: []string{"video-gen"}, routes: routes})
	cfg := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{missing.URL}}
	res := Run(context.Background(), cfg, &recordingRunner{}, video(nil), "remote", nil)
	if res.OK || res.DeferClass != core.DeferClassCapacity {
		t.Fatalf("%+v", res)
	}
	for _, want := range []string{missing.URL, "generate_video", "BOUND-BUT-MISSING"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("the defer must name %q: %s", want, res.Reason)
		}
	}
	// A node that reports routes and lists the route is eligible; one that reports none (older) is too.
	ok := fakeNode(t, fakeOpts{tasks: []string{"video-gen"}, routes: `[{"route":"generate_video","engine":"comfyui","state":"CONFIGURED"}]`, jobData: `{"video_path":"/x/v.mp4"}`, media: t.TempDir()})
	old := fakeNode(t, fakeOpts{tasks: []string{"video-gen"}})
	for name, srv := range map[string]*httptest.Server{"configured": ok, "older": old} {
		_, node, err := pickNode(context.Background(), config.Config{}, []string{srv.URL}, "video-gen", "video-gen", []string{"generate_video"})
		if err != nil || node == "" {
			t.Errorf("%s node must be eligible: %v", name, err)
		}
	}
}

func TestPlacementNamesEveryMissAndMediaJobIsRequiredForInputs(t *testing.T) {
	notServing := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}})
	noDoor := fakeNode(t, fakeOpts{tasks: []string{"video-gen"}})
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()
	_, _, err := pickNode(context.Background(), config.Config{}, []string{notServing.URL, noDoor.URL, downURL}, "video-gen", "media-job", []string{"generate_video"})
	if err == nil {
		t.Fatal("no node can take a still: every one must be refused")
	}
	for _, want := range []string{notServing.URL, "does not serve video-gen", noDoor.URL, "does not advertise media-job", "fleet_media_inputs", downURL} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the miss list must name %q: %v", want, err)
		}
	}
	var pe *placementError
	if !asPlacement(err, &pe) || pe.class != core.DeferClassCapacity {
		t.Errorf("a placement miss is a capacity defer: %v", err)
	}
	// Without input files the same node is fine: media-job is only required when files travel.
	if _, _, err := pickNode(context.Background(), config.Config{}, []string{noDoor.URL}, "video-gen", "video-gen", []string{"generate_video"}); err != nil {
		t.Errorf("a node without media-job serves a job with no input file: %v", err)
	}
}

func asPlacement(err error, target **placementError) bool {
	pe, ok := err.(*placementError)
	if ok {
		*target = pe
	}
	return ok
}

func TestRankingPrefersNoHeldLeaseThenTheShortestQueueThenConfigOrder(t *testing.T) {
	routes := `[{"route":"generate_video","engine":"comfyui","state":"CONFIGURED"}]`
	mk := func(lease string, queue, running int) string {
		return fakeNode(t, fakeOpts{tasks: []string{"video-gen"}, routes: routes, lease: lease, queue: queue, running: running}).URL
	}
	held := mk(`{"held":true,"class":"media","busy":false}`, 0, 0)
	busy := mk("", 3, 1)
	idle := mk("", 0, 1)
	text := mk(`{"held":true,"class":"text","busy":true}`, 0, 0)
	first := mk("", 0, 1)

	pick := func(bases ...string) string {
		t.Helper()
		b, _, err := pickNode(context.Background(), config.Config{}, bases, "video-gen", "video-gen", []string{"generate_video"})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if got := pick(held, busy); got != busy {
		t.Errorf("a node holding a lease ranks after one without, even with a longer queue: picked %s", got)
	}
	if got := pick(busy, idle); got != idle {
		t.Errorf("the shorter queue wins: picked %s", got)
	}
	if got := pick(text, busy); got != busy {
		t.Errorf("a held TEXT lease skips the node: picked %s", got)
	}
	if got := pick(idle, first); got != idle {
		t.Errorf("config order breaks a tie: picked %s", got)
	}
	if got := pick(first, idle); got != first {
		t.Errorf("config order breaks a tie: picked %s", got)
	}
	_, _, err := pickNode(context.Background(), config.Config{}, []string{text}, "video-gen", "video-gen", []string{"generate_video"})
	if err == nil || !strings.Contains(err.Error(), "text lease") {
		t.Errorf("the miss must name the text lease: %v", err)
	}
}

func TestCallerRemotesMustBeAmongDelegateRemotes(t *testing.T) {
	n := startNode(t, nodeOpts{})
	stranger := startNode(t, nodeOpts{})
	cfg := clientCfg(t, n)
	res := Run(context.Background(), cfg, &recordingRunner{}, video(nil), "remote", []string{stranger.srv.URL})
	if res.OK || res.DeferClass != core.DeferClassContract || !strings.Contains(res.Reason, "delegate_remotes") {
		t.Fatalf("%+v", res)
	}
	if len(n.requests()) != 0 || len(stranger.requests()) != 0 {
		t.Fatal("a refused remote must not be probed: no node was to be contacted")
	}
	// A trailing slash is the same node; a subset narrows the fleet.
	if res := Run(context.Background(), cfg, &recordingRunner{}, core.Request{Task: core.TaskGenerateImage, Input: "p"}, "remote", []string{n.srv.URL + "/"}); !res.OK {
		t.Fatalf("a configured node named with a trailing slash must be accepted: %+v", res)
	}
	// With no delegate_remotes at all, naming a remote is still refused.
	bare := config.Config{MediaDir: t.TempDir()}
	if res := Run(context.Background(), bare, &recordingRunner{}, video(nil), "remote", []string{n.srv.URL}); res.OK || res.DeferClass != core.DeferClassContract {
		t.Fatalf("%+v", res)
	}
	// And with none configured and none named, remote is a config defer.
	if res := Run(context.Background(), bare, &recordingRunner{}, video(nil), "remote", nil); res.OK || res.DeferClass != core.DeferClassConfig {
		t.Fatalf("%+v", res)
	}
}

// ---- contract --------------------------------------------------------------------------------------

func TestWhatTheNodeCannotCarryIsRefusedByNameNotDropped(t *testing.T) {
	n := startNode(t, nodeOpts{mediaInputs: true})
	cfg := clientCfg(t, n)
	dir := t.TempDir()
	still := writeFile(t, dir, "s.png", png)
	for name, tc := range map[string]struct {
		req  core.Request
		want string
	}{
		"refine false":           {core.Request{Task: core.TaskGenerateImage, Input: "p", Params: map[string]any{"refine": false}}, "refine=false"},
		"tts_voice":              {core.Request{Task: core.TaskGenerateAudio, Input: "p", Params: map[string]any{"tts_voice": "ana"}}, "tts_voice"},
		"transformer":            {video(map[string]any{"transformer": "bf16.safetensors"}), "transformer"},
		"missing still":          {video(map[string]any{"still": filepath.Join(dir, "gone.png")}), "still"},
		"a directory":            {video(map[string]any{"still": dir}), "not a regular file"},
		"animate needs both":     {core.Request{Task: core.TaskAnimateCharacter, Input: "p", Params: map[string]any{"ref": still}}, "ref and driver"},
		"no prompt":              {core.Request{Task: core.TaskGenerateVideo}, "prompt required"},
		"not media":              {core.Request{Task: core.TaskSummarize, Input: "x"}, "not a media task"},
		"graph is not an object": {core.Request{Task: core.TaskRunGraph, Params: map[string]any{"graph_path": writeFile(t, dir, "bad.json", []byte(`[1]`))}}, "JSON object"},
		"graph missing":          {core.Request{Task: core.TaskRunGraph}, "graph_path or graph_json required"},
	} {
		res := Run(context.Background(), cfg, &recordingRunner{}, tc.req, "remote", nil)
		if res.OK || res.DeferClass != core.DeferClassContract || !strings.Contains(res.Reason, tc.want) {
			t.Errorf("%s: %+v (want a contract defer naming %q)", name, res, tc.want)
		}
	}
	if len(n.requests()) != 0 {
		t.Fatalf("a refused contract must not touch the network: %+v", n.requests())
	}
	// refine=true (or absent) is fine: only an explicit false cannot travel.
	if res := Run(context.Background(), cfg, &recordingRunner{}, core.Request{Task: core.TaskGenerateImage, Input: "p", Params: map[string]any{"refine": true}}, "remote", nil); !res.OK {
		t.Fatalf("%+v", res)
	}
}

func TestTheNodeRefusingTheBundleIsAContractDefer(t *testing.T) {
	n := startNode(t, nodeOpts{mediaInputs: true})
	cfg := clientCfg(t, n)
	// A text file where an image goes: the node's magic-byte sniff refuses it with a 400.
	res := Run(context.Background(), cfg, &recordingRunner{}, video(map[string]any{"still": writeFile(t, t.TempDir(), "s.png", []byte("not an image at all, only text"))}), "remote", nil)
	if res.OK || res.DeferClass != core.DeferClassContract || !strings.Contains(res.Reason, "not an image") {
		t.Fatalf("%+v", res)
	}
}

func TestANodeDeferIsPassedThroughWithTheNode(t *testing.T) {
	n := startNode(t, nodeOpts{})
	n.runner.deferAs = "GPU_BUSY: the card is held"
	res := Run(context.Background(), clientCfg(t, n), &recordingRunner{}, video(nil), "remote", nil)
	if res.OK || !res.Deferred || !strings.Contains(res.Reason, "GPU_BUSY") || res.Meta.Node != "render-node" {
		t.Fatalf("%+v", res)
	}
}

func TestTheBundleCapIsCheckedBeforeAnyUpload(t *testing.T) {
	n := startNode(t, nodeOpts{mediaInputs: true})
	cfg := clientCfg(t, n)
	cfg.FleetMediaInputsMaxMB = 1
	big := make([]byte, 2<<20)
	copy(big, png)
	for i := 64; i < len(big); i++ {
		big[i] = byte(i * 7919 >> 3) // not compressible enough to fit under 1 MiB
	}
	res := Run(context.Background(), cfg, &recordingRunner{}, video(map[string]any{"still": writeFile(t, t.TempDir(), "big.png", big)}), "remote", nil)
	if res.OK || res.DeferClass != core.DeferClassContract || !strings.Contains(res.Reason, "fleet_media_inputs_max_mb") {
		t.Fatalf("%+v", res)
	}
	if len(n.requests()) != 0 {
		t.Fatal("an oversize bundle was uploaded")
	}
}

func TestBundleNamesAreFieldPlusExtension(t *testing.T) {
	for in, want := range map[string]string{
		"/x/My Photo.PNG": "still.png", "/x/clip": "still", "/x/we ird.p:g": "still", "/x/a.verylongextension": "still", `C:\x\y.wav`: "still.wav",
	} {
		if got := bundleName("still", in); got != want {
			t.Errorf("bundleName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBudgetsCoverEveryTask(t *testing.T) {
	for task, want := range map[string]time.Duration{taskImage: 2 * time.Hour, taskVideo: 6 * time.Hour, taskAnimate: 6 * time.Hour, taskAudio: time.Hour, taskRunGraph: 2 * time.Hour} {
		if Budgets[task] != want {
			t.Errorf("%s budget %v, want %v", task, Budgets[task], want)
		}
	}
}

func TestLocalConfiguredReadsTheFilesNotTheBinding(t *testing.T) {
	script := writeFile(t, t.TempDir(), "comfy-run-graph.mjs", []byte("//"))
	ghost := filepath.Join(t.TempDir(), "nope.mjs")
	graph := core.Request{Task: core.TaskRunGraph}
	if !LocalConfigured(config.Config{RunGraphScript: script}, graph) {
		t.Error("a present script is a local run_graph lane")
	}
	if LocalConfigured(config.Config{RunGraphScript: ghost}, graph) {
		t.Error("a bound script that is not on disk is no lane: a default config binds every script")
	}
	if LocalConfigured(config.Config{}, graph) || LocalConfigured(config.Config{}, video(nil)) {
		t.Error("an empty config has no lane")
	}
	if LocalConfigured(config.Config{VideoGenScript: ghost}, video(nil)) {
		t.Error("a missing video script is no lane")
	}
	// Audio: music needs the music route, voice=endpoint the speech endpoint.
	music := core.Request{Task: core.TaskGenerateAudio, Params: map[string]any{"kind": "music"}}
	endpoint := core.Request{Task: core.TaskGenerateAudio, Params: map[string]any{"voice": "endpoint"}}
	if LocalConfigured(config.Config{TTSEndpoint: "http://192.0.2.5:8000"}, music) {
		t.Error("a speech endpoint is not a music lane")
	}
	if !LocalConfigured(config.Config{TTSEndpoint: "http://192.0.2.5:8000"}, endpoint) {
		t.Error("a speech endpoint is the voice=endpoint lane")
	}
}
