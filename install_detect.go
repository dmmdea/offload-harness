package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/hwdetect"
	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// hailortcliRun executes `hailortcli <args...>` for accelerator detection
// (hwdetect.DetectAccelerators). A package var so tests can stand in a fake
// device; the real runner's error (tool absent, driver down) is the normal
// no-NPU case and DetectAccelerators treats it as "no accelerator".
var hailortcliRun = func(args ...string) (string, error) {
	out, err := exec.Command("hailortcli", args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

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
	verdict.Accelerators = hwdetect.DetectAccelerators(hailortcliRun)

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
	if len(verdict.Accelerators) > 0 {
		fmt.Println("accel:   ", strings.Join(verdict.Accelerators, ", "))
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
	verdict.Accelerators = hwdetect.DetectAccelerators(hailortcliRun)
	installHome := *home
	if installHome == "" {
		installHome = config.DefaultBase()
	}

	var seed map[string]any
	rawProfiles, err := profilesJSON(*root)
	if err != nil {
		return err
	}
	doc, err := tierseed.ParseDoc(rawProfiles)
	if err != nil {
		return err
	}
	p, ok := doc.Profiles[verdict.Profile]
	if !ok {
		return fmt.Errorf("classified as %q but that tier is not in profiles.json", verdict.Profile)
	}
	if seed, err = tierseed.Resolve(p, verdict.Profile, tierseed.Options{Home: installHome}); err != nil {
		return err
	}
	// Accelerator seeds merge OVER the tier seed — same order as install.ps1, so the
	// plan predicts exactly the config the install would write. HAILO_HOME resolution
	// mirrors install.ps1 verbatim ($env:HAILO_HOME else <OFFLOAD_HOME>\hailo) and is
	// never empty: an empty HailoHome would expand __HAILO_HOME__ to "" and produce
	// the plausible-wrong "/hailo-http.cmd".
	if len(verdict.Accelerators) > 0 {
		hailoHome := os.Getenv("HAILO_HOME")
		if hailoHome == "" {
			hailoHome = filepath.Join(installHome, "hailo")
		}
		accSeed, err := tierseed.ResolveAccelerators(doc.Accelerators, verdict.Accelerators,
			tierseed.Options{Home: installHome, HailoHome: hailoHome})
		if err != nil {
			return err
		}
		if len(accSeed) > 0 {
			if seed == nil {
				seed = map[string]any{}
			}
			for k, v := range accSeed {
				seed[k] = v
			}
		}
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
