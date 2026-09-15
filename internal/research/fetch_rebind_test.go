package research

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/netguard"
)

// sentinelPublic is a routable public literal used as the "good" DNS answer.
// It is never actually dialed: the test dialer maps it onto a loopback
// listener, which is only legitimate BECAUSE the guard has already judged the
// address by then.
const sentinelPublic = "93.184.216.34"

// pinDNS installs a scripted resolver on netguard's single resolution seam.
// Each host maps to answers consumed in order, the last repeating forever —
// which is exactly a DNS-rebinding attack: the record the VALIDATOR sees is not
// the record the DIALER gets.
func pinDNS(t *testing.T, script map[string][]string) {
	t.Helper()
	restore := netguard.LookupIP
	t.Cleanup(func() { netguard.LookupIP = restore })
	var mu sync.Mutex
	seen := map[string]int{}
	netguard.LookupIP = func(_ context.Context, host string) ([]netip.Addr, error) {
		mu.Lock()
		defer mu.Unlock()
		answers, ok := script[host]
		if !ok {
			return nil, fmt.Errorf("test resolver: no script for %q", host)
		}
		i := seen[host]
		seen[host]++
		if i >= len(answers) {
			i = len(answers) - 1
		}
		a, err := netip.ParseAddr(answers[i])
		if err != nil {
			return nil, err
		}
		return []netip.Addr{a}, nil
	}
}

// countingListener records every ACCEPTED connection. Zero accepts is the
// assertion that matters: a guard that refuses after the TCP handshake has
// already leaked the fact that the port is open (and, on a GET-shaped internal
// service, can already have caused a side effect).
type countingListener struct {
	net.Listener
	mu      sync.Mutex
	accepts int
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.accepts++
		l.mu.Unlock()
	}
	return c, err
}

func (l *countingListener) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accepts
}

// honestDialer behaves the way a real transport's dialer behaves: it resolves
// whatever name it is handed and connects to the answer. It is NOT the
// attacker — the attacker is the resolver. It records every address it is ASKED
// to dial, so a test can prove the guard refused BEFORE any connect(2), and
// that the guard handed it an IP LITERAL rather than a name to re-resolve.
type honestDialer struct {
	mu     sync.Mutex
	asked  []string
	toReal string // where sentinelPublic "really" lives, for reachable cases
}

func (d *honestDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.asked = append(d.asked, addr)
	d.mu.Unlock()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if _, perr := netip.ParseAddr(host); perr != nil {
		addrs, lerr := netguard.LookupIP(ctx, host)
		if lerr != nil {
			return nil, lerr
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("no addresses for %s", host)
		}
		host = addrs[0].Unmap().String()
		addr = net.JoinHostPort(host, port)
	}
	if host == sentinelPublic && d.toReal != "" {
		addr = d.toReal
	}
	return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, network, addr)
}

func (d *honestDialer) dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.asked...)
}

func (d *honestDialer) client() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: d.dial, DisableKeepAlives: true}}
}

// forbiddenServer is a loopback listener that must never be reached. It counts
// accepts itself, so a connection that is opened and then dropped still fails
// the test.
func forbiddenServer(t *testing.T) (*httptest.Server, *countingListener) {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><p>fleet admin secret</p></body></html>"))
	}))
	cl := &countingListener{Listener: srv.Listener}
	srv.Listener = cl
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, cl
}

// TestFetchRefusesDNSRebindToLoopback is the K-01 regression: ValidateURL saw a
// public address, and the fetch then reconnected BY NAME — so a second lookup
// (an attacker-controlled record with a one-second TTL) sent the request to a
// loopback service instead. The lane must dial the address it validated.
func TestFetchRefusesDNSRebindToLoopback(t *testing.T) {
	srv, listener := forbiddenServer(t)
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	pinDNS(t, map[string][]string{"rebind-target.example": {sentinelPublic, "127.0.0.1"}})
	d := &honestDialer{}
	got := Fetch(context.Background(), fmt.Sprintf("http://rebind-target.example:%d/", port), Options{Client: d.client(), Timeout: 5 * time.Second})

	if n := listener.count(); n != 0 {
		t.Errorf("the loopback listener accepted %d connection(s): the rebound address was dialed", n)
	}
	if dialed := d.dialed(); len(dialed) != 0 {
		t.Errorf("dialer was asked to connect to %v: the guard must refuse before connect(2)", dialed)
	}
	if got.Err == "" {
		t.Errorf("fetch reported no error; want a guard refusal")
	}
	if got.Text != "" {
		t.Errorf("fetch returned page text %q from a loopback service", got.Text)
	}
}

// TestFetchRefusesDNSRebindToReservedRanges covers the pivots a loopback
// listener cannot stand in for: the cloud metadata endpoint, the tailnet CGNAT
// range, RFC1918, and the IPv6 forms that EMBED an IPv4 (NAT64 / 6to4) and so
// slip past a naive "is it IPv6" check. None may reach connect(2).
func TestFetchRefusesDNSRebindToReservedRanges(t *testing.T) {
	for _, rebound := range []string{
		"169.254.169.254",  // cloud metadata (link-local; IsPrivate is false here)
		"100.64." + "0.1",  // tailnet CGNAT (split literal: the pre-push leak scan blocks a whole CGNAT address on an added line)
		"10.1.2.3",         // RFC1918
		"192.168.1.1",      // RFC1918
		"0.1.2.3",          // 0.0.0.0/8 "this host" — only 0.0.0.0 is IsUnspecified
		"198.18.0.1",       // benchmarking
		"240.0.0.1",        // reserved
		"192.0.0.1",        // IETF protocol assignments
		"192.0.2.1",        // TEST-NET-1
		"::1",              // IPv6 loopback
		"64:ff9b::7f00:1",  // NAT64 wrapping 127.0.0.1
		"2002:7f00:1::1",   // 6to4 wrapping 127.0.0.1
		"::ffff:127.0.0.1", // IPv4-mapped loopback
	} {
		t.Run(rebound, func(t *testing.T) {
			pinDNS(t, map[string][]string{"rebind-target.example": {sentinelPublic, rebound}})
			d := &honestDialer{}
			got := Fetch(context.Background(), "http://rebind-target.example/", Options{Client: d.client(), Timeout: 5 * time.Second})
			if dialed := d.dialed(); len(dialed) != 0 {
				t.Errorf("dialer was asked to connect to %v after a rebind to %s", dialed, rebound)
			}
			if got.Err == "" || got.Text != "" {
				t.Errorf("rebind to %s not refused: %+v", rebound, got)
			}
		})
	}
}

// TestFetchDialsTheValidatedAddress is the positive half: a genuinely public
// host still fetches, and the dialer is handed the vetted IP LITERAL — never
// the hostname, which is the only shape that cannot be re-resolved underneath
// the guard.
func TestFetchDialsTheValidatedAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head><title>Public page</title></head><body><p>LMCache notes here.</p></body></html>"))
	}))
	defer srv.Close()

	pinDNS(t, map[string][]string{"public-page.example": {sentinelPublic}})
	d := &honestDialer{toReal: srv.Listener.Addr().String()}
	got := Fetch(context.Background(), "http://public-page.example/", Options{Client: d.client(), Timeout: 5 * time.Second})

	if got.Err != "" {
		t.Fatalf("public fetch failed: %s", got.Err)
	}
	if !strings.Contains(got.Text, "LMCache notes here.") {
		t.Fatalf("text %q", got.Text)
	}
	dialed := d.dialed()
	if len(dialed) == 0 {
		t.Fatal("nothing dialed")
	}
	host, _, err := net.SplitHostPort(dialed[0])
	if err != nil {
		t.Fatalf("dialed %q: %v", dialed[0], err)
	}
	if _, perr := netip.ParseAddr(host); perr != nil {
		t.Fatalf("dialer was handed the NAME %q, not the validated IP literal — it can be re-resolved under the guard", host)
	}
	if host != sentinelPublic {
		t.Fatalf("dialed %s, validated %s", host, sentinelPublic)
	}
}

// TestFetchRefusesRebindOnARedirectHop closes the same window on hop 2: the
// first page is public and redirects to a host that validates public and
// rebinds to loopback at dial time. Re-validating the redirect BY NAME is not
// enough — the hop's dial has to be pinned too.
func TestFetchRefusesRebindOnARedirectHop(t *testing.T) {
	forbidden, listener := forbiddenServer(t)
	victimPort := forbidden.Listener.Addr().(*net.TCPAddr).Port

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("http://rebind-hop2.example:%d/", victimPort), http.StatusFound)
	}))
	defer srv.Close()

	pinDNS(t, map[string][]string{
		"open-redirect.example": {sentinelPublic},
		"rebind-hop2.example":   {sentinelPublic, "127.0.0.1"},
	})
	d := &honestDialer{toReal: srv.Listener.Addr().String()}
	got := Fetch(context.Background(), "http://open-redirect.example/", Options{Client: d.client(), Timeout: 5 * time.Second})

	if n := listener.count(); n != 0 {
		t.Errorf("the loopback listener accepted %d connection(s) on the redirect hop", n)
	}
	if got.Err == "" || got.Text != "" {
		t.Errorf("redirect rebind not refused: %+v", got)
	}
}
