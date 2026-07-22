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
	"time"
)

// inferMu serializes every inference POST to the whisper upstream. whisper-server is
// SINGLE-SLOT: two overlapping /inference requests crash it (SIGSEGV → the harness
// sees an empty-body 502 and llama-swap cold-restarts it, ~60s of refusals). This
// mutex is process-global (the single server is a single shared resource, regardless
// of how many Client values exist), so it holds even across concurrent callers.
var inferMu sync.Mutex

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
	wav, err := os.ReadFile(wavPath)
	if err != nil {
		return Result{}, fmt.Errorf("sttclient: read wav %q: %w", wavPath, err)
	}
	body, contentType, err := buildMultipart(wav, filepath.Base(wavPath), p)
	if err != nil {
		return Result{}, fmt.Errorf("sttclient: build form: %w", err)
	}
	// Target the subpath directly; the bare /upstream/<model> form 301-redirects.
	url := c.base + "/upstream/" + model + "/inference"
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
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		// An empty-body 5xx is the crash signature: whisper-server SIGSEGV'd (a
		// cold-load on near-silent/no-speech audio is the known trigger) and
		// llama-swap is cold-restarting it. Surface a distinct, descriptive error so
		// the caller defers with an accurate reason instead of a bare status code.
		if resp.StatusCode >= 500 && len(bytes.TrimSpace(b)) == 0 {
			return Result{}, fmt.Errorf("whisper-server %d (empty body): upstream crashed — likely near-silent/no-speech audio on a cold load; cold-restart in progress", resp.StatusCode)
		}
		return Result{}, fmt.Errorf("whisper-server %d: %s", resp.StatusCode, string(b))
	}
	var out Result
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Result{}, fmt.Errorf("sttclient: decode verbose_json: %w", err)
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
func (c *Client) Unload(ctx context.Context, model string) error {
	url := c.base + "/api/models/unload/" + model
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
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
// passthrough, exactly like the whisper /inference path. Verified live on Qube
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

	url := c.base + "/upstream/" + model + "/v1/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	inferMu.Lock()
	defer inferMu.Unlock()
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
	dur := wavDurationSec(wav)
	r := Result{Language: lang, Duration: dur, Text: text}
	if text != "" {
		r.Segments = []Segment{{ID: 0, Start: 0, End: dur, Text: text}}
	}
	return r, nil
}
