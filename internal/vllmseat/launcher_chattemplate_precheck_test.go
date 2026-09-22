package vllmseat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLauncherRefusesAMissingChatTemplateBeforeTheMPServer (review R6): a rendered seat passes
// `--chat-template <seat dir>/templates/<name>`, and that file exists in the distro only if the
// operator copied the render's templates/ directory there. The launcher must check the file and
// refuse, naming it, BEFORE it stops the old MP unit — vLLM would refuse too, but only after the
// wrapper restarted the MP server, with the reason in a traceback and HTTP 500 on the lane.
func TestLauncherRefusesAMissingChatTemplateBeforeTheMPServer(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "vllm-seat", "seat_fg.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	check := strings.Index(s, "# Chat template precheck")
	stop := strings.Index(s, `systemctl stop "$MP_UNIT"`)
	if check < 0 || stop < 0 {
		t.Fatalf("launcher lost a landmark: chat-template precheck=%d mp stop=%d", check, stop)
	}
	if check > stop {
		t.Fatalf("the chat-template precheck (%d) must run before the old MP unit is stopped (%d)", check, stop)
	}
	if strings.Count(s, "# Chat template precheck") != 1 {
		t.Fatal("the launcher carries more than one chat-template precheck")
	}
	for _, want := range []string{
		`--chat-template)   ct_path="${CT_ARGS[$((ct_i + 1))]:-}" ;;`,
		`--chat-template=*) ct_path="${CT_ARGS[$ct_i]#--chat-template=}" ;;`,
		`/*) if [ ! -r "$ct_path" ]; then`,
		"seat_fg: REFUSING to start — --chat-template $ct_path does not exist",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("chat-template precheck lost %q", want)
		}
	}
}

// TestRenderedEnvTellsTheOperatorToCopyTemplates: the rendered env of a seat that names a shipped
// chat template carries the installed path in SEAT_EXTRA_ARGS AND says, in its header, that the
// render's templates/ directory is copied into the seat directory with it.
func TestRenderedEnvTellsTheOperatorToCopyTemplates(t *testing.T) {
	s := pipelineFlagship()
	s.ChatTemplate = "qwen3-fold-system.jinja"
	files, err := s.Artifacts(wslTemplatesDir(), wslRT())
	if err != nil {
		t.Fatal(err)
	}
	env := strings.ReplaceAll(files[s.ID+".env"], "\r\n", "\n")
	if !strings.Contains(env, "--chat-template /root/g7/templates/qwen3-fold-system.jinja") {
		t.Fatal("rendered SEAT_EXTRA_ARGS does not pass the installed template path")
	}
	if !strings.Contains(env, "# Copy ALL of it into /root/g7: when the tier names a shipped chat template, the render includes\n# templates/<name> and SEAT_EXTRA_ARGS names /root/g7/templates/<name>") {
		t.Fatal("the rendered env header no longer tells the operator to copy templates/ into the seat directory")
	}
	if _, ok := files["templates/qwen3-fold-system.jinja"]; !ok {
		t.Fatal("the render does not include templates/qwen3-fold-system.jinja")
	}
}
