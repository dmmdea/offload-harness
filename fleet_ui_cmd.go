package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/fleetview"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// fleetUIRemotes is the roster the overview polls: the configured delegate
// remotes plus this box's own fleet-serve when it is bound beyond loopback
// (a delegator that is also a node deserves a card).
func fleetUIRemotes(cfg config.Config, explicit []string) []string {
	if len(explicit) > 0 {
		return explicit
	}
	out := append([]string(nil), cfg.DelegateRemotes...)
	if cfg.FleetListen != "" && !netguard.LoopbackAddr(cfg.FleetListen) {
		out = append(out, "http://"+cfg.FleetListen)
	}
	return out
}

// refuseListen is runFleetUI's bind-safety decision, extracted so it is
// table-testable without a real listener: nil means listen is fine to bind,
// a non-nil error is the refusal runFleetUI returns verbatim.
//
// Two checks, in order, and the order matters: the all-interfaces check must
// run FIRST and unconditionally — trusted never overrides it, because
// `--listen-trusted-network` exists to permit a single tailnet address, never
// 0.0.0.0/[::]/a bare port (that refusal has its own message for exactly that
// reason). Only past that gate does the loopback-vs-trusted check apply:
// loopback is always fine, and anything else needs trusted.
func refuseListen(listen string, trusted bool) error {
	if netguard.AllInterfaces(listen) {
		return fmt.Errorf("fleet-ui: refusing to bind all interfaces (%s)", listen)
	}
	if !trusted && !netguard.LoopbackAddr(listen) {
		return fmt.Errorf("fleet-ui: %s is not loopback; pass --listen-trusted-network to bind a tailnet address (never 0.0.0.0)", listen)
	}
	return nil
}

// refuseHailoPort is the sidecar-port half of fleet-ui's bind safety (register E-08), kept apart
// from refuseListen because it needs the loaded config. fleet-ui's documented default listen is
// 127.0.0.1:18813 and the Hailo sidecar's documented endpoint is the same port, so on a box that
// lists hailo-8l the page would squat the sidecar's port (or the sidecar's on-demand spawn would
// find it taken). Such a box refuses to bind that port and names the way out.
//
// Neither documented port moves: a box without the Hailo binds the default exactly as before, and
// the check is per device — a Coral or RKNPU box is untouched. Only the PORT is compared, whatever
// the host half, because the refusal is cheap to satisfy (pass another --listen) and a sidecar on
// the same port number is the collision the operator cares about.
func refuseHailoPort(cfg config.Config, listen string) error {
	if !cfg.HasAccelerator("hailo-8l") {
		return nil
	}
	_, listenPort, err := net.SplitHostPort(listen)
	if err != nil {
		return nil // a malformed address is refuseListen's and net.Listen's to name; this check is only about the port
	}
	endpoint := strings.TrimSpace(cfg.HailoEndpoint)
	if endpoint == "" {
		endpoint = config.Default().HailoEndpoint // the documented sidecar port, as the lane itself would read it
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" {
		return nil // an unusable base never loads (validateConfiguredBases), so there is no sidecar port to collide with
	}
	sidecarPort := u.Port()
	if sidecarPort == "" { // a portless base dials its scheme's own port
		sidecarPort = "80"
		if u.Scheme == "https" {
			sidecarPort = "443"
		}
	}
	want, werr := net.LookupPort("tcp", sidecarPort)
	got, gerr := net.LookupPort("tcp", listenPort)
	if werr != nil || gerr != nil || want != got {
		return nil
	}
	return fmt.Errorf("fleet-ui: refusing to bind %s — this box lists hailo-8l and its sidecar answers on port %d (hailo_endpoint %s); pass --listen with another port", listen, want, endpoint)
}

func runFleetUI(args []string) error {
	fs := flag.NewFlagSet("fleet-ui", flag.ExitOnError)
	fs.String("config", "", "config file path")
	listen := fs.String("listen", "127.0.0.1:18813", "listen address (loopback unless --listen-trusted-network)")
	trusted := fs.Bool("listen-trusted-network", false, "allow --listen beyond loopback (the Tailscale address). The page is read-only but unauthenticated — tailnet only, NEVER 0.0.0.0.")
	interval := fs.Duration("interval", 5*time.Second, "poll interval")
	history := fs.Int("history", 120, "sparkline points kept per node")
	var remotes multiFlag
	fs.Var(&remotes, "remote", "node base URL (repeatable; default: delegate_remotes + this box's fleet_listen)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _ := loadCfgWithSource(fs)
	if err := refuseListen(*listen, *trusted); err != nil {
		return err
	}
	if err := refuseHailoPort(cfg, *listen); err != nil {
		return err
	}
	if *interval < time.Second {
		return fmt.Errorf("fleet-ui: --interval must be at least 1s, got %s", *interval)
	}
	if *history < 1 {
		return fmt.Errorf("fleet-ui: --history must be at least 1, got %d", *history)
	}
	bases := fleetUIRemotes(cfg, remotes)
	if len(bases) == 0 {
		return fmt.Errorf("fleet-ui: no nodes to poll — set delegate_remotes in the config or pass --remote")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	p := fleetview.NewPoller(cfg, bases, *interval, *history)
	go p.Run(ctx)
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("fleet-ui: listen %s: %w", *listen, err)
	}
	fmt.Fprintf(os.Stderr, "[fleet-ui] http://%s — polling %d node(s) every %s\n", *listen, len(bases), *interval)
	srv := &http.Server{Handler: fleetview.NewHandler(p), ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
