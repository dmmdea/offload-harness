package fleetnode

// The stt upload door (ADR 0072): POST /fleet/stt transcribes audio a caller SENDS, so a box whose
// own whisper is held (a render owns its cards) can have a fleet node transcribe it. The legacy "stt"
// task of POST /fleet/dispatch takes only a path on THIS node's disk and a body capped at 1 MiB, so
// nothing could ever upload audio to it; this door carries the bytes (base64 in JSON), capped by
// fleet_stt_upload_max_mb, writes them to a private file, runs the node's own transcribe pipeline
// over it and returns the WHOLE core.Result — defer_class and err_class survive the wire, like the
// vision and text lanes — then deletes the file.
//
// The door is token-gated exactly like the vision lane (tokenGated: a fleet_auth_token for anything
// beyond loopback; loopback with no token stays open), and the bearer is checked BEFORE a byte of
// the body is read. It is advertised in health ("stt-upload", with stt_hq and stt_upload_max_mb)
// only when it would admit, so an asker never picks a node that cannot take the job; a node that
// predates the door never lists it.
//
// The node's outputs (.srt, .txt, .segments.json) are written under media_dir and fetchable through
// GET /fleet/media/{name}, as for every transcription: the asker fetches the segment list from
// there when the inline copy was truncated. They do not stay: they are removed when the job record
// is evicted or after fleet_stt_transcript_ttl_min, and on a node with a token the media route
// serves them only to a bearer holder (media_gate.go).
//
// Concurrency: the legacy lane and this door share ONE cap (fleet_stt_max_concurrent, default 1):
// whisper is a single-slot upstream, and a job over the cap waits its turn in arrival order.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/sttclient"
)

// STTUploadTask is the door's fleet task_type: what health lists in supported_task_types and what
// the job feed shows as `task`. The legacy path-taking lane keeps "stt".
const STTUploadTask = "stt-upload"

// STTUploadPath is the door's route.
const STTUploadPath = "/fleet/stt"

// STTUploadPayload is the POST /fleet/stt body. job_id is the caller-minted id exactly as on
// /fleet/dispatch (re-acks and polls key on it); audio_b64 is the audio file, standard base64;
// audio_ext its extension without the dot (ffmpeg sniffs the content, the extension only names the
// file); language and hq mirror offload_transcribe's parameters.
type STTUploadPayload struct {
	JobID    string `json:"job_id"`
	AudioB64 sttB64 `json:"audio_b64"`
	AudioExt string `json:"audio_ext,omitempty"`
	Language string `json:"language,omitempty"`
	HQ       bool   `json:"hq,omitempty"`
}

// sttB64 is the audio_b64 field decoded WITHOUT a copy: the body of an upload is up to 85 MiB of base64,
// and an ordinary string field would unquote it into a second buffer on every decode. The slice
// aliases the JSON the caller handed to Unmarshal, which the handler owns and never mutates for the
// life of the request (the documented hazard of keeping UnmarshalJSON's argument does not arise).
// A value carrying a JSON escape (an encoder that writes "\/") takes the ordinary copying path.
type sttB64 []byte

func (f *sttB64) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*f = nil
		return nil
	}
	if len(b) < 2 || b[0] != '"' {
		return errors.New("audio_b64 must be a string")
	}
	inner := b[1 : len(b)-1]
	if bytes.IndexByte(inner, byte(0x5c)) >= 0 { // a backslash
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		*f = sttB64(str)
		return nil
	}
	*f = sttB64(inner)
	return nil
}

// sttSkip discards a JSON value without keeping it: parseSTTUpload uses it to list a body's keys.
type sttSkip struct{}

func (*sttSkip) UnmarshalJSON([]byte) error { return nil }

var sttUploadKeys = map[string]bool{"job_id": true, "audio_b64": true, "audio_ext": true, "language": true, "hq": true}

// parseSTTUpload is the door's strict decode (unknown fields refused, as on every fleet door) that
// never copies the audio: two passes over the same bytes, one to list the keys and one into the
// payload, whose audio field aliases body. The handler calls it to learn the job id and again, at
// admission, through buildSTTUpload.
func parseSTTUpload(body []byte) (STTUploadPayload, error) {
	var keys map[string]sttSkip
	if err := json.Unmarshal(body, &keys); err != nil {
		return STTUploadPayload{}, err
	}
	for k := range keys {
		if !sttUploadKeys[k] {
			return STTUploadPayload{}, fmt.Errorf("unknown field %q", k)
		}
	}
	var p STTUploadPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return STTUploadPayload{}, err
	}
	return p, nil
}

// sttUploadInFlightMax bounds the uploads one node holds in memory at once. The body is read and
// decoded BEFORE the admission gates (a known job id must still re-ack, so the id has to be read
// first), so without a bound N concurrent uploads hold N times the peak. A caller over the bound
// waits for a slot (a waiter holds a goroutine, not a body), so a burst is served in turn like the
// jobs behind it; one that waits longer than sttUploadSlotWait gets a re-placeable 503.
const sttUploadInFlightMax = 2

// sttUploadSlotWait is a var only so a test can shorten it.
var sttUploadSlotWait = 30 * time.Second

// sttUploadSlotAnswerRoom is the write time left for the 503 once the slot wait has run out.
const sttUploadSlotAnswerRoom = 10 * time.Second

// takeSTTUploadSlot waits for an upload slot, or answers the 503 and reports false. The caller
// releases the slot by receiving from s.sttUploadSlots.
func (s *Server) takeSTTUploadSlot(w http.ResponseWriter, r *http.Request) bool {
	// net/http arms the blanket 30 s WriteTimeout at header-read, so a slot wait of that length would
	// expire it before the 503 below is written and the caller would read a bare connection reset, not
	// the re-placeable refusal: the wait extends its own write deadline first, with room for the answer.
	s.extendWrite(w, sttUploadSlotWait+sttUploadSlotAnswerRoom, "the stt upload slot wait")
	wait := time.NewTimer(sttUploadSlotWait)
	defer wait.Stop()
	select {
	case s.sttUploadSlots <- struct{}{}:
		return true
	case <-r.Context().Done():
		return false // the caller left: nobody is listening for an answer
	case <-wait.C:
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("node busy receiving stt uploads (%d in flight for over %s): retry shortly or place elsewhere", sttUploadInFlightMax, sttUploadSlotWait))
		return false
	}
}

// sttUploadBodySlack is the envelope room on top of the base64-inflated audio cap: the other fields
// and the JSON framing.
const sttUploadBodySlack = 64 << 10

// STTUploadBodyCap is the request-body ceiling of POST /fleet/stt for cfg: the node's upload cap in
// base64 plus slack.
func STTUploadBodyCap(cfg config.Config) int64 {
	return int64(base64.StdEncoding.EncodedLen(int(cfg.EffectiveSTTUploadMaxBytes()))) + sttUploadBodySlack
}

// STTUploadAdmissible is THE predicate behind the door, on both sides of the wire (the health
// advertisement and the ack-time gate): a bound whisper model and safe reachability, the vision
// lane's rule — a loopback listener, or a fleet_auth_token for anything beyond it.
func STTUploadAdmissible(cfg config.Config, loopbackListener bool) bool {
	return cfg.STTModel != "" && AgentLaneSafelyReachable(cfg, loopbackListener)
}

// sttUploadWindow is how long a caller that passed the bearer check may take to deliver its body: the
// server's blanket 30 s read timeout would cut 64 MiB on an ordinary link (64 MiB in 30 s needs
// 18 Mbit/s).
const sttUploadWindow = 10 * time.Minute

var (
	sttExtRe  = regexp.MustCompile(`^[a-z0-9]{1,8}$`)
	sttLangRe = regexp.MustCompile(`^(auto|[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})?)$`)
)

// sttUploadDir is the door's private directory: a dot directory under media_dir (so a node's own
// media listing never shows it and /fleet/media refuses a dot name), holding only the files of jobs
// in flight. A node with no media_dir uses the OS temp directory.
func sttUploadDir(cfg config.Config) string {
	base := cfg.MediaDir
	if base == "" {
		base = filepath.Join(os.TempDir(), "local-offload")
	}
	return filepath.Join(base, ".stt-upload")
}

// handleSTTUpload is the door's ack path. It checks the bearer BEFORE reading the body, gives a token
// holder the longer delivery window, caps and strictly decodes the body, and joins the shared
// admission path (admit): known-id re-ack, drain, lease, band and queue gates, then BuildRequest
// (buildSTTUpload, which writes the file) and the job store.
func (s *Server) handleSTTUpload(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeGated(w, r, STTUploadTask) {
		return
	}
	// A caller that passed the bearer check holds one of the node's few upload slots from before the
	// first body byte until the job is admitted (or refused): see sttUploadInFlightMax.
	if !s.takeSTTUploadSlot(w, r) {
		return
	}
	defer func() { <-s.sttUploadSlots }()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(sttUploadWindow))
	_ = rc.SetWriteDeadline(time.Now().Add(sttUploadWindow))
	limit := STTUploadBodyCap(s.opts.Cfg)
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("content-type must be application/json (got %q)", ct))
			return
		}
	}
	// One buffer sized from Content-Length: io.ReadAll would grow by doubling and peak near twice the body.
	var buf bytes.Buffer
	if cl := r.ContentLength; cl > 0 && cl <= limit {
		buf.Grow(int(cl))
	}
	_, err := buf.ReadFrom(r.Body)
	body := buf.Bytes()
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("request body too large (limit %d bytes: fleet_stt_upload_max_mb as base64 plus slack)", limit))
			return
		}
		writeError(w, http.StatusBadRequest, "reading stt body: "+err.Error())
		return
	}
	p, err := parseSTTUpload(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "malformed stt body: "+err.Error())
		return
	}
	s.admit(w, r, dispatchEnvelope{JobID: p.JobID, TaskType: STTUploadTask, Payload: body})
}

// buildSTTUpload turns an admitted payload into a transcribe request over a private copy of the
// audio. Every refusal is a 400 at ack time (the caller can get each of them wrong) and leaves
// nothing on disk; a failure that is this node's own (the directory, a full disk) is a nodeSideError,
// answered 500. The returned cleanup removes the file: the server runs it when the job ends, and on
// every refusal or drop before that.
func buildSTTUpload(cfg config.Config, payload json.RawMessage) (core.Request, func(), error) {
	noop := func() {}
	p, err := parseSTTUpload(payload)
	if err != nil {
		return core.Request{}, noop, fmt.Errorf("stt: payload: %w", err)
	}
	if len(bytes.TrimSpace(p.AudioB64)) == 0 {
		return core.Request{}, noop, errors.New("stt: audio_b64 required (the audio file's bytes, base64: nothing here reads a path on the caller's disk)")
	}
	ext := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(p.AudioExt)), ".")
	if ext == "" {
		ext = "audio"
	}
	if !sttExtRe.MatchString(ext) {
		return core.Request{}, noop, fmt.Errorf("stt: audio_ext %q must be 1 to 8 letters or digits (no path)", p.AudioExt)
	}
	if p.Language != "" && !sttLangRe.MatchString(p.Language) {
		return core.Request{}, noop, fmt.Errorf("stt: language %q must be auto or a language code such as en or es", p.Language)
	}
	if p.HQ && cfg.STTModelHQ == "" {
		return core.Request{}, noop, errors.New("stt: hq requested but this node has no stt_model_hq")
	}
	limit := cfg.EffectiveSTTUploadMaxBytes()
	// Estimate before decoding: base64 length x 3/4 bounds the decoded size from above.
	if est := int64(len(p.AudioB64)) * 3 / 4; est > limit+3 {
		return core.Request{}, noop, fmt.Errorf("stt: the audio is about %d bytes, cap %d (fleet_stt_upload_max_mb on this node)", est, limit)
	}
	raw := make([]byte, base64.StdEncoding.DecodedLen(len(p.AudioB64)))
	n, err := base64.StdEncoding.Decode(raw, p.AudioB64)
	if err != nil {
		return core.Request{}, noop, fmt.Errorf("stt: audio_b64 is not valid base64: %w", err)
	}
	raw = raw[:n]
	if len(raw) == 0 {
		return core.Request{}, noop, errors.New("stt: audio_b64 decodes to no bytes")
	}
	if int64(len(raw)) > limit {
		return core.Request{}, noop, fmt.Errorf("stt: the audio is %d bytes, cap %d (fleet_stt_upload_max_mb on this node)", len(raw), limit)
	}
	dir := sttUploadDir(cfg)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return core.Request{}, noop, nodeSideError{fmt.Errorf("stt: %w", err)}
	}
	f, err := os.CreateTemp(dir, "stt-*."+ext) // mode 0600: private to this node's account
	if err != nil {
		return core.Request{}, noop, nodeSideError{fmt.Errorf("stt: %w", err)}
	}
	path := f.Name()
	cleanup := func() {
		if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
			log.Printf("fleet: stt upload: removing %s: %v (the startup sweep retries it)", path, rerr)
		}
	}
	if _, werr := f.Write(raw); werr != nil {
		_ = f.Close()
		cleanup()
		return core.Request{}, noop, nodeSideError{fmt.Errorf("stt: writing the upload: %w", werr)}
	}
	if cerr := f.Close(); cerr != nil {
		cleanup()
		return core.Request{}, noop, nodeSideError{fmt.Errorf("stt: writing the upload: %w", cerr)}
	}
	params := map[string]any{}
	if p.Language != "" {
		params["language"] = p.Language
	}
	if p.HQ {
		params["hq"] = true
	}
	return core.Request{Task: core.TaskTranscribe, Audio: path, Params: params}, cleanup, nil
}

// sttJobData is what an stt upload job stores as its result: the FULL core.Result, defers included
// (see visionJobData), so the asker reads `deferred`, `reason`, `defer_class` and `meta` (model,
// latency, err_class) exactly as a local call returns them.
func sttJobData(res core.Result) (json.RawMessage, error) {
	b, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("stt: encoding result: %w", err)
	}
	return b, nil
}

// isSTTTask reports whether a task type runs against the whisper upstream: the legacy lane and the
// upload door, which share the node's stt concurrency cap.
func isSTTTask(taskType string) bool { return taskType == "stt" || taskType == STTUploadTask }

// SweepOrphanedSTTUploads removes upload files no job holds any more: fleet-serve calls it at
// startup, so a crash between the write and the cleanup never leaves audio on disk for good. Only
// files older than the longest transcription plus an hour go, so a second process sharing the media
// dir keeps the ones it is running. Every failure is collected and none stops the rest of the sweep.
func SweepOrphanedSTTUploads(cfg config.Config, now time.Time) (swept int, err error) {
	dir := sttUploadDir(cfg)
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return 0, nil
		}
		return 0, fmt.Errorf("sweep stt uploads: %w", rerr)
	}
	timeout := cfg.STTRequestTimeoutSec
	if timeout <= 0 {
		timeout = 1800
	}
	maxAge := time.Duration(timeout)*time.Second + time.Hour
	var failures []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "stt-") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			failures = append(failures, fmt.Errorf("sweep stt uploads: %s: %w", e.Name(), ierr))
			continue
		}
		if now.Sub(info.ModTime()) < maxAge {
			continue
		}
		if rmErr := os.Remove(filepath.Join(dir, e.Name())); rmErr != nil {
			failures = append(failures, fmt.Errorf("sweep stt uploads: %s: %w", e.Name(), rmErr))
			continue
		}
		swept++
	}
	return swept, errors.Join(failures...)
}

// sttGate is the node's stt concurrency cap (fleet_stt_max_concurrent) shared by the legacy lane and
// the upload door: a FIFO counting semaphore. A job over the cap waits its turn in arrival order and
// is never refused; a waiter whose context ends (the node is shutting down) leaves the line.
type sttGate struct {
	mu   sync.Mutex
	cap  int
	held int
	q    []chan struct{}
}

func newSTTGate(capacity int) *sttGate {
	if capacity < 1 {
		capacity = 1
	}
	return &sttGate{cap: capacity}
}

// acquire takes a slot, waiting its turn; the returned release (idempotent) hands the slot to the
// first waiter, or frees it.
func (g *sttGate) acquire(ctx context.Context) (release func(), err error) {
	g.mu.Lock()
	if g.held < g.cap && len(g.q) == 0 {
		g.held++
		g.mu.Unlock()
		return g.releaser(), nil
	}
	ch := make(chan struct{})
	g.q = append(g.q, ch)
	g.mu.Unlock()
	select {
	case <-ch: // the releasing job handed its slot over: held is unchanged
		return g.releaser(), nil
	case <-ctx.Done():
		g.mu.Lock()
		for i, c := range g.q {
			if c == ch {
				g.q = append(g.q[:i], g.q[i+1:]...)
				g.mu.Unlock()
				return nil, ctx.Err()
			}
		}
		g.mu.Unlock()
		// Not in the line any more: a release handed this waiter the slot as the context ended.
		// Pass it on rather than leak it.
		g.releaser()()
		return nil, ctx.Err()
	}
}

func (g *sttGate) releaser() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if len(g.q) > 0 {
				next := g.q[0]
				g.q = g.q[1:]
				close(next) // the slot passes to the first waiter
				return
			}
			g.held--
		})
	}
}

func (g *sttGate) waiting() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.q)
}

func (g *sttGate) holders() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held
}

// enterSTT is the one place a job of either stt lane takes its slot (both doors' run closures call
// it, so the cap holds for pushed and pulled jobs alike). A task type that is not stt takes no slot.
// While the job waits it is counted as queued on the whisper client, so the call ahead of it keeps
// the model loaded for it (sttclient.Queued): a burst held here pays one cold start, not one per job.
func (s *Server) enterSTT(ctx context.Context, taskType string) (release func(), err error) {
	if !isSTTTask(taskType) {
		return func() {}, nil
	}
	queued := sttclient.Queued()
	defer queued()
	return s.sttGate.acquire(ctx)
}
