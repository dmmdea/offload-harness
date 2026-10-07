package research

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/netguard"
)

// The research lane reads the public web only, and refuses a tailnet host BY NAME before
// any lookup: an address check has nothing to look at for a name. A node shared in from
// another tailnet lives under that tailnet's zone (ADR 0074), so the refusal must judge
// every configured zone, not only the primary one: with a second zone listed, a page
// under it is the shared node's admin surface, not web.
func TestValidateURLRefusesAHostUnderAnyConfiguredTailnetZone(t *testing.T) {
	const (
		own    = "tailnnnnnn.ts.net"
		sharer = "tailmmmmmm.ts.net"
	)
	prev := netguard.TailnetSuffixes()
	t.Cleanup(func() { _ = netguard.SetTailnetSuffixes(prev) })
	if err := netguard.SetTailnetSuffixes([]string{own, sharer}); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"http://node-a." + own + "/health",
		"https://node-b." + sharer + "/fleet/health",
		"http://NODE-B." + strings.ToUpper(sharer) + "/",
		"http://" + sharer + "/", // the zone itself
	} {
		_, err := ValidateURL(context.Background(), raw)
		if err == nil || !strings.Contains(err.Error(), "on the tailnet") {
			t.Errorf("ValidateURL(%q) = %v, want the tailnet-by-name refusal", raw, err)
		}
	}
}
