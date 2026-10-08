package accelremote

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// D20: the accelerator lane's HTTP client rides netguard.SafeTransport like every other fleet
// client, so a node base outside loopback and the tailnet dies at the dial gate (ADR 0001, ADR 0042).
// 192.0.2.x is TEST-NET-1: a public literal that is guaranteed unroutable, so a regression would
// still fail, just slowly (and with a different error).
//
// The lane's shape check (ADR 0074) now refuses such an entry before any dial, so this proves the
// dial gate through the lane's own request helper, which is what every poll and dispatch rides.
func TestTheLaneClientRefusesAnOffTailnetNodeAtTheDialGate(t *testing.T) {
	_, err := getJSON[jobWire](context.Background(), config.Default(), "http://192.0.2.10:9/fleet/jobs/x")
	if err == nil {
		t.Fatal("the lane's client reached a public IP literal")
	}
	if !strings.Contains(err.Error(), "tailnet guard") {
		t.Fatalf("error %q does not come from the tailnet dial gate", err)
	}
}

// The same entry named in delegate_remotes never reaches the dial: the shape check names it, and
// the error says it was not dialled (ADR 0074), so a refused roster entry is not mistaken for a
// node that failed.
func TestCallNamesAnOffTailnetRosterEntryWithoutDialingIt(t *testing.T) {
	cfg := config.Default()
	cfg.DelegateRemotes = []string{"http://192.0.2.10:9"}
	_, err := Call(context.Background(), cfg, "coral-edgetpu", "classify", map[string]any{})
	if err == nil {
		t.Fatal("Call reached a public IP literal")
	}
	for _, want := range []string{"http://192.0.2.10:9", "not dialled", "tailnet guard"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "refusing dial") {
		t.Errorf("error %q is the dial gate's: the shape check must come first", err)
	}
}
