package placement

import (
	"os"
	"strings"
	"testing"
)

// The composite-tier page is where an operator learns what display_device may be. The sentinel, the
// refusals it adds, and the seed that ships it have to be there in words, and the sentence that said the
// seed ships the layer "with display_device as an index" is retired: it is false once the table says auto.
func TestCompositeTierDocsNameTheAutoSentinel(t *testing.T) {
	raw, err := os.ReadFile("../../docs/systems/composite-tier.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")

	for _, retired := range []string{
		"with `display_device` as an index",
		// Nothing in the probe's notes or tests measures a remote-desktop session or an older driver as a
		// reason for the driver to flag no card: an older driver still flags the monitor's card while the
		// screen is lit (display_active), and display_attached does not depend on the session.
		"remote-desktop",
	} {
		if strings.Contains(text, retired) {
			t.Errorf("the page still says %q, which is retired (a sentence the seed or the probe no longer backs)", retired)
		}
	}
	for _, want := range []string{
		`"display_device": "auto"`,                  // how it is spelled
		"display device auto (index N)",             // how the guard's reasons print it
		"does not choose between cards",             // more than one screen card refuses
		"reports the monitor on more than one card", // what it refuses
		"its pin does not include that card",        // the seat-pin coherence rule
		"audit-yaml --against-render",               // a stamped render goes STALE until re-rendered

		// The causes of "the driver flags no card" are the ones gpuprobe/display.go documents.
		"a screen on an iGPU",
		"a driver that does not report `display_attached`",

		// The watcher asks the same guard of a loaded twin, so the strictness is destructive there.
		"does not name exactly one card",
		"unloaded on that check, with no drain",
		"whether the layer is dormant or not",
		"trusts its declaration through a reading that cannot say",

		// A live config seeded from the earlier table differs from the shipped seed.
		"reports `layers` as `DIFFERENT`",
		"`local-offload audit-config`",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("composite-tier.md must say %q", want)
		}
	}
}
