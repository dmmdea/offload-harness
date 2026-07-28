package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/hwdetect"
	"github.com/dmmdea/offload-harness/internal/mediaseat"
	"github.com/dmmdea/offload-harness/internal/servingtmpl"
)

// The serving templates are EMBEDDED so a fetched binary can render a config on a
// machine that has no checkout. That is the whole shape of a real install: fetch one
// binary, ask it what this machine is, let it write the serving config.
//
//go:embed setup/templates/llama-swap.*.yaml
var servingTemplates embed.FS

// The tier table is embedded for the same reason as the templates: an install
// begins by fetching ONE binary onto a machine with no checkout. Reading it from
// a --root path is the development case, not the install case — and when the
// lookup silently failed, install.sh produced a config with no media bindings at
// all and said nothing, which is the failure this whole workstream exists to end.
//
//go:embed setup/templates/profiles.json
var embeddedProfiles []byte

// profilesJSON returns the tier table: the checkout's copy when --root names one
// that has it, else the embedded copy. A --root that was given explicitly and does
// NOT have it is an error, never a silent fallback.
func profilesJSON(root string) ([]byte, error) {
	if root == "" {
		return embeddedProfiles, nil
	}
	b, err := os.ReadFile(filepath.Join(root, "setup", "templates", "profiles.json"))
	if err != nil {
		if os.IsNotExist(err) && root == "." {
			return embeddedProfiles, nil // default root, no checkout: use the built-in table
		}
		return nil, fmt.Errorf("reading the tier table from --root %q: %w", root, err)
	}
	return b, nil
}

// servingProfile is the slice of a profiles.json entry that decides how a tier
// SERVES. (The media half lives in internal/tierseed.)
type servingProfile struct {
	CtxSize    int    `json:"ctx_size"`
	KVType     string `json:"kv_type"`
	FlashAttn  string `json:"flash_attn"`
	Backend    string `json:"backend"`
	Include26B bool   `json:"include_26b"`
	MoE26B     string `json:"moe_26b"`
	// MediaSeats are rendered into the models map and the group their residency
	// role maps to. The same declaration produces the harness config binding via
	// internal/tierseed, so the seat and the alias routing to it cannot disagree.
	MediaSeats []mediaseat.Seat `json:"media_seats"`
}

// moeFlag turns the tier's declared 26B PLACEMENT into the llama.cpp flag form the
// template carries. The distinction is load-bearing on small cards: "cpu_moe" keeps
// every expert in RAM, "gpu" offloads them, and getting it backwards either OOMs the
// card or wastes it.
func moeFlag(placement string) string {
	switch placement {
	case "gpu":
		return "-ngl 99"
	case "cpu_moe":
		return "--cpu-moe"
	default:
		return ""
	}
}

// templateFor picks the serving template for an OS + backend pair, and says exactly
// what exists when there is no match — a wrong template is worse than none.
func templateFor(goos, backend string) (string, error) {
	name := fmt.Sprintf("setup/templates/llama-swap.%s-%s.yaml", osTag(goos), backend)
	b, err := servingTemplates.ReadFile(name)
	if err == nil {
		return string(b), nil
	}
	entries, _ := servingTemplates.ReadDir("setup/templates")
	var have []string
	for _, e := range entries {
		have = append(have, strings.TrimSuffix(strings.TrimPrefix(e.Name(), "llama-swap."), ".yaml"))
	}
	sort.Strings(have)
	return "", fmt.Errorf("no serving template for %s/%s (have: %s)", osTag(goos), backend, strings.Join(have, ", "))
}

// seatsPlaceable reports whether the serving template for this target can host
// tier-declared media seats, so `install seed` can refuse exactly the pairs
// `install render` refuses instead of writing a binding the node cannot honour.
func seatsPlaceable(goos, backend string) error {
	target := goos
	if target == "" {
		target = runtime.GOOS
	}
	tmpl, err := templateFor(target, backend)
	if err != nil {
		return err
	}
	if !servingtmpl.SupportsSeats(tmpl) {
		return fmt.Errorf("this tier declares media seats, but the %s/%s serving template carries no "+
			"`# offload-seats:` directive, so they cannot be placed. Seeding their bindings here would write a "+
			"config naming aliases this node will never serve — refusing both halves instead", osTag(target), backend)
	}
	return nil
}

func osTag(goos string) string {
	if goos == "windows" {
		return "win"
	}
	return goos
}

func runInstallRender(args []string) error {
	fs := flag.NewFlagSet("install render", flag.ExitOnError)
	profileID := fs.String("profile", "", "tier id (default: classify this machine)")
	llamaBin := fs.String("llama-bin", "", "directory holding llama-server and its shared objects")
	modelsDir := fs.String("models", "", "directory holding the GGUF model files")
	listen := fs.String("listen", "127.0.0.1:11436", "llama-swap listen address")
	threads := fs.Int("threads", 0, "--threads per server (default: half the logical CPUs)")
	goos := fs.String("os", "", "target OS: windows|linux (default: this machine)")
	out := fs.String("out", "", "write the rendered config here instead of stdout")
	root := fs.String("root", ".", "repo root holding setup/templates/profiles.json")
	home := fs.String("home", "", "install root, for media seat paths (__OFFLOAD_HOME__)")
	_ = fs.Parse(args)

	target := *goos
	if target == "" {
		target = runtime.GOOS
	}
	id := *profileID
	if id == "" {
		id = hwdetect.Classify(hwdetect.Detect()).Profile
	}

	raw, err := profilesJSON(*root)
	if err != nil {
		return err
	}
	var doc struct {
		Profiles map[string]servingProfile `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("profiles.json: %w", err)
	}
	p, ok := doc.Profiles[id]
	if !ok {
		return fmt.Errorf("unknown tier %q", id)
	}

	tmpl, err := templateFor(target, p.Backend)
	if err != nil {
		return err
	}

	n := *threads
	if n <= 0 {
		n = runtime.NumCPU() / 2
		if n < 1 {
			n = 1
		}
	}
	rendered, err := servingtmpl.Render(tmpl, servingtmpl.Params{
		LlamaBin: *llamaBin, ModelsDir: *modelsDir, Listen: *listen,
		Ctx: p.CtxSize, KVType: p.KVType, FlashAttn: p.FlashAttn,
		MoE26B: moeFlag(p.MoE26B), Threads: n, Include26B: p.Include26B && p.MoE26B != "drop",
		Seats: p.MediaSeats, Home: *home, GOOS: target,
	})
	if err != nil {
		return fmt.Errorf("tier %s: %w", id, err)
	}

	if *out == "" {
		fmt.Print(rendered)
		return nil
	}
	if err := os.WriteFile(*out, []byte(rendered), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (tier %s, %s/%s)\n", *out, id, osTag(target), p.Backend)
	return nil
}
