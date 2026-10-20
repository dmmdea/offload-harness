package main

import (
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
)

// writeLANIPEndpointsSection prints one WARN row for each configured endpoint the harness dials whose host
// is an IP literal in a private range, instead of a name.
//
// Cross-machine reach uses tailnet names (a dotless MagicDNS name, or a name under a zone listed in
// tailnet_suffix / tailnet_suffixes). A literal works until the box it names moves, or until its firewall is
// scoped to the tailnet, and then the endpoint is dead with nothing in the file that explains why. The rows
// are the three shapes of that mistake:
//
//	lan ip:     WARN  <field> names its host by a private-range IP literal (RFC 1918 or a unique-local address) ...
//	lan ip:     WARN  <field> names its host by a link-local IP literal ...
//	lan ip:     WARN  <field> names its host by a tailnet CGNAT IP literal: use the MagicDNS name ...
//
// Loopback is fine (the box itself), and so is every name. A public literal is not this row's business: the
// tailnet guard refuses it where it is applied, and a row about private ranges would only repeat that.
//
// A row names the field (with its list index or map key) and never prints the address: doctor output is
// pasted into issues and chats, and the operator finds the value by the field. The endpoint line at the top of
// doctor already prints cfg.Endpoint, so nothing is hidden from the operator by this.
//
// It is informational, like the skew, token and capacity rows: it never changes doctor's exit code, and a
// config with nothing to warn about prints nothing. What it reads is the config alone, so a serving layer that
// is down cannot hide it.
//
// Fields read: endpoint, seat_endpoints, cascade_remote_lanes, delegate_remotes, fleet_queue_holder and the
// explicit entries of pair_workloads_relay. Two kinds of value are left out on purpose. nim_endpoint and
// nim_bases: a self-hosted NIM on the LAN is what that allowlist is for. A kv_cache_server address: the config
// documents a literal on the direct LAN or the tailnet as a valid store address and prefers it to a name (bulk
// KV over MagicDNS measured several times slower, and the status readback dials literals only, so a name
// switches it off), which makes a row that asks for a name wrong for a store, with no way to silence it. Values
// that are not usable URLs are left to the config rows that already say so.
func writeLANIPEndpointsSection(w io.Writer, cfg config.Config) {
	for _, e := range lanIPEndpoints(cfg) {
		kind := classifyLANIP(e.host)
		if kind == lanIPNone {
			continue
		}
		fmt.Fprintf(w, "lan ip:     WARN  %s names its host by %s%s\n", e.field, kind.phrase(), kind.advice())
	}
}

// lanIPKind is what an IP literal in a host position is, for this row.
type lanIPKind int

const (
	lanIPNone      lanIPKind = iota // a name, loopback, or a public literal
	lanIPPrivate                    // RFC 1918, or the IPv6 unique-local block
	lanIPLinkLocal                  // the link-local blocks
	lanIPTailnet                    // the tailnet's own CGNAT / unique-local range
)

// The tailnet's address space: the CGNAT block for IPv4 and the tailnet unique-local /48 for IPv6. Built from
// bytes, not parsed from text, so no address of that range sits in a tracked file.
var (
	lanIPTailnetV4 = netip.PrefixFrom(netip.AddrFrom4([4]byte{100, 64, 0, 0}), 10)
	lanIPTailnetV6 = netip.PrefixFrom(netip.AddrFrom16([16]byte{0xfd, 0x7a, 0x11, 0x5c, 0xa1, 0xe0}), 48)
)

// classifyLANIP judges one host. Anything that is not an IP literal is a name, and a name is the right shape.
// An IPv4-mapped IPv6 literal is judged as the IPv4 address it carries (the unmap is what lets the tailnet
// prefix test see it: a prefix never matches an address in the 4-in-6 form), a zone suffix is ignored, and the
// tailnet ranges are told apart from the private ranges before those are tested: the tailnet's IPv6 block sits
// inside the unique-local block, and both get their own advice.
func classifyLANIP(host string) lanIPKind {
	host = strings.TrimSpace(host)
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return lanIPNone
	}
	a = a.Unmap()
	switch {
	case a.IsLoopback():
		return lanIPNone
	case lanIPTailnetV4.Contains(a), lanIPTailnetV6.Contains(a):
		return lanIPTailnet
	case a.IsPrivate():
		return lanIPPrivate
	case a.IsLinkLocalUnicast():
		return lanIPLinkLocal
	}
	return lanIPNone
}

func (k lanIPKind) phrase() string {
	switch k {
	case lanIPPrivate:
		return "a private-range IP literal (RFC 1918 or a unique-local address) instead of a name"
	case lanIPLinkLocal:
		return "a link-local IP literal instead of a name"
	case lanIPTailnet:
		return "a tailnet CGNAT IP literal instead of a name"
	}
	return ""
}

// advice is what to do about it, with the reason.
//
// The tailnet row also covers a node shared in from another tailnet, where the plain advice cannot be followed:
// its dotless MagicDNS name does not resolve here, and its full name sits under the other tailnet's zone, which
// the tailnet guard refuses unless tailnet_suffixes lists it. Renaming such a node by its full name alone drops it
// from every fleet lane, so the row names both ways out.
func (k lanIPKind) advice() string {
	switch k {
	case lanIPTailnet:
		return ": use the MagicDNS name, because a name follows the node and a literal does not; a node shared in from another tailnet has no dotless name here, and its full name is refused unless tailnet_suffixes lists its zone, so list the zone and use the full name, or keep the 100.x address"
	default:
		return ": cross-machine reach uses a tailnet name, because a literal breaks when the box moves or when its firewall is scoped to the tailnet"
	}
}

// lanIPEndpoint is one configured value that carries a host the harness dials: the field to name and the host
// read out of it.
type lanIPEndpoint struct {
	field string
	host  string
}

// lanIPEndpoints lists the dialed hosts in a fixed order, so doctor output can be diffed: the config's own
// field order, map keys sorted, lists by index.
func lanIPEndpoints(cfg config.Config) []lanIPEndpoint {
	var out []lanIPEndpoint
	base := func(field, raw string) {
		if host := lanIPBaseHost(raw); host != "" {
			out = append(out, lanIPEndpoint{field: field, host: host})
		}
	}
	base("endpoint", cfg.Endpoint)
	for _, key := range lanIPSortedKeys(cfg.SeatEndpoints) {
		base(fmt.Sprintf("seat_endpoints[%q]", key), cfg.SeatEndpoints[key])
	}
	for _, key := range lanIPSortedKeys(cfg.CascadeRemoteLanes) {
		base(fmt.Sprintf("cascade_remote_lanes[%q]", key), cfg.CascadeRemoteLanes[key])
	}
	for i, raw := range cfg.DelegateRemotes {
		base(fmt.Sprintf("delegate_remotes[%d]", i), raw)
	}
	base("fleet_queue_holder", cfg.FleetQueueHolder)
	for i, raw := range cfg.PairWorkloadsRelay {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "auto", "off":
			continue // the relay's two words, not a base
		}
		base(fmt.Sprintf("pair_workloads_relay[%d]", i), raw)
	}
	return out
}

// lanIPBaseHost is the host of a configured base URL, or "" for an empty value and for one that is not a URL with
// a host (a scheme-less or half-substituted value is reported as unusable by the endpoint-shape rows, and a
// second row about it would only be noise).
func lanIPBaseHost(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// lanIPSortedKeys visits a base-URL map in sorted order: Go randomizes map iteration, and a doctor row order that
// changes between runs cannot be diffed.
func lanIPSortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
