package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/dmmdea/offload-harness/internal/tierseed"
	"github.com/dmmdea/offload-harness/internal/volumes"
)

// `local-offload install …` is the verb group the installer's DECISIONS move into,
// so PowerShell and shell wrappers fetch and place files while one implementation
// — this one, cross-compiled and unit-tested — decides anything that can be wrong.
// `volumes` is the first: where should this machine's harness, models and media
// live? The answer was previously "$HOME", i.e. the OS drive, on every machine.
func runInstall(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("install needs a subcommand: volumes")
	}
	switch args[0] {
	case "volumes":
		return runInstallVolumes(args[1:])
	case "seed":
		return runInstallSeed(args[1:])
	default:
		return fmt.Errorf("unknown install subcommand %q (have: volumes, seed)", args[0])
	}
}

func runInstallVolumes(args []string) error {
	fs := flag.NewFlagSet("install volumes", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit the enumeration + choice as JSON (for the installer wrappers)")
	minFreeGB := fs.Int("min-free-gb", 0, "minimum free space a volume must have to qualify (0 = the built-in floor)")
	all := fs.Bool("all", false, "list every filesystem instead of the roomiest ones")
	allowOS := fs.Bool("allow-os-volume", false, "permit the OS volume when nothing else qualifies — an explicit decision, never a fallback")
	_ = fs.Parse(args)

	vols, err := volumes.List()
	if err != nil {
		return fmt.Errorf("enumerating volumes: %w", err)
	}
	opt := volumes.PickOptions{AllowOSVolume: *allowOS}
	if *minFreeGB > 0 {
		opt.MinFreeBytes = uint64(*minFreeGB) * volumes.GiB
	}
	choice, pickErr := volumes.Pick(vols, opt)

	if *asJSON {
		payload := map[string]any{"volumes": vols}
		if pickErr != nil {
			payload["error"] = pickErr.Error()
		} else {
			payload["choice"] = choice
		}
		b, _ := json.MarshalIndent(payload, "", "  ")
		fmt.Println(string(b))
		return pickErr
	}

	// Roomiest first, so the candidates are the first thing read. A services box
	// with a ZFS pool enumerates 39 filesystems (measured); an unbounded table
	// buries the answer, so the human view shows the top rows plus the chosen one
	// and says how many it withheld. --json and --all always carry everything.
	shown := append([]volumes.Volume(nil), vols...)
	sort.SliceStable(shown, func(i, j int) bool { return shown[i].FreeBytes > shown[j].FreeBytes })
	withheld := 0
	if !*all && len(shown) > humanTableRows {
		kept := shown[:humanTableRows]
		if pickErr == nil && !containsRoot(kept, choice.Volume.Root) {
			kept = append(kept, choice.Volume)
		}
		withheld = len(shown) - len(kept)
		shown = kept
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ROOT\tFS\tTOTAL\tFREE\tNOTE")
	for _, v := range shown {
		note := ""
		switch {
		case v.IsOS:
			note = "OS volume"
		case v.Removable:
			note = "removable — never an install target"
		case v.Network:
			note = "network share — never an install target"
		}
		if pickErr == nil && v.Root == choice.Volume.Root {
			note = strings.TrimSpace(note + "  <== install target")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", v.Root, dash(v.FS),
			volumes.Human(v.TotalBytes), volumes.Human(v.FreeBytes), note)
	}
	_ = w.Flush()
	if withheld > 0 {
		fmt.Printf("… and %d more filesystems with less free space (--all to list them, --json for the full data)\n", withheld)
	}

	if pickErr != nil {
		fmt.Fprintf(os.Stdout, "\ninstall target: NONE — %v\n", pickErr)
		return pickErr
	}
	fmt.Fprintf(os.Stdout, "\ninstall target: %s\n  because: %s\n", choice.Volume.Root, choice.Because)
	return nil
}

// humanTableRows bounds the console table; --all and --json are unbounded.
const humanTableRows = 8

func containsRoot(vs []volumes.Volume, root string) bool {
	for _, v := range vs {
		if v.Root == root {
			return true
		}
	}
	return false
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// runInstallSeed resolves ONE hardware tier's media/config seed for a target machine.
// The seeds lived only inside install.ps1, so a tier's bindings were Windows-shaped
// (`sd-cli.exe` in the table) and unreachable from a Linux install. A tier is a
// HARDWARE class: --os lets a Windows box render a Linux node's fragment, and the
// resolver validates the seed rather than shipping a typo to every machine of that
// class.
func runInstallSeed(args []string) error {
	fs := flag.NewFlagSet("install seed", flag.ExitOnError)
	profile := fs.String("profile", "", "tier id (see docs/tiers/README.md)")
	home := fs.String("home", "", "install root substituted for __OFFLOAD_HOME__")
	goos := fs.String("os", "", "target OS for binary names: windows|linux (default: this machine)")
	ramTier := fs.String("ram-tier", "", "apply the config_seed_ram_mid_high overlay: mid|high")
	root := fs.String("root", ".", "repo root holding setup/templates/profiles.json")
	_ = fs.Parse(args)
	if *profile == "" {
		return fmt.Errorf("install seed needs --profile <tier id>")
	}
	profiles, err := tierseed.Load(*root)
	if err != nil {
		return err
	}
	p, ok := profiles[*profile]
	if !ok {
		names := make([]string, 0, len(profiles))
		for n := range profiles {
			names = append(names, n)
		}
		sort.Strings(names)
		return fmt.Errorf("unknown tier %q (have: %s)", *profile, strings.Join(names, ", "))
	}
	seed, err := tierseed.Resolve(p, *profile, tierseed.Options{Home: *home, GOOS: *goos, RAMTier: *ramTier})
	if err != nil {
		return err
	}
	if seed == nil {
		fmt.Println("tier", *profile, "ships no media configuration — it serves text only until an operator binds media by hand")
		return nil
	}
	b, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
