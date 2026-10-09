package fleetnode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func artJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func artifactsOf(t *testing.T, data json.RawMessage) []Artifact {
	t.Helper()
	var m struct {
		Artifacts []Artifact `json:"artifacts"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("data %s: %v", data, err)
	}
	return m.Artifacts
}

func TestWithArtifactsListsEveryOutputInsideMediaDir(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{MediaDir: dir}
	clip, snap, aud := []byte("clip bytes"), []byte("png bytes"), []byte("wav bytes")
	for name, b := range map[string][]byte{"clip.mp4": clip, "snap.png": snap, "voice.wav": aud} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "elsewhere.mp4")
	if err := os.WriteFile(outside, []byte("not ours"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "deep.png"), []byte("deep"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("video result", func(t *testing.T) {
		data := artJSON(t, map[string]any{"video_path": filepath.Join(dir, "clip.mp4"), "seed": 9007199254740993})
		got := withArtifacts(cfg, "video-gen", data)
		a := artifactsOf(t, got)
		if len(a) != 1 || a[0].Name != "clip.mp4" || a[0].Bytes != int64(len(clip)) || a[0].SHA256 != sha(clip) {
			t.Fatalf("artifacts %+v", a)
		}
		if !bytes.Contains(got, []byte("9007199254740993")) {
			t.Fatalf("the rest of the result must be carried untouched (a large seed lost digits): %s", got)
		}
	})
	t.Run("a bare name is a file of media_dir", func(t *testing.T) {
		a := artifactsOf(t, withArtifacts(cfg, "audio-gen", artJSON(t, map[string]any{"audio_path": "voice.wav"})))
		if len(a) != 1 || a[0].Name != "voice.wav" || a[0].SHA256 != sha(aud) {
			t.Fatalf("artifacts %+v", a)
		}
	})
	t.Run("run-graph outputs and its image alias are listed once", func(t *testing.T) {
		data := artJSON(t, map[string]any{
			"image_path": filepath.Join(dir, "snap.png"),
			"outputs": map[string]any{
				"9":  []map[string]any{{"path": filepath.Join(dir, "snap.png"), "type": "output", "kind": "image"}},
				"12": []map[string]any{{"path": filepath.Join(dir, "clip.mp4"), "type": "output", "kind": "video"}},
			},
		})
		a := artifactsOf(t, withArtifacts(cfg, "run-graph", data))
		names := map[string]bool{}
		for _, x := range a {
			if names[x.Name] {
				t.Errorf("%s listed twice", x.Name)
			}
			names[x.Name] = true
		}
		if len(a) != 2 || !names["snap.png"] || !names["clip.mp4"] {
			t.Fatalf("artifacts %+v", a)
		}
	})
	t.Run("a path outside media_dir is not listed", func(t *testing.T) {
		data := artJSON(t, map[string]any{"video_path": outside})
		got := withArtifacts(cfg, "video-gen", data)
		if !bytes.Equal(got, data) {
			t.Fatalf("a result naming only a file outside media_dir must be stored untouched, got %s", got)
		}
		both := artJSON(t, map[string]any{"video_path": outside, "image_path": filepath.Join(dir, "snap.png")})
		a := artifactsOf(t, withArtifacts(cfg, "image-gen", both))
		if len(a) != 1 || a[0].Name != "snap.png" {
			t.Fatalf("only the file inside media_dir may be listed, got %+v", a)
		}
	})
	t.Run("a nested file, a directory, a traversal and a missing file are not listed", func(t *testing.T) {
		for _, p := range []string{filepath.Join(dir, "sub", "deep.png"), filepath.Join(dir, "sub"), dir,
			filepath.Join(dir, "..", filepath.Base(dir), "..", "x.mp4"), filepath.Join(dir, "gone.mp4"), "sub/deep.png"} {
			data := artJSON(t, map[string]any{"video_path": p})
			if got := withArtifacts(cfg, "video-gen", data); !bytes.Equal(got, data) {
				t.Errorf("%q was listed: %s", p, got)
			}
		}
	})
	t.Run("a symlink inside media_dir is not listed", func(t *testing.T) {
		link := filepath.Join(dir, "link.mp4")
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
		data := artJSON(t, map[string]any{"video_path": link})
		if got := withArtifacts(cfg, "video-gen", data); !bytes.Equal(got, data) {
			t.Errorf("a symlink was listed: %s", got)
		}
	})
	t.Run("not a media task, not an object, no media_dir", func(t *testing.T) {
		data := artJSON(t, map[string]any{"video_path": filepath.Join(dir, "clip.mp4")})
		if got := withArtifacts(cfg, "stt", data); !bytes.Equal(got, data) {
			t.Errorf("stt result changed: %s", got)
		}
		if got := withArtifacts(cfg, "video-gen", json.RawMessage(`"text"`)); string(got) != `"text"` {
			t.Errorf("a non-object result changed: %s", got)
		}
		if got := withArtifacts(config.Config{}, "video-gen", data); !bytes.Equal(got, data) {
			t.Errorf("no media_dir: result changed: %s", got)
		}
	})
}

// A hashing failure never fails the job: an unreadable output is left out and the rest still lands.
func TestWithArtifactsNeverFailsOnAnUnreadableOutput(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{MediaDir: dir}
	good := filepath.Join(dir, "good.png")
	if err := os.WriteFile(good, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	data := artJSON(t, map[string]any{"image_path": good, "video_path": filepath.Join(dir, "missing.mp4")})
	a := artifactsOf(t, withArtifacts(cfg, "image-gen", data))
	if len(a) != 1 || a[0].Name != "good.png" {
		t.Fatalf("artifacts %+v", a)
	}
}

// End to end through the job store: a finished job's polled data carries the artifacts, over both the
// tokenless dispatch door and the media-job door.
func TestFinishedMediaJobDataCarriesArtifacts(t *testing.T) {
	cfg := mediaJobCfg(t)
	clip := []byte("rendered clip bytes")
	runner := &fakeRunner{fn: func(_ context.Context, req core.Request) core.Result {
		p := filepath.Join(cfg.MediaDir, "video-abc.mp4")
		_ = os.WriteFile(p, clip, 0o644)
		return core.Result{OK: true, Data: artJSON(t, map[string]any{"video_path": p, "seed": 5})}
	}}
	s, jobs := newTestServer(t, cfg, runner, nil)

	rec := do(t, s, "POST", "/fleet/dispatch", `{"job_id":"art-1","task_type":"video-gen","payload":{"prompt":"p"}}`, nil)
	if rec.Code != 202 {
		t.Fatalf("dispatch status %d: %s", rec.Code, rec.Body.String())
	}
	v := waitDone(t, jobs, "art-1")
	a := artifactsOf(t, v.Data)
	if len(a) != 1 || a[0].Name != "video-abc.mp4" || a[0].Bytes != int64(len(clip)) || a[0].SHA256 != sha(clip) {
		t.Fatalf("dispatch: artifacts %+v in %s", a, v.Data)
	}

	rec = do(t, s, "POST", MediaJobPath, string(mediaPayload("video-gen", `{"prompt":"p"}`, packFiles(t, map[string][]byte{"s.png": mjPNG}),
		map[string]string{"still": "s.png"}, func(p *MediaJobPayload) { p.JobID = "art-2" })), bearer())
	if rec.Code != 202 {
		t.Fatalf("media-job status %d: %s", rec.Code, rec.Body.String())
	}
	v = waitDone(t, jobs, "art-2")
	a = artifactsOf(t, v.Data)
	if len(a) != 1 || a[0].SHA256 != sha(clip) {
		t.Fatalf("media-job: artifacts %+v in %s", a, v.Data)
	}
	// The polled wire carries it too.
	rec = do(t, s, "GET", "/fleet/jobs/art-2", "", bearer())
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte(`"artifacts"`)) {
		t.Fatalf("poll: %d %s", rec.Code, rec.Body.String())
	}
}
