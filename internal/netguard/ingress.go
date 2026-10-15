package netguard

// Ingress guard for the harness's own HTTP listeners (register SF-16): the fleet node and
// `local-agent --serve`.
//
// THE THREAT. A web page in the operator's browser can reach a listener on loopback or the
// tailnet two ways. (1) A cross-site "simple" request: a POST with Content-Type text/plain
// needs no CORS preflight, so a page can fire it blind; a handler that decodes JSON from any
// Content-Type then acts on it (local-agent --serve runs a tool-using agent loop; not every
// fleet lane checks its Content-Type). (2) DNS rebinding: the page's own name is re-pointed
// at this listener, so its requests become same-origin and can read responses and send JSON.
// Both carry tells a legitimate client never has: rebinding arrives with the attacker's
// dotted name in Host, and every browser-made request that matters carries an Origin header
// (Go's http client and curl send none).
//
// THE RULES. Host must pass the rule a configured endpoint passes (TailnetURL: loopback, a
// 100.64/10 literal, a dotless MagicDNS name, or a name under ANY of the operator's
// tailnet zones: tailnet_suffix and tailnet_suffixes). Any Origin header is refused: no
// harness client is a browser. A POST must be
// application/json when the caller asks for it (both listeners do).
//
// THREE MODES, one key: off (the old behaviour byte for byte), warn (log what enforce would
// refuse, once per kind of violation, peer and offending value in each window, and serve the
// request: the rollout mode; the default, so a stock install is not guarded), enforce (refuse:
// 421 for Host, 403 for Origin, 415 for Content-Type, each naming the key that relaxes it and
// never the tailnet zones, which only the operator's log line carries). Any other mode string is enforce: an unknown mode must
// never fail open (config validation refuses it by name before it gets here).

import (
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// HostAllowed reports whether a request's Host header (host or host:port) names this
// listener in a way only the operator's own clients would: the TailnetURL rules applied to a
// bare host. why says what failed when ok is false.
//
// The name test is TailnetURL's own (tailnetNameAllowedIn), so the two cannot disagree about a
// name; the port test is url.Parse's (digits only, no range). The one place this is stricter is
// a name made only of dots, which TailnetURL reads as an empty, dotless name and admits.
func HostAllowed(hostport string) (ok bool, why string) {
	h := strings.TrimSpace(hostport)
	if h == "" {
		return false, "empty Host header"
	}
	if host, port, err := net.SplitHostPort(h); err == nil {
		// SplitHostPort splits on the last colon and checks nothing after it; url.Parse, the
		// endpoint rule, refuses a port that is not digits.
		if !portDigits(port) {
			return false, fmt.Sprintf("Host %q has a port that is not a number", hostport)
		}
		h = host
	} else if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		if tailnetAddrAllowed(addr) {
			return true, ""
		}
		return false, fmt.Sprintf("Host %q is an address outside loopback and the tailnet (100.64/10)", h)
	}
	// ONE trailing root dot is the same name; a second is not part of any name. No other
	// function trims again before the zone test.
	name := strings.ToLower(strings.TrimSuffix(h, "."))
	if name == "" {
		return false, fmt.Sprintf("Host %q has no host name", hostport)
	}
	if strings.Contains(name, ":") {
		// A colon that is neither a port separator nor part of an IPv6 literal.
		return false, fmt.Sprintf("Host %q is not a host name", hostport)
	}
	if name == "localhost" {
		return true, ""
	}
	// EVERY configured zone, as TailnetURL judges them: `tailnet_suffix` and `tailnet_suffixes`
	// (a node shared in from another tailnet is named under that tailnet's zone), or a dotless
	// MagicDNS short name. A web page cannot steer which name a short name resolves to (a hostile
	// resolver path could; the Origin rule still covers a rebound POST). The bare zone names no
	// node and TailnetURL refuses it, so it is refused here too, unless it sits under another
	// zone, which is how nested zones read.
	zones := TailnetSuffixes()
	if tailnetNameAllowedIn(zones, name) {
		return true, ""
	}
	switch len(zones) {
	case 0:
		return false, fmt.Sprintf("Host %q is a dotted name and no tailnet zone is configured (config tailnet_suffix, or tailnet_suffixes for a node shared in from another tailnet)", name)
	case 1:
		return false, fmt.Sprintf("Host %q is not loopback, a tailnet address, a MagicDNS name or a name under %s", name, zones[0])
	}
	return false, fmt.Sprintf("Host %q is not loopback, a tailnet address, a MagicDNS name or a name under one of %s", name, strings.Join(zones, ", "))
}

// portDigits reports whether p is empty or only ASCII digits: the port shape url.Parse accepts
// (it checks digits, not that they fit in 16 bits, and neither does this).
func portDigits(p string) bool {
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return false
		}
	}
	return true
}

// IngressOptions configures IngressGuard.
type IngressOptions struct {
	// Mode is off, warn or enforce; anything else is treated as enforce.
	Mode string
	// RequireJSONPost refuses (enforce) or logs (warn) a POST whose Content-Type is not
	// application/json.
	RequireJSONPost bool
	// AllowSameOrigin admits an Origin whose host is the request's own Host: a browser UI the
	// listener serves itself (fleet-ui). The fleet node and local-agent leave it false: the node
	// serves generated media (an SVG can carry script) from its own origin.
	AllowSameOrigin bool
	// Key is the config key a refusal names, so the operator knows what relaxes it.
	Key string
	// Logf receives the warn and enforce lines; nil discards them.
	Logf func(format string, args ...any)
}

// The log is bounded in both directions (SF-16 verify) and does not go blind (offenderLog): a
// peer must not grow the guard's memory or the log without limit, in any mode, nor silence it.
// The dedupe key is a fixed-size hash of the whole offending value, so a 900 KB Host costs a
// hash, not its length; values echoed to a client or a log line are clipped to maxLoggedValue
// bytes and each line to maxLoggedLine. In each dedupeWindow, at most maxLoggedOffenders
// distinct offenders are logged one by one and at most maxLoggedPerPeer of those from one peer;
// the rest are counted, and a summary line at most every overflowSummaryEvery says how many.
const (
	maxLoggedOffenders   = 4096
	maxLoggedPerPeer     = 64
	dedupeWindow         = 5 * time.Minute
	maxLoggedValue       = 200
	maxLoggedLine        = 1024
	overflowSummaryEvery = time.Minute
)

// clipTo shortens s to at most n bytes, on a rune boundary, and says how long it was.
func clipTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("...(%d bytes)", len(s))
}

// guardLogConfig is the budget the guard's log runs on: the package constants, on the real clock.
func guardLogConfig(key string, logf func(format string, args ...any)) offenderLogConfig {
	return offenderLogConfig{
		key:          key,
		logf:         logf,
		now:          time.Now,
		maxOffenders: maxLoggedOffenders,
		maxPerPeer:   maxLoggedPerPeer,
		window:       dedupeWindow,
		summaryEvery: overflowSummaryEvery,
	}
}

// IngressGuard wraps next with the SF-16 checks.
func IngressGuard(o IngressOptions, next http.Handler) http.Handler {
	mode := strings.ToLower(strings.TrimSpace(o.Mode))
	if mode == "off" {
		return next
	}
	warn := mode == "warn"
	logOnce := newOffenderLog(guardLogConfig(o.Key, o.Logf)).once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer := r.RemoteAddr
		if host, _, err := net.SplitHostPort(peer); err == nil {
			peer = host
		}
		// why is for the operator's log. public is what the caller is told, and the caller may be
		// a rebound page in a browser, so it never carries the tailnet zones or the keys that set
		// them (the operator's private names); it echoes only the caller's own value, clipped.
		type violation struct {
			kind, offender, why, public string
			code                        int
		}
		var vs []violation
		if ok, why := HostAllowed(r.Host); !ok && !hostIsArrivalAddr(r) {
			vs = append(vs, violation{"Host", r.Host, why, fmt.Sprintf("Host %q is not an allowed name for this listener", clipTo(r.Host, maxLoggedValue)), http.StatusMisdirectedRequest})
		}
		if origin := r.Header.Get("Origin"); origin != "" && !(o.AllowSameOrigin && sameOrigin(origin, r.Host)) {
			why := fmt.Sprintf("Origin %q present: harness clients are not browsers", clipTo(origin, maxLoggedValue))
			vs = append(vs, violation{"Origin", origin, why, why, http.StatusForbidden})
		}
		if o.RequireJSONPost && r.Method == http.MethodPost {
			ct := r.Header.Get("Content-Type")
			mt, _, err := mime.ParseMediaType(ct)
			if err != nil || mt != "application/json" {
				why := fmt.Sprintf("POST Content-Type %q is not application/json", clipTo(ct, maxLoggedValue))
				vs = append(vs, violation{"Content-Type", ct, why, why, http.StatusUnsupportedMediaType})
			}
		}
		if len(vs) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		if warn {
			for _, v := range vs {
				logOnce(v.kind, peer, v.offender, fmt.Sprintf("ingress guard (%s=warn): would refuse %q %q from %q: %s; set %s=enforce to refuse, or off to silence",
					o.Key, r.Method, clipTo(r.URL.Path, maxLoggedValue), peer, v.why, o.Key))
			}
			next.ServeHTTP(w, r)
			return
		}
		v := vs[0]
		logOnce(v.kind, peer, v.offender, fmt.Sprintf("ingress guard (%s=enforce): refused %q %q from %q: %s", o.Key, r.Method, clipTo(r.URL.Path, maxLoggedValue), peer, v.why))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(v.code)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "error",
			"error":  v.public + " (refused by " + o.Key + "=enforce; set it to warn or off to allow)",
		})
	})
}

// hostIsArrivalAddr reports whether the request's Host is the IP address the connection
// arrived on: a client that dialled this listener by its own address (a node bound to a LAN
// address, node-swap's health probe, fleet-ui's base URL). A DNS-rebinding page arrives with
// its own domain name in Host, never this listener's IP.
func hostIsArrivalAddr(r *http.Request) bool {
	h := r.Host
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	hostIP, err := netip.ParseAddr(h)
	if err != nil {
		return false
	}
	la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || la == nil {
		return false
	}
	lh, _, err := net.SplitHostPort(la.String())
	if err != nil {
		return false
	}
	localIP, err := netip.ParseAddr(strings.Trim(lh, "[]"))
	return err == nil && localIP.Unmap() == hostIP.Unmap()
}

// sameOrigin reports whether an Origin header names the request's own Host (scheme ignored).
func sameOrigin(origin, host string) bool {
	if origin == "null" {
		return false
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, host)
}
