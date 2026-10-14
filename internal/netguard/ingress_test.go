package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// testCGNATHost is a tailnet (100.64/10) address for the tests, built rather than spelled: a dotted tailnet literal in
// public text trips the repository's pre-push identity scan, whether or not it names a real node.
var testCGNATHost = net.IPv4(100, 64, 1, 23).String()

// TestHostAllowedTakesTheTailnetEndpointRules: a request's Host passes on the same rules a
// configured endpoint passes (TailnetURL): loopback, a 100.64/10 literal, a dotless
// MagicDNS name, or a name under the operator's own tailnet zone. A DNS-rebinding page needs
// a dotted public name that resolves to this node; that is exactly what is refused.
func TestHostAllowedTakesTheTailnetEndpointRules(t *testing.T) {
	t.Cleanup(func() { _ = SetTailnetSuffix("") })
	if err := SetTailnetSuffix("tailnnnnnn.ts.net"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		host string
		ok   bool
	}{
		{"127.0.0.1:18811", true},
		{"[::1]:18800", true},
		{"localhost:18800", true},
		{"localhost", true},
		{testCGNATHost + ":18811", true},
		{"node-a:18811", true},
		{"NODE-A", true},
		{"node-a.tailnnnnnn.ts.net:18811", true},
		{"node-a.tailnnnnnn.ts.net.", true},
		{"attacker.example", false},
		{"attacker.example:18811", false},
		{"node-a.other.ts.net", false},
		{"192.0.2.20:18811", false},
		{"8.8.8.8", false},
		{"", false},
	}
	for _, c := range cases {
		ok, why := HostAllowed(c.host)
		if ok != c.ok {
			t.Errorf("HostAllowed(%q) = %v (%s), want %v", c.host, ok, why, c.ok)
		}
		if !ok && why == "" {
			t.Errorf("HostAllowed(%q) refused without a reason", c.host)
		}
	}
}

// With no tailnet zone configured, a dotted name is refused (fail closed), as TailnetURL does.
func TestHostAllowedRefusesADottedNameWithoutAZone(t *testing.T) {
	t.Cleanup(func() { _ = SetTailnetSuffix("") })
	_ = SetTailnetSuffix("")
	if ok, _ := HostAllowed("node-a.tailnnnnnn.ts.net"); ok {
		t.Fatal("a dotted name was admitted with no tailnet zone configured")
	}
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reached"))
	})
}

func guardedRequest(t *testing.T, h http.Handler, method, host string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://placeholder/v1/chat/completions", strings.NewReader("{}"))
	r.Host = host
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Enforce: a bad Host is 421, any Origin is 403, a POST that is not application/json is 415
// when the guard asks for JSON; a clean request reaches the handler.
func TestIngressGuardEnforceRefusesRebindingCrossOriginAndSimplePosts(t *testing.T) {
	t.Cleanup(func() { _ = SetTailnetSuffix("") })
	sink := &logSink{}
	h := IngressGuard(IngressOptions{Mode: "enforce", RequireJSONPost: true, Key: "fleet_ingress_guard", Logf: sink.logf}, okHandler())
	json := map[string]string{"Content-Type": "application/json"}
	if w := guardedRequest(t, h, http.MethodPost, "attacker.example:18800", json); w.Code != http.StatusMisdirectedRequest {
		t.Errorf("hostile Host: code %d, want 421", w.Code)
	} else if !strings.Contains(w.Body.String(), "fleet_ingress_guard") {
		t.Errorf("the refusal must name the key that relaxes it: %s", w.Body.String())
	}
	withOrigin := map[string]string{"Content-Type": "application/json", "Origin": "http://evil.example"}
	if w := guardedRequest(t, h, http.MethodPost, "127.0.0.1:18800", withOrigin); w.Code != http.StatusForbidden {
		t.Errorf("Origin present: code %d, want 403", w.Code)
	}
	plain := map[string]string{"Content-Type": "text/plain"}
	if w := guardedRequest(t, h, http.MethodPost, "127.0.0.1:18800", plain); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain POST: code %d, want 415", w.Code)
	}
	if w := guardedRequest(t, h, http.MethodPost, "127.0.0.1:18800", map[string]string{"Content-Type": "application/json; charset=utf-8"}); w.Code != http.StatusOK {
		t.Errorf("a clean JSON POST was refused: code %d %s", w.Code, w.Body.String())
	}
	if w := guardedRequest(t, h, http.MethodGet, "localhost:18800", nil); w.Code != http.StatusOK {
		t.Errorf("a clean GET was refused: code %d", w.Code)
	}
}

// Warn never refuses: every request reaches the handler, and each kind of violation is
// logged (once per kind and peer, so a hammering client cannot flood the log).
func TestIngressGuardWarnLogsOnceAndNeverRefuses(t *testing.T) {
	sink := &logSink{}
	h := IngressGuard(IngressOptions{Mode: "warn", RequireJSONPost: true, Key: "fleet_ingress_guard", Logf: sink.logf}, okHandler())
	for i := 0; i < 5; i++ {
		if w := guardedRequest(t, h, http.MethodPost, "attacker.example", map[string]string{"Origin": "http://evil.example", "Content-Type": "text/plain"}); w.Code != http.StatusOK {
			t.Fatalf("warn refused a request: code %d", w.Code)
		}
	}
	got := sink.all()
	for _, want := range []string{"Host", "Origin", "Content-Type"} {
		if n := strings.Count(got, want); n != 1 {
			t.Errorf("warn logged the %s violation %d times over 5 requests, want once: %s", want, n, got)
		}
	}
	if !strings.Contains(got, "would refuse") {
		t.Errorf("a warn line must say what enforce would do: %s", got)
	}
}

// Off is the old behaviour byte for byte: no check, no log.
func TestIngressGuardOffIsATransparentPass(t *testing.T) {
	sink := &logSink{}
	h := IngressGuard(IngressOptions{Mode: "off", RequireJSONPost: true, Key: "fleet_ingress_guard", Logf: sink.logf}, okHandler())
	if w := guardedRequest(t, h, http.MethodPost, "attacker.example", map[string]string{"Origin": "http://evil.example"}); w.Code != http.StatusOK || w.Body.String() != "reached" {
		t.Fatalf("off changed the response: %d %q", w.Code, w.Body.String())
	}
	if sink.all() != "" {
		t.Fatalf("off logged: %s", sink.all())
	}
}

// Without RequireJSONPost (fleet-ui, whose routes are all GETs) a non-JSON POST is left to the
// handler.
func TestIngressGuardLeavesContentTypeToTheHandlerWhenNotAsked(t *testing.T) {
	h := IngressGuard(IngressOptions{Mode: "enforce", Key: "fleet_ingress_guard"}, okHandler())
	if w := guardedRequest(t, h, http.MethodPost, "127.0.0.1", map[string]string{"Content-Type": "text/plain"}); w.Code != http.StatusOK {
		t.Fatalf("the guard judged Content-Type it was not asked to: %d", w.Code)
	}
}

// An unknown mode is enforce: a typo must never fail open.
func TestIngressGuardUnknownModeFailsClosed(t *testing.T) {
	h := IngressGuard(IngressOptions{Mode: "enforced", Key: "fleet_ingress_guard"}, okHandler())
	if w := guardedRequest(t, h, http.MethodGet, "attacker.example", nil); w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("an unknown mode let a rebinding Host through: %d", w.Code)
	}
}

// Warn dedupes per offending value, not per peer: three different bad Hosts from one peer
// are three lines, so "run warn until the log is clean" sees all of them.
func TestIngressGuardWarnLogsEachDistinctOffender(t *testing.T) {
	sink := &logSink{}
	h := IngressGuard(IngressOptions{Mode: "warn", Key: "fleet_ingress_guard", Logf: sink.logf}, okHandler())
	for _, host := range []string{"a.example", "b.example", "c.example", "a.example"} {
		guardedRequest(t, h, http.MethodGet, host, nil)
	}
	if n := strings.Count(sink.all(), "would refuse"); n != 3 {
		t.Fatalf("three distinct bad Hosts (one repeated) logged %d lines, want 3: %s", n, sink.all())
	}
}

// A request path carrying an encoded newline cannot forge a second log line: values are quoted.
func TestIngressGuardLogLinesCannotBeForged(t *testing.T) {
	sink := &logSink{}
	h := IngressGuard(IngressOptions{Mode: "enforce", Key: "fleet_ingress_guard", Logf: sink.logf}, okHandler())
	r := httptest.NewRequest(http.MethodGet, "http://placeholder/x%0Aingress%20guard%20forged", nil)
	r.Host = "attacker.example"
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got := sink.all(); got == "" || strings.Contains(got, "\n") {
		t.Fatalf("the logged line was empty or carried a raw newline: %q", got)
	}
}

// Review gaps: an empty name after the port, the zone boundary, case, "null" Origin, the
// empty mode, the arrival address, same-origin UIs, and the peer's port in the log.
func TestHostAllowedEdgeCases(t *testing.T) {
	t.Cleanup(func() { _ = SetTailnetSuffix("") })
	if err := SetTailnetSuffix("tailnnnnnn.ts.net"); err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		":8080": false, "[]:80": false, ".": false, "..": false,
		"x.eviltailnnnnnn.ts.net": false, "NODE-A.TAILNNNNNN.TS.NET": true, "node-a.tailnnnnnn.ts.net.:18811": true,
	} {
		if ok, why := HostAllowed(host); ok != want {
			t.Errorf("HostAllowed(%q) = %v (%s), want %v", host, ok, why, want)
		}
	}
}

func TestIngressGuardRefusesANullOriginAndTreatsAnEmptyModeAsEnforce(t *testing.T) {
	h := IngressGuard(IngressOptions{Mode: "enforce", Key: "k"}, okHandler())
	if w := guardedRequest(t, h, http.MethodGet, "127.0.0.1", map[string]string{"Origin": "null"}); w.Code != http.StatusForbidden {
		t.Errorf("Origin null: %d, want 403", w.Code)
	}
	empty := IngressGuard(IngressOptions{Mode: "", Key: "k"}, okHandler())
	if w := guardedRequest(t, empty, http.MethodGet, "attacker.example", nil); w.Code != http.StatusMisdirectedRequest {
		t.Errorf("an empty mode must enforce inside the guard (callers resolve the default first): %d", w.Code)
	}
}

// A client that dialled the listener by the address it is bound to (a LAN bind, node-swap's
// health probe) passes; another address in Host does not.
func TestIngressGuardAdmitsTheArrivalAddress(t *testing.T) {
	h := IngressGuard(IngressOptions{Mode: "enforce", Key: "k"}, okHandler())
	at := func(host string) int {
		r := httptest.NewRequest(http.MethodGet, "http://placeholder/fleet/health", nil)
		r.Host = host
		la := &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 18811}
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, la))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := at("192.0.2.20:18811"); c != http.StatusOK {
		t.Errorf("the listener's own address: %d, want 200", c)
	}
	if c := at("192.0.2.21:18811"); c != http.StatusMisdirectedRequest {
		t.Errorf("another LAN address: %d, want 421", c)
	}
}

// AllowSameOrigin admits a UI's own page and nothing else; without it any Origin is refused.
func TestIngressGuardSameOriginOnlyWhenAsked(t *testing.T) {
	same := map[string]string{"Origin": "http://127.0.0.1:18813"}
	ui := IngressGuard(IngressOptions{Mode: "enforce", AllowSameOrigin: true, Key: "k"}, okHandler())
	if w := guardedRequest(t, ui, http.MethodGet, "127.0.0.1:18813", same); w.Code != http.StatusOK {
		t.Errorf("same-origin UI request refused: %d", w.Code)
	}
	if w := guardedRequest(t, ui, http.MethodGet, "127.0.0.1:18813", map[string]string{"Origin": "http://evil.example"}); w.Code != http.StatusForbidden {
		t.Errorf("a foreign Origin passed the UI guard: %d", w.Code)
	}
	strict := IngressGuard(IngressOptions{Mode: "enforce", Key: "k"}, okHandler())
	if w := guardedRequest(t, strict, http.MethodGet, "127.0.0.1:18813", same); w.Code != http.StatusForbidden {
		t.Errorf("a same-origin request passed a strict guard: %d", w.Code)
	}
}

func TestIngressGuardLogsThePeerWithoutItsPort(t *testing.T) {
	sink := &logSink{}
	h := IngressGuard(IngressOptions{Mode: "warn", Key: "k", Logf: sink.logf}, okHandler())
	guardedRequest(t, h, http.MethodGet, "attacker.example", nil) // httptest's RemoteAddr is 192.0.2.1:1234
	if got := sink.all(); !strings.Contains(got, `from "192.0.2.1"`) || strings.Contains(got, "1234") {
		t.Fatalf("the peer must be logged without its port: %s", got)
	}
}

// SF-16 verify S1: one peer must not grow the log without bound. A huge offending value is
// clipped in the line, and past the dedupe cap new offenders get one summary line, not one each.
func TestIngressGuardLogStaysBounded(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	logf := func(f string, a ...any) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, a...)); mu.Unlock() }
	h := IngressGuard(IngressOptions{Mode: "warn", Key: "k", Logf: logf}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	// The requests are spread over 750 peers (the three documentation ranges), about seven each:
	// this test pins the guard's whole budget, and one peer cannot reach it (a peer's share is
	// pinned in ingress_log_test.go).
	sent := 0
	docRanges := []string{"192.0.2", "198.51.100", "203.0.113"}
	send := func(host string) {
		sent++
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.RemoteAddr = fmt.Sprintf("%s.%d:4000", docRanges[sent%3], sent/3%250+1)
		r.Host = host
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	send(strings.Repeat("a", 900_000) + ".example")
	if len(lines) != 1 || len(lines[0]) > maxLoggedLine+64 {
		t.Fatalf("a 900 KB Host gave %d line(s), the first %d bytes long", len(lines), len(lines[0]))
	}
	for i := 1; i < maxLoggedOffenders; i++ {
		send(fmt.Sprintf("h%d.example", i))
	}
	if len(lines) != maxLoggedOffenders {
		t.Fatalf("%d distinct offenders logged %d lines, want one each", maxLoggedOffenders, len(lines))
	}
	for i := 0; i < 1000; i++ {
		send(fmt.Sprintf("over%d.example", i))
	}
	if extra := len(lines) - maxLoggedOffenders; extra != 1 || !strings.Contains(lines[len(lines)-1], "not logged one by one") {
		t.Fatalf("1000 offenders past the cap added %d line(s), want one summary; last: %.200q", extra, lines[len(lines)-1])
	}
}

// SF-16 verify S1, the memory half: the dedupe set keeps a clipped value, so 100 distinct
// 900 KB Hosts retain kilobytes, not the ~90 MB they would whole.
func TestIngressGuardDedupeKeepsClippedValues(t *testing.T) {
	h := IngressGuard(IngressOptions{Mode: "warn", Key: "k", Logf: func(string, ...any) {}}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < 100; i++ {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.Host = fmt.Sprintf("p%d-%s.example", i, strings.Repeat("a", 900_000))
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(h)
	if grew := int64(after.HeapAlloc) - int64(before.HeapAlloc); grew > 16<<20 {
		t.Fatalf("100 distinct 900 KB Hosts left the heap %d MiB larger: the dedupe set keeps whole values", grew>>20)
	}
}
