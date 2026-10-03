package pairworkloads

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// The attribution a request carries to the fleet node that will run its work (PAIR routing fixes,
// D7/D11): who asked, and whether the node has to card the job because the asker will not.
//
// The signal is INVERTED on purpose (rollout safety): an asker sends core.PairCardHeader = "node"
// only when its own emitter is not enabled, i.e. when nothing on the asking box will card the job.
// A build that predates the header sends nothing and keeps today's behaviour, so a job is never
// carded twice in a mixed-version fleet.

// NodeName is the name a remote placement is reported under BEFORE the node has answered: the host
// of its dispatch URL (the tailnet name the fleet config lists, which is the hostname PAIR's
// members.json carries), lowercased and without the port. The fleet node id is not a PAIR member
// name and resolves to nothing. fallback is used when base has no usable host.
func NodeName(base, fallback string) string {
	u, err := url.Parse(strings.TrimSpace(base))
	if err == nil && u.Hostname() != "" {
		return strings.ToLower(u.Hostname())
	}
	return fallback
}

// AskerName is the name this box sends as core.AskerHeader: its PAIR member name when the emitter is
// enabled and the box is a member (so the node's row and card name the same machine PAIR does), else
// the short lowercase hostname. Always sanitized; never empty unless the OS reports no hostname.
func (e *Emitter) AskerName() string {
	if e.Enabled() {
		e.idMu.Lock()
		name := e.selfName
		e.idMu.Unlock()
		if name != "" {
			return core.SanitizeAsker(name)
		}
	}
	return core.SanitizeAsker(shortHostname())
}

func shortHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	h, _, _ = strings.Cut(strings.TrimSpace(h), ".")
	return strings.ToLower(h)
}

// SetWireHeaders stamps the attribution headers on a request that creates work on a fleet node:
// always the asker's name, and the node-carding signal only when this emitter will not card the job.
// A nil emitter is a disabled one.
func (e *Emitter) SetWireHeaders(h http.Header) {
	if name := e.AskerName(); name != "" {
		h.Set(core.AskerHeader, name)
	}
	if !e.Enabled() {
		h.Set(core.PairCardHeader, core.PairCardNode)
	}
}

var (
	wireMu       sync.Mutex
	wireEmitters = map[string]*Emitter{}
)

// WireHeadersFor is SetWireHeaders for a caller that holds a config and no emitter (the
// accelerator lane, the remote lanes' request builders). The emitter it asks is cached per
// (enabled, endpoint, app dir, node-info URL, relay), so the identity files are read once per identityTTL, not per call.
// A box that reports through a card relay (relay.go) has an enabled emitter, so it sends no
// X-Offload-Pair-Card: the asker cards the job through its relay and the serving node must not card
// it too (one job, one card).
func WireHeadersFor(cfg config.Config, h http.Header) {
	c := FromConfig(cfg)
	key := strings.Join([]string{boolKey(c.Enabled), c.Endpoint, strings.TrimSpace(os.Getenv("OFFLOAD_PAIR_APPDIR")), c.NodeInfoURL, c.Relay.signature()}, "|")
	wireMu.Lock()
	e, ok := wireEmitters[key]
	if !ok {
		// No state dir: this emitter only reads identity, it never emits a frame.
		e = New(Config{Enabled: c.Enabled, Endpoint: c.Endpoint, NodeInfoURL: c.NodeInfoURL, Relay: c.Relay})
		wireEmitters[key] = e
	}
	wireMu.Unlock()
	e.SetWireHeaders(h)
}

func boolKey(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
