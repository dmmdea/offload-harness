package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The outputs of a media-job are rendered from the caller's private input files (a still, a driver video, a
// voice sample), so they are served like a project render: only to a holder of the fleet token (ADR 0077).
// The outputs of the tokenless /fleet/dispatch door keep their bare-name reads.

// outRunner writes the file a render of each task would at the path it is given in `out`, and under the
// pipeline's own name (<task>-<hash8>.<ext>) when it is given none: exactly the two situations the node
// has. It reports the path the way the pipeline does.
type outRunner struct {
	media string // the node's media dir, for a render that is given no out
	mu    sync.Mutex
	reqs  []core.Request
}

func (r *outRunner) Run(_ context.Context, req core.Request) core.Result {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	out, _ := req.Params["out"].(string)
	key, ext := "image_path", "png"
	switch req.Task {
	case core.TaskGenerateVideo, core.TaskAnimateCharacter:
		key, ext = "video_path", "mp4"
	case core.TaskGenerateAudio:
		key, ext = "audio_path", "wav"
	}
	if out == "" {
		prefix := map[core.TaskType]string{core.TaskGenerateImage: "render", core.TaskGenerateVideo: "video", core.TaskAnimateCharacter: "animate", core.TaskGenerateAudio: "voice"}[req.Task]
		out = filepath.Join(r.media, prefix+"-0a1b2c3d."+ext)
	}
	if err := os.WriteFile(out, []byte("rendered:"+filepath.Base(out)), 0o644); err != nil {
		return core.Deferf("write: "+err.Error(), "", core.Meta{})
	}
	raw, _ := json.Marshal(map[string]any{key: out})
	return core.Result{OK: true, Data: raw}
}

// Each task the door carries a file for (and image-gen) is told an output path of the door's own choosing,
// directly under media_dir, whose name the gate recognises; run-graph, whose outputs the graph names, is not.
func TestMediaJobRendersUnderAGatedStem(t *testing.T) {
	cfg := mediaJobCfg(t)
	for _, tc := range []struct {
		name, task, inner string
		files             map[string][]byte
		inputs            map[string]string
		ext               string
	}{
		{"video", "video-gen", `{"prompt":"p"}`, map[string][]byte{"s.png": mjPNG}, map[string]string{"still": "s.png"}, "mp4"},
		{"animate", "animate", `{"prompt":"p"}`, map[string][]byte{"r.png": mjPNG, "d.mp4": mjMP4}, map[string]string{"ref": "r.png", "driver": "d.mp4"}, "mp4"},
		{"voice clone", "audio-gen", `{"text":"hola"}`, map[string][]byte{"c.wav": mjWAV}, map[string]string{"clone": "c.wav"}, "wav"},
		{"music", "audio-gen", `{"text":"x","kind":"music"}`, nil, nil, "flac"},
		{"image without files", "image-gen", `{"prompt":"p"}`, nil, nil, "png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bundle []byte
			if tc.files != nil {
				bundle = packFiles(t, tc.files)
			}
			req, cleanup, err := BuildRequest(context.Background(), cfg, true, MediaJobTask, mediaPayload(tc.task, tc.inner, bundle, tc.inputs, nil))
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			out, _ := req.Params["out"].(string)
			if out == "" || filepath.Dir(out) != cfg.MediaDir {
				t.Fatalf("out = %q, want a file directly under media_dir %q", out, cfg.MediaDir)
			}
			base := filepath.Base(out)
			if !mediaJobOutputRe.MatchString(base) || !strings.HasPrefix(base, mediaJobOutputPrefix) || filepath.Ext(base) != "."+tc.ext {
				t.Errorf("out name %q, want mediajob-<16 hex>.%s", base, tc.ext)
			}
			if !gatedMediaName(base) {
				t.Errorf("%q is not a gated media name: GET /fleet/media would serve it tokenless", base)
			}
			again, cleanup2, err := BuildRequest(context.Background(), cfg, true, MediaJobTask, mediaPayload(tc.task, tc.inner, bundle, tc.inputs, nil))
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup2()
			if again.Params["out"] == out {
				t.Errorf("two jobs were given the same output %q", out)
			}
		})
	}
	req, cleanup, err := BuildRequest(context.Background(), cfg, true, MediaJobTask, mediaPayload("run-graph", `{"graph":{"1":{"class_type":"X"}}}`, nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, has := req.Params["out"]; has {
		t.Errorf("run-graph is told an out (%v): its outputs are the graph's to name", req.Params["out"])
	}
	// A caller cannot choose the path: the payload's own `out` is dropped by the builders and replaced.
	req, cleanup3, err := BuildRequest(context.Background(), cfg, true, MediaJobTask, mediaPayload("image-gen", `{"prompt":"p","out":"/tmp/chosen.png"}`, nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup3()
	if out, _ := req.Params["out"].(string); !strings.HasPrefix(filepath.Base(out), mediaJobOutputPrefix) {
		t.Errorf("a caller-named out reached the pipeline: %q", out)
	}
}

// The end-to-end privacy rule: the output of a media-job is refused to a tokenless fetch and served with the
// token; the output of a plain dispatch is still read by bare name.
func TestMediaJobOutputNeedsTheBearerAPlainDispatchOutputDoesNot(t *testing.T) {
	cfg := mediaJobCfg(t)
	s, jobs := newTestServer(t, cfg, &outRunner{media: cfg.MediaDir}, nil)

	rec := do(t, s, "POST", MediaJobPath, string(mediaPayload("video-gen", `{"prompt":"p"}`, packFiles(t, map[string][]byte{"s.png": mjPNG}),
		map[string]string{"still": "s.png"}, func(p *MediaJobPayload) { p.JobID = "mj-private" })), bearer())
	if rec.Code != http.StatusAccepted {
		t.Fatalf("media-job status %d: %s", rec.Code, rec.Body.String())
	}
	v := waitDone(t, jobs, "mj-private")
	var data struct {
		VideoPath string `json:"video_path"`
		Artifacts []struct {
			Name string `json:"name"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(v.Data, &data); err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(data.VideoPath)
	if !strings.HasPrefix(name, mediaJobOutputPrefix) || len(data.Artifacts) != 1 || data.Artifacts[0].Name != name {
		t.Fatalf("the job's result names %q with artifacts %+v, want the gated output published as an artifact", data.VideoPath, data.Artifacts)
	}
	if rec := do(t, s, http.MethodGet, "/fleet/media/"+name, "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("tokenless GET of a media-job output = %d, want 401", rec.Code)
	}
	if rec := do(t, s, http.MethodGet, "/fleet/media/"+name, "", map[string]string{"Authorization": "Bearer wrong"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET of a media-job output with a wrong bearer = %d, want 401", rec.Code)
	}
	if rec := do(t, s, http.MethodGet, "/fleet/media/"+name, "", bearer()); rec.Code != http.StatusOK || rec.Body.String() != "rendered:"+name {
		t.Errorf("GET of a media-job output with the bearer = %d (%q)", rec.Code, rec.Body.String())
	}

	// A plain dispatch on the same node: the tokenless door, the pipeline's own name, read by bare name.
	env := `{"job_id":"d-plain","task_type":"image-gen","payload":{"prompt":"p"}}`
	if rec := do(t, s, "POST", "/fleet/dispatch", env, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch status %d: %s", rec.Code, rec.Body.String())
	}
	dv := waitDone(t, jobs, "d-plain")
	var plain struct {
		ImagePath string `json:"image_path"`
	}
	if err := json.Unmarshal(dv.Data, &plain); err != nil {
		t.Fatal(err)
	}
	pname := filepath.Base(plain.ImagePath)
	if strings.HasPrefix(pname, mediaJobOutputPrefix) {
		t.Fatalf("a plain dispatch output carries the media-job stem: %q", pname)
	}
	if rec := do(t, s, http.MethodGet, "/fleet/media/"+pname, "", nil); rec.Code != http.StatusOK {
		t.Errorf("tokenless GET of a plain dispatch output %q = %d, want 200", pname, rec.Code)
	}
}
