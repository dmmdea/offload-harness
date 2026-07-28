package main

import (
	"encoding/json"
	"flag"
	"fmt"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/hwdetect"
	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// `install detect` and `install plan` answer the two questions an install begins
// with — what IS this machine, and what would an install do here — on any OS.
//
// setup/detect.ps1 answers the first only on Windows: its second statement is
// `if ($os -ne 'windows') { Write-Error ...; exit 1 }`. So a Linux box could never be
// told what it is, and its serving topology, resident tier and media bindings were
// hand-derived instead. The two hand-derivations on the measured Linux node were both
// wrong in ways that broke chat — which is the entire argument for a classifier.
//
// Both verbs are READ-ONLY: they probe, classify and print. Nothing is installed,
// downloaded or written.

func runInstallDetect(args []string) error {
	fs := flag.NewFlagSet("install detect", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit the facts + verdict as JSON (for installer wrappers)")
	_ = fs.Parse(args)

	facts := hwdetect.Detect()
	verdict := hwdetect.Classify(facts)

	if *asJSON {
		b, err := json.MarshalIndent(map[string]any{"facts": facts, "verdict": verdict}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}

	fmt.Println("os:      ", facts.OS)
	fmt.Println("gpu:     ", dash(facts.GPUName))
	fmt.Printf("vendor:   %s / %s\n", facts.Vendor, facts.Arch)
	fmt.Printf("vram:     %.1f GB   gpus: %d\n", facts.VRAMGb, facts.GPUCount)
	fmt.Printf("ram:      %d GB\n", facts.RAMGb)
	if facts.DriverVersion != "" {
		fmt.Println("driver:  ", facts.DriverVersion)
	}
	fmt.Println()
	fmt.Println("tier:    ", verdict.Profile)
	fmt.Println("  because:", verdict.Reason)
	if verdict.BigRAM {
		fmt.Println("  big_ram: true")
	}
	return nil
}

func runInstallPlan(args []string) error {
	fs := flag.NewFlagSet("install plan", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit the whole plan as JSON")
	home := fs.String("home", "", "install root the plan should assume (default: the resolved harness home)")
	root := fs.String("root", ".", "repo root holding setup/templates/profiles.json")
	_ = fs.Parse(args)

	facts := hwdetect.Detect()
	verdict := hwdetect.Classify(facts)
	installHome := *home
	if installHome == "" {
		installHome = config.DefaultBase()
	}

	var seed map[string]any
	rawProfiles, err := profilesJSON(*root)
	if err != nil {
		return err
	}
	profiles, err := tierseed.Parse(rawProfiles)
	if err != nil {
		return err
	}
	p, ok := profiles[verdict.Profile]
	if !ok {
		return fmt.Errorf("classified as %q but that tier is not in profiles.json", verdict.Profile)
	}
	if seed, err = tierseed.Resolve(p, verdict.Profile, tierseed.Options{Home: installHome}); err != nil {
		return err
	}

	if *asJSON {
		b, err := json.MarshalIndent(map[string]any{
			"facts": facts, "verdict": verdict, "home": installHome, "config_seed": seed,
		}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}

	fmt.Printf("tier:     %s  (%s)\n", verdict.Profile, verdict.Reason)
	fmt.Printf("machine:  %s %s %s, %.1f GB VRAM x%d, %d GB RAM\n",
		facts.OS, facts.Vendor, facts.Arch, facts.VRAMGb, facts.GPUCount, facts.RAMGb)
	fmt.Println("home:    ", installHome)
	fmt.Printf("docs:     docs/tiers/%s.md\n\n", verdict.Profile)
	if seed == nil {
		fmt.Println("media:    this tier ships no media configuration — it serves text only until an operator binds it")
		return nil
	}
	fmt.Println("media bindings this tier seeds:")
	b, err := json.MarshalIndent(seed, "  ", "  ")
	if err != nil {
		return err
	}
	fmt.Println("  " + string(b))
	return nil
}
