package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// Transcript hygiene for the stt upload door (ADR 0072 follow-up) and the bearer on the media of the
// token-gated lanes. Names below are the shapes the pipeline writes: an upload's private file is
// stt-<digits>.<ext>, its outputs stt-<digits>-<8 hex>.srt|txt|segments.json; a project render is
// composeproj-<16 hex>.<ext> with composeproj-<16 hex>-snap-*.png beside it.

const (
	gatedSRT  = "stt-1234567890-0a1b2c3d.srt"
	gatedTXT  = "stt-1234567890-0a1b2c3d.txt"
	gatedJSON = "stt-1234567890-0a1b2c3d.segments.json"
	gatedMP4  = "composeproj-0123456789abcdef.mp4"
	gatedSnap = "composeproj-0123456789abcdef-snap-0001.png"
	// a media-job render (ADR 0077): rendered from the caller's private input files
	gatedJobMP4 = "mediajob-0123456789abcdef.mp4"
)

func writeMedia(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("data:"+name), 0o644); err != nil {
		t.Fatal(err)
	}
	if age > 0 {
		old := time.Now().Add(-age)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// sttOutputShape is sttOutputRe's text, written out here and in internal/pipeline's producer pin: a
// change to either side has to touch both.
const sttOutputShape = `^stt-[0-9]+-[0-9a-f]{8}\.(srt|txt|segments\.json)$`

// isGatedLaneOutput says whether a name has the shape one of the gated lanes gives its outputs: the stt upload
// door's transcripts, a project render and its snapshots, a media-job render. Production no longer asks (every
// artifact needs the bearer); the builders and the sweep still rely on the shapes, and these tests hold them.
func isGatedLaneOutput(name string) bool {
	return sttOutputRe.MatchString(name) || projectOutputRe.MatchString(name) || mediaJobOutputRe.MatchString(name)
}

func TestGatedLaneOutputShapes(t *testing.T) {
	if sttOutputRe.String() != sttOutputShape {
		t.Fatalf("sttOutputRe = %s, want %s: update internal/pipeline's producer pin with it", sttOutputRe, sttOutputShape)
	}
	for _, n := range []string{gatedSRT, gatedTXT, gatedJSON, gatedMP4, gatedSnap, "composeproj-0123456789abcdef.webm", "stt-7-ffffffff.srt",
		// the media-job door's renders: the video, a voice, a music clip, a still, and a name derived from the stem
		gatedJobMP4, "mediajob-0123456789abcdef.wav", "mediajob-0123456789abcdef.flac", "mediajob-0123456789abcdef.png", "mediajob-0123456789abcdef-up.mp4",
	} {
		if !isGatedLaneOutput(n) {
			t.Errorf("%q must have a gated lane's output shape", n)
		}
	}
	for _, n := range []string{
		"render-0a1b2c3d.png",          // image-gen
		"compose-0a1b2c3d.mp4",         // the vetted compose-video lane
		"stt-123-0a1b2c3d.wav",         // not a transcript output
		"stt-123-0a1b2c3d.srt.bak",     // not a transcript output
		"stt-123.ogg",                  // the private upload itself (a dot directory holds it anyway)
		"composeproj-xyz.mp4",          // not a 16-hex stem
		"composeproj-0123456789abcdef", // no extension
		"xcomposeproj-0123456789abcdef.mp4",
		"video-0a1b2c3d.mp4",        // a plain dispatch's video: the pipeline's own name
		"mediajob-xyz.mp4",          // not a 16-hex stem
		"mediajob-0123456789abcdef", // no extension
		"xmediajob-0123456789abcdef.mp4",
		"mediajob-0123456789abcdeff.mp4", // 17 hex
	} {
		if isGatedLaneOutput(n) {
			t.Errorf("%q must not have a gated lane's output shape", n)
		}
	}
}

// A node WITH a token serves no artifact without the bearer, whichever task made it or however its name is
// spelled; a loopback node with no token serves as it always did, and one that listens beyond loopback with
// none refuses the read.
func TestEveryMediaFileNeedsTheBearerOnATokenedNode(t *testing.T) {
	cfg := tokenCfg("s3cret")
	cfg.MediaDir = t.TempDir()
	names := []string{gatedSRT, gatedTXT, gatedJSON, gatedMP4, gatedSnap, gatedJobMP4, "render-0a1b2c3d.png", "compose-0a1b2c3d.mp4", "video-0a1b2c3d.mp4", "interview-0a1b2c3d.srt"}
	for _, n := range names {
		writeMedia(t, cfg.MediaDir, n, 0)
	}
	s, _ := newTestServer(t, cfg, &fakeRunner{}, authOpts(true))
	for _, n := range names {
		if rec := do(t, s, http.MethodGet, "/fleet/media/"+n, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no bearer = %d, want 401", n, rec.Code)
		}
		if rec := do(t, s, http.MethodGet, "/fleet/media/"+n, "", map[string]string{"Authorization": "Bearer wrong"}); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with a wrong bearer = %d, want 401", n, rec.Code)
		}
		rec := do(t, s, http.MethodGet, "/fleet/media/"+n, "", map[string]string{"Authorization": "Bearer s3cret"})
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "data:"+n) {
			t.Errorf("GET %s with the bearer = %d (%q)", n, rec.Code, rec.Body.String())
		}
	}
	// The spellings a Windows filesystem folds onto a real file get no bearer-free read either
	// (and are 401 on every OS: the gate runs before the file is looked up).
	for _, n := range []string{"STT-1234567890-0A1B2C3D.SRT", gatedSRT + ".", gatedSRT + "%20", "COMPOSEPROJ-0123456789ABCDEF.MP4", gatedMP4 + ".", gatedJSON + "%20.", "stt-1234~1.srt", "RENDER-0A1B2C3D.PNG"} {
		if rec := do(t, s, http.MethodGet, "/fleet/media/"+n, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no bearer = %d, want 401 (a folded spelling of a name)", n, rec.Code)
		}
	}
	// A missing file is still a 401 without the bearer: the answer must not say whether it exists.
	if rec := do(t, s, http.MethodGet, "/fleet/media/stt-9-deadbeef.srt", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing name with no bearer = %d, want 401", rec.Code)
	}

	// No token on a loopback node: nothing is gated (the local trust boundary).
	open := imageCfg()
	open.MediaDir = cfg.MediaDir
	s2, _ := newTestServer(t, open, &fakeRunner{}, authOpts(true))
	for _, n := range []string{gatedSRT, "render-0a1b2c3d.png"} {
		if rec := do(t, s2, http.MethodGet, "/fleet/media/"+n, "", nil); rec.Code != http.StatusOK {
			t.Errorf("tokenless loopback node GET %s = %d, want 200", n, rec.Code)
		}
	}
	// No token beyond loopback: refused for every name (a misconfiguration; fleet-serve will not start so).
	s3, _ := newTestServer(t, open, &fakeRunner{}, authOpts(false))
	if rec := do(t, s3, http.MethodGet, "/fleet/media/render-0a1b2c3d.png", "", nil); rec.Code != http.StatusForbidden {
		t.Errorf("tokenless non-loopback node GET = %d, want 403", rec.Code)
	}
}

// The sweep removes upload transcripts older than the TTL and nothing else.
func TestSweepSTTTranscriptsRemovesOnlyExpiredUploadOutputs(t *testing.T) {
	cfg := config.Config{MediaDir: t.TempDir()}
	ttl := cfg.EffectiveSTTTranscriptTTL()
	if ttl != 30*time.Minute {
		t.Fatalf("default transcript TTL = %v, want 30m", ttl)
	}
	old := ttl + time.Minute
	for _, n := range []string{gatedSRT, gatedTXT, gatedJSON} {
		writeMedia(t, cfg.MediaDir, n, old)
	}
	fresh := writeMedia(t, cfg.MediaDir, "stt-42-aaaaaaaa.srt", ttl-5*time.Minute)
	keepers := []string{
		writeMedia(t, cfg.MediaDir, "render-0a1b2c3d.png", old*100),
		writeMedia(t, cfg.MediaDir, "stt-legacy-0a1b2c3d.srt", old*100),
		writeMedia(t, cfg.MediaDir, gatedMP4, old*100), // a project render is not a transcript: it is gated, never swept
	}
	if err := os.MkdirAll(filepath.Join(cfg.MediaDir, ".stt-upload"), 0o755); err != nil {
		t.Fatal(err)
	}
	n, err := SweepSTTTranscripts(cfg, time.Now())
	if err != nil || n != 3 {
		t.Fatalf("swept %d (%v), want the 3 expired transcript files", n, err)
	}
	for _, n := range []string{gatedSRT, gatedTXT, gatedJSON} {
		if _, err := os.Stat(filepath.Join(cfg.MediaDir, n)); !os.IsNotExist(err) {
			t.Errorf("expired %s still on disk", n)
		}
	}
	for _, p := range append(keepers, fresh) {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", filepath.Base(p), err)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.MediaDir, ".stt-upload")); err != nil {
		t.Errorf("the upload directory was removed: %v", err)
	}

	// A negative TTL keeps transcripts for good (the behaviour before this existed).
	keep := config.Config{MediaDir: cfg.MediaDir, FleetSTTTranscriptTTLMin: -1}
	left := writeMedia(t, cfg.MediaDir, "stt-43-bbbbbbbb.srt", 1000*time.Hour)
	if n, _ := SweepSTTTranscripts(keep, time.Now()); n != 0 {
		t.Errorf("swept %d with the TTL off", n)
	}
	if _, err := os.Stat(left); err != nil {
		t.Errorf("transcript removed with the TTL off: %v", err)
	}
	// An empty media_dir sweeps nothing and fails nothing.
	if n, err := SweepSTTTranscripts(config.Config{}, time.Now()); n != 0 || err != nil {
		t.Errorf("no media_dir: swept %d, err %v", n, err)
	}
}

// The janitor tick sweeps transcripts: a file past the TTL goes at the next sweep of the job store.
func TestJobStoreSweepAlsoSweepsExpiredTranscripts(t *testing.T) {
	cfg := sttCfg(t, "tok")
	expired := writeMedia(t, cfg.MediaDir, gatedSRT, 2*time.Hour)
	recent := writeMedia(t, cfg.MediaDir, "stt-99-cccccccc.srt", time.Minute)
	_, jobs := newTestServer(t, cfg, &fakeRunner{}, authOpts(true))
	jobs.sweep() // the janitor's tick
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Errorf("an expired transcript survived the janitor sweep: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("a recent transcript was swept: %v", err)
	}
}

// When the job record is evicted its outputs go with it, even before the TTL; a path the result
// names outside media_dir, or by anything but a bare gated name, is never touched.
func TestSTTUploadOutputsGoWhenTheJobRecordIsEvicted(t *testing.T) {
	cfg := sttCfg(t, "tok")
	outside := filepath.Join(t.TempDir(), "elsewhere.srt")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srt := writeMedia(t, cfg.MediaDir, gatedSRT, 0)
	txt := writeMedia(t, cfg.MediaDir, gatedTXT, 0)
	other := writeMedia(t, cfg.MediaDir, "render-0a1b2c3d.png", 0)
	// The result names, besides the two transcript files, a file in media_dir that is NOT an upload
	// output (a render) under json_path, and a file outside media_dir under another key: the node's
	// own pipeline wrote the result, but nothing may delete a file the name does not vouch for.
	data, _ := json.Marshal(map[string]any{
		"language": "en", "num_segments": 1, "segments": []any{},
		"srt_path": srt, "text_path": txt,
		"json_path":  other,
		"extra_path": filepath.ToSlash(outside),
	})
	data2 := string(data)

	now := time.Now()
	clock := func() time.Time { return now }
	jobs := newJobs(time.Minute, clock, time.Hour, 0)
	t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })
	r := &sttRunner{res: core.Result{OK: true, Data: json.RawMessage(data2), Meta: core.Meta{Model: "whisper-stt"}}}
	opts := authOpts(true)
	opts.Cfg = cfg
	s := New(r, jobs, *opts)
	if rec := do(t, s, http.MethodPost, STTUploadPath, sttBody("stt-evict-1", []byte("RIFFaudio"), nil), map[string]string{"Authorization": "Bearer tok"}); rec.Code != http.StatusAccepted {
		t.Fatalf("upload = %d (%s)", rec.Code, rec.Body.String())
	}
	pollDone(t, s, "stt-evict-1", map[string]string{"Authorization": "Bearer tok"})
	for _, p := range []string{srt, txt, other, outside} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s is gone while its job is still held: %v", filepath.Base(p), err)
		}
	}
	now = now.Add(2 * time.Minute) // past the record TTL
	jobs.sweep()
	for _, p := range []string{srt, txt} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the eviction of its job record: %v", filepath.Base(p), err)
		}
	}
	for _, p := range []string{other, outside} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed though it is no gated output of the job: %v", filepath.Base(p), err)
		}
	}
}

// A finished upload job refreshes its outputs' clocks, so a cache hit that returns an earlier job's
// files (the pipeline's cache is keyed on the audio's content) keeps them for a full TTL.
func TestSTTUploadResultRefreshesTheOutputsClock(t *testing.T) {
	cfg := sttCfg(t, "tok")
	srt := writeMedia(t, cfg.MediaDir, gatedSRT, 29*time.Minute)
	data, _ := json.Marshal(map[string]any{"num_segments": 1, "segments": []any{}, "srt_path": srt})
	r := &sttRunner{res: core.Result{OK: true, Data: json.RawMessage(data), Meta: core.Meta{Model: "whisper-stt", CacheHit: true}}}
	s, _ := newTestServer(t, cfg, r, authOpts(true))
	if rec := do(t, s, http.MethodPost, STTUploadPath, sttBody("stt-touch-1", []byte("RIFFaudio"), nil), map[string]string{"Authorization": "Bearer tok"}); rec.Code != http.StatusAccepted {
		t.Fatalf("upload = %d", rec.Code)
	}
	pollDone(t, s, "stt-touch-1", map[string]string{"Authorization": "Bearer tok"})
	fi, err := os.Stat(srt)
	if err != nil {
		t.Fatal(err)
	}
	if age := time.Since(fi.ModTime()); age > time.Minute {
		t.Fatalf("output is %v old after a job returned it; the retention clock must restart", age)
	}
	if n, _ := SweepSTTTranscripts(cfg, time.Now().Add(20*time.Minute)); n != 0 {
		t.Errorf("a file a job just returned was swept 20 minutes later")
	}
}

// A project render's output carries the gated stem, so /fleet/media serves it only to a bearer holder
// from the first byte (no registry to lose across a restart, and no window while it renders).
func TestComposeProjectOutputsCarryTheGatedStem(t *testing.T) {
	cfg := projectCfg(t)
	cfg.MediaDir = t.TempDir()
	payload := projectPayload(func(p *ComposeProjectPayload) { p.JobID = "proj-stem" }, map[string]string{"index.html": "<p>x</p>"})
	req, cleanup, err := BuildRequest(context.Background(), cfg, true, ComposeProjectTask, payload)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	out, _ := req.Params["out"].(string)
	if out == "" || filepath.Dir(out) != cfg.MediaDir {
		t.Fatalf("out = %q, want a file directly under media_dir", out)
	}
	base := filepath.Base(out)
	if !isGatedLaneOutput(base) || filepath.Ext(base) != ".mp4" {
		t.Fatalf("out %q is not a gated .mp4 name", base)
	}
	stem := strings.TrimSuffix(base, ".mp4")
	if !isGatedLaneOutput(stem + "-snap-0001.png") {
		t.Errorf("the render's snapshots (%s-snap-*) must be gated with it", stem)
	}
	// Two jobs never share an output name.
	req2, cleanup2, err := BuildRequest(context.Background(), cfg, true, ComposeProjectTask, projectPayload(func(p *ComposeProjectPayload) { p.JobID = "proj-stem-2" }, map[string]string{"index.html": "<p>x</p>"}))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup2()
	if out2, _ := req2.Params["out"].(string); out2 == out {
		t.Errorf("two project jobs got the same output %q", out)
	}
	// A format other than mp4 keeps its own extension.
	req3, cleanup3, err := BuildRequest(context.Background(), cfg, true, ComposeProjectTask, projectPayload(func(p *ComposeProjectPayload) { p.JobID = "proj-webm"; p.Format = "webm" }, map[string]string{"index.html": "<p>x</p>"}))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup3()
	if o, _ := req3.Params["out"].(string); filepath.Ext(o) != ".webm" || !isGatedLaneOutput(filepath.Base(o)) {
		t.Errorf("webm out = %q", o)
	}
}
