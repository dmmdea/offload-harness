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
	} {
		if strings.Contains(text, retired) {
			t.Errorf("the page still says %q, which the seed no longer does", retired)
		}
	}
	for _, want := range []string{
		`"display_device": "auto"`,                  // how it is spelled
		"display device auto (index N)",             // how the guard's reasons print it
		"does not choose between cards",             // more than one screen card refuses
		"reports the monitor on more than one card", // what it refuses
		"its pin does not include that card",        // the seat-pin coherence rule
		"audit-yaml --against-render",               // a stamped render goes STALE until re-rendered
	} {
		if !strings.Contains(text, want) {
			t.Errorf("composite-tier.md must say %q", want)
		}
	}
}
