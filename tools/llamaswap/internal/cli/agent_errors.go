// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave LS-1): the machine-readable error contract.
// Not a command: no pp:data-source marker.
//
// Before this file, an agent driving the CLI got a typed EXIT CODE and a
// human sentence on stderr. That is enough to know something failed and not
// enough to decide what to do about it: whether to retry, whether to fix its
// own arguments, or whether the server is simply not there. Only the HTTP-409
// branch emitted any JSON at all, and it emitted {error, code} - a message and
// a number, with the remediation glued into the prose.
//
// Every non-zero exit under --json/--agent now emits ONE envelope on stdout:
//
//	{"ok":false,"error":{"code","category","retryable","http_status",
//	                     "message","remediation","exit_code"}}
//
// Human mode is untouched: no envelope, same stderr line, same exit code.

package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// agentError is the machine-readable half of a failure.
type agentError struct {
	// Code is the stable symbolic identifier. Branch on this, not on the
	// message, which is prose and may be reworded.
	Code string `json:"code"`
	// Category groups codes into the four decisions a caller can make:
	// fix_request, retry, unavailable, refusal.
	Category string `json:"category"`
	// Retryable answers "is trying this again, unchanged, ever useful?"
	Retryable bool `json:"retryable"`
	// HTTPStatus is the upstream status when the failure came from one, 0
	// otherwise. Extracted from the error chain, never guessed.
	HTTPStatus int `json:"http_status,omitempty"`
	// Message is the human sentence, verbatim.
	Message string `json:"message"`
	// Remediation is the concrete next action. Never a command this CLI
	// would run itself.
	Remediation string `json:"remediation,omitempty"`
	// ExitCode is the process exit code, repeated here so a caller reading
	// only stdout does not need the shell's $?.
	ExitCode int `json:"exit_code"`
}

type agentErrorEnvelope struct {
	OK    bool       `json:"ok"`
	Error agentError `json:"error"`
}

// Error categories.
const (
	catFixRequest  = "fix_request"
	catRetry       = "retry"
	catUnavailable = "unavailable"
	catRefusal     = "refusal"
	catInternal    = "internal"
)

// agentErrorSpec describes one exit code's machine contract.
type agentErrorSpec struct {
	code        string
	category    string
	retryable   bool
	remediation string
}

// agentErrorSpecs maps every exit code this CLI can produce. A code missing
// from this table falls through to a generic entry that still carries the
// number, so a new typed code added without a spec degrades to "unmapped"
// rather than to silence.
var agentErrorSpecs = map[int]agentErrorSpec{
	1: {"unspecified_error", catInternal, false,
		"an untyped failure: read the message. If it is reproducible, it is a gap in this CLI's error classification"},
	2: {"usage_error", catFixRequest, false,
		"fix the arguments and re-run; `<command> --help` lists the accepted flags, and `which \"<capability>\" --json` finds the right command"},
	3: {"not_found", catFixRequest, false,
		"the named model, file, or record does not exist. `models list` shows the roster (aliases included); `bench compare --list` shows recorded rows"},
	4: {"server_unreachable", catUnavailable, true,
		"llama-swap did not answer on the configured host. Check it is listening (`server health`), and note this CLI addresses the loopback as 127.0.0.1 because an ::1-first lookup stalls"},
	5: {"api_error", catRetry, true,
		"the server answered with an unexpected status. Read http_status; a 4xx means the request was wrong, a 5xx means try again"},
	6: {"partial_failure", catFixRequest, false,
		"the request was accepted but some operations inside it failed; inspect the partial_failure block in the success envelope"},
	7: {"rate_limited", catRetry, true,
		"back off and retry. --rate-limit paces this CLI's own requests"},
	10: {"config_error", catFixRequest, false,
		"the CLI's own configuration could not be loaded or is invalid; run `doctor` and `configure`"},
	ExitKeepsetRefusal: {"keepset_refusal", catRefusal, false,
		"the target is a protected keep-set member (matched by id AND alias, from config, never from the server's ttl field). This is a deliberate refusal: remove it from the keep-set in the CLI's config if the unload is genuinely intended"},
	ExitDrainTimeout: {"drain_timeout", catRetry, true,
		"the seat did not go idle within the timeout and NOTHING was unloaded (fail-closed). Retry when traffic subsides, or raise --drain-timeout"},
	ExitDrainUnobservable: {"drain_unobservable", catRetry, true,
		"/slots was unreadable, so idleness could not be confirmed and NOTHING was unloaded (fail-closed). Check the seat was started with --slots enabled"},
	ExitPortConflict: {"port_conflict", catFixRequest, false,
		"the chosen port is already listening or falls inside the proxy's startPort span / a reserved band. Pick another port"},
	ExitConfigInvalid: {"config_invalid", catFixRequest, false,
		"the llama-swap YAML failed schema or semantic validation; `config lint` names the offending keys"},
	ExitDrift: {"drift_detected", catRefusal, false,
		"a FINDING, not a failure: the live state diverges from the file. `config diff` and `seat show --diff-yaml` show what moved; `version drift` shows a server/CLI surface gap"},
	ExitProbeFailed: {"probe_degraded", catRefusal, false,
		"the memory stack answered but its output is outside the stored calibrated tolerance. A model can be loaded and serving degraded results (a dropped --pooling does exactly this); compare the seat's flags with `seat show --diff-yaml`"},
	ExitUpstream5xx: {"upstream_5xx", catRetry, true,
		"the model server itself answered 5xx. Check `logs` for the seat, and `upstream health <model>`"},
	ExitFitRefusal: {"refused_to_guess", catRefusal, false,
		"the command REFUSED to answer rather than emit a number it cannot stand behind (an interval straddling capacity, an incomplete shard set, an MLA/SSM architecture, or a non-model GGUF). The message names the measurement that would settle it"},
	ExitNotComparable: {"not_comparable", catRefusal, false,
		"the two rows carry different comparability keys, so their difference measures the configuration change rather than the thing being compared. The comparability_key_diff field names the fields that differ"},
}

// httpStatusPattern recovers a status from a message when the error chain
// carries no typed HTTP error (the generated client formats its failures as
// prose containing "HTTP 404").
var httpStatusPattern = regexp.MustCompile(`HTTP (\d{3})`)

// buildAgentError classifies err at exit code into the machine contract.
func buildAgentError(err error, code int) agentError {
	spec, ok := agentErrorSpecs[code]
	if !ok {
		spec = agentErrorSpec{
			code:        "exit_" + strconv.Itoa(code),
			category:    catInternal,
			remediation: "this exit code has no registered machine contract in this build; treat it as a failure and read the message",
		}
	}
	out := agentError{
		Code:        spec.code,
		Category:    spec.category,
		Retryable:   spec.retryable,
		Message:     err.Error(),
		Remediation: spec.remediation,
		ExitCode:    code,
	}
	// Prefer the typed status over the message scrape: a message can quote a
	// status from a hint sentence, the typed error cannot.
	var he *mcHTTPError
	if errors.As(err, &he) && he.Status > 0 {
		out.HTTPStatus = he.Status
	} else if m := httpStatusPattern.FindStringSubmatch(out.Message); m != nil {
		if n, cerr := strconv.Atoi(m[1]); cerr == nil {
			out.HTTPStatus = n
		}
	}
	return out
}

// agentEnvelopeEmittedFor records WHICH invocation already wrote an envelope,
// so the 409 path inside the generated classifyAPIError and the process-wide
// handler cannot both emit one. A single slot rather than a set: one process
// runs one Execute, and a set keyed on pointers would grow without bound
// whenever a caller emitted but never reached the finalizer.
var (
	agentEnvelopeMu         sync.Mutex
	agentEnvelopeEmittedFor *rootFlags
)

// markAgentEnvelopeEmitted claims the emission slot for flags. It reports
// false when this invocation already emitted.
func markAgentEnvelopeEmitted(flags *rootFlags) bool {
	agentEnvelopeMu.Lock()
	defer agentEnvelopeMu.Unlock()
	if agentEnvelopeEmittedFor == flags {
		return false
	}
	agentEnvelopeEmittedFor = flags
	return true
}

// releaseAgentEnvelopeSlot clears the slot at the end of an invocation.
func releaseAgentEnvelopeSlot(flags *rootFlags) {
	agentEnvelopeMu.Lock()
	defer agentEnvelopeMu.Unlock()
	if agentEnvelopeEmittedFor == flags {
		agentEnvelopeEmittedFor = nil
	}
}

// wantsAgentErrorEnvelope reports whether this invocation asked for machine
// output. Human mode gets nothing new.
//
// The argv scan is load-bearing, not a belt-and-braces extra. A flag-parse
// failure aborts inside cobra BEFORE PersistentPreRunE runs, so rootFlags is
// still zero at that point: the very case an agent most needs an envelope for
// (`ps --agent --definitely-not-a-flag`) is the one where the parsed flags
// cannot tell us the caller wanted one. Reading argv directly is the only
// source of truth available that early.
func wantsAgentErrorEnvelope(flags *rootFlags) bool {
	if flags != nil && (flags.asJSON || flags.agent) {
		return true
	}
	return argvRequestsMachineOutput(os.Args)
}

// argvRequestsMachineOutput scans a raw argument vector for the machine-output
// flags, in every spelling pflag accepts.
func argvRequestsMachineOutput(argv []string) bool {
	for _, a := range argv {
		switch {
		case a == "--json", a == "--agent":
			return true
		case strings.HasPrefix(a, "--json="), strings.HasPrefix(a, "--agent="):
			// pflag accepts --json=false; honour the negative form rather
			// than treating the flag's presence as consent.
			return !strings.EqualFold(a[strings.IndexByte(a, '=')+1:], "false")
		case a == "--":
			// Everything after the terminator is a positional argument, not
			// a flag; a file literally named --json must not switch modes.
			return false
		}
	}
	return false
}

// agentStdoutClaimed records that a command already wrote a RESULT document to
// stdout during this invocation.
//
// When that has happened the error envelope goes to STDERR instead: a command
// that printed its report and then exited non-zero (bench compare's
// not-comparable refusal is the canonical case) must not append a second
// top-level document to stdout, or a consumer doing one json.Unmarshal of the
// stream gets "Extra data". The refusal is still machine-readable; it is just
// on the channel that does not corrupt the result.
var agentStdoutClaimed bool

// markStdoutDocument is called by the result-emitting funnels.
func markStdoutDocument(flags *rootFlags) {
	if flags != nil {
		agentStdoutClaimed = true
	}
}

// emitAgentErrorEnvelopeOnce writes the envelope unless one was already
// written for this invocation.
func emitAgentErrorEnvelopeOnce(flags *rootFlags, err error, code int) {
	if err == nil || code == 0 || !wantsAgentErrorEnvelope(flags) {
		return
	}
	if !markAgentEnvelopeEmitted(flags) {
		return
	}
	_ = json.NewEncoder(agentErrorWriter()).Encode(agentErrorEnvelope{OK: false, Error: buildAgentError(err, code)})
}

// agentErrorWriterOverride lets tests capture the envelope without stealing
// the process's stdout.
var agentErrorWriterOverride io.Writer

// agentErrorWriter picks the channel. Stdout while it is still clean, stderr
// once a result document has claimed it.
func agentErrorWriter() io.Writer {
	if agentErrorWriterOverride != nil {
		return agentErrorWriterOverride
	}
	if agentStdoutClaimed {
		return os.Stderr
	}
	return os.Stdout
}

// finalizeAgentErrorEnvelope is the process-wide handler, deferred from
// Execute so it covers EVERY exit: cobra's pre-RunE usage errors, dial
// failures, and typed refusals alike.
func finalizeAgentErrorEnvelope(flags *rootFlags, retErr *error) {
	if retErr == nil {
		return
	}
	err := *retErr
	if err != nil {
		emitAgentErrorEnvelopeOnce(flags, err, ExitCode(err))
	}
	releaseAgentEnvelopeSlot(flags)
	agentStdoutClaimed = false
}

// usageEnvelopeErr is the hand-written commands' bare-invocation path. It
// emits the SAME envelope the process-wide handler would, marks it emitted so
// there is exactly one document on stdout, and returns the usage error.
//
// Generated required-input commands keep their own short {error, usage}
// object; theirs is a usage document rather than an error envelope, and the
// two compose as a JSON stream.
func usageEnvelopeErr(flags *rootFlags, err error) error {
	wrapped := usageErr(err)
	emitAgentErrorEnvelopeOnce(flags, wrapped, 2)
	return wrapped
}
