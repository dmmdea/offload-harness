package modelaffinity

// A remote engine endpoint (register C-58, operator 2026-09-18: "nvidia pair
// showing the qube doing lenovo work").
//
// A config's `endpoint` is normally this box's llama-swap on loopback. A bench
// or trial config may point it at ANOTHER box's engine (the Lenovo's vLLM arm
// on :18797, reached over the tailnet) and run the delegator here. Two things
// then went wrong: every run was attributed to this box (the ledger row, the
// PAIR card), and this box's machine-wide GPU lease cordoned runs that never
// touched a local card — 8 of 16 contracts deferred "gpu busy" in 0.7 s while
// the Qube's cards sat under a lease and the work was on the Lenovo.
//
// EndpointHost is the one reading both fixes rest on: the endpoint's host when
// it is not this box, "" when it is. Attribution follows it (internal/delegate
// names the node after it), and the text-load gate is disarmed under it
// (config.Load: there is no local load for the gate to protect).

import (
	"net"
	"net/url"
	"os"
	"strings"
)

// EndpointHost returns the lower-cased host of base when base names another
// box's engine, and "" when base is this box (loopback, "localhost", this
// machine's hostname or its short form) or unparseable.
func EndpointHost(base string) string {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return ""
	}
	host := strings.ToLower(strings.Trim(u.Hostname(), "[]"))
	if host == "" || host == "localhost" {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsUnspecified() {
			return ""
		}
		return host
	}
	if hn, herr := os.Hostname(); herr == nil {
		hn = strings.ToLower(hn)
		short := hn
		if i := strings.IndexByte(hn, '.'); i > 0 {
			short = hn[:i]
		}
		hostShort := host
		if i := strings.IndexByte(host, '.'); i > 0 {
			hostShort = host[:i]
		}
		if host == hn || hostShort == short {
			return ""
		}
	}
	return host
}

// DisarmGPULease turns the text-load gate off for this process: AwaitRunSlot,
// awaitCard and GPULeaseDir then read as "not armed". config.Load calls it
// when the endpoint is another box's engine.
func DisarmGPULease() {
	leaseMu.Lock()
	defer leaseMu.Unlock()
	leaseDir = ""
}
