// The stt upload door (ADR 0072): POST /fleet/stt carries the audio BYTES, so a box whose own
// whisper is held can have a fleet node transcribe. Three invariants carry it:
//
//  1. Advertisement == admission, as for the vision lane: "stt-upload" (and the capability fields)
//     appear in health exactly when the door admits, and the door is token-gated.
//  2. The caller reads back the FULL core.Result the node's pipeline produced, defers included.
//  3. The upload lives exactly as long as the job: a private temp file, removed on every exit.
//
// Beside them, the legacy path-taking stt lane joins the bearer rule when the node has a token
// (D17), and both lanes share one concurrency cap whose excess waits in arrival order (D18).
package fleetnode

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	"github.com/dmmdea/offload-harness/internal/fleetqueue"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/sttclient"
)

// sttCfg is a node with a bound whisper seat and its own media dir.
func sttCfg(t *testing.T, token string) config.Config {
	t.Helper()
	c := imageCfg()
	c.STTModel = "whisper-stt"
	c.MediaDir = t.TempDir()
	c.FleetAuthToken = token
	return c
}

func sttBody(jobID string, audio []byte, extra map[string]any) string {
	m := map[string]any{"job_id": jobID, "audio_b64": base64.StdEncoding.EncodeToString(audio), "audio_ext": "ogg"}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// sttRunner records each request and what its audio file held while the job ran.
type sttRunner struct {
	mu       sync.Mutex
	reqs     []core.Request
	audio    [][]byte
	existed  []bool
	res      core.Result
	inflight int
	peak     int
	started  []string
	gate     chan struct{} // when set, every run waits for one token
	entered  chan string   // when set, receives the job id as the run starts
}

func (r *sttRunner) Run(ctx context.Context, req core.Request) core.Result {
	b, err := os.ReadFile(req.Audio)
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.audio = append(r.audio, b)
	r.existed = append(r.existed, err == nil)
	r.inflight++
	if r.inflight > r.peak {
		r.peak = r.inflight
	}
	r.started = append(r.started, req.FleetJobID)
	gate, entered := r.gate, r.entered
	r.mu.Unlock()
	if entered != nil {
		entered <- req.FleetJobID
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
		}
	}
	r.mu.Lock()
	r.inflight--
	r.mu.Unlock()
	return r.res
}

func (r *sttRunner) snapshot() (reqs []core.Request, audio [][]byte, existed []bool, peak int, started []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.Request(nil), r.reqs...), append([][]byte(nil), r.audio...), append([]bool(nil), r.existed...), r.peak, append([]string(nil), r.started...)
}

var sttOK = core.Result{OK: true, Data: json.RawMessage(`{"language":"en","duration_sec":1.5,"num_segments":1,"gist":"hello","segments":[],"segments_truncated":false,"srt_path":"x.srt","text_path":"x.txt","json_path":"x.segments.json"}`), Meta: core.Meta{Model: "whisper-stt"}}

func tasksOf(t *testing.T, h map[string]any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	list, _ := h["supported_task_types"].([]any)
	for _, x := range list {
		s, _ := x.(string)
		out[s] = true
	}
	return out
}

// uploadDirEntries lists what the door's private directory holds right now.
func uploadDirEntries(t *testing.T, cfg config.Config) []string {
	t.Helper()
	ents, err := os.ReadDir(sttUploadDir(cfg))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// Advertisement == admission over the (listener, token, header) matrix, with the capability fields.
func TestSTTUploadAdvertisementMatchesAdmission(t *testing.T) {
	cases := []struct {
		name       string
		token      string
		loopback   bool
		header     string
		advertised bool
		wantStatus int
	}{
		{"loopback, no token", "", true, "", true, http.StatusAccepted},
		{"non-loopback, no token", "", false, "", false, http.StatusForbidden},
		{"non-loopback, token, no header", "s3cret", false, "", true, http.StatusUnauthorized},
		{"non-loopback, token, wrong header", "s3cret", false, "Bearer nope", true, http.StatusUnauthorized},
		{"non-loopback, token, right header", "s3cret", false, "Bearer s3cret", true, http.StatusAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := sttCfg(t, tc.token)
			cfg.STTModelHQ = "whisper-stt-hq"
			s, _ := newTestServer(t, cfg, &sttRunner{res: sttOK}, authOpts(tc.loopback))
			h := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
			if got := tasksOf(t, h)[STTUploadTask]; got != tc.advertised {
				t.Fatalf("%s advertised = %v, want %v (tasks %v)", STTUploadTask, got, tc.advertised, h["supported_task_types"])
			}
			_, hasHQ := h["stt_hq"]
			_, hasCap := h["stt_upload_max_mb"]
			if hasHQ != tc.advertised || hasCap != tc.advertised {
				t.Fatalf("capability fields present = hq %v cap %v, want both %v", hasHQ, hasCap, tc.advertised)
			}
			if tc.advertised && (h["stt_hq"] != true || h["stt_upload_max_mb"] != float64(48)) {
				t.Fatalf("stt_hq = %v, stt_upload_max_mb = %v, want true and 48", h["stt_hq"], h["stt_upload_max_mb"])
			}
			hdr := map[string]string{}
			if tc.header != "" {
				hdr["Authorization"] = tc.header
			}
			rec := do(t, s, http.MethodPost, STTUploadPath, sttBody("stt-j1", []byte("OggS"), nil), hdr)
			if rec.Code != tc.wantStatus {
				t.Fatalf("dispatch status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// The hq capability is the node's stt_model_hq: no hq model, no stt_hq, and an hq upload is refused
// at ack rather than silently run on the standard model.
func TestSTTUploadHQIsAdvertisedFromTheHQModelAndRefusedWithout(t *testing.T) {
	cfg := sttCfg(t, "")
	s, _ := newTestServer(t, cfg, &sttRunner{res: sttOK}, authOpts(true))
	h := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
	if h["stt_hq"] != false {
		t.Fatalf("stt_hq = %v on a node with no stt_model_hq, want false (present, so an asker can tell it from an older node)", h["stt_hq"])
	}
	rec := do(t, s, http.MethodPost, STTUploadPath, sttBody("stt-hq-1", []byte("OggS"), map[string]any{"hq": true}), nil)
	wantErrorShape(t, rec, http.StatusBadRequest, "stt_model_hq")
}

// No bound whisper model: not advertised, and a dispatch is a plain 400 (the lane is not there).
func TestSTTUploadNeedsABoundModel(t *testing.T) {
	cfg := imageCfg()
	cfg.MediaDir = t.TempDir()
	s, _ := newTestServer(t, cfg, &sttRunner{}, authOpts(true))
	h := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
	if tasksOf(t, h)[STTUploadTask] {
		t.Fatalf("stt-upload advertised without a bound stt model: %v", h["supported_task_types"])
	}
	if _, has := h["stt_upload_max_mb"]; has {
		t.Fatal("stt_upload_max_mb published without the lane")
	}
	rec := do(t, s, http.MethodPost, STTUploadPath, sttBody("stt-j2", []byte("OggS"), nil), nil)
	wantErrorShape(t, rec, http.StatusBadRequest, "unsupported task_type")
}

// The runner sees a transcribe request over the uploaded bytes, on a private file that exists while
// the job runs and is gone after it; the node stamps the fleet door, the job id and the asker.
func TestSTTUploadRunsTheUploadedBytesAndRemovesThemAfter(t *testing.T) {
	cfg := sttCfg(t, "tok")
	r := &sttRunner{res: sttOK}
	s, _ := newTestServer(t, cfg, r, authOpts(false))
	auth := map[string]string{"Authorization": "Bearer tok", core.AskerHeader: "node-a"}
	audio := []byte("OggS-not-really-audio")
	rec := do(t, s, http.MethodPost, STTUploadPath, sttBody("stt-j3", audio, map[string]any{"language": "es", "hq": false}), auth)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch status = %d (body %s)", rec.Code, rec.Body.String())
	}
	m := pollDone(t, s, "stt-j3", auth)
	if m["state"] != "done" {
		t.Fatalf("state = %v: %v", m["state"], m)
	}
	reqs, audios, existed, _, _ := r.snapshot()
	if len(reqs) != 1 || !existed[0] || string(audios[0]) != string(audio) {
		t.Fatalf("runner saw %d request(s), audio existed %v, bytes %q: want the uploaded bytes on disk during the run", len(reqs), existed, audios)
	}
	req := reqs[0]
	if req.Task != core.TaskTranscribe || req.Params["language"] != "es" || req.Door != "fleet" || req.FleetJobID != "stt-j3" || req.Requester != "node-a" {
		t.Fatalf("request = %+v, want transcribe, language es, door fleet, the job id and the asker", req)
	}
	if !strings.HasSuffix(req.Audio, ".ogg") || filepath.Dir(req.Audio) != sttUploadDir(cfg) {
		t.Fatalf("audio path %q: want a .ogg file in the door's private directory %q", req.Audio, sttUploadDir(cfg))
	}
	if _, err := os.Stat(req.Audio); !os.IsNotExist(err) {
		t.Fatalf("the upload %q is still on disk after the job finished (stat err %v)", req.Audio, err)
	}
	if left := uploadDirEntries(t, cfg); len(left) != 0 {
		t.Fatalf("private directory still holds %v", left)
	}
}

// The job's data is the WHOLE core.Result, so a defer keeps its class and error class across the wire
// (the legacy lane turned a defer into job state "error" with only the reason).
func TestSTTUploadJobCarriesTheFullResultDefersIncluded(t *testing.T) {
	cfg := sttCfg(t, "tok")
	deferred := core.Deferf("gpu busy: a media job holds the GPU", "", core.Meta{ErrClass: "gpu_busy", Model: "whisper-stt"})
	deferred.DeferClass = core.DeferClassCapacity
	s, _ := newTestServer(t, cfg, &sttRunner{res: deferred}, authOpts(false))
	auth := map[string]string{"Authorization": "Bearer tok"}
	if rec := do(t, s, http.MethodPost, STTUploadPath, sttBody("stt-j4", []byte("OggS"), nil), auth); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch status = %d (body %s)", rec.Code, rec.Body.String())
	}
	m := pollDone(t, s, "stt-j4", auth)
	if m["state"] != "done" {
		t.Fatalf("state = %v, want done (a defer is a done job whose data says deferred): %v", m["state"], m)
	}
	var res core.Result
	if err := json.Unmarshal(mustJSON(t, m["data"]), &res); err != nil {
		t.Fatalf("data is not a core.Result: %v", err)
	}
	if !res.Deferred || res.Reason != deferred.Reason || res.DeferClass != core.DeferClassCapacity || res.Meta.ErrClass != "gpu_busy" {
		t.Fatalf("result = %+v, want the runner's defer verbatim, class and error class included", res)
	}
}

// A job's transcript is the caller's audio in prose: a poll without the bearer is 401.
func TestSTTUploadJobPollIsTokenGated(t *testing.T) {
	cfg := sttCfg(t, "tok")
	s, _ := newTestServer(t, cfg, &sttRunner{res: sttOK}, authOpts(false))
	auth := map[string]string{"Authorization": "Bearer tok"}
	if rec := do(t, s, http.MethodPost, STTUploadPath, sttBody("stt-j5", []byte("OggS"), nil), auth); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch status = %d", rec.Code)
	}
	wantErrorShape(t, do(t, s, http.MethodGet, "/fleet/jobs/stt-j5", "", nil), http.StatusUnauthorized, "unauthorized")
	if m := pollDone(t, s, "stt-j5", auth); m["state"] != "done" {
		t.Fatalf("state = %v", m["state"])
	}
}

// The bearer is checked BEFORE the body is read: an unauthorized caller with a body that would be a
// validation error (or too large) gets 401, never a 400 to probe the envelope with.
func TestSTTUploadChecksTheBearerBeforeReadingTheBody(t *testing.T) {
	cfg := sttCfg(t, "tok")
	s, _ := newTestServer(t, cfg, &sttRunner{res: sttOK}, authOpts(false))
	for name, body := range map[string]string{
		"garbage":  "this is not json",
		"too big":  `{"job_id":"x","audio_b64":"` + strings.Repeat("A", int(STTUploadBodyCap(cfg))+10) + `"}`,
		"no bytes": `{"job_id":"x"}`,
	} {
		rec := do(t, s, http.MethodPost, STTUploadPath, body, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401 before the body is read (body %s)", name, rec.Code, rec.Body.String())
		}
	}
}

// The body cap is the decoded-audio cap in base64 plus slack; past it is a 400 naming the key, and a
// body under it whose decoded audio is over the cap is refused at ack with the key named.
func TestSTTUploadBodyCapFollowsTheUploadCap(t *testing.T) {
	cfg := sttCfg(t, "")
	cfg.FleetSTTUploadMaxMB = 1
	s, _ := newTestServer(t, cfg, &sttRunner{res: sttOK}, authOpts(true))
	if got, want := STTUploadBodyCap(cfg), int64(base64.StdEncoding.EncodedLen(1<<20))+sttUploadBodySlack; got != want {
		t.Fatalf("STTUploadBodyCap = %d, want %d", got, want)
	}
	big := `{"job_id":"stt-big","audio_b64":"` + strings.Repeat("A", int(STTUploadBodyCap(cfg))+1) + `"}`
	wantErrorShape(t, do(t, s, http.MethodPost, STTUploadPath, big, nil), http.StatusBadRequest, "request body too large")
	// 1 MiB + 1 byte decoded: inside the body cap's slack, over the audio cap.
	over := sttBody("stt-over", make([]byte, 1<<20+1), nil)
	wantErrorShape(t, do(t, s, http.MethodPost, STTUploadPath, over, nil), http.StatusBadRequest, "fleet_stt_upload_max_mb")
	if left := uploadDirEntries(t, cfg); len(left) != 0 {
		t.Fatalf("a refused upload left %v on disk", left)
	}
	// Exactly at the cap passes.
	ok := sttBody("stt-at-cap", make([]byte, 1<<20), nil)
	if rec := do(t, s, http.MethodPost, STTUploadPath, ok, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("an upload exactly at the cap: status = %d (body %s)", rec.Code, rec.Body.String())
	}
}

// Every caller mistake is a 400 that names the problem and leaves nothing behind.
func TestSTTUploadPayloadValidation(t *testing.T) {
	cfg := sttCfg(t, "")
	s, _ := newTestServer(t, cfg, &sttRunner{res: sttOK}, authOpts(true))
	cases := []struct {
		name, body, want string
	}{
		{"missing job id", `{"audio_b64":"T2dnUw=="}`, "job_id required"},
		{"no audio", `{"job_id":"v1"}`, "audio_b64"},
		{"empty audio", `{"job_id":"v2","audio_b64":""}`, "audio_b64"},
		{"not base64", `{"job_id":"v3","audio_b64":"%%%"}`, "base64"},
		{"unknown field", `{"job_id":"v4","audio_b64":"T2dnUw==","path":"/etc/passwd"}`, "malformed stt body"},
		{"extension with a dot path", `{"job_id":"v5","audio_b64":"T2dnUw==","audio_ext":"../x"}`, "audio_ext"},
		{"extension too long", `{"job_id":"v6","audio_b64":"T2dnUw==","audio_ext":"abcdefghi"}`, "audio_ext"},
		{"language with a flag", `{"job_id":"v7","audio_b64":"T2dnUw==","language":"-x"}`, "language"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantErrorShape(t, do(t, s, http.MethodPost, STTUploadPath, tc.body, nil), http.StatusBadRequest, tc.want)
			if left := uploadDirEntries(t, cfg); len(left) != 0 {
				t.Fatalf("a refused upload left %v on disk", left)
			}
		})
	}
	rec := do(t, s, http.MethodPost, STTUploadPath, `{"job_id":"v8","audio_b64":"T2dnUw=="}`, map[string]string{"Content-Type": "text/plain"})
	wantErrorShape(t, rec, http.StatusBadRequest, "content-type")
}

// An extension is lowercase letters and digits after an optional dot; an empty one still works.
func TestSTTUploadExtensions(t *testing.T) {
	cfg := sttCfg(t, "")
	r := &sttRunner{res: sttOK}
	s, _ := newTestServer(t, cfg, r, authOpts(true))
	for i, tc := range []struct{ ext, suffix string }{{".OGG", ".ogg"}, {"m4a", ".m4a"}, {"", ".audio"}} {
		id := "stt-ext-" + string(rune('a'+i))
		body := sttBody(id, []byte("OggS"), map[string]any{"audio_ext": tc.ext})
		if rec := do(t, s, http.MethodPost, STTUploadPath, body, nil); rec.Code != http.StatusAccepted {
			t.Fatalf("ext %q: status = %d (body %s)", tc.ext, rec.Code, rec.Body.String())
		}
		pollDone(t, s, id, nil)
	}
	reqs, _, _, _, _ := r.snapshot()
	var got []string
	for _, q := range reqs {
		got = append(got, filepath.Ext(q.Audio))
	}
	if len(got) != 3 || got[0] != ".ogg" && got[1] != ".ogg" {
		t.Fatalf("extensions seen = %v", got)
	}
	want := map[string]bool{".ogg": true, ".m4a": true, ".audio": true}
	for _, e := range got {
		delete(want, e)
	}
	if len(want) != 0 {
		t.Fatalf("extensions seen = %v, missing %v", got, want)
	}
}

// A re-dispatch of a job id the node already holds re-acks without writing a second upload.
func TestSTTUploadReDispatchOfAKnownJobWritesNothing(t *testing.T) {
	cfg := sttCfg(t, "")
	r := &sttRunner{res: sttOK, gate: make(chan struct{}), entered: make(chan string, 4)}
	s, _ := newTestServer(t, cfg, r, authOpts(true))
	body := sttBody("stt-dup", []byte("OggS"), nil)
	if rec := do(t, s, http.MethodPost, STTUploadPath, body, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("first dispatch: %d", rec.Code)
	}
	<-r.entered
	if rec := do(t, s, http.MethodPost, STTUploadPath, body, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("re-dispatch: %d", rec.Code)
	}
	if left := uploadDirEntries(t, cfg); len(left) != 1 {
		t.Fatalf("private directory holds %v after a re-dispatch, want exactly the running job's upload", left)
	}
	close(r.gate)
	pollDone(t, s, "stt-dup", nil)
	if left := uploadDirEntries(t, cfg); len(left) != 0 {
		t.Fatalf("private directory still holds %v", left)
	}
}

// A job cancelled while it waits at the stt gate (the node shut down) never runs, and its upload goes
// with it, as does the running job's.
func TestSTTUploadOfAJobCancelledAtTheGateIsRemoved(t *testing.T) {
	cfg := sttCfg(t, "")
	r := &sttRunner{res: sttOK, gate: make(chan struct{}), entered: make(chan string, 4)}
	s, jobs := newTestServer(t, cfg, r, authOpts(true))
	if rec := do(t, s, http.MethodPost, STTUploadPath, sttBody("stt-run", []byte("OggS"), nil), nil); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch: %d", rec.Code)
	}
	<-r.entered
	if rec := do(t, s, http.MethodPost, STTUploadPath, sttBody("stt-wait", []byte("OggS"), nil), nil); rec.Code != http.StatusAccepted {
		t.Fatalf("second dispatch: %d", rec.Code)
	}
	sttWaitFor(t, "second upload written", func() bool { return len(uploadDirEntries(t, cfg)) == 2 })
	jobs.DrainAndStop(200 * time.Millisecond) // the first run never finishes: the drain cancels both
	if left := uploadDirEntries(t, cfg); len(left) != 0 {
		t.Fatalf("after a drain the private directory still holds %v", left)
	}
	if _, _, _, _, started := r.snapshot(); len(started) != 1 {
		t.Fatalf("runs started = %v: the waiting job must never have reached the pipeline", started)
	}
}

// D17: the legacy path-taking stt lane joins the bearer rule when the node has a token, so a tailnet
// peer cannot make the node convert and transcribe an arbitrary node-local file. A node with NO token
// keeps its behaviour (the lane stays open beyond loopback), so no deployed tokenless node breaks.
func TestLegacySTTDispatchIsTokenGatedWhenTheNodeHasAToken(t *testing.T) {
	const body = `{"job_id":"stt-legacy-1","task_type":"stt","payload":{"audio":"/node/local/a.wav"}}`
	t.Run("token set, no bearer", func(t *testing.T) {
		s, _ := newTestServer(t, sttCfg(t, "tok"), &sttRunner{res: sttOK}, authOpts(false))
		wantErrorShape(t, do(t, s, http.MethodPost, "/fleet/dispatch", body, nil), http.StatusUnauthorized, "unauthorized")
	})
	t.Run("token set, wrong bearer", func(t *testing.T) {
		s, _ := newTestServer(t, sttCfg(t, "tok"), &sttRunner{res: sttOK}, authOpts(false))
		wantErrorShape(t, do(t, s, http.MethodPost, "/fleet/dispatch", body, map[string]string{"Authorization": "Bearer nope"}), http.StatusUnauthorized, "unauthorized")
	})
	t.Run("token set, right bearer", func(t *testing.T) {
		s, _ := newTestServer(t, sttCfg(t, "tok"), &sttRunner{res: sttOK}, authOpts(false))
		if rec := do(t, s, http.MethodPost, "/fleet/dispatch", body, map[string]string{"Authorization": "Bearer tok"}); rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
		}
	})
	t.Run("no token, beyond loopback: unchanged", func(t *testing.T) {
		s, _ := newTestServer(t, sttCfg(t, ""), &sttRunner{res: sttOK}, authOpts(false))
		if rec := do(t, s, http.MethodPost, "/fleet/dispatch", body, nil); rec.Code != http.StatusAccepted {
			t.Fatalf("a tokenless node's stt lane changed behaviour: status = %d (body %s)", rec.Code, rec.Body.String())
		}
	})
}

// The same rule on the poll: a legacy stt job's transcript is masked from a reader without the bearer
// once the node has a token, and the jobs feed hides its error text; a tokenless node is unchanged.
func TestLegacySTTJobPollIsMaskedWhenTheNodeHasAToken(t *testing.T) {
	const body = `{"job_id":"stt-legacy-2","task_type":"stt","payload":{"audio":"/node/local/a.wav"}}`
	s, _ := newTestServer(t, sttCfg(t, "tok"), &sttRunner{res: sttOK}, authOpts(false))
	auth := map[string]string{"Authorization": "Bearer tok"}
	if rec := do(t, s, http.MethodPost, "/fleet/dispatch", body, auth); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch: %d", rec.Code)
	}
	wantErrorShape(t, do(t, s, http.MethodGet, "/fleet/jobs/stt-legacy-2", "", nil), http.StatusUnauthorized, "unauthorized")
	if m := pollDone(t, s, "stt-legacy-2", auth); m["state"] != "done" {
		t.Fatalf("state = %v", m["state"])
	}
	open, _ := newTestServer(t, sttCfg(t, ""), &sttRunner{res: sttOK}, authOpts(false))
	if rec := do(t, open, http.MethodPost, "/fleet/dispatch", strings.Replace(body, "stt-legacy-2", "stt-legacy-3", 1), nil); rec.Code != http.StatusAccepted {
		t.Fatalf("tokenless dispatch: %d", rec.Code)
	}
	if m := pollDone(t, open, "stt-legacy-3", nil); m["state"] != "done" {
		t.Fatalf("a tokenless node's poll changed: %v", m)
	}
}

// legacySTT and uploadSTT dispatch one job of each lane.
func legacySTT(t *testing.T, s *Server, id string, hdr map[string]string) {
	t.Helper()
	body := `{"job_id":"` + id + `","task_type":"stt","payload":{"audio":"/node/local/a.wav"}}`
	if rec := do(t, s, http.MethodPost, "/fleet/dispatch", body, hdr); rec.Code != http.StatusAccepted {
		t.Fatalf("legacy dispatch %s: %d (body %s)", id, rec.Code, rec.Body.String())
	}
}

func uploadSTT(t *testing.T, s *Server, id string, hdr map[string]string) {
	t.Helper()
	if rec := do(t, s, http.MethodPost, STTUploadPath, sttBody(id, []byte("OggS"), nil), hdr); rec.Code != http.StatusAccepted {
		t.Fatalf("upload dispatch %s: %d (body %s)", id, rec.Code, rec.Body.String())
	}
}

func sttWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// D18: one cap covers both lanes. Jobs over it wait in arrival order and run once a slot frees; none
// fails, none runs beside another.
func TestSTTJobsOverTheCapWaitInOrderAcrossBothLanes(t *testing.T) {
	cfg := sttCfg(t, "")
	r := &sttRunner{res: sttOK, gate: make(chan struct{}), entered: make(chan string, 8)}
	s, _ := newTestServer(t, cfg, r, authOpts(true))

	legacySTT(t, s, "stt-q1", nil)
	if got := <-r.entered; got != "stt-q1" {
		t.Fatalf("first run = %s", got)
	}
	// The next three queue behind it, alternating lanes; they are admitted in this order.
	uploadSTT(t, s, "stt-q2", nil)
	sttWaitFor(t, "stt-q2 admitted", func() bool { _, ok := s.jobs.Get("stt-q2"); return ok })
	legacySTT(t, s, "stt-q3", nil)
	sttWaitFor(t, "stt-q3 admitted", func() bool { _, ok := s.jobs.Get("stt-q3"); return ok })
	uploadSTT(t, s, "stt-q4", nil)
	sttWaitFor(t, "stt-q4 admitted", func() bool { _, ok := s.jobs.Get("stt-q4"); return ok })

	time.Sleep(150 * time.Millisecond) // a second run beside the first would have started by now
	// The three waiters are counted on the whisper client, so the call ahead of them keeps the model
	// loaded for them (sttclient.Queued): a burst pays one cold start.
	if n := sttclient.QueuedNow(); n != 3 {
		t.Fatalf("sttclient sees %d queued job(s), want the 3 waiting at the gate", n)
	}
	if _, _, _, peak, started := r.snapshot(); peak != 1 || len(started) != 1 {
		t.Fatalf("peak concurrency %d, runs started %v: want one at a time while the cap is 1", peak, started)
	}
	for _, want := range []string{"stt-q2", "stt-q3", "stt-q4"} {
		r.gate <- struct{}{} // let the running job finish
		select {
		case got := <-r.entered:
			if got != want {
				t.Fatalf("next run = %s, want %s: excess jobs must run in arrival order", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never ran", want)
		}
	}
	r.gate <- struct{}{}
	for _, id := range []string{"stt-q1", "stt-q2", "stt-q3", "stt-q4"} {
		if m := pollDone(t, s, id, nil); m["state"] != "done" {
			t.Fatalf("%s state = %v: a queued stt job must complete, not fail (%v)", id, m["state"], m)
		}
	}
	if _, _, _, peak, _ := r.snapshot(); peak != 1 {
		t.Fatalf("peak concurrency %d, want 1", peak)
	}
}

// fleet_stt_max_concurrent raises the cap: two run together, the third waits.
func TestSTTConcurrencyCapFollowsTheKey(t *testing.T) {
	cfg := sttCfg(t, "")
	cfg.FleetSTTMaxConcurrent = 2
	r := &sttRunner{res: sttOK, gate: make(chan struct{}), entered: make(chan string, 8)}
	s, _ := newTestServer(t, cfg, r, authOpts(true))
	legacySTT(t, s, "stt-c1", nil)
	legacySTT(t, s, "stt-c2", nil)
	legacySTT(t, s, "stt-c3", nil)
	<-r.entered
	<-r.entered
	time.Sleep(150 * time.Millisecond)
	if _, _, _, peak, started := r.snapshot(); peak != 2 || len(started) != 2 {
		t.Fatalf("peak %d, started %v: want two at once and the third waiting", peak, started)
	}
	close(r.gate)
	for _, id := range []string{"stt-c1", "stt-c2", "stt-c3"} {
		pollDone(t, s, id, nil)
	}
}

// Other task types never touch the stt gate: a render runs while stt is saturated.
func TestSTTGateDoesNotHoldBackOtherLanes(t *testing.T) {
	cfg := sttCfg(t, "")
	r := &sttRunner{res: sttOK, gate: make(chan struct{}), entered: make(chan string, 8)}
	s, _ := newTestServer(t, cfg, r, authOpts(true))
	legacySTT(t, s, "stt-g1", nil)
	<-r.entered
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"img-1","task_type":"image-gen","payload":{"prompt":"p"}}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("image dispatch: %d (%s)", rec.Code, rec.Body.String())
	}
	select {
	case got := <-r.entered:
		if got != "img-1" {
			t.Fatalf("entered %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("an image job was held behind a running stt job")
	}
	close(r.gate)
}

// The gate itself: strict arrival order, a cancelled waiter leaves the line without taking a slot,
// and a release hands the slot to the first waiter.
func TestSTTGateIsFIFOAndCancellable(t *testing.T) {
	g := newSTTGate(1)
	rel1, err := g.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	cancelled := make(chan error, 1)
	ctxB, cancelB := context.WithCancel(context.Background())
	start := func(n int, ctx context.Context) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := g.acquire(ctx)
			if err != nil {
				cancelled <- err
				return
			}
			mu.Lock()
			order = append(order, n)
			mu.Unlock()
			rel()
		}()
	}
	waiting := func(n int) {
		sttWaitFor(t, "waiters queued", func() bool { return g.waiting() == n })
	}
	start(1, context.Background())
	waiting(1)
	start(2, ctxB) // will be cancelled
	waiting(2)
	start(3, context.Background())
	waiting(3)
	cancelB()
	if err := <-cancelled; err != context.Canceled {
		t.Fatalf("cancelled waiter err = %v", err)
	}
	waiting(2)
	rel1()
	wg.Wait()
	if len(order) != 2 || order[0] != 1 || order[1] != 3 {
		t.Fatalf("order = %v, want [1 3]: arrival order, the cancelled waiter gone", order)
	}
	if g.waiting() != 0 || g.holders() != 0 {
		t.Fatalf("gate not empty at the end: waiting %d holders %d", g.waiting(), g.holders())
	}
	// A release called twice frees one slot, not two.
	relA, _ := g.acquire(context.Background())
	relA()
	relA()
	relB, _ := g.acquire(context.Background())
	if g.holders() != 1 {
		t.Fatalf("holders = %d after a double release and one acquire, want 1", g.holders())
	}
	relB()
}

// A pulled (claim-loop) stt job meets the same gate and the same bearer-masking rule as a pushed one.
func TestPulledSTTJobIsGatedAndMaskedLikeAPushedOne(t *testing.T) {
	cfg := sttCfg(t, "tok")
	s, _ := newTestServer(t, cfg, &sttRunner{res: sttOK}, authOpts(false))
	spec := s.claimSpec("stt", func() {})
	if !spec.Gated {
		t.Error("a pulled stt job is not marked Gated although the node has a token: its result would read without the bearer")
	}
	if !spec.Uncapped {
		t.Error("stt must stay outside fleet_max_concurrent_jobs (it has its own cap)")
	}
	if up := s.claimSpec(STTUploadTask, func() {}); !up.Gated {
		t.Error("a pulled stt-upload job is not marked Gated")
	}
	open, _ := newTestServer(t, sttCfg(t, ""), &sttRunner{res: sttOK}, authOpts(true))
	if open.claimSpec("stt", func() {}).Gated {
		t.Error("a tokenless node's pulled stt job became Gated")
	}
}

// Orphaned uploads (a crash between the write and the cleanup) are swept at startup; a young one
// (a job another process is running) stays.
func TestSweepOrphanedSTTUploads(t *testing.T) {
	cfg := sttCfg(t, "")
	dir := sttUploadDir(cfg)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old, young, foreign := filepath.Join(dir, "stt-111.ogg"), filepath.Join(dir, "stt-222.ogg"), filepath.Join(dir, "keep.txt")
	for _, p := range []string{old, young, foreign} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	long := time.Now().Add(-48 * time.Hour)
	for _, p := range []string{old, foreign} {
		if err := os.Chtimes(p, long, long); err != nil {
			t.Fatal(err)
		}
	}
	n, err := SweepOrphanedSTTUploads(cfg, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("swept %d (err %v), want 1", n, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the old upload survived the sweep")
	}
	for _, p := range []string{young, foreign} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was swept: only old stt-* uploads go", filepath.Base(p))
		}
	}
	if n, err := SweepOrphanedSTTUploads(sttCfg(t, ""), time.Now()); err != nil || n != 0 {
		t.Errorf("a node with no upload directory: swept %d, err %v", n, err)
	}
}

// The upload directory lives under media_dir, and /fleet/media must never list or serve it: a dot
// name is refused outright.
func TestMediaRouteNeverServesTheUploadDirectory(t *testing.T) {
	cfg := sttCfg(t, "")
	s, _ := newTestServer(t, cfg, &sttRunner{res: sttOK}, authOpts(true))
	if err := os.MkdirAll(sttUploadDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sttUploadDir(cfg), "stt-9.ogg"), []byte("secret audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.MediaDir, "ok.srt"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{filepath.Base(sttUploadDir(cfg)), ".hidden"} {
		rec := do(t, s, http.MethodGet, "/fleet/media/"+name, "", nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /fleet/media/%s = %d, want 404 (body %q)", name, rec.Code, rec.Body.String())
		}
	}
	if rec := do(t, s, http.MethodGet, "/fleet/media/ok.srt", "", nil); rec.Code != http.StatusOK {
		t.Errorf("an ordinary media file stopped being served: %d", rec.Code)
	}
}

// A pulled (claim-loop) stt job takes its slot under the same cap as a pushed one: two claimed jobs
// run one after the other, and the second one's card stays queued until it holds the slot.
func TestPulledSTTJobsWaitUnderTheSameCap(t *testing.T) {
	cfg := sttCfg(t, "tok")
	r := &sttRunner{res: sttOK, gate: make(chan struct{}), entered: make(chan string, 4)}
	s, jobs := newTestServer(t, cfg, r, authOpts(false))
	next := 0
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/fleet/queue/claim" {
			next++
			if next > 2 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(fleetqueue.Job{
				ID: "pulled-stt-" + string(rune('0'+next)), TaskType: "stt",
				Payload: json.RawMessage(`{"audio":"/node/local/a.wav"}`),
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer holder.Close()
	client := &http.Client{Timeout: 5 * time.Second, Transport: netguard.SafeTransport(nil)}
	for i := 1; i <= 2; i++ {
		if id, ok := s.claimOne(context.Background(), client, holder.URL, "testnode", cfg); !ok || id != "pulled-stt-"+string(rune('0'+i)) {
			t.Fatalf("claimOne #%d = %q, %v", i, id, ok)
		}
	}
	if got := <-r.entered; got != "pulled-stt-1" {
		t.Fatalf("first run = %s", got)
	}
	time.Sleep(150 * time.Millisecond)
	if _, _, _, peak, started := r.snapshot(); peak != 1 || len(started) != 1 {
		t.Fatalf("peak %d, started %v: a pulled stt job ran beside another, past the node's cap", peak, started)
	}
	r.gate <- struct{}{}
	if got := <-r.entered; got != "pulled-stt-2" {
		t.Fatalf("second run = %s", got)
	}
	r.gate <- struct{}{}
	waitJobState(t, jobs, "pulled-stt-1", JobDone)
	waitJobState(t, jobs, "pulled-stt-2", JobDone)
}
