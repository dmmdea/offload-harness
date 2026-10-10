package mcpserver

import (
	"context"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
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
