package netguard

import (
	"fmt"
	"strings"
	"testing"
)

// HostAllowed and TailnetURL are two guards over one rule: which NAMES belong to this
// tailnet. They once disagreed on shapes the first table never fed in (nested zones, a second
// trailing dot, a port that is not a number), so this file feeds in every combination of a
// small set of parts and demands the same verdict from both. The one place HostAllowed is
// stricter on purpose is a name made only of dots (".", ".."): TailnetURL reads "." as an
// empty, dotless name and admits it, and TestHostAllowedEdgeCases pins the refusal.
//
// A port above 65535 is admitted by both: url.Parse checks that a port is digits, not that it
// fits, and a browser cannot send one.

func zoneSetsForParity() [][]string {
	sub := "sub." + ownZone
	return [][]string{nil, {ownZone}, {ownZone, sharerZone}, {ownZone, sub}, {sub, ownZone}, {sub}}
}

func TestHostAllowedAgreesWithTailnetURLOnGeneratedShapes(t *testing.T) {
	bases := []string{
		"127.0.0.1", "[::1]", testCGNATHost, "100.128.0.1", "192.0.2.20",
		"localhost", "node-a", "NODE-A",
		"node-a." + ownZone, "NODE-B." + strings.ToUpper(sharerZone), "deep.name.node-b." + sharerZone,
		"node-x." + strangerZone, "evil.ts.net", "evil" + sharerZone, "attacker.example",
		ownZone, sharerZone, "sub." + ownZone, "x.sub." + ownZone, // the bare zones, and names under the nested one
	}
	dots := []string{"", ".", ".."}
	ports := []string{"", ":", ":18811", ":99999", ":evil.example", ":+80", ":08"}
	var disagree []string
	checked := 0
	for _, zones := range zoneSetsForParity() {
		installZones(t, zones...)
		for _, base := range bases {
			for _, dot := range dots {
				for _, port := range ports {
					host := base + dot + port
					checked++
					want := TailnetURLIn(TailnetSuffixes(), "http://"+host) == nil
					if got, why := HostAllowed(host); got != want {
						disagree = append(disagree, fmt.Sprintf("zones %v: HostAllowed(%q) = %v (%s), but TailnetURL says %v", zones, host, got, why, want))
					}
				}
			}
		}
	}
	if len(disagree) > 0 {
		shown := disagree
		if len(shown) > 12 {
			shown = shown[:12]
		}
		t.Errorf("HostAllowed and TailnetURL disagree on %d of %d shapes; the first %d:\n%s", len(disagree), checked, len(shown), strings.Join(shown, "\n"))
	}
}

// Zones that nest: a name under the OUTER zone is a host under it, even when it is also the
// bare INNER zone. TailnetURL admits it by the suffix test; the guard used to refuse it as a
// bare zone, so under enforce the node got 421 and under warn the log filled with a false
// violation. The inner zone alone, with the outer one not configured, is still a bare zone.
func TestHostAllowedNestedZonesAdmitAHostUnderTheOuterZone(t *testing.T) {
	inner := "sub." + ownZone
	installZones(t, ownZone, inner)
	for _, host := range []string{inner, inner + ":18811", "node-a." + inner, "NODE-A." + strings.ToUpper(inner) + "."} {
		if ok, why := HostAllowed(host); !ok {
			t.Errorf("zones {outer, inner}: HostAllowed(%q) = false (%s), want true: the name is under the outer zone", host, why)
		}
	}
	if ok, _ := HostAllowed(ownZone); ok {
		t.Errorf("zones {outer, inner}: the bare outer zone names no node and must stay refused")
	}

	installZones(t, inner)
	if ok, _ := HostAllowed(inner); ok {
		t.Errorf("zones {inner}: the bare inner zone names no node and must be refused")
	}
	if ok, why := HostAllowed("node-a." + inner); !ok {
		t.Errorf("zones {inner}: a host under the inner zone was refused (%s)", why)
	}
}

// One trailing root dot is ignored; a second is not part of any name. The guard trimmed one dot
// itself and InTailnetZone trimmed another, so "zone.." passed as a host under a zone while
// TailnetURL refused it, and the bare-zone refusal never saw it.
func TestHostAllowedIgnoresOneTrailingDotNotTwo(t *testing.T) {
	installZones(t, ownZone, sharerZone)
	for _, host := range []string{
		"node-a." + ownZone + ".", "node-a." + ownZone + ".:18811", // one root dot is the same name
	} {
		if ok, why := HostAllowed(host); !ok {
			t.Errorf("HostAllowed(%q) = false (%s), want true", host, why)
		}
	}
	for _, host := range []string{
		ownZone + "..", sharerZone + "..:18811", // the bare zone, hidden behind a second dot
		"node-a." + ownZone + "..", "node-b." + sharerZone + "..:18811",
		"node-a..", "localhost..",
	} {
		if ok, _ := HostAllowed(host); ok {
			t.Errorf("HostAllowed(%q) = true, want a refusal: two trailing dots are no name", host)
		}
	}
}

// A port is digits, as url.Parse reads it. net.SplitHostPort splits on the last colon and checks
// nothing after it, so a Host such as "localhost:evil.example" passed as loopback.
func TestHostAllowedRefusesAPortThatIsNotANumber(t *testing.T) {
	installZones(t, ownZone)
	for _, host := range []string{
		"localhost:evil.example", "127.0.0.1:abc", "[::1]:abc", "node-a:+80", "node-a:8 0",
		"node-a." + ownZone + ":evil", testCGNATHost + ":0x50", "node-a:1:2", "localhost:evil:x",
	} {
		if ok, _ := HostAllowed(host); ok {
			t.Errorf("HostAllowed(%q) = true, want a refusal: the port is not a number", host)
		}
	}
	for _, host := range []string{"localhost:18811", "127.0.0.1:0", "[::1]:08", "node-a." + ownZone + ":", "node-a." + ownZone + ":99999"} {
		if ok, why := HostAllowed(host); !ok {
			t.Errorf("HostAllowed(%q) = false (%s), want true: a numeric or empty port is the endpoint rule's", host, why)
		}
	}
}
