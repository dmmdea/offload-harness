package agent

import (
	"strings"
	"testing"
)

func TestSystemPromptCapabilityAware(t *testing.T) {
	ro := SystemPrompt(false, false, false)
	for _, bad := range []string{"write_file", "web_fetch", "run_shell"} {
		if strings.Contains(ro, bad) {
			t.Errorf("read-only prompt must not advertise %q: %q", bad, ro)
		}
	}
	if !strings.Contains(ro, "read files") {
		t.Errorf("read-only prompt should still offer read; got %q", ro)
	}
	w := SystemPrompt(true, false, false)
	if !strings.Contains(w, "write_file") || strings.Contains(w, "run_shell") || strings.Contains(w, "web_fetch") {
		t.Errorf("write-only prompt wrong: %q", w)
	}
	f := SystemPrompt(false, true, false)
	if !strings.Contains(f, "web_fetch") || !strings.Contains(f, "UNTRUSTED_WEB_CONTENT") || strings.Contains(f, "run_shell") {
		t.Errorf("fetch-only prompt wrong: %q", f)
	}
	sh := SystemPrompt(false, false, true)
	if !strings.Contains(sh, "run_shell") || !strings.Contains(sh, "OS sandbox") || strings.Contains(sh, "write_file") || strings.Contains(sh, "web_fetch") {
		t.Errorf("shell-only prompt wrong: %q", sh)
	}
	all := SystemPrompt(true, true, true)
	if !strings.Contains(all, "write_file") || !strings.Contains(all, "web_fetch") || !strings.Contains(all, "run_shell") {
		t.Errorf("all-caps prompt should advertise all three: %q", all)
	}
}
