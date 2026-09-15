package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

// TestCheckPublicIPTable walks every range the predicate must refuse — one
// representative per reserved block plus the stdlib-covered families — and a
// few genuinely public addresses. The blocked list is the SPEC: a range that
// loses coverage fails here rather than in a lane's fetch path six months on.
func TestCheckPublicIPTable(t *testing.T) {
	blocked := map[string]string{
		"127.0.0.1":            "loopback",
		"127.1.2.3":            "loopback (the whole /8)",
		"::1":                  "IPv6 loopback",
		"::ffff:127.0.0.1":     "IPv4-mapped loopback",
		"10.1.2.3":             "RFC1918 10/8",
		"172.16.0.1":           "RFC1918 172.16/12",
		"192.168.1.1":          "RFC1918 192.168/16",
		"fd00::1":              "IPv6 ULA",
		"169.254.0.1":          "link-local",
		"169.254.169.254":      "cloud metadata (link-local, NOT IsPrivate)",
		"fe80::1":              "IPv6 link-local",
		"0.0.0.0":              "unspecified",
		"::":                   "IPv6 unspecified",
		"0.0.0.1":              "0.0.0.0/8 this-host (only 0.0.0.0 is IsUnspecified)",
		"0.1.2.3":              "0.0.0.0/8 this-host",
		"100.64." + "0.1":      "CGNAT / tailnet (NOT IsPrivate)",
		"100." + "127.255.254": "CGNAT top of range",
		"192.0.0.1":            "IETF protocol assignments",
		"192.0.2.1":            "TEST-NET-1",
		"192.88.99.1":          "6to4 relay anycast",
		"198.18.0.1":           "benchmarking",
		"198.51.100.1":         "TEST-NET-2",
		"203.0.113.1":          "TEST-NET-3",
		"240.0.0.1":            "reserved 240/4",
		"255.255.255.255":      "broadcast (inside 240/4)",
		"224.0.0.1":            "multicast",
		"ff02::1":              "IPv6 link-local multicast",
		"ff01::1":              "IPv6 interface-local multicast",
		"64:ff9b::7f00:1":      "NAT64 wrapping 127.0.0.1",
		"64:ff9b:1::1":         "local-use IPv4/IPv6 translation",
		"2002:7f00:1::1":       "6to4 wrapping 127.0.0.1",
		"2001::1":              "Teredo",
		"2001:db8::1":          "IPv6 documentation",
		"3fff::1":              "IPv6 documentation (RFC 9637)",
		"5f00::1":              "SRv6 SIDs (RFC 9602)",
		"100::1":               "IPv6 discard-only",
		"2001:20::1":           "ORCHIDv2",
		"2620:4f:8000::1":      "AS112 direct delegation",
	}
	for s, why := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test address %q", s)
		}
		if err := CheckPublicIP(ip); err == nil {
			t.Errorf("CheckPublicIP(%s) = nil, want blocked (%s)", s, why)
		}
		if PublicIP(ip) {
			t.Errorf("PublicIP(%s) = true, want false (%s)", s, why)
		}
	}

	for _, s := range []string{
		"8.8.8.8", "1.1.1.1", "93.184.216.34", "140.82.121.4", "99.64.0.1", "100." + "63.255.255", "101.0.0.1",
		"2606:4700:4700::1111", "2001:4860:4860::8888",
	} {
		ip := net.ParseIP(s)
		if err := CheckPublicIP(ip); err != nil {
			t.Errorf("CheckPublicIP(%s) = %v, want allowed (public)", s, err)
		}
		if !PublicIP(ip) {
			t.Errorf("PublicIP(%s) = false, want true", s)
		}
	}

	if err := CheckPublicIP(nil); err == nil {
		t.Error("CheckPublicIP(nil) = nil, want an error — an unknown address is not a public one")
	}

	// The two gaps that make net.IP.IsPrivate insufficient on its own, asserted
	// so a future reader does not "simplify" the predicate back down to it.
	if net.ParseIP("169.254.169.254").IsPrivate() {
		t.Error("sanity: 169.254.169.254 IsPrivate() should be false (link-local, not RFC1918)")
	}
	if net.ParseIP("100.64." + "0.1").IsPrivate() {
		t.Error("sanity: the CGNAT block's IsPrivate() should be false (CGNAT is not RFC1918)")
	}
}

func TestPublicDialControl(t *testing.T) {
	for _, bad := range []string{"127.0.0.1:80", "169.254.169.254:80", "100.64." + "0.1:18811", "[::1]:443"} {
		if err := PublicDialControl("tcp4", bad, nil); err == nil {
			t.Errorf("PublicDialControl(%q) = nil, want blocked", bad)
		}
	}
	if err := PublicDialControl("tcp4", "8.8.8.8:443", nil); err != nil {
		t.Errorf("PublicDialControl public = %v, want nil", err)
	}
	if err := PublicDialControl("udp", "8.8.8.8:53", nil); err == nil {
		t.Error("PublicDialControl should refuse a non-tcp network")
	}
	if err := PublicDialControl("tcp4", "example.com:443", nil); err == nil {
		t.Error("PublicDialControl should refuse a non-literal dial address — it fires after DNS, so a name here means the guard was bypassed")
	}
}

// pinLookup scripts netguard's resolution seam for one test.
func pinLookup(t *testing.T, answers map[string][]string) {
	t.Helper()
	restore := LookupIP
	t.Cleanup(func() { LookupIP = restore })
	seen := map[string]int{}
	LookupIP = func(_ context.Context, host string) ([]netip.Addr, error) {
		list, ok := answers[host]
		if !ok {
			return nil, fmt.Errorf("no script for %q", host)
		}
		i := seen[host]
		seen[host]++
		if i >= len(list) {
			i = len(list) - 1
		}
		a, err := netip.ParseAddr(list[i])
		if err != nil {
			return nil, err
		}
		return []netip.Addr{a}, nil
	}
}

func TestPublicDialContextPinsTheResolvedAddress(t *testing.T) {
	var got []string
	next := func(_ context.Context, _, addr string) (net.Conn, error) {
		got = append(got, addr)
		return nil, errors.New("dial reached next")
	}

	pinLookup(t, map[string][]string{"good.example": {"93.184.216.34"}})
	if _, err := PublicDialContext(next)(context.Background(), "tcp", "good.example:443"); err == nil || !strings.Contains(err.Error(), "dial reached next") {
		t.Fatalf("public host: err = %v, want the next-dialer's error", err)
	}
	if len(got) != 1 || got[0] != "93.184.216.34:443" {
		t.Fatalf("next was handed %v, want the vetted IP literal — a NAME can be re-resolved under the guard", got)
	}

	// The rebind: public at validation, private at the dial.
	got = nil
	pinLookup(t, map[string][]string{"rebind.example": {"93.184.216.34", "127.0.0.1"}})
	_, _ = LookupIP(context.Background(), "rebind.example") // the validator's lookup
	_, err := PublicDialContext(next)(context.Background(), "tcp", "rebind.example:80")
	if err == nil || !strings.Contains(err.Error(), "DNS-rebinding guard") {
		t.Fatalf("rebind: err = %v, want a rebinding refusal", err)
	}
	if len(got) != 0 {
		t.Fatalf("next was dialed %v after a rebind; the guard must refuse before connect(2)", got)
	}

	// A literal is judged directly, without any lookup at all.
	got = nil
	pinLookup(t, map[string][]string{})
	if _, err := PublicDialContext(next)(context.Background(), "tcp", "127.0.0.1:80"); err == nil {
		t.Error("literal loopback accepted")
	}
	if len(got) != 0 {
		t.Fatalf("next was dialed %v for a loopback literal", got)
	}
}

func TestPublicTransportAlwaysCarriesTheGate(t *testing.T) {
	// nil, a plain transport, and a non-*http.Transport RoundTripper must all
	// come back guarded: the dial gate is the one part that cannot be optional.
	for name, fallback := range map[string]http.RoundTripper{
		"nil":       nil,
		"transport": &http.Transport{MaxIdleConns: 7},
		"roundtrip": roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("nope") }),
		"withProxy": &http.Transport{Proxy: http.ProxyFromEnvironment},
	} {
		tr, ok := PublicTransport(fallback).(*http.Transport)
		if !ok {
			t.Fatalf("%s: PublicTransport did not return an *http.Transport", name)
		}
		if tr.Proxy != nil {
			t.Errorf("%s: Proxy survived — an env proxy would dial outside the gate", name)
		}
		pinLookup(t, map[string][]string{})
		if _, err := tr.DialContext(context.Background(), "tcp", "127.0.0.1:80"); err == nil {
			t.Errorf("%s: dialed loopback through the returned transport", name)
		}
	}

	// The caller's own transport is cloned, never mutated.
	orig := &http.Transport{MaxIdleConns: 7}
	_ = PublicTransport(orig)
	if orig.DialContext != nil {
		t.Error("PublicTransport mutated the caller's transport")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
