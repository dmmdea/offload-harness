// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// captureAgentEnvelope redirects the envelope writer for one test.
func captureAgentEnvelope(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := agentErrorWriterOverride
	agentErrorWriterOverride = buf
	t.Cleanup(func() { agentErrorWriterOverride = prev })
	return buf
}

// runExecute drives the real Execute() entry point, which is where the
// process-wide envelope handler is deferred from. runCLI() builds its own root
// command and therefore never exercises it.
func runExecute(t *testing.T, args ...string) error {
	t.Helper()
	t.Setenv("LLAMASWAP_NO_LEARN", "true")
	prev := os.Args
	os.Args = append([]string{"llamaswap-pp-cli"}, args...)
	t.Cleanup(func() { os.Args = prev })
	return Execute()
}

func decodeEnvelope(t *testing.T, buf *bytes.Buffer) agentErrorEnvelope {
	t.Helper()
	var env agentErrorEnvelope
	body := strings.TrimSpace(buf.String())
	if body == "" {
		t.Fatal("no error envelope was emitted")
	}
	// Take the LAST document: a generated required-input command may print its
	// own usage object first.
	if i := strings.LastIndex(body, "\n"); i >= 0 {
		body = body[i+1:]
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("envelope is not JSON: %v\n%s", err, buf.String())
	}
	return env
}

// The whole point: a machine caller gets a code it can branch on, a category,
// a retry decision, and a remediation - not just a sentence.
func TestAgentEnvelopeCarriesTheFullContract(t *testing.T) {
	env := agentErrorEnvelope{OK: false, Error: buildAgentError(
		&cliError{code: ExitKeepsetRefusal, err: fmt.Errorf("refusing to unload embeddinggemma")}, ExitKeepsetRefusal)}
	if env.OK {
		t.Error("ok must be false on an error envelope")
	}
	e := env.Error
	if e.Code != "keepset_refusal" {
		t.Errorf("code = %q, want keepset_refusal", e.Code)
	}
	if e.Category != catRefusal {
		t.Errorf("category = %q, want %q", e.Category, catRefusal)
	}
	if e.Retryable {
		t.Error("a keep-set refusal is deliberate; retrying it unchanged is never useful")
	}
	if e.ExitCode != ExitKeepsetRefusal {
		t.Errorf("exit_code = %d, want %d", e.ExitCode, ExitKeepsetRefusal)
	}
	if e.Message != "refusing to unload embeddinggemma" {
		t.Errorf("message = %q, want the original sentence verbatim", e.Message)
	}
	if !strings.Contains(e.Remediation, "keep-set") {
		t.Errorf("remediation = %q; it must name the concrete next action", e.Remediation)
	}
}

// Every typed exit code this CLI can produce must have a contract. A code with
// no spec is how an agent ends up branching on prose.
func TestEveryTypedExitCodeHasAnAgentContract(t *testing.T) {
	codes := map[string]int{
		"ExitModelNotFound":     ExitModelNotFound,
		"ExitServerUnreachable": ExitServerUnreachable,
		"ExitKeepsetRefusal":    ExitKeepsetRefusal,
		"ExitDrainTimeout":      ExitDrainTimeout,
		"ExitDrainUnobservable": ExitDrainUnobservable,
		"ExitPortConflict":      ExitPortConflict,
		"ExitConfigInvalid":     ExitConfigInvalid,
		"ExitDrift":             ExitDrift,
		"ExitProbeFailed":       ExitProbeFailed,
		"ExitUpstream5xx":       ExitUpstream5xx,
		"ExitFitRefusal":        ExitFitRefusal,
		"ExitNotComparable":     ExitNotComparable,
	}
	for name, code := range codes {
		spec, ok := agentErrorSpecs[code]
		if !ok {
			t.Errorf("%s (%d) has no entry in agentErrorSpecs", name, code)
			continue
		}
		if spec.code == "" || spec.category == "" || spec.remediation == "" {
			t.Errorf("%s (%d) has an incomplete contract: %+v", name, code, spec)
		}
	}
	// The framework codes matter just as much: a dial failure is the most
	// common thing an agent hits.
	for _, code := range []int{1, 2, 3, 4, 5, 6, 7, 10} {
		if _, ok := agentErrorSpecs[code]; !ok {
			t.Errorf("framework exit code %d has no agent contract", code)
		}
	}
}

// An unmapped code must degrade to a visible "unregistered" contract rather
// than to an empty one.
func TestUnmappedExitCodeStaysVisible(t *testing.T) {
	e := buildAgentError(fmt.Errorf("boom"), 99)
	if e.Code != "exit_99" {
		t.Errorf("code = %q, want exit_99", e.Code)
	}
	if !strings.Contains(e.Remediation, "no registered machine contract") {
		t.Errorf("remediation = %q", e.Remediation)
	}
}

func TestAgentEnvelopeExtractsHTTPStatus(t *testing.T) {
	typed := buildAgentError(apiErr(&mcHTTPError{Status: 503, Method: "GET", Path: "/v1/models"}), 5)
	if typed.HTTPStatus != 503 {
		t.Errorf("http_status = %d, want 503 from the typed error", typed.HTTPStatus)
	}
	scraped := buildAgentError(fmt.Errorf("GET /api/hardware returned HTTP 404"), 3)
	if scraped.HTTPStatus != 404 {
		t.Errorf("http_status = %d, want 404 scraped from the message", scraped.HTTPStatus)
	}
	none := buildAgentError(fmt.Errorf("nvidia-smi not found"), 1)
	if none.HTTPStatus != 0 {
		t.Errorf("http_status = %d; a non-HTTP failure must not invent one", none.HTTPStatus)
	}
}

// Human mode must be untouched. An envelope on a human's terminal is a
// regression, not a feature.
func TestHumanModeEmitsNoEnvelope(t *testing.T) {
	buf := captureAgentEnvelope(t)
	emitAgentErrorEnvelopeOnce(&rootFlags{}, fmt.Errorf("boom"), 3)
	if buf.Len() != 0 {
		t.Errorf("human mode emitted an envelope: %s", buf.String())
	}
	for _, f := range []*rootFlags{{asJSON: true}, {agent: true}} {
		buf.Reset()
		emitAgentErrorEnvelopeOnce(f, fmt.Errorf("boom"), 3)
		if buf.Len() == 0 {
			t.Error("machine mode emitted no envelope")
		}
	}
}

// Exactly one envelope per invocation: the 409 path inside the generated
// classifyAPIError and the process-wide handler must not both write one.
func TestAgentEnvelopeIsEmittedExactlyOnce(t *testing.T) {
	buf := captureAgentEnvelope(t)
	flags := &rootFlags{asJSON: true}
	err := apiErr(fmt.Errorf("HTTP 409 already exists"))
	writeAPIErrorEnvelope(flags, err, 5)
	finalizeAgentErrorEnvelope(flags, &err)
	if got := strings.Count(strings.TrimSpace(buf.String()), "\n"); got != 0 {
		t.Errorf("expected exactly one envelope document, got %d newlines:\n%s", got, buf.String())
	}
	env := decodeEnvelope(t, buf)
	if env.Error.HTTPStatus != 409 {
		t.Errorf("http_status = %d, want 409", env.Error.HTTPStatus)
	}
}

// The marker must not leak between invocations, or the second failure in a
// long-lived process would emit nothing.
func TestAgentEnvelopeMarkerIsClearedPerInvocation(t *testing.T) {
	buf := captureAgentEnvelope(t)
	for i := 0; i < 3; i++ {
		flags := &rootFlags{asJSON: true}
		err := error(&cliError{code: 3, err: fmt.Errorf("run %d", i)})
		finalizeAgentErrorEnvelope(flags, &err)
	}
	if got := len(strings.Split(strings.TrimSpace(buf.String()), "\n")); got != 3 {
		t.Errorf("got %d envelopes across 3 invocations, want 3:\n%s", got, buf.String())
	}
	agentEnvelopeMu.Lock()
	remaining := agentEnvelopeEmittedFor
	agentEnvelopeMu.Unlock()
	if remaining != nil {
		t.Error("the emission slot was not released after the invocation finished")
	}
}

// A successful invocation emits nothing.
func TestNoEnvelopeOnSuccess(t *testing.T) {
	buf := captureAgentEnvelope(t)
	flags := &rootFlags{asJSON: true}
	var err error
	finalizeAgentErrorEnvelope(flags, &err)
	if buf.Len() != 0 {
		t.Errorf("a successful invocation emitted an error envelope: %s", buf.String())
	}
}

// End-to-end through Execute(): a cobra PRE-RunE usage error (unknown flag)
// never reaches any RunE, and used to produce a bare `Error:` line with no
// machine output at all.
func TestExecuteEmitsEnvelopeForACobraUsageError(t *testing.T) {
	buf := captureAgentEnvelope(t)
	err := runExecute(t, "--json", "ps", "--definitely-not-a-flag")
	if err == nil {
		t.Fatal("an unknown flag must fail")
	}
	if code := ExitCode(err); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	env := decodeEnvelope(t, buf)
	if env.Error.Code != "usage_error" || env.Error.Category != catFixRequest {
		t.Errorf("envelope = %+v, want a usage_error/fix_request contract", env.Error)
	}
	if env.Error.ExitCode != 2 {
		t.Errorf("exit_code = %d, want 2", env.Error.ExitCode)
	}
	if !strings.Contains(env.Error.Message, "definitely-not-a-flag") {
		t.Errorf("message lost the offending flag: %q", env.Error.Message)
	}
}

// End-to-end through Execute(): a dial failure. This is the case the backlog
// singled out - `ps --agent` against a dead port printed plain text.
func TestExecuteEmitsEnvelopeForADialFailure(t *testing.T) {
	// 18799 is inside the scratch band this project reserves for tests and is
	// not bound by anything.
	t.Setenv("LLAMASWAP_BASE_URL", "http://127.0.0.1:18799")
	buf := captureAgentEnvelope(t)
	err := runExecute(t, "ps", "--agent")
	if err == nil {
		t.Fatal("ps against a dead port must fail")
	}
	if code := ExitCode(err); code != ExitServerUnreachable {
		t.Fatalf("exit code = %d, want %d", code, ExitServerUnreachable)
	}
	env := decodeEnvelope(t, buf)
	if env.OK {
		t.Error("ok must be false")
	}
	if env.Error.Code != "server_unreachable" {
		t.Errorf("code = %q, want server_unreachable", env.Error.Code)
	}
	if !env.Error.Retryable {
		t.Error("a dial failure IS retryable; an agent must be told so")
	}
	if env.Error.Category != catUnavailable {
		t.Errorf("category = %q, want %q", env.Error.Category, catUnavailable)
	}
	if !strings.Contains(env.Error.Remediation, "server health") {
		t.Errorf("remediation = %q; it must name the check to run", env.Error.Remediation)
	}
}

// A hand-written command invoked bare in agent mode emits exactly ONE
// document: the envelope, not a separate usage object plus an envelope.
func TestBareNovelCommandEmitsOneEnvelopeInAgentMode(t *testing.T) {
	buf := captureAgentEnvelope(t)
	flags := &rootFlags{asJSON: true}
	err := usageEnvelopeErr(flags, fmt.Errorf(`"llamaswap-pp-cli gguf" requires a path or a loaded model name`))
	if ExitCode(err) != 2 {
		t.Errorf("exit code = %d, want 2", ExitCode(err))
	}
	// The process-wide handler must then find it already emitted.
	finalizeAgentErrorEnvelope(flags, &err)
	if n := len(strings.Split(strings.TrimSpace(buf.String()), "\n")); n != 1 {
		t.Fatalf("expected exactly 1 document, got %d:\n%s", n, buf.String())
	}
	env := decodeEnvelope(t, buf)
	if env.Error.Code != "usage_error" {
		t.Errorf("code = %q", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "requires a path") {
		t.Errorf("message = %q", env.Error.Message)
	}
}

// ---------------------------------------------------------------------------
// channel routing + argv scan (byte-compatibility with the comfyui twin)
// ---------------------------------------------------------------------------

// A command that already printed its result must not append a second
// top-level document to stdout. `bench compare` does exactly this: it prints
// the two rows and the refusal, then exits 29.
func TestEnvelopeMovesToStderrOnceAResultClaimedStdout(t *testing.T) {
	prevOverride := agentErrorWriterOverride
	agentErrorWriterOverride = nil
	t.Cleanup(func() { agentErrorWriterOverride = prevOverride; agentStdoutClaimed = false })

	agentStdoutClaimed = false
	if got := agentErrorWriter(); got != os.Stdout {
		t.Error("with stdout clean the envelope belongs on stdout")
	}
	markStdoutDocument(&rootFlags{asJSON: true})
	if got := agentErrorWriter(); got != os.Stderr {
		t.Error("once a result document claimed stdout the envelope must move to stderr")
	}
	// The flag must not survive the invocation.
	flags := &rootFlags{asJSON: true}
	var none error
	finalizeAgentErrorEnvelope(flags, &none)
	if agentStdoutClaimed {
		t.Error("the stdout claim leaked past the end of the invocation")
	}
}

// The flag-parse path never reaches PersistentPreRunE, so rootFlags is still
// zero when the failure happens. Only argv can say the caller wanted machine
// output.
func TestArgvScanCoversThePreParseFailurePath(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want bool
	}{
		{[]string{"cli", "ps", "--agent", "--bogus"}, true},
		{[]string{"cli", "ps", "--json"}, true},
		{[]string{"cli", "--json=true", "ps"}, true},
		{[]string{"cli", "--json=false", "ps"}, false},
		{[]string{"cli", "--agent=FALSE", "ps"}, false},
		{[]string{"cli", "ps"}, false},
		// After the -- terminator a token is a positional, not a flag.
		{[]string{"cli", "gguf", "--", "--json"}, false},
	} {
		if got := argvRequestsMachineOutput(c.argv); got != c.want {
			t.Errorf("argvRequestsMachineOutput(%v) = %v, want %v", c.argv[1:], got, c.want)
		}
	}
	// And the fallback must be reachable through the real predicate with
	// unset flags, which is the whole point.
	prev := os.Args
	os.Args = []string{"llamaswap-pp-cli", "ps", "--agent", "--bogus"}
	t.Cleanup(func() { os.Args = prev })
	if !wantsAgentErrorEnvelope(&rootFlags{}) {
		t.Error("a zero rootFlags with --agent in argv must still get an envelope")
	}
}

// The field names are a cross-CLI contract shared with the comfyui twin.
// A rename here silently breaks every agent written against either.
func TestEnvelopeFieldNamesAreTheSharedContract(t *testing.T) {
	raw, err := json.Marshal(agentErrorEnvelope{OK: false, Error: buildAgentError(
		&cliError{code: ExitServerUnreachable, err: fmt.Errorf("dial failed")}, ExitServerUnreachable)})
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe["ok"]; !ok {
		t.Error(`missing top-level "ok"`)
	}
	inner, ok := probe["error"].(map[string]any)
	if !ok {
		t.Fatalf(`"error" is not an object: %s`, raw)
	}
	for _, field := range []string{"code", "category", "retryable", "http_status", "message", "remediation", "exit_code"} {
		if field == "http_status" {
			continue // omitempty: absent when the failure was not an HTTP one
		}
		if _, ok := inner[field]; !ok {
			t.Errorf("error envelope is missing %q; the shape is shared with the comfyui twin: %s", field, raw)
		}
	}
	if len(probe) != 2 {
		t.Errorf("envelope has extra top-level members: %s", raw)
	}
}
