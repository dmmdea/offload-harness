package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/tierseed"
	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// tableDoc reads the embedded tier table as generic JSON so a test can author one mistake
// into one tier and hand the result to deriveRender, exactly as a mistaken edit of
// profiles.json would arrive.
func tableDoc(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(embeddedProfiles, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func tierEntry(doc map[string]any, tier string) map[string]any {
	return doc["profiles"].(map[string]any)[tier].(map[string]any)
}

func marshalTable(t *testing.T, doc map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// renderReq is a render request no machine detail leaks into: RFC 5737 addresses, a made-up
// install root, and the vLLM half PINNED so the result cannot depend on whether the machine
// running the test happens to have a hand-built venv.
func renderReq(tier, goos string, pin *pinnedVLLM) renderRequest {
	return renderRequest{
		TierID: tier, RAMTier: "high", GOOS: goos,
		LlamaBin: "/opt/offload/build/llamacpp/build/bin", ModelsDir: "/opt/offload/models",
		Listen: "127.0.0.1:11436", Home: "/opt/offload", Threads: 8,
		PinnedVLLM: pin,
	}
}

func seatRuntime() vllmseat.Runtime {
	return vllmseat.Runtime{
		User: "svcuser", ProxyHost: "192.0.2.10",
		StackDir: "/opt/offload", SeatDir: "/opt/offload/seat", VenvDir: "/opt/offload/vllm-env", HFHome: "/hf",
		Distro: "distro", WSLSeatDir: "/opt/seat", CacheMountSrc: "//store.invalid/kvcache", CacheMountOpts: "vers=3.1.1",
		LMCacheOverlay: "/opt/lmcache-overlay",
	}
}

// pinFor pins a tier's declared vLLM seats as a box that runs them would resolve them.
func pinFor(t *testing.T, raw []byte, tier string, lane, extras bool) *pinnedVLLM {
	t.Helper()
	var doc struct {
		Profiles map[string]servingProfile `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	p := doc.Profiles[tier]
	pin := &pinnedVLLM{Runtime: seatRuntime()}
	resolve := func(s vllmseat.Spec) *vllmseat.Spec {
		s.ModelPath = "/hf/" + s.ModelRepo + "/snapshots/0123456789abcdef"
		return &s
	}
	if lane && p.VLLMSeat != nil {
		pin.Seat = resolve(*p.VLLMSeat)
	}
	if extras {
		for _, e := range p.ExtraVLLMSeats {
			pin.Extras = append(pin.Extras, resolve(e))
		}
	}
	return pin
}

func layerNamesOf(ls []config.LayerSpec) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}

// INV-16 (operator, 2026-09-02/03, verbatim): the harness installs and serves on ANY single
// PC, and the cache-server tier is an OPTIONAL configuration — off by default, never a
// dependency of the install or of the single-box seats. Until now nothing made a build fail
// when a tier's install came to lean on it. This walks the shipped table:
//
//   - with no vLLM prerequisites at all (any plain PC) every tier renders, passes the write
//     gate, and the serving config names no vLLM seat and no cache-server piece;
//   - a tier whose vLLM seat declares no cache_server renders the seat, its unit and its
//     wrappers with no L2/LMCache anywhere, and SEEDS an explicit storeless binding with a
//     reason for it, so `doctor` (which fails a vLLM seat with no binding) passes a fresh
//     install;
//   - a tier with no vLLM seat seeds neither a roster nor a binding.
func TestInstallRendersOnAnyTierWithoutACacheServer(t *testing.T) {
	var doc struct {
		Profiles map[string]servingProfile `json:"profiles"`
	}
	if err := json.Unmarshal(embeddedProfiles, &doc); err != nil {
		t.Fatal(err)
	}
	seedProfiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	// The cache-server pieces a render must never carry unless the tier declared a store.
	cacheTokens := []string{"kv-transfer-config", "lmcache", "seat_l2", "kv_connector", "kvcache"}
	noCache := func(what, text string) {
		t.Helper()
		low := strings.ToLower(codeOnly(text))
		for _, tok := range cacheTokens {
			if strings.Contains(low, tok) {
				t.Errorf("%s carries %q — the cache-server tier is optional and must not enter an install that declared none", what, tok)
			}
		}
	}

	storeless, plain, withStore := 0, 0, 0
	for _, tier := range sortedKeys(doc.Profiles) {
		p := doc.Profiles[tier]
		rendered := false
		for _, goos := range []string{"linux", "windows"} {
			// 1. any plain PC: no venv, no weights, nothing pinned.
			res, err := deriveRender(embeddedProfiles, renderReq(tier, goos, &pinnedVLLM{}))
			if err != nil {
				if strings.Contains(err.Error(), "no serving template") {
					continue // this tier has no template for this OS; not this test's business
				}
				t.Errorf("tier %s (%s) does not render on a PC with no vLLM prerequisites: %v", tier, goos, err)
				continue
			}
			rendered = true
			if err := renderGate(res); err != nil {
				t.Errorf("tier %s (%s): the write gate refuses a plain-PC render: %v", tier, goos, err)
			}
			if names := vllmEntries(t, res.Config); len(names) != 0 {
				t.Errorf("tier %s (%s): a box with no vLLM prerequisites rendered vLLM seat entries %v", tier, goos, names)
			}
			noCache("the plain-PC render of "+tier+"/"+goos, res.Config)
		}
		if !rendered {
			t.Errorf("tier %s rendered on neither OS — the install cannot serve there at all", tier)
		}

		// 2/3. the seed, in the binding an install writes.
		sp := seedProfiles[tier]
		all := map[string]bool{}
		for _, e := range sp.ExtraVLLMSeats {
			all[e.ID] = true
		}
		seed, err := tierseed.Resolve(sp, tier, tierseed.Options{Home: "/opt/offload", GOOS: "linux", RAMTier: "high", VLLMSeatActive: true, ExtraVLLMSeatsActive: all})
		if err != nil {
			t.Errorf("tier %s: seed does not resolve: %v", tier, err)
			continue
		}
		if p.VLLMSeat == nil {
			plain++
			for _, k := range []string{"vllm_seats", "kv_cache_server"} {
				if _, present := seed[k]; present {
					t.Errorf("tier %s declares no vLLM seat but seeds %s (%v)", tier, k, seed[k])
				}
			}
			continue
		}
		// a vLLM tier: the seeded config must pass doctor's per-seat gate.
		b, _ := json.Marshal(seed)
		var c config.Config
		if err := json.Unmarshal(b, &c); err != nil {
			t.Errorf("tier %s: seed does not load: %v", tier, err)
			continue
		}
		if un := c.KVCacheServers.UnboundSeats(c.VLLMSeats); len(un) != 0 {
			t.Errorf("tier %s: a fresh install would fail doctor — vLLM seats with no cache-server binding: %v", tier, un)
		}
		if p.VLLMSeat.CacheServer != nil {
			withStore++
			continue // a store-declaring tier: the store is its own optional choice, covered by the seat-binding tests
		}
		storeless++
		roster := append([]string{p.VLLMSeat.ID}, func() []string {
			var ids []string
			for _, e := range p.ExtraVLLMSeats {
				ids = append(ids, e.ID)
			}
			return ids
		}()...)
		if got, _ := seed["vllm_seats"].([]string); !reflect.DeepEqual(got, roster) {
			t.Errorf("tier %s: vllm_seats = %v, want %v", tier, got, roster)
		}
		binds, _ := seed["kv_cache_server"].([]map[string]any)
		if len(binds) != len(roster) {
			t.Errorf("tier %s: kv_cache_server = %v, want one binding per seat %v", tier, seed["kv_cache_server"], roster)
			continue
		}
		for i, seat := range roster {
			bnd := binds[i]
			if bnd["seat"] != seat || bnd["storeless"] != true || strings.TrimSpace(asString(bnd["reason"])) == "" {
				t.Errorf("tier %s: seat %s must be bound storeless with a stated reason (an absent binding is the silence doctor refuses), got %v", tier, seat, bnd)
			}
			if _, has := bnd["address"]; has {
				t.Errorf("tier %s: storeless binding for %s names a store address: %v", tier, seat, bnd)
			}
		}
		// The seat itself renders with no cache server: unit, wrappers, run script, polkit rule.
		spec := *p.VLLMSeat
		spec.ModelPath = "/hf/" + spec.ModelRepo + "/snapshots/0123456789abcdef"
		rt := seatRuntime()
		rt.LMCacheOverlay = "" // a storeless seat needs no overlay — that is the point
		files, err := spec.Artifacts(filepath.Join("setup", "templates", "vllm-seat", "linux-systemd"), rt)
		if err != nil {
			t.Errorf("tier %s: the seat's artifacts do not render without a cache server: %v", tier, err)
			continue
		}
		for name, body := range files {
			noCache("tier "+tier+" seat artifact "+name, body)
		}
		// And the render with the seat ACTIVE still passes the write gate, entry included.
		for _, goos := range []string{"linux", "windows"} {
			res, err := deriveRender(embeddedProfiles, renderReq(tier, goos, pinFor(t, embeddedProfiles, tier, true, true)))
			if err != nil {
				continue // no template for this OS, or a seat launch this OS cannot host
			}
			if err := renderGate(res); err != nil {
				t.Errorf("tier %s (%s): the write gate refuses the storeless-seat render: %v", tier, goos, err)
			}
			noCache("the storeless-seat render of "+tier+"/"+goos, res.Config)
		}
	}
	// The gate must have had something to look at in each bucket, or it passed vacuously.
	if storeless == 0 || plain == 0 {
		t.Fatalf("INV-16 walked %d storeless-seat tier(s), %d plain tier(s) (%d with a store) — a bucket was empty, so the gate proved nothing",
			storeless, plain, withStore)
	}
	t.Logf("INV-16: %d tiers without a vLLM seat, %d with a storeless seat, %d declaring a store — every one installs without a cache server", plain, storeless, withStore)
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// codeOnly drops comment lines (`#` in yaml and shell, `//` in a polkit rule): the templates
// EXPLAIN the cache-server tier in prose, and a gate that read the prose would fail on the
// explanation of why the tier is optional.
func codeOnly(text string) string {
	var keep []string
	for _, l := range strings.Split(text, "\n") {
		if tl := strings.TrimSpace(l); strings.HasPrefix(tl, "#") || strings.HasPrefix(tl, "//") {
			continue
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, "\n")
}

// vllmEntries names the models of a rendered llama-swap config that are vLLM seats. The marker
// is structural, not a substring: `useModelName` is written only by a seat's entry (vLLM 404s
// any name it does not serve, so the entry rewrites every alias), while the templates mention
// vLLM freely in the comments that explain their layout.
func vllmEntries(t *testing.T, cfg string) []string {
	t.Helper()
	var doc struct {
		Models map[string]struct {
			UseModelName string `yaml:"useModelName"`
		} `yaml:"models"`
	}
	if err := yaml.Unmarshal([]byte(cfg), &doc); err != nil {
		t.Fatalf("a rendered config is not parseable YAML: %v", err)
	}
	var out []string
	for name, m := range doc.Models {
		if m.UseModelName != "" {
			out = append(out, name)
		}
	}
	return out
}

// H-01's remaining rule at the write gate: `install render` refuses a config whose
// `--n-cpu-moe` exceeds the tier's MEASURED spill (profiles.json `n_cpu_moe_max`), and refuses
// a partial placement that names no N, because that one renders the every-expert `--cpu-moe`.
// Nothing shipped declares a partial spill today, so this is a control: the gate is built and
// can be made to fail, not merely assumed to pass.
func TestInstallRenderRefusesNCPUMoEAboveTheTiersMeasuredSpill(t *testing.T) {
	render := func(moe string, n, max int) (renderResult, error) {
		doc := tableDoc(t)
		e := tierEntry(doc, "blackwell-16") // a CUDA tier that serves the 26B
		e["moe_26b"] = moe
		if n != 0 {
			e["n_cpu_moe"] = n
		}
		if max != 0 {
			e["n_cpu_moe_max"] = max
		}
		return deriveRender(marshalTable(t, doc), renderReq("blackwell-16", "linux", &pinnedVLLM{}))
	}

	// the spill the tier measured is sanctioned...
	res, err := render("n_cpu_moe", 14, 14)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Config, "--n-cpu-moe 14 -ngl 999") {
		t.Fatalf("the control render carries no `--n-cpu-moe 14`, so the gate would be tested against nothing:\n%s", res.Config)
	}
	if err := renderGate(res); err != nil {
		t.Fatalf("a spill equal to the measured one must pass the write gate: %v", err)
	}
	// ...one layer more is refused, naming the tier, the flag, both numbers and the rule.
	res, err = render("n_cpu_moe", 20, 14)
	if err != nil {
		t.Fatal(err)
	}
	err = renderGate(res)
	if err == nil {
		t.Fatal("install render accepted --n-cpu-moe 20 against a measured spill of 14")
	}
	for _, want := range []string{"blackwell-16", "--n-cpu-moe 20", "14", "INV-1", "not written"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must carry %q, got: %v", want, err)
		}
	}
	// a tier that recorded NO measured spill sanctions none.
	res, err = render("n_cpu_moe", 14, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := renderGate(res); err == nil || !strings.Contains(err.Error(), "no measured spill") {
		t.Fatalf("a tier with no n_cpu_moe_max must refuse any --n-cpu-moe, got %v", err)
	}
	// the partial placement that names no N silently renders --cpu-moe (every expert in RAM)
	res, err = render("n_cpu_moe", 0, 14)
	if err != nil {
		t.Fatal(err)
	}
	if err := renderGate(res); err == nil || !strings.Contains(err.Error(), "no N") {
		t.Fatalf("moe_26b n_cpu_moe with no n_cpu_moe renders the every-expert form and must be refused, got %v", err)
	}
	// and the text-level rule does not depend on the profile: a template that regressed to a
	// literal `--n-cpu-moe` is refused even though the tier's own fields are innocent.
	clean, err := render("gpu", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := renderGate(clean); err != nil {
		t.Fatalf("the unmodified tier must pass: %v", err)
	}
	regressed := clean
	// The anchor is the 26B entry's own command line: the first `-ngl 99` in the text is the
	// template's header comment, which lists the substituted tokens and is not a command.
	regressed.Config = strings.Replace(clean.Config, "-ngl 99 --parallel 1", "--n-cpu-moe 30 -ngl 999 --parallel 1", 1)
	if regressed.Config == clean.Config {
		t.Fatal("the control edit found no 26B command line to regress")
	}
	if err := renderGate(regressed); err == nil || !strings.Contains(err.Error(), "--n-cpu-moe 30") {
		t.Fatalf("a hard-coded --n-cpu-moe in a template must be refused, got %v", err)
	}
}

// renderGate now hosts H-01's original rules too; a move must not have lost them.
func TestRenderGateStillRefusesTheOperatorRules(t *testing.T) {
	res, err := deriveRender(embeddedProfiles, renderReq("blackwell-16", "linux", &pinnedVLLM{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := renderGate(res); err != nil {
		t.Fatalf("the unmodified render must pass: %v", err)
	}
	broken := res
	broken.Config = strings.Replace(res.Config, "--n-gpu-layers 99", "--n-gpu-layers 0", 1)
	if broken.Config == res.Config {
		t.Fatal("the control edit found nothing to break")
	}
	err = renderGate(broken)
	if err == nil || !strings.Contains(err.Error(), "INV-1/INV-2") || !strings.Contains(err.Error(), "not written") {
		t.Fatalf("a CPU-resident model must be refused by the write gate, got %v", err)
	}
}

// A rendered ampere-16 install serves the fast layer with no hand edit: when the box runs the
// 35B, its llama-swap entry is in the rendered config, alternated with the 27B (they cannot
// share the card), the layer names it, and the composition check — now run for any tier that
// declares layers, not only composite ones — finds every layer seat defined.
func TestAmpere16RenderServesTheFastLayerSeat(t *testing.T) {
	res, err := deriveRender(embeddedProfiles, renderReq("ampere-16", "linux", pinFor(t, embeddedProfiles, "ampere-16", true, true)))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := layerNamesOf(res.Layers), []string{"single", "fast"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("layers = %v, want %v", got, want)
	}
	if err := renderGate(res); err != nil {
		t.Fatalf("the write gate refuses the fast-layer render: %v", err)
	}
	for _, want := range []string{
		"  qwen38-27b-gsq-vllm:", "  qwen36-35b-a3b-gsq-vllm:",
		"cmd: /opt/offload/seat/vllm-35b-seat-cmd.sh", // the extra seat's OWN wrappers
		"cmd: /opt/offload/seat/vllm-seat-cmd.sh",     // the lane seat's did not move
		"aliases: [a2-pool-35b, qwen36-35b-gsq]",
		"concurrencyLimit: 8",
	} {
		if !strings.Contains(res.Config, want) {
			t.Errorf("the rendered config is missing %q:\n%s", want, res.Config)
		}
	}
	// Two heavy seats cannot both be loaded on the card: alternatives, never co-resident.
	if !strings.Contains(res.Config, "(vagt | vagt2)") || strings.Contains(res.Config, "vagt & vagt2") {
		t.Errorf("the seats must be alternatives inside the residents set:\n%s", res.Config)
	}

	// Only the lane seat present: no fast entry, no fast layer — and the gate still passes.
	only, err := deriveRender(embeddedProfiles, renderReq("ampere-16", "linux", pinFor(t, embeddedProfiles, "ampere-16", true, false)))
	if err != nil {
		t.Fatal(err)
	}
	if got := layerNamesOf(only.Layers); !reflect.DeepEqual(got, []string{"single"}) {
		t.Fatalf("layers = %v, want only single", got)
	}
	if strings.Contains(only.Config, "qwen36-35b-a3b-gsq-vllm") {
		t.Error("a box without the 35B's weights rendered its entry")
	}
	if err := renderGate(only); err != nil {
		t.Fatalf("the write gate refuses the single-layer render: %v", err)
	}

	// No vLLM at all: byte-identical to the tier as it rendered before layers and the extra
	// seat existed. The tier's declarations are stripped from a copy of the table to prove it.
	plain, err := deriveRender(embeddedProfiles, renderReq("ampere-16", "linux", &pinnedVLLM{}))
	if err != nil {
		t.Fatal(err)
	}
	doc := tableDoc(t)
	e := tierEntry(doc, "ampere-16")
	delete(e, "layers")
	delete(e, "extra_vllm_seats")
	before, err := deriveRender(marshalTable(t, doc), renderReq("ampere-16", "linux", &pinnedVLLM{}))
	if err != nil {
		t.Fatal(err)
	}
	if plain.Config != before.Config {
		t.Fatal("a plain llama.cpp ampere-16 install renders differently now that the tier declares layers and an extra seat")
	}
	if len(plain.Layers) != 0 {
		t.Errorf("a box with no vLLM seat resolved layers %v", layerNamesOf(plain.Layers))
	}
	if err := renderGate(plain); err != nil {
		t.Fatalf("the plain render fails the write gate: %v", err)
	}
}

// The composition check exists to refuse a PHANTOM binding: a layer that routes to a seat the
// rendered config does not define. It used to run only for tiers that declare `composes`; a
// layers-only tier like ampere-16 had the phantom risk and no check.
func TestLayerOnlyTierRenderRefusesAPhantomLayerSeat(t *testing.T) {
	doc := tableDoc(t)
	e := tierEntry(doc, "ampere-16")
	layers := e["layers"].([]any)
	fastSeat := layers[1].(map[string]any)["seats"].([]any)[0].(map[string]any)
	fastSeat["model"] = "qwen36-35b-a3b-typo" // names no seat anywhere: not a vLLM seat, not in the template
	res, err := deriveRender(marshalTable(t, doc), renderReq("ampere-16", "linux", pinFor(t, embeddedProfiles, "ampere-16", true, true)))
	if err != nil {
		t.Fatal(err)
	}
	err = renderGate(res)
	if err == nil || !strings.Contains(err.Error(), "qwen36-35b-a3b-typo") || !strings.Contains(err.Error(), "not defined in the rendered config") {
		t.Fatalf("a layer naming an undefined seat must be refused by name, got %v", err)
	}
}

// The renderer and the audit replay must agree on which seats a box runs: a stamp records
// the extra seats a render was given, and the replay pins them instead of re-detecting on
// the auditing machine (which would report every node in the fleet STALE).
func TestReplayPinsTheExtraSeatsAStampRecorded(t *testing.T) {
	pin := pinFor(t, embeddedProfiles, "ampere-16", true, true)
	res, err := deriveRender(embeddedProfiles, renderReq("ampere-16", "linux", pin))
	if err != nil {
		t.Fatal(err)
	}
	req, ok := replayRequest(res.Basis)
	if !ok {
		t.Fatal("the stamp of an ampere-16 render is not replayable")
	}
	if req.PinnedVLLM == nil || len(req.PinnedVLLM.Extras) != 1 || req.PinnedVLLM.Extras[0].ID != "qwen36-35b-a3b-gsq-vllm" {
		t.Fatalf("the replay did not pin the extra seat the stamp recorded: %+v", req.PinnedVLLM)
	}
	again, err := deriveRender(embeddedProfiles, req)
	if err != nil {
		t.Fatal(err)
	}
	if again.Config != res.Config {
		t.Fatal("replaying a stamp does not reproduce the config it was taken from")
	}
}

// An extra seat's llama-swap entry is rendered, but its unit and wrapper scripts are the
// operator's step, and llama-swap does not check that an entry's `cmd` exists when it loads
// its config — so a seat whose wrappers were never installed sits in the roster and fails only
// when asked for. The render says so at install time, while the fix is cheap.
func TestRenderWarnsWhenAnExtraSeatsWrappersAreNotInstalled(t *testing.T) {
	extras := pinFor(t, embeddedProfiles, "ampere-16", true, true).Extras
	if len(extras) != 1 {
		t.Fatalf("ampere-16 pins %d extra seats, want the 35B", len(extras))
	}
	dir := t.TempDir()
	var buf strings.Builder
	warnMissingExtraSeatWrappers(extras, dir, runtime.GOOS, &buf)
	for _, want := range []string{"vllm-35b-seat-cmd.sh", "vllm-35b-seat-cmdstop.sh", "by hand", "qwen36-35b-a3b-gsq-vllm", "2 extra vLLM seat wrapper"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the warning is missing %q:\n%s", want, buf.String())
		}
	}
	// Rendering for another machine: a local miss means nothing.
	buf.Reset()
	other := "windows"
	if runtime.GOOS == "windows" {
		other = "linux"
	}
	warnMissingExtraSeatWrappers(extras, dir, other, &buf)
	if buf.Len() != 0 {
		t.Errorf("warned about another machine's directory:\n%s", buf.String())
	}
	// Installed: silence.
	for _, f := range []string{"vllm-35b-seat-cmd.sh", "vllm-35b-seat-cmdstop.sh"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	warnMissingExtraSeatWrappers(extras, dir, runtime.GOOS, &buf)
	if buf.Len() != 0 {
		t.Errorf("warned although both wrappers are installed:\n%s", buf.String())
	}
	// No extra seats (every other tier): nothing, ever.
	warnMissingExtraSeatWrappers(nil, dir, runtime.GOOS, &buf)
	if buf.Len() != 0 {
		t.Errorf("warned for a tier with no extra seats:\n%s", buf.String())
	}
}

// `install vllm-seat` renders the LANE seat only. At the moment the operator is installing seats
// it must say what it did not render for the tier's extra seats, and a tier with none says nothing.
func TestInstallVLLMSeatSaysWhatItDoesNotRenderForExtraSeats(t *testing.T) {
	var doc struct {
		Profiles map[string]servingProfile `json:"profiles"`
	}
	if err := json.Unmarshal(embeddedProfiles, &doc); err != nil {
		t.Fatal(err)
	}
	note := extraSeatsNote(doc.Profiles["ampere-16"])
	for _, want := range []string{"qwen36-35b-a3b-gsq-vllm", "NOT rendered", "vllm-35b-seat.service", "vllm-35b-seat-run.sh",
		"vllm-35b-seat-cmd.sh", "vllm-35b-seat-cmdstop.sh", "by hand", "composite-tier.md"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note is missing %q:\n%s", want, note)
		}
	}
	for _, tier := range []string{"blackwell-16", "blackwell-2x16", "ampere-8"} {
		if n := extraSeatsNote(doc.Profiles[tier]); n != "" {
			t.Errorf("tier %s declares no extra seat but the note says:\n%s", tier, n)
		}
	}
}
