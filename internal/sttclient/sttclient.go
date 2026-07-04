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
	"strconv"
	"strings"
	"time"
)

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
// http://127.0.0.1:11436). One transcription is mutex-serialized server-side;
// the harness calls one at a time.
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
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
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
