package composeremote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
)

// nodeRunner stands in for a render node's pipeline: it writes a "video" and a snapshot into the
// node's media dir and reports them the way the compose pipeline does.
type nodeRunner struct {
	mu       sync.Mutex
	media    string
	defer_   string
	lastReq  core.Request
	sawFiles []string
}

func (n *nodeRunner) Run(_ context.Context, req core.Request) core.Result {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lastReq = req
	if dir, _ := req.Params["project_dir"].(string); dir != "" {
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(dir, p)
				n.sawFiles = append(n.sawFiles, filepath.ToSlash(rel))
			}
			return nil
		})
	}
	if n.defer_ != "" {
		return core.Deferf(n.defer_, "", core.Meta{})
	}
	video := filepath.Join(n.media, "compose-abcd1234.mp4")
	snap := filepath.Join(n.media, "compose-abcd1234-snap-00-at-1s.png")
	_ = os.WriteFile(video, []byte("MP4DATA"), 0o644)
	_ = os.WriteFile(snap, []byte("PNGDATA"), 0o644)
	data, _ := json.Marshal(map[string]any{"video_path": video, "snapshots": []string{snap}, "frames": 90, "codec": "h264"})
	return core.Result{OK: true, Data: data}
}

type node struct {
	srv    *httptest.Server
	runner *nodeRunner
	hits   atomic.Int64
	// lastPost is the header of the last POST the node received (the attribution headers ride it).
	hdrMu    sync.Mutex
	lastPost http.Header
}

func (n *node) postHeader() http.Header {
	n.hdrMu.Lock()
	defer n.hdrMu.Unlock()
	return n.lastPost.Clone()
}

// startNode runs a real fleet node server (the doors, auth, job store and media serving) behind httptest.
func startNode(t *testing.T, openProjects bool) *node {
	t.Helper()
	media := t.TempDir()
	cfg := config.Config{
		MediaDir: media, ComposeCacheDir: t.TempDir(),
		ComposeScript: "render/compose-hyperframes.mjs", HyperframesDir: "/x/hf", HyperframesBrowserPath: "/x/hf/chs",
		FleetAuthToken: "tok", FleetComposeProjects: openProjects,
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
	n := &node{runner: r}
	h := s.Handler()
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			n.hits.Add(1)
			n.hdrMu.Lock()
			n.lastPost = req.Header.Clone()
			n.hdrMu.Unlock()
		}
		h.ServeHTTP(w, req)
	}))
	t.Cleanup(n.srv.Close)
	return n
}

func clientCfg(t *testing.T, n *node) config.Config {
	return config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{n.srv.URL}, FleetAuthToken: "tok"}
}

func compose(params map[string]any) core.Request {
	return core.Request{Task: core.TaskComposeVideo, Params: params}
}

func TestATemplateRendersOnTheNodeAndComesBack(t *testing.T) {
	n := startNode(t, false)
	cfg := clientCfg(t, n)
	want := filepath.Join(t.TempDir(), "sub", "elsewhere.mp4")
	res, err := Call(context.Background(), cfg, compose(map[string]any{"template": "title-card", "variables": map[string]any{"title": "Hi"}, "format": "mp4", "out": want}))
	if err != nil || !res.OK {
		t.Fatalf("Call: %v %+v", err, res)
	}
	if got := n.runner.lastReq.Params["template"]; got != "title-card" {
		t.Errorf("the node rendered %v", n.runner.lastReq.Params)
	}
	if _, leaked := n.runner.lastReq.Params["out"]; leaked {
		t.Error("the caller's out must never reach the node")
	}
	var out map[string]any
	_ = json.Unmarshal(res.Data, &out)
	video, _ := out["video_path"].(string)
	if b, _ := os.ReadFile(video); string(b) != "MP4DATA" || video != want {
		t.Fatalf("the video must be fetched back to the caller's out %q, got %q at %q", want, b, video)
	}
	if res.Meta.Node != "render-node" {
		t.Errorf("Meta.Node = %q", res.Meta.Node)
	}
}

func TestAProjectTravelsAsABundleAndComesBack(t *testing.T) {
	n := startNode(t, true)
	cfg := clientCfg(t, n)
	proj := t.TempDir()
	_ = os.MkdirAll(filepath.Join(proj, "assets"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "index.html"), []byte(`<script src="assets/gsap.min.js"></script><img src="assets/x.png">`), 0o644)
	_ = os.WriteFile(filepath.Join(proj, "assets", "gsap.min.js"), []byte("gsap"), 0o644)
	_ = os.WriteFile(filepath.Join(proj, "assets", "x.png"), []byte("png"), 0o644)
	res, err := Call(context.Background(), cfg, compose(map[string]any{"project_dir": proj, "workers": 1, "snapshots": []float64{1}}))
	if err != nil || !res.OK {
		t.Fatalf("Call: %v %+v", err, res)
	}
	got := strings.Join(n.runner.sawFiles, ",")
	if !strings.Contains(got, "index.html") || !strings.Contains(got, "assets/gsap.min.js") || !strings.Contains(got, "assets/x.png") {
		t.Fatalf("the node must render the whole project, saw %q", got)
	}
	var out map[string]any
	_ = json.Unmarshal(res.Data, &out)
	video, _ := out["video_path"].(string)
	if !strings.HasPrefix(video, cfg.MediaDir) {
		t.Errorf("with no out the video lands in this box's media dir, got %q", video)
	}
	snaps, _ := out["snapshots"].([]any)
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %v", out["snapshots"])
	}
	if b, _ := os.ReadFile(snaps[0].(string)); string(b) != "PNGDATA" {
		t.Errorf("the snapshot must be fetched back, got %q", b)
	}
}

func TestAProjectThatLeavesItselfIsRefusedBeforeUpload(t *testing.T) {
	n := startNode(t, true)
	cfg := clientCfg(t, n)
	proj := t.TempDir()
	_ = os.WriteFile(filepath.Join(proj, "index.html"), []byte(`<img src="../../secret.png">`), 0o644)
	res, err := Call(context.Background(), cfg, compose(map[string]any{"project_dir": proj}))
	if err != nil || res.OK || !strings.Contains(res.Reason, "refers outside itself") {
		t.Fatalf("want a deferred result naming the reason, got %v %+v", err, res)
	}
	if n.hits.Load() != 0 {
		t.Fatalf("nothing may be uploaded, the node saw %d POSTs", n.hits.Load())
	}
}

func TestAProjectNeedsANodeThatOpenedTheDoor(t *testing.T) {
	n := startNode(t, false)
	cfg := clientCfg(t, n)
	proj := t.TempDir()
	_ = os.WriteFile(filepath.Join(proj, "index.html"), []byte(`<p>x</p>`), 0o644)
	_, err := Call(context.Background(), cfg, compose(map[string]any{"project_dir": proj}))
	if err == nil || !strings.Contains(err.Error(), "fleet_compose_projects") {
		t.Fatalf("want a placement error naming the door, got %v", err)
	}
}

func TestTheNodesTypedFailureComesThrough(t *testing.T) {
	n := startNode(t, false)
	n.runner.defer_ = "compose_video: LINT_ERRORS: 2 errors"
	res, err := Call(context.Background(), clientCfg(t, n), compose(map[string]any{"template": "title-card"}))
	if err != nil || res.OK || !strings.Contains(res.Reason, "LINT_ERRORS") {
		t.Fatalf("want the node's LINT_ERRORS defer, got %v %+v", err, res)
	}
}

func TestAWrongTokenIsRefusedByTheProjectDoor(t *testing.T) {
	n := startNode(t, true)
	cfg := clientCfg(t, n)
	cfg.FleetAuthToken = "wrong"
	proj := t.TempDir()
	_ = os.WriteFile(filepath.Join(proj, "index.html"), []byte(`<p>x</p>`), 0o644)
	_, err := Call(context.Background(), cfg, compose(map[string]any{"project_dir": proj}))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want a 401 placement error, got %v", err)
	}
}

func TestPngSequenceAndInputShapeAreRefused(t *testing.T) {
	n := startNode(t, true)
	cfg := clientCfg(t, n)
	for name, params := range map[string]map[string]any{
		"png-sequence": {"template": "title-card", "format": "png-sequence"},
		"two inputs":   {"template": "title-card", "html": "<p>x</p>"},
		"no input":     {},
	} {
		res, err := Call(context.Background(), cfg, compose(params))
		if err != nil || res.OK {
			t.Errorf("%s: want a deferred result, got %v %+v", name, err, res)
		}
	}
}

type localRunner struct{ called int }

func (l *localRunner) Run(context.Context, core.Request) core.Result {
	l.called++
	return core.Result{OK: true, Data: json.RawMessage(`{"video_path":"local.mp4"}`)}
}

func TestRunPlacesByRoute(t *testing.T) {
	n := startNode(t, false)
	withLane := clientCfg(t, n)
	withLane.ComposeScript, withLane.HyperframesDir, withLane.HyperframesBrowserPath = "r.mjs", "/hf", "/hf/c"
	noLane := clientCfg(t, n)
	req := compose(map[string]any{"template": "title-card"})

	lr := &localRunner{}
	if res := Run(context.Background(), withLane, lr, req, ""); !res.OK || lr.called != 1 || res.Meta.Placement != "" {
		t.Errorf("auto with the lane here runs locally, unchanged: %+v (local calls %d)", res, lr.called)
	}
	lr = &localRunner{}
	if res := Run(context.Background(), noLane, lr, req, "auto"); !res.OK || lr.called != 0 || !strings.HasPrefix(res.Meta.Placement, "remote") {
		t.Errorf("auto without a lane goes to a fleet node: %+v (local calls %d)", res, lr.called)
	}
	lr = &localRunner{}
	if res := Run(context.Background(), noLane, lr, req, "local"); lr.called != 1 || !res.OK {
		t.Errorf("local always runs here: %+v", res)
	}
	lr = &localRunner{}
	if res := Run(context.Background(), withLane, lr, req, "remote"); lr.called != 0 || res.Meta.Placement != "remote: forced" {
		t.Errorf("remote always goes to a fleet node: %+v", res)
	}
	if res := Run(context.Background(), noLane, &localRunner{}, req, "sideways"); res.OK || res.DeferClass != core.DeferClassContract {
		t.Errorf("an unknown route is a contract defer: %+v", res)
	}
	empty := noLane
	empty.DelegateRemotes = nil
	if res := Run(context.Background(), empty, &localRunner{}, req, "auto"); res.OK || res.DeferClass != core.DeferClassConfig ||
		!strings.Contains(res.Reason, "hyperframes_dir") || !strings.Contains(res.Reason, "delegate_remotes") {
		t.Errorf("no lane and no delegate_remotes is a config defer naming both fixes: %+v", res)
	}
}

// The node's output path is its own, but its name becomes a file here: only a plain name may.
func TestNodeNameIsAPlainFileName(t *testing.T) {
	for in, want := range map[string]string{
		`C:\offload\media\compose-1.mp4`:         "compose-1.mp4",
		"/srv/offload/media/compose-1.png":       "compose-1.png",
		"../../evil.mp4":                         "evil.mp4",
		`D:\media\compose-1-snap-00-at-1.5s.png`: "compose-1-snap-00-at-1.5s.png",
	} {
		if got, err := nodeName(in); err != nil || got != want {
			t.Errorf("nodeName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", ".", "..", "/", "a/..", `C:`, "media/x:y.mp4", "x\x00.mp4"} {
		if got, err := nodeName(in); err == nil {
			t.Errorf("nodeName(%q) = %q: must be refused (not a plain file name)", in, got)
		}
	}
}
