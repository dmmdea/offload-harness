package tierdocs

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// A windows-wsl seat is not rendered by the Windows installer (it binds the tier's fallback
// seat); `local-offload install vllm-seat` renders its env and Windows scripts. The tier page
// says the installer RENDERS the seat, so for this launch it also has to name the command and
// the box-side inputs it needs, or a reader of the page has no way to produce the seat it
// describes. A seat that starts through systemd needs none of that sentence.
func TestWindowsWSLSeatPageNamesTheCommandThatRendersIt(t *testing.T) {
	lane := laneSpec()
	lane.Launch = vllmseat.LaunchWindowsWSL
	page := renderTier("t", Profile{CtxSize: 8192, VLLMSeat: lane}, nil)
	for _, want := range []string{
		"install vllm-seat", "--distro", "--lmcache-overlay", "--host-prefix", "(../OPERATOR-GUIDE.md)",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page of a windows-wsl seat does not mention %q:\n%s", want, page)
		}
	}

	lane = laneSpec()
	page = renderTier("t", Profile{CtxSize: 8192, VLLMSeat: lane}, nil)
	if strings.Contains(page, "install vllm-seat") {
		t.Errorf("a seat that starts through systemd must not carry the windows-wsl render note:\n%s", page)
	}
}
