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
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dmmdea/offload-harness/internal/modelaffinity"
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

// warmed records, per llama-swap base and model, that a request has gone out to the
// upstream since the last unload this process sent (register C-91). UnloadIfIdle unloads
// only a model in it, which is what makes "one unload per burst" true when several
// finishing calls reach it at once, and keeps a call that never reached the upstream (the
// card was fenced, the wav would not read) from unloading anything. Guarded by inferMu.
var warmed = map[string]bool{}

// Pending reports how many transcriptions in this process are waiting for the whisper
// upstream or running on it (register C-91): the count UnloadIfIdle reads. It exists so a
// test can wait until a second call is in line instead of sleeping and hoping.
func Pending() int { return int(sttCalls.Load()) }

// ErrUpstreamNoSpeech is the verdict "the upstream ANSWERED, successfully, with an empty
// transcript": Transcribe and TranscribeOAI return it, wrapped, in place of an empty
// Result. It is never inferred from a failure to get an answer (register C-91). Until
// 2026-10-01 it also marked the empty-body 5xx that whisper-server's exit on audio with no
// speech content leaves behind (root-caused 2026-09-23, F-35; see Transcribe) — but the
// same bare status, or a read cut off mid-answer, follows a model that is unloaded,
// swapped or restarted under a call, and audio WITH speech was reported to its caller as
// silent. A status cannot tell the two apart; only an answer can. Callers treat this as
// "no speech found", not as an infrastructure failure, and have nothing to retry.
var ErrUpstreamNoSpeech = errors.New("stt upstream: the upstream answered with an empty transcript (no speech found)")

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
}

// fenceBudget is how long a transcription waits for a fenced card when the
// client carries no timeout of its own.
const fenceBudget = 120 * time.Second

// fencedURL builds the whisper upstream's URL behind the GPU-lease fence
// (2026-09-22). The passthrough STARTS the whisper model when it is not loaded,
// so under a render or an exclusive hold the request waits for the card — the
// same bound a text admission uses: the client's own timeout, then ctx — and on
// exhaustion returns the fence's *modelaffinity.LeaseError, which the pipeline
// defers as congestion ("timeout"). A resident whisper model is served at once.
func (c *Client) fencedURL(ctx context.Context, model, path string) (string, error) {
	budget := c.http.Timeout
	if budget <= 0 {
		budget = fenceBudget
	}
	return modelaffinity.AwaitUpstream(ctx, c.base, model, path, time.Now().Add(budget))
}

// New builds a client. timeout bounds one transcription (long audio).
func New(base string, timeout time.Duration) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: timeout},
	}
}

// Transcribe uploads wavPath to the whisper upstream `model` and returns the
// parsed verbose_json. The multipart body is built in memory (wavs are 16 kHz
// mono — ~2 MB/min — comfortable in 64 GB RAM).
func (c *Client) Transcribe(ctx context.Context, model, wavPath string, p Params) (Result, error) {
	// In line from the first statement (register C-91): the call counts as waiting for the
	// whole of its fence wait and its queue on inferMu, which is what UnloadIfIdle reads.
	sttCalls.Add(1)
	defer sttCalls.Add(-1)
	wav, err := os.ReadFile(wavPath)
	if err != nil {
		return Result{}, fmt.Errorf("sttclient: read wav %q: %w", wavPath, err)
	}
	body, contentType, err := buildMultipart(wav, filepath.Base(wavPath), p)
	if err != nil {
		return Result{}, fmt.Errorf("sttclient: build form: %w", err)
	}
	// Target the subpath directly; the bare /upstream/<model> form 301-redirects.
	url, err := c.fencedURL(ctx, model, "/inference")
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", contentType)
	// Serialize against the single-slot server (see inferMu): overlapping requests
	// crash whisper-server. Held across the whole request/response so a second caller
	// waits for the connection to fully drain, not just for Do() to return.
	inferMu.Lock()
	defer inferMu.Unlock()
	warmed[c.warmKey(model)] = true
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, rerr := io.ReadAll(io.LimitReader(resp.Body, 400))
		// NO ANSWER IS NOT NO SPEECH (register C-91). An empty-body 5xx is llama-swap's
		// bare answer when its connection to whisper-server drops mid-request ("upstream
		// process exited unexpectedly" in llama-swap.log), and a read that was cut off
		// reads empty too — the error of that read used to be thrown away. Both follow a
		// crash: whisper.cpp (build-v194) reliably exits on audio with no speech content
		// (root-caused 2026-09-23, F-35: direct /inference calls reproduced it 100% of
		// the time on a near-silent ACE-Step tail, on the same clip with the per-request
		// vad field on and off, and on a plain 440 Hz tone — no speech, not loudness, not
		// a cold load; an upstream bug outside this repo). They follow an unload, a swap
		// or a restart under the call just as well, and then the audio HAD speech: the
		// old reading reported a call whose model was taken away mid-flight as a
		// recording with nothing in it. The status cannot say which it was, so the error
		// says what is known and leaves the verdict to an upstream that answered
		// (ErrUpstreamNoSpeech, below). A crash on no-speech audio therefore reaches the
		// caller as a failed call; a retry of an unloaded one succeeds.
		switch {
		case rerr != nil:
			return Result{}, fmt.Errorf("whisper-server %d: the answer was cut off mid-read (%v): the upstream was stopped or restarted under the call", resp.StatusCode, rerr)
		case resp.StatusCode >= 500 && len(bytes.TrimSpace(b)) == 0:
			return Result{}, fmt.Errorf("whisper-server %d (empty body): the upstream vanished mid-request — it was unloaded, restarted or crashed (whisper.cpp also exits on audio with no speech content, so this is not a no-speech verdict)", resp.StatusCode)
		}
		return Result{}, fmt.Errorf("whisper-server %d: %s", resp.StatusCode, string(b))
	}
	var out Result
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Result{}, fmt.Errorf("sttclient: decode verbose_json: %w", err)
	}
	// The one producer of the no-speech verdict: the upstream answered 200 and heard nothing.
	if strings.TrimSpace(out.Text) == "" && len(out.Segments) == 0 {
		return Result{}, fmt.Errorf("whisper-server answered with an empty transcript: %w", ErrUpstreamNoSpeech)
	}
	return out, nil
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
	}
	if p.SplitOnWord {
		fields["split_on_word"] = "true"
	}
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

// warmKey names one (llama-swap base, model) pair in warmed.
func (c *Client) warmKey(model string) string { return c.base + "\x00" + model }

// UnloadIfIdle is Unload for the zero-always-warm caller (register C-91): it frees the
// upstream only when this is the last transcription of a burst, and only once per burst.
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
//   - the model was not used since the last unload (warmed): a call of the same burst
//     already freed it, or this call never reached the upstream (the card was fenced, the
//     wav would not read).
//
// It never BLOCKS on inferMu, because a finished call must not hold its own answer back
// for the length of the next call's inference: a 30-minute transcription would delay a
// ten-second one's result by 30 minutes. That is safe because a failed TryLock always means
// somebody else has the unload covered: the holder is a call that came in after the check
// above (it is the last one out), or another UnloadIfIdle that has already seen an empty
// line and will unload the model if it was used. The decision is taken on sttCalls BEFORE
// the lock, so a call that only looked and went away never holds the slot against another.
//
// The last call out unloads even when it failed: the pipeline calls this after every
// call, so a burst whose final call was refused by the fence still frees the model its
// earlier calls warmed. A failed unload leaves the model marked warm for the next one; the
// ttl is the backstop, as it is for a mixed burst whose second model was left to it.
func (c *Client) UnloadIfIdle(ctx context.Context, model string) error {
	if sttCalls.Load() > 0 {
		return nil
	}
	if !inferMu.TryLock() {
		return nil
	}
	defer inferMu.Unlock()
	key := c.warmKey(model)
	if sttCalls.Load() > 0 || !warmed[key] {
		return nil
	}
	if err := c.Unload(ctx, model); err != nil {
		return err
	}
	delete(warmed, key)
	return nil
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
	sttCalls.Add(1)
	defer sttCalls.Add(-1)
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

	url, err := c.fencedURL(ctx, model, "/v1/audio/transcriptions")
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	inferMu.Lock()
	defer inferMu.Unlock()
	warmed[c.warmKey(model)] = true
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("transcriptions endpoint %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
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
