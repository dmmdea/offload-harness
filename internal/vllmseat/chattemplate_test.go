package vllmseat

import (
	"strings"
	"testing"
)

// TestSeatRendersChatTemplateAndPromptTokensDetails: a seat that names the shipped fold template
// renders --chat-template with the template's INSTALLED path inside the WSL seat directory, emits
// the template itself as an artifact at that relative path (verbatim, LF), and renders
// --enable-prompt-tokens-details when asked.
func TestSeatRendersChatTemplateAndPromptTokensDetails(t *testing.T) {
	s := pipelineFlagship()
	s.ChatTemplate = "qwen3-fold-system.jinja"
	s.EnablePromptTokensDetails = true
	if err := s.Validate("blackwell-3x16"); err != nil {
		t.Fatalf("validate: %v", err)
	}
	files, err := s.Artifacts(wslTemplatesDir(), wslRT())
	if err != nil {
		t.Fatal(err)
	}
	env := files["qwen3.8-27b-vllm-3card.env"]
	for _, want := range []string{
		"--chat-template /root/g7/templates/qwen3-fold-system.jinja",
		"--enable-prompt-tokens-details",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("rendered env is missing %q", want)
		}
	}
	tmpl, ok := files["templates/qwen3-fold-system.jinja"]
	if !ok {
		t.Fatalf("the shipped template was not emitted as an artifact; got %v", keys(files))
	}
	if !strings.Contains(tmpl, "messages[0].role == 'system' and messages[1].role == 'system'") {
		t.Error("the emitted template does not carry the system-fold block")
	}
	if !strings.Contains(tmpl, "raise_exception('System message must be at the beginning.')") {
		t.Error("the emitted template lost the upstream structure it is based on")
	}
	if strings.Contains(tmpl, "\r") {
		t.Error("the emitted template carries a CR; it must be LF only (a CR in a string literal reaches the prompt)")
	}
}

// TestSeatWithoutChatTemplateRendersNeitherFlag: the flags are opt-in; an unchanged seat renders
// exactly what it did before, and no template artifact.
func TestSeatWithoutChatTemplateRendersNeitherFlag(t *testing.T) {
	files, err := pipelineFlagship().Artifacts(wslTemplatesDir(), wslRT())
	if err != nil {
		t.Fatal(err)
	}
	env := files["qwen3.8-27b-vllm-3card.env"]
	if strings.Contains(env, "--chat-template") || strings.Contains(env, "--enable-prompt-tokens-details") {
		t.Errorf("a seat that asks for neither rendered one: %q", env)
	}
	for k := range files {
		if strings.HasPrefix(k, "templates/") {
			t.Errorf("unexpected template artifact %s", k)
		}
	}
}

// TestSeatChatTemplateAbsolutePathPassesThrough: an operator-kept template on the serving box is
// named by its absolute path, passed as-is, and nothing is emitted for it.
func TestSeatChatTemplateAbsolutePathPassesThrough(t *testing.T) {
	s := pipelineFlagship()
	s.ChatTemplate = "/srv/templates/custom.jinja"
	files, err := s.Artifacts(wslTemplatesDir(), wslRT())
	if err != nil {
		t.Fatal(err)
	}
	if env := files["qwen3.8-27b-vllm-3card.env"]; !strings.Contains(env, "--chat-template /srv/templates/custom.jinja") {
		t.Errorf("absolute chat_template not passed through: %q", env)
	}
	if len(files) != 5 {
		t.Errorf("an absolute chat_template must emit no artifact; got %v", keys(files))
	}
}

func TestSeatChatTemplateRefusesBadValues(t *testing.T) {
	cases := map[string]func(*Spec){
		"a relative path":                  func(s *Spec) { s.ChatTemplate = "templates/x.jinja" },
		"a path with a space":              func(s *Spec) { s.ChatTemplate = "/srv/my templates/x.jinja" },
		"a shipped name that is not jinja": func(s *Spec) { s.ChatTemplate = "fold.txt" },
		"the linux-systemd launch, which renders no argument tail": func(s *Spec) {
			s.ChatTemplate = "qwen3-fold-system.jinja"
			s.Launch = LaunchLinuxSystemd
			s.PipelineParallel, s.LayerPartition, s.KVCacheMemoryBytes, s.Device, s.TensorParallel = 0, "", 0, "0", 1
		},
		"prompt-tokens-details on linux-systemd": func(s *Spec) {
			s.EnablePromptTokensDetails = true
			s.Launch = LaunchLinuxSystemd
			s.PipelineParallel, s.LayerPartition, s.KVCacheMemoryBytes, s.Device, s.TensorParallel = 0, "", 0, "0", 1
		},
	}
	for name, mutate := range cases {
		s := pipelineFlagship()
		mutate(&s)
		if err := s.Validate("t"); err == nil {
			t.Errorf("%s: validated, want a refusal", name)
		}
	}
	missing := pipelineFlagship()
	missing.ChatTemplate = "no-such-template.jinja"
	if _, err := missing.Artifacts(wslTemplatesDir(), wslRT()); err == nil || !strings.Contains(err.Error(), "no-such-template.jinja") {
		t.Errorf("a shipped template that does not exist must refuse the render, got %v", err)
	}
}
