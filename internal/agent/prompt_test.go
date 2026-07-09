package agent

import (
	"strings"
	"testing"
)

func TestSystemPromptAdvertisesEditWhenOpenWrite(t *testing.T) {
	open := SystemPrompt(true, true, false, false, false, false)
	if !strings.Contains(open, "edit_file") {
		t.Errorf("open-write prompt should advertise edit_file:\n%s", open)
	}
	closed := SystemPrompt(true, false, false, false, false, false)
	if strings.Contains(closed, "edit_file") {
		t.Errorf("create-only prompt must NOT advertise edit_file:\n%s", closed)
	}
	if !strings.Contains(closed, "REFUSED") {
		t.Errorf("create-only prompt should still warn overwrite is refused")
	}
}

func TestSystemPromptCapabilityAware(t *testing.T) {
	ro := SystemPrompt(false, false, false, false, false, false)
	for _, bad := range []string{"write_file", "web_fetch", "run_shell", "web_search", "github_"} {
		if strings.Contains(ro, bad) {
			t.Errorf("read-only prompt must not advertise %q: %q", bad, ro)
		}
	}
	if !strings.Contains(ro, "read files") {
		t.Errorf("read-only prompt should still offer read; got %q", ro)
	}
	w := SystemPrompt(true, false, false, false, false, false)
	if !strings.Contains(w, "write_file") || strings.Contains(w, "run_shell") || strings.Contains(w, "web_fetch") {
		t.Errorf("write-only prompt wrong: %q", w)
	}
	f := SystemPrompt(false, false, true, false, false, false)
	if !strings.Contains(f, "web_fetch") || !strings.Contains(f, "UNTRUSTED_WEB_CONTENT") || strings.Contains(f, "run_shell") {
		t.Errorf("fetch-only prompt wrong: %q", f)
	}
	sh := SystemPrompt(false, false, false, true, false, false)
	if !strings.Contains(sh, "run_shell") || !strings.Contains(sh, "OS sandbox") || strings.Contains(sh, "write_file") || strings.Contains(sh, "web_fetch") {
		t.Errorf("shell-only prompt wrong: %q", sh)
	}
	all := SystemPrompt(true, false, true, true, false, false)
	if !strings.Contains(all, "write_file") || !strings.Contains(all, "web_fetch") || !strings.Contains(all, "run_shell") {
		t.Errorf("all-caps prompt should advertise all three: %q", all)
	}
}

func TestSystemPromptSearchAndGitHub(t *testing.T) {
	s := SystemPrompt(false, false, false, false, true, true)
	for _, want := range []string{"web_search", "github_create_repo", "github_upload_file", "github_api", "search the web", "GitHub"} {
		if !strings.Contains(s, want) {
			t.Errorf("search+github prompt should advertise %q:\n%s", want, s)
		}
	}
	// off by default
	none := SystemPrompt(false, false, false, false, false, false)
	if strings.Contains(none, "web_search") || strings.Contains(none, "github_") {
		t.Errorf("default prompt must NOT advertise search/github: %q", none)
	}
}
