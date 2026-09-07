// Package ttsclient renders speech through an OpenAI-compatible
// `POST /v1/audio/speech` server (VoiceStudio, an OpenAI-shaped proxy, any
// engine that speaks that contract) and writes the encoded audio to a file.
//
// It is the harness's `tts_endpoint` lane: the HTTP twin of the in-process
// Chatterbox worker, for boxes where the voice engine is a running server
// rather than a python script. The server owns its own GPU, so this lane
// takes no media lease — the same rule as any remote engine.
package ttsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultModel is what an OpenAI-compatible server accepts when the box
// configures no tts_model: every such server maps the OpenAI alias `tts-1` to
// its default engine (VoiceStudio 0.5.1 measured 2026-09-07: `tts-1` → 200,
// `default` → 400).
const DefaultModel = "tts-1"

// DefaultVoice is the server's own default voice.
const DefaultVoice = "default"

// minAudioBytes is the smallest body that can be a real WAV/MP3 (a RIFF
// header alone is 44 bytes); anything shorter is an error page or an empty
// render and is reported as such instead of being written as "audio".
const minAudioBytes = 64

// Request is one render.
type Request struct {
	Base     string // server base URL, no trailing /v1 (e.g. http://127.0.0.1:3900)
	APIKey   string // optional bearer token
	Model    string // "" → DefaultModel
	Voice    string // "" → DefaultVoice
	Text     string // required
	Language string // optional ISO 639-1 hint (VoiceStudio extra; ignored by servers that do not know it)
	Format   string // "" → "wav"
	Out      string // output file path; parent dir is created
	Timeout  time.Duration
	Client   *http.Client // nil → a client with Timeout
}

// Result is what a successful render reports.
type Result struct {
	Path        string
	Bytes       int64
	ContentType string
	Model       string
	Voice       string
}

// Speak POSTs the render and writes the body to req.Out. An HTTP error, a
// non-audio body, or a too-small body is an error naming the server's own
// words — never a silent empty file.
func Speak(ctx context.Context, req Request) (Result, error) {
	base := strings.TrimRight(strings.TrimSpace(req.Base), "/")
	if base == "" {
		return Result{}, errors.New("ttsclient: empty endpoint")
	}
	if strings.TrimSpace(req.Text) == "" {
		return Result{}, errors.New("ttsclient: empty text")
	}
	if req.Out == "" {
		return Result{}, errors.New("ttsclient: empty output path")
	}
	model := req.Model
	if model == "" {
		model = DefaultModel
	}
	voice := req.Voice
	if voice == "" {
		voice = DefaultVoice
	}
	format := req.Format
	if format == "" {
		format = "wav"
	}
	body := map[string]any{
		"model":           model,
		"input":           req.Text,
		"voice":           voice,
		"response_format": format,
	}
	if req.Language != "" {
		body["language"] = req.Language
	}
	raw, _ := json.Marshal(body)

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	client := req.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(cctx, http.MethodPost, base+"/v1/audio/speech", bytes.NewReader(raw))
	if err != nil {
		return Result{}, fmt.Errorf("ttsclient: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	if req.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return Result{}, fmt.Errorf("ttsclient: POST %s/v1/audio/speech: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return Result{}, fmt.Errorf("ttsclient: %s answered %d: %s", base, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") || strings.HasPrefix(ct, "text/html") {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return Result{}, fmt.Errorf("ttsclient: %s answered 200 with %s, not audio: %s", base, ct, strings.TrimSpace(string(msg)))
	}
	if err := os.MkdirAll(filepath.Dir(req.Out), 0o755); err != nil {
		return Result{}, fmt.Errorf("ttsclient: creating output dir: %w", err)
	}
	tmp := req.Out + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return Result{}, fmt.Errorf("ttsclient: creating output: %w", err)
	}
	n, cerr := io.Copy(f, resp.Body)
	if closeErr := f.Close(); cerr == nil {
		cerr = closeErr
	}
	if cerr != nil {
		os.Remove(tmp)
		return Result{}, fmt.Errorf("ttsclient: writing output: %w", cerr)
	}
	if n < minAudioBytes {
		os.Remove(tmp)
		return Result{}, fmt.Errorf("ttsclient: %s returned %d bytes — not an audio file", base, n)
	}
	if err := os.Rename(tmp, req.Out); err != nil {
		os.Remove(tmp)
		return Result{}, fmt.Errorf("ttsclient: finalizing output: %w", err)
	}
	return Result{Path: req.Out, Bytes: n, ContentType: ct, Model: model, Voice: voice}, nil
}
