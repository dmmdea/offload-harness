// Package rostertest is test support for code that reads the fleet roster: a black-holed node
// and a way to pin the tailnet zones for one test. It is a separate package so the production
// rosterprobe package never imports "testing".
package rostertest

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dmmdea/offload-harness/internal/netguard"
)

// Hole is a fake fleet node that is down in the way a sleeping or unplugged peer is: its port
// accepts the connection and never says anything back. A client that dials it waits for its own
// timeout, which is what makes a dead roster member expensive, and Dials says how many times
// anything tried.
type Hole struct {
	l     net.Listener
	dials atomic.Int64
	mu    sync.Mutex
	conns []net.Conn
}

// NewBlackHole listens on a loopback port (an address the tailnet guard admits) and holds every
// connection open without ever answering. It is closed when the test ends.
func NewBlackHole(t testing.TB) *Hole {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &Hole{l: l}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			h.dials.Add(1)
			h.mu.Lock()
			h.conns = append(h.conns, c)
			h.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, c := range h.conns {
			_ = c.Close()
		}
	})
	return h
}

// URL is the base URL of the node ("http://127.0.0.1:<port>").
func (h *Hole) URL() string { return "http://" + h.l.Addr().String() }

// Dials is how many connections the hole has accepted: how many times a client reached for it.
func (h *Hole) Dials() int { return int(h.dials.Load()) }

// Zones installs the tailnet zones for one test and restores the whole previous list when it
// ends (not only its first zone). Zones() with no argument installs none, which is the
// fail-closed default: a dotted tailnet name is then refused by shape.
func Zones(t testing.TB, zones ...string) {
	t.Helper()
	prev := netguard.TailnetSuffixes()
	if err := netguard.SetTailnetSuffixes(zones); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netguard.SetTailnetSuffixes(prev) })
}

// RedirectTrap is a fleet node that answers every request with a 307 to a second server, and a way to ask
// that second server how many requests (and which Authorization headers) it received. A client that sends
// the fleet bearer must never reach it.
type RedirectTrap struct {
	// URL is the base of the node that redirects.
	URL    string
	hits   atomic.Int64
	authMu sync.Mutex
	auths  []string
}

// NewRedirectTrap starts both servers on loopback (an address the tailnet guard admits); they close when
// the test ends.
func NewRedirectTrap(t testing.TB) *RedirectTrap {
	t.Helper()
	rt := &RedirectTrap{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt.hits.Add(1)
		rt.authMu.Lock()
		rt.auths = append(rt.auths, r.Header.Get("Authorization"))
		rt.authMu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	rt.URL = redirector.URL
	return rt
}

// Hits is how many requests reached the redirect target.
func (rt *RedirectTrap) Hits() int { return int(rt.hits.Load()) }

// Auths are the Authorization headers the redirect target received.
func (rt *RedirectTrap) Auths() []string {
	rt.authMu.Lock()
	defer rt.authMu.Unlock()
	return append([]string(nil), rt.auths...)
}

// LateNode is a fleet node that is down when a test starts and comes up on the SAME address when the test
// says so: the shape of a box that was unreachable when one call probed it and is serving by the time another
// call's dispatch reaches it. Every GET answers as a healthy node ("node-late"); other methods are handed to
// serve.
type LateNode struct {
	// Base is the node's base URL ("http://127.0.0.1:<port>"), refusing connections until Start.
	Base  string
	addr  string
	serve http.Handler
	t     testing.TB
}

// NewLateNode reserves a loopback address and releases it, so a dial to it is refused until Start.
func NewLateNode(t testing.TB, serve http.Handler) *LateNode {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return &LateNode{Base: "http://" + addr, addr: addr, serve: serve, t: t}
}

// Start brings the node up on its address. It is closed when the test ends.
func (n *LateNode) Start() {
	n.t.Helper()
	l, err := net.Listen("tcp", n.addr)
	if err != nil {
		n.t.Fatalf("the address reserved for the late node is gone: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"node_id":"node-late"}`))
			return
		}
		n.serve.ServeHTTP(w, r)
	}))
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	n.t.Cleanup(srv.Close)
}
