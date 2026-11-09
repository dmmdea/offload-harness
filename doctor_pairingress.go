package main

import (
	"fmt"
	"io"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// writePairIngressRow prints the "pair ingress:" row: whether the loopback ingress this box posts PAIR cards to accepts a
// TCP connection (one dial, 1 s). Silent when pair_workloads_enabled is off, when the emitter is off, and when frames go to a
// card relay on a member instead. A refused ingress is a WARN, never an exit code: the harness works without PAIR. The
// command-line emitters print their "ingress unreachable" warning at most once per six hours, so this row is where the
// condition stays visible.
//
//	pair ingress: OK    127.0.0.1:14324 accepts connections
//	pair ingress: WARN  127.0.0.1:14324 is not reachable (<dial error>): enable the ingress listener ... or set pair_workloads_enabled=false
func writePairIngressRow(w io.Writer, cfg config.Config) {
	p, checked := pairworkloads.ProbeIngress(cfg, pairworkloads.IngressDialTimeout)
	if !checked {
		return
	}
	if p.Reachable {
		fmt.Fprintf(w, "pair ingress: OK    %s accepts connections\n", p.Addr)
		return
	}
	fmt.Fprintf(w, "pair ingress: WARN  %s is not reachable (%v): %s\n", p.Addr, p.Err, pairworkloads.IngressFix)
}
