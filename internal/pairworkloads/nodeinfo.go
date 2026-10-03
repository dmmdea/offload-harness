package pairworkloads

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/netguard"
)

// The identity fallback: a harness that runs as a different OS user than PAIR.
//
// The emitter's identity is PAIR's node-id.json (this box's UUID) under PAIR's app-data dir. PAIR
// rewrites that file 0600, so on a box where the harness runs as another user (a small ARM node: its
// fleet node runs as a service user, PAIR as the logged-in one) the file is unreadable, or the harness user's
// own default app dir does not exist, and the emitter stays disabled forever even though PAIR's worker
// is up and its loopback ingress answers. PAIR's own node-info service, which listens on loopback
// without a login, reports the same UUID as `hostUuid`, so when node-id.json is missing or unreadable
// the emitter asks it (fork services/nvpair-node-info: GET /v1/node-info, plaintext HTTP on 14318).
//
// The fallback identity is accepted only when the configured ingress ALSO answers HTTP, so a box that
// has node-info but no ingress (a view-only node, or a PAIR whose worker has no ingress) stays
// disabled: a card it posted would go nowhere. Both answers are cached (fallbackTTL), and the
// identity reload that calls them is itself throttled (identityTTL), so nothing is probed per call.
//
// The primary path (a readable node-id.json) never touches any of this.

const (
	// DefaultNodeInfoURL is PAIR's node-info on its default loopback port.
	DefaultNodeInfoURL = "http://127.0.0.1:14318/v1/node-info"
	// nodeInfoTimeout bounds each probe. Both are loopback: an answer takes milliseconds, and a
	// refused connection fails at once, so only a hung listener ever spends it.
	nodeInfoTimeout = time.Second
	// nodeInfoMaxBody bounds what is read of node-info's answer (it carries GPU inventory).
	nodeInfoMaxBody = 1 << 20
	// fallbackTTL is how long a SUCCESSFUL probe is trusted: a node's UUID does not change, and the
	// ingress being up is re-checked on this cadence. A failed probe is not cached separately; the
	// identity reload that asked is already throttled to identityTTL.
	fallbackTTL = 10 * time.Minute
)

var canonicalUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// loopbackHTTPURL reports whether raw is an http(s) URL whose host is a loopback address (or
// localhost). The fallback only ever dials this box's own PAIR.
func loopbackHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return false
	}
	return netguard.LoopbackAddr(net.JoinHostPort(u.Hostname(), "0"))
}

// fallbackIdentityLocked is this node's PAIR UUID when node-id.json cannot give it: node-info's
// hostUuid, accepted only while the configured ingress answers HTTP. "" = no fallback identity (the
// emitter stays disabled). readErr is why the primary read failed, for the one log line. Caller holds
// idMu.
func (e *Emitter) fallbackIdentityLocked(readErr error) string {
	raw := strings.TrimSpace(e.cfg.NodeInfoURL)
	if raw == "" {
		return ""
	}
	if !loopbackHTTPURL(raw) {
		e.nodeInfoWarn.Do(func() {
			log.Printf("pairworkloads: node-info URL %q is not a loopback address; the identity fallback is off", raw)
		})
		return ""
	}
	now := time.Now()
	if e.fbUUID == "" || now.Sub(e.fbUUIDAt) >= fallbackTTL {
		e.fbUUID, e.fbUUIDAt = probeHostUUID(raw), now
	}
	if e.fbUUID == "" {
		return ""
	}
	if !e.fbIngress || now.Sub(e.fbIngressAt) >= fallbackTTL {
		e.fbIngress, e.fbIngressAt = probeIngress(e.cfg.Endpoint), now
	}
	if !e.fbIngress {
		return ""
	}
	e.nodeInfoLog.Do(func() {
		log.Printf("pairworkloads: PAIR's node-id.json is not readable here (%v); this node's identity comes from PAIR's node-info at %s", readErr, raw)
	})
	return e.fbUUID
}

// probeClient dials only the loopback endpoints it is given and follows no redirect, so a loopback
// service cannot send the probe anywhere else.
func probeClient() *http.Client {
	return &http.Client{
		Timeout:       nodeInfoTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// probeHostUUID reads hostUuid from PAIR's node-info: "" unless the answer is HTTP 200 JSON whose
// hostUuid is a canonical UUID.
func probeHostUUID(rawURL string) string {
	ctx, cancel := context.WithTimeout(context.Background(), nodeInfoTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/json")
	resp, err := probeClient().Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, nodeInfoMaxBody))
	if err != nil {
		return ""
	}
	var info struct {
		HostUUID string `json:"hostUuid"`
	}
	if json.Unmarshal(body, &info) != nil {
		return ""
	}
	id := strings.TrimSpace(info.HostUUID)
	if !canonicalUUID.MatchString(id) {
		return ""
	}
	return id
}

// probeIngress reports whether anything answers HTTP at the ingress URL. It posts an empty JSON
// object, which PAIR's ingress refuses with a 4xx (it is not a frame) and which therefore opens no
// card; any HTTP answer, whatever its status, proves a listener is there. A transport error, or a
// URL that does not parse, is "no ingress".
func probeIngress(endpoint string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), nodeInfoTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := probeClient().Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	return true
}
