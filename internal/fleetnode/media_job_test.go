package fleetnode

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
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

// Magic-byte fixtures: the first bytes a real file of each format starts with, padded so the sniff has
// its whole 16-byte window.
var (
	mjPNG  = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	mjJPEG = append([]byte("\xff\xd8\xff\xe0"), make([]byte, 32)...)
	mjWEBP = append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), make([]byte, 32)...)
	mjWAV  = append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, 32)...)
	mjMP4  = append([]byte("\x00\x00\x00\x18ftypmp42"), make([]byte, 32)...)
	mjWEBM = append([]byte("\x1a\x45\xdf\xa3"), make([]byte, 32)...)
	mjFLAC = append([]byte("fLaC"), make([]byte, 32)...)
	mjMP3  = append([]byte("ID3\x04\x00"), make([]byte, 32)...)
	mjMP3Frame  = append([]byte("\xff\xfb\x90\x00"), make([]byte, 32)...)
	mjOGG  = append([]byte("OggS\x00\x02"), make([]byte, 32)...)
	mjText = []byte("this is not media at all, only text padding")
)

// mediaJobCfg is a node with every media task bound (the test seam reads a bound route as CONFIGURED), the
// door open, and a media dir of its own.
func mediaJobCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Config{
		ImageGenScript: "render/comfy-generate.mjs", VideoGenScript: "render/comfy-video.mjs",
		AnimateGenScript: "render/comfy-animate.mjs", VoiceGenScript: "render/tts.mjs",
		RunGraphScript:   "render/comfy-run-graph.mjs",
		FleetMediaInputs: true, FleetAuthToken: "tok", MediaDir: t.TempDir(),
	}
	return cfg
}

// packFiles packs files with the real packer.
func packFiles(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := composebundle.Pack(dir, composebundle.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// mediaPayload builds a media-job body; mut edits the typed payload before it is encoded.
func mediaPayload(task, inner string, bundle []byte, inputs map[string]string, mut func(*MediaJobPayload)) []byte {
	p := MediaJobPayload{JobID: "mj-1", TaskType: task, Payload: json.RawMessage(inner), Inputs: inputs}
	if bundle != nil {
		sum := sha256.Sum256(bundle)
		p.Bundle = base64.StdEncoding.EncodeToString(bundle)
		p.BundleSHA256 = hex.EncodeToString(sum[:])
	}
	if mut != nil {
		mut(&p)
	}
	raw, _ := json.Marshal(p)
	return raw
}

// rawTar builds a bundle from raw entries (a symlink, a traversal name) the packer would never write.
func rawTar(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		h := h
		body := ""
		if h.Typeflag == tar.TypeReg {
			body = "x"
			h.Size = 1
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			_, _ = tw.Write([]byte(body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func leftoverInputDirs(t *testing.T, cfg config.Config) []string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(cfg.MediaDir, mediaInputsDir))
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func bearer() map[string]string { return map[string]string{"Authorization": "Bearer tok"} }

func TestMediaJobIsOpenOnlyWithOptInTokenAndATask(t *testing.T) {
	cfg := mediaJobCfg(t)
	if !slices.Contains(SupportedTasks(cfg), MediaJobTask) {
		t.Fatalf("an opted-in node with a token and media tasks must advertise media-job: %v", SupportedTasks(cfg))
	}
	body := mediaPayload("image-gen", `{"prompt":"p"}`, nil, nil, nil)
	for name, mut := range map[string]func(*config.Config){
		"not opted in": func(c *config.Config) { c.FleetMediaInputs = false },
		"no token":     func(c *config.Config) { c.FleetAuthToken = "" },
		"no media task": func(c *config.Config) {
			c.ImageGenScript, c.VideoGenScript, c.AnimateGenScript, c.VoiceGenScript, c.RunGraphScript = "", "", "", "", ""
		},
	} {
		c := mediaJobCfg(t)
		mut(&c)
		if slices.Contains(SupportedTasks(c), MediaJobTask) {
			t.Errorf("%s: media-job advertised", name)
		}
		if _, cleanup, err := BuildRequest(context.Background(), c, true, MediaJobTask, body); err == nil {
			cleanup()
			t.Errorf("%s: media-job admitted", name)
		}
	}
}

// The door answers before it reads: closed is 403 and an unauthorized caller is 401 whatever it sends,
// so an unauthenticated peer cannot make the node buffer a body.
func TestMediaJobDoorChecksTheDoorAndTheBearerBeforeReadingTheBody(t *testing.T) {
	cfg := mediaJobCfg(t)
	cfg.FleetMediaInputsMaxMB = 1
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	huge := `{"job_id":"x","task_type":"video-gen","bundle":"` + strings.Repeat("A", 3<<20) + `"}`
	for name, hdr := range map[string]map[string]string{
		"no bearer":    nil,
		"wrong bearer": {"Authorization": "Bearer nope"},
	} {
		if rec := do(t, s, "POST", MediaJobPath, huge, hdr); rec.Code != 401 {
			t.Errorf("%s with an oversized body: status %d, want 401 before the body is read", name, rec.Code)
		}
	}
	if rec := do(t, s, "POST", MediaJobPath, huge, bearer()); rec.Code != 413 {
		t.Errorf("an authorized oversized body: status %d, want 413", rec.Code)
	}
	for name, mut := range map[string]func(*config.Config){
		"not opted in": func(c *config.Config) { c.FleetMediaInputs = false },
		"tokenless":    func(c *config.Config) { c.FleetAuthToken = "" },
	} {
		c := mediaJobCfg(t)
		mut(&c)
		closed, _ := newTestServer(t, c, &fakeRunner{}, nil)
		if rec := do(t, closed, "POST", MediaJobPath, huge, bearer()); rec.Code != 403 {
			t.Errorf("%s: status %d, want 403 (the door is closed)", name, rec.Code)
		}
	}
	// Through the shared dispatch door a media-job is token-gated too: no bearer, no admission.
	open, _ := newTestServer(t, mediaJobCfg(t), &fakeRunner{}, nil)
	env := `{"job_id":"mj-d","task_type":"media-job","payload":` + string(mediaPayload("image-gen", `{"prompt":"p"}`, nil, nil, nil)) + `}`
	if rec := do(t, open, "POST", "/fleet/dispatch", env, nil); rec.Code != 401 {
		t.Errorf("media-job over /fleet/dispatch without the bearer: status %d, want 401", rec.Code)
	}
}

func TestMediaJobRefusesBadPayloadsAndLeavesNothing(t *testing.T) {
	still := map[string][]byte{"still.png": mjPNG}
	video := `{"prompt":"p"}`
	cases := map[string][]byte{
		"sha mismatch": mediaPayload("video-gen", video, packFiles(t, still), map[string]string{"still": "still.png"},
			func(p *MediaJobPayload) { p.BundleSHA256 = strings.Repeat("0", 64) }),
		"not base64": mediaPayload("video-gen", video, packFiles(t, still), map[string]string{"still": "still.png"},
			func(p *MediaJobPayload) { p.Bundle = "!!!" }),
		"traversal name": mediaPayload("video-gen", video, packFiles(t, still), map[string]string{"still": "../still.png"}, nil),
		"nested name":    mediaPayload("video-gen", video, packFiles(t, still), map[string]string{"still": "a/still.png"}, nil),
		"absolute name":  mediaPayload("video-gen", video, packFiles(t, still), map[string]string{"still": "/etc/passwd"}, nil),
		"drive name":     mediaPayload("video-gen", video, packFiles(t, still), map[string]string{"still": "C:still.png"}, nil),
		"symlink member": mediaPayload("video-gen", video, rawTar(t, tar.Header{Name: "still.png", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o644}),
			map[string]string{"still": "still.png"}, nil),
		"traversal member": mediaPayload("video-gen", video, rawTar(t, tar.Header{Name: "../evil.png", Typeflag: tar.TypeReg, Mode: 0o644}),
			map[string]string{"still": "evil.png"}, nil),
		"file not in bundle": mediaPayload("video-gen", video, packFiles(t, still), map[string]string{"still": "other.png"}, nil),
		"wrong magic for the field": mediaPayload("video-gen", video, packFiles(t, map[string][]byte{"still.png": mjText}),
			map[string]string{"still": "still.png"}, nil),
		"a video where an image goes": mediaPayload("video-gen", video, packFiles(t, map[string][]byte{"still.png": mjMP4}),
			map[string]string{"still": "still.png"}, nil),
		"a wav where an image goes": mediaPayload("video-gen", video, packFiles(t, map[string][]byte{"still.png": mjWAV}),
			map[string]string{"still": "still.png"}, nil),
		"an image where a driver video goes": mediaPayload("animate", `{"prompt":"p"}`, packFiles(t, map[string][]byte{"ref.png": mjPNG, "driver.mp4": mjPNG}),
			map[string]string{"ref": "ref.png", "driver": "driver.mp4"}, nil),
		"a png where a clone sample goes": mediaPayload("audio-gen", `{"text":"hola"}`, packFiles(t, map[string][]byte{"clone.wav": mjPNG}),
			map[string]string{"clone": "clone.wav"}, nil),
		"unknown input field": mediaPayload("video-gen", video, packFiles(t, still), map[string]string{"bogus": "still.png"}, nil),
		"field not allowed for the task": mediaPayload("image-gen", `{"prompt":"p"}`, packFiles(t, still),
			map[string]string{"still": "still.png"}, nil),
		"run-graph takes no input file": mediaPayload("run-graph", `{"graph":{"1":{"class_type":"X"}}}`, packFiles(t, still),
			map[string]string{"still": "still.png"}, nil),
		"a node-local path in a file field": mediaPayload("video-gen", `{"prompt":"p","still":"/etc/passwd"}`, nil, nil, nil),
		"inputs without a bundle":           mediaPayload("video-gen", video, nil, map[string]string{"still": "still.png"}, nil),
		"a bundle without inputs":           mediaPayload("video-gen", video, packFiles(t, still), nil, nil),
		"task not carried by the door":      mediaPayload("compose-video", `{"template":"title-card"}`, nil, nil, nil),
		"stt is not carried":                mediaPayload("stt", `{"audio":"a.wav"}`, nil, nil, nil),
		"the door does not nest":            mediaPayload("media-job", `{}`, nil, nil, nil),
		"inner payload invalid":             mediaPayload("video-gen", `{}`, packFiles(t, still), map[string]string{"still": "still.png"}, nil),
		"inner payload not an object":       mediaPayload("video-gen", `[1]`, nil, nil, nil),
	}
	cfg := mediaJobCfg(t)
	for name, payload := range cases {
		if _, cleanup, err := BuildRequest(context.Background(), cfg, true, MediaJobTask, payload); err == nil {
			cleanup()
			t.Errorf("%s: accepted", name)
		}
	}
	if left := leftoverInputDirs(t, cfg); len(left) != 0 {
		t.Fatalf("a refused media job must leave no directory behind: %v", left)
	}
	// An inner task this node does not run is refused too, by the same predicate dispatch uses.
	noAnimate := mediaJobCfg(t)
	noAnimate.AnimateGenScript = ""
	if _, cleanup, err := BuildRequest(context.Background(), noAnimate, true, MediaJobTask, mediaPayload("animate", `{"prompt":"p"}`,
		packFiles(t, map[string][]byte{"ref.png": mjPNG, "driver.mp4": mjMP4}), map[string]string{"ref": "ref.png", "driver": "driver.mp4"}, nil)); err == nil {
		cleanup()
		t.Error("animate admitted on a node with no animate route")
	}
	// Over HTTP an unsafe payload is a 400 naming the reason, and an unknown top-level key is a 400 too.
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	rec := do(t, s, "POST", MediaJobPath, string(cases["wrong magic for the field"]), bearer())
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "not an image") {
		t.Fatalf("a wrong-magic input over HTTP is a 400 naming the reason, got %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, s, "POST", MediaJobPath, `{"job_id":"x","task_type":"image-gen","bogus":1}`, bearer())
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "malformed media-job body") {
		t.Fatalf("an unknown top-level field must be refused by the strict decoder, got %d %s", rec.Code, rec.Body.String())
	}
	if left := leftoverInputDirs(t, cfg); len(left) != 0 {
		t.Fatalf("refusals over HTTP left directories behind: %v", left)
	}
}

// Over the cap the bundle is refused (413 from the body cap, or a 400 from the builder when the body
// fits but the bundle does not): neither leaves a directory.
func TestMediaJobRefusesAnOversizeBundle(t *testing.T) {
	cfg := mediaJobCfg(t)
	cfg.FleetMediaInputsMaxMB = 1
	big := append(append([]byte{}, mjPNG...), []byte(incompressible(2<<20))...)
	payload := mediaPayload("video-gen", `{"prompt":"p"}`, packFiles(t, map[string][]byte{"still.png": big}), map[string]string{"still": "still.png"}, nil)
	if _, cleanup, err := BuildRequest(context.Background(), cfg, true, MediaJobTask, payload); err == nil {
		cleanup()
		t.Fatal("a bundle over fleet_media_inputs_max_mb was accepted")
	} else if !strings.Contains(err.Error(), "fleet_media_inputs_max_mb") {
		t.Fatalf("the refusal must name the cap: %v", err)
	}
	if left := leftoverInputDirs(t, cfg); len(left) != 0 {
		t.Fatalf("directories left behind: %v", left)
	}
}

// inputRunner records, while the job runs, the paths the pipeline was handed and whether the files exist.
type inputRunner struct {
	mu      sync.Mutex
	req     core.Request
	content map[string][]byte
}

func (r *inputRunner) Run(_ context.Context, req core.Request) core.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.req = req
	r.content = map[string][]byte{}
	for _, k := range []string{"still", "ref", "driver", "clone"} {
		if p, ok := req.Params[k].(string); ok {
			b, _ := os.ReadFile(p)
			r.content[k] = b
		}
	}
	return core.Result{OK: true, Data: json.RawMessage(`{"video_path":"clip.mp4"}`)}
}

func waitDone(t *testing.T, jobs *Jobs, id string) *JobView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if v, ok := jobs.Get(id); ok && Terminal(v.State) {
			if v.State != JobDone {
				t.Fatalf("job %s ended %s: %s", id, v.State, v.Error)
			}
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never finished", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMediaJobRewritesTheInputFieldsAndRemovesTheDirectory(t *testing.T) {
	for _, tc := range []struct {
		name, task, inner string
		files             map[string][]byte
		inputs            map[string]string
		wantTask          core.TaskType
		fields            []string
	}{
		{"video still", "video-gen", `{"prompt":"p"}`, map[string][]byte{"s.webp": mjWEBP}, map[string]string{"still": "s.webp"}, core.TaskGenerateVideo, []string{"still"}},
		{"animate ref and driver", "animate", `{"prompt":"p"}`, map[string][]byte{"r.jpg": mjJPEG, "d.webm": mjWEBM},
			map[string]string{"ref": "r.jpg", "driver": "d.webm"}, core.TaskAnimateCharacter, []string{"ref", "driver"}},
		{"audio clone", "audio-gen", `{"text":"hola"}`, map[string][]byte{"c.wav": mjWAV}, map[string]string{"clone": "c.wav"}, core.TaskGenerateAudio, []string{"clone"}},
		{"audio clone flac", "audio-gen", `{"text":"hola"}`, map[string][]byte{"c.flac": mjFLAC}, map[string]string{"clone": "c.flac"}, core.TaskGenerateAudio, []string{"clone"}},
		{"audio clone mp3", "audio-gen", `{"text":"hola"}`, map[string][]byte{"c.mp3": mjMP3}, map[string]string{"clone": "c.mp3"}, core.TaskGenerateAudio, []string{"clone"}},
		{"audio clone mp3 frame", "audio-gen", `{"text":"hola"}`, map[string][]byte{"c.mp3": mjMP3Frame}, map[string]string{"clone": "c.mp3"}, core.TaskGenerateAudio, []string{"clone"}},
		{"audio clone ogg", "audio-gen", `{"text":"hola"}`, map[string][]byte{"c.ogg": mjOGG}, map[string]string{"clone": "c.ogg"}, core.TaskGenerateAudio, []string{"clone"}},
		{"audio clone m4a", "audio-gen", `{"text":"hola"}`, map[string][]byte{"c.m4a": mjMP4}, map[string]string{"clone": "c.m4a"}, core.TaskGenerateAudio, []string{"clone"}},
		{"video still png", "video-gen", `{"prompt":"p"}`, map[string][]byte{"s.png": mjPNG}, map[string]string{"still": "s.png"}, core.TaskGenerateVideo, []string{"still"}},
		{"animate mov driver", "animate", `{"prompt":"p"}`, map[string][]byte{"r.png": mjPNG, "d.mov": mjMP4},
			map[string]string{"ref": "r.png", "driver": "d.mov"}, core.TaskAnimateCharacter, []string{"ref", "driver"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mediaJobCfg(t)
			r := &inputRunner{}
			s, jobs := newTestServer(t, cfg, r, nil)
			id := "mj-" + strings.ReplaceAll(tc.name, " ", "-")
			rec := do(t, s, "POST", MediaJobPath, string(mediaPayload(tc.task, tc.inner, packFiles(t, tc.files), tc.inputs,
				func(p *MediaJobPayload) { p.JobID = id })), bearer())
			if rec.Code != 202 {
				t.Fatalf("status %d, want 202: %s", rec.Code, rec.Body.String())
			}
			waitDone(t, jobs, id)
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.req.Task != tc.wantTask {
				t.Fatalf("the pipeline ran %q, want %q", r.req.Task, tc.wantTask)
			}
			root := filepath.Join(cfg.MediaDir, mediaInputsDir)
			for _, f := range tc.fields {
				p, _ := r.req.Params[f].(string)
				if !filepath.IsAbs(p) || filepath.Dir(filepath.Dir(p)) != root || !strings.HasPrefix(filepath.Base(filepath.Dir(p)), "in-") {
					t.Fatalf("%s must be rewritten to an absolute path under %s/in-*, got %q", f, root, p)
				}
				if want := tc.files[tc.inputs[f]]; !bytes.Equal(r.content[f], want) {
					t.Fatalf("%s: the pipeline read %d bytes, want the %d bytes sent", f, len(r.content[f]), len(want))
				}
				if _, err := os.Stat(filepath.Dir(p)); !os.IsNotExist(err) {
					t.Fatalf("the input directory must be removed when the job ends: %v", err)
				}
			}
			if left := leftoverInputDirs(t, cfg); len(left) != 0 {
				t.Fatalf("directories left after the job: %v", left)
			}
		})
	}
}

// An animate request names its two files through the same fields the node's builder reads, so the request
// the pipeline sees carries Image and Video too.
func TestMediaJobAnimateRequestCarriesImageAndVideo(t *testing.T) {
	cfg := mediaJobCfg(t)
	req, cleanup, err := BuildRequest(context.Background(), cfg, true, MediaJobTask, mediaPayload("animate", `{"prompt":"p"}`,
		packFiles(t, map[string][]byte{"r.png": mjPNG, "d.mp4": mjMP4}), map[string]string{"ref": "r.png", "driver": "d.mp4"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if req.Image == "" || req.Video == "" || req.Image != req.Params["ref"] || req.Video != req.Params["driver"] {
		t.Fatalf("image %q video %q params %v", req.Image, req.Video, req.Params)
	}
}

// With no input file the door still carries a task (the thin client uses /fleet/dispatch for that, but the
// door must not demand a bundle): run-graph and image-gen go straight through.
func TestMediaJobWithoutInputsRunsTheInnerTask(t *testing.T) {
	cfg := mediaJobCfg(t)
	for task, inner := range map[string]string{"image-gen": `{"prompt":"p"}`, "run-graph": `{"graph":{"1":{"class_type":"X"}}}`} {
		req, cleanup, err := BuildRequest(context.Background(), cfg, true, MediaJobTask, mediaPayload(task, inner, nil, nil, nil))
		if err != nil {
			t.Fatalf("%s: %v", task, err)
		}
		cleanup()
		if req.Task == "" {
			t.Fatalf("%s: no request", task)
		}
	}
	if left := leftoverInputDirs(t, cfg); len(left) != 0 {
		t.Fatalf("no bundle, no directory: %v", left)
	}
}

// A media-job's state, error text and output names belong to the token holder that sent it, like a project's.
func TestMediaJobJobsAreMaskedWithoutTheBearer(t *testing.T) {
	cfg := mediaJobCfg(t)
	s, jobs := newTestServer(t, cfg, &inputRunner{}, nil)
	rec := do(t, s, "POST", MediaJobPath, string(mediaPayload("video-gen", `{"prompt":"p"}`, packFiles(t, map[string][]byte{"s.png": mjPNG}),
		map[string]string{"still": "s.png"}, func(p *MediaJobPayload) { p.JobID = "mj-masked" })), bearer())
	if rec.Code != 202 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if v, ok := jobs.Get("mj-masked"); !ok || !v.Gated {
		t.Fatalf("a media-job must be gated: %+v", v)
	}
	if rec := do(t, s, "GET", "/fleet/jobs/mj-masked", "", nil); rec.Code != 401 {
		t.Errorf("a poll without the bearer: status %d, want 401", rec.Code)
	}
	if rec := do(t, s, "GET", "/fleet/jobs/mj-masked", "", bearer()); rec.Code != 200 {
		t.Errorf("a poll with the bearer: status %d: %s", rec.Code, rec.Body.String())
	}
	if got := s.claimSpec(MediaJobTask, func() {}); !got.Gated || !got.Uncapped {
		t.Errorf("a pulled media-job must be gated and uncapped like a dispatched one: %+v", got)
	}
	// The five inner media tasks stay tokenless over /fleet/dispatch.
	if s.claimSpec("video-gen", func() {}).Gated {
		t.Error("video-gen over /fleet/dispatch must stay tokenless")
	}
}

// The door gives a token holder time to upload; a request without the bearer never gets it.
func TestMediaJobGivesATokenHolderTimeToUpload(t *testing.T) {
	cfg := mediaJobCfg(t)
	s, _ := newTestServer(t, cfg, &inputRunner{}, nil)
	body := string(mediaPayload("video-gen", `{"prompt":"p"}`, packFiles(t, map[string][]byte{"s.png": mjPNG}),
		map[string]string{"still": "s.png"}, func(p *MediaJobPayload) { p.JobID = "mj-window" }))
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest("POST", MediaJobPath, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	s.handleMediaJob(rec, req)
	if rec.Code != 202 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if until := time.Until(rec.read); until < 10*time.Minute {
		t.Errorf("read deadline %v ahead, want the door's window", until)
	}
	if until := time.Until(rec.write); until < 10*time.Minute {
		t.Errorf("write deadline %v ahead, want the door's window", until)
	}
	anon := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.handleMediaJob(anon, httptest.NewRequest("POST", MediaJobPath, strings.NewReader(body)))
	if anon.Code != 401 || !anon.read.IsZero() {
		t.Errorf("without the bearer: status %d, read deadline %v; want 401 and the blanket timeout", anon.Code, anon.read)
	}
}

// A node that cannot unpack (here: its media dir is a file) answers 500, its own failure, not a refusal.
func TestMediaJobNodeSideFailureIsNotARefusal(t *testing.T) {
	cfg := mediaJobCfg(t)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.MediaDir = blocker
	s, _ := newTestServer(t, cfg, &inputRunner{}, nil)
	rec := do(t, s, "POST", MediaJobPath, string(mediaPayload("video-gen", `{"prompt":"p"}`, packFiles(t, map[string][]byte{"s.png": mjPNG}),
		map[string]string{"still": "s.png"}, func(p *MediaJobPayload) { p.JobID = "mj-disk" })), bearer())
	if rec.Code != 500 {
		t.Fatalf("status %d, want 500 for the node's own failure: %s", rec.Code, rec.Body.String())
	}
}

// A crash leaves extracted inputs behind; the startup sweep removes only the old in-* directories.
func TestSweepOrphanedInputDirs(t *testing.T) {
	cfg := config.Config{MediaDir: t.TempDir(), VideoGenTimeoutSec: 60, ImageGenTimeoutSec: 60, AnimateGenTimeoutSec: 60, AudioGenTimeoutSec: 60}
	base := filepath.Join(cfg.MediaDir, mediaInputsDir)
	for _, d := range []string{"in-old", "in-young", "keep-me"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, d, "still.png"), mjPNG, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	for _, d := range []string{"in-old", "keep-me"} {
		if err := os.Chtimes(filepath.Join(base, d), old, old); err != nil {
			t.Fatal(err)
		}
	}
	n, err := SweepOrphanedInputDirs(cfg, now)
	if err != nil || n != 1 {
		t.Fatalf("swept %d, %v; want 1", n, err)
	}
	for d, want := range map[string]bool{"in-old": false, "in-young": true, "keep-me": true} {
		if _, err := os.Stat(filepath.Join(base, d)); (err == nil) != want {
			t.Errorf("%s present=%v, want %v", d, err == nil, want)
		}
	}
	if n, err := SweepOrphanedInputDirs(config.Config{MediaDir: t.TempDir()}, now); n != 0 || err != nil {
		t.Errorf("no fleet-inputs dir: %d, %v", n, err)
	}
}

// The directory the inputs land in is a directory of media_dir, never a file the media route serves.
func TestMediaJobInputsAreNotServedByTheMediaRoute(t *testing.T) {
	cfg := mediaJobCfg(t)
	req, cleanup, err := BuildRequest(context.Background(), cfg, true, MediaJobTask, mediaPayload("video-gen", `{"prompt":"p"}`,
		packFiles(t, map[string][]byte{"s.png": mjPNG}), map[string]string{"still": "s.png"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	still, _ := req.Params["still"].(string)
	rel, _ := filepath.Rel(cfg.MediaDir, still)
	for _, name := range []string{mediaInputsDir, filepath.ToSlash(rel), "..%2f" + filepath.Base(still)} {
		if rec := do(t, s, "GET", "/fleet/media/"+name, "", nil); rec.Code == 200 {
			t.Errorf("GET /fleet/media/%s served an input file", name)
		}
	}
}
