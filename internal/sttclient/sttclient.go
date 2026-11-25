// Package sttclient calls a whisper.cpp whisper-server (over llama-swap's
// /upstream/<model>/inference passthrough) to transcribe a 16 kHz mono WAV. It
// requests response_format=verbose_json so the reply carries per-segment
// timestamps (the {gist, segments[]} citation pattern). Audio bytes go straight
// to whisper-server and NEVER touch the text Gemma cascade. Pure net/http +
// mime/multipart; no cgo, no cloud.
package sttclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/seatwait"
	"github.com/dmmdea/offload-harness/internal/swapclient"
	"llamaswap-pp-cli/pkg/llamaswap"
)

// inferMu serializes every inference POST to the whisper upstream. whisper-server is
// SINGLE-SLOT: two overlapping /inference requests crash it (SIGSEGV → the harness
// sees an empty-body 502 and llama-swap cold-restarts it, ~60s of refusals). This
// mutex is process-global (the single server is a single shared resource, regardless
// of how many Client values exist), so it holds even across concurrent callers.
var inferMu sync.Mutex

// sttCalls counts the transcriptions in this process that are waiting for the whisper
// upstream or running on it (register C-91). Transcribe and TranscribeOAI count themselves
// in on their first line — before the fenced URL wait and the queue on inferMu — and out
// when they return, so a call is "in line" for the whole of its wait. UnloadIfIdle reads
// it. Process-wide for the reason inferMu is: the one upstream is one shared resource
// however many Client values exist.
var sttCalls atomic.Int64

// warmed records, per llama-swap base, the models a request has gone out to since the last
// unload this process sent for them (register C-91). UnloadIfIdle unloads exactly these:
// every model of its base and nothing else. That is what makes "one unload per burst" true
// when several finishing calls reach it at once, what lets the last call out free a model an
// earlier call of a mixed burst warmed (stt_model and stt_model_hq share the one slot), and
// what keeps a call that never reached the upstream (the card was fenced, the wav would not
// read) from unloading anything. Guarded by inferMu.
var warmed = map[string]map[string]bool{}

// sttQueued counts jobs a caller holds back BEFORE they reach the line (Queued): a fleet node's
// stt gate (fleet_stt_max_concurrent) keeps the jobs over its cap out of the pipeline until a
// slot frees. They will transcribe next, so UnloadIfIdle reads them like calls in line.
var sttQueued atomic.Int64

// Queued notes that one more transcription is waiting behind the caller's own gate and will use the
// upstream as soon as the gate lets it through; the returned func (idempotent) takes it back, and the
// caller calls it once the job has joined the line (or been abandoned). While any is outstanding,
// UnloadIfIdle leaves the model loaded, so a burst held at a node's gate pays one cold start, not one
// per job (register C-91, D18).
func Queued() func() {
	sttQueued.Add(1)
	var once sync.Once
	return func() { once.Do(func() { sttQueued.Add(-1) }) }
}

// QueuedNow reports how many jobs are held back by a Queued gate right now, so a test can wait until
// one is in line instead of sleeping.
func QueuedNow() int { return int(sttQueued.Load()) }

// Pending reports how many transcriptions in this process are waiting for the whisper
// upstream or running on it (register C-91): the count UnloadIfIdle reads. It exists so a
// test can wait until a second call is in line instead of sleeping and hoping.
func Pending() int { return int(sttCalls.Load()) }

// joinLine counts a call into the line (sttCalls, which UnloadIfIdle reads); the returned func counts it out.
func joinLine() (leave func()) {
	sttCalls.Add(1)
	return func() { sttCalls.Add(-1) }
}

// ErrUpstreamNoSpeech is the verdict "the audio had no speech": Transcribe and TranscribeOAI return it, wrapped, in
// place of an empty Result when the upstream ANSWERED, successfully, with an empty transcript, and for nothing else.
// A failure to get an answer says nothing about the audio. It was read as if it did twice: until 2026-10-01 every
// empty-body 5xx was no speech, and until 2026-11-25 one in a call that had run alone in its process still was,
// because whisper-server died on audio without speech (F-35). An unload by another process looks exactly like that: on
// a real run a clip full of speech whose model was taken away came back as silent. What whisper-server died of is
// avoided in the request now (buildMultipart, no_language_probabilities), so audio without speech is answered, 200 and
// empty, and a lost answer is only ever that. Callers treat this as "no speech found", not as an infrastructure
// failure, and have nothing to retry.
var ErrUpstreamNoSpeech = errors.New("stt upstream: no speech found")

// ErrUpstreamVanished marks a call whose upstream went away under it every time it was sent (exchange): the answer was
// an empty-body 5xx (llama-swap's bare answer when its connection to the model drops) or was cut off mid-read. An
// unload, a swap, a restart and a crash all look like that, so it is a failed call, which a caller can retry, and never
// a verdict about the audio: tell it from ErrUpstreamNoSpeech with errors.Is.
var ErrUpstreamVanished = errors.New("stt upstream: the upstream vanished mid-request")

// Word is one timestamped word with whisper's per-word confidence. whisper-server
// emits words[] in verbose_json BY DEFAULT (no extra request field needed — adding
// token_timestamps can segfault this build); the dataset/segmentation tooling uses
// these to cut only at whole-word boundaries and to drop low-confidence stumbles.
type Word struct {
	Word        string  `json:"word"`
	Start       float64 `json:"start"`
	End         float64 `json:"end"`
	Probability float64 `json:"probability"`
}

// Segment is one timestamped span of the transcript (start/end in seconds), plus the
// per-word array and decode-confidence fields whisper-server already returns. These
// flow through to the on-disk .segments.json (pipeline writes the full tr.Segments)
// for downstream consumers that need word-accurate timing and per-word confidence.
type Segment struct {
	ID           int     `json:"id"`
	Start        float64 `json:"start"`
	End          float64 `json:"end"`
	Text         string  `json:"text"`
	Words        []Word  `json:"words"`
	AvgLogprob   float64 `json:"avg_logprob"`
	NoSpeechProb float64 `json:"no_speech_prob"`
}

// Result is the parsed whisper-server verbose_json response.
type Result struct {
	Language string    `json:"language"`
	Duration float64   `json:"duration"`
	Text     string    `json:"text"`
	Segments []Segment `json:"segments"`
	// WordsReliable is the self-check of the word timestamps (D-38, issue #169): false when the FIRST
	// word of some segment starts wordDriftThresholdSec or more away from that segment's start, true
	// when the first word of every segment that carries words starts within it, nil (the key is
	// absent) when no segment carries words. True is a statement about those first words only: a
	// later word that sits far outside its segment still reads true (words[0] is the issue's own
	// formula, and a stricter bound on every word waits for a measurement of honest audio). It flags
	// the symptom and names no cause: whisper-server was seen to hand back word times lagging their
	// segment by minutes on a long file, and a consumer that cuts audio at whole-word boundaries needs
	// to know not to trust them. Set by Transcribe from checkWordDrift; pipeline and sttremote publish
	// it in the transcribe payload.
	WordsReliable *bool `json:"words_reliable,omitempty"`
	// WordsDriftSec is the largest |segment.start - words[0].start| over the segments that carry
	// words, in seconds: the number behind WordsReliable. Zero (and absent) when WordsReliable is nil
	// or the words agree to the millisecond.
	WordsDriftSec float64 `json:"words_drift_sec,omitempty"`
}

// wordDriftThresholdSec is the drift at which the word times stop being believed: the issue's own
// ask (#169), 0.5 s. A segment's start and its first word's start are both whisper's estimates and
// differ by a few hundred milliseconds of leading silence on honest audio; the failure this exists
// for is off by tens of seconds.
const wordDriftThresholdSec = 0.5

// checkWordDrift is the pure self-check behind Result.WordsReliable: the largest
// |segment.start - words[0].start| over the segments that carry words, and whether it stays
// below threshold. It returns (nil, 0) when no segment carries words, so a transcript with no
// word array (the OpenAI path, a model that sends none) gets no verdict.
func checkWordDrift(segs []Segment, threshold float64) (reliable *bool, driftSec float64) {
	judged := false
	for _, s := range segs {
		if len(s.Words) == 0 {
			continue
		}
		judged = true
		if d := math.Abs(s.Start - s.Words[0].Start); d > driftSec {
			driftSec = d
		}
	}
	if !judged {
		return nil, 0
	}
	ok := driftSec < threshold
	return &ok, driftSec
}

// Params are the per-request decode knobs whisper-server accepts as form fields.
// Defaults (DefaultParams) are the research-tuned profile for noisy EN/ES field
// audio; Language is set by the caller per call.
type Params struct {
	Language      string  // "" => auto-detect (sent as language=auto)
	BeamSize      int     // -bs; default greedy in whisper, so 5 is set explicitly
	BestOf        int     // -bo; candidates for temperature fallback
	MaxContext    int     // -mc; condition-on-previous-text cap (kills repetition loops)
	EntropyThold  float64 // -et; raise to fire the repetition detector sooner
	NoSpeechThold float64 // -nth
	Temperature   float64 // base decode temperature (fallback steps up from here)
	VAD           bool    // enable Silero VAD (server loaded with -vm)
	VADThreshold  float64 // -vt
	MaxLen        int     // -ml; subtitle-sized segments (chars)
	SplitOnWord   bool    // -sow; split at word boundaries for clean SRT
	// VADOff sends vad=false with the request, so a seat started with --vad decodes this one call without the voice
	// activity detector (whisper-server reads a per-request vad field over its own flag). Only meaningful with VAD false;
	// without it a VAD-less call omits the field and the server's own setting stands.
	VADOff bool
	// NoFallback sends temperature_inc=0: the decode runs at Temperature alone, with no retry at a higher one when a
	// segment trips the entropy or log-probability check. whisper-server's own default steps up by 0.2, which makes a
	// hard segment's text depend on the retry that happened to fire.
	NoFallback bool
}

// DefaultParams returns the tuned profile for noisy EN/ES field audio.
func DefaultParams() Params {
	return Params{
		Language:      "",
		BeamSize:      5,
		BestOf:        5,
		MaxContext:    64,
		EntropyThold:  2.8,
		NoSpeechThold: 0.6,
		Temperature:   0,
		VAD:           true,
		VADThreshold:  0.5,
		MaxLen:        60,
		SplitOnWord:   true,
	}
}

// ResponseFormat is fixed: verbose_json is the ONLY format that returns segments.
func (Params) ResponseFormat() string { return "verbose_json" }

// Client posts audio to whisper-server through llama-swap on base (e.g.
// http://127.0.0.1:11436). Every inference is serialized by the process-global
// inferMu (whisper-server is single-slot and crashes on overlapping requests).
type Client struct {
	base string
	http *http.Client
	// fenceWait is the budget for waiting out a fenced card when fenceWaitSet (register
	// C-89); see WithFenceWait. A client that never set one waits its HTTP timeout, as it
	// did before the budget existed.
	fenceWait    time.Duration
	fenceWaitSet bool
}

// fenceBudget is how long a transcription waits for a fenced card when the
// client carries no timeout of its own.
const fenceBudget = 120 * time.Second

// fenceWaitLeft is how long one transcription may wait at the GPU-lease fence in all. The passthrough STARTS the
// whisper model when it is not loaded, so under a render or an exclusive hold the request waits for the card
// (2026-09-22): the fence-wait budget (WithFenceWait; the pipeline hands it gpu_wait_ms, the bound every GPU door
// shares), never longer than the client's own timeout, then ctx. On exhaustion the fence's *modelaffinity.LeaseError
// comes back, which the pipeline defers as capacity ("gpu busy"). A resident whisper model is served at once, unless
// a holder is clearing it off its cards (gpulease.Meta.Clearing).
func (c *Client) fenceWaitLeft() time.Duration {
	budget := c.http.Timeout
	if budget <= 0 {
		budget = fenceBudget
	}
	if c.fenceWaitSet && c.fenceWait < budget {
		budget = c.fenceWait
	}
	return budget
}

// reply is what one transcription got back from the upstream.
type reply struct {
	status int
	body   []byte
	cut    error  // the read that was cut off mid-answer; nil for a whole answer
	resent string // how often the call was sent again ("" when it was not), for the error it ends with
}

// silent reports a bare 5xx: llama-swap's answer when its connection to the model drops.
func (r reply) silent() bool { return r.status >= 500 && len(bytes.TrimSpace(r.body)) == 0 }

// errorText is the start of an answer that is not a transcript.
func (r reply) errorText() string {
	if r.status == http.StatusOK {
		return ""
	}
	return strings.TrimSpace(string(r.body[:min(len(r.body), 400)]))
}

// lost reports an answer that never came because the model went away under the call.
func (r reply) lost() bool { return r.cut != nil || seatwait.TakenAway(r.status, r.errorText()) }

// maxErrorBody bounds what is read back of an answer that is not a transcript.
const maxErrorBody = 4096

// exchange sends one transcription and returns the upstream's reply: the part the two protocols share. It waits at the
// fence, serializes against the single-slot server (inferMu: overlapping requests crash whisper-server; it is held
// until the answer is read, so a second caller waits for the connection to fully drain), and marks the call where
// other processes can see it (modelaffinity.Call). A call whose model went away under it is sent again: the audio is on
// disk, the request is the same, and llama-swap loads the model back. It waits at the fence like a new call, so a lease
// that took the model is waited for, and it is bounded by the call's own timeout (seatwait.Resend).
func (c *Client) exchange(ctx context.Context, model, path, contentType string, body []byte) (reply, error) {
	call := modelaffinity.Call{Kind: gpuactivity.KindSTT, Endpoint: c.base, Model: model, Path: path, Fence: c.fenceWaitLeft(), Slot: &inferMu}
	again := seatwait.NewResend(c.http.Timeout)
	for {
		var rep reply
		var sentAt time.Time
		rctx, cancel := again.Bound(ctx)
		err := call.Run(rctx, func(url string) error {
			req, err := http.NewRequestWithContext(rctx, http.MethodPost, url, bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", contentType)
			c.markWarm(model)
			sentAt = time.Now()
			resp, err := c.http.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			rep.status = resp.StatusCode
			var answer io.Reader = resp.Body
			if resp.StatusCode != http.StatusOK {
				answer = io.LimitReader(resp.Body, maxErrorBody)
			}
			rep.body, rep.cut = io.ReadAll(answer)
			return nil
		})
		cancel()
		if err != nil {
			return reply{}, again.Err(err)
		}
		if !rep.lost() || !again.Again(ctx, sentAt) {
			rep.resent = again.Note()
			return rep, nil
		}
		log.Printf("sttclient: %s went away under a transcription (HTTP %d %s); sending it again", model, rep.status, rep.errorText())
	}
}

// New builds a client. timeout bounds one transcription (long audio).
func New(base string, timeout time.Duration) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: timeout},
	}
}

// WithFenceWait sets how long a transcription waits for a card a render holds (register
// C-89) and returns the client, so it chains at construction:
// New(...).WithFenceWait(p.gpuWait()).
//
// The wait used to BE the client's timeout, which for a transcription is
// stt_request_timeout_sec (1,800 s: long audio decodes at 5-8x realtime) — the same limit
// at which the MCP client aborts an idle call, so a held card made the two race, while
// every other GPU door gave up after gpu_wait_ms (90 s by default). The fence wait is its
// own budget now, and the client's timeout only CAPS it (min of the two), because a wait
// cannot outlast the request it is part of.
//
// Zero is a value, not "unset": gpu_wait_ms 0 means a single try for every GPU door, and
// a zero here is one inspection of the lease. Only a client that never called this waits
// its HTTP timeout, as before.
func (c *Client) WithFenceWait(d time.Duration) *Client {
	c.fenceWait, c.fenceWaitSet = d, true
	return c
}

// Transcribe uploads wavPath to the whisper upstream `model` and returns the
// parsed verbose_json. The multipart body is built in memory (wavs are 16 kHz
// mono — ~2 MB/min — comfortable in 64 GB RAM).
func (c *Client) Transcribe(ctx context.Context, model, wavPath string, p Params) (Result, error) {
	// In line from the first statement (register C-91): the call counts as waiting for the
	// whole of its fence wait and its queue on inferMu, which is what UnloadIfIdle reads.
	defer joinLine()()
	wav, err := os.ReadFile(wavPath)
	if err != nil {
		return Result{}, fmt.Errorf("sttclient: read wav %q: %w", wavPath, err)
	}
	body, contentType, err := buildMultipart(wav, filepath.Base(wavPath), p)
	if err != nil {
		return Result{}, fmt.Errorf("sttclient: build form: %w", err)
	}
	// Target the subpath directly; the bare /upstream/<model> form 301-redirects.
	rep, err := c.exchange(ctx, model, "/inference", contentType, body.Bytes())
	if err != nil {
		return Result{}, err
	}
	if rep.status != http.StatusOK {
		// NO ANSWER IS NOT NO SPEECH (register C-91, ADR 0104): an empty-body 5xx and a read that was cut off are
		// what a call gets when its model is unloaded, swapped, restarted or crashes under it, and they say nothing
		// about the audio (see ErrUpstreamNoSpeech). exchange has sent the call again by now.
		switch {
		case rep.cut != nil:
			return Result{}, fmt.Errorf("%w: whisper-server %d: the answer was cut off mid-read (%w): the upstream was stopped or restarted under the call%s", ErrUpstreamVanished, rep.status, rep.cut, rep.resent)
		case rep.silent():
			return Result{}, fmt.Errorf("%w: whisper-server %d (empty body): it was unloaded, swapped, restarted or crashed under the call%s", ErrUpstreamVanished, rep.status, rep.resent)
		}
		return Result{}, fmt.Errorf("whisper-server %d: %s%s", rep.status, rep.errorText(), rep.resent)
	}
	var out Result
	if err := decodeAnswer(rep, &out); err != nil {
		// An answer cut off mid-read is a vanished upstream whatever its status, as above.
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return Result{}, fmt.Errorf("%w: whisper-server %d: the answer was cut off mid-read (%w): the upstream was stopped or restarted under the call%s", ErrUpstreamVanished, rep.status, err, rep.resent)
		}
		return Result{}, fmt.Errorf("sttclient: decode verbose_json: %w", err)
	}
	// The one producer of the no-speech verdict: the upstream answered 200 and heard nothing.
	if strings.TrimSpace(out.Text) == "" && len(out.Segments) == 0 {
		return Result{}, fmt.Errorf("whisper-server answered with an empty transcript: %w", ErrUpstreamNoSpeech)
	}
	// The words[] the upstream returned are checked against the segments they belong to (issue #169):
	// a transcript whose word times lag or lead their segment says words_reliable:false, and the
	// number behind it. Set here, after the decode, because the upstream never sends these keys.
	out.WordsReliable, out.WordsDriftSec = checkWordDrift(out.Segments, wordDriftThresholdSec)
	return out, nil
}

// decodeAnswer decodes a 200 answer. The error that cut the read off comes first: what arrived before it is not the answer.
func decodeAnswer(rep reply, out *Result) error {
	if rep.cut != nil {
		return rep.cut
	}
	return json.NewDecoder(bytes.NewReader(rep.body)).Decode(out)
}

// buildMultipart assembles the multipart/form-data body: the wav under "file"
// plus the decode params. An empty Language is sent as "auto" (omitting it would
// fall back to whisper-server's -l default of "en").
func buildMultipart(wav []byte, filename string, p Params) (*bytes.Buffer, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(wav); err != nil {
		return nil, "", err
	}
	// whisper-server's -l default is "en", so OMITTING language forces English and
	// would mis-transcribe Spanish. Send "auto" explicitly for "no preference".
	lang := p.Language
	if lang == "" {
		lang = "auto"
	}
	fields := map[string]string{
		"response_format": p.ResponseFormat(),
		"language":        lang,
		"beam_size":       strconv.Itoa(p.BeamSize),
		"best_of":         strconv.Itoa(p.BestOf),
		"max_context":     strconv.Itoa(p.MaxContext),
		"entropy_thold":   strconv.FormatFloat(p.EntropyThold, 'g', -1, 64),
		"no_speech_thold": strconv.FormatFloat(p.NoSpeechThold, 'g', -1, 64),
		"temperature":     strconv.FormatFloat(p.Temperature, 'g', -1, 64),
		"max_len":         strconv.Itoa(p.MaxLen),
	}
	if p.VAD {
		fields["vad"] = "true"
		fields["vad_threshold"] = strconv.FormatFloat(p.VADThreshold, 'g', -1, 64)
	} else if p.VADOff {
		fields["vad"] = "false"
	}
	if p.NoFallback {
		fields["temperature_inc"] = "0"
	}
	if p.SplitOnWord {
		fields["split_on_word"] = "true"
	}
	// verbose_json makes whisper-server detect the language a SECOND time after the decode, only to list a
	// probability per language, which nothing here reads. On audio its VAD found no speech in, that detection has no
	// audio: it fails, the server prints a null language name, and the request dies (2026-11-25, read in whisper.cpp
	// 1.9.5: libstdc++ answers 500 "basic_string: construction from null is not valid"; the Windows build reads the null
	// and exits, which is F-35). Without the list, silent audio is answered 200 with an empty transcript, and a
	// transcript with speech is the same. A build that does not know the field ignores it.
	fields["no_language_probabilities"] = "true"
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return &buf, mw.FormDataContentType(), nil
}

// Unload force-frees the whisper upstream's VRAM immediately (zero-always-warm),
// rather than waiting for the ttl:300 idle timer. Best-effort: any error is the
// caller's to ignore.
//
// Routed through pkg/llamaswap rather than posting /api/models/unload/{model}
// raw, which buys two guards this call site never had: the name is resolved
// through the roster first (the harness binds ALIASES — `whisper`, `stt` — and
// only the canonical id is guaranteed to key the unload route), and a seat the
// llama-swap config marks resident is REFUSED instead of taken down.
//
// Drain is deliberately off: the pre-existing behavior was an unconditional
// unload. Unload itself holds NOTHING — the single-slot inference mutex is released
// when Transcribe returns, so a caller that fires it right after a call lands it on
// whatever the next call has in flight (register C-91; this comment used to say the
// caller holds the mutex, which no caller did). The zero-always-warm caller uses
// UnloadIfIdle, which unloads holding that mutex and only when nothing is in line,
// so there is nothing in flight to drain. Passing UnloadOpts{Drain: true} is the
// one-line change if a future caller unloads a seat it does not own.
func (c *Client) Unload(ctx context.Context, model string) error {
	ls, err := swapclient.New(c.base, c.http.Timeout)
	if err != nil {
		return err
	}
	_, err = ls.Unload(ctx, model, &llamaswap.UnloadOpts{})
	return err
}

// markWarm records that a request has gone out to the upstream for model. Callers hold inferMu.
func (c *Client) markWarm(model string) {
	m := warmed[c.base]
	if m == nil {
		m = map[string]bool{}
		warmed[c.base] = m
	}
	m[model] = true
}

// warmedModels lists, in name order, the models warmed on this client's base. Callers hold
// inferMu.
func (c *Client) warmedModels() []string {
	return slices.Sorted(maps.Keys(warmed[c.base]))
}

// UnloadIfIdle is Unload for the zero-always-warm caller (register C-91): it frees the
// upstream only when this is the last transcription of a burst, only once per burst, and then
// frees every model the burst warmed.
//
// The caller used to unload after EVERY call, holding nothing: inferMu is released when
// Transcribe returns, so a call that finished while others were in line sent its unload
// while the next one's inference was on the upstream, and llama-swap answered that call
// "matrix: model unloaded" with a 500 (or cut its read off). The calls queued behind it paid
// a cold start each, whatever the order. Here the unload goes out holding inferMu, which
// keeps the next inference off the upstream until it is done, and only when nothing is
// in line:
//
//   - another call is waiting or running (sttCalls): that call is the last one out and
//     unloads then, so a burst of N pays one cold start, not N;
//   - no model was used since the last unload (warmed): a call of the same burst already
//     freed it, or this call never reached the upstream (the card was fenced, the wav would
//     not read).
//
// A model another PROCESS has a transcription on is left to that process (2026-11-25): its call is marked in the
// run registry (modelaffinity.Call) and frees the model when it ends.
//
// What it frees is every model warmed on this client's base, not the model of the call that
// happens to be last out. A burst can mix stt_model with stt_model_hq, or the two protocols
// on two models; they share the one slot, so they are one burst, and the call that finishes
// last knows only its own model. Unloading just that one left the first finisher's model
// loaded until llama-swap's ttl, with nothing logged to say so. A call that never reached the
// upstream warmed nothing, so on its own it still unloads nothing; as the last one out it
// frees what the calls before it warmed.
//
// Each model is unloaded on its own. One that fails stays marked warm, so the next call out
// retries it, and it does not stop the others; every failure is returned, joined, each naming
// its model, and the ttl is the backstop. The caller logs the error and does not fail the
// transcription for it.
//
// It never BLOCKS on inferMu, because a finished call must not hold its own answer back
// for the length of the next call's inference: a 30-minute transcription would delay a
// ten-second one's result by 30 minutes. That is safe because a failed TryLock always means
// somebody else has the unload covered: the holder is a call that came in after the check
// above (it is the last one out), or another UnloadIfIdle that has already seen an empty
// line and will unload what was used. The decision is taken on sttCalls BEFORE the lock, so
// a call that only looked and went away never holds the slot against another.
//
// The last call out unloads even when it failed: the pipeline calls this after every call,
// so a burst whose final call was refused by the fence still frees the models its earlier
// calls warmed.
func (c *Client) UnloadIfIdle(ctx context.Context) error {
	if sttCalls.Load() > 0 || sttQueued.Load() > 0 {
		return nil
	}
	if !inferMu.TryLock() {
		return nil
	}
	defer inferMu.Unlock()
	if sttCalls.Load() > 0 || sttQueued.Load() > 0 {
		return nil
	}
	var errs []error
	for _, model := range c.warmedModels() {
		if modelaffinity.CallsElsewhere(gpuactivity.KindSTT, model) > 0 {
			// Another process's transcription is on this model right now. It warmed the model too, so it is the last
			// one out and unloads it then (or the ttl does); unloading it here would cut that call.
			delete(warmed[c.base], model)
			continue
		}
		if err := c.Unload(ctx, model); err != nil {
			errs = append(errs, fmt.Errorf("unload %q: %w", model, err))
			continue
		}
		delete(warmed[c.base], model)
	}
	if len(warmed[c.base]) == 0 {
		delete(warmed, c.base)
	}
	return errors.Join(errs...)
}

// SRT renders segments as SubRip text (1-indexed, HH:MM:SS,mmm timestamps).
func SRT(segs []Segment) string {
	var b strings.Builder
	for i, s := range segs {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, srtTime(s.Start), srtTime(s.End), strings.TrimSpace(s.Text))
	}
	return b.String()
}

func srtTime(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	ms := int64(math.Round(sec * 1000))
	h := ms / 3600000
	ms %= 3600000
	m := ms / 60000
	ms %= 60000
	s := ms / 1000
	ms %= 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

// --- OpenAI-transcriptions path (llama-server mtmd STT) -------------------------------
// The HQ accuracy tier may be served by llama-server (mtmd; e.g. Qwen3-ASR) rather than
// whisper-server. llama-server exposes the OpenAI /v1/audio/transcriptions shape —
// multipart with a `file` field — reachable through llama-swap's /upstream/<model>/
// passthrough, exactly like the whisper /inference path. Verified live on <node-b>
// 2026-07-22 (HTTP 200; body {"type":"transcript.text.done","text":...}).

// asrLangPrefix matches exactly the Qwen3-ASR language span ("language English") —
// nothing else may be treated as one: a transcript that legitimately CONTAINS the
// literal marker must not lose its leading content to an over-eager parse.
var asrLangPrefix = regexp.MustCompile(`^(?i)language\s+(\S+)$`)

// ParseASRText splits a Qwen3-ASR-style transcript. The model prefixes its output with
// a detected-language span: "language English<asr_text>the transcript…". The prefix is
// consumed ONLY when it matches that exact shape; otherwise the whole trimmed input is
// the transcript and language is unknown ("").
func ParseASRText(raw string) (lang, text string) {
	const marker = "<asr_text>"
	raw = strings.TrimSpace(raw)
	i := strings.Index(raw, marker)
	if i < 0 {
		return "", raw
	}
	m := asrLangPrefix.FindStringSubmatch(strings.TrimSpace(raw[:i]))
	if m == nil {
		return "", raw // marker present but prefix is not a language span: keep everything
	}
	return strings.ToLower(m[1]), strings.TrimSpace(raw[i+len(marker):])
}

// wavDurationSec computes duration from a 16 kHz mono s16 WAV by locating the RIFF
// `data` chunk and using ITS size (32000 bytes/second). The naive (size-44)/32000
// assumed a canonical 44-byte header, but the actual producer (ffmpeg via
// ConvertToWav16k) writes a LIST/INFO chunk too — 78 header bytes, empirically
// verified in review — so the constant was wrong by construction. Falls back to the
// 44-byte estimate only when the chunk walk fails (never worse than before).
func wavDurationSec(wav []byte) float64 {
	if len(wav) > 12 && string(wav[0:4]) == "RIFF" && string(wav[8:12]) == "WAVE" {
		for off := 12; off+8 <= len(wav); {
			id := string(wav[off : off+4])
			sz := int(uint32(wav[off+4]) | uint32(wav[off+5])<<8 | uint32(wav[off+6])<<16 | uint32(wav[off+7])<<24)
			if id == "data" {
				return float64(sz) / 32000.0
			}
			off += 8 + sz + (sz & 1) // chunks are word-aligned
		}
	}
	if len(wav) <= 44 {
		return 0
	}
	return float64(len(wav)-44) / 32000.0
}

// TranscribeOAI uploads wavPath to the model's OpenAI-compatible transcriptions
// endpoint and adapts the reply to the whisper-shaped Result: parsed language, the
// transcript, and ONE synthesized segment spanning the whole clip (SRT and the
// segments.json consumers rely on segments existing; timestamps are not available on
// this path). Serialized by the same process-global mutex as Transcribe — the mtmd
// upstream is served single-slot too.
func (c *Client) TranscribeOAI(ctx context.Context, model, wavPath string) (Result, error) {
	// In line from the first statement, exactly as Transcribe is (register C-91).
	defer joinLine()()
	wav, err := os.ReadFile(wavPath)
	if err != nil {
		return Result{}, fmt.Errorf("read wav: %w", err)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filepath.Base(wavPath))
	if err != nil {
		return Result{}, err
	}
	if _, err := fw.Write(wav); err != nil {
		return Result{}, err
	}
	if err := mw.WriteField("response_format", "json"); err != nil {
		return Result{}, err
	}
	if err := mw.Close(); err != nil {
		return Result{}, err
	}

	rep, err := c.exchange(ctx, model, "/v1/audio/transcriptions", mw.FormDataContentType(), body.Bytes())
	if err != nil {
		return Result{}, err
	}
	raw := rep.body
	// The same two shapes of "no answer" as on the whisper path, typed alike (register C-91).
	switch {
	case rep.cut != nil:
		return Result{}, fmt.Errorf("%w: transcriptions endpoint %d: the answer was cut off mid-read (%w): the upstream was stopped or restarted under the call%s", ErrUpstreamVanished, rep.status, rep.cut, rep.resent)
	case rep.silent():
		return Result{}, fmt.Errorf("%w: transcriptions endpoint %d (empty body): it was unloaded, swapped, restarted or crashed under the call%s", ErrUpstreamVanished, rep.status, rep.resent)
	case rep.status != http.StatusOK:
		return Result{}, fmt.Errorf("transcriptions endpoint %d: %s%s", rep.status, strings.TrimSpace(string(raw)), rep.resent)
	}

	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Result{}, fmt.Errorf("transcriptions response parse: %w (body %.200s)", err, raw)
	}
	lang, text := ParseASRText(parsed.Text)
	// As on the whisper path, the verdict needs an answer: this one came back 200 and empty.
	if text == "" {
		return Result{}, fmt.Errorf("transcriptions endpoint answered with an empty transcript: %w", ErrUpstreamNoSpeech)
	}
	dur := wavDurationSec(wav)
	return Result{Language: lang, Duration: dur, Text: text, Segments: []Segment{{ID: 0, Start: 0, End: dur, Text: text}}}, nil
}
