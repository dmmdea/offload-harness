// Typed exit codes for unattended callers (nightshift agents, the harness).
//
// NOT generated — markerless on purpose, so `printing-press generate --force`
// preserves it. Do not add the generated-file marker. Mirrors the sibling
// contract in llamaswap-pp-cli's internal/cli/exitcodes.go so an operator
// driving both CLIs reads one taxonomy, not two.
//
// This file is the SINGLE registry of every non-zero code this CLI can exit
// with. Before this file the domain codes were scattered across four files
// (comfy_jobs.go, comfy_nodes.go, comfy_slots.go, internal/comfy/submit) with
// no shared view, which is how 12/13 and 21/22 each came to carry two
// meanings without anyone noticing. Nothing is renumbered here — every value
// below is the value the shipped binary already used — but every one now has
// a name, and the reuses are documented rather than accidental.
//
// Rules for adding to this file:
//
//  1. A command that exits with a domain code for non-error control flow MUST
//     declare it in cmd.Annotations["pp:typed-exit-codes"] as a comma-separated
//     list (e.g. "0,2,3,20,21,22") and document the codes in its help text.
//     `agent-context` surfaces that annotation, so the annotation — not this
//     file — is what an agent reads to learn a specific command's contract.
//  2. Never reuse a number for a NEW meaning. The four historical reuses below
//     are grandfathered and command-scoped; they are the reason rule 1 exists.
//  3. Reference these constants, never raw ints.

package cli

import (
	"comfyui-pp-cli/internal/comfy/exp"
	"comfyui-pp-cli/internal/comfy/submit"
)

// Compile-time guard: internal/comfy/submit declares its own exit codes so the
// submit classifier stays usable without importing internal/cli. That is a
// second source of truth, and a second source of truth drifts. These
// assertions fail the BUILD the moment the two disagree, which is the only
// enforcement that cannot be forgotten in review.
const (
	_ = uint(ExitSubmitRejected - submit.ExitRejected)
	_ = uint(submit.ExitRejected - ExitSubmitRejected)
	_ = uint(ExitSubmitPartialAccept - submit.ExitPartialAccept)
	_ = uint(submit.ExitPartialAccept - ExitSubmitPartialAccept)
	_ = uint(ExitSubmitMalformed - submit.ExitMalformed)
	_ = uint(submit.ExitMalformed - ExitSubmitMalformed)
)

// Framework codes. Emitted by the generated helpers in helpers.go
// (usageErr, notFoundErr, authErr, apiErr, partialFailureErr, rateLimitErr,
// configErr) and by root.go's cobra usage-error wrap. Listed here so the
// registry is complete; the constructors stay where the generator put them.
const (
	// ExitOK — the command did what it was asked.
	ExitOK = 0

	// ExitError — unclassified failure. An agent seeing this has no contract
	// to branch on and should read the error envelope's message.
	ExitError = 1

	// ExitUsage — bad invocation: unknown flag, unknown command, missing
	// required flag, unparseable flag value. Raised by cobra/pflag before any
	// RunE runs and wrapped in root.go's Execute.
	ExitUsage = 2

	// ExitNotFound — the named resource does not exist (HTTP 404, or a local
	// lookup that resolved to nothing).
	ExitNotFound = 3

	// ExitServerUnreachable — the CLI could not successfully talk to ComfyUI:
	// dial failure, DNS failure, connection refused, TLS failure, or an
	// intervening reverse proxy demanding credentials (HTTP 401/403).
	//
	// Aligned with the sibling's ExitServerUnreachable = 4. ComfyUI itself is
	// unauthenticated (spec auth_type: none), so in practice 4 is unambiguous
	// here: there is no ComfyUI credential to get wrong, and every route to
	// this code means "you could not reach the render server". The generated
	// authErr() constructor also lives on 4, which is why the 401/403 case is
	// folded into the same meaning rather than given a code of its own.
	ExitServerUnreachable = 4

	// ExitAPI — the server was reached and rejected or failed the request in
	// a way with no more specific code below.
	ExitAPI = 5

	// ExitPartialFailure — a 2xx whose body reported that some operations in a
	// batch failed. Downgraded to a warning by --allow-partial-failure.
	ExitPartialFailure = 6

	// ExitRateLimit — HTTP 429.
	ExitRateLimit = 7

	// ExitConfigInvalid — local config could not be loaded or is invalid.
	ExitConfigInvalid = 10
)

// Domain codes. Everything below is ComfyUI-specific and sits above the
// framework range so a caller can branch on a render-domain outcome without
// parsing text.
const (
	// ExitNodeClassDrift — the graph and the live node schema disagree about a
	// class. Two commands raise it, both meaning "the class is not what you
	// assumed":
	//
	//   nodes options / models why — the COMBO input exists but offers ZERO
	//     options, i.e. the model CLASS is unregistered (a missing
	//     extra_model_paths.yaml key). Deliberately NOT 3: nothing is missing
	//     on disk, so an agent must be able to branch on this without
	//     string-matching a message.
	//   set — the addressed node no longer holds the class it held when the
	//     slot address was captured, so the patch was refused rather than
	//     applied to the wrong node.
	ExitNodeClassDrift = 12

	// ExitGraphInvalid — validate found the graph will not run: unknown class,
	// missing required input, or a COMBO value absent from the option list.
	// ComfyUI has no validate-only endpoint, so this is a client-side verdict
	// against the cached schema.
	//
	// COMPATIBILITY: 13 is also ExitModelNotVisible (below). validate has
	// exited 13 since it shipped and keeps doing so — renumbering it would
	// break every caller that already branches on it. The two meanings never
	// collide in practice because they belong to different commands, and each
	// command declares its own set in pp:typed-exit-codes.
	ExitGraphInvalid = 13

	// ExitModelNotVisible — models why: the file is offered by no loader and
	// no COMBO is empty (not-listed / no-such-input). Separate name from
	// ExitNodeClassDrift so "unregistered class" and "file not in a registered
	// folder" never collapse into one branch. Shares 13 with ExitGraphInvalid;
	// see the compatibility note above.
	ExitModelNotVisible = 13

	// ExitWaitTimeout — `wait --timeout` expired while the job was still
	// non-terminal. Distinct from a failure: the render is very probably still
	// going, and the prompt_id remains valid to re-attach to.
	ExitWaitTimeout = 20

	// ExitJobFailed — the job reached a terminal FAILED state. Since this
	// wave, interruption and OOM are split out into ExitExecutionInterrupted
	// and ExitUpstreamOOM, so 21 now means a genuine execution_error that is
	// neither of those.
	ExitJobFailed = 21

	// ExitSubmitRejected — the server rejected the whole graph at submit time
	// (validation); nothing was queued. Shares 21 with ExitJobFailed: submit
	// and wait are different commands and each declares its own set.
	ExitSubmitRejected = 21

	// ExitOutputsPending — the job recorded execution_success but /history
	// still published no outputs when the settle window expired. Not a
	// success, not a failure: an honest "the server has not told us what it
	// made yet".
	ExitOutputsPending = 22

	// ExitSubmitPartialAccept — the server queued SOME output branches and
	// dropped others. Never treat as success. Shares 22 with
	// ExitOutputsPending across the submit/wait command split.
	ExitSubmitPartialAccept = 22

	// ExitSubmitMalformed — a 2xx reply carried no prompt_id, so there is no
	// handle to attach to and no way to poll.
	ExitSubmitMalformed = 23

	// ExitExecutionInterrupted — the run ended because it was interrupted
	// (queue interrupt, or a shutdown), not because the graph is wrong.
	// Retrying the same graph unchanged is the correct response, which is
	// exactly why it must not stay folded into ExitJobFailed.
	//
	// NEW in the perfection wave. Previously `wait` reported interruption as
	// 21; a caller distinguishing "my graph is broken" from "someone hit
	// interrupt" had to string-match the exception text.
	ExitExecutionInterrupted = 24

	// ExitUpstreamOOM — the run died allocating memory (CUDA OOM, host OOM).
	// A first-class code because it is the one failure whose correct response
	// is to change the RESOURCE plan (smaller batch, lower resolution, free
	// VRAM, or `free --unload-models`) rather than the graph. On a two-GPU box
	// shared with llama-swap this is the expected contention failure.
	//
	// NEW in the perfection wave. Detection reuses exp.ClassifyFailure, so the
	// sweep runner and the wait path agree on what an OOM looks like.
	ExitUpstreamOOM = 25

	// ExitUpstream5xx — ComfyUI answered 5xx. Separated from ExitAPI so an
	// agent can distinguish "the server is broken or wedged" (retry later,
	// consider a restart) from "the server refused this request" (fix the
	// request).
	//
	// NEW in the perfection wave.
	ExitUpstream5xx = 26
)

// unreachableErr — the CLI never got an answer from ComfyUI. Raised by
// classifyAPIError for a statusless failure naming a dead socket.
func unreachableErr(err error) error { return &cliError{code: ExitServerUnreachable, err: err} }

// upstream5xxErr — ComfyUI answered 5xx. Separate from apiErr so "the server
// is broken" and "the server refused this request" are different branches.
func upstream5xxErr(err error) error { return &cliError{code: ExitUpstream5xx, err: err} }

// exitCodeForFailureClass maps an exp.ClassifyFailure class to the typed exit
// code for a terminal render failure. Keeping the mapping in one function is
// what stops `wait`, `status`, `replay` and the sweep runner from drifting
// into three different opinions about what an OOM exits with.
//
// Returns ExitJobFailed for the generic and unrecognised classes: a failure
// with no more specific code is still a failure.
func exitCodeForFailureClass(class string) int {
	switch class {
	case exp.ExitOOM:
		return ExitUpstreamOOM
	case exp.ExitInterrupted:
		return ExitExecutionInterrupted
	case exp.ExitValidation:
		return ExitGraphInvalid
	case exp.ExitMissingModel:
		return ExitModelNotVisible
	default:
		return ExitJobFailed
	}
}

// comfyTerminalFailureErr builds the typed error for a terminal render
// failure, classifying the server's own exception type and message so the
// exit code carries the diagnosis. err is returned unwrapped in the envelope,
// so the exception text still reaches the operator verbatim.
func comfyTerminalFailureErr(excType, message string, err error) error {
	return &cliError{code: exitCodeForFailureClass(exp.ClassifyFailure(excType, message)), err: err}
}
