package update

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Component names the verb takes.
const (
	Llama       = "llama"
	LlamaSwap   = "llama-swap"
	Whisper     = "whisper"
	SDCpp       = "sdcpp"
	Hyperframes = "hyperframes"
)

// AllComponents is the order `--component all` walks: the engines first, the daemon that hosts them last.
var AllComponents = []string{Llama, Whisper, SDCpp, Hyperframes, LlamaSwap}

type tagRule int

const (
	ruleB tagRule = iota
	ruleV
	ruleMaster
	ruleNPM
)

// compDef is everything fixed about a component. No version, tag, digest or build number lives here.
type compDef struct {
	Name     string
	Upstream string // the key in floors.json, "" = no floor
	Repo     string // GitHub owner/repo, or the npm package for ruleNPM
	Rule     tagRule
	Single   bool   // the live path is one file (llama-swap), swapped through nodeswap
	Binary   string // the program the offline self-check runs (no .exe)
	Probes   [][]string
	IDProbe  *regexp.Regexp // pulls the upstream id out of the probe's output
	IDFormat string         // sprintf of submatch 1
}

var defs = map[string]*compDef{
	Llama: {Name: Llama, Upstream: "llama.cpp", Repo: "ggml-org/llama.cpp", Rule: ruleB, Binary: "llama-server",
		Probes: [][]string{{"--version"}}, IDProbe: regexp.MustCompile(`build (\d+)`), IDFormat: "b%s"},
	LlamaSwap: {Name: LlamaSwap, Repo: "mostlygeek/llama-swap", Rule: ruleV, Single: true, Binary: "llama-swap",
		Probes: [][]string{{"-version"}, {"--version"}}, IDProbe: regexp.MustCompile(`version:\s*v?(\d+)`), IDFormat: "v%s"},
	Whisper: {Name: Whisper, Upstream: "whisper.cpp", Repo: "ggml-org/whisper.cpp", Rule: ruleB, Binary: "whisper-server",
		Probes: [][]string{{"--version"}, {"-h"}}},
	SDCpp: {Name: SDCpp, Upstream: "stable-diffusion.cpp", Repo: "leejet/stable-diffusion.cpp", Rule: ruleMaster, Binary: "sd-cli",
		Probes: [][]string{{"--version"}, {"-h"}}, IDProbe: regexp.MustCompile(`(master-\d+-[0-9a-f]{7})`), IDFormat: "%s"},
	Hyperframes: {Name: Hyperframes, Upstream: "hyperframes", Repo: "hyperframes", Rule: ruleNPM},
}

var tagRegex = map[tagRule]*regexp.Regexp{
	ruleB:      regexp.MustCompile(`^b(\d+)$`),
	ruleV:      regexp.MustCompile(`^v(\d+)$`),
	ruleMaster: regexp.MustCompile(`^master-(\d+)-([0-9a-f]{7,})$`),
}

// liveName is the default live path under <home>.
func (d *compDef) liveRel() string {
	if d.Single {
		return filepath.Join(d.Name, d.exeName())
	}
	return d.Name
}

func (d *compDef) exeName() string {
	if goosWindows() {
		return d.Binary + ".exe"
	}
	return d.Binary
}

// versionsRoot is where a live path keeps its generations: "<live>-versions" for a directory component,
// "<dir>/<name>-versions" beside the file for a single-file one.
func versionsRoot(live string, single bool) string {
	if single {
		base := filepath.Base(live)
		base = strings.TrimSuffix(base, filepath.Ext(base))
		return filepath.Join(filepath.Dir(live), base+"-versions")
	}
	return live + "-versions"
}
