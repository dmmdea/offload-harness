package fleetnode

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/composebundle"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// composeProjectTestCache is the compose cache every project test extracts into (never a real path).
var composeProjectTestCache string

func init() {
	d, err := os.MkdirTemp("", "fleetnode-compose-cache-")
	if err != nil {
		panic(err)
	}
	composeProjectTestCache = d
	remoteMediaPayloads[ComposeProjectTask] = string(projectPayload(nil, map[string]string{"index.html": "<p>hello</p>"}))
}

func projectCfg() config.Config {
	return config.Config{
		ComposeScript: "render/compose-hyperframes.mjs", HyperframesDir: "/opt/offload/hyperframes",
		HyperframesBrowserPath: "/opt/offload/hyperframes/chs",
		FleetComposeProjects:   true, FleetAuthToken: "tok", ComposeCacheDir: composeProjectTestCache,
	}
}

// projectPayload packs files with the real packer and fills a payload; mut edits it afterwards.
func projectPayload(mut func(*ComposeProjectPayload), files map[string]string) []byte {
	dir, err := os.MkdirTemp("", "fleetnode-project-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(body), 0o644)
	}
	b, err := composebundle.Pack(dir, composebundle.Limits{})
	if err != nil {
		panic(err)
	}
	return payloadFor(b, mut)
}

func payloadFor(bundle []byte, mut func(*ComposeProjectPayload)) []byte {
	sum := sha256.Sum256(bundle)
	p := ComposeProjectPayload{JobID: "proj-1", Bundle: base64.StdEncoding.EncodeToString(bundle), BundleSHA256: hex.EncodeToString(sum[:]), Format: "mp4"}
	if mut != nil {
		mut(&p)
	}
	raw, _ := json.Marshal(p)
	return raw
}

// incompressible is n random bytes as a string, so gzip cannot shrink it under a cap.
func incompressible(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return string(b)
}

// rawTarGz is a one-member bundle with any name, for what the packer would never write.
func rawTarGz(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func leftoverProjectDirs(t *testing.T) []string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(composeProjectTestCache, "fleet-projects"))
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestComposeProjectIsOpenOnlyWithOptInTokenAndLane(t *testing.T) {
	if !slices.Contains(SupportedTasks(projectCfg()), ComposeProjectTask) {
		t.Fatalf("an opted-in node with a token and the lane must advertise compose-project: %v", SupportedTasks(projectCfg()))
	}
	for name, mut := range map[string]func(*config.Config){
		"not opted in": func(c *config.Config) { c.FleetComposeProjects = false },
		"no token":     func(c *config.Config) { c.FleetAuthToken = "" },
		"no lane":      func(c *config.Config) { c.ComposeScript = "" },
	} {
		cfg := projectCfg()
		mut(&cfg)
		if slices.Contains(SupportedTasks(cfg), ComposeProjectTask) {
			t.Errorf("%s: compose-project advertised", name)
		}
		if _, _, err := BuildRequest(context.Background(), cfg, true, ComposeProjectTask, projectPayload(nil, map[string]string{"index.html": "x"})); err == nil {
			t.Errorf("%s: compose-project admitted", name)
		}
	}
}

func TestComposeProjectDoorChecksTheBearerBeforeReadingTheBody(t *testing.T) {
	cfg := projectCfg()
	cfg.FleetComposeBundleMaxMB = 1
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	huge := `{"job_id":"x","bundle":"` + strings.Repeat("A", 3<<20) + `"}`
	for name, hdr := range map[string]map[string]string{
		"no bearer":    nil,
		"wrong bearer": {"Authorization": "Bearer nope"},
	} {
		if rec := do(t, s, "POST", ComposeProjectPath, huge, hdr); rec.Code != 401 {
			t.Errorf("%s with an oversized body: status %d, want 401 before the body is read", name, rec.Code)
		}
	}
	if rec := do(t, s, "POST", ComposeProjectPath, huge, map[string]string{"Authorization": "Bearer tok"}); rec.Code != 413 {
		t.Errorf("an authorized oversized body: status %d, want 413", rec.Code)
	}
	for name, mut := range map[string]func(*config.Config){
		"not opted in": func(c *config.Config) { c.FleetComposeProjects = false },
		"tokenless":    func(c *config.Config) { c.FleetAuthToken = "" },
	} {
		c := projectCfg()
		mut(&c)
		closed, _ := newTestServer(t, c, &fakeRunner{}, nil)
		if rec := do(t, closed, "POST", ComposeProjectPath, string(projectPayload(nil, map[string]string{"index.html": "x"})), map[string]string{"Authorization": "Bearer tok"}); rec.Code != 403 {
			t.Errorf("%s: status %d, want 403 (the door is closed)", name, rec.Code)
		}
	}
}

// dirRunner records whether the project directory existed while the job ran.
type dirRunner struct {
	mu      sync.Mutex
	dir     string
	present bool
}

func (d *dirRunner) Run(_ context.Context, req core.Request) core.Result {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dir, _ = req.Params["project_dir"].(string)
	_, err := os.Stat(filepath.Join(d.dir, "index.html"))
	d.present = err == nil && req.Task == core.TaskComposeVideo
	return core.Result{OK: true, Data: json.RawMessage(`{"video_path":"compose-abc.mp4"}`)}
}

func TestComposeProjectRendersTheExtractedProjectAndRemovesIt(t *testing.T) {
	r := &dirRunner{}
	s, jobs := newTestServer(t, projectCfg(), r, nil)
	rec := do(t, s, "POST", ComposeProjectPath, string(projectPayload(func(p *ComposeProjectPayload) { p.JobID = "proj-ok" }, map[string]string{
		"index.html": `<link href="css/a.css" rel="stylesheet"><img src="img/x.png">`, "css/a.css": `b{background:url(../img/x.png)}`, "img/x.png": "PNG",
	})), map[string]string{"Authorization": "Bearer tok"})
	if rec.Code != 202 {
		t.Fatalf("status %d, want 202: %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if v, ok := jobs.Get("proj-ok"); ok && Terminal(v.State) {
			if v.State != JobDone {
				t.Fatalf("job ended %s: %s", v.State, v.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the job never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.present || !strings.HasPrefix(r.dir, filepath.Join(composeProjectTestCache, "fleet-projects")) {
		t.Fatalf("the pipeline must render the extracted project under the compose cache, got %q (present=%v)", r.dir, r.present)
	}
	if _, err := os.Stat(r.dir); !os.IsNotExist(err) {
		t.Fatalf("the project directory must be removed when the job ends: %v", err)
	}
}

func TestComposeProjectRefusesBadPayloadsAndLeavesNothing(t *testing.T) {
	ok := map[string]string{"index.html": "<p>x</p>"}
	cases := map[string][]byte{
		"sha mismatch":  projectPayload(func(p *ComposeProjectPayload) { p.BundleSHA256 = strings.Repeat("0", 64) }, ok),
		"not base64":    projectPayload(func(p *ComposeProjectPayload) { p.Bundle = "!!!" }, ok),
		"png-sequence":  projectPayload(func(p *ComposeProjectPayload) { p.Format = "png-sequence" }, ok),
		"missing entry": projectPayload(func(p *ComposeProjectPayload) { p.Composition = "nope.html" }, ok),
		"entry outside": projectPayload(func(p *ComposeProjectPayload) { p.Composition = "../index.html" }, ok),
		"ref outside":   projectPayload(nil, map[string]string{"index.html": `<img src="../../secret.png">`}),
		"tailnet fetch": projectPayload(nil, map[string]string{"index.html": `<script src="http://some-box:18791/x.js"></script>`}),
		"unsafe member": payloadFor(rawTarGz(t, "../evil.html", "x"), nil),
		"over size cap": projectPayload(nil, map[string]string{"index.html": "x", "noise.bin": incompressible(2 << 20)}),
	}
	before := leftoverProjectDirs(t)
	for name, payload := range cases {
		cfg := projectCfg()
		cfg.FleetComposeBundleMaxMB = 1
		if _, cleanup, err := BuildRequest(context.Background(), cfg, true, ComposeProjectTask, payload); err == nil {
			cleanup()
			t.Errorf("%s: accepted", name)
		}
	}
	if after := leftoverProjectDirs(t); len(after) != len(before) {
		t.Fatalf("a refused bundle must leave no directory behind: before %v, after %v", before, after)
	}
	s, _ := newTestServer(t, projectCfg(), &fakeRunner{}, nil)
	rec := do(t, s, "POST", ComposeProjectPath, string(cases["ref outside"]), map[string]string{"Authorization": "Bearer tok"})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "refers outside itself") {
		t.Fatalf("over HTTP an unsafe project is a 400 naming the reason, got %d %s", rec.Code, rec.Body.String())
	}
}
