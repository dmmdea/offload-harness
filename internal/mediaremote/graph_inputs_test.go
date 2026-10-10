package mediaremote

// A graph that reads input files stays off the auto-overflow (graph.go), found by review of the first S4 round.
//
// INPUT FILES. A graph's input files (LoadImage, LoadVideo, LoadAudio) are the caller's own, with names two machines share
// with different bytes (image.png, ComfyUI_00001_.png [output]). A node can match one only by name, so a node holding a
// same-named file would run the graph on the wrong bytes and answer ok with a sha-verified file. A graph that reads one
// therefore never auto-overflows: it waits for its own lane, and the answer says why the fleet was not asked.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// brief is a result in one line: a failure message that dumps the whole struct buries what it is about.
func brief(res core.Result) string {
	return fmt.Sprintf("ok=%v deferred=%v node=%q reason=%.200q", res.OK, res.Deferred, res.Meta.Node, res.Reason)
}

const (
	// refGraph reads ref.png beside the weight file a node of these tests holds.
	refGraph = `{"3":{"class_type":"UNETLoader","inputs":{"unet_name":"krea2_turbo_bf16.safetensors"}},"5":{"class_type":"LoadImage","inputs":{"image":"ref.png"}}}`
)

func TestRunGraphThatReadsAnInputFileStaysOnThisMachine(t *testing.T) {
	weights := `"3":{"class_type":"UNETLoader","inputs":{"unet_name":"krea2_turbo_bf16.safetensors"}}`
	for name, c := range map[string]struct{ graph, shows string }{
		"an image":  {refGraph, "ref.png"},
		"an output": {`{` + weights + `,"5":{"class_type":"LoadImageOutput","inputs":{"image":"ref.png [output]"}}}`, "ref.png [output]"},
		"a video":   {`{` + weights + `,"5":{"class_type":"LoadVideo","inputs":{"file":"ref.png"}}}`, "ref.png"},
		"an audio":  {`{` + weights + `,"5":{"class_type":"LoadAudio","inputs":{"audio":"ref.png"}}}`, "ref.png"},
		"a list": {`{` + weights + `,"1":{"class_type":"LoadImage","inputs":{"image":"a.png"}},"2":{"class_type":"LoadImage","inputs":{"image":"b.png"}},` +
			`"4":{"class_type":"LoadImage","inputs":{"image":"c.png"}},"5":{"class_type":"LoadImage","inputs":{"image":"d.png"}},"6":{"class_type":"LoadImage","inputs":{"image":"e.png"}}}`, "a.png, b.png, c.png, and 2 more"},
	} {
		t.Run(name, func(t *testing.T) {
			// The node holds the weight file AND files of the same names: by name it would take the graph.
			n := startGraphNode(t, "node-b", []string{krea2File, "input/ref.png", "output/ref.png", "input/a.png", "input/b.png", "input/c.png", "input/d.png", "input/e.png"}, 7.96)
			cfg := graphClient(t, n)
			lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
			res := Run(context.Background(), cfg, lr, graphReqOf(t, c.graph, nil), "auto", nil)
			noPosts(t, "the node that holds a same-named file", n)
			if probes, runs := lr.counts(); probes != 1 || runs != 1 {
				t.Fatalf("the call waits for its own lane: probes %d runs %d (%s)", probes, runs, brief(res))
			}
			for _, want := range []string{c.shows, "by name"} {
				if !strings.Contains(res.Reason, want) {
					t.Errorf("the answer says why the fleet was not asked (%q is missing): %s", want, res.Reason)
				}
			}
			if strings.Contains(res.Reason, "d.png") {
				t.Errorf("a long list is cut after three names: %s", res.Reason)
			}
			if note, _ := decode(t, res)["fleet_note"].(string); !strings.Contains(note, c.shows) {
				t.Errorf("the note is in the answer's data: %v", decode(t, res))
			}
			if rows := clusterOf(t, res); len(rows) != 0 {
				t.Errorf("no node was asked, so there is no per-node row: %+v", rows)
			}
		})
	}
}

// The rule is about a file being read. An input loader fed by a link reads no file of its own, and a graph that reads
// nothing from disk overflows as before.
func TestRunGraphWhoseInputLoaderReadsNoFileOverflows(t *testing.T) {
	n := startGraphNode(t, "node-b", []string{krea2File}, 7.96)
	cfg := graphClient(t, n)
	graph := `{"3":{"class_type":"UNETLoader","inputs":{"unet_name":"krea2_turbo_bf16.safetensors"}},"5":{"class_type":"LoadImage","inputs":{"image":["9",0]}}}`
	res := Run(context.Background(), cfg, &laneRunner{verdict: heldLane()}, graphReqOf(t, graph, nil), "auto", nil)
	if !res.OK || res.Meta.Node != "node-b" {
		t.Fatalf("a graph that reads no input file goes to the idle node: %s", brief(res))
	}
}

// A caller who names the node by hand has chosen where the files are: the forced route keeps the check by name (the node
// refuses at its door when it lacks one), and a free lane keeps the call here without a word about the fleet.
func TestInputFilesAreOnlyAnAutoOverflowRule(t *testing.T) {
	n := startGraphNode(t, "node-b", []string{krea2File, "input/ref.png"}, 7.96)
	cfg := graphClient(t, n)
	res := Run(context.Background(), cfg, &laneRunner{}, graphReqOf(t, refGraph, nil), "remote", nil)
	if !res.OK || res.Meta.Node != "node-b" {
		t.Fatalf("route remote places the graph on the node it names: %s", brief(res))
	}

	n2 := startGraphNode(t, "node-c", []string{krea2File, "input/ref.png"}, 7.96)
	cfg2 := graphClient(t, n2)
	lr := &laneRunner{verdict: core.LaneVerdict{Free: true}, local: queuedLocal()}
	free := Run(context.Background(), cfg2, lr, graphReqOf(t, refGraph, nil), "auto", nil)
	if _, has := decode(t, free)["fleet_note"]; has || strings.Contains(free.Reason, "by name") {
		t.Errorf("a free lane says nothing about the fleet: %s", brief(free))
	}
	untouched(t, "the node", n2)
}
