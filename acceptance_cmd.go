package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dmmdea/offload-harness/internal/acceptance"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/mediacap"
	"github.com/dmmdea/offload-harness/internal/mediaops"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// `local-offload acceptance` is the gate a node must pass before it is handed
// work. It is deliberately NOT doctor: doctor stats configured files, which both
// of the 2026-07-27 fleet failures passed cleanly while every dispatched job died.
//
//	Windows node: the venv python.exe existed and was readable, but it is a uv
//	  trampoline whose base interpreter lives in ANOTHER account's roaming profile.
//	  It stats for everyone; it executes only for its owner.
//	Linux node:   the GPU lease directory existed and was readable, but was owned
//	  by a different user, so the running identity could not create a lease file.
//
// So every check here EXERCISES the capability as the running identity — it runs
// the interpreter, it writes to the lease directory — and the report leads with
// which identity that was, because in both cases the binary, config and files were
// all correct and only the account was wrong.
func runAcceptance(args []string) error {
	fs := flag.NewFlagSet("acceptance", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "emit the report as JSON")
	render := fs.Bool("render", false, "after the checks, make one REAL call per bound route and check each against a known answer")
	only := fs.String("only", "", "with --render: run only these route groups (comma-separated); implies --render")
	list := fs.Bool("list", false, "print each route group, whether it is bound and the exact command it would run; runs nothing; implies --render")
	outDir := fs.String("out-dir", "", "with --render: where the outputs stay (default: a new folder under the OS temp dir; never deleted)")
	_ = fs.Parse(args)
	cfg, src := loadCfgWithSource(fs)

	doRender := *render || *only != "" || *list
	var ropts renderOpts
	if doRender {
		var err error
		if ropts, err = newRenderOpts(*only, *outDir); err != nil {
			return err
		}
	}
	if *list {
		return listRender(cfg, src, mediacap.Routes(cfg), ropts, *asJSON)
	}

	ctx := context.Background()
	rep := acceptance.New()

	// 0. The config the verdict is about. One that failed to load (or a named file that is not there) leaves every value below
	//    on the defaults, and each render child would resolve its own config: say so, and start no child.
	configBad := false
	switch {
	case src.LoadErr != nil:
		configBad = true
		rep.Add(acceptance.Check{Name: "config", Status: acceptance.Fail,
			Detail: src.Path + " " + config.LoadFailure(src.LoadErr) + ": " + src.LoadErr.Error()})
	case src.Path != "" && src.NotFound:
		configBad = true
		rep.Add(acceptance.Check{Name: "config", Status: acceptance.Fail, Detail: src.Path + ": file not found"})
	}

	// 1. The GPU lease: machine-wide by design, and the single most common way a
	//    correctly-installed node still cannot execute anything.
	if dir, err := gpulease.LeaseDir(cfg.GPULockPath, cfg.StateDir); err != nil {
		rep.Add(acceptance.Check{Name: "gpu lease writable", Status: acceptance.Fail,
			Detail: "cannot resolve the lease directory: " + err.Error()})
	} else {
		rep.Add(acceptance.WritableDir(dir))
	}

	// 2. Every interpreter a bound route will spawn. Unbound ones SKIP.
	rep.Add(acceptance.Runnable(ctx, "node", cfg.NodePath, "--version"))
	rep.Add(acceptance.Runnable(ctx, "ffmpeg", cfg.FFmpegPath, "-version"))
	if py := mediaops.ResolveEditPython(cfg.EditPython, cfg.ComfyDir); py != "" {
		rep.Add(acceptance.Runnable(ctx, "edit python (PIL)", py, "--version"))
	} else {
		rep.Add(acceptance.Check{Name: "run edit python (PIL)", Status: acceptance.Skip, Detail: "not bound on this machine"})
	}
	if cfg.ImageGenEngine == "sdcpp" {
		rep.Add(acceptance.Runnable(ctx, "sdcpp", cfg.SdcppBin, "--help"))
	}
	// The composition lane (ADR 0059): run the installed CLI as this identity, through the
	// runner — the same allowlisted env and --json every render uses — so a node whose
	// install exists but cannot execute is caught here, not by the first composition.
	if cfg.ComposeRouteConfigured() {
		if script, err := gpugen.ResolveScript(cfg.ComposeScript); err == nil {
			rep.Add(acceptance.Runnable(ctx, "hyperframes (compose runner)", cfg.NodePath, script, "version", "--hyperframes-dir", cfg.HyperframesDir))
		}
	}

	// 3. The derived media routes. A route bound to a file that is not there is a
	//    promise this node cannot keep; one it never bound is a legitimate machine.
	//    With --only, a route no requested group uses is out of scope: it is a SKIP, not a failure of the groups asked for.
	scoped := "out of scope (--only " + *only + ")"
	routes := mediacap.Routes(cfg)
	if missing := mediacap.Missing(routes); len(missing) == 0 {
		rep.Add(acceptance.Check{Name: "media routes", Status: acceptance.Pass,
			Detail: fmt.Sprintf("%d route(s) derived; none bound to a missing file", len(routes))})
	} else {
		for _, r := range missing {
			if doRender && !ropts.wantsRoute(r.Name) {
				rep.Add(acceptance.Check{Name: "media route " + r.Name, Status: acceptance.Skip, Detail: scoped + ": " + r.Detail})
				continue
			}
			rep.Add(acceptance.Check{Name: "media route " + r.Name, Status: acceptance.Fail, Detail: r.Detail})
		}
	}

	// 4. The serving layer: every configured alias must be live, or the tasks that
	//    route to it defer at call time. A scoped run asked about named groups only; each group's own call proves its seat.
	if ropts.only != nil {
		rep.Add(acceptance.Check{Name: "model aliases live", Status: acceptance.Skip, Detail: scoped})
	} else {
		rep.Add(aliasCheck2(ctx, cfg))
	}

	// 5. --render: one real call per bound route through the real verbs (acceptance_render.go).
	//    A node is installed only when every route it binds has produced real output.
	if doRender && !configBad {
		rep.Add(runRender(ctx, cfg, src, routes, ropts)...)
	}

	// What was exercised: the render rows that passed ("render outputs" only names the folder).
	rendered, notBound := 0, 0
	for _, c := range rep.Checks {
		switch {
		case c.Name == "render outputs" || !strings.HasPrefix(c.Name, "render "):
		case c.Status == acceptance.Pass:
			rendered++
		case c.Status == acceptance.Skip:
			notBound++
		}
	}
	// A scoped run in which every requested group was skipped proved nothing: it is not READY.
	nothing := ropts.only != nil && rep.Ready && rendered == 0
	if nothing {
		rep.Ready = false
	}

	if *asJSON {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		if nothing {
			return errors.New("nothing was exercised: every requested group was skipped")
		}
		if !rep.Ready {
			return fmt.Errorf("%d acceptance check(s) failed", len(rep.Failures()))
		}
		return nil
	}

	fmt.Fprintln(os.Stdout, config.SourceLine(src))
	fmt.Printf("identity:   %s\n\n", rep.Identity)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, c := range rep.Checks {
		fmt.Fprintf(w, "%s\t%s\t%s\n", c.Status, c.Name, c.Detail)
	}
	_ = w.Flush()
	if nothing {
		fmt.Println("\nNOTHING EXERCISED — every requested group was skipped (see the SKIP rows above for why).")
		return errors.New("nothing was exercised: every requested group was skipped")
	}
	if !rep.Ready {
		fmt.Printf("\nNOT READY — %d check(s) failed. This node should not be handed work until they pass.\n", len(rep.Failures()))
		return fmt.Errorf("%d acceptance check(s) failed", len(rep.Failures()))
	}
	if !doRender {
		fmt.Println("\nREADY — the lease and the interpreters ran as this identity; no route was rendered (pass --render).")
		return nil
	}
	fmt.Printf("\nREADY — %d route group(s) rendered as this identity; %d not bound on this node.\n", rendered, notBound)
	return nil
}

// aliasCheck2 diffs the configured model aliases against the live roster, reusing
// doctor's alias set so the two verbs cannot disagree about what is configured.
func aliasCheck2(ctx context.Context, cfg config.Config) acceptance.Check {
	const name = "model aliases live"
	// A delegation client (install client) configures no local model at all: there is no roster
	// to check, and a box with no local endpoint must not fail for it.
	if !hasLocalModel(cfg) {
		return acceptance.Check{Name: name, Status: acceptance.Skip, Detail: "no local model is configured (a delegation client); the fleet serves every model"}
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	roster, err := swapclient.FetchRoster(cctx, cfg.Endpoint, 10*time.Second)
	if err != nil {
		return acceptance.Check{Name: name, Status: acceptance.Fail,
			Detail: "cannot list " + cfg.Endpoint + "/v1/models: " + err.Error()}
	}
	var missing []string
	for _, a := range modelAliases(cfg) {
		if a.Alias != "" && !roster.Serves(a.Alias) {
			missing = append(missing, a.Key+"="+a.Alias)
		}
	}
	if len(missing) > 0 {
		return acceptance.Check{Name: name, Status: acceptance.Fail,
			Detail: fmt.Sprintf("%d alias(es) not served: %v", len(missing), missing)}
	}
	return acceptance.Check{Name: name, Status: acceptance.Pass, Detail: "every configured alias is in the live roster"}
}
