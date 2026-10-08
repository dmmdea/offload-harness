package mediaremote

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// A roster entry the tailnet guard refuses is a named miss, never a dial and never a failed call (ADR 0074):
// the media lane used to dial delegate_remotes through the dial gate alone while the agent lane refused the
// same entry by shape. The entries after it still serve.
func TestPickNodeNamesARosterEntryTheTailnetGuardRefusesAndDoesNotDialIt(t *testing.T) {
	rostertest.Zones(t)
	rosterprobe.Default.Reset()
	t.Cleanup(rosterprobe.Default.Reset)
	refused := "http://node-x.tailkkkkkk.ts.net:18811"
	live := fakeNode(t, fakeOpts{tasks: []string{"video-gen"}})
	routes := []string{"generate_video"}

	base, _, err := pickNode(context.Background(), config.Config{}, []string{refused, live.URL}, "video-gen", "video-gen", routes)
	if err != nil || base != live.URL {
		t.Fatalf("pickNode = (%q, %v), want the live node past the refused entry", base, err)
	}

	_, _, err = pickNode(context.Background(), config.Config{}, []string{refused}, "video-gen", "video-gen", routes)
	var pe *placementError
	if !asPlacement(err, &pe) || pe.class != core.DeferClassCapacity {
		t.Fatalf("err = %v, want a capacity placementError", err)
	}
	for _, want := range []string{refused, "not dialled", "tailnet guard"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must contain %q", err, want)
		}
	}
	for _, dialled := range []string{"refusing dial", "no such host", "connection"} {
		if strings.Contains(err.Error(), dialled) {
			t.Errorf("error %q reads as though the refused entry was dialled (%q)", err, dialled)
		}
	}
}

// The same verdict reaches the caller through Run: a roster whose only entry is refused defers with the
// entry named (redacted: a token pasted into the entry never reaches the message), and the job is not sent.
func TestRunNamesARefusedRosterEntryInTheDeferAndRedactsItsToken(t *testing.T) {
	rostertest.Zones(t)
	rosterprobe.Default.Reset()
	t.Cleanup(rosterprobe.Default.Reset)
	refused := "http://node-x.tailkkkkkk.ts.net:18811/?token=hunter2"
	cfg := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{refused}}

	res := Run(context.Background(), cfg, &recordingRunner{}, video(nil), "remote", nil)
	if res.OK || res.DeferClass != core.DeferClassCapacity {
		t.Fatalf("want a capacity defer, got %+v", res)
	}
	if !strings.Contains(res.Reason, "not dialled") {
		t.Errorf("the defer must say the entry was not dialled: %s", res.Reason)
	}
	if strings.Contains(res.Reason, "hunter2") {
		t.Errorf("the defer leaks the token pasted into the roster entry: %s", res.Reason)
	}
}
