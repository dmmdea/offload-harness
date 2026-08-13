package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/mediacap"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// `local-offload report` answers ONE question for someone who is not at the
// machine: what can this box actually do, and what is it running?
//
// It exists because that answer has been assembled by hand, over chat, every time
// a collaborator hit trouble — and the hand-assembled version was wrong more than
// once (a node reported ComfyUI it did not have; another had a media route bound
// to a file that was not there). It is strictly READ-ONLY: config, filesystem
// stats and one HTTP GET of the serving endpoint. No model is loaded, no GPU work
// is queued, nothing is written unless --out is given. Output is Markdown so it
// can be pasted straight back into a thread.
//
// Every verdict comes from the SAME code paths the harness routes on — doctor's
// alias diff and mediacap's derivation — so a report can never claim a capability
// the box would defer.

// aliasVerdict is one configured model alias measured against the live roster.
type aliasVerdict struct{ Key, Alias, State string } // State: OK | MISSING | unset

// reportInput is everything the report renders. Passing it as data keeps
// renderReport pure (and testable without a machine, an endpoint or a GPU).
type reportInput struct {
	Version      string
	Host         string
	OS, Arch     string
	Generated    string
	ConfigSource string
	Endpoint     string
	Health       string // "OK", or the failure text
	Aliases      []aliasVerdict
	Routes       []mediacap.Route
	Profile      string // installer tier id, e.g. "amd-rdna3"
	Backend      string // serving backend, e.g. "vulkan"
	ManifestPath string
	ManifestNote string // why the manifest is absent, when it is
}

// renderReport turns the gathered facts into the Markdown a collaborator sends
// back. Pure: same input, same bytes.
func renderReport(in reportInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# local-offload capability report\n\n")
	fmt.Fprintf(&b, "| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| host | %s |\n", orNA(in.Host))
	fmt.Fprintf(&b, "| harness version | %s |\n", in.Version)
	fmt.Fprintf(&b, "| platform | %s/%s |\n", in.OS, in.Arch)
	fmt.Fprintf(&b, "| generated | %s |\n", in.Generated)
	fmt.Fprintf(&b, "| config | %s |\n", orNA(in.ConfigSource))
	if in.Profile != "" {
		fmt.Fprintf(&b, "| hardware tier | %s (backend %s) |\n", in.Profile, orNA(in.Backend))
	} else {
		fmt.Fprintf(&b, "| hardware tier | UNKNOWN — %s |\n", orNA(in.ManifestNote))
	}
	fmt.Fprintf(&b, "| manifest | %s |\n", orNA(in.ManifestPath))

	fmt.Fprintf(&b, "\n## Serving\n\n")
	fmt.Fprintf(&b, "Endpoint `%s` — **%s**\n\n", in.Endpoint, in.Health)
	if len(in.Aliases) == 0 {
		b.WriteString("_No model aliases could be checked (the endpoint did not answer)._\n")
	} else {
		b.WriteString("| config key | alias | live |\n|---|---|---|\n")
		for _, a := range in.Aliases {
			alias := a.Alias
			if alias == "" {
				alias = "—"
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", a.Key, alias, a.State)
		}
	}

	fmt.Fprintf(&b, "\n## Media routes\n\n")
	fmt.Fprintf(&b, "Derived from this machine's bindings, not declared. `CONFIGURED` = bound and the "+
		"file exists · `NOT CONFIGURED` = no binding here, the task defers by design · "+
		"`BOUND-BUT-MISSING` = the config names a file that is not there, so the task fails when called.\n\n")
	b.WriteString("| route | verdict | engine | bindings |\n|---|---|---|---|\n")
	for _, r := range in.Routes {
		name := r.Name
		if r.Prereq {
			name += " _(prereq)_"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", name, r.State, r.Engine, mdEscape(r.Detail))
	}

	if missing := mediacap.Missing(in.Routes); len(missing) > 0 {
		fmt.Fprintf(&b, "\n### Needs attention (%d)\n\n", len(missing))
		for _, r := range missing {
			fmt.Fprintf(&b, "- **%s** — %s\n", r.Name, mdEscape(r.Detail))
		}
	}

	fmt.Fprintf(&b, "\n---\n\nSend this file back as-is. It is read-only output: no model was loaded, "+
		"no GPU work was queued, and nothing on this machine changed.\n")
	return b.String()
}

// mdEscape keeps a path with a pipe from breaking the table.
func mdEscape(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

func orNA(s string) string {
	if strings.TrimSpace(s) == "" {
		return "n/a"
	}
	return s
}

// gatherReport performs the read-only IO: one health probe, one /v1/models GET,
// the installer manifest, and mediacap's filesystem stats.
func gatherReport(cfg config.Config, src config.Source, routes []mediacap.Route, now time.Time) reportInput {
	host, _ := os.Hostname()
	in := reportInput{
		Version:      version,
		Host:         host,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Generated:    now.UTC().Format("2006-01-02 15:04 MST"),
		// SourceLine is doctor's fixed-width console line; the table wants the value
		// alone, and it must keep disclosing BUILT-IN DEFAULTS — a report built on
		// defaults describes a machine whose real bindings are inactive.
		ConfigSource: strings.TrimSpace(strings.TrimPrefix(config.SourceLine(src), "config:")),
		Endpoint:     cfg.Endpoint,
		Routes:       routes,
	}

	manifest := fleetnode.InstalledJSONPath()
	in.ManifestPath = manifest
	if info, err := fleetnode.ReadInstalledInfo(manifest); err == nil {
		in.Profile, in.Backend = info.Profile, info.Backend
	} else {
		// A hand-built box legitimately has no manifest; say which, don't guess a tier.
		in.ManifestNote = "no installer manifest at that path (" + err.Error() + ")"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 5*time.Second)
	if err := client.Health(ctx); err != nil {
		in.Health = "DOWN (" + err.Error() + ")"
		return in
	}
	in.Health = "OK"
	roster, err := swapclient.FetchRoster(ctx, cfg.Endpoint, 10*time.Second)
	if err != nil {
		in.Health = "OK, but /v1/models could not be listed (" + err.Error() + ")"
		return in
	}
	for _, a := range modelAliases(cfg) {
		v := aliasVerdict{Key: a.Key, Alias: a.Alias, State: "unset"}
		switch {
		case a.Alias == "":
		case roster.Serves(a.Alias):
			v.State = "OK"
		default:
			v.State = "**MISSING**"
		}
		in.Aliases = append(in.Aliases, v)
	}
	return in
}

func runReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	fs.String("config", "", "config file path")
	out := fs.String("out", "", "write the report to this file instead of stdout")
	_ = fs.Parse(args)
	cfg, src := loadCfgWithSource(fs)
	md := renderReport(gatherReport(cfg, src, mediacap.Routes(cfg), time.Now()))
	if *out == "" {
		fmt.Print(md)
		return nil
	}
	if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
	return nil
}
