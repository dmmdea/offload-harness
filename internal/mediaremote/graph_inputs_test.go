package mediaremote

// A graph that reads input files stays off the fleet (graph.go), found by review of the first S4 round.
//
// INPUT FILES. A graph's input files (LoadImage, LoadVideo, LoadAudio) are the caller's own, with names two machines share
// with different bytes (image.png, ComfyUI_00001_.png [output]). A node can match one only by name, so a node holding a
// same-named file would run the graph on the wrong bytes and answer ok with a verified file. A graph that reads one is
// therefore never offered to a node: it is a LOCAL-ONLY ticket (ADR 0083 Amendment 1), one per request, keeping its place in
// this machine's line, and every answer it gives says why the fleet was not asked. A call that then runs here and succeeds
// says nothing about the fleet.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

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
			lr := newKeeperRunner(heldLane())
			res := runDoor(cfg, lr, graphReqOf(t, c.graph, nil), AtOnce)
			a := answerOf(t, res)
			time.Sleep(150 * time.Millisecond) // several scans
			untouched(t, "the node that holds a same-named file (the fleet is not even read)", n)
			if probes, runs := lr.counts(); probes < 1 || runs != 0 {
				t.Fatalf("the ticket asks its own lane and waits for it: probes %d runs %d (%s)", probes, runs, brief(res))
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
			if len(a.Cluster) != 0 {
				t.Errorf("no node was asked, so there is no per-node row: %+v", a.Cluster)
			}
		})
	}
}

// The note is on the ticket's answers and on no result: a graph that reads an input file whose local run then succeeds is the
// caller's answer as the lane gave it, with nothing added about the fleet and no node asked.
func TestAnInputFileGraphThatRunsHereAfterTheWaitCarriesNoFleetNote(t *testing.T) {
	n := startGraphNode(t, "node-b", []string{krea2File, "input/ref.png"}, 7.96)
	cfg := graphClient(t, n)
	lr := newKeeperRunner(heldLane())
	req := graphReqOf(t, refGraph, nil)
	answerOf(t, runDoor(cfg, lr, req, AtOnce))
	lr.set(core.LaneVerdict{Free: true}) // the lane frees up and the local run succeeds
	res := settle(t, cfg, lr, req, func(r core.Result) bool { return r.OK })
	if probes, runs := lr.counts(); probes < 1 || runs != 1 {
		t.Fatalf("the graph waited for its own lane and ran there: probes %d runs %d (%s)", probes, runs, brief(res))
	}
	if _, has := decode(t, res)["fleet_note"]; has || strings.Contains(res.Reason, "by name") {
		t.Errorf("a result carries no note about the fleet: %s", string(res.Data))
	}
	untouched(t, "the node", n)
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

// A call that must leave this machine anyway keeps the node's check by name (the node refuses at its door when it lacks the
// file): route remote, and auto on a machine with no graph lane of its own, which has no queue to wait in. A free lane keeps
// the call here without a word about the fleet.
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

// A thin client (no graph lane of its own) has no queue to wait in, so refusing would turn a placement into a refusal: its
// `auto` call goes to a node as it always did, checked by name at the node's door. The same-named-file risk stays on this
// path; it is a named limit and this test pins it as a decision.
func TestAThinClientPlacesAnInputFileGraphByTheNodesCheckOnly(t *testing.T) {
	holds := startGraphNode(t, "node-b", []string{krea2File, "input/ref.png"}, 7.96)
	cfg := graphClient(t, holds)
	stubLocalRoutes(t, configured()) // this machine has no run_graph route
	res := Run(context.Background(), cfg, &laneRunner{}, graphReqOf(t, refGraph, nil), "auto", nil)
	if !res.OK || res.Meta.Node != "node-b" || !strings.Contains(res.Meta.Placement, "no graph lane on this machine") {
		t.Fatalf("a machine with no lane places the graph on the node that holds its files: %s (%s)", brief(res), res.Meta.Placement)
	}

	lacks := startGraphNode(t, "node-c", []string{krea2File}, 7.96)
	cfg2 := graphClient(t, lacks)
	stubLocalRoutes(t, configured())
	res = Run(context.Background(), cfg2, &laneRunner{}, graphReqOf(t, refGraph, nil), "auto", nil)
	if res.OK || !strings.Contains(res.Reason, "ref.png") {
		t.Fatalf("the node's check by name refuses a file it lacks, naming it: %s", brief(res))
	}
}
