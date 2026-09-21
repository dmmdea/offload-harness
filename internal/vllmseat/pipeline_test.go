package vllmseat

import (
	"strings"
	"testing"
)

// pipelineFlagship is the 3-card agent seat the operator ordered on 2026-09-19 ("the 3 card tier as
// the agent seat now and the 2 card tier to be the opt in one"): the 27B across all three Qube cards
// as a pipeline, the 5070 Ti (card 1) LAST so it carries the lightest share, card 0 lighter than
// card 2 (card 2 has the better cooling), a fixed KV budget so the display card keeps room for the
// desktop. Values are illustrative of the shape; the measured operating point is pinned in the tier.
func pipelineFlagship() Spec {
	s := pinned()
	s.ID = "qwen3.8-27b-vllm-3card"
	s.Device = "0,2,1"
	s.TensorParallel = 0
	s.PipelineParallel = 3
	s.LayerPartition = "25,29,10"
	s.KVCacheMemoryBytes = 4026531840 // 3.75 GiB
	s.MaxModelLen = 262144
	return s
}

func TestPipelineSeatValidates(t *testing.T) {
	if err := pipelineFlagship().Validate("blackwell-3x16"); err != nil {
		t.Fatalf("the pipeline flagship must validate: %v", err)
	}
}

// TestPipelineSeatRendersItsLayout: the launcher (seat_fg.sh) already understood SEAT_PP and
// SEAT_PARTITION; nothing the tier table could write ever set them, which is why every 3-card seed
// fell back to a copy of the 2-card pair. The rendered env must now carry the whole layout.
func TestPipelineSeatRendersItsLayout(t *testing.T) {
	files, err := pipelineFlagship().Artifacts(wslTemplatesDir(), wslRT())
	if err != nil {
		t.Fatal(err)
	}
	env := files["qwen3.8-27b-vllm-3card.env"]
	for _, want := range []string{
		"SEAT_DEVICES=0,2,1",
		"SEAT_PP=3",
		"SEAT_PARTITION=25,29,10",
		"SEAT_MAX_LEN=262144",
		"--kv-cache-memory-bytes 4026531840",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("rendered env is missing %q", want)
		}
	}
}

// TestTensorSeatRendersAnEmptyPipeline: the launcher reads an EMPTY SEAT_PP as "tensor-parallel
// seat". A 2-card seat must keep rendering exactly that, or it would start as a pipeline.
func TestTensorSeatRendersAnEmptyPipeline(t *testing.T) {
	files, err := pinned().Artifacts(wslTemplatesDir(), wslRT())
	if err != nil {
		t.Fatal(err)
	}
	env := files["qwen3.8-27b-vllm.env"]
	for _, l := range strings.Split(env, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "SEAT_PP=") && strings.TrimSpace(l) != "SEAT_PP=" {
			t.Fatalf("a tensor-parallel seat rendered %q; it must be empty", strings.TrimSpace(l))
		}
	}
	if strings.Contains(env, "--kv-cache-memory-bytes") {
		t.Error("a seat with no fixed KV budget must not render --kv-cache-memory-bytes")
	}
}

func TestPipelineSeatRefusesAMismatchedLayout(t *testing.T) {
	cases := map[string]func(*Spec){
		"three cards but a two-stage pipeline":              func(s *Spec) { s.PipelineParallel = 2; s.LayerPartition = "32,32" },
		"a partition that lists the wrong number of stages": func(s *Spec) { s.LayerPartition = "32,32" },
		"a pipeline on the linux-systemd launch, which renders no pipeline flags": func(s *Spec) {
			s.Launch = LaunchLinuxSystemd
		},
	}
	for name, mutate := range cases {
		s := pipelineFlagship()
		mutate(&s)
		if err := s.Validate("blackwell-3x16"); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}
