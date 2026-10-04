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
func TestCallRefusesAnOffTailnetNodeAtTheDialGate(t *testing.T) {
	cfg := config.Default()
	cfg.DelegateRemotes = []string{"http://192.0.2.10:9"}
	_, err := Call(context.Background(), cfg, "coral-edgetpu", "classify", map[string]any{})
	if err == nil {
		t.Fatal("Call reached a public IP literal")
	}
	if !strings.Contains(err.Error(), "tailnet guard") {
		t.Fatalf("error %q does not come from the tailnet dial gate", err)
	}
}
