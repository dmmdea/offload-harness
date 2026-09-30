package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// A tier's extra vLLM seat (ampere-16's 35B) is prerequisite-gated like the lane seat, with one
// addition these tests pin: its wrapper scripts are the OPERATOR's step (the seat's launch line
// carries flags the shared unit template cannot express), and llama-swap does not check that an
// entry's `cmd` exists when it loads its config, so a box that has the venv and the weights but not
// the scripts would list the seat, seed its layer, and fail only when a contract asked for it.
// The box advertises the seat only once the scripts are on it.

// fakeBox is a made-up machine under a temp dir. Its paths are kept in the slash form the
// installer resolves them to, whatever OS runs the test.
type fakeBox struct{ home, venv, hf, seatDir string }

func newFakeBox(t *testing.T) fakeBox {
	t.Helper()
	root := filepath.ToSlash(t.TempDir())
	return fakeBox{home: root, venv: root + "/vllm-env", hf: root + "/hf", seatDir: root + "/seat"}
}

func putScript(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// withVenv lays down the hand-built vLLM environment's entry point.
func (b fakeBox) withVenv(t *testing.T) fakeBox {
	t.Helper()
	putScript(t, b.venv+"/bin/vllm")
	return b
}

// withWeights lays down exactly one HF snapshot for a seat's model repo.
func (b fakeBox) withWeights(t *testing.T, repo string) fakeBox {
	t.Helper()
	if err := os.MkdirAll(b.hf+"/"+repo+"/snapshots/abc", 0o755); err != nil {
		t.Fatal(err)
	}
	return b
}

// withWrappers lays down the two wrapper scripts a seat's llama-swap entry runs.
func (b fakeBox) withWrappers(t *testing.T, unit string) fakeBox {
	t.Helper()
	putScript(t, b.seatDir+"/"+unit+"-cmd.sh")
	putScript(t, b.seatDir+"/"+unit+"-cmdstop.sh")
	return b
}

func (b fakeBox) flags() vllmRuntimeFlags {
	return vllmRuntimeFlags{user: "svcuser", proxyHost: "192.0.2.10", venv: b.venv, hfHome: b.hf, seatDir: b.seatDir}
}

func (b fakeBox) runtime() vllmseat.Runtime { return b.flags().resolve(b.home) }

// renderReq is the request `install render` builds for this box, with nothing pinned: the seats
// the box runs are detected from the files above.
func (b fakeBox) renderReq() renderRequest {
	return renderRequest{
		TierID: "ampere-16", RAMTier: "high", GOOS: "linux",
		LlamaBin: "/opt/offload/llama", ModelsDir: "/opt/offload/models",
		Listen: "127.0.0.1:11436", Home: b.home, Threads: 8, VLLM: b.flags(),
	}
}

// captureStderr is captureStdout's counterpart: the installer says why it skipped a seat on stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	os.Stderr = orig
	return <-done
}

// ampere16Serving is the render-side view of the tier (the seed-side view is ampere16Profile).
func ampere16Serving(t *testing.T) servingProfile {
	t.Helper()
	var doc struct {
		Profiles map[string]servingProfile `json:"profiles"`
	}
	if err := json.Unmarshal(embeddedProfiles, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Profiles["ampere-16"]
}

func TestAnExtraSeatNeedsItsWrappersBeforeABoxAdvertisesIt(t *testing.T) {
	p := ampere16Profile(t)
	lane, fast := p.VLLMSeat, p.ExtraVLLMSeats[0]
	box := newFakeBox(t).withVenv(t).withWeights(t, lane.ModelRepo).withWeights(t, fast.ModelRepo)

	// The venv and both seats' weights, no wrapper scripts for the 35B: the 27B is served, the 35B
	// and the layer that names it are not, and both commands say why.
	var res renderResult
	notes := captureStderr(t, func() {
		var err error
		if res, err = deriveRender(embeddedProfiles, box.renderReq()); err != nil {
			t.Fatal(err)
		}
	})
	if res.Params.VLLMSeat == nil || res.Params.VLLMSeat.ID != lane.ID {
		t.Fatalf("the lane seat (its wrappers are installer output) must render: %+v", res.Params.VLLMSeat)
	}
	if len(res.Params.ExtraVLLMSeats) != 0 || strings.Contains(res.Config, "  "+fast.ID+":") {
		t.Errorf("the 35B has no wrapper scripts on this box, yet it was rendered (%d extra seat(s) in the params)", len(res.Params.ExtraVLLMSeats))
	}
	if got := layerNamesOf(res.Layers); !reflect.DeepEqual(got, []string{"single"}) {
		t.Errorf("layers = %v, want only single until the 35B's wrappers are installed", got)
	}
	for _, want := range []string{fast.ID, fast.Unit + "-cmd.sh", "by hand", "not rendered"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the render's note is missing %q:\n%s", want, notes)
		}
	}
	var seedNote strings.Builder
	if _, extras := detectVLLMSeats(p, box.runtime(), &seedNote); extras[fast.ID] {
		t.Error("`install seed` would roster and bind a seat whose wrapper scripts are not on the box")
	}
	for _, want := range []string{fast.ID, fast.Unit + "-cmd.sh", "by hand", "not seeded"} {
		if !strings.Contains(seedNote.String(), want) {
			t.Errorf("the seed's note is missing %q:\n%s", want, seedNote.String())
		}
	}

	// The operator installs them (docs/systems/composite-tier.md, step 4) and re-runs: now the box
	// advertises the seat, and its entry runs the scripts that were just checked.
	box.withWrappers(t, fast.Unit)
	captureStderr(t, func() {
		var err error
		if res, err = deriveRender(embeddedProfiles, box.renderReq()); err != nil {
			t.Fatal(err)
		}
	})
	if err := renderGate(res); err != nil {
		t.Fatalf("the write gate refuses the two-seat render: %v", err)
	}
	if got := layerNamesOf(res.Layers); !reflect.DeepEqual(got, []string{"single", "fast"}) {
		t.Errorf("layers = %v, want single and fast once the wrappers are on the box", got)
	}
	for _, want := range []string{
		"  " + fast.ID + ":", "cmd: " + box.seatDir + "/" + fast.Unit + "-cmd.sh", "cmdStop: " + box.seatDir + "/" + fast.Unit + "-cmdstop.sh",
		"(vagt | vagt2)",
	} {
		if !strings.Contains(res.Config, want) {
			t.Errorf("the rendered config is missing %q:\n%s", want, res.Config)
		}
	}
	if _, extras := detectVLLMSeats(p, box.runtime(), io.Discard); !extras[fast.ID] {
		t.Error("`install seed` must roster and bind the 35B once its wrappers are on the box")
	}
}

// `install seed`, `audit-config` and `install render` must decide the same seats, or the config
// names a layer the rendered llama-swap does not serve (or serves one the config never names).
// Every combination of the three things a box can have or lack is compared.
func TestSeedAndRenderDecideAnExtraSeatAlike(t *testing.T) {
	p := ampere16Profile(t)
	sp := ampere16Serving(t)
	fast := p.ExtraVLLMSeats[0]
	for _, venv := range []bool{false, true} {
		for _, weights := range []bool{false, true} {
			for _, wrappers := range []bool{false, true} {
				box := newFakeBox(t)
				if venv {
					box.withVenv(t)
				}
				if weights {
					box.withWeights(t, fast.ModelRepo)
				}
				if wrappers {
					box.withWrappers(t, fast.Unit)
				}
				want := venv && weights && wrappers
				var seeded map[string]bool
				var rendered []*vllmseat.Spec
				captureStderr(t, func() {
					_, seeded = detectVLLMSeats(p, box.runtime(), io.Discard)
					rendered = extraVLLMSeatsFor(sp, box.home, box.flags())
				})
				if seeded[fast.ID] != want || (len(rendered) == 1) != want {
					t.Errorf("venv=%v weights=%v wrappers=%v: the 35B is active=%v for the seed and %v for the render, want %v for both",
						venv, weights, wrappers, seeded[fast.ID], len(rendered) == 1, want)
				}
			}
		}
	}
}
