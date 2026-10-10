package core

import (
	"context"
	"testing"
)

type fixedProber struct{ v LaneVerdict }

func (p fixedProber) MediaLaneFree(context.Context, Request) LaneVerdict { return p.v }

// A runner that cannot answer the question (a test double, a node's own runner) reads as a free lane, so the
// router leaves the call exactly where it always was.
func TestProbeLaneIsFreeWhenTheRunnerCannotSay(t *testing.T) {
	for name, runner := range map[string]any{"nil": nil, "a runner with no probe": struct{}{}} {
		if v := ProbeLane(context.Background(), runner, Request{Task: TaskGenerateImage}); !v.Free || v.Why != "" || len(v.Holders) != 0 {
			t.Errorf("%s: want a plain yes, got %+v", name, v)
		}
	}
}

// A runner that can answer is asked, and its verdict comes back whole.
func TestProbeLaneAsksTheRunner(t *testing.T) {
	want := LaneVerdict{Free: false, Why: "card 2 held", Ahead: 1, Holders: []LaneHolder{{Epoch: 7, Class: "media", RemainingSec: 90}}}
	got := ProbeLane(context.Background(), fixedProber{want}, Request{Task: TaskGenerateImage})
	if got.Free || got.Why != want.Why || got.Ahead != 1 || len(got.Holders) != 1 || got.Holders[0].Epoch != 7 {
		t.Fatalf("the verdict must come back as the runner gave it: %+v", got)
	}
}

// The no-op handle takes the new method too: a lane may bounce on any handle.
func TestNopAttributionBounces(t *testing.T) {
	var h RemoteAttribution = NopAttribution{}
	h.Bounce(Deferf("held", "", Meta{ErrClass: ErrClassGPUBusy}))
}
