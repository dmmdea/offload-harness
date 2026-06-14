// Package verifier runs structural quality checks on a model response and
// decides whether to retry, accept, or defer. (SmallCode's quality monitor,
// minus the coding-agent rollback bits.)
package verifier

import "strings"

// Verdict is the outcome of a quality check.
type Verdict struct {
	OK     bool
	Retry  bool   // worth one corrective re-prompt
	Reason string // populated when !OK
	// Terminal marks a failure that a LARGER tier cannot fix either, so the
	// pipeline should defer to Opus immediately instead of escalating. Truncation
	// is the case: every local tier shares the same context window, so climbing
	// to the slow 26B-A4B just burns compute before deferring anyway.
	Terminal bool
}

// Check inspects raw content, whether it was truncated, and any parse error.
func Check(content string, truncated bool, parseErr error) Verdict {
	if strings.TrimSpace(content) == "" {
		return Verdict{Retry: true, Reason: "empty model output"}
	}
	if truncated {
		// Output hit the token limit mid-structure; a retry rarely helps and a
		// bigger tier has the same context window — the input is too large for
		// any local tier. Defer straight to Opus (Terminal), don't escalate.
		return Verdict{Retry: false, Terminal: true, Reason: "output truncated (hit token limit)"}
	}
	if parseErr != nil {
		return Verdict{Retry: true, Reason: "unparseable output: " + parseErr.Error()}
	}
	return Verdict{OK: true}
}
