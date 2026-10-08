package config

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/netguard"
)

// The agent lane refuses a roster entry the tailnet guard does not admit, at intake, and the
// single-shot lanes skip it; ADR 0023 says the guard runs at config load, but for delegate_remotes it
// did not, so a bad entry loaded clean, passed doctor, and failed on the first call that reached it.
// The load now WARNS (doctor prints the same finding as a FAIL row): by index, naming the guard.
func TestEndpointWarningsFlagARosterEntryTheTailnetGuardRefuses(t *testing.T) {
	c := Config{
		TailnetSuffix: "tailnnnnnn.ts.net",
		DelegateRemotes: []string{
			"http://node-a:18811",                   // dotless MagicDNS: admitted
			"http://node-x.tailkkkkkk.ts.net:18811", // a zone nobody listed
			"http://node-b.tailnnnnnn.ts.net:18811", // under the own zone: admitted
			"http://example.com:18811",              // a public name
			"",                                      // an unset slot is skipped, and still counts as a slot
			"http://192.168.1.5:18811",              // a LAN literal
		},
	}
	got := EndpointWarnings(c)
	joined := strings.Join(got, "\n")
	for _, want := range []string{"delegate_remotes[1]", "delegate_remotes[3]", "delegate_remotes[5]"} {
		if !hasRefusalRowFor(got, want) {
			t.Errorf("no tailnet-guard finding names %s:\n%s", want, joined)
		}
	}
	for _, clean := range []string{"delegate_remotes[0]", "delegate_remotes[2]", "delegate_remotes[4]"} {
		if hasRefusalRowFor(got, clean) {
			t.Errorf("%s is admitted (or blank) and must not be flagged:\n%s", clean, joined)
		}
	}
	for _, want := range []string{"tailnet_suffixes", "dotless MagicDNS", "100.x"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the finding must say how to fix it (%q):\n%s", want, joined)
		}
	}
}

func hasRefusalRowFor(rows []string, label string) bool {
	for _, r := range rows {
		if strings.HasPrefix(r, label+" is refused by the tailnet guard") {
			return true
		}
	}
	return false
}

// A node shared in from another tailnet is admitted once the sharer's zone is listed in
// tailnet_suffixes, and the finding goes with it.
func TestEndpointWarningsAdmitTheSharersZone(t *testing.T) {
	c := Config{
		TailnetSuffix:   "tailnnnnnn.ts.net",
		TailnetSuffixes: []string{"tailmmmmmm.ts.net"},
		DelegateRemotes: []string{"http://node-b.tailmmmmmm.ts.net:18811"},
	}
	if got := EndpointWarnings(c); len(got) != 0 {
		t.Fatalf("an entry under a listed zone must be silent, got %q", got)
	}
	c.TailnetSuffixes = nil
	if got := EndpointWarnings(c); !hasRefusalRowFor(got, "delegate_remotes[0]") {
		t.Fatalf("without the sharer's zone listed the entry must be flagged, got %q", got)
	}
}

// The finding is a function of the CONFIG, not of whichever zones the process happened to install
// last: doctor reports on the config it loaded, and `install client` judges a config it just
// rendered. Installing a zone in netguard must not change what a config's own zones say.
func TestEndpointWarningsJudgeTheConfigsOwnZonesNotTheInstalledOnes(t *testing.T) {
	restoreZones(t)
	entry := "http://node-b.tailmmmmmm.ts.net:18811"

	if err := netguard.SetTailnetSuffixes([]string{"tailmmmmmm.ts.net"}); err != nil {
		t.Fatal(err)
	}
	if got := EndpointWarnings(Config{DelegateRemotes: []string{entry}}); !hasRefusalRowFor(got, "delegate_remotes[0]") {
		t.Errorf("a config that names no zone must be flagged even when the process has one installed, got %q", got)
	}

	if err := netguard.SetTailnetSuffixes(nil); err != nil {
		t.Fatal(err)
	}
	if got := EndpointWarnings(Config{TailnetSuffixes: []string{"tailmmmmmm.ts.net"}, DelegateRemotes: []string{entry}}); len(got) != 0 {
		t.Errorf("a config that names the zone must be silent even when the process has none installed, got %q", got)
	}
}

// Doctor output gets pasted into chats. A roster entry is a base URL, but nothing stops one being
// pasted with credentials in it; no finding about an entry may print them.
func TestEndpointWarningsNeverPrintCredentialsFromARosterEntry(t *testing.T) {
	secretVals := []string{"pa55word", "tok3n", "frag-secret"}
	for _, raw := range []string{
		"http://user:pa55word@node-x.tailkkkkkk.ts.net:18798/v1?token=tok3n#frag-secret",
		"http://user:pa55word@127.0.0.1:18798/v1?token=tok3n#frag-secret",
		"http://user:pa55word@node-x.tailkkkkkk.ts.net:notaport/?token=tok3n#frag-secret", // does not parse
	} {
		got := strings.Join(EndpointWarnings(Config{DelegateRemotes: []string{raw}}), "\n")
		if got == "" {
			t.Errorf("%q produced no finding at all", raw)
		}
		for _, s := range secretVals {
			if strings.Contains(got, s) {
				t.Errorf("a finding about %q prints %q:\n%s", raw, s, got)
			}
		}
		if !strings.Contains(got, "delegate_remotes[0]") {
			t.Errorf("the finding must still name the entry by index:\n%s", got)
		}
	}
}

// RedactBase leaves an ordinary base byte-identical (so no existing message changes), strips what a
// URL can carry that is a secret, and masks the userinfo of a value that does not parse.
func TestRedactBase(t *testing.T) {
	for in, want := range map[string]string{
		"http://node-a:18811":                  "http://node-a:18811",
		"  http://node-a:18811/v1  ":           "http://node-a:18811/v1",
		"http://user:pw@node-a:18811":          "http://node-a:18811",
		"http://node-a:18811/path?token=abc":   "http://node-a:18811/path",
		"http://node-a:18811/path?":            "http://node-a:18811/path",
		"http://node-a:18811/path#frag":        "http://node-a:18811/path",
		"http://user:pw@node-a:18811/v1?x=1#f": "http://node-a:18811/v1",
		"${NODE_A_HOST}:18811":                 "${NODE_A_HOST}:18811",
		"http://user:pw@node-a:bad":            "http://<redacted>@node-a:bad",
		"":                                     "",
	} {
		if got := RedactBase(in); got != want {
			t.Errorf("RedactBase(%q) = %q, want %q", in, got, want)
		}
	}
}

// The load stays a warning: the entry loads, and the finding is reachable from the loaded value,
// by index, which is what doctor reads.
func TestLoadCarriesTheRosterGuardFindingAsAWarning(t *testing.T) {
	restoreZones(t)
	cfg, err := loadJSON(t, `{"tailnet_suffix":"tailnnnnnn.ts.net","delegate_remotes":["http://node-a:18811","http://node-x.tailkkkkkk.ts.net:18811"]}`)
	if err != nil {
		t.Fatalf("a refused roster entry must still LOAD (warn first): %v", err)
	}
	f := cfg.Findings()
	if !hasRefusalRowFor(f, "delegate_remotes[1]") || hasRefusalRowFor(f, "delegate_remotes[0]") {
		t.Fatalf("Findings() = %q, want exactly delegate_remotes[1] flagged", f)
	}
}

// TailnetZones is all-or-nothing, as netguard.SetTailnetSuffixes is, so a config built by hand with
// one malformed zone is judged against NO zone: the safe choice, since a half-read list would admit
// some of what the operator wrote and drop the rest silently. The findings say so: the zone error
// comes first, and the entry under the GOOD zone is flagged beside it, so its refusal has a cause.
func TestEndpointWarningsJudgeAMalformedZoneListAsNoZonesAndSayWhy(t *testing.T) {
	good := "http://node-a.tailnnnnnn.ts.net:18811"
	c := Config{
		TailnetSuffix:   "tailnnnnnn.ts.net",
		TailnetSuffixes: []string{"tailmmmmmm.ts.net", "*.tailkkkkkk.ts.net"},
		DelegateRemotes: []string{good},
	}
	got := EndpointWarnings(c)
	if len(got) == 0 || !strings.Contains(got[0], `tailnet_suffixes[1] "*.tailkkkkkk.ts.net" is not a DNS zone`) {
		t.Fatalf("the first finding must be the zone error, got %q", got)
	}
	if !hasRefusalRowFor(got, "delegate_remotes[0]") {
		t.Errorf("with the zone list unreadable, even a host under the good zone is judged against none and must be flagged, got %q", got)
	}
	// The same config with the bad zone removed is clean: the refusal above is the malformed zone's doing.
	c.TailnetSuffixes = []string{"tailmmmmmm.ts.net"}
	if got := EndpointWarnings(c); len(got) != 0 {
		t.Errorf("a well-formed zone list must be silent for an entry under its own zone, got %q", got)
	}
	// And a malformed list is no reason to print a finding for a config that names no roster at all.
	if got := EndpointWarnings(Config{TailnetSuffix: "ts.net"}); len(got) != 1 || !strings.Contains(got[0], "generic tailnet domain") {
		t.Errorf("a bare ts.net zone must surface as exactly one finding, got %q", got)
	}
}

// A configured base may have been pasted with a user:password or a token in it. Every message about
// one, from the tailnet guard that refuses it to the dead-port and loopback checks and the doctor
// findings for a lane, names it by RedactBase and never quotes the credential: these messages reach
// chats. The shapes are the ones a real paste produces.
func TestEveryMessageAboutAConfiguredBaseOmitsItsCredentials(t *testing.T) {
	restoreZones(t)
	marks := []string{"s3cret", "tok%33", "p%77", "frag-"}
	bases := []string{
		"http://user:pw-s3cret@example.com:18811/?token=tok-s3cret",
		"http://example.com:18811/?token=s%33cret#frag-s3cret",
		"https://us%65r:p%77-s3cret@example.com/",
		"user:pw-s3cret@node-a:18811",
		"tok-s3cret@node-a:18811",
		"http://user:pw-s3cret@node-a:notaport/?token=s3cret",
		"http://user:pw-s3cret@127.0.0.1:9/?token=s3cret", // admitted by the guard, refused by the dead-port rule
	}
	check := func(what, raw string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: %q was not refused", what, raw)
			return
		}
		for _, m := range marks {
			if strings.Contains(err.Error(), m) {
				t.Errorf("%s: the message about %q carries %q:\n%s", what, raw, m, err)
			}
		}
	}
	for _, raw := range bases {
		check("a tailnet-guarded map (seat_endpoints / cascade_remote_lanes)", raw, validateEndpointValue(`seat_endpoints["gemma"]`, raw, true))
		// These two admit some of the values (a public name is a usable URL; port 9 on loopback is
		// a loopback base), so only the message of a REFUSAL is judged.
		if err := validateEndpointValue("delegate_remotes[2]", raw, false); err != nil {
			check("an unguarded list (delegate_remotes)", raw, err)
		}
		if err := validateLoopbackBase("pair_node_info_url", raw); err != nil {
			check("pair_node_info_url", raw, err)
		}
	}
	// Through the loader, the way a config file reaches it.
	_, err := loadJSON(t, `{"seat_endpoints":{"gemma":"http://user:pw-s3cret@example.com:18811/?token=tok-s3cret"}}`)
	check("Load of seat_endpoints", "seat_endpoints", err)
	_, err = loadJSON(t, `{"delegate_remotes":["http://user:pw-s3cret@127.0.0.1:9/?token=tok-s3cret"]}`)
	check("Load of delegate_remotes", "delegate_remotes", err)

	// The doctor findings for a cascade lane base print the same redacted value.
	var warnings []string
	for i, raw := range append(bases, "http://user:pw-s3cret@node-a:18811/v1?token=s3cret") {
		warnings = append(warnings, EndpointWarnings(Config{CascadeRemoteLanes: map[string]string{"lane" + string(rune('a'+i)): raw}, DelegateRemotes: []string{raw}})...)
	}
	if len(warnings) == 0 {
		t.Fatal("precondition: these bases must produce findings")
	}
	for _, w := range warnings {
		for _, m := range marks {
			if strings.Contains(w, m) {
				t.Errorf("a finding carries %q: %s", m, w)
			}
		}
	}
}
