// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave E): the shared raw-text read path.
// Not a command — no pp:data-source marker.
//
// Four llama-swap endpoints answer TEXT, not JSON: /health returns the literal
// "OK", /logs returns the interleaved buffer, and both /metrics and
// /upstream/{model}/metrics return Prometheus exposition. The generated read
// path decodes every response as JSON, so those four commands failed with
// "API returned a non-JSON response; expected JSON" (exit 5) against a healthy
// server — a client-side shape assumption reported as an API fault.
//
// This file is the single fetch those commands share. It deliberately mirrors
// glueFetchLogs (logs_triage.go), which solved the same problem for the triage
// subcommand first: reuse spineBaseURL so --host and the 127.0.0.1 loopback
// normalization apply identically, ask for text/plain, and return the body
// verbatim with its status code so each caller maps status to meaning itself.
// Status is NOT turned into an error here: for the metrics endpoints a non-200
// is a feature-off finding, not a failure.

package cli

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// rawTextSchemaVersion stamps every raw-text JSON envelope so a consumer can
// detect a shape change without diffing fields. Same contract, and same value,
// as glueSchemaVersion.
const rawTextSchemaVersion = 1

// rawTextDefaultTimeout is the request deadline when --timeout is unset.
const rawTextDefaultTimeout = 30 * time.Second

// Reasons for the two feature-off answers, stated once so the human note and
// the JSON envelope can never drift apart.
const (
	// rawTextPrometheusOffReason: llama-swap answers 503 on /metrics when the
	// proxy was started without monitoring. Documented in the spec as the
	// switched-off signal, not an outage.
	rawTextPrometheusOffReason = "llama-swap monitoring disabled (503)"
	// rawTextUpstreamMetricsOffReason: llama.cpp's server only serves
	// /metrics when it was launched with --metrics. Without the flag the seat
	// answers 501 (and some builds 404). Same convention this CLI already uses
	// for hardware (404 -> feature absent) and prometheus (503 -> monitoring
	// off): an absent feature is reported, never raised.
	rawTextUpstreamMetricsOffReason = "seat's llama-server lacks --metrics (HTTP 501/404) — a seat-flag absence, not an error"
)

// rawTextResponse is one plain-text HTTP answer, kept whole: the caller needs
// the status to decide what the body MEANS.
type rawTextResponse struct {
	// BaseURL is the resolved proxy root the request went to. Carried so an
	// error message can name the address that actually failed rather than the
	// one in the config file.
	BaseURL string
	// Path is the request path, already parameter-substituted.
	Path string
	// Status is the HTTP status code; 0 when the request never completed.
	Status int
	// ContentType is the response media type with parameters stripped.
	ContentType string
	// Body is the response body verbatim, including trailing newlines.
	Body string
	// Latency is the wall time from request start to body read.
	Latency time.Duration
}

// latencyMS renders the round trip in whole milliseconds for JSON consumers.
func (r rawTextResponse) latencyMS() int64 { return r.Latency.Milliseconds() }

// lineCount counts the lines in the body, tolerating CRLF and not counting the
// empty remainder after a trailing newline. A one-line body with no newline
// still counts as 1; an empty body counts as 0.
func (r rawTextResponse) lineCount() int {
	body := strings.ReplaceAll(r.Body, "\r\n", "\n")
	body = strings.TrimSuffix(body, "\n")
	if body == "" {
		return 0
	}
	return strings.Count(body, "\n") + 1
}

// contentTypeOrDefault reports the response media type, falling back to
// text/plain when the server sent none. The fallback is honest here: every
// endpoint routed through this helper is a text endpoint by definition.
func (r rawTextResponse) contentTypeOrDefault() string {
	if strings.TrimSpace(r.ContentType) == "" {
		return "text/plain"
	}
	return r.ContentType
}

// rawTextGet performs a plain GET against the resolved proxy base URL and
// returns the body verbatim.
//
// Base-URL resolution goes through spineBaseURL, which reads config.Load — the
// same loader --host writes through (remotes.go sets LLAMASWAP_BASE_URL in
// PersistentPreRunE) — and rewrites a loopback ALIAS to the 127.0.0.1 literal
// so an ::1-first lookup cannot stall the request.
//
// A non-2xx is NOT an error: the returned response carries the status and the
// caller decides. A transport failure IS an error, and the returned response
// still carries BaseURL so the caller can name the address in its message.
func rawTextGet(ctx context.Context, flags *rootFlags, path string) (rawTextResponse, error) {
	base, err := spineBaseURL(flags)
	if err != nil {
		return rawTextResponse{Path: path}, err
	}
	return rawTextGetFrom(ctx, flags, base, path)
}

// rawTextGetFrom is rawTextGet with the base URL supplied, so the behaviour is
// testable against an httptest server without touching the config loader.
func rawTextGetFrom(ctx context.Context, flags *rootFlags, base, path string) (rawTextResponse, error) {
	out := rawTextResponse{BaseURL: base, Path: path}
	timeout := rawTextDefaultTimeout
	if flags != nil && flags.timeout > 0 {
		timeout = flags.timeout
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Accept", "text/plain")
	start := time.Now()
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		out.Latency = time.Since(start)
		return out, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	out.Latency = time.Since(start)
	out.Status = resp.StatusCode
	out.ContentType = strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	out.Body = string(body)
	if readErr != nil {
		return out, readErr
	}
	return out, nil
}

// rawTextHealthPayload is the --json envelope for /health. The literal body is
// kept alongside the normalized status so a consumer can assert on either, and
// latency is reported because "is the proxy alive" and "is the proxy answering
// promptly" are the same question asked twice.
type rawTextHealthPayload struct {
	SchemaVersion int    `json:"schema_version"`
	Status        string `json:"status"`
	Body          string `json:"body"`
	LatencyMS     int64  `json:"latency_ms"`
}

func newRawTextHealthPayload(r rawTextResponse) rawTextHealthPayload {
	return rawTextHealthPayload{
		SchemaVersion: rawTextSchemaVersion,
		Status:        "ok",
		Body:          strings.TrimSpace(r.Body),
		LatencyMS:     r.latencyMS(),
	}
}

// rawTextBodyPayload is the --json envelope for a text endpoint that answered
// 200 and has no feature switch behind it (/logs).
type rawTextBodyPayload struct {
	SchemaVersion int    `json:"schema_version"`
	ContentType   string `json:"content_type"`
	Lines         int    `json:"lines"`
	Body          string `json:"body"`
}

func newRawTextBodyPayload(r rawTextResponse) rawTextBodyPayload {
	return rawTextBodyPayload{
		SchemaVersion: rawTextSchemaVersion,
		ContentType:   r.contentTypeOrDefault(),
		Lines:         r.lineCount(),
		Body:          r.Body,
	}
}

// rawTextMetricsPayload is rawTextBodyPayload plus the metrics_enabled
// discriminator. Both metrics commands emit the SAME key whether the feature is
// on or off, so a consumer branches on one boolean instead of on the presence
// of a field.
type rawTextMetricsPayload struct {
	SchemaVersion  int    `json:"schema_version"`
	MetricsEnabled bool   `json:"metrics_enabled"`
	ContentType    string `json:"content_type"`
	Lines          int    `json:"lines"`
	Body           string `json:"body"`
}

func newRawTextMetricsPayload(r rawTextResponse) rawTextMetricsPayload {
	return rawTextMetricsPayload{
		SchemaVersion:  rawTextSchemaVersion,
		MetricsEnabled: true,
		ContentType:    r.contentTypeOrDefault(),
		Lines:          r.lineCount(),
		Body:           r.Body,
	}
}

// rawTextMetricsOffPayload is the answer when the metrics feature is switched
// off upstream. It exits 0 on purpose: "this seat was not started with
// --metrics" is a fact about the deployment, and an agent that treats it as a
// failure would retry forever against a server that will never answer.
type rawTextMetricsOffPayload struct {
	SchemaVersion  int    `json:"schema_version"`
	MetricsEnabled bool   `json:"metrics_enabled"`
	Reason         string `json:"reason"`
	HTTPStatus     int    `json:"http_status"`
}

func newRawTextMetricsOffPayload(status int, reason string) rawTextMetricsOffPayload {
	return rawTextMetricsOffPayload{
		SchemaVersion:  rawTextSchemaVersion,
		MetricsEnabled: false,
		Reason:         reason,
		HTTPStatus:     status,
	}
}

// rawTextPrintBody writes a text body to the human writer, adding the trailing
// newline the server may have omitted so the shell prompt is not glued to the
// last log line.
func rawTextPrintBody(w io.Writer, body string) {
	if body == "" {
		return
	}
	_, _ = io.WriteString(w, body)
	if !strings.HasSuffix(body, "\n") {
		_, _ = io.WriteString(w, "\n")
	}
}
