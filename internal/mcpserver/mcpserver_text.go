// Text and vision tool handlers (summarize, classify, extract, triage, VQA) and the route schema fragments.
package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/textremote"
	"github.com/dmmdea/offload-harness/internal/visionremote"
)

func (s *Server) handleSummarize(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Text      string `json:"text"`
		MaxPoints int    `json:"max_points"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{}
	if in.MaxPoints > 0 {
		params["max_points"] = in.MaxPoints
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskSummarize, Door: "offload_summarize", Input: in.Text, Params: params}))
}

func (s *Server) handleClassify(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Text   string   `json:"text"`
		Labels []string `json:"labels"`
		Route  string   `json:"route"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	return result(s.textRun(ctx, core.Request{Task: core.TaskClassify, Door: "offload_classify", Input: in.Text, Params: map[string]any{"labels": in.Labels}}, in.Route))
}

func (s *Server) handleExtract(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Text   string         `json:"text"`
		Schema map[string]any `json:"schema"`
		Route  string         `json:"route"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	return result(s.textRun(ctx, core.Request{Task: core.TaskExtract, Door: "offload_extract", Input: in.Text, Params: map[string]any{"schema": in.Schema}}, in.Route))
}

func (s *Server) handleTriage(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Text     string `json:"text"`
		Question string `json:"question"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskTriage, Door: "offload_triage", Input: in.Text, Params: map[string]any{"question": in.Question}}))
}

func (s *Server) handleVQA(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Image    string `json:"image"`
		Question string `json:"question"`
		Route    string `json:"route"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	return result(s.visionRun(ctx, core.Request{Task: core.TaskVQA, Door: "offload_vqa", Image: in.Image, Params: map[string]any{"question": in.Question}}, in.Route))
}

// visionRun is the ONE call behind the three single-image vision tools
// (0.116.0): route "" / local runs in-process exactly as before, auto and
// remote go through visionremote — the placement rule and the wire live
// there so the CLI verbs share them.
func (s *Server) visionRun(ctx context.Context, req core.Request, route string) core.Result {
	return visionremote.Run(ctx, s.p.Cfg(), s.p, req, route)
}

// textRun is the ONE call behind offload_classify and offload_extract: the default route (and
// "local") runs the in-process pipeline exactly as before the route existed; auto and remote go
// through textremote, where the placement rule and the wire live (0.154.0).
func (s *Server) textRun(ctx context.Context, req core.Request, route string) core.Result {
	return textremote.Run(ctx, s.p.Cfg(), s.p, req, route)
}

// mediaRouteSchema is the `route` and `remotes` properties the five media doors share (offload_generate_image,
// offload_generate_video, offload_animate_character, offload_generate_audio, offload_run_graph; ADR 0077): one
// string so the five descriptions cannot drift. The default is auto, which runs here whenever this machine
// has the lane, so a caller that never passes it is unchanged on a render box.
const mediaRouteSchema = `"route":{"type":"string","enum":["local","auto","remote"],"description":"where the job runs (ADR 0077): auto (default; here when this machine has the lane, derived from the files its routes load, else a fleet node from delegate_remotes; a box with no lane and no fleet runs it here and gets the lane's own deferral), remote (always a fleet node; the input files you name travel to it in a hash-checked bundle and the output is fetched back and verified against the sha256 the node published), local (always this machine). meta.node and meta.placement say where it ran. out_dir (run_graph) is where the fetched outputs land on this machine (created if missing; it is never sent to the node). Not carried to a node: tts_voice, transformer and devices (those defer on a remote route). refine=false IS carried, to a node whose health says it honours it (any other node is a named miss). With a fleet configured, an auto IMAGE call or whole-node run_graph whose own lane is not free becomes a TICKET (ADR 0083): the harness holds the job and starts it on the first matching lane that frees anywhere, in order of arrival, either this machine's own or an idle node that renders the SAME recipe (an image: same weight files with their sizes and sampled content fingerprints, same sampling, strict; ADR 0082) under that node's own family name, or that holds every file the graph names (a graph: one card, this release; see that tool). The call answers inside its window (wait_s) with the ticket, its position and cluster[] saying per node why not; do not call again, a repeat of the identical request attaches to the same ticket and waiter_token=mq-... collects the result. A tk- waiter_token (a place in this machine's own line) is brought into the ticket. A graph that reads input files is a ticket that stays on this machine (a node can match such a file only by name); its answer says why"},"remotes":{"type":"array","items":{"type":"string"},"description":"fleet node base URLs for this call, tailnet-only (e.g. http://node-c:18811); each must be one of delegate_remotes, so a call can narrow the fleet and never extend it. Default: delegate_remotes"}`

// mediaNodeSchema is the `node` property the video, animation and audio doors take (ADR 0082 Amendment 3). The image and graph
// doors do not take it, so no door advertises a parameter it would drop.
const mediaNodeSchema = `"node":{"type":"string","description":"OPTIONAL. Name the ONE fleet node that runs this call: a node_id (the node_id its /fleet/health publishes, in any case) or one of the delegate_remotes URLs. The named node is the only candidate: this machine's own lane is not used (even when it has one), no other node is tried, and route local with a node is refused. A node that cannot take the call is refused before anything is sent, so no job is created on it, and data.refusal says why: node_unknown (lists the roster), node_is_self, node_lacks_route (it does not serve this lane, or its route is not configured, or its weights are gone), node_lacks_media_job (the call carries input files and the node has not opened that door), node_lacks_model (the node's media_recipes row for the family the call renders shows a weight file it lacks; a node that publishes no row for the lane is not judged and the call goes to its own door) or node_below_min_vram. A node that does not answer, or holds a text lease, is a capacity defer: it may take the call later. Two answers carry no data.refusal: a delegate_remotes entry that this machine's tailnet guard refuses to dial is a configuration fact (capacity, err_class no_node_configured), and a machine with no delegate_remotes at all gets the plain config defer. Without model the node renders its own default family; the answer says which (rendered_family, rendered_files) when the node publishes a row for it. Naming another build is your explicit act: pass model as well."}`

// mediaMinVRAMSchema is the `min_vram_gb` property the video and animation doors take, as the graph door does.
const mediaMinVRAMSchema = `"min_vram_gb":{"type":"number","description":"OPTIONAL. Only a fleet node whose LARGEST card is at least this many GB may take the call (a card counts as the size it rounds to: a 16 GB card reads 15.92 GiB). Filters nodes only; this machine's own lane is its own grant. 0 asks for nothing. A number of GB, 0 or more; anything else is refused by name."}`

// textRouteSchema is the `route` property offload_classify and offload_extract share. Adding it
// changed tools/list on every box (0.154.0): the default is local, so every caller that never
// passes it is unchanged.
const textRouteSchema = `"route":{"type":"string","enum":["local","auto","remote"],"description":"where the call runs (0.154.0): local (default; this box's own cascade, unchanged behaviour), auto (an idle local card runs it; when the machine-wide GPU lease is held — a render in flight or a text reservation — a fleet node advertising the text lane for this task runs it instead, and with no eligible node it still runs local), remote (force a fleet node; with none eligible it returns deferred:true with defer_class capacity — or config when no delegate_remotes are configured — and never touches the local GPU). The text lane is dark until a node's tier declares it (text_tasks in its health), so on today's fleets auto stays local and remote defers. The node runs its own pipeline and returns its full result; meta.node / meta.placement say where it ran"}`

// sttRouteSchema is the `route` property of offload_transcribe (ADR 0072). Unlike the vision and text
// tools its default is AUTO, not local: every fleet node serves the same whisper family, so a spill to a
// node costs no quality, while a transcription that waits behind a render holding the cards is the
// failure this route exists to remove (five 90 s refusals in one morning). A caller that wants the old
// behaviour passes route "local".
const sttRouteSchema = `"route":{"type":"string","enum":["local","auto","remote"],"description":"where the whisper model runs (ADR 0072). auto (the DEFAULT): this box's own whisper when the request would be served at once, and when a render or an exclusive hold would keep it waiting (a resident whisper is still served locally) a fleet node that advertises the stt upload door transcribes it instead; with no eligible node it still runs local. The default is auto because every node serves the same whisper family, so a spill costs no quality. local: only this box's whisper, unchanged behaviour; a held card then defers gpu_busy and the reason says a fleet node could take it. remote: force a fleet node; with none eligible it returns deferred:true with defer_class capacity (config when no delegate_remotes are configured) and never touches the local GPU. The audio is read on THIS box and sent as 16 kHz mono Opus (the original file when conversion is impossible); srt_path, text_path and json_path are always files written on THIS box. meta.node / meta.placement say where it ran. Does not apply under engine:npu, which is this box's own Hailo sidecar: a route you name there other than local is refused"}`

// visionRouteSchema is the `route` property every single-image vision tool
// carries; one string so the three descriptions cannot drift.
const visionRouteSchema = `"route":{"type":"string","enum":["local","auto","remote"],"description":"where the vision model runs (0.116.0): local (default; this box's vision seat, unchanged behaviour), auto (idle local card runs it; when the machine-wide GPU lease is held — a render in flight or a text reservation — or the local vision seat is busy — a request in flight, a load or unload in progress, or a load that would unload a loaded vLLM seat — the least-loaded fleet node advertising the vision lane runs it instead, and with no eligible node it still runs local), remote (force a fleet node; with none eligible it returns deferred:true with defer_class capacity — or config when no delegate_remotes are configured — and never touches the local GPU). The image is read on THIS box under vision_max_image_bytes and travels with the job; meta.node / meta.placement say where it ran"}`
