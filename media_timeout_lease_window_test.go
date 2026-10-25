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

	"github.com/dmmdea/offload-harness/internal/config"
)

// The lease window a generation call runs under IS its own timeout: the pipeline hands acquireMediaLease the route's
// timeout as the lease's declared window (a batch: the per-image timeout times its jobs) and kills the render at the same
// value (pinned per route in internal/pipeline, TestEveryMediaRouteLeasesForItsOwnTimeoutAndIsKilledThere). So a timeout can
// never outlive the window it holds, and the 3,600 s DefaultTTL of the lease library is only the fallback for a lease taken
// with no window at all.
//
// The window is granted whole, whatever its length. gpu_max_term_min is not a bound on it: that cap is the renewal term of a
// `gpu reserve` (or `gpu hold`) lease, and only that holder's own term check reads it. A pipeline media lease runs no term
// check, so nothing labels it expired or asks it to yield while the render is inside its window; its 15 s heartbeat keeps
// it, and past the window the same timeout is the render's own deadline. What a long timeout costs is how long a WEDGED
// render holds the machine-wide lease. So nothing here bounds a seeded timeout by a term; these tests hold what is true and
// checkable: the key list tracks the config, the template restates the code's own clocks, a seeded gpu_wait_ms passes the
// doctor's vision-wait rule, and the two docs say where a media call's window comes from and what holds it.

// mediaTimeoutKeys are the seven generation routes' timeout keys and their code defaults. Each is the timeout a route
// leases for and is killed at.
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
// docs and template checks unnoticed.
var notMediaLeaseTimeouts = map[string]string{
	"agent_timeout_sec":            "the text agent loop's wall",
	"stt_request_timeout_sec":      "one speech-to-text request",
	"forecast_request_timeout_sec": "one forecast request",
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
	declared := map[string]bool{}
	var out []string
	for _, tag := range tags {
		declared[tag] = true
		_, media := mediaTimeoutKeys[tag]
		_, other := notMediaLeaseTimeouts[tag]
		if !media && !other {
			out = append(out, tag+" is a config.Config *_timeout_sec key in neither mediaTimeoutKeys nor notMediaLeaseTimeouts: if it is a generation route's clock, add it to mediaTimeoutKeys so the docs and template checks cover it")
		}
	}
	for _, key := range sortedMediaKeys() {
		if !declared[key] {
			out = append(out, key+" is in mediaTimeoutKeys but config.Config no longer declares it")
		}
	}
	others := make([]string, 0, len(notMediaLeaseTimeouts))
	for key := range notMediaLeaseTimeouts {
		others = append(others, key)
	}
	sort.Strings(others)
	for _, key := range others {
		if !declared[key] {
			out = append(out, key+" is in notMediaLeaseTimeouts but config.Config no longer declares it")
		}
	}
	return out
}

func sortedMediaKeys() []string {
	keys := make([]string, 0, len(mediaTimeoutKeys))
	for k := range mediaTimeoutKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// gpuWaitProblems judges one layer's gpu_wait_ms against the doctor's rule (more than 3x vision_gpu_wait_sec). gpu_wait_ms is
// the queue wait, not a window, so this is the one clock of a seed layer that has a rule of its own. seen is false when the
// layer seeds no gpu_wait_ms.
func gpuWaitProblems(where string, layer map[string]any) (seen bool, problems []string) {
	ms, ok := layer["gpu_wait_ms"].(float64)
	if !ok {
		return false, nil
	}
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
	return true, problems
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

// TestTheMediaTimeoutKeyListMatchesTheConfig: the key list the docs and template checks walk is the config's own set of
// generation timeouts. The two controls are what prove it can fail: a new route key nobody classified, and a listed key the
// config dropped.
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

// TestSeededGPUWaitsPassTheDoctorsVisionWaitRule holds every shipped gpu_wait_ms (the example config, the setup template and,
// should a tier seed ever carry one, every tier seed layer) to the doctor's rule that a media queue wait is not more than 3x
// the vision wait. It is a guard, green on the tree as it stands: the controls below are what prove it can fail.
func TestSeededGPUWaitsPassTheDoctorsVisionWaitRule(t *testing.T) {
	layers := map[string]map[string]any{
		"config.example.json":         readJSONMap(t, "config.example.json"),
		"setup/templates/config.json": readJSONMap(t, "setup/templates/config.json"),
	}
	for tier, layer := range tierSeedLayers(t) {
		layers["profiles.json "+tier] = layer
	}

	names := make([]string, 0, len(layers))
	for n := range layers {
		names = append(names, n)
	}
	sort.Strings(names)
	seen := 0
	for _, name := range names {
		ok, problems := gpuWaitProblems(name, layers[name])
		if ok {
			seen++
		}
		for _, p := range problems {
			t.Error(p)
		}
	}
	// Blind-gate control: the example config and the template both seed one, so a walk that finds fewer went blind.
	if seen < 2 {
		t.Fatalf("only %d seeded gpu_wait_ms found across the seed files, want at least the example config and the template: the walk went blind", seen)
	}

	// Negative controls: the predicate fails for a 600,000 ms wait against the 90 s vision wait, and stays quiet for the code
	// default and for the same wait once the vision wait is raised to match.
	if _, problems := gpuWaitProblems("control", map[string]any{"gpu_wait_ms": float64(600000)}); len(problems) != 1 {
		t.Errorf("a 600,000 ms gpu_wait_ms against the 90 s vision wait must be refused once, got %v", problems)
	}
	if _, problems := gpuWaitProblems("control", map[string]any{"gpu_wait_ms": float64(config.Default().GPUWaitMs)}); len(problems) != 0 {
		t.Errorf("the code default gpu_wait_ms must pass, got %v", problems)
	}
	if _, problems := gpuWaitProblems("control", map[string]any{"gpu_wait_ms": float64(600000), "vision_gpu_wait_sec": float64(300)}); len(problems) != 0 {
		t.Errorf("a 600,000 ms gpu_wait_ms is within 3x a 300 s vision wait, got %v", problems)
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

// Where a media call's lease window comes from and what holds it, as the operator guide's engine + lease row and the fleet-node
// page state it. The node page says it for a fleet media job (whose timeout keys are the node's own, never the delegator's).
// The statements are the ones the code backs:
//
//   - the window is the route's own timeout (a batch: the per-image timeout times its jobs) and is granted whole;
//   - it is kept by the call's heartbeat, and no term check runs on it (gpu_max_term_min is the renewal term of a
//     `gpu reserve` hold, renewed by that hold's own holder, not a bound on a media call);
//   - gpu_wait_ms is a queue wait, not a lease length;
//   - a `gpu reserve --for` wrapper is sized from the key of the route it wraps.
//
// The claims of the retired rule (a media timeout must fit one lease term, because a window above a term "reaches its end only
// by renewal" and is "labelled expired while the render runs"; an image batch is "the case the bound does not cover") are
// refused by name, so they cannot come back.
var registerAnchor = regexp.MustCompile(`#[a-z0-9-]*register-[a-z]-[0-9]+`)

// retiredTermClaims are phrases of the retired rule that a seeded media timeout must fit one lease term.
var retiredTermClaims = []string{
	"single job's timeout",
	"must stay under it",
	"A window is also one lease term",
	"The window is one lease term",
	"asks for a window above the term",
	"the case that bound does not cover",
	"job count",
}

// flat collapses whitespace so a phrase that the source wraps across lines still matches.
func flat(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func TestTheDocsStateWhereAMediaCallsLeaseWindowComesFromAndWhatHoldsIt(t *testing.T) {
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
	row = flat(row)
	for _, w := range []struct{ phrase, why string }{
		{"the call's own timeout", "a media call's lease window is its own timeout"},
		{"per-image timeout times its jobs", "a batch's window is the per-image timeout times its jobs"},
		{"granted whole", "the window is granted whole, whatever its length"},
		{"kept by its heartbeat", "the lease is kept by the call's heartbeat, not renewed by a term check"},
		{"`gpu_max_term_min`", "the term cap is named, so a reader sees what it governs"},
		{"renewal term of a `gpu reserve` hold", "gpu_max_term_min governs a gpu reserve hold, not a media call"},
		{"queue wait", "gpu_wait_ms is the queue wait, not a lease length"},
		{"not a lease length", "gpu_wait_ms is the queue wait, not a lease length"},
		{"`--for`", "a gpu reserve wrapper is sized with --for"},
		{"same key as the route it wraps", "--for is sized from the key of the route the wrapper wraps"},
	} {
		if !strings.Contains(row, w.phrase) {
			t.Errorf("the guide's engine + lease row does not say %q (%s)", w.phrase, w.why)
		}
	}
	for _, retired := range retiredTermClaims {
		if strings.Contains(row, retired) {
			t.Errorf("the guide's engine + lease row still says %q, a claim of the retired rule that a media timeout must fit one lease term", retired)
		}
	}

	node, err := os.ReadFile(filepath.FromSlash("docs/FLEET-NODE.md"))
	if err != nil {
		t.Fatal(err)
	}
	def := config.Default()
	wait := fmt.Sprintf("%d s", def.GPUWaitMs/1000)
	want := append([]string{"lease", "`gpu_wait_ms`", wait, "queue wait", "OPERATOR-GUIDE.md", "granted whole", "heartbeat", "--for"}, func() []string {
		keys := sortedMediaKeys()
		for i, k := range keys {
			keys[i] = "`" + k + "`"
		}
		return keys
	}()...)
	missingBest := len(want) + 1
	var best string
	for _, para := range strings.Split(strings.ReplaceAll(string(node), "\r\n", "\n"), "\n\n") {
		para = flat(para)
		var missing []string
		for _, w := range want {
			if !strings.Contains(para, w) {
				missing = append(missing, w)
			}
		}
		if len(missing) < missingBest {
			best, missingBest = para, len(missing)
		}
		if len(missing) == 0 {
			if slug := registerAnchor.FindString(para); slug != "" {
				t.Errorf("docs/FLEET-NODE.md links the guide through an anchor that carries a plan id (%s): name the section in prose", slug)
			}
			for _, retired := range retiredTermClaims {
				if strings.Contains(para, retired) {
					t.Errorf("docs/FLEET-NODE.md's media-lease paragraph still says %q, a claim of the retired rule that a media timeout must fit one lease term", retired)
				}
			}
			return
		}
	}
	t.Errorf("docs/FLEET-NODE.md has no paragraph that states a fleet media job's lease window is its route's own timeout key, granted whole and kept by its heartbeat: the closest paragraph is missing %d of %v", missingBest, want)
	for _, retired := range retiredTermClaims {
		if strings.Contains(best, retired) {
			t.Errorf("the closest paragraph of docs/FLEET-NODE.md still says %q, a claim of the retired rule", retired)
		}
	}
}
