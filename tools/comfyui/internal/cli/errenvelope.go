// Structured error envelope for machine callers.
//
// NOT generated — markerless on purpose, so `printing-press generate --force`
// preserves it. Do not add the generated-file marker.
//
// THE PROBLEM THIS SOLVES. Before this file, a failing invocation under
// --json/--agent gave a machine caller nothing machine-readable. Only the
// HTTP-409 branch of classifyAPIError emitted anything structured, and it
// emitted {error, code} — a prose string and an int. Everything else (cobra
// usage errors, dial failures, every typed domain exit) reached the caller as
// a bare `Error: ...` line on stderr, so an agent had to string-match prose to
// find out whether to retry, fix its arguments, or give up.
//
// THE CONTRACT. Under --json/--agent every failing exit now emits exactly one:
//
//	{"ok": false,
//	 "error": {"code": "wait_timeout", "category": "domain", "retryable": true,
//	           "http_status": 0, "message": "...", "remediation": "...",
//	           "exit_code": 20}}
//
// FIELD NAMES ARE FROZEN AND SHARED WITH THE SIBLING CLI (llamaswap-pp-cli).
// An operator drives both against the same box, and an agent that learns to
// read one must read the other without a second parser. Do not rename, do not
// add a field on one side only.
//
//   - code        stable machine token; the thing to branch on. Never
//     localised, never reworded once shipped.
//   - category    coarse bucket for callers that do not know a specific code:
//     usage | config | network | client | server | domain.
//   - retryable   whether re-running the SAME invocation unchanged could
//     plausibly succeed. A wait timeout is retryable; an invalid
//     graph is not. Conservative: false when unknown.
//   - http_status the upstream status when the failure came from an HTTP
//     reply, else 0. Never invented.
//   - message     the human error text, verbatim, hints included.
//   - remediation one concrete next action, naming a real command where one
//     exists. Empty rather than vague.
//   - exit_code   the process exit code, so a caller reading the envelope and
//     a caller reading $? agree.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// errorEnvelope is the top-level failure document.
type errorEnvelope struct {
	OK    bool            `json:"ok"`
	Error errorEnvelopeIn `json:"error"`
}

// errorEnvelopeIn carries the classification. Field order here is the emitted
// order; keep it stable so golden fixtures stay readable in diffs.
type errorEnvelopeIn struct {
	Code        string `json:"code"`
	Category    string `json:"category"`
	Retryable   bool   `json:"retryable"`
	HTTPStatus  int    `json:"http_status"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
	ExitCode    int    `json:"exit_code"`
}

// Error categories.
const (
	errCategoryUsage   = "usage"
	errCategoryConfig  = "config"
	errCategoryNetwork = "network"
	errCategoryClient  = "client"
	errCategoryServer  = "server"
	errCategoryDomain  = "domain"
)

// httpStatusPattern extracts the upstream status from an error message. The
// generated client formats HTTP failures as "HTTP 409: ..." and
// classifyAPIError branches on that same substring, so this reads the value
// that is already there rather than threading a status through every call
// site. Anchored to the literal shape the client emits; when it does not
// match, http_status stays 0 rather than being guessed.
var httpStatusPattern = regexp.MustCompile(`\bHTTP (\d{3})\b`)

func httpStatusFromError(err error) int {
	if err == nil {
		return 0
	}
	m := httpStatusPattern.FindStringSubmatch(err.Error())
	if m == nil {
		return 0
	}
	code, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return 0
	}
	return code
}

// networkFailureMessages are the substrings that mean "we never got an answer
// from ComfyUI". Kept in sync with data_source.go's isNetworkError, which
// makes the same judgement for the live/local fallback decision — the two must
// agree or a fallback and an exit code will tell different stories about the
// same failure.
var networkFailureMessages = []string{
	"connection refused",
	"no such host",
	"network is unreachable",
	"connectex",
	"dial tcp",
	"i/o timeout",
	"context deadline exceeded",
	"unexpected eof",
	"tls handshake",
	"api unreachable",
}

func isNetworkFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range networkFailureMessages {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// classifyErrorEnvelope maps a finished error plus its exit code onto the
// envelope. The exit code is the primary key because it is already the
// contract every command declares in pp:typed-exit-codes; message inspection
// only refines what the code cannot express (which of two meanings a shared
// code carries, and whether a generic failure was actually a dead socket).
func classifyErrorEnvelope(err error, exitCode int) errorEnvelopeIn {
	out := errorEnvelopeIn{
		Code:       "error",
		Category:   errCategoryClient,
		Retryable:  false,
		HTTPStatus: httpStatusFromError(err),
		ExitCode:   exitCode,
	}
	if err != nil {
		out.Message = err.Error()
	}

	switch exitCode {
	case ExitUsage:
		out.Code, out.Category = "usage", errCategoryUsage
		out.Remediation = "Re-read the command's contract: comfyui-pp-cli <command> --help, or comfyui-pp-cli agent-context --pretty for the whole tree."
	case ExitNotFound:
		out.Code, out.Category = "not_found", errCategoryClient
		out.Remediation = "List what exists before naming an id. ComfyUI's history is RAM-only and is destroyed on restart, so a valid-looking prompt_id can simply be gone."
	case ExitServerUnreachable:
		out.Code, out.Category = "server_unreachable", errCategoryNetwork
		out.Retryable = true
		out.Remediation = "Confirm ComfyUI is listening and reachable: comfyui-pp-cli doctor --json. Check --host if the server is not on the default address."
	case ExitAPI:
		// A dial failure classified as a generic API error is really a
		// network failure; say so rather than blaming the request.
		if isNetworkFailure(err) {
			out.Code, out.Category = "server_unreachable", errCategoryNetwork
			out.Retryable = true
			out.Remediation = "Confirm ComfyUI is listening and reachable: comfyui-pp-cli doctor --json."
			break
		}
		out.Code, out.Category = "api_error", errCategoryServer
		if out.HTTPStatus >= 500 {
			out.Code = "upstream_5xx"
			out.Retryable = true
			out.Remediation = "The server failed rather than refused. Retry; if it persists, check the ComfyUI console for the traceback."
		} else {
			out.Remediation = "The server was reached and rejected the request. The response body carries the reason; for a graph, node_errors names the offending node."
		}
	case ExitPartialFailure:
		out.Code, out.Category = "partial_failure", errCategoryServer
		out.Remediation = "Some operations in the batch failed. Read partial_failure in the result envelope; pass --allow-partial-failure to downgrade this to a warning."
	case ExitRateLimit:
		out.Code, out.Category = "rate_limited", errCategoryServer
		out.Retryable = true
		out.Remediation = "Slow down with --rate-limit, then retry."
	case ExitConfigInvalid:
		out.Code, out.Category = "config_invalid", errCategoryConfig
		out.Remediation = "Check the config file named in the message, or pass --config explicitly. comfyui-pp-cli doctor --json reports what was loaded."
	case ExitNodeClassDrift:
		out.Code, out.Category = "node_class_drift", errCategoryDomain
		out.Remediation = "The live schema disagrees with the graph. Re-read it: comfyui-pp-cli nodes options <ClassType> <input>. An EMPTY option list means the model class is unregistered (a missing extra_model_paths.yaml key), not a missing file."
	case ExitGraphInvalid:
		// 13 is shared by validate (graph invalid) and models why (file not
		// visible). Disambiguate on the message so the code field is precise
		// even though the exit code cannot be.
		if strings.Contains(strings.ToLower(errText(err)), "not visible") ||
			strings.Contains(strings.ToLower(errText(err)), "no loader") {
			out.Code, out.Category = "model_not_visible", errCategoryDomain
			out.Remediation = "ComfyUI cannot see that file from any loader. comfyui-pp-cli models why <file> separates the four causes; a folder missing from extra_model_paths.yaml is the usual one."
			break
		}
		out.Code, out.Category = "graph_invalid", errCategoryDomain
		out.Remediation = "The graph will not run as written. comfyui-pp-cli validate <graph.json> --json lists every offending node; ComfyUI has no validate-only endpoint, so this is the only dry run that exists."
	case ExitWaitTimeout:
		out.Code, out.Category = "wait_timeout", errCategoryDomain
		out.Retryable = true
		out.Remediation = "The render is very probably still going — this is not a failure. Re-attach with comfyui-pp-cli wait <prompt_id>, or raise --timeout."
	case ExitJobFailed:
		out.Code, out.Category = "job_failed", errCategoryDomain
		out.Remediation = "The server reported execution_error. The exception text is in message; comfyui-pp-cli status <prompt_id> --json carries the full record."
	case ExitOutputsPending:
		out.Code, out.Category = "outputs_pending", errCategoryDomain
		out.Retryable = true
		out.Remediation = "The run succeeded but /history has not published its outputs yet. This is not a zero-output render — re-check with comfyui-pp-cli status <prompt_id>, or raise --outputs-settle."
	case ExitSubmitMalformed:
		out.Code, out.Category = "submit_malformed", errCategoryServer
		out.Remediation = "A 2xx reply carried no prompt_id, so there is no handle to poll. Confirm the address really is ComfyUI and not a proxy or another service."
	case ExitExecutionInterrupted:
		out.Code, out.Category = "execution_interrupted", errCategoryDomain
		out.Retryable = true
		out.Remediation = "The run was interrupted, not broken. Re-submitting the same graph unchanged is the correct response."
	case ExitUpstreamOOM:
		out.Code, out.Category = "upstream_oom", errCategoryServer
		out.Retryable = true
		out.Remediation = "Out of memory. Change the resource plan, not the graph: lower resolution or batch size, or release VRAM with comfyui-pp-cli free --execute. On a shared-GPU box, check what else is resident first."
	case ExitUpstream5xx:
		out.Code, out.Category = "upstream_5xx", errCategoryServer
		out.Retryable = true
		out.Remediation = "The server failed rather than refused. Retry; if it persists, read the ComfyUI console for the traceback."
	default:
		if isNetworkFailure(err) {
			out.Code, out.Category = "server_unreachable", errCategoryNetwork
			out.Retryable = true
			out.Remediation = "Confirm ComfyUI is listening and reachable: comfyui-pp-cli doctor --json."
		}
	}

	// Codes 21/22 are shared across the submit and wait families. The wait
	// meanings are handled above; refine to the submit meanings when the
	// message shows this came from a submission.
	//
	// Retryable is reset EXPLICITLY in both branches rather than inherited.
	// The wait meaning of 22 (outputs-pending) is retryable and the submit
	// meaning (partial accept) is emphatically not — re-submitting duplicates
	// the branches that DID queue. Leaving retryable at whatever the switch
	// above set is how this refinement told an agent to double a render.
	switch {
	case exitCode == ExitSubmitRejected && strings.Contains(errText(err), "rejected, nothing was queued"):
		out.Code, out.Category, out.Retryable = "submit_rejected", errCategoryDomain, false
		out.Remediation = "The whole graph was rejected and nothing queued. node_errors in the message names the offending node; comfyui-pp-cli validate <graph.json> catches these before submitting."
	case exitCode == ExitSubmitPartialAccept && strings.Contains(errText(err), "partial accept"):
		out.Code, out.Category, out.Retryable = "submit_partial_accept", errCategoryDomain, false
		out.Remediation = "Some output branches queued and others were dropped — never treat this as success. Re-submitting duplicates the branches that DID queue; fix the dropped ones and submit only those."
	}

	return out
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// wantsErrorEnvelope reports whether this invocation should get the structured
// envelope instead of a prose error line.
//
// os.Args is consulted as well as the parsed flags because the parse-failure
// path never reaches PersistentPreRunE, which is where --agent turns on
// --json. Without the argv scan, the single most common machine failure — a
// malformed invocation under --agent — would be the one case that still got
// bare prose. Mirrors the argsDisableLearn(os.Args[1:]) pattern in root.go,
// which exists for exactly the same reason.
func wantsErrorEnvelope(flags *rootFlags) bool {
	if flags != nil && (flags.asJSON || flags.agent) {
		return true
	}
	for _, arg := range os.Args[1:] {
		if arg == "--" {
			break
		}
		if arg == "--json" || arg == "--agent" || strings.HasPrefix(arg, "--json=") || strings.HasPrefix(arg, "--agent=") {
			return true
		}
	}
	return false
}

// errorEnvelopeStream picks the stream for the envelope.
//
// stdout is the default so a machine caller reads one JSON document from the
// channel it already reads. When the command ALREADY wrote a structured
// document to stdout — a wait envelope, a mutate envelope — and then returned
// a typed error, a second document on stdout would make the stream invalid
// JSON for a naive reader. In that case the envelope goes to stderr, which is
// where the caller looks for diagnostics anyway, and stdout stays exactly one
// document. flags.structuredWritten is set by the two output funnels.
func errorEnvelopeStream(flags *rootFlags) io.Writer {
	if flags != nil && flags.structuredWritten {
		return os.Stderr
	}
	return os.Stdout
}

// finalizeErrorOutput is Execute()'s single error-reporting site. It replaces
// Cobra's own error printing (suppressed via SilenceErrors) so that exactly
// one representation of a failure reaches the caller: the structured envelope
// for machines, one prose line for humans.
//
// Never returns an error: a failure to report a failure must not change the
// exit code the caller is about to receive.
func finalizeErrorOutput(flags *rootFlags, err error) {
	if err == nil {
		return
	}
	if !wantsErrorEnvelope(flags) {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return
	}
	env := errorEnvelope{OK: false, Error: classifyErrorEnvelope(err, ExitCode(err))}
	w := errorEnvelopeStream(flags)
	enc := json.NewEncoder(w)
	if encErr := enc.Encode(env); encErr != nil {
		// Falling back to prose is strictly better than emitting nothing.
		fmt.Fprintln(os.Stderr, "Error:", err)
	}
}
