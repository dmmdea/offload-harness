package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

// TestTailnetURL locks the config-time shape check for remote seat endpoints:
// allowed = loopback, 100.64.0.0/10 literals, dotless MagicDNS short names,
// and hostnames under the HOUSE tailnet suffix only. Generic ".ts.net" is
// rejected on purpose — any tailnet on earth can Funnel-publish a host under
// .ts.net to the public internet, so accepting the generic suffix would let a
// config point a "local" seat at an arbitrary public server wearing a
// tailnet-looking name.
func TestTailnetURL(t *testing.T) {
	accept := []string{
		"http://127.0.0.1:11436",              // loopback literal
		"http://100.127.9.110:18811",          // tailnet CGNAT literal
		"http://lenovo-m720q:11436",           // dotless MagicDNS short name
		"http://qube.tail38a707.ts.net:11436", // FQDN under the house suffix
		"https://qube.tail38a707.ts.net:11436",
		"http://[::1]:11436",     // bracketed IPv6 loopback
		"http://localhost:11436", // dotless by shape; resolves loopback
		"http://qube:11436",
		"http://100.64.0.0:80", // CGNAT range floor is inside the /10
	}
	reject := []string{
		"http://example.com",     // public FQDN
		"http://8.8.8.8",         // public IP literal (port defaulted, still checked)
		"https://api.openai.com", // the exact cloud egress ADR 0001 forbids
		"http://evil.ts.net:443", // generic .ts.net NOT under the house suffix
		"http://192.168.1.5:11436",         // LAN literal: reachable, but not the tailnet
		"http://100.63.255.255:80",         // one address below the CGNAT /10
		"http://100.128.0.0:80",            // one address above the CGNAT /10
		"http://eviltail38a707.ts.net:443", // suffix match without the label boundary dot
		"http://tail38a707.ts.net:443",     // the bare zone is not a host UNDER it
		"gopher://100.64.1.2/x",            // non-http scheme, even with a tailnet host
		"ftp://127.0.0.1/x",
		"",
		"http://",
	}

	for _, raw := range accept {
		if err := TailnetURL(raw); err != nil {
			t.Errorf("TailnetURL(%q) = %v, want nil", raw, err)
		}
	}
	for _, raw := range reject {
		if err := TailnetURL(raw); err == nil {
			t.Errorf("TailnetURL(%q) = nil, want refusal", raw)
		}
	}
}

// pipeDialer returns a DialFunc that records every address it is asked to
// dial and hands back one end of an in-memory pipe (a real net.Conn, no
// network touched).
func pipeDialer(dialed *[]string) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		*dialed = append(*dialed, addr)
		c, s := net.Pipe()
		s.Close()
		return c, nil
	}
}

// TestSafeDialContextIPLiterals: an IP-literal dial address is judged
// directly — loopback and CGNAT pass through to the wrapped dialer unchanged;
// anything else is refused BEFORE the dialer is ever invoked (the refusal must
// cost zero network activity).
func TestSafeDialContextIPLiterals(t *testing.T) {
	cases := []struct {
		addr  string
		allow bool
	}{
		{"127.0.0.1:11436", true},
		{"100.100.1.1:18811", true},
		{"[::1]:11436", true},
		{"8.8.8.8:80", false},
		{"192.168.1.5:443", false},
		{"203.0.113.9:11436", false},
	}
	for _, tc := range cases {
		var dialed []string
		dial := SafeDialContext(pipeDialer(&dialed))
		conn, err := dial(context.Background(), "tcp", tc.addr)
		if tc.allow {
			if err != nil {
				t.Errorf("dial %q = %v, want nil", tc.addr, err)
				continue
			}
			conn.Close()
			if len(dialed) != 1 || dialed[0] != tc.addr {
				t.Errorf("dial %q: wrapped dialer saw %v, want [%s] unchanged", tc.addr, dialed, tc.addr)
			}
			continue
		}
		if err == nil {
			t.Errorf("dial %q = nil, want refusal", tc.addr)
			conn.Close()
		}
		if len(dialed) != 0 {
			t.Errorf("dial %q: wrapped dialer was invoked (%v) — a refused address must never be dialed", tc.addr, dialed)
		}
	}
}

// TestSafeDialContextResolvesAndPins is the anti-rebinding core: for a
// hostname the guard resolves it ITSELF, refuses any answer outside
// loopback/CGNAT, and hands the wrapped dialer the vetted IP LITERAL — never
// the hostname (re-dialing the name after inspecting it is the classic DNS
// rebinding bypass ingress.go documents). The resolver is pinned per case so
// the test controls the "DNS" answers.
func TestSafeDialContextResolvesAndPins(t *testing.T) {
	restore := lookupNetIP
	defer func() { lookupNetIP = restore }()

	mustAddr := func(s string) netip.Addr { return netip.MustParseAddr(s) }
	answers := map[string][]netip.Addr{
		"lenovo-m720q": {mustAddr("100.77.1.9")},
		"evil-host":    {mustAddr("203.0.113.7")},                          // rebound to a public address
		"mixed-host":   {mustAddr("203.0.113.7"), mustAddr("100.77.1.9")}, // poisoned answer alongside a real one
		"mapped-host":  {mustAddr("::ffff:100.77.1.9")},                   // IPv4-mapped IPv6 form of a tailnet address
	}
	lookupNetIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
		a, ok := answers[host]
		if !ok {
			return nil, fmt.Errorf("no such host %q", host)
		}
		return a, nil
	}

	cases := []struct {
		name     string
		addr     string
		wantDial string // "" = must refuse without dialing
	}{
		{"tailnet answer dials the pinned literal", "lenovo-m720q:11436", "100.77.1.9:11436"},
		{"public answer refused", "evil-host:11436", ""},
		{"mixed answers dial ONLY the tailnet one", "mixed-host:11436", "100.77.1.9:11436"},
		{"IPv4-mapped answer unmapped then dialed", "mapped-host:11436", "100.77.1.9:11436"},
		{"resolver failure propagates", "unknown-host:11436", ""},
	}
	for _, tc := range cases {
		var dialed []string
		dial := SafeDialContext(pipeDialer(&dialed))
		conn, err := dial(context.Background(), "tcp", tc.addr)
		if tc.wantDial == "" {
			if err == nil {
				t.Errorf("%s: dial %q = nil, want refusal", tc.name, tc.addr)
				conn.Close()
			}
			if len(dialed) != 0 {
				t.Errorf("%s: wrapped dialer saw %v, want no dial at all", tc.name, dialed)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: dial %q = %v, want nil", tc.name, tc.addr, err)
			continue
		}
		conn.Close()
		if len(dialed) != 1 || dialed[0] != tc.wantDial {
			t.Errorf("%s: wrapped dialer saw %v, want [%s] (the vetted literal, never the hostname)",
				tc.name, dialed, tc.wantDial)
		}
	}
}

// TestSafeTransportEndToEnd proves the transport actually carries HTTP:
// a loopback request succeeds, a house-suffix hostname resolving (via the
// pinned resolver) to loopback succeeds — through the resolve-and-pin path —
// and a public IP literal is refused at dial time with the guard's error.
func TestSafeTransportEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: SafeTransport(nil)}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("loopback GET through SafeTransport: %v", err)
	}
	resp.Body.Close()

	restore := lookupNetIP
	defer func() { lookupNetIP = restore }()
	lookupNetIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
		if host == "qube.tail38a707.ts.net" {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return nil, fmt.Errorf("no such host %q", host)
	}
	resp, err = client.Get("http://qube.tail38a707.ts.net:" + u.Port() + "/")
	if err != nil {
		t.Fatalf("hostname GET through resolve-and-pin: %v", err)
	}
	resp.Body.Close()

	_, err = client.Get("http://203.0.113.9:9/")
	if err == nil {
		t.Fatal("public-IP GET through SafeTransport succeeded, want dial refusal")
	}
	if !strings.Contains(err.Error(), "tailnet guard") {
		t.Errorf("refusal error = %q, want it to name the tailnet guard", err.Error())
	}
}

// TestSafeTransportPreservesFallbackShape: a caller's *http.Transport is
// cloned (never mutated) and keeps its identity-relevant settings, while the
// clone gains the dial gate and drops any env proxy — a proxy would be handed
// the request and dial anywhere on our behalf, outside the gate (the same
// reasoning as ingress.go's newIngressClient).
func TestSafeTransportPreservesFallbackShape(t *testing.T) {
	orig := http.DefaultTransport.(*http.Transport).Clone()
	orig.MaxIdleConns = 7 // a recognizable, harmless marker

	rt := SafeTransport(orig)
	tr, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("SafeTransport returned %T, want *http.Transport", rt)
	}
	if tr == orig {
		t.Fatal("SafeTransport returned the fallback itself — it must clone, never mutate")
	}
	if tr.MaxIdleConns != 7 {
		t.Errorf("MaxIdleConns = %d, want the fallback's 7 preserved", tr.MaxIdleConns)
	}
	if tr.Proxy != nil {
		t.Error("Proxy survived — an env proxy dials on our behalf outside the gate and must be stripped")
	}
	if orig.Proxy == nil {
		t.Error("the fallback transport was mutated (its Proxy was nilled)")
	}
}
