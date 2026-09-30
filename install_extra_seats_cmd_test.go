package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// The resolver-level tests (tierseed, servingtmpl, deriveRender) each take the seats a box runs, and
// the write gate, as an INPUT. What a fresh ampere-16 install needs is that the COMMANDS compute those
// from the box and run the gate before writing, so these tests drive runInstallSeed, runInstallRender,
// runAuditConfig and runInstallVLLMSeat over a fake box and read what they print, write and refuse.
// Dropping the detection from a command, or the gate from the render, leaves the resolver-level tests
// green and a fresh install without the fast layer, or with a config the gate should have refused.

// tableRoot writes doc as the tier table of a repo root, so a command run with -root reads a table
// carrying one authored mistake exactly as it would read an edited profiles.json.
func tableRoot(t *testing.T, doc map[string]any) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "setup", "templates")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profiles.json"), marshalTable(t, doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// seedCommand runs `install seed` and returns the seed it printed and what it said on stderr.
func seedCommand(t *testing.T, args ...string) (map[string]any, string) {
	t.Helper()
	var out string
	notes := captureStderr(t, func() {
		out = captureStdout(t, func() {
			if err := runInstallSeed(args); err != nil {
				t.Errorf("install seed: %v", err)
			}
		})
	})
	var seed map[string]any
	if err := json.Unmarshal([]byte(out), &seed); err != nil {
		t.Fatalf("install seed printed no JSON: %v\n%s", err, out)
	}
	return seed, notes
}

func seedLayerNames(seed map[string]any) []string {
	var out []string
	layers, _ := seed["layers"].([]any)
	for _, l := range layers {
		m, _ := l.(map[string]any)
		out = append(out, fmt.Sprint(m["name"]))
	}
	return out
}

func (b fakeBox) seedArgs(extra ...string) []string {
	return append([]string{"-profile", "ampere-16", "-os", "linux", "-root", ".", "-home", b.home, "-vllm-venv", b.venv, "-hf-home", b.hf}, extra...)
}

func (b fakeBox) renderArgs(extra ...string) []string {
	return append([]string{"-profile", "ampere-16", "-os", "linux", "-root", ".", "-home", b.home,
		"-models", "/opt/offload/models", "-llama-bin", "/opt/offload/llama", "-ram-tier", "high",
		"-vllm-user", "svcuser", "-vllm-proxy-host", "192.0.2.10", "-vllm-venv", b.venv, "-hf-home", b.hf}, extra...)
}

// renderCommand runs `install render` with -out and returns the config it wrote (empty when it
// wrote none), what it said on stderr, and its error.
func renderCommand(t *testing.T, args ...string) (cfg, notes string, err error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "llama-swap.yaml")
	notes = captureStderr(t, func() {
		captureStdout(t, func() {
			err = runInstallRender(append(append([]string{}, args...), "-out", out))
		})
	})
	if b, rerr := os.ReadFile(out); rerr == nil {
		cfg = string(b)
	}
	return cfg, notes, err
}

func TestInstallSeedSeedsTheFastLayerOnlyWhereBothSeatsCanRun(t *testing.T) {
	p := ampere16Profile(t)
	lane, fast := p.VLLMSeat, p.ExtraVLLMSeats[0]
	box := newFakeBox(t).withVenv(t).withWeights(t, lane.ModelRepo).withWeights(t, fast.ModelRepo).withWrappers(t, fast.Unit)

	// Everything the box needs: both layers, both seats in the roster, one binding each.
	seed, notes := seedCommand(t, box.seedArgs()...)
	if got := seedLayerNames(seed); !reflect.DeepEqual(got, []string{"single", "fast"}) {
		t.Fatalf("layers = %v, want single and fast on a box with the venv, both seats' weights and the 35B's wrappers", got)
	}
	if got, _ := seed["vllm_seats"].([]any); len(got) != 2 || got[0] != lane.ID || got[1] != fast.ID {
		t.Errorf("vllm_seats = %v, want %s and %s", seed["vllm_seats"], lane.ID, fast.ID)
	}
	if got, _ := seed["kv_cache_server"].([]any); len(got) != 2 {
		t.Errorf("kv_cache_server = %v, want one binding per seat", seed["kv_cache_server"])
	}
	if strings.Contains(notes, "unavailable") {
		t.Errorf("a box that runs both seats printed a skip note:\n%s", notes)
	}

	// The 35B's weights go: only the 27B's layer, and the note says which prerequisite is missing.
	if err := os.RemoveAll(box.hf + "/" + fast.ModelRepo); err != nil {
		t.Fatal(err)
	}
	seed, notes = seedCommand(t, box.seedArgs()...)
	if got := seedLayerNames(seed); !reflect.DeepEqual(got, []string{"single"}) {
		t.Errorf("layers = %v, want only single once the 35B's weights are gone", got)
	}
	if got, _ := seed["vllm_seats"].([]any); len(got) != 1 || got[0] != lane.ID {
		t.Errorf("vllm_seats = %v, want only the 27B", seed["vllm_seats"])
	}
	for _, want := range []string{fast.ID, "snapshot", "not seeded"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the seed's note is missing %q:\n%s", want, notes)
		}
	}
}

// The wrapper scripts live in the seat directory, which `install render` takes as a flag; `install
// seed` must look where the render does, or the config names a layer the rendered llama-swap does
// not serve (or the reverse).
func TestInstallSeedFindsTheWrappersWhereTheSeatDirectoryFlagPointsIt(t *testing.T) {
	p := ampere16Profile(t)
	lane, fast := p.VLLMSeat, p.ExtraVLLMSeats[0]
	box := newFakeBox(t).withVenv(t).withWeights(t, lane.ModelRepo).withWeights(t, fast.ModelRepo)
	elsewhere := box
	elsewhere.seatDir = box.home + "/elsewhere"
	elsewhere.withWrappers(t, fast.Unit)

	seed, notes := seedCommand(t, box.seedArgs()...)
	if got := seedLayerNames(seed); !reflect.DeepEqual(got, []string{"single"}) {
		t.Errorf("layers = %v, want only single: the wrappers are not in the default seat directory", got)
	}
	if !strings.Contains(notes, box.seatDir+"/"+fast.Unit+"-cmd.sh") {
		t.Errorf("the note must name the file it looked for in the default seat directory:\n%s", notes)
	}
	seed, _ = seedCommand(t, box.seedArgs("-vllm-seat-dir", elsewhere.seatDir)...)
	if got := seedLayerNames(seed); !reflect.DeepEqual(got, []string{"single", "fast"}) {
		t.Errorf("layers = %v, want single and fast with --vllm-seat-dir pointing at the wrappers", got)
	}
}

func TestInstallRenderCommandRendersTheSeatsTheBoxCanRun(t *testing.T) {
	p := ampere16Profile(t)
	lane, fast := p.VLLMSeat, p.ExtraVLLMSeats[0]
	box := newFakeBox(t).withVenv(t).withWeights(t, lane.ModelRepo).withWeights(t, fast.ModelRepo)

	// The venv and both seats' weights but not the 35B's wrappers: the 27B only, and the note says
	// what to install (an entry whose scripts are missing would be listed and fail when asked for).
	cfg, notes, err := renderCommand(t, box.renderArgs()...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, "  "+lane.ID+":") || strings.Contains(cfg, "  "+fast.ID+":") {
		t.Errorf("without the 35B's wrappers the config must hold the 27B alone:\n%s", cfg)
	}
	for _, want := range []string{fast.ID, fast.Unit + "-cmd.sh", "by hand"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the render's note is missing %q:\n%s", want, notes)
		}
	}

	// With them: both entries, as alternatives, running the scripts that were just checked.
	box.withWrappers(t, fast.Unit)
	cfg, _, err = renderCommand(t, box.renderArgs()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  " + lane.ID + ":", "  " + fast.ID + ":", "(vagt | vagt2)",
		"cmd: " + box.seatDir + "/" + fast.Unit + "-cmd.sh", "proxy: http://192.0.2.10:18797",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("the rendered config is missing %q:\n%s", want, cfg)
		}
	}

	// A wrapper directory of the operator's choosing is honoured by the render, and the seat
	// directory it runs the entry from is that directory.
	other := newFakeBox(t).withVenv(t).withWeights(t, lane.ModelRepo).withWeights(t, fast.ModelRepo)
	other.seatDir = other.home + "/elsewhere"
	other.withWrappers(t, fast.Unit)
	cfg, _, err = renderCommand(t, other.renderArgs("-vllm-seat-dir", other.seatDir)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, "cmd: "+other.seatDir+"/"+fast.Unit+"-cmd.sh") {
		t.Errorf("the entry does not run the wrappers in --vllm-seat-dir:\n%s", cfg)
	}
}

// The write gate is what stands between a mistaken table edit and a config on a node. It is tested
// as a function (renderGate) elsewhere; this drives the COMMAND, because a render that no longer
// called it would pass every one of those tests and write whatever it was handed.
func TestInstallRenderCommandRefusesWhatTheWriteGateRefuses(t *testing.T) {
	commandArgs := func(root string) []string {
		return []string{"-profile", "blackwell-16", "-os", "linux", "-root", root, "-home", "/opt/offload",
			"-models", "/opt/offload/models", "-llama-bin", "/opt/offload/llama", "-ram-tier", "high"}
	}
	spill := func(n, max int) string {
		doc := tableDoc(t)
		e := tierEntry(doc, "blackwell-16")
		e["moe_26b"], e["n_cpu_moe"], e["n_cpu_moe_max"] = "n_cpu_moe", n, max
		return tableRoot(t, doc)
	}

	// The control: a spill equal to the measured one is written, so the refusals below are the gate's.
	cfg, _, err := renderCommand(t, commandArgs(spill(14, 14))...)
	if err != nil || !strings.Contains(cfg, "--n-cpu-moe 14") {
		t.Fatalf("the control render (spill 14 of a measured 14) must be written with the flag in it; got err %v\n%s", err, cfg)
	}

	// A spill above the measured one is refused, by tier and flag, and nothing is written.
	cfg, _, err = renderCommand(t, commandArgs(spill(20, 14))...)
	if err == nil || !strings.Contains(err.Error(), "--n-cpu-moe 20") || !strings.Contains(err.Error(), "not written") {
		t.Fatalf("the command must refuse a spill above the measured one, got %v", err)
	}
	if cfg != "" {
		t.Errorf("the refused render was written anyway:\n%s", cfg)
	}
	// A partial placement that names no N renders the every-expert form and is refused too.
	if _, _, err = renderCommand(t, commandArgs(spill(0, 14))...); err == nil || !strings.Contains(err.Error(), "no N") {
		t.Errorf("the command must refuse a partial placement that names no N, got %v", err)
	}

	// The layer check runs from the command for a tier that declares layers and no composition: a
	// layer that names a seat the rendered config does not define is refused before it is written.
	p := ampere16Profile(t)
	box := newFakeBox(t).withVenv(t).withWeights(t, p.VLLMSeat.ModelRepo).withWeights(t, p.ExtraVLLMSeats[0].ModelRepo).withWrappers(t, p.ExtraVLLMSeats[0].Unit)
	doc := tableDoc(t)
	fastLayerSeat(t, doc)["model"] = "qwen36-35b-a3b-typo"
	root := tableRoot(t, doc)
	cfg, _, err = renderCommand(t, box.renderArgs("-root", root)...)
	if err == nil || !strings.Contains(err.Error(), "qwen36-35b-a3b-typo") || !strings.Contains(err.Error(), "not written") {
		t.Fatalf("the command must refuse a layer that names an undefined seat, got %v", err)
	}
	if cfg != "" {
		t.Errorf("the phantom-layer render was written anyway:\n%s", cfg)
	}
}

// `audit-config` speaks for a node's vLLM seats as a set: `true` says the node serves the lane seat
// and every extra seat, and `auto` detects each one locally, as the installer does.
func TestAuditConfigDecidesTheExtraSeatsTheWayTheInstallerDoes(t *testing.T) {
	p := ampere16Profile(t)
	lane, fast := p.VLLMSeat, p.ExtraVLLMSeats[0]
	box := newFakeBox(t).withVenv(t).withWeights(t, lane.ModelRepo).withWeights(t, fast.ModelRepo).withWrappers(t, fast.Unit)

	// A node the installer seeded on a box that runs both seats.
	seed, err := tierseed.Resolve(p, "ampere-16", tierseed.Options{Home: box.home, GOOS: "linux", VLLMSeatActive: true,
		ExtraVLLMSeatsActive: map[string]bool{fast.ID: true}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	audit := func(seatFlags ...string) (string, error) {
		var runErr error
		out := captureStdout(t, func() {
			runErr = runAuditConfig(append([]string{"--config", cfg, "--tier", "ampere-16", "--root", ".", "--home", box.home,
				"--goos", "linux", "--all"}, seatFlags...))
		})
		return out, runErr
	}

	if out, err := audit("--vllm-seat-active", "true"); err != nil {
		t.Errorf("--vllm-seat-active true speaks for every vLLM seat: a node seeded with both audits clean, got %v\n%s", err, out)
	}
	autoFlags := []string{"--vllm-seat-active", "auto", "--vllm-venv", box.venv, "--hf-home", box.hf}
	if out, err := audit(autoFlags...); err != nil {
		t.Errorf("auto on the box that runs both seats must audit clean, got %v\n%s", err, out)
	}
	// The 35B's wrappers are gone from the box: the installer would no longer seed the layer, so a
	// node that still carries it has drifted from what a fresh install writes.
	for _, f := range []string{"-cmd.sh", "-cmdstop.sh"} {
		if err := os.Remove(box.seatDir + "/" + fast.Unit + f); err != nil {
			t.Fatal(err)
		}
	}
	out, err := audit(autoFlags...)
	if !errors.Is(err, errConfigDrift) {
		t.Fatalf("auto must see that the 35B's wrappers are gone and report drift, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "layers") {
		t.Errorf("the drift report must name the layers the box can no longer serve:\n%s", out)
	}
	// Installed again, in a seat directory of the operator's choosing: audit-config takes the same
	// --vllm-seat-dir as `install seed` and `install render`, so all three look in one place.
	elsewhere := box
	elsewhere.seatDir = box.home + "/elsewhere"
	elsewhere.withWrappers(t, fast.Unit)
	if out, err := audit(append(autoFlags, "--vllm-seat-dir", elsewhere.seatDir)...); err != nil {
		t.Errorf("auto with --vllm-seat-dir pointing at the wrappers must audit clean, got %v\n%s", err, out)
	}
}

// `install vllm-seat` renders the LANE seat only. At the moment the operator is installing seats it
// must say what it did not render for the tier's extra seats, and a tier with none says nothing.
func TestInstallVLLMSeatCommandSaysWhatItDidNotRenderForAnExtraSeat(t *testing.T) {
	run := func(tier, repo string) string {
		box := newFakeBox(t).withVenv(t).withWeights(t, repo)
		var out string
		captureStderr(t, func() {
			out = captureStdout(t, func() {
				if err := runInstallVLLMSeat([]string{"-profile", tier, "-root", ".", "-home", box.home, "-out", box.home + "/rendered",
					"-user", "svcuser", "-proxy-host", "192.0.2.10", "-venv", box.venv, "-hf-home", box.hf}); err != nil {
					t.Errorf("install vllm-seat --profile %s: %v", tier, err)
				}
			})
		})
		return out
	}
	p := ampere16Profile(t)
	out := run("ampere-16", p.VLLMSeat.ModelRepo)
	if !strings.Contains(out, "wrote ") {
		t.Fatalf("the control command wrote no artifact:\n%s", out)
	}
	for _, want := range []string{p.ExtraVLLMSeats[0].ID, "NOT rendered by this command", "vllm-35b-seat.service", "vllm-35b-seat-cmd.sh", "by hand"} {
		if !strings.Contains(out, want) {
			t.Errorf("the command's output is missing %q:\n%s", want, out)
		}
	}
	// A tier with a lane seat and no extra seat has nothing to say.
	var doc struct {
		Profiles map[string]servingProfile `json:"profiles"`
	}
	if err := json.Unmarshal(embeddedProfiles, &doc); err != nil {
		t.Fatal(err)
	}
	plain := run("blackwell-16", doc.Profiles["blackwell-16"].VLLMSeat.ModelRepo)
	if !strings.Contains(plain, "wrote ") || strings.Contains(plain, "NOT rendered by this command") {
		t.Errorf("a tier with no extra seat must render its seat and say nothing about extras:\n%s", plain)
	}
}
