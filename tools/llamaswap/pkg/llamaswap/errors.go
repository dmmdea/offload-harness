// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package llamaswap

import (
	"errors"
	"fmt"
)

// Sentinel errors. Every one maps 1:1 onto a llamaswap-pp-cli exit code, so a
// Go caller and a shell script classify the same failure the same way. Compare
// with errors.Is; the concrete values returned wrap these with detail.
var (
	// ErrModelNotFound means the name resolved to nothing in the roster,
	// checked against canonical ids AND meta.llamaswap.aliases. CLI exit 3.
	ErrModelNotFound = errors.New("model not found in roster")

	// ErrKeepsetRefusal means the operation would have touched a protected
	// keep-set member. Nothing was sent to the server. CLI exit 20.
	ErrKeepsetRefusal = errors.New("refused: keep-set member")

	// ErrDrainTimeout means the target was still processing when the drain
	// deadline expired. Nothing was unloaded — the check fails closed.
	// CLI exit 21.
	ErrDrainTimeout = errors.New("drain timed out; nothing unloaded")

	// ErrDrainUnobservable means slot state could not be read (timeout or
	// 5xx), so idleness could not be established. Nothing was unloaded.
	// A 404 from /slots is NOT this error: that is the endpoint being absent,
	// which triggers the documented activity-ring fallback instead.
	// CLI exit 22.
	ErrDrainUnobservable = errors.New("drain unobservable; nothing unloaded")

	// ErrUnreachable means the proxy did not answer at all. CLI exit 4.
	ErrUnreachable = errors.New("llama-swap unreachable")

	// ErrUpstream5xx means the upstream model server answered 5xx through the
	// passthrough. CLI exit 27.
	ErrUpstream5xx = errors.New("upstream model server returned 5xx")

	// ErrNotLoaded means the operation requires the model to already hold
	// VRAM and it does not. Returned instead of silently triggering a
	// multi-GB auto-start via an /upstream probe.
	ErrNotLoaded = errors.New("model is not loaded")
)

// Exit codes mirrored from internal/cli/exitcodes.go. Duplicated here on
// purpose: this package is importable by programs that will never link the CLI,
// and the numbers are part of its public contract.
const (
	// ExitOK is the success code.
	ExitOK = 0
	// ExitGeneric is the catch-all failure code for an error this package
	// does not classify.
	ExitGeneric = 1
	// ExitModelNotFoundCode corresponds to [ErrModelNotFound].
	ExitModelNotFoundCode = 3
	// ExitUnreachableCode corresponds to [ErrUnreachable].
	ExitUnreachableCode = 4
	// ExitKeepsetRefusalCode corresponds to [ErrKeepsetRefusal].
	ExitKeepsetRefusalCode = 20
	// ExitDrainTimeoutCode corresponds to [ErrDrainTimeout].
	ExitDrainTimeoutCode = 21
	// ExitDrainUnobservableCode corresponds to [ErrDrainUnobservable].
	ExitDrainUnobservableCode = 22
	// ExitUpstream5xxCode corresponds to [ErrUpstream5xx].
	ExitUpstream5xxCode = 27
)

// ExitCode maps an error from this package onto the CLI exit code an
// unattended caller should report. A nil error is [ExitOK]; an error this
// package does not recognize is [ExitGeneric].
//
// Use it to give a wrapper program the same taxonomy the CLI already promises:
//
//	if err := c.Unload(ctx, "local-embed", nil); err != nil {
//		os.Exit(llamaswap.ExitCode(err))
//	}
func ExitCode(err error) int {
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, ErrModelNotFound):
		return ExitModelNotFoundCode
	case errors.Is(err, ErrKeepsetRefusal):
		return ExitKeepsetRefusalCode
	case errors.Is(err, ErrDrainTimeout):
		return ExitDrainTimeoutCode
	case errors.Is(err, ErrDrainUnobservable):
		return ExitDrainUnobservableCode
	case errors.Is(err, ErrUpstream5xx):
		return ExitUpstream5xxCode
	case errors.Is(err, ErrUnreachable):
		return ExitUnreachableCode
	}
	return ExitGeneric
}

// HTTPError is returned when the proxy answered with a non-2xx status. The
// status is exposed because callers legitimately branch on it: a 404 from
// /upstream/{model}/slots means "started without --slots" (endpoint absent),
// which is a different fact from a 500 (unobservable).
type HTTPError struct {
	// Status is the HTTP status code the proxy returned.
	Status int
	// Method is the HTTP method of the failing request.
	Method string
	// Path is the request path, relative to the base URL.
	Path string
	// Body is the response body, truncated for readability.
	Body string
}

// Error implements error.
func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d from %s %s: %s", e.Status, e.Method, e.Path, e.Body)
}

// Unwrap maps a 5xx onto [ErrUpstream5xx] so errors.Is classifies it without
// the caller inspecting the status. Other statuses stay unclassified.
func (e *HTTPError) Unwrap() error {
	if e.Status >= 500 {
		return ErrUpstream5xx
	}
	return nil
}
