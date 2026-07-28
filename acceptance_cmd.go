package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/dmmdea/offload-harness/internal/acceptance"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/mediacap"
	"github.com/dmmdea/offload-harness/internal/mediaops"
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
	_ = fs.Parse(args)
	cfg, src := loadCfgWithSource(fs)

	ctx := context.Background()
	rep := acceptance.New()

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

	// 3. The derived media routes. A route bound to a file that is not there is a
	//    promise this node cannot keep; one it never bound is a legitimate machine.
	routes := mediacap.Routes(cfg)
	if missing := mediacap.Missing(routes); len(missing) == 0 {
		rep.Add(acceptance.Check{Name: "media routes", Status: acceptance.Pass,
			Detail: fmt.Sprintf("%d route(s) derived; none bound to a missing file", len(routes))})
	} else {
		for _, r := range missing {
			rep.Add(acceptance.Check{Name: "media route " + r.Name, Status: acceptance.Fail, Detail: r.Detail})
		}
	}

	// 4. The serving layer: every configured alias must be live, or the tasks that
	//    route to it defer at call time.
	rep.Add(aliasCheck2(ctx, cfg))

	if *asJSON {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
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
	if !rep.Ready {
		fmt.Printf("\nNOT READY — %d check(s) failed. This node should not be handed work until they pass.\n", len(rep.Failures()))
		return fmt.Errorf("%d acceptance check(s) failed", len(rep.Failures()))
	}
	fmt.Println("\nREADY — every bound capability was exercised as this identity.")
	return nil
}

// aliasCheck2 diffs the configured model aliases against the live roster, reusing
// doctor's alias set so the two verbs cannot disagree about what is configured.
func aliasCheck2(ctx context.Context, cfg config.Config) acceptance.Check {
	const name = "model aliases live"
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	roster, err := fetchModelRoster(cctx, cfg.Endpoint)
	if err != nil {
		return acceptance.Check{Name: name, Status: acceptance.Fail,
			Detail: "cannot list " + cfg.Endpoint + "/v1/models: " + err.Error()}
	}
	var missing []string
	for _, a := range modelAliases(cfg) {
		if a.Alias != "" && !roster[a.Alias] {
			missing = append(missing, a.Key+"="+a.Alias)
		}
	}
	if len(missing) > 0 {
		return acceptance.Check{Name: name, Status: acceptance.Fail,
			Detail: fmt.Sprintf("%d alias(es) not served: %v", len(missing), missing)}
	}
	return acceptance.Check{Name: name, Status: acceptance.Pass, Detail: "every configured alias is in the live roster"}
}
