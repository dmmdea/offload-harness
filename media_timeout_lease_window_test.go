package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// The lease window a generation call runs under IS its own timeout: the pipeline hands acquireMediaLease the route's
// timeout as the lease's declared window and kills the render at the same value (pinned per route in internal/pipeline,
// TestEveryMediaRouteLeasesForItsOwnTimeoutAndIsKilledThere). So a timeout can never outlive the window it holds, and the
// 3,600 s DefaultTTL of the lease library is only the fallback for a lease taken with no window at all.
//
// What a value CAN still do is ask for a window longer than one lease TERM. A window above gpu_max_term_min (6 h unless the
// config says otherwise) is accepted whole but reaches its declared end only by renewal, and a term that is not renewed is
// labelled expired while the render is still running (gpulease.PlanTerm, ADR 0070). These tests hold every SINGLE-JOB timeout
// the repository ships (the code defaults, the setup template, the example config and every tier seed layer, per-family keys
// included) to that rule, and the seeds that restate a default to the code's value.
//
// What they cannot hold is an image BATCH: its window is the per-image timeout times its jobs (pipeline.RunImageBatch), and
// no cap bounds the job count, so a batch longer than gpu_max_term_min / imagegen_timeout_sec images asks for a window above
// the term whatever the seed says. That stays a stated limit in the operator guide, not a number this file can pin.

// mediaTimeoutKeys are the seven generation routes' timeout keys and their code defaults. Each is a single job's timeout.
var mediaTimeoutKeys = map[string]func(config.Config) int{
	"imagegen_timeout_sec":   func(c config.Config) int { return c.ImageGenTimeoutSec },
	"videogen_timeout_sec":   func(c config.Config) int { return c.VideoGenTimeoutSec },
	"inpaint_timeout_sec":    func(c config.Config) int { return c.InpaintTimeoutSec },
	"upscale_timeout_sec":    func(c config.Config) int { return c.UpscaleTimeoutSec },
	"gen_edit_timeout_sec":   func(c config.Config) int { return c.GenEditTimeoutSec },
	"animategen_timeout_sec": func(c config.Config) int { return c.AnimateGenTimeoutSec },
	"audiogen_timeout_sec":   func(c config.Config) int { return c.AudioGenTimeoutSec },
}

// notMediaLeaseTimeouts are the other *_timeout_sec keys of config.Config, each a clock that never becomes a media lease
// window. A new *_timeout_sec key must land in mediaTimeoutKeys or here, so adding a generation route cannot slip past the
// seed test unnoticed.
var notMediaLeaseTimeouts = map[string]string{
	"agent_timeout_sec":            "the text agent loop's wall",
	"stt_request_timeout_sec":      "one speech-to-text request",
	"imagegen_refiner_timeout_sec": "the prompt refiner's text call",
	"edit_timeout_sec":             "CPU edit ops: no GPU lease",
	"compose_timeout_sec":          "composition renders in software: no GPU lease",
	"browse_timeout_sec":           "one browser fetch",
	"request_timeout_sec":          "one chat request",
	"nim_timeout_sec":              "a remote hosted call",
	"hailo_timeout_sec":            "an accelerator call",
	"coral_timeout_sec":            "an accelerator call",
	"rknpu_timeout_sec":            "an accelerator call",
}

// configTimeoutTags returns every top-level *_timeout_sec JSON key config.Config declares.
func configTimeoutTags() []string {
	var tags []string
	rt := reflect.TypeOf(config.Config{})
	for i := 0; i < rt.NumField(); i++ {
		name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if strings.HasSuffix(name, "_timeout_sec") {
			tags = append(tags, name)
		}
	}
	sort.Strings(tags)
	return tags
}

// mediaTimeoutKeyDrift compares the config's *_timeout_sec keys with the key list: a media key the config no longer declares,
// and a config key that is in neither list.
func mediaTimeoutKeyDrift(tags []string) []string {
	return nil // red: not written yet
}

func sortedMediaKeys() []string {
	keys := make([]string, 0, len(mediaTimeoutKeys))
	for k := range mediaTimeoutKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// seededTimeouts walks a seed layer for media timeout keys at any depth (a family block carries its own) and returns
// path -> seconds. A layer that leaves a top-level route unset runs the code default, so that default is reported too.
func seededTimeouts(layer map[string]any) map[string]float64 {
	out := map[string]float64{}
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			path := prefix + k
			switch x := v.(type) {
			case map[string]any:
				walk(path+".", x)
			case float64:
				if _, ok := mediaTimeoutKeys[k]; ok && x > 0 {
					out[path] = x
				}
			}
		}
	}
	walk("", layer)
	def := config.Default()
	for key, get := range mediaTimeoutKeys {
		if v, ok := layer[key].(float64); !ok || v <= 0 {
			out[key+" (code default)"] = float64(get(def))
		}
	}
	return out
}

// leaseWindowProblems judges one layer. The window a call takes is its timeout; the lease plan for that window must be
// one whole first term under the layer's own gpu_max_term_min (else the 6 h default).
func leaseWindowProblems(where string, layer map[string]any) (explicit int, problems []string) {
	termCap := config.Default().GPUMaxTerm()
	if m, ok := layer["gpu_max_term_min"].(float64); ok && m > 0 {
		termCap = time.Duration(m) * time.Minute
	}
	timeouts := seededTimeouts(layer)
	paths := make([]string, 0, len(timeouts))
	for p := range timeouts {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		sec := timeouts[p]
		if !strings.HasSuffix(p, "(code default)") {
			explicit++
		}
		window := time.Duration(sec * float64(time.Second))
		plan := gpulease.PlanTerm(window, false, termCap, 0)
		if plan.Requested != 0 || plan.Term < window {
			problems = append(problems, fmt.Sprintf("%s: %s = %.0f s is a lease window above the %s term cap (gpu_max_term_min): it would reach its end only by renewal, and a term that is not renewed is labelled expired mid-render", where, p, sec, termCap))
		}
	}
	// gpu_wait_ms is the queue wait, not a window; a seeded one must not trip the doctor's 3x vision-wait finding.
	if ms, ok := layer["gpu_wait_ms"].(float64); ok {
		cfg := config.Default()
		cfg.GPUWaitMs = int(ms)
		if v, ok := layer["vision_gpu_wait_sec"].(float64); ok {
			cfg.VisionGPUWaitSec = int(v)
		}
		for _, f := range cfg.Findings() {
			if strings.Contains(f, "gpu_wait_ms") {
				problems = append(problems, where+": "+f)
			}
		}
	}
	return explicit, problems
}

func readJSONMap(t *testing.T, rel string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s is not valid JSON: %v", rel, err)
	}
	return m
}

// tierSeedLayers returns, per tier, what a box of that tier receives: the base config_seed alone, and each conditional
// overlay (config_seed_ram_*) merged over it, as tierseed.Resolve does.
func tierSeedLayers(t *testing.T) map[string]map[string]any {
	t.Helper()
	doc := readJSONMap(t, "setup/templates/profiles.json")
	profiles, _ := doc["profiles"].(map[string]any)
	if len(profiles) == 0 {
		t.Fatal("profiles.json has no profiles: every gate below went blind")
	}
	out := map[string]map[string]any{}
	for tier, raw := range profiles {
		p, _ := raw.(map[string]any)
		base, _ := p["config_seed"].(map[string]any)
		if base != nil {
			out[tier] = base
		}
		for key, v := range p {
			overlay, _ := v.(map[string]any)
			if !strings.HasPrefix(key, "config_seed_") || overlay == nil {
				continue
			}
			merged := map[string]any{}
			for k, bv := range base {
				merged[k] = bv
			}
			for k, ov := range overlay {
				merged[k] = ov
			}
			out[tier+"/"+key] = merged
		}
	}
	return out
}

// TestTheMediaTimeoutKeyListMatchesTheConfig: the key list the seed test walks is the config's own set of generation
// timeouts. The two controls are what prove it can fail: a new route key nobody classified, and a listed key the config
// dropped.
func TestTheMediaTimeoutKeyListMatchesTheConfig(t *testing.T) {
	tags := configTimeoutTags()
	if len(tags) < len(mediaTimeoutKeys) {
		t.Fatalf("only %d *_timeout_sec keys found on config.Config: the reflection went blind", len(tags))
	}
	if drift := mediaTimeoutKeyDrift(tags); len(drift) != 0 {
		t.Errorf("the media timeout key list and config.Config drifted: %v", drift)
	}
	if d := mediaTimeoutKeyDrift(append(append([]string{}, tags...), "newroute_timeout_sec")); len(d) != 1 || !strings.Contains(d[0], "newroute_timeout_sec") {
		t.Errorf("a *_timeout_sec key in neither list must be reported once, got %v", d)
	}
	var without []string
	for _, tag := range tags {
		if tag != "videogen_timeout_sec" {
			without = append(without, tag)
		}
	}
	if d := mediaTimeoutKeyDrift(without); len(d) != 1 || !strings.Contains(d[0], "videogen_timeout_sec") {
		t.Errorf("a listed media key the config no longer declares must be reported once, got %v", d)
	}
}

// TestSeededMediaTimeoutsFitTheLeaseWindowTheyRunUnder holds every shipped single-job media timeout to one lease term. It is a guard,
// green on the tree as it stands (the longest seed is 7,200 s against a 21,600 s term): the two controls below are what
// prove it can fail.
func TestSeededMediaTimeoutsFitTheLeaseWindowTheyRunUnder(t *testing.T) {
	layers := map[string]map[string]any{
		"code defaults":               {},
		"config.example.json":         readJSONMap(t, "config.example.json"),
		"setup/templates/config.json": readJSONMap(t, "setup/templates/config.json"),
	}
	for tier, layer := range tierSeedLayers(t) {
		layers["profiles.json "+tier] = layer
	}

	explicit, seen := 0, map[string]bool{}
	names := make([]string, 0, len(layers))
	for n := range layers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		n, problems := leaseWindowProblems(name, layers[name])
		explicit += n
		for _, p := range problems {
			t.Error(p)
		}
		for path := range seededTimeouts(layers[name]) {
			path = strings.TrimSuffix(path, " (code default)")
			seen[path[strings.LastIndex(path, ".")+1:]] = true
		}
	}

	// Blind-gate control: the walk found the seeds it is meant to judge.
	if explicit < 20 {
		t.Fatalf("only %d explicit media timeouts found across the seed files: the walk went blind", explicit)
	}
	for _, key := range sortedMediaKeys() {
		if !seen[key] {
			t.Errorf("no layer carries %s (explicit or default): the key list and the config drifted", key)
		}
	}

	// Negative controls: the predicate fails for a video seed that outlives a one-hour term, and for a window above the
	// default term; and it stays quiet for the same seed under the default term.
	_, problems := leaseWindowProblems("control", map[string]any{"videogen_timeout_sec": float64(5400), "gpu_max_term_min": float64(60)})
	if len(problems) != 1 || !strings.Contains(problems[0], "videogen_timeout_sec") {
		t.Errorf("a 5,400 s video seed under a 60 min term must be refused once, got %v", problems)
	}
	if _, problems := leaseWindowProblems("control", map[string]any{"imagegen_timeout_sec": float64(7 * 3600)}); len(problems) != 1 {
		t.Errorf("a 7 h image seed must be refused under the 6 h default term, got %v", problems)
	}
	if _, problems := leaseWindowProblems("control", map[string]any{"videogen_timeout_sec": float64(5400)}); len(problems) != 0 {
		t.Errorf("a 5,400 s video seed fits the default term, got %v", problems)
	}
	if _, problems := leaseWindowProblems("control", map[string]any{"gpu_wait_ms": float64(600000)}); len(problems) != 1 {
		t.Errorf("a 600,000 ms gpu_wait_ms against the 90 s vision wait must be refused once, got %v", problems)
	}
}

// TestTheSetupTemplateRestatesTheCodeDefaultsOrNothing: the keys the setup template's config writes for a fresh install
// are the code's own, so the template and the code cannot name two answers for one clock. (config.example.json is held to
// the whole of config.Default() by TestConfigExampleRoundTrips.)
func TestTheSetupTemplateRestatesTheCodeDefaultsOrNothing(t *testing.T) {
	tpl := readJSONMap(t, "setup/templates/config.json")
	def := config.Default()
	checked := 0
	for key, get := range mediaTimeoutKeys {
		v, ok := tpl[key].(float64)
		if !ok {
			continue
		}
		checked++
		if int(v) != get(def) {
			t.Errorf("setup/templates/config.json %s = %.0f, config.Default() = %d: a fresh install would run a different clock than the code says", key, v, get(def))
		}
	}
	for key, want := range map[string]int{"gpu_wait_ms": def.GPUWaitMs, "vision_gpu_wait_sec": def.VisionGPUWaitSec} {
		if v, ok := tpl[key].(float64); ok {
			checked++
			if int(v) != want {
				t.Errorf("setup/templates/config.json %s = %.0f, config.Default() = %d", key, v, want)
			}
		}
	}
	if checked < 4 {
		t.Fatalf("only %d template clocks compared: the gate went blind", checked)
	}
}

// TestTheDocsStateWhereAMediaCallsLeaseWindowComesFrom: the operator guide's engine + lease row and the fleet-node page
// both tell a reader what the lease window of a generation call is, and the node page says it for a fleet media job
// (whose timeout keys are the node's own, never the delegator's).
var registerAnchor = regexp.MustCompile(`#[a-z0-9-]*register-[a-z]-[0-9]+`)

func TestTheDocsStateWhereAMediaCallsLeaseWindowComesFrom(t *testing.T) {
	guide, err := os.ReadFile(filepath.FromSlash("docs/OPERATOR-GUIDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	var row string
	for _, line := range strings.Split(string(guide), "\n") {
		if strings.HasPrefix(line, "| engine + lease |") {
			row = line
		}
	}
	if row == "" {
		t.Fatal("docs/OPERATOR-GUIDE.md has no engine + lease row: the gate went blind")
	}
	if !strings.Contains(row, "`gpu_max_term_min`") {
		t.Errorf("the guide's engine + lease row does not name gpu_max_term_min: a reader cannot see that a seeded timeout must fit one lease term")
	}
	// The term bound is a single job's timeout. A batch's window is the per-image timeout times its jobs and no cap bounds the
	// job count, so the row must name the batch as the case the bound does not hold for.
	for _, w := range []string{"single job's timeout", "per-image timeout times its jobs", "job count"} {
		if !strings.Contains(row, w) {
			t.Errorf("the guide's engine + lease row does not say %q: it would let a reader take the term bound for a batch's whole window", w)
		}
	}

	node, err := os.ReadFile(filepath.FromSlash("docs/FLEET-NODE.md"))
	if err != nil {
		t.Fatal(err)
	}
	def := config.Default()
	wait := fmt.Sprintf("%d s", def.GPUWaitMs/1000)
	want := append([]string{"lease", "`gpu_max_term_min`", "`gpu_wait_ms`", wait, "OPERATOR-GUIDE.md", "batch"}, func() []string {
		keys := sortedMediaKeys()
		for i, k := range keys {
			keys[i] = "`" + k + "`"
		}
		return keys
	}()...)
	best, missingBest := "", len(want)+1
	for _, para := range strings.Split(strings.ReplaceAll(string(node), "\r\n", "\n"), "\n\n") {
		var missing []string
		for _, w := range want {
			if !strings.Contains(para, w) {
				missing = append(missing, w)
			}
		}
		if len(missing) < missingBest {
			best, missingBest = para, len(missing)
			if len(missing) == 0 {
				if slug := registerAnchor.FindString(para); slug != "" {
					t.Errorf("docs/FLEET-NODE.md links the guide through an anchor that carries a plan id (%s): name the section in prose", slug)
				}
				return
			}
		}
	}
	_ = best
	t.Errorf("docs/FLEET-NODE.md has no paragraph that states a fleet media job's lease window is its route's own timeout key: the closest paragraph is missing %d of %v", missingBest, want)
}
