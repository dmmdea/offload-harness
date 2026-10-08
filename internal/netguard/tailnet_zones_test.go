package netguard

import (
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// cgnatURL is a roster-style base on a raw tailnet CGNAT-range address, built in code so no
// address literal of that range sits in the source.
var cgnatURL = "http://" + net.IPv4(100, 64, 0, 7).String() + ":18811"

// Fixture zones. ownZone is the operator's own tailnet; sharerZone is the zone of ANOTHER
// tailnet that shared a node in (Tailscale names a shared node under the sharer's zone,
// and only by that name); strangerZone is a third tailnet nobody configured.
const (
	ownZone      = "tailnnnnnn.ts.net"
	sharerZone   = "tailmmmmmm.ts.net"
	strangerZone = "tailkkkkkk.ts.net"
)

// installZones installs a zone list for one test and restores the whole previous list
// (not just its first zone) afterwards, so a test that runs after this one can never be
// made to pass or fail by what this one left behind.
func installZones(t *testing.T, zones ...string) {
	t.Helper()
	prev := TailnetSuffixes()
	if err := SetTailnetSuffixes(zones); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetTailnetSuffixes(prev) })
}

// A node shared in from another tailnet is reachable only by its FQDN under the SHARER's
// zone. With one zone configured that name was refused, so the node could be named only
// by its raw 100.x address; with the sharer's zone listed beside the operator's own, both
// forms pass, and nothing else does.
func TestTailnetURLAdmitsEveryConfiguredZone(t *testing.T) {
	installZones(t, ownZone, sharerZone)

	for _, raw := range []string{
		"http://node-a." + ownZone + ":18811",
		"http://node-b." + sharerZone + ":18811", // the shared-in node, by its name
		"https://deep.name.node-b." + sharerZone + ":18811",
		"http://NODE-B." + strings.ToUpper(sharerZone) + ":18811",
		"http://node-b." + sharerZone + ".:18811", // trailing root dot
		cgnatURL,              // the raw form keeps working
		"http://node-c:18811", // and so does a dotless MagicDNS name
	} {
		if err := TailnetURL(raw); err != nil {
			t.Errorf("TailnetURL(%q) = %v, want nil", raw, err)
		}
	}
	for _, raw := range []string{
		"http://node-x." + strangerZone + ":18811", // a tailnet nobody configured
		"http://evil.ts.net:18811",                 // generic .ts.net is still refused
		"http://eviltailmmmmmm.ts.net:18811",       // suffix match without the label boundary dot
		"http://" + sharerZone + ":18811",          // the bare zone is not a host UNDER it
		"http://node-b." + sharerZone + ".example.com:18811",
		"http://example.com",
	} {
		if err := TailnetURL(raw); err == nil {
			t.Errorf("TailnetURL(%q) = nil, want a refusal", raw)
		}
	}
}

// The refusal names the zones it judged against, so the operator who gets it can see
// whether the list reached the process at all.
func TestTailnetURLRefusalNamesTheConfiguredZones(t *testing.T) {
	const stranger = "http://node-x." + strangerZone + ":18811"

	installZones(t)
	err := TailnetURL(stranger)
	if err == nil || !strings.Contains(err.Error(), "tailnet_suffix") || !strings.Contains(err.Error(), "tailnet_suffixes") {
		t.Errorf("no zone configured: error %q must point at both config keys", err)
	}

	installZones(t, ownZone)
	err = TailnetURL(stranger)
	if err == nil || !strings.Contains(err.Error(), "under "+ownZone) || strings.Contains(err.Error(), "one of") {
		t.Errorf("one zone configured: error %q must name that zone alone", err)
	}

	installZones(t, ownZone, sharerZone)
	err = TailnetURL(stranger)
	if err == nil || !strings.Contains(err.Error(), "one of "+ownZone+", "+sharerZone) {
		t.Errorf("two zones configured: error %q must list both", err)
	}
}

// TailnetURLIn judges the zones it is given and nothing else, so a caller that wants the
// verdict a config will reach (the doctor's roster finding) does not depend on which
// config the process happened to load last.
func TestTailnetURLInJudgesTheGivenZonesNotTheInstalledOnes(t *testing.T) {
	installZones(t) // nothing installed
	raw := "http://node-b." + sharerZone + ":18811"

	if err := TailnetURL(raw); err == nil {
		t.Fatal("precondition: with no zone installed the name must be refused")
	}
	if err := TailnetURLIn([]string{sharerZone}, raw); err != nil {
		t.Errorf("TailnetURLIn with the zone given refused it: %v", err)
	}
	if err := TailnetURLIn([]string{ownZone}, raw); err == nil {
		t.Error("TailnetURLIn passed a host under a zone that was not given")
	}

	installZones(t, sharerZone)
	if err := TailnetURLIn(nil, raw); err == nil {
		t.Error("TailnetURLIn(nil) fell back to the installed zones; the given list must be the only input")
	}
}

// SetTailnetSuffixes normalizes each entry the way the single key always did, skips unset
// slots and collapses duplicates, and keeps the order written: the first zone is the one
// TailnetSuffix reports.
func TestSetTailnetSuffixesNormalizesDedupesAndKeepsOrder(t *testing.T) {
	installZones(t)
	in := []string{"  " + strings.ToUpper(ownZone) + ". ", "", "." + ownZone, sharerZone + ".", "   ", strings.ToUpper(sharerZone)}
	if err := SetTailnetSuffixes(in); err != nil {
		t.Fatal(err)
	}
	want := []string{ownZone, sharerZone}
	if got := TailnetSuffixes(); !reflect.DeepEqual(got, want) {
		t.Errorf("TailnetSuffixes() = %q, want %q", got, want)
	}
	if got := TailnetSuffix(); got != ownZone {
		t.Errorf("TailnetSuffix() = %q, want the first zone %q", got, ownZone)
	}
}

// One malformed entry refuses the whole list and leaves the previous one in force: a
// half-installed list would admit some of what the operator wrote and silently drop the
// rest, which reads as a working config.
func TestSetTailnetSuffixesIsAllOrNothing(t *testing.T) {
	installZones(t, ownZone)
	err := SetTailnetSuffixes([]string{sharerZone, "not a zone", strangerZone})
	if err == nil {
		t.Fatal("a malformed entry was accepted")
	}
	if !strings.Contains(err.Error(), "tailnet_suffixes[1]") {
		t.Errorf("error %q does not name the entry that failed", err)
	}
	if got, want := TailnetSuffixes(), []string{ownZone}; !reflect.DeepEqual(got, want) {
		t.Errorf("zones after a refused install = %q, want the previous %q", got, want)
	}
	for _, bad := range []string{"notazone", "has space.ts.net", "http://" + ownZone, "user@" + ownZone} {
		if err := SetTailnetSuffixes([]string{bad}); err == nil {
			t.Errorf("SetTailnetSuffixes(%q) = nil, want a refusal", bad)
		}
	}
}

// ParseZone accepts only dot-separated [a-z0-9-] labels and never the bare tailnet domain: a
// wildcard, a trailing query or backslash, an empty label or the generic "ts.net" would each
// sit in the list looking like a configured zone while admitting nothing (a typo) or, for the
// generic domain, every tailnet's hostnames including Funnel-published public ones.
func TestParseZoneRefusesWhatIsNotAZone(t *testing.T) {
	for _, bad := range []string{
		"ts.net", ".ts.net", "ts.net.", "  TS.NET  ", "..ts.net..", // the generic domain, however it is spelled
		"*.x.ts.net", "x.*.ts.net", "*.ts.net", // wildcards
		"x.ts.net?", "x.ts.net?a=b", "x.ts.net#f", "x.ts.net\\", "x.ts.net/", "x.ts.net:443", "u@x.ts.net", // stray characters
		"x..ts.net", "x.ts.net..y", // an empty label
		"x_y.ts.net", "x y.ts.net", "x%2e.ts.net", "ünï.ts.net", "x.t\ts.net", "x.t\x00s.net", // outside [a-z0-9-]
		"notazone", "net", "http://" + ownZone, "user@" + ownZone,
	} {
		if z, err := ParseZone("tailnet_suffixes[3]", bad); err == nil {
			t.Errorf("ParseZone(%q) = %q, nil; want a refusal", bad, z)
		} else if !strings.Contains(err.Error(), "tailnet_suffixes[3]") {
			t.Errorf("ParseZone(%q) error %q does not name the key", bad, err)
		}
	}
	// SetTailnetSuffixes refuses the same values, so a bad one never reaches the gate.
	installZones(t, ownZone)
	for _, bad := range []string{"ts.net", ".ts.net", "*.x.ts.net", "x.ts.net?", "x.ts.net\\"} {
		if err := SetTailnetSuffixes([]string{sharerZone, bad}); err == nil {
			t.Errorf("SetTailnetSuffixes accepted %q", bad)
		}
		if got := TailnetSuffixes(); !reflect.DeepEqual(got, []string{ownZone}) {
			t.Fatalf("after refusing %q the zones are %q, want the previous list", bad, got)
		}
	}
}

// What ParseZone accepts is normalized and unchanged in meaning: case folded, one leading
// and trailing dot dropped, digits and hyphens allowed, blanks left as an unset slot.
func TestParseZoneAcceptsAndNormalizesAZone(t *testing.T) {
	for in, want := range map[string]string{
		ownZone:                                ownZone,
		"  " + strings.ToUpper(ownZone) + ". ": ownZone,
		"." + sharerZone:                       sharerZone,
		"net-1a2b3c.ts.net":                    "net-1a2b3c.ts.net",
		"lab.example":                          "lab.example",
		"xn--bcher-kva.example":                "xn--bcher-kva.example", // punycode is ASCII
		"":                                     "",
		"   ":                                  "",
		"...":                                  "",
	} {
		got, err := ParseZone("tailnet_suffix", in)
		if err != nil || got != want {
			t.Errorf("ParseZone(%q) = %q, %v; want %q, nil", in, got, err, want)
		}
	}
}

// The one-zone key keeps its contract exactly: it REPLACES the installed list with one
// zone, an empty value clears every zone, and a bad value changes nothing.
func TestSetTailnetSuffixKeepsTheSingleZoneContract(t *testing.T) {
	installZones(t, ownZone, sharerZone)
	if err := SetTailnetSuffix(strangerZone); err != nil {
		t.Fatal(err)
	}
	if got, want := TailnetSuffixes(), []string{strangerZone}; !reflect.DeepEqual(got, want) {
		t.Errorf("after SetTailnetSuffix zones = %q, want only %q", got, want)
	}
	if err := SetTailnetSuffix("not a zone"); err == nil || !strings.Contains(err.Error(), `tailnet_suffix "not a zone"`) {
		t.Errorf("a bad single zone must be refused naming the key, got %v", err)
	}
	if got := TailnetSuffix(); got != strangerZone {
		t.Errorf("a refused value changed the installed zone to %q", got)
	}
	if err := SetTailnetSuffix(""); err != nil {
		t.Fatal(err)
	}
	if got := TailnetSuffixes(); got != nil || TailnetSuffix() != "" {
		t.Errorf("an empty value must clear every zone, got %q", got)
	}
}

// TailnetSuffixes hands out the caller's own copy: a caller that sorts or edits it must
// not be able to change what the gate admits.
func TestTailnetSuffixesReturnsACopy(t *testing.T) {
	installZones(t, ownZone, sharerZone)
	got := TailnetSuffixes()
	got[0] = strangerZone
	if after := TailnetSuffixes(); after[0] != ownZone {
		t.Errorf("mutating the returned slice changed the installed zones: %q", after)
	}
	in := []string{ownZone, sharerZone}
	if err := SetTailnetSuffixes(in); err != nil {
		t.Fatal(err)
	}
	in[0] = strangerZone
	if after := TailnetSuffixes(); after[0] != ownZone {
		t.Errorf("mutating the input slice changed the installed zones: %q", after)
	}
}

// InTailnetZone is the NAME test the research lane uses to keep tailnet hosts out of the
// web: it must see every configured zone (the apex too), and only those.
func TestInTailnetZoneSeesEveryZone(t *testing.T) {
	installZones(t, ownZone, sharerZone)
	for host, want := range map[string]bool{
		ownZone:                true,
		"node-a." + ownZone:    true,
		"node-b." + sharerZone: true,
		"NODE-B." + strings.ToUpper(sharerZone) + ".": true,
		sharerZone:               true,
		"node-x." + strangerZone: false,
		"eviltailmmmmmm.ts.net":  false,
		"example.com":            false,
		"":                       false,
	} {
		if got := InTailnetZone(host); got != want {
			t.Errorf("InTailnetZone(%q) = %v, want %v", host, got, want)
		}
	}
	installZones(t)
	if InTailnetZone("node-a." + ownZone) {
		t.Error("with no zone configured nothing is on the tailnet by name")
	}
}

// Every config load installs the list and two loads can run at once; readers must always
// see a whole list, never a half-written one.
func TestTheTailnetZoneListIsSafeForConcurrentLoads(t *testing.T) {
	installZones(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := SetTailnetSuffixes([]string{ownZone, sharerZone}); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			_ = TailnetURL("http://node-b." + sharerZone + ":18811")
			if zs := TailnetSuffixes(); len(zs) != 0 && len(zs) != 2 {
				t.Errorf("a reader saw a partial list: %q", zs)
			}
			_ = InTailnetZone("node-b." + sharerZone)
		}()
	}
	wg.Wait()
	if got, want := TailnetSuffixes(), []string{ownZone, sharerZone}; !reflect.DeepEqual(got, want) {
		t.Fatalf("zones after concurrent loads = %q, want %q", got, want)
	}
}
