package nimclient

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// hostileBases are caller-supplied bases that must never receive the key: the
// shapes a prompt-injected offload_nim call can take against a substring test
// (the NVIDIA host in the path, the query, the fragment, a subdomain of the
// attacker, the userinfo), plain http to the real host, a non-default port, a
// scheme-less base, and case and trailing-dot variants.
var hostileBases = []string{
	"https://attacker.example/integrate.api/v1",
	"https://attacker.example/?x=api.nvidia.com",
	"https://api.nvidia.com.attacker.example/v1",
	"https://integrate.api.nvidia.com.attacker.example/v1",
	"https://API.NVIDIA.COM.Attacker.Example./v1",
	"https://integrate.api.nvidia.com@attacker.example/v1",
	"https://user:pw@integrate.api.nvidia.com/v1",
	"http://api.nvidia.com/v1",
	"http://integrate.api.nvidia.com/v1",
	"https://integrate.api.nvidia.com:8443/v1",
	"https://evilapi.nvidia.com/v1",
	"https://nvidia.com/v1",
	"https://attacker.example/v1#api.nvidia.com",
	"integrate.api.nvidia.com/v1",
	"",
}

// hostedBases are NVIDIA's hosted API hosts, which must keep receiving it.
var hostedBases = []string{
	"https://integrate.api.nvidia.com/v1",
	"https://ai.api.nvidia.com/v1",
	"https://api.nvidia.com/v1",
	"https://INTEGRATE.API.NVIDIA.COM/v1",
	"https://integrate.api.nvidia.com./v1",
	"https://integrate.api.nvidia.com:443/v1",
	"  https://integrate.api.nvidia.com/v1  ",
}

func TestTheKeyIsBoundToNVIDIAsExactHosts(t *testing.T) {
	t.Setenv("NGC_API_KEY", "")
	t.Setenv("NVIDIA_API_KEY", "nvapi-sek")
	for _, base := range hostileBases {
		if IsHostedNVIDIA(base) || KeyForBase(base) != "" {
			t.Errorf("hostile base %q would receive the key", base)
		}
	}
	for _, base := range hostedBases {
		if !IsHostedNVIDIA(base) || KeyForBase(base) != "nvapi-sek" {
			t.Errorf("NVIDIA base %q must keep receiving the key", base)
		}
	}
}

// On the wire: whatever host a base names, the request is delivered to a local
// server by a fake dialer, and the headers that server sees are the proof — the
// key appears in none of them for a hostile base (plain or base64-encoded; a
// base with userinfo still gets Go's Basic header built from that userinfo,
// which is the caller's own text, never the key), NVIDIA's hosts receive it as
// the Bearer token, and a keyless self-hosted NIM sends no Authorization.
func TestNoAuthorizationHeaderLeavesForAHostileBase(t *testing.T) {
	t.Setenv("NGC_API_KEY", "")
	t.Setenv("NVIDIA_API_KEY", "nvapi-sek")
	var mu sync.Mutex
	var got []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var all strings.Builder
		for k, vs := range r.Header {
			all.WriteString(k + ": " + strings.Join(vs, ",") + "\n")
		}
		mu.Lock()
		got = append(got, all.String())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	})
	tlsSrv := httptest.NewTLSServer(handler)
	defer tlsSrv.Close()
	plainSrv := httptest.NewServer(handler)
	defer plainSrv.Close()
	dialTo := func(addr string) func(ctx context.Context, network, _ string) (net.Conn, error) {
		return func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	}
	call := func(base string) string {
		t.Helper()
		c := New(base, KeyForBase(base), 5*time.Second)
		target := plainSrv.Listener.Addr().String()
		if u, err := url.Parse(base); err == nil && u.Scheme == "https" {
			target = tlsSrv.Listener.Addr().String()
		}
		c.http.Transport = &http.Transport{
			DialContext:     dialTo(target),
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}, // #nosec G402 -- the fake dialer's own test server
		}
		mu.Lock()
		got = nil
		mu.Unlock()
		_, _ = c.ListModels(context.Background())
		mu.Lock()
		defer mu.Unlock()
		if len(got) != 1 {
			t.Fatalf("base %q: the fake server saw %d requests, want 1", base, len(got))
		}
		return got[0]
	}
	leaks := func(headers string) bool {
		return strings.Contains(headers, "nvapi-sek") ||
			strings.Contains(headers, base64.StdEncoding.EncodeToString([]byte("nvapi-sek"))) ||
			strings.Contains(headers, "Bearer")
	}
	for _, base := range []string{
		"https://attacker.example/integrate.api/v1",
		"https://api.nvidia.com.attacker.example/v1",
		"https://attacker.example/?x=api.nvidia.com",
		"https://integrate.api.nvidia.com@attacker.example/v1",
		"http://api.nvidia.com/v1",
	} {
		if h := call(base); leaks(h) {
			t.Errorf("hostile base %q sent the key or a Bearer token:\n%s", base, h)
		}
	}
	if h := call("https://integrate.api.nvidia.com/v1"); !strings.Contains(h, "Authorization: Bearer nvapi-sek") {
		t.Errorf("NVIDIA's host did not receive the key as the Bearer token:\n%s", h)
	}
	if h := call("http://lan-host:8000/v1"); strings.Contains(h, "Authorization") {
		t.Errorf("a keyless self-hosted NIM sent an Authorization header:\n%s", h)
	}
}
