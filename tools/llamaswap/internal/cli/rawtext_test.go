// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Wave E: the raw-text read path.
//
// The bug these pin: the generated client decoded EVERY response as JSON, so
// the four text endpoints (/health, /logs, /metrics, /upstream/{model}/metrics)
// failed with "API returned a non-JSON response; expected JSON" (exit 5)
// against a perfectly healthy server. The fix has two halves and both are
// asserted here: the fetch must return the body verbatim with its status
// intact, and a status that means "the feature is switched off" must exit 0
// with metrics_enabled:false rather than being raised as an error.

package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

const rawTextPrometheusSample = `# HELP llamaswap_requests_total Total requests.
# TYPE llamaswap_requests_total counter
llamaswap_requests_total{model="embeddinggemma"} 42
`

// TestRawTextGetFromPreservesStatusAndBody covers the fetch itself: a text body
// must survive verbatim and a non-2xx must come back as DATA, not as an error,
// because only the caller knows whether a given status means failure or
// feature-off.
func TestRawTextGetFromPreservesStatusAndBody(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantLines   int
		wantCT      string
	}{
		{
			name:        "200 plain text health",
			status:      http.StatusOK,
			contentType: "text/plain; charset=utf-8",
			body:        "OK",
			wantLines:   1,
			wantCT:      "text/plain",
		},
		{
			name:        "200 prometheus exposition",
			status:      http.StatusOK,
			contentType: "text/plain; version=0.0.4",
			body:        rawTextPrometheusSample,
			wantLines:   3,
			wantCT:      "text/plain",
		},
		{
			name:      "503 monitoring disabled",
			status:    http.StatusServiceUnavailable,
			body:      "monitoring is disabled\n",
			wantLines: 1,
			wantCT:    "text/plain", // no header sent: the documented fallback
		},
		{
			name:      "501 seat lacks --metrics",
			status:    http.StatusNotImplemented,
			body:      "",
			wantLines: 0,
			wantCT:    "text/plain",
		},
		{
			name:        "404 endpoint absent",
			status:      http.StatusNotFound,
			contentType: "application/json",
			body:        `{"error":"model not found","src":"llama-swap"}`,
			wantLines:   1,
			wantCT:      "application/json",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Accept"); got != "text/plain" {
					t.Errorf("Accept = %q, want text/plain (a JSON Accept is what made the server answer a shape the client could not read)", got)
				}
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				} else {
					// Suppress Go's sniffed Content-Type so the fallback is exercised.
					w.Header()["Content-Type"] = nil
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			resp, err := rawTextGetFrom(context.Background(), &rootFlags{timeout: 10 * time.Second}, srv.URL, "/metrics")
			if err != nil {
				t.Fatalf("rawTextGetFrom returned an error for HTTP %d; a non-2xx must be reported as data: %v", tc.status, err)
			}
			if resp.Status != tc.status {
				t.Errorf("Status = %d, want %d", resp.Status, tc.status)
			}
			if resp.Body != tc.body {
				t.Errorf("Body = %q, want %q (verbatim)", resp.Body, tc.body)
			}
			if got := resp.contentTypeOrDefault(); got != tc.wantCT {
				t.Errorf("contentTypeOrDefault() = %q, want %q", got, tc.wantCT)
			}
			if got := resp.lineCount(); got != tc.wantLines {
				t.Errorf("lineCount() = %d, want %d", got, tc.wantLines)
			}
			if resp.BaseURL != srv.URL {
				t.Errorf("BaseURL = %q, want %q (error messages must name the address actually used)", resp.BaseURL, srv.URL)
			}
		})
	}
}

// TestRawTextGetFromCRLFLineCount pins the count against Windows line endings,
// which is what llama-swap's buffer carries on this deployment.
func TestRawTextGetFromCRLFLineCount(t *testing.T) {
	r := rawTextResponse{Body: "one\r\ntwo\r\nthree\r\n"}
	if got := r.lineCount(); got != 3 {
		t.Fatalf("lineCount() = %d, want 3 (CRLF must not double-count or drop the last line)", got)
	}
}

// TestRawTextGetFromSurfacesTransportFailure: an unreachable proxy IS an error,
// and the response still has to name the base URL so the caller can say which
// address failed.
func TestRawTextGetFromSurfacesTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close() // nothing listens now

	resp, err := rawTextGetFrom(context.Background(), &rootFlags{timeout: 2 * time.Second}, base, "/health")
	if err == nil {
		t.Fatal("rawTextGetFrom returned no error against a closed listener")
	}
	if resp.BaseURL != base {
		t.Errorf("BaseURL = %q, want %q even on failure", resp.BaseURL, base)
	}
}

// TestRawTextPayloadShapes locks the three JSON envelopes. These are the
// contract a machine consumer branches on, so the key names are asserted, not
// just the values.
func TestRawTextPayloadShapes(t *testing.T) {
	health := newRawTextHealthPayload(rawTextResponse{Body: "OK\n", Latency: 7 * time.Millisecond})
	assertJSONFields(t, health, map[string]any{
		"schema_version": float64(rawTextSchemaVersion),
		"status":         "ok",
		"body":           "OK",
		"latency_ms":     float64(7),
	})

	body := newRawTextBodyPayload(rawTextResponse{Body: "a\nb\n", ContentType: "text/plain"})
	assertJSONFields(t, body, map[string]any{
		"schema_version": float64(rawTextSchemaVersion),
		"content_type":   "text/plain",
		"lines":          float64(2),
		"body":           "a\nb\n",
	})

	on := newRawTextMetricsPayload(rawTextResponse{Body: rawTextPrometheusSample})
	assertJSONFields(t, on, map[string]any{
		"schema_version":  float64(rawTextSchemaVersion),
		"metrics_enabled": true,
		"lines":           float64(3),
	})

	off := newRawTextMetricsOffPayload(http.StatusNotImplemented, rawTextUpstreamMetricsOffReason)
	assertJSONFields(t, off, map[string]any{
		"schema_version":  float64(rawTextSchemaVersion),
		"metrics_enabled": false,
		"reason":          rawTextUpstreamMetricsOffReason,
		"http_status":     float64(http.StatusNotImplemented),
	})
}

func assertJSONFields(t *testing.T, payload any, want map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %T: %v", payload, err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %T: %v", payload, err)
	}
	for k, v := range want {
		actual, present := got[k]
		if !present {
			t.Errorf("%T: key %q absent; got %s", payload, k, raw)
			continue
		}
		if actual != v {
			t.Errorf("%T: %s = %#v, want %#v", payload, k, actual, v)
		}
	}
}

// rawTextCommandEnv points a full RootCmd() run at a stub llama-swap. The
// config path is deliberately absent so the developer's real config.json can
// never leak into the assertion.
func rawTextCommandEnv(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("LLAMASWAP_BASE_URL", srv.URL)
	t.Setenv("LLAMASWAP_NO_LEARN", "true")
	t.Setenv("LLAMASWAP_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("PRINTING_PRESS_VERIFY", "")
	t.Setenv("PRINTING_PRESS_DOGFOOD", "")
}

// TestUpstreamMetricsFeatureAbsentExitsZero is the whole point of the mapping:
// a seat started WITHOUT --metrics answers 501, and that is a fact about the
// configuration. Reporting it as an error made a healthy read look like a
// failed one, and an unattended agent would retry a call that can never
// succeed.
func TestUpstreamMetricsFeatureAbsentExitsZero(t *testing.T) {
	for _, status := range []int{http.StatusNotImplemented, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			rawTextCommandEnv(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/upstream/embeddinggemma/metrics" {
					t.Errorf("path = %q, want /upstream/embeddinggemma/metrics", r.URL.Path)
				}
				w.WriteHeader(status)
			})

			out, _, err := runSpine(t, "upstream", "metrics", "--model", "embeddinggemma", "--json")
			if err != nil {
				t.Fatalf("HTTP %d must exit 0 (feature absent, not an error); got exit %d: %v\n%s",
					status, ExitCode(err), err, out)
			}
			envelope := lastJSONObject(t, out)
			if enabled, ok := envelope["metrics_enabled"].(bool); !ok || enabled {
				t.Fatalf("metrics_enabled = %#v, want false:\n%s", envelope["metrics_enabled"], out)
			}
			reason, _ := envelope["reason"].(string)
			if !strings.Contains(reason, "--metrics") {
				t.Fatalf("reason = %q, want it to name the missing seat flag:\n%s", reason, out)
			}
			if got, ok := envelope["http_status"].(float64); !ok || int(got) != status {
				t.Fatalf("http_status = %#v, want %d:\n%s", envelope["http_status"], status, out)
			}
		})
	}
}

// TestUpstreamMetricsServesPrometheusTextOn200 pins the other half: when the
// seat DOES carry --metrics, the exposition text must come through intact
// instead of failing a JSON decode.
func TestUpstreamMetricsServesPrometheusTextOn200(t *testing.T) {
	rawTextCommandEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(rawTextPrometheusSample))
	})

	out, _, err := runSpine(t, "upstream", "metrics", "--model", "embeddinggemma", "--json")
	if err != nil {
		t.Fatalf("200 text/plain must succeed, got exit %d: %v\n%s", ExitCode(err), err, out)
	}
	envelope := lastJSONObject(t, out)
	if enabled, ok := envelope["metrics_enabled"].(bool); !ok || !enabled {
		t.Fatalf("metrics_enabled = %#v, want true:\n%s", envelope["metrics_enabled"], out)
	}
	if body, _ := envelope["body"].(string); body != rawTextPrometheusSample {
		t.Fatalf("body = %q, want the exposition verbatim", body)
	}
}

// TestUpstreamMetricsUpstream5xxIsTyped: a real upstream fault keeps its typed
// exit code. The feature-off mapping must not swallow genuine failures.
func TestUpstreamMetricsUpstream5xxIsTyped(t *testing.T) {
	rawTextCommandEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})

	out, _, err := runSpine(t, "upstream", "metrics", "--model", "embeddinggemma", "--json")
	if got := ExitCode(err); got != ExitUpstream5xx {
		t.Fatalf("exit = %d, want %d (ExitUpstream5xx); err=%v\n%s", got, ExitUpstream5xx, err, out)
	}
}

// TestServerHealthReadsPlainOK: /health answers a bare "OK". The generated
// path called that a non-JSON API fault; it is the success case.
func TestServerHealthReadsPlainOK(t *testing.T) {
	rawTextCommandEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %q, want /health", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("OK"))
	})

	out, _, err := runSpine(t, "server", "health", "--json")
	if err != nil {
		t.Fatalf("plain \"OK\" must succeed, got exit %d: %v\n%s", ExitCode(err), err, out)
	}
	envelope := lastJSONObject(t, out)
	if envelope["status"] != "ok" || envelope["body"] != "OK" {
		t.Fatalf("status/body = %#v/%#v, want \"ok\"/\"OK\":\n%s", envelope["status"], envelope["body"], out)
	}
	if _, ok := envelope["latency_ms"].(float64); !ok {
		t.Fatalf("latency_ms absent or not a number:\n%s", out)
	}
}

// TestServerHealthNonOKIsUnreachable: anything other than 200 on /health means
// the proxy is not serving, which is exit 4 and not a generic API error.
func TestServerHealthNonOKIsUnreachable(t *testing.T) {
	rawTextCommandEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	out, _, err := runSpine(t, "server", "health", "--json")
	if got := ExitCode(err); got != ExitServerUnreachable {
		t.Fatalf("exit = %d, want %d (ExitServerUnreachable); err=%v\n%s", got, ExitServerUnreachable, err, out)
	}
}

// TestLogsReadsPlainTextBuffer: /logs is text/plain, and the parent command
// must read it the same way its own `triage` subcommand always has.
func TestLogsReadsPlainTextBuffer(t *testing.T) {
	const buffer = "level=INFO msg=\"listening\"\nlevel=ERROR msg=\"upstream exited\"\n"
	rawTextCommandEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/logs" {
			t.Errorf("path = %q, want /logs", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(buffer))
	})

	out, _, err := runSpine(t, "logs", "--json")
	if err != nil {
		t.Fatalf("logs must succeed against a text/plain buffer, got exit %d: %v\n%s", ExitCode(err), err, out)
	}
	envelope := lastJSONObject(t, out)
	if body, _ := envelope["body"].(string); body != buffer {
		t.Fatalf("body = %q, want the buffer verbatim", body)
	}
	if lines, ok := envelope["lines"].(float64); !ok || int(lines) != 2 {
		t.Fatalf("lines = %#v, want 2:\n%s", envelope["lines"], out)
	}
}

// TestActivityPrometheusMonitoringDisabledExitsZero: llama-swap answers 503 on
// /metrics when it was started without monitoring. Switched off is a finding,
// not an outage.
func TestActivityPrometheusMonitoringDisabledExitsZero(t *testing.T) {
	rawTextCommandEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			t.Errorf("path = %q, want /metrics", r.URL.Path)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	out, _, err := runSpine(t, "activity", "prometheus", "--json")
	if err != nil {
		t.Fatalf("503 must exit 0 (monitoring disabled); got exit %d: %v\n%s", ExitCode(err), err, out)
	}
	envelope := lastJSONObject(t, out)
	if enabled, ok := envelope["metrics_enabled"].(bool); !ok || enabled {
		t.Fatalf("metrics_enabled = %#v, want false:\n%s", envelope["metrics_enabled"], out)
	}
	if reason, _ := envelope["reason"].(string); reason != rawTextPrometheusOffReason {
		t.Fatalf("reason = %q, want %q", reason, rawTextPrometheusOffReason)
	}
}

// TestActivityPrometheusServesExpositionOn200 is the enabled half.
func TestActivityPrometheusServesExpositionOn200(t *testing.T) {
	rawTextCommandEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(rawTextPrometheusSample))
	})

	out, _, err := runSpine(t, "activity", "prometheus", "--json")
	if err != nil {
		t.Fatalf("200 exposition must succeed, got exit %d: %v\n%s", ExitCode(err), err, out)
	}
	envelope := lastJSONObject(t, out)
	if body, _ := envelope["body"].(string); body != rawTextPrometheusSample {
		t.Fatalf("body = %q, want the exposition verbatim", body)
	}
}

// TestUpstreamListCommandsCarryLiveHappyArgs pins the Class-2 fix: the
// generated fixture was the literal string "example-value", which llama-swap
// correctly answered 404 for. Every upstream read command must name a model
// that actually exists on this deployment, and it must be a keep-set resident
// so a live dogfood can never trigger an auto-start.
func TestUpstreamListCommandsCarryLiveHappyArgs(t *testing.T) {
	tree := walkCommands(t)
	for _, path := range []string{
		"upstream health",
		"upstream props",
		"upstream slots",
		"upstream metrics",
		"upstream lora-adapters",
	} {
		cmd, ok := tree[path]
		if !ok {
			t.Errorf("%q is not in the command tree", path)
			continue
		}
		if got := cmd.Annotations["pp:happy-args"]; got != "--model;embeddinggemma" {
			t.Errorf("%q pp:happy-args = %q, want %q", path, got, "--model;embeddinggemma")
		}
		if strings.Contains(cmd.Example, "example-value") {
			t.Errorf("%q example still carries the placeholder fixture: %s", path, cmd.Example)
		}
	}
}

// TestFeedbackHasExamples pins the Class-3 fix. `feedback --help` rendered no
// Examples section at all, and the example must invoke flags the command
// actually has.
func TestFeedbackHasExamples(t *testing.T) {
	cmd, ok := walkCommands(t)["feedback"]
	if !ok {
		t.Fatal("feedback is not in the command tree")
	}
	example := strings.TrimSpace(cmd.Example)
	if example == "" {
		t.Fatal("feedback has no Example; an agent reads the example before the flags")
	}
	if !strings.Contains(example, "llamaswap-pp-cli feedback") {
		t.Errorf("feedback example does not invoke the command: %s", example)
	}
	// Every long flag named in the example must exist, so the example can be
	// pasted and run.
	for _, line := range strings.Split(example, "\n") {
		for _, tok := range strings.Fields(line) {
			if !strings.HasPrefix(tok, "--") {
				continue
			}
			name := strings.TrimPrefix(tok, "--")
			if cmd.Flags().Lookup(name) == nil && cmd.Root().PersistentFlags().Lookup(name) == nil &&
				!exampleFlagOnSubcommand(cmd, name) {
				t.Errorf("feedback example uses --%s, which is not a flag on the command: %s", name, line)
			}
		}
	}
}

func exampleFlagOnSubcommand(cmd *cobra.Command, name string) bool {
	for _, sub := range cmd.Commands() {
		if sub.Flags().Lookup(name) != nil {
			return true
		}
	}
	return false
}
