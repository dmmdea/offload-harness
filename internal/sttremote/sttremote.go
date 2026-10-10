// Package sttremote is the CALLER side of the fleet stt upload door (ADR 0072): it decides where ONE
// transcription runs, this box's own whisper or a fleet node's, and drives the wire when the answer is
// a node. The node side is fleetnode's POST /fleet/stt.
//
// The route vocabulary is the vision lane's (internal/visionremote), with the same quality-first rule:
//
//   - local (the default, and ""): run in-process, exactly as before the route existed. The one
//     difference is on a refusal: a gpu_busy defer gains a hint that route auto or remote would let a
//     fleet node take the work, when delegate_remotes is configured.
//   - auto: run local unless the local whisper request would actually be held right now
//     (modelaffinity.WouldBlockUpstream: a lease fences the cards over the model AND the model is not
//     already resident), in which case a fleet node is tried, and with none usable the work still runs
//     local. Every node serves the same whisper family, so a spill costs no quality.
//   - remote: force a fleet node; with none eligible the call DEFERS (defer_class capacity, or config
//     when no remotes are configured at all), never a silent local run: "remote" is the caller saying
//     its own card must stay untouched.
//
// The audio is read HERE, on the box that has it, converted to 16 kHz mono Opus at 32 kbps (about 14 MB
// per hour of speech; the original file is sent when conversion is impossible and it fits the node's
// cap) and travels as base64 inside the job. The node's answer is the full core.Result it would have
// returned to a local caller; on success this box writes its OWN .srt, .txt and .segments.json under
// media_dir from the full segment list (fetching the node's copy through /fleet/media when the inline
// one was cut), so the result's paths are local files and nothing here refers to a path on the node.
// The result is stamped with the node id and the placement reason (Meta.Node / Meta.Placement); the
// route itself is on the asker's ledger row.
package sttremote

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/audioio"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
	"github.com/dmmdea/offload-harness/internal/sttclient"
)

// Route values. RouteLocal is the default: an empty route means local.
const (
	RouteLocal  = "local"
	RouteAuto   = "auto"
	RouteRemote = "remote"
)

// healthTimeout is how long one call waits for one node's /fleet/health: the single-shot lanes'
// shared bound, probed concurrently and cached (internal/rosterprobe). A var so a test compresses
// it; production never mutates it.
var healthTimeout = rosterprobe.DefaultTimeout

const (
	pollEvery   = 500 * time.Millisecond
	pollTimeout = 20 * time.Second
	maxBody     = 32 << 20
	// maxMediaBody bounds the node's .segments.json read: a long recording with word timestamps is tens
	// of MB.
	maxMediaBody = 256 << 20
	// maxPollFailures bounds consecutive failed polls before the call gives up on the node.
	maxPollFailures = 5

	// dispatchBase and dispatchPerMiB size the delivery of the upload: the 20 s the small lanes get
	// cannot carry 40 MB over a tailnet link.
	dispatchBase   = 30 * time.Second
	dispatchPerMiB = 4 * time.Second

	// budgetBase and budgetRealtime size the wait for the node's answer from the audio: a base for the
	// queue and a cold seat, plus half the audio's length (whisper decodes several times faster than
	// realtime; the cap below covers a slow card). stt_request_timeout_sec bounds it.
	budgetBase     = 180 * time.Second
	budgetRealtime = 0.5

	// opusBytesPerSec is 32 kbps: the duration of an Opus upload is read back from its size.
	opusBytesPerSec = 32000 / 8
)

// HTTPClient is the transport every request uses; tests swap it. It rides netguard.SafeTransport like
// every other fleet client: the lane may only ever reach loopback or the operator's tailnet
// (never-cloud, ADR 0001), enforced at dial time. It carries no client-level Timeout on purpose: a
// transcription is waited on for up to stt_request_timeout_sec, so every request bounds itself with
// its own context.
var HTTPClient = &http.Client{Transport: netguard.SafeTransport(nil), CheckRedirect: rosterprobe.NoRedirect}

// localBusy is the auto route's trigger: would the local whisper request be held at the upstream fence
// right now. It reads the same thing the request itself will meet (modelaffinity.WouldBlockUpstream),
// for the model the call will use, so a render holding other cards, or a whisper that is already
// resident, does not send audio off the box. A seam so tests drive both branches.
var localBusy = func(ctx context.Context, cfg config.Config, hq bool) bool {
	model := cfg.STTModel
	if hq && cfg.STTModelHQ != "" {
		model = cfg.STTModelHQ // the pipeline's own choice (sttRoute)
	}
	if model == "" {
		return false
	}
	return modelaffinity.WouldBlockUpstream(ctx, cfg.Endpoint, model)
}

// Runner runs one request in-process; *pipeline.Pipeline satisfies it.
type Runner interface {
	Run(ctx context.Context, req core.Request) core.Result
}

// NormalizeRoute maps the caller's route to one of the three values; ok is false for anything
// unrecognized. "" is local.
func NormalizeRoute(route string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(route)) {
	case "", RouteLocal:
		return RouteLocal, true
	case RouteAuto:
		return RouteAuto, true
	case RouteRemote:
		return RouteRemote, true
	}
	return "", false
}

// fleetHint ends the reason of a local gpu_busy defer when a fleet is configured (D19).
const fleetHint = ` (route "auto" or "remote" lets a fleet node transcribe this)`

// Run is THE entry point the MCP handler and the CLI verb share: it applies the route rule above to req
// and returns the result exactly as the pipeline would, plus Meta.Node / Meta.Placement when the route
// made a decision. A local run under the default route carries neither.
func Run(ctx context.Context, cfg config.Config, runner Runner, req core.Request, route string) core.Result {
	r, ok := NormalizeRoute(route)
	if !ok {
		res := core.Deferf(fmt.Sprintf("unrecognized route %q; use local, auto or remote", route), "", core.Meta{})
		res.DeferClass = core.DeferClassContract
		return res
	}
	switch r {
	case RouteLocal:
		res := runner.Run(ctx, req)
		// A held card on a box that has a fleet: say so. Only this defer, only the local route (auto
		// already tried the fleet), and the class, error class and everything else stay as they were.
		if res.Deferred && res.Meta.ErrClass == "gpu_busy" && hasRemotes(cfg) {
			res.Reason += fleetHint
		}
		return res
	case RouteRemote:
		// A call sent to a node is the remote lane's own: it writes its ledger row and, once a node is
		// chosen, its one PAIR card.
		h := core.BeginRemote(runner, req, r)
		defer core.CloseOnPanic(h) // a panic in the dispatch, the poll or the fetch closes the card before the door dies of it
		res, err := callWith(ctx, cfg, req, h)
		if err != nil {
			res = placementDefer(err, "remote: forced")
		} else {
			res.Meta.Placement = "remote: forced"
		}
		h.Finish(res)
		return res
	}
	// auto
	if !localBusy(ctx, cfg, paramBool(req.Params, "hq")) {
		res := runner.Run(ctx, req)
		res.Meta.Placement = "local: gpu idle"
		return res
	}
	h := core.BeginRemote(runner, req, r)
	defer core.CloseOnPanic(h) // as above, on the auto route
	res, err := callWith(ctx, cfg, req, h)
	if err == nil {
		res.Meta.Placement = "remote: local gpu busy"
		h.Finish(res)
		return res
	}
	// Busy local, no usable remote: queued-local beats ineligible-remote. The reason travels so a slow
	// call is attributable. The local run writes its own row; an attempt that never reached a node
	// leaves nothing behind, one that did leaves a failed card.
	h.Discard(err.Error())
	res = runner.Run(ctx, req)
	res.Meta.Placement = "local: gpu busy, " + err.Error()
	return res
}

func hasRemotes(cfg config.Config) bool {
	for _, b := range cfg.DelegateRemotes {
		if strings.TrimSpace(b) != "" {
			return true
		}
	}
	return false
}

// placementDefer renders a Call failure as the deferred result the route contract promises:
// deferred:true, a reason, and a defer_class a caller can branch on: capacity when the fleet has no
// eligible node right now, config when no remotes are configured, infrastructure when a node took the
// job and the wire or the node then failed.
func placementDefer(err error, placement string) core.Result {
	res := core.Deferf(err.Error(), "", core.Meta{Placement: placement})
	var pe *placementError
	if errors.As(err, &pe) {
		res.DeferClass = pe.class
	} else {
		res.DeferClass = core.DeferClassInfrastructure
	}
	return res
}

// placementError is a Call failure with the defer class it maps to.
type placementError struct {
	class string
	msg   string
}

func (e *placementError) Error() string { return e.msg }

func infra(format string, a ...any) *placementError {
	return &placementError{core.DeferClassInfrastructure, fmt.Sprintf(format, a...)}
}

// upload is the file that travels: the Opus conversion (path in a temp dir, removed by cleanup) or the
// original when conversion was impossible. Its size is known before a byte is read, so the node is
// picked against its cap first.
type upload struct {
	path        string
	ext         string
	size        int64
	durationSec float64 // estimated from the Opus size; 0 when unknown (an original)
	cleanup     func()
}

// prepareUpload converts the audio to Opus, or falls back to the original file.
func prepareUpload(cfg config.Config, audio string) (upload, error) {
	fi, err := os.Stat(audio)
	if err != nil {
		return upload{}, fmt.Errorf("audio %q: %w", audio, err)
	}
	if fi.IsDir() {
		return upload{}, fmt.Errorf("audio %q is a directory", audio)
	}
	if ogg, cleanup, cerr := audioio.ConvertToOpus16k(audio, cfg.FFmpegPath); cerr == nil {
		if ofi, serr := os.Stat(ogg); serr == nil {
			return upload{path: ogg, ext: "ogg", size: ofi.Size(), durationSec: float64(ofi.Size()) / opusBytesPerSec, cleanup: cleanup}, nil
		}
		cleanup()
	} else {
		log.Printf("sttremote: converting %q to Opus failed, sending the original file: %v", audio, cerr)
	}
	return upload{path: audio, ext: cleanExt(audio), size: fi.Size(), cleanup: func() {}}, nil
}

// cleanExt is the file's extension as the node's door accepts it: lowercase letters and digits, at
// most 8, else "audio".
func cleanExt(path string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	if ext == "" || len(ext) > 8 {
		return "audio"
	}
	for _, c := range ext {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return "audio"
		}
	}
	return ext
}

// budgetFor is how long a call waits for a node's answer: stt_request_timeout_sec when the audio's
// length is unknown, else a base plus half the audio's length, never more than stt_request_timeout_sec.
func budgetFor(cfg config.Config, durationSec float64) time.Duration {
	limit := time.Duration(cfg.STTRequestTimeoutSec) * time.Second
	if limit <= 0 {
		limit = 1800 * time.Second
	}
	if durationSec <= 0 {
		return limit
	}
	b := budgetBase + time.Duration(durationSec*budgetRealtime*float64(time.Second))
	if b > limit {
		return limit
	}
	return b
}

// dispatchTimeoutFor is how long delivering a body of n bytes may take.
func dispatchTimeoutFor(n int) time.Duration {
	return dispatchBase + time.Duration(n>>20)*dispatchPerMiB
}

// Call runs req on the best eligible fleet node.
func Call(ctx context.Context, cfg config.Config, req core.Request) (core.Result, error) {
	return callWith(ctx, cfg, req, core.NopAttribution{})
}

// callWith is Call reporting the dispatch and the node's progress to h (core.RemoteAttribution). A bad
// or missing audio file is a DEFERRED result, not an error (the shape the local convert produces); a
// placement or wire failure is an error the caller maps to a defer class; the node's own result,
// including its defers, comes back as the core.Result it produced.
func callWith(ctx context.Context, cfg config.Config, req core.Request, h core.RemoteAttribution) (core.Result, error) {
	start := time.Now()
	if !hasRemotes(cfg) {
		return core.Result{}, &placementError{core.DeferClassConfig, "no delegate_remotes configured — nothing to place the transcription on"}
	}
	up, err := prepareUpload(cfg, req.Audio)
	if err != nil {
		return core.Deferf("audio convert: "+err.Error(), "", core.Meta{}), nil
	}
	defer up.cleanup()
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budgetFor(cfg, up.durationSec))
		defer cancel()
	}
	hq := paramBool(req.Params, "hq")
	base, node, err := pickNode(ctx, cfg, hq, up.size)
	if err != nil {
		return core.Result{}, err
	}
	raw, err := os.ReadFile(up.path)
	if err != nil {
		return core.Deferf("audio convert: "+err.Error(), "", core.Meta{}), nil
	}
	jobID, err := dispatch(ctx, cfg, base, node, req, up.ext, raw, hq, h)
	if err != nil {
		return core.Result{}, err
	}
	res, err := wait(ctx, cfg, base, jobID, h)
	if err != nil {
		return core.Result{}, &placementError{core.DeferClassInfrastructure, fmt.Sprintf("node %s: %v", node, err)}
	}
	res.Meta.Node = node
	if res.Meta.LatencyMs == 0 {
		res.Meta.LatencyMs = time.Since(start).Milliseconds()
	}
	if res.OK && !res.Deferred {
		res = materialize(ctx, cfg, base, node, req, raw, res)
	}
	return res, nil
}

// pickNode probes delegate_remotes and hands the views to delegate.PlaceSTT. Every miss is named in
// the error so "no node" is never a mystery: an unreachable node, a node without the door (an older
// build lists only the legacy path-taking stt, which cannot be sent bytes), one without an hq model
// when hq was asked, one whose upload cap is smaller than the file, a leased card.
func pickNode(ctx context.Context, cfg config.Config, hq bool, size int64) (base, node string, err error) {
	var (
		bases  []string
		views  []delegate.NodeView
		misses []string
	)
	for _, r := range rosterprobe.Probe(ctx, cfg.DelegateRemotes, cfg.FleetAuthToken, healthTimeout) {
		b, v := r.Base, r.View
		if r.Err != nil {
			misses = append(misses, r.Miss())
			continue
		}
		switch {
		case !v.ServesSTTUpload():
			misses = append(misses, fmt.Sprintf("%s (%s): no stt upload door (an older build, or no whisper model; tasks %v)", r.Shown(), v.NodeID, v.Tasks))
			continue
		case hq && !v.ServesSTTUploadOf(true, 0):
			misses = append(misses, fmt.Sprintf("%s (%s): no hq whisper model", r.Shown(), v.NodeID))
			continue
		case !v.ServesSTTUploadOf(hq, size):
			misses = append(misses, fmt.Sprintf("%s (%s): takes uploads up to %d MiB, this one is %d bytes", r.Shown(), v.NodeID, nodeCapMB(v), size))
			continue
		case v.LeasedText || v.LeaseBusy:
			misses = append(misses, fmt.Sprintf("%s (%s): card leased", r.Shown(), v.NodeID))
			continue
		}
		bases = append(bases, b)
		views = append(views, v)
	}
	i, ok := delegate.PlaceSTT(views, hq, size)
	if !ok {
		return "", "", &placementError{core.DeferClassCapacity,
			"no fleet node is eligible for the stt upload door — probed " + strings.Join(misses, "; ")}
	}
	node = views[i].NodeID
	if node == "" {
		node = bases[i]
	}
	return bases[i], node, nil
}

func nodeCapMB(v delegate.NodeView) int {
	if v.STTUploadMaxMB > 0 {
		return v.STTUploadMaxMB
	}
	return 48
}

// payload is the POST /fleet/stt body (fleetnode.STTUploadPayload's shape).
type payload struct {
	JobID    string `json:"job_id"`
	AudioB64 string `json:"audio_b64"`
	AudioExt string `json:"audio_ext,omitempty"`
	Language string `json:"language,omitempty"`
	HQ       bool   `json:"hq,omitempty"`
}

func dispatch(ctx context.Context, cfg config.Config, base, node string, req core.Request, ext string, raw []byte, hq bool, h core.RemoteAttribution) (string, error) {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", infra("job id: %v", err)
	}
	p := payload{JobID: "stt-" + hex.EncodeToString(rnd[:]), AudioB64: base64.StdEncoding.EncodeToString(raw), AudioExt: ext, HQ: hq}
	// The asker's own language setting decides, as it does for a local call: the request's, else
	// stt_language. "auto" is sent as such so the node detects instead of applying its own default.
	if lang := paramStr(req.Params, "language"); lang != "" {
		p.Language = lang
	} else if cfg.STTLanguage != "" {
		p.Language = cfg.STTLanguage
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "", infra("encoding stt job: %v", err)
	}
	dctx, cancel := context.WithTimeout(ctx, dispatchTimeoutFor(len(body)))
	defer cancel()
	hreq, err := http.NewRequestWithContext(dctx, http.MethodPost, base+"/fleet/stt", bytes.NewReader(body))
	if err != nil {
		return "", infra("%v", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	auth(cfg, hreq)
	// Who asked, and whether the serving node must card the job because this box will not.
	pairworkloads.WireHeadersFor(cfg, hreq.Header)
	// The call's one PAIR card opens here, on the node about to receive the job.
	h.Dispatched(base, node, p.JobID)
	resp, err := HTTPClient.Do(hreq)
	if err != nil {
		return "", infra("dispatch %s: %v", base, err)
	}
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		// A refusal is the node's own verdict (queue full, leased, draining, 401, an upload over its
		// cap): capacity when it is re-placeable, infrastructure otherwise.
		class := core.DeferClassInfrastructure
		if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests {
			class = core.DeferClassCapacity
		}
		return "", &placementError{class, rosterprobe.Scrub(base, fmt.Errorf("dispatch %s: status %d: %s", base, resp.StatusCode, truncate(rb)))}
	}
	// The node accepted the job: whatever the negative cache holds against it is out of date.
	rosterprobe.Default.Forget(base)
	return p.JobID, nil
}

type jobWire struct {
	State string          `json:"state"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

// wait polls the job until it is done or errored. A done job's data is the node's full core.Result
// (fleetnode.sttJobData): defers ride inside it. An error state is the node's answer too (the run
// died, the node shut down): it comes back as a deferred result, never a transport error. A failed
// poll is retried; the caller's context, a 404 (the node denies holding the job) and
// maxPollFailures consecutive failures end the wait early.
func wait(ctx context.Context, cfg config.Config, base, jobID string, h core.RemoteAttribution) (core.Result, error) {
	failures := 0
	for {
		pctx, cancel := context.WithTimeout(ctx, pollTimeout)
		j, err := getJSON[jobWire](pctx, cfg, base+"/fleet/jobs/"+jobID)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return core.Result{}, fmt.Errorf("job %s: %w", jobID, ctx.Err())
			}
			if strings.Contains(err.Error(), "status 404") {
				return core.Result{}, fmt.Errorf("job %s: the node denies holding it (evicted or restarted): %w", jobID, err)
			}
			failures++
			if failures >= maxPollFailures {
				return core.Result{}, fmt.Errorf("job %s: %d consecutive poll failures, last: %w", jobID, failures, err)
			}
			select {
			case <-ctx.Done():
				return core.Result{}, fmt.Errorf("job %s: %w", jobID, ctx.Err())
			case <-time.After(pollEvery):
			}
			continue
		}
		failures = 0
		switch j.State {
		case "running":
			h.Running()
		case "done":
			var res core.Result
			if len(j.Data) == 0 || json.Unmarshal(j.Data, &res) != nil {
				res := core.Deferf("job done with an unreadable result: "+truncate(j.Data), "", core.Meta{})
				res.DeferClass = core.DeferClassInfrastructure
				return res, nil
			}
			return res, nil
		case "error":
			res := core.Deferf(j.Error, "", core.Meta{})
			res.DeferClass = core.DeferClassInfrastructure
			return res, nil
		}
		select {
		case <-ctx.Done():
			return core.Result{}, fmt.Errorf("job %s still %s: %w", jobID, j.State, ctx.Err())
		case <-time.After(pollEvery):
		}
	}
}

// nodeResult mirrors the transcribe result a node returns (pipeline's transcribeResult).
type nodeResult struct {
	Language          string              `json:"language"`
	DurationSec       float64             `json:"duration_sec"`
	NumSegments       int                 `json:"num_segments"`
	Gist              string              `json:"gist"`
	Segments          []sttclient.Segment `json:"segments"`
	SegmentsTruncated bool                `json:"segments_truncated"`
	SRTPath           string              `json:"srt_path"`
	TextPath          string              `json:"text_path"`
	JSONPath          string              `json:"json_path"`
}

func answerDefer(node, format string, a ...any) core.Result {
	res := core.Deferf(fmt.Sprintf("node %s: ", node)+fmt.Sprintf(format, a...), "", core.Meta{})
	res.DeferClass = core.DeferClassInfrastructure
	return res
}

// materialize turns the node's successful answer into this box's: the full segment list (fetched from
// the node when its inline copy was cut) is validated, the .srt, .txt and .segments.json are written
// under media_dir, and the result's paths and inline segments follow THIS box's config, exactly as a
// local call's do. A node whose answer cannot be believed or completed is a deferred result naming it.
func materialize(ctx context.Context, cfg config.Config, base, node string, req core.Request, raw []byte, res core.Result) core.Result {
	var nr nodeResult
	if err := json.Unmarshal(res.Data, &nr); err != nil {
		return answerDefer(node, "the transcript is not readable: %v", err)
	}
	segs := nr.Segments
	if nr.SegmentsTruncated || nr.NumSegments > len(segs) {
		name := nodeFileBase(nr.JSONPath)
		if name == "" {
			return answerDefer(node, "the transcript was cut at %d of %d segments and the node named no segment file to fetch the rest from", len(segs), nr.NumSegments)
		}
		full, err := fetchSegments(ctx, cfg, base, name)
		if err != nil {
			return answerDefer(node, "the transcript was cut at %d of %d segments and its full segment list could not be fetched: %v", len(segs), nr.NumSegments, err)
		}
		if nr.NumSegments != 0 && len(full) != nr.NumSegments {
			return answerDefer(node, "its segment file holds %d segments but the result says %d", len(full), nr.NumSegments)
		}
		segs = full
	}
	if msg := checkSegments(segs); msg != "" {
		return answerDefer(node, "the answer failed validation: %s", msg)
	}

	var texts []string
	for _, s := range segs {
		if t := strings.TrimSpace(s.Text); t != "" {
			texts = append(texts, t)
		}
	}
	full := strings.Join(texts, " ")
	stem := outputBase(cfg.MediaDir, req.Audio, raw, res.Meta.Model)
	srtPath, txtPath, jsonPath := stem+".srt", stem+".txt", stem+".segments.json"
	_ = os.MkdirAll(filepath.Dir(stem), 0o755)
	srtPath = writeOutput(srtPath, []byte(sttclient.SRT(segs)))
	txtPath = writeOutput(txtPath, []byte(full))
	if sj, err := json.MarshalIndent(segs, "", "  "); err == nil {
		jsonPath = writeOutput(jsonPath, sj)
	} else {
		jsonPath = ""
	}

	inline, truncated := segs, false
	if cfg.STTMaxInlineSegments > 0 && len(segs) > cfg.STTMaxInlineSegments {
		inline, truncated = segs[:cfg.STTMaxInlineSegments], true
	}
	gist := nr.Gist
	if gist == "" {
		gist = preview(full, 400)
	}
	out := nodeResult{
		Language: nr.Language, DurationSec: nr.DurationSec, NumSegments: len(segs), Gist: gist,
		Segments: inline, SegmentsTruncated: truncated, SRTPath: srtPath, TextPath: txtPath, JSONPath: jsonPath,
	}
	data, err := json.Marshal(out)
	if err != nil {
		return answerDefer(node, "encoding the transcript: %v", err)
	}
	res.Data = data
	return res
}

// writeOutput writes one output file best-effort, like the local pipeline (the inline data still
// carries the answer): it returns the path, or "" when the write failed, so a result never points at a
// file that is not there.
func writeOutput(path string, b []byte) string {
	if err := os.WriteFile(path, b, 0o644); err != nil {
		log.Printf("sttremote: writing %s: %v", path, err)
		return ""
	}
	return path
}

// checkSegments judges the shape of a node's transcript, as textremote does for a classification: at
// least one segment, each ending no earlier than it starts, starts never running backwards.
func checkSegments(segs []sttclient.Segment) string {
	if len(segs) == 0 {
		return "no segments"
	}
	for i, s := range segs {
		if s.End < s.Start {
			return fmt.Sprintf("segment %d ends (%.2fs) before it starts (%.2fs)", i, s.End, s.Start)
		}
		if i > 0 && s.Start < segs[i-1].Start {
			return fmt.Sprintf("segment %d starts (%.2fs) before segment %d does (%.2fs)", i, s.Start, i-1, segs[i-1].Start)
		}
	}
	return ""
}

// fetchSegments reads the node's .segments.json (the full list) from its media route by bare name.
func fetchSegments(ctx context.Context, cfg config.Config, base, name string) ([]sttclient.Segment, error) {
	fctx, cancel := context.WithTimeout(ctx, pollTimeout*3)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, base+"/fleet/media/"+name, nil)
	if err != nil {
		return nil, err
	}
	auth(cfg, req)
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMediaBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /fleet/media/%s: status %d: %s", name, resp.StatusCode, truncate(body))
	}
	var segs []sttclient.Segment
	if err := json.Unmarshal(body, &segs); err != nil {
		return nil, fmt.Errorf("GET /fleet/media/%s: not a segment list: %w", name, err)
	}
	return segs, nil
}

// nodeFileBase is the bare file name of a path on the node, whatever separators its OS spells ("" when
// the path ends in one). Only this ever reaches the node's media route.
func nodeFileBase(p string) string {
	i := strings.LastIndexAny(p, `/\`)
	return p[i+1:]
}

// outputBase is <media_dir>/<sanitized source stem>-<8 hex of the recording's content and model>, the
// shape the local pipeline names its outputs: two recordings that share a name do not overwrite each
// other's files, and the same recording keeps one stem.
func outputBase(mediaDir, audioPath string, raw []byte, model string) string {
	name := nodeFileBase(audioPath)
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		name = name[:i]
	}
	name = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." {
		name = "transcript"
	}
	sum := sha256.Sum256(append(append([]byte{}, raw...), []byte("|model="+model)...))
	return filepath.Join(mediaDir, name+"-"+hex.EncodeToString(sum[:])[:8])
}

// preview returns roughly the first n bytes of s at a word boundary, with an ellipsis when cut; the
// node's gist is used when it sent one.
func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	for len(cut) > 0 && !validUTF8(cut) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut) + "…"
}

func validUTF8(s string) bool { return strings.ToValidUTF8(s, "") == s }

func paramBool(p map[string]any, k string) bool {
	b, _ := p[k].(bool)
	return b
}

func paramStr(p map[string]any, k string) string {
	s, _ := p[k].(string)
	return strings.TrimSpace(s)
}

func getJSON[T any](ctx context.Context, cfg config.Config, url string) (T, error) {
	var zero T
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return zero, err
	}
	auth(cfg, req)
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return zero, err
	}
	if resp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, truncate(body))
	}
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return zero, fmt.Errorf("GET %s: not JSON: %w", url, err)
	}
	return v, nil
}

func auth(cfg config.Config, req *http.Request) {
	if cfg.FleetAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.FleetAuthToken)
	}
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 256 {
		return s[:256] + "…"
	}
	return s
}
