package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// The MCP doors hand mediaremote.Run runTaskAs as their runner. Overflow (ADR 0082) asks that runner whether the lane
// is free, so runTaskAs must answer with the server's pipeline and not leave the question to the default "free": a door
// that did would never move a busy call to an idle node. Without a pipeline there is nothing to ask and the lane reads as
// free, so the call goes through runTask exactly as before.
func TestTheMediaDoorsForwardTheLaneQuestionToThePipeline(t *testing.T) {
	s := New(nil)
	busy := core.LaneVerdict{Free: false, Why: "card(s) c2 held by a media-class lease", Ahead: 1}
	var asked []core.Request
	s.laneHook = func(_ context.Context, req core.Request) core.LaneVerdict {
		asked = append(asked, req)
		return busy
	}
	req := core.Request{Task: core.TaskGenerateImage, Input: "a red door", Params: map[string]any{"family": "qwen-image-2.1"}}
	got := core.ProbeLane(context.Background(), runTaskAs{s}, req)
	if got.Free || got.Why != busy.Why || got.Ahead != 1 {
		t.Fatalf("the verdict must come back as the pipeline gave it: %+v", got)
	}
	if len(asked) != 1 || asked[0].Params["family"] != "qwen-image-2.1" {
		t.Fatalf("the pipeline was asked about the door's request: %+v", asked)
	}
}

func TestAServerWithNoPipelineReadsAsAFreeLane(t *testing.T) {
	s := New(nil)
	if got := core.ProbeLane(context.Background(), runTaskAs{s}, core.Request{Task: core.TaskGenerateImage, Input: "p"}); !got.Free {
		t.Fatalf("no pipeline, no lane to ask about: %+v", got)
	}
}

// The runner reaches the REAL pipeline, not only the test seam: a server that holds a pipeline gets the pipeline's own
// verdict. The request is one the pipeline does not model (a video call), so it answers "not judged" at once, without
// reading a card table or a lease directory; a runner that returned "free" without asking would carry no such reason.
func TestTheRunnerAsksTheServersPipeline(t *testing.T) {
	s := New(pipeline.New(config.Config{}, nil, nil, nil))
	got := core.ProbeLane(context.Background(), runTaskAs{s}, core.Request{Task: core.TaskGenerateVideo, Input: "a slow pan"})
	if !got.Free || !strings.HasPrefix(got.Why, "not judged:") {
		t.Fatalf("the pipeline's own verdict must come back (free, with its reason): %+v", got)
	}
}
