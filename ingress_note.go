package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
)

// ingressGuardNote is the line that says which ingress guard mode a listener runs (register
// SF-16, fleet_ingress_guard). The default is warn, which SERVES every request it would refuse,
// so a stock install is unguarded and, without this line, shows no sign of it: the only trace
// would be a log line when some request trips the rule. fleet-serve and fleet-ui print it at
// start, and doctor prints it, so an operator reads the mode where they read the rest.
func ingressGuardNote(cfg config.Config) string {
	const key = "fleet_ingress_guard"
	raw := strings.TrimSpace(cfg.FleetIngressGuard)
	switch mode := cfg.FleetIngressGuardMode(); {
	case mode == "off":
		return "ingress guard: OFF (" + key + "=off): no Host, Origin or Content-Type check; a web page in a browser that can reach this listener can drive it"
	case mode == "warn" && raw == "":
		return "ingress guard: warn (the default; " + key + " is unset): requests it would refuse are logged and SERVED; set " + key + "=enforce to refuse them"
	case mode == "warn":
		return "ingress guard: warn (" + key + "=" + strings.ToLower(raw) + "): requests it would refuse are logged and SERVED; set " + key + "=enforce to refuse them"
	case strings.ToLower(raw) == "enforce":
		return "ingress guard: enforce (" + key + "=enforce): requests that fail the Host, Origin or Content-Type rule are refused (421, 403, 415)"
	}
	// FleetIngressGuardMode resolves a value no mode matches to enforce, never to off.
	return fmt.Sprintf("ingress guard: enforce (%s=%q is not a mode, so it resolves to enforce; the config load reports it): requests that fail the Host, Origin or Content-Type rule are refused (421, 403, 415)", key, raw)
}

// writeIngressGuardSection prints the ingress guard line in doctor.
func writeIngressGuardSection(w io.Writer, cfg config.Config) {
	fmt.Fprintln(w, ingressGuardNote(cfg))
}
