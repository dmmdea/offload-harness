package netguard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The ingress guard's Host rule must judge EVERY configured tailnet zone, as TailnetURL does.
// A node shared in from another tailnet is reachable only by its name under the SHARER's zone
// (config tailnet_suffixes); the guard first read only the primary zone (TailnetSuffix), so that
// name was refused under enforce and logged as a violation under warn.

// A name under the second configured zone passes, and so does one under the first. A name
// under neither, a lookalike with no label boundary, a deeper lookalike and the bare zone do
// not.
func TestHostAllowedAdmitsEveryConfiguredTailnetZone(t *testing.T) {
	installZones(t, ownZone, sharerZone)

	for _, host := range []string{
		"node-a." + ownZone + ":18811",
		"node-b." + sharerZone + ":18811", // the shared-in node, by its name
		"node-b." + sharerZone,
		"deep.name.node-b." + sharerZone + ":18811",
		"NODE-B." + strings.ToUpper(sharerZone),
		"node-b." + sharerZone + ".", // trailing root dot
		"node-b." + sharerZone + ".:18811",
	} {
		if ok, why := HostAllowed(host); !ok {
			t.Errorf("HostAllowed(%q) = false (%s), want true", host, why)
		}
	}
	for _, host := range []string{
		"node-x." + strangerZone + ":18811",       // a tailnet nobody configured
		"evil.ts.net",                             // the generic domain is still no zone
		"evil" + sharerZone,                       // suffix match without the label boundary dot
		"x.evil" + sharerZone + ":18811",          // the same, one label deeper
		"node-b." + sharerZone + ".attacker.test", // the zone in the middle of a name
		sharerZone, // the bare zone is not a host UNDER it
		ownZone + ":18811",
		"attacker.example",
	} {
		if ok, _ := HostAllowed(host); ok {
			t.Errorf("HostAllowed(%q) = true, want a refusal", host)
		}
	}
}

// HostAllowed and TailnetURL are two guards over one rule. This pins that they agree on every
// shape of host, for no zone, one zone and two zones, so a change to either one cannot drift
// the other again.
func TestHostAllowedAgreesWithTailnetURLForEveryShape(t *testing.T) {
	hosts := []string{
		"127.0.0.1", "127.0.0.1:18800", "[::1]:18800", "localhost", "localhost:18800",
		testCGNATHost + ":18811", net.IPv4(100, 127, 255, 254).String(), "100.128.0.1", // tailnet addresses are built, not spelled (testCGNATHost)
		"node-a", "NODE-A:18811", "node-a.",
		"node-a." + ownZone, "node-a." + ownZone + ":18811", "NODE-A." + strings.ToUpper(ownZone),
		"node-b." + sharerZone, "node-b." + sharerZone + ":18811", "node-b." + sharerZone + ".",
		"node-b." + sharerZone + ".:18811", "deep.name.node-b." + sharerZone,
		"node-x." + strangerZone, "evil.ts.net", "evil" + sharerZone, "x.evil" + ownZone,
		ownZone, sharerZone, // the bare zone
		"attacker.example", "attacker.example:18811", "192.0.2.20:18811", "8.8.8.8",
	}
	for _, zones := range [][]string{nil, {ownZone}, {ownZone, sharerZone}} {
		installZones(t, zones...)
		for _, host := range hosts {
			want := TailnetURLIn(TailnetSuffixes(), "http://"+host) == nil
			if got, why := HostAllowed(host); got != want {
				t.Errorf("zones %v: HostAllowed(%q) = %v (%s), but TailnetURL says %v", zones, host, got, why, want)
			}
		}
	}
}

// The refusal names the zones it judged against and the config keys that set them, so the
// operator who reads the warn log can see whether the list reached the process at all.
func TestHostAllowedRefusalNamesEveryConfiguredZone(t *testing.T) {
	const stranger = "node-x." + strangerZone

	installZones(t)
	_, why := HostAllowed(stranger)
	if !strings.Contains(why, "tailnet_suffix") || !strings.Contains(why, "tailnet_suffixes") {
		t.Errorf("no zone configured: %q must point at both config keys", why)
	}

	installZones(t, ownZone)
	_, why = HostAllowed(stranger)
	if !strings.Contains(why, "under "+ownZone) || strings.Contains(why, "one of") {
		t.Errorf("one zone configured: %q must name that zone alone", why)
	}

	installZones(t, ownZone, sharerZone)
	_, why = HostAllowed(stranger)
	if !strings.Contains(why, "one of "+ownZone+", "+sharerZone) {
		t.Errorf("two zones configured: %q must list both", why)
	}
}

// The whole guard, not only the Host rule: with two zones configured, enforce lets a request
// for the shared-in node through (200), still refuses a rebinding name (421), and warn logs
// nothing for the shared-in node, because a warn window with a false denial in it is a window
// that can never show the zero unexplained denials enforce is promoted on.
func TestIngressGuardEnforceAdmitsASecondaryZoneHost(t *testing.T) {
	installZones(t, ownZone, sharerZone)

	enforce := IngressGuard(IngressOptions{Mode: "enforce", Key: "fleet_ingress_guard"}, okHandler())
	for host, want := range map[string]int{
		"node-a." + ownZone + ":18811":      http.StatusOK,
		"node-b." + sharerZone + ":18811":   http.StatusOK,
		"attacker.example":                  http.StatusMisdirectedRequest,
		"node-x." + strangerZone + ":18811": http.StatusMisdirectedRequest,
		"evil" + sharerZone:                 http.StatusMisdirectedRequest,
	} {
		if w := guardedRequest(t, enforce, http.MethodGet, host, nil); w.Code != want {
			t.Errorf("enforce, Host %q: %d, want %d", host, w.Code, want)
		}
	}

	sink := &logSink{}
	warn := IngressGuard(IngressOptions{Mode: "warn", Key: "fleet_ingress_guard", Logf: sink.logf}, okHandler())
	if w := guardedRequest(t, warn, http.MethodGet, "node-b."+sharerZone+":18811", nil); w.Code != http.StatusOK {
		t.Errorf("warn, shared-in node: %d, want 200", w.Code)
	}
	if got := sink.all(); got != "" {
		t.Errorf("warn logged a violation for a name under a configured zone: %q", got)
	}
}

// The arrival-address rule admits a Host that is the IP LITERAL the connection arrived on,
// and nothing else. A name never rides it, even when the connection did arrive on that
// name's address: the rebinding defence rests on this, because a rebound page's Host is its
// own domain name while the connection lands on this listener's address.
func TestIngressGuardAdmitsTheArrivalAddressOnlyForALiteral(t *testing.T) {
	installZones(t, ownZone)
	const arrival, other = "192.0.2.10", "198.51.100.7"
	h := IngressGuard(IngressOptions{Mode: "enforce", Key: "fleet_ingress_guard"}, okHandler())
	at := func(host string) int {
		r := httptest.NewRequest(http.MethodGet, "http://placeholder/fleet/health", nil)
		r.Host = host
		la := &net.TCPAddr{IP: net.ParseIP(arrival), Port: 18811}
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, la))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for host, want := range map[string]int{
		arrival + ":18811":              http.StatusOK,                 // the listener's own address
		arrival:                         http.StatusOK,                 // with no port
		other + ":18811":                http.StatusMisdirectedRequest, // another address
		"rebind.attacker.example:18811": http.StatusMisdirectedRequest, // a name, though it resolves here
		arrival + ".attacker.example":   http.StatusMisdirectedRequest, // a name that begins like the address
		"x" + arrival + ":18811":        http.StatusMisdirectedRequest, // not a literal at all
	} {
		if c := at(host); c != want {
			t.Errorf("connection arrived on %s, Host %q: %d, want %d", arrival, host, c, want)
		}
	}
}
