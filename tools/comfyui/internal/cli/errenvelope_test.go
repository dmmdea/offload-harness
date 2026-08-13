// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
//
// Tests for the structured error envelope and the typed exit-code registry.
//
// WHAT THESE PROTECT. The envelope is a CONTRACT with machine callers and with
// the sibling CLI: an agent branches on error.code and error.retryable without
// reading the message. So the assertions below pin the field NAMES, the code
// TOKENS, and the retryable judgement — a rename that compiles and "looks
// fine" is exactly the regression these exist to fail on.
//
// No server is required: classification is a pure function of an error and an
// exit code.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"comfyui-pp-cli/internal/comfy/exp"
	"comfyui-pp-cli/internal/comfy/submit"
)

// TestErrorEnvelopeFieldNamesAreFrozen pins the wire shape. These names are
// shared with llamaswap-pp-cli; an agent that learns one must read the other.
// Renaming a field here silently breaks that, so it must break this test first.
func TestErrorEnvelopeFieldNamesAreFrozen(t *testing.T) {
	env := errorEnvelope{OK: false, Error: classifyErrorEnvelope(errors.New("boom"), ExitWaitTimeout)}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := decoded["ok"]; !ok {
		t.Errorf("envelope lost its top-level %q field; got %s", "ok", raw)
	}
	if ok, _ := decoded["ok"].(bool); ok {
		t.Errorf("ok must be false on an error envelope; got %s", raw)
	}
	inner, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("envelope lost its nested error object; got %s", raw)
	}
	for _, field := range []string{
		"code", "category", "retryable", "http_status", "message", "remediation", "exit_code",
	} {
		if _, present := inner[field]; !present {
			t.Errorf("error object lost the %q field — this name is shared with the sibling CLI; got %s", field, raw)
		}
	}
	if len(inner) != 7 {
		t.Errorf("error object has %d fields, expected exactly 7; a new field is a contract change: %s", len(inner), raw)
	}
}

func TestClassifyErrorEnvelope(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		exitCode      int
		wantCode      string
		wantCategory  string
		wantRetryable bool
		wantStatus    int
	}{
		{
			name: "usage error is not retryable", err: errors.New(`unknown flag: --foob`),
			exitCode: ExitUsage, wantCode: "usage", wantCategory: errCategoryUsage,
		},
		{
			name: "dial failure is a retryable network failure",
			err:  errors.New(`GET /queue: dial tcp 127.0.0.1:8188: connectex: No connection could be made`),
			// Even routed through the generic API code, a statusless dead
			// socket must classify as unreachable rather than blaming the
			// request.
			exitCode: ExitAPI, wantCode: "server_unreachable", wantCategory: errCategoryNetwork, wantRetryable: true,
		},
		{
			name: "explicit unreachable exit", err: errors.New("connection refused"),
			exitCode: ExitServerUnreachable, wantCode: "server_unreachable",
			wantCategory: errCategoryNetwork, wantRetryable: true,
		},
		{
			name: "5xx is retryable and carries its status", err: errors.New("HTTP 503: upstream is restarting"),
			exitCode: ExitAPI, wantCode: "upstream_5xx", wantCategory: errCategoryServer,
			wantRetryable: true, wantStatus: 503,
		},
		{
			name: "4xx is a server refusal, not retryable", err: errors.New("HTTP 400: invalid prompt"),
			exitCode: ExitAPI, wantCode: "api_error", wantCategory: errCategoryServer, wantStatus: 400,
		},
		{
			name: "wait timeout is retryable — the render is still going",
			err:  errors.New("prompt abc did not finish within 25m"),
			// The single most important retryable=true in the whole table: an
			// agent that treats a timeout as a failure re-submits and doubles
			// the work on an already-busy GPU.
			exitCode: ExitWaitTimeout, wantCode: "wait_timeout", wantCategory: errCategoryDomain, wantRetryable: true,
		},
		{
			name:     "graph invalid is a domain error and not retryable",
			err:      errors.New("graph validation failed: 2 error(s), 0 warning(s)"),
			exitCode: ExitGraphInvalid, wantCode: "graph_invalid", wantCategory: errCategoryDomain,
		},
		{
			name:     "13 disambiguates to model-not-visible on message",
			err:      errors.New("model.safetensors is not visible to any loader"),
			exitCode: ExitModelNotVisible, wantCode: "model_not_visible", wantCategory: errCategoryDomain,
		},
		{
			name:     "OOM is retryable after a resource change",
			err:      errors.New("prompt abc failed: KSampler(3) OutOfMemoryError"),
			exitCode: ExitUpstreamOOM, wantCode: "upstream_oom", wantCategory: errCategoryServer, wantRetryable: true,
		},
		{
			name:     "interruption is retryable unchanged",
			err:      errors.New("prompt abc was interrupted before it finished"),
			exitCode: ExitExecutionInterrupted, wantCode: "execution_interrupted",
			wantCategory: errCategoryDomain, wantRetryable: true,
		},
		{
			name:     "21 disambiguates to submit-rejected on message",
			err:      errors.New("rejected, nothing was queued: invalid prompt"),
			exitCode: ExitSubmitRejected, wantCode: "submit_rejected", wantCategory: errCategoryDomain,
		},
		{
			name:     "22 disambiguates to submit-partial-accept on message",
			err:      errors.New("partial accept: ComfyUI queued 9 but dropped output branch(es) 12"),
			exitCode: ExitSubmitPartialAccept, wantCode: "submit_partial_accept", wantCategory: errCategoryDomain,
		},
		{
			name:     "wait's 22 stays outputs-pending",
			err:      errors.New("prompt abc recorded execution_success but /history published no outputs"),
			exitCode: ExitOutputsPending, wantCode: "outputs_pending", wantCategory: errCategoryDomain, wantRetryable: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyErrorEnvelope(tc.err, tc.exitCode)
			if got.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", got.Code, tc.wantCode)
			}
			if got.Category != tc.wantCategory {
				t.Errorf("category = %q, want %q", got.Category, tc.wantCategory)
			}
			if got.Retryable != tc.wantRetryable {
				t.Errorf("retryable = %t, want %t", got.Retryable, tc.wantRetryable)
			}
			if got.HTTPStatus != tc.wantStatus {
				t.Errorf("http_status = %d, want %d", got.HTTPStatus, tc.wantStatus)
			}
			if got.ExitCode != tc.exitCode {
				t.Errorf("exit_code = %d, want %d — the envelope and $? must agree", got.ExitCode, tc.exitCode)
			}
			if strings.TrimSpace(got.Message) == "" {
				t.Error("message is empty; the human text must always survive into the envelope")
			}
			if strings.TrimSpace(got.Remediation) == "" {
				t.Error("remediation is empty; every classified code owes the caller a next action")
			}
		})
	}
}

// TestClassifyErrorEnvelopeNeverPanicsOnNil guards the reporting path: a
// failure to report a failure must not become a second failure.
func TestClassifyErrorEnvelopeNeverPanicsOnNil(t *testing.T) {
	got := classifyErrorEnvelope(nil, ExitError)
	if got.ExitCode != ExitError {
		t.Errorf("exit_code = %d, want %d", got.ExitCode, ExitError)
	}
	if got.Message != "" {
		t.Errorf("message = %q, want empty for a nil error", got.Message)
	}
}

func TestHTTPStatusFromError(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"HTTP 409: already exists", 409},
		{"HTTP 503: gateway", 503},
		{"dial tcp: connection refused", 0},
		{"", 0},
		// Must not match a bare number that is not a status.
		{"queued 200 items", 0},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := httpStatusFromError(errors.New(tc.in)); got != tc.want {
				t.Errorf("httpStatusFromError(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
	if got := httpStatusFromError(nil); got != 0 {
		t.Errorf("httpStatusFromError(nil) = %d, want 0", got)
	}
}

func TestIsNetworkFailure(t *testing.T) {
	networkErrs := []string{
		"dial tcp 127.0.0.1:8188: connectex: No connection could be made",
		"connection refused",
		"no such host",
		"context deadline exceeded",
		"API unreachable and no local data",
	}
	for _, msg := range networkErrs {
		if !isNetworkFailure(errors.New(msg)) {
			t.Errorf("isNetworkFailure(%q) = false, want true", msg)
		}
	}
	notNetwork := []string{
		"HTTP 400: invalid prompt",
		"graph validation failed",
		// Guards the old bare-"eof" substring, which matched innocent prose.
		"the leof node is unknown",
	}
	for _, msg := range notNetwork {
		if isNetworkFailure(errors.New(msg)) {
			t.Errorf("isNetworkFailure(%q) = true, want false", msg)
		}
	}
}

// TestExitCodeForFailureClass pins the split that this wave introduced:
// interruption and OOM no longer collapse into the generic job-failed code,
// because the correct response to each is different.
func TestExitCodeForFailureClass(t *testing.T) {
	tests := []struct {
		class string
		want  int
	}{
		{exp.ExitOOM, ExitUpstreamOOM},
		{exp.ExitInterrupted, ExitExecutionInterrupted},
		{exp.ExitValidation, ExitGraphInvalid},
		{exp.ExitMissingModel, ExitModelNotVisible},
		{exp.ExitError, ExitJobFailed},
		{"", ExitJobFailed},
		{"something-new-upstream", ExitJobFailed},
	}
	for _, tc := range tests {
		t.Run(tc.class, func(t *testing.T) {
			if got := exitCodeForFailureClass(tc.class); got != tc.want {
				t.Errorf("exitCodeForFailureClass(%q) = %d, want %d", tc.class, got, tc.want)
			}
		})
	}
}

// TestComfyTerminalFailureErrClassifiesFromServerText is the end-to-end of the
// same split, driven by the exception text ComfyUI actually reports.
func TestComfyTerminalFailureErrClassifiesFromServerText(t *testing.T) {
	tests := []struct {
		name     string
		excType  string
		message  string
		wantCode int
	}{
		{"cuda oom", "OutOfMemoryError", "CUDA error: out of memory", ExitUpstreamOOM},
		{"allocation", "RuntimeError", "Allocation on device 0 failed", ExitUpstreamOOM},
		{"interrupt", "InterruptedException", "processing interrupted", ExitExecutionInterrupted},
		{"validation", "ValueError", "Required input is missing", ExitGraphInvalid},
		{"other", "TypeError", "unsupported operand", ExitJobFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := comfyTerminalFailureErr(tc.excType, tc.message, fmt.Errorf("prompt abc failed"))
			if got := ExitCode(err); got != tc.wantCode {
				t.Errorf("ExitCode = %d, want %d (exc %q / %q)", got, tc.wantCode, tc.excType, tc.message)
			}
		})
	}
}

// TestSubmitPackageExitCodesMatchRegistry is the runtime twin of the
// compile-time guard in exitcodes.go. The build already fails if these drift,
// so this documents the intent for a reader who never sees the const trick.
func TestSubmitPackageExitCodesMatchRegistry(t *testing.T) {
	if submit.ExitRejected != ExitSubmitRejected {
		t.Errorf("submit.ExitRejected = %d, registry = %d", submit.ExitRejected, ExitSubmitRejected)
	}
	if submit.ExitPartialAccept != ExitSubmitPartialAccept {
		t.Errorf("submit.ExitPartialAccept = %d, registry = %d", submit.ExitPartialAccept, ExitSubmitPartialAccept)
	}
	if submit.ExitMalformed != ExitSubmitMalformed {
		t.Errorf("submit.ExitMalformed = %d, registry = %d", submit.ExitMalformed, ExitSubmitMalformed)
	}
}

// TestWantsErrorEnvelopeReadsArgv covers the case that used to be the ONLY one
// still emitting bare prose to a machine: a malformed invocation under
// --agent, where flag parsing failed before PersistentPreRunE could turn
// --agent into --json.
func TestWantsErrorEnvelopeReadsArgv(t *testing.T) {
	if !wantsErrorEnvelope(&rootFlags{asJSON: true}) {
		t.Error("--json should want the envelope")
	}
	if !wantsErrorEnvelope(&rootFlags{agent: true}) {
		t.Error("--agent should want the envelope")
	}
	if wantsErrorEnvelope(&rootFlags{}) {
		t.Error("a human invocation should not want the envelope")
	}
}
