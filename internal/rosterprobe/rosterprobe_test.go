package rosterprobe

import (
	"net"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

const (
	ownZone    = "tailnnnnnn.ts.net"
	sharerZone = "tailmmmmmm.ts.net"
	otherZone  = "tailkkkkkk.ts.net"
)

// cgnatURL is a roster-style base on a raw tailnet CGNAT-range address, built in code so no
// address literal of that range sits in the source.
var cgnatURL = "http://" + net.IPv4(100, 64, 0, 7).String() + ":18811"

// Members is the one place a lane learns what the roster holds: entries normalized, blanks
// left out, configured order and slot numbers kept, and each entry judged by the tailnet shape
// check the agent lane applies. A refused entry is FLAGGED, not dropped, so a lane can name it.
func TestMembersNormalizeVetAndKeepConfiguredOrder(t *testing.T) {
	rostertest.Zones(t, ownZone)
	got := Members([]string{
		"  http://node-a:18811/ ",
		"",
		"http://node-x." + otherZone + ":18811", // a zone nobody listed
		cgnatURL,
		"http://example.com:18811", // a public name
		"   ",
		"http://node-b." + ownZone + ":18811/",
	})
	type want struct {
		index   int
		base    string
		refused bool
	}
	wants := []want{
		{0, "http://node-a:18811", false},
		{2, "http://node-x." + otherZone + ":18811", true},
		{3, cgnatURL, false},
		{4, "http://example.com:18811", true},
		{6, "http://node-b." + ownZone + ":18811", false},
	}
	if len(got) != len(wants) {
		t.Fatalf("Members returned %d entries, want %d: %+v", len(got), len(wants), got)
	}
	for i, w := range wants {
		g := got[i]
		if g.Index != w.index || g.Base != w.base || (g.Refused != nil) != w.refused {
			t.Errorf("member %d = {index %d, base %q, refused %v}, want {index %d, base %q, refused %v}",
				i, g.Index, g.Base, g.Refused, w.index, w.base, w.refused)
		}
	}
}

// A node shared in from another tailnet is admitted by its name once that tailnet's zone is
// listed (ADR 0074), by every lane through this one function.
func TestMembersAdmitANodeUnderAnyListedZone(t *testing.T) {
	shared := "http://node-b." + sharerZone + ":18811"
	rostertest.Zones(t, ownZone)
	if m := Members([]string{shared}); len(m) != 1 || m[0].Refused == nil {
		t.Fatalf("with only the own zone listed the sharer's name must be refused: %+v", m)
	}
	rostertest.Zones(t, ownZone, sharerZone)
	if m := Members([]string{shared}); len(m) != 1 || m[0].Refused != nil {
		t.Fatalf("with the sharer's zone listed its name must be admitted: %+v", m)
	}
}

// Miss names the entry first and says it was NOT dialled, so "no node is eligible ... probed
// <list>" never reads as though a refused entry had been tried and had failed.
func TestMissNamesWhatWasNotDone(t *testing.T) {
	rostertest.Zones(t)
	m := Members([]string{"http://node-x." + otherZone + ":18811"})
	if len(m) != 1 || m[0].Refused == nil {
		t.Fatalf("precondition: the entry must be refused: %+v", m)
	}
	miss := m[0].Miss()
	for _, want := range []string{m[0].Base + ":", "not dialled", "tailnet guard", "tailnet_suffix"} {
		if !strings.Contains(miss, want) {
			t.Errorf("miss %q must contain %q", miss, want)
		}
	}
}
