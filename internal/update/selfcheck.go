package update

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/winexec"
	"gopkg.in/yaml.v3"
)

// binaryIn finds the component's program under dir ("" when absent).
func binaryIn(def *compDef, dir string) string {
	p := filepath.Join(dir, def.exeName())
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p
	}
	return findFile(dir, def.exeName())
}

func runIn(ctx context.Context, bin string, args ...string) (string, int, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := winexec.CommandContext(cctx, bin, args...)
	cmd.Dir = filepath.Dir(bin)
	if runtime.GOOS != "windows" {
		cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("LD_LIBRARY_PATH"))
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		return string(out), -1, err
	}
	return string(out), code, nil
}

// Probe runs the component's version probe under dir and returns the upstream id it prints ("" when the program
// prints none) and whether any probe exited 0. Hyperframes reads its installed package.json instead.
func Probe(ctx context.Context, def *compDef, dir string) (id string, ok bool, detail string) {
	if def.Rule == ruleNPM {
		b, err := os.ReadFile(filepath.Join(dir, "node_modules", def.Repo, "package.json"))
		if err != nil {
			return "", false, "no installed " + def.Repo + " package under " + dir
		}
		var v struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(b, &v) != nil || v.Version == "" {
			return "", false, "installed package.json has no version"
		}
		return v.Version, true, "package.json " + v.Version
	}
	bin := binaryIn(def, dir)
	if bin == "" {
		return "", false, def.exeName() + " not found under " + dir
	}
	detail = ""
	for _, args := range def.Probes {
		out, code, err := runIn(ctx, bin, args...)
		if err != nil {
			detail = err.Error()
			continue
		}
		if code != 0 {
			detail = fmt.Sprintf("%s %s exited %d", filepath.Base(bin), strings.Join(args, " "), code)
			continue
		}
		if def.IDProbe != nil {
			if m := def.IDProbe.FindStringSubmatch(out); m != nil {
				sub := m[len(m)-1]
				if strings.Contains(def.IDFormat, "%s") {
					return fmt.Sprintf(def.IDFormat, sub), true, truncate(strings.TrimSpace(out), 80)
				}
			}
		}
		return "", true, truncate(strings.TrimSpace(firstLine(out)), 80)
	}
	return "", false, detail
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// SelfCheck is the offline check every staged build passes before anything moves: the program starts (--version
// exits 0) and, for llama, every long flag the rendered llama-swap yaml passes to this binary exists in the staged
// `llama-server --help`. It returns notes for the table and an error that rejects the candidate.
func SelfCheck(ctx context.Context, def *compDef, dir, yamlPath, live string) ([]string, error) {
	_, ok, detail := Probe(ctx, def, dir)
	if !ok {
		return nil, fmt.Errorf("self-check: the staged %s does not start: %s", def.Name, detail)
	}
	notes := []string{"starts: " + detail}
	if def.Name == Llama {
		n, err := CheckFlags(ctx, binaryIn(def, dir), yamlPath, live)
		if err != nil {
			return notes, err
		}
		notes = append(notes, n)
	}
	return notes, nil
}

var longFlag = regexp.MustCompile(`--[A-Za-z0-9][A-Za-z0-9-]*`)
var optNo = regexp.MustCompile(`--\[no-\]([A-Za-z0-9-]+)`)

type swapYAML struct {
	Macros map[string]any `yaml:"macros"`
	Models map[string]struct {
		Cmd string   `yaml:"cmd"`
		Env []string `yaml:"env"`
		TTL *int     `yaml:"ttl"`
	} `yaml:"models"`
}

// CheckFlags compares the long flags of every cmd line that runs the live llama-server with the staged help text.
func CheckFlags(ctx context.Context, stagedServer, yamlPath, live string) (string, error) {
	if yamlPath == "" {
		return "flags: no llama-swap yaml configured; not checked", nil
	}
	raw, err := os.ReadFile(yamlPath)
	if err != nil {
		return "flags: " + filepath.Base(yamlPath) + " not readable; not checked", nil
	}
	var cfg swapYAML
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("self-check: %s does not parse as yaml: %w", filepath.Base(yamlPath), err)
	}
	liveNorm := normPath(live)
	flagModels := map[string][]string{}
	checked := 0
	for name, m := range cfg.Models {
		cmd := expandMacros(m.Cmd, cfg.Macros)
		low := normPath(cmd)
		if !strings.Contains(low, "llama-server") || !strings.Contains(low, liveNorm+"/") {
			continue
		}
		checked++
		for _, tok := range strings.Fields(cmd) {
			if !strings.HasPrefix(tok, "--") {
				continue
			}
			f := longFlag.FindString(tok)
			if f == "" || len(f) != len(strings.SplitN(tok, "=", 2)[0]) {
				continue
			}
			flagModels[f] = appendOnce(flagModels[f], name)
		}
	}
	if checked == 0 {
		return "flags: no cmd line of " + filepath.Base(yamlPath) + " runs " + live + "; not checked", nil
	}
	out, _, rerr := runIn(ctx, stagedServer, "--help")
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("self-check: the staged llama-server printed no --help text (%v)", rerr)
	}
	have := map[string]bool{}
	for _, f := range longFlag.FindAllString(out, -1) {
		have[f] = true
	}
	for _, m := range optNo.FindAllStringSubmatch(out, -1) {
		have["--"+m[1]] = true
		have["--no-"+m[1]] = true
	}
	var unknown []string
	for f, models := range flagModels {
		if !have[f] {
			sort.Strings(models)
			unknown = append(unknown, fmt.Sprintf("%s (%s)", f, strings.Join(models, ", ")))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return "", fmt.Errorf("self-check: the staged llama-server does not know flag(s) the rendered yaml passes it: %s", strings.Join(unknown, "; "))
	}
	return fmt.Sprintf("flags: %d flag(s) of %d seat cmd line(s) all known to the staged --help", len(flagModels), checked), nil
}

func appendOnce(l []string, v string) []string {
	for _, e := range l {
		if e == v {
			return l
		}
	}
	return append(l, v)
}

var macroRef = regexp.MustCompile(`\$\{([A-Za-z0-9_.-]+)\}`)

func expandMacros(s string, macros map[string]any) string {
	for i := 0; i < 6; i++ {
		next := macroRef.ReplaceAllStringFunc(s, func(m string) string {
			name := m[2 : len(m)-1]
			if v, ok := macros[name]; ok {
				return fmt.Sprint(v)
			}
			return m
		})
		if next == s {
			break
		}
		s = next
	}
	return s
}

func normPath(p string) string {
	return strings.ToLower(strings.TrimRight(strings.ReplaceAll(p, `\`, "/"), "/"))
}
