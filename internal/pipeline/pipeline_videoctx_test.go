package pipeline

import "testing"

// isContextOverflow drives runVideoDescribe's resolution-halving retry: it must
// fire on a VLM ctx-overflow defer (so a hi-res clip degrades gracefully instead
// of hard-failing) and stay quiet on every other defer reason.
func TestIsContextOverflow(t *testing.T) {
	overflow := []string{
		`vision model call failed: llama-server 400: {"error":{"code":400,"message":"request (5584 tokens) exceeds the available context size (4096 tokens), try increasing it","type":"exceed_context_size_error"}}`,
		"request EXCEEDS THE AVAILABLE CONTEXT size",
		"prompt is larger than the context size",
	}
	for _, s := range overflow {
		if !isContextOverflow(s) {
			t.Errorf("expected overflow detection for: %q", s)
		}
	}
	notOverflow := []string{
		"empty vision output",
		"frame sampling: ffmpeg failed",
		"vision output truncated",
		"no vision model configured",
		"",
	}
	for _, s := range notOverflow {
		if isContextOverflow(s) {
			t.Errorf("false positive overflow detection for: %q", s)
		}
	}
}
