package pairworkloads

import (
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// IngressDialTimeout bounds the one TCP dial doctor and offload_status make to the PAIR ingress.
const IngressDialTimeout = time.Second

// IngressFix is the operator's remedy for a refused ingress, shared by every surface that names it.
const IngressFix = "enable the ingress listener (workload-ingress.json in PAIR's app dir) or set pair_workloads_enabled=false"

// IngressProbe is one reachability reading of the loopback ingress this box's frames are posted to.
type IngressProbe struct {
	// Addr is the host:port that was dialled.
	Addr string
	// Reachable is true when the TCP connection opened. It says the listener is there, not that PAIR accepts the frames.
	Reachable bool
	// Err is the dial error when it did not.
	Err error
}

// ProbeIngress dials the ingress endpoint of cfg once. checked is false, and nothing is dialled, when there is nothing to
// check: pair_workloads_enabled is off, the emitter is off for this process, or frames go to a card relay on a member
// instead of the local ingress. Enabled() alone does not prove the ingress is listening (a PAIR identity can exist on a
// box whose ingress is off), which is why every frame there fails and warns.
func ProbeIngress(cfg config.Config, timeout time.Duration) (p IngressProbe, checked bool) {
	if !cfg.PairWorkloadsEnabled {
		return IngressProbe{}, false
	}
	switch ModeFor(cfg).Mode {
	case ModeLocal, ModeNodeInfo:
	default:
		return IngressProbe{}, false
	}
	ep := FromConfig(cfg).Endpoint
	u, err := url.Parse(strings.TrimSpace(ep))
	if err != nil || u.Hostname() == "" {
		return IngressProbe{Addr: ep, Err: errBadEndpoint}, true
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	addr := net.JoinHostPort(u.Hostname(), port)
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return IngressProbe{Addr: addr, Err: err}, true
	}
	_ = c.Close()
	return IngressProbe{Addr: addr, Reachable: true}, true
}

type endpointError string

func (e endpointError) Error() string { return string(e) }

const errBadEndpoint = endpointError("pair_workloads_endpoint is not a URL with a host")
