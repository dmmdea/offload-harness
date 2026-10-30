// Package mcpserver exposes the offload pipeline as MCP tools over stdio so
// Claude Code can delegate grunt work. Tools return the full Result JSON as
// text — a defer is a valid result (Claude then does the task itself), not an
// error.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/accelclient"
	"github.com/dmmdea/offload-harness/internal/askcache"
	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/research"
	"github.com/dmmdea/offload-harness/internal/seatguard"
	"github.com/dmmdea/offload-harness/internal/sttremote"
)

// rescueFunc is the rescue a delegation hands the engine: the injected seam, else
// this server's pipeline (pipeline.RescueRepack: the lossless reading, then one re-pack completion on
// its own agent seat or, when that seat is cold, on the node that wrote the answer).
func (s *Server) rescueFunc() delegate.RescueFunc {
	if s.rescue != nil {
		return s.rescue
	}
	return s.p.RescueRepack
}

// fleetDispatch is delegate.RunWith's signature, named so the review lane can
// hold it behind a test seam. Deliberately the WHOLE engine and not a narrower
// "dispatch one contract to one node": placement, the ctx-fit gate, the
// re-placement loop and the telemetry all live inside it, and a lane that
// reached past them would be a second, unaudited router.
type fleetDispatch func(ctx context.Context, cfg config.Config, local delegate.LocalRunner, subtasks []core.AgentContract, route string, remotes []string, opts *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error)

type Server struct {
	p *pipeline.Pipeline
	// runHook is the seam the media doors run a request through: nil (production) is p.Run;
	// tests inject one to see exactly what a door hands the pipeline.
	runHook func(context.Context, core.Request) core.Result
	// templateExec is the seam offload_run_template reaches the node-side catalog through (render/run-template.mjs: list, get,
	// readiness, resolve): nil (production) runs the script on this machine's node runtime; tests inject the answers.
	templateExec func(ctx context.Context, args []string) ([]byte, error)
	// laneHook is the seam the lane question (core.LaneProber) is answered through: nil (production) is the pipeline's
	// MediaLaneFree, or "free" when there is no pipeline; tests inject one to see that the question reaches the pipeline.
	laneHook func(context.Context, core.Request) core.LaneVerdict
	// placeHook is the seam the place a queued call keeps in this box's line (core.PlaceKeeper) is kept through: nil
	// (production) is the pipeline, or "this box keeps none" when there is no pipeline.
	placeHook core.PlaceKeeper
	// configPath is the config file this server's config came from: the ticket queue re-reads its delegate_remotes when
	// the file changes, because a running server never reloads its config (WithConfigSource).
	configPath string
	// localAgent is the LOCAL execution seam shared by agent_delegate and offload_ask:
	// nil (production)
	// resolves to p.RunAgentContract at call time; tests inject a fake so the
	// handler is exercisable without a live planner.
	localAgent delegate.LocalRunner
	// researchFetch is offload_research's fetch seam (tests inject pages; nil =
	// research.FetchAll against the public web).
	researchFetch func(ctx context.Context, urls []string, opt research.Options) []research.Fetched
	// rescue is the seam of the delegator's rescue of a finished answer whose
	// structured re-pack failed (register C-66, PR-4): nil (production) resolves
	// to the pipeline's own RescueRepack at call time; tests inject a fake so a
	// delegation is exercisable without a live seat.
	rescue delegate.RescueFunc
	// reviewFleet is the FLEET dispatch seam of the review lane's fenced-seat
	// fallthrough (register D-110): nil (production) resolves to
	// delegate.RunWith at call time; tests inject a fake so the handler is
	// exercisable without a live fleet.
	reviewFleet fleetDispatch
	// reviewDiffFS is offload_review_diff's filesystem seam (reviewDiffIO): nil (production) is the real
	// working directory, filepath.Abs and delegate.InlineContextPaths; tests break one call at a time.
	reviewDiffFS *reviewDiffIO
	// askSeatGuard is offload_ask's seam onto the seat guard (askOccupant): nil
	// (production) resolves to the process-wide seatguard.Shared at call time.
	askSeatGuard func(ctx context.Context, model string) seatguard.Verdict
	// askNow is the clock offload_ask times its first attempt with, to size what is left of the
	// contract's wall for a cross-seat retry: nil (production) is time.Now; a test advances a
	// fake one inside its seat so a long first attempt needs no real wait.
	askNow func() time.Time
	// sttRun is offload_transcribe's placement seam (ADR 0072): nil (production) resolves to
	// sttremote.Run at call time; tests inject a fake so the handler is exercisable without a
	// whisper, an ffmpeg or a fleet node.
	sttRun func(ctx context.Context, cfg config.Config, runner sttremote.Runner, req core.Request, route string) core.Result
	// forecastRun is offload_forecast's placement seam: nil (production) resolves to forecastPlace at
	// call time; tests inject a fake so the handler is exercisable without a forecast seat.
	forecastRun forecastRunFunc
	// foreignFence is the agent_run door's lease-fence seam: nil (production)
	// resolves to delegate.ForeignFence at call time.
	//
	// It exists because the pre-check and the cordon below it share one
	// predicate — delegate.ForeignFence delegates to modelaffinity.BlocksNewRun,
	// which is exactly what AwaitRunSlot waits on — so with the real function a
	// hold that could time out at the cordon is precisely a hold the pre-check
	// already refused, and the cordon's own defer shape would be unreachable
	// from a test (measured across all eight lease shapes: the two agree on
	// every one; see TestForeignFenceAndTheCordonShareOnePredicate). The path
	// stays in the code for the race the two reads leave open — a lease taken
	// between them — and a seam is the only way to prove it reports honestly.
	foreignFence func(gpulease.Info) (bool, string)
	// quarantine remembers fleet nodes whose answers failed the document
	// fingerprint twice (delegate.Quarantine) for this server's lifetime, so
	// a research call's later chunks and later calls stop placing work there
	// until the TTL expires.
	quarantine *delegate.Quarantine
	// pins is the pin accounting of this server's lifetime (ADR 0078): every door that runs the engine hands it
	// over (RunOptions.PinTally), and offload_status publishes it in the fleet block, so its window is "since this
	// server started" by construction and a test server starts at zero.
	pins *delegate.PinTally
	// tenant identifies THIS server (one Claude session) to the fleet, so a
	// node's backlog round-robins across sessions (0.113.18). Fixed at New:
	// one process, one tenant, for its whole life.
	tenant string
	// configErr is the config VALIDATION error this server is running under, or
	// nil. It is not a startup failure on purpose: an MCP server that exits
	// removes every offload_* tool from every session with no message on any
	// surface the operator reads. So the server starts and carries the error —
	// offload_status publishes it as config_error and every other tool defers
	// naming it (see configGate). Set once by WithConfigError, read-only after.
	configErr error
	// hailo is the lazily-built accelerator lane (ADR 0024): one Sidecar shared
	// by every NPU tool so concurrent first calls share a single spawn.
	// accelSidecars: one on-demand Sidecar per listed accelerator (ADR 0024, Coral
	// D2/D5), shared by every tool on that device so concurrent first calls share
	// a single spawn. Was a single Hailo pair until the second device arrived.
	accelSidecars
	// askCache short-circuits an IDENTICAL repeat of an offload_ask call — same
	// question, same read_root, same file BYTES — with the answer the seat
	// already produced, so the 46-75 s of seat time is not spent twice.
	//
	// It lives on the Server rather than behind a caller-supplied session id
	// because the MCP server is spawned per client over stdio: one connection
	// is one process is one cache, born and destroyed with the connection.
	// That is exactly the scope a session_id argument would express, without
	// adding a required input to the one-call tool whose entire purpose is
	// having no arguments to think about. See internal/askcache.
	askCache *askcache.Cache

	// originSession names the session this server serves (the ledger's rule:
	// LOCAL_OFFLOAD_ORIGIN, else CLAUDE_CODE_SESSION_ID). A field so a test names its own
	// session instead of reading the developer's.
	originSession func() string

	// startup is the identity of the image this server started from (buildinfo.CaptureStartup, taken by Run), and the
	// source of offload_status's process block: the block is built from what the Server holds, never from the process
	// global, so a test injects a fixed identity and the test binary's own hash never reaches a pinned answer. nil
	// (a server that was not Run, the test seam) means the block is absent, as pair's is.
	startup *buildinfo.Startup
	// installed caches what the last moved installed image hashed to, so a status call reads the whole executable
	// only after its stat changed (stale_image.go).
	installed installedImages
	// exe is the stale-binary guard: the executable this server started from, compared with the file on disk when
	// a door whose behaviour a release changes answers (stalebinary.go).
	exe *exeWatch
}

func New(p *pipeline.Pipeline) *Server {
	return &Server{p: p, askCache: askcache.New(), quarantine: delegate.NewQuarantine(0), pins: delegate.NewPinTally(), tenant: delegate.DefaultTenant(),
		originSession: func() string { return ledger.ProcessOrigin().Session }, exe: newServerExeWatch()}
}

// WithConfigSource records the config file this server runs on, so a node added to its delegate_remotes joins the running
// session instead of waiting for a restart (internal/mediaremote: the ticket re-reads the file when its mtime changes). An
// empty path (built-in defaults) leaves the roster the server started with.
func (s *Server) WithConfigSource(path string) *Server {
	s.configPath = path
	return s
}

// WithConfigError records the config validation error this server is running
// under and returns the server, so the caller reads as one line. A nil error is
// the normal case and leaves every surface byte-identical: no config_error key,
// no gate installed, no behaviour changed.
func (s *Server) WithConfigError(err error) *Server {
	s.configErr = err
	return s
}

// configGate is the receiving middleware that keeps a server whose config failed
// validation HONEST without making it disappear.
//
// A refusal means the loader could not vouch for a value the tools are about to
// act on — typically an endpoint that dials nothing. Running the work anyway
// spends a wall to arrive at a dial timeout, which is the exact cost this change
// exists to remove; exiting instead takes the whole tool surface away with no
// message. So every tool but the discovery one defers, by name, and
// offload_status still answers because it is how the operator finds out WHY.
//
// It is installed only when there IS an error, so a healthy box runs the
// unmodified handler chain.
func (s *Server) configGate(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
		if !ok || params.Name == "offload_status" {
			return next(ctx, method, req)
		}
		res, err := jsonResult(map[string]any{
			"deferred": true,
			"reason":   "config invalid: " + s.configErr.Error(),
		})
		return res, err
	}
}

// parseArgs unmarshals the raw tool arguments into in. On a decode error it
// returns a non-nil {deferred:true, reason:"bad arguments: <err>"} result that
// the handler must return verbatim (LO-10: previously every handler did
// `_ = json.Unmarshal(...)`, silently running the tool on zero values — e.g. a
// wrongly-typed "text" became an empty input and produced a misleading
// "input too small to offload" defer). Absent/null arguments keep the prior
// zero-value behavior: required-field validation stays with the task itself.
// runTask runs a request through the pipeline, or the test seam.
func (s *Server) runTask(ctx context.Context, req core.Request) core.Result {
	// This door returns a queued media answer's waiter_token to its caller and takes it again on
	// the next call (withMediaPlace), so a place in line is worth keeping for its callers. No other
	// door does: the CLI verbs, the fleet dispatch and the image batch leave Resumable false.
	req.Resumable = true
	if s.runHook != nil {
		return s.runHook(ctx, req)
	}
	return s.p.Run(ctx, req)
}

// runTaskAs hands mediaremote.Run this server's runTask as its in-process runner, so a media door
// whose job stays on this machine goes through the same seam as every other door: Resumable is set
// and the test hook sees the request. The remote lane never reaches it.
type runTaskAs struct{ s *Server }

// mediaCfg is the config the route decision reads. A server built without a pipeline (the test seam) has
// none, which reads as a machine with no fleet: the job runs through runTask.
func (s *Server) mediaCfg() config.Config {
	if s.p == nil {
		return config.Config{}
	}
	return s.p.Cfg()
}

func (r runTaskAs) Run(ctx context.Context, req core.Request) core.Result {
	return r.s.runTask(ctx, req)
}

// BeginRemote makes the runner the remote lane's attributor: mediaremote asks its Runner for the handle
// that writes the asker's ledger row and PAIR card for a call sent to a node, and the server's pipeline is
// the one that does (without this the MCP doors would send the job and attribute nothing). A server with
// no pipeline (the test seam) returns nil, which core.BeginRemote turns into the no-op handle.
func (r runTaskAs) BeginRemote(req core.Request, route string) core.RemoteAttribution {
	if r.s.p == nil {
		return nil
	}
	return r.s.p.BeginRemote(req, route)
}

var _ core.RemoteAttributor = runTaskAs{}

// MediaLaneFree makes the runner the lane prober mediaremote asks before it sends a busy image call, or a busy whole-node
// graph, to an idle node of the fleet (ADR 0082): the server's pipeline answers it, read-only. A server built without a pipeline (the test seam) has no
// lane to ask about, and reads as free: the call goes through runTask as it always did.
func (r runTaskAs) MediaLaneFree(ctx context.Context, req core.Request) core.LaneVerdict {
	if r.s.laneHook != nil {
		return r.s.laneHook(ctx, req)
	}
	if r.s.p == nil {
		return core.LaneVerdict{Free: true}
	}
	return r.s.p.MediaLaneFree(ctx, req)
}

var _ core.LaneProber = runTaskAs{}

// LeaveMediaPlace, KeepMediaPlace and DropMediaPlace make the runner the place keeper mediaremote's ticket queue asks to keep a
// queued call's arrival time in this box's own line (ADR 0083). A server built without a pipeline keeps none.
func (r runTaskAs) LeaveMediaPlace(ctx context.Context, req core.Request, since time.Time) (core.MediaPlace, error) {
	if r.s.placeHook != nil {
		return r.s.placeHook.LeaveMediaPlace(ctx, req, since)
	}
	if r.s.p == nil {
		return core.MediaPlace{}, core.ErrNoMediaPlace
	}
	return r.s.p.LeaveMediaPlace(ctx, req, since)
}

func (r runTaskAs) KeepMediaPlace(token string) bool {
	if r.s.placeHook != nil {
		return r.s.placeHook.KeepMediaPlace(token)
	}
	return r.s.p != nil && r.s.p.KeepMediaPlace(token)
}

func (r runTaskAs) DropMediaPlace(token string) {
	if r.s.placeHook != nil {
		r.s.placeHook.DropMediaPlace(token)
		return
	}
	if r.s.p != nil {
		r.s.p.DropMediaPlace(token)
	}
}

var _ core.PlaceKeeper = runTaskAs{}

// withMediaPlace threads the waiter_token a queued media answer returned into the request's params,
// so the call resumes the place in line it left (internal/gpulease/tokens.go). Absent or blank is a
// new arrival, and params comes back untouched.
func withMediaPlace(raw json.RawMessage, params map[string]any) map[string]any {
	var in struct {
		WaiterToken string `json:"waiter_token"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &in) != nil {
		return params
	}
	tok := strings.TrimSpace(in.WaiterToken)
	if tok == "" {
		return params
	}
	if params == nil {
		params = map[string]any{}
	}
	params["waiter_token"] = tok
	return params
}

// laneTargeting puts the lane-targeting parameters of the video, animation and audio doors (ADR 0082 Amendment 3) into a request's
// params, only when the caller passed them: node (all three), min_vram_gb (video and animation; the audio door passes nil). A
// min_vram_gb of 0 is a number the caller gave and travels.
func laneTargeting(params map[string]any, node string, minVRAM any) {
	if node != "" {
		params["node"] = node
	}
	if minVRAM != nil {
		params["min_vram_gb"] = minVRAM
	}
}

func parseArgs(raw json.RawMessage, in any) *mcp.CallToolResult {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, in); err != nil {
		res, _ := jsonResult(map[string]any{"deferred": true, "reason": "bad arguments: " + err.Error()})
		return res
	}
	return nil
}

// Run serves the MCP tools on stdin/stdout until the client disconnects.
func (s *Server) Run(ctx context.Context, version string) error {
	s.holdStartup()
	return s.serve(ctx, version, &mcp.StdioTransport{})
}

// serve is Run over any transport. It puts this process in the session registry for as
// long as it serves: a GPU lease records which session asked for it, and a session is
// alive exactly while some registered process carries its id (gpulease.RegisterOwner).
// The entry is removed when the server stops (a client disconnect, a signal); a server
// that is killed leaves an entry whose pid reads as dead, which is the same answer.
func (s *Server) serve(ctx context.Context, version string, t mcp.Transport) error {
	defer s.registerSession()()
	return s.buildServer(version).Run(ctx, t)
}

// registerSession records this server's process under its session and returns the
// function that removes the record. Best effort and silent on the protocol (stdout is
// JSON-RPC): a registry that cannot be written costs the lease readers one fact, never the
// tool surface. A server with no session (a bare shell, a service) registers nothing.
func (s *Server) registerSession() func() {
	noop := func() {}
	if s.p == nil || s.originSession == nil {
		return noop
	}
	session := s.originSession()
	if session == "" {
		return noop
	}
	cfg := s.p.Cfg()
	dir, err := gpulease.LeaseDir(cfg.GPULockPath, cfg.StateDir)
	if err != nil {
		return noop
	}
	unreg, err := gpulease.RegisterOwner(dir, session, os.Getpid())
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-offload: session registry not written (%v); GPU leases taken for this session will read its owner as unknown\n", err)
		return noop
	}
	return unreg
}

// buildServer assembles the tool surface. Split from Run so the registration
// SET is testable over an in-memory transport — the delta-13 pin: tools/list
// must be byte-identical with agent_delegation_enabled off.
func (s *Server) buildServer(version string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "local-offload", Version: version}, nil)
	if s.configErr != nil {
		srv.AddReceivingMiddleware(s.configGate)
	}
	// After the gate, so it wraps outside it: every door's result, a config deferral included, carries the stale notice (stale_notice.go).
	srv.AddReceivingMiddleware(s.staleNotice)
	// tools_schema_profile=compact (toolsprofile.go) rewrites the ADVERTISED list only. Installed only for compact, so the
	// default configuration has no extra middleware and its tools/list is byte-identical to the list before the key existed.
	if s.p != nil && s.p.Cfg().ToolsSchemaProfileMode() == config.ToolsProfileCompact {
		srv.AddReceivingMiddleware(compactToolsMiddleware)
	}

	// Discovery FIRST (LO-18): before this tool existed, offload_nim was the only
	// tool that named or listed any model, so an agent inspecting the harness
	// concluded the text/LLM capability was NIM's cloud catalog and never
	// discovered the LOCAL model cascade every other tool runs on. offload_status
	// makes the local surface enumerable: configured roster + live served models
	// + which media engines this machine has + the (only) remote surface.
	srv.AddTool(&mcp.Tool{
		Name:        "offload_status",
		Description: "Discover this harness's capability — call this FIRST when inspecting what the harness can do. To size or place a contract, pass section:\"brief\" (fleet + one-line gpu_lease/local verdicts + per-card gpu_cards + process, the stale-image signal); section:<block> returns one block. Returns {local:{endpoint, roster{workhorse,agent,triage,escalation,reasoning,vision,ocr,stt,stt_hq,forecast,embed}, served_now[...] (live model ids from the LOCAL llama-swap endpoint)}, fleet:{delegation_enabled, local_agent_seat{model, loaded, ctx_tokens?}, nodes[{base, reachable, node_id, agent_enabled, agent_seat, agent_ctx_tokens, agent_seat_resident, queue_depth}], agent_capable_nodes, idle_agent_nodes} — the LIVE delegation roster, probed at call time: who the delegation seats are, their real context ceilings, and how deep their queues run}. Trust it over any rules file or written figure. idle_agent_nodes > 0 means paid-for capacity is sitting unused. fleet.pins counts, since this server started, the subtasks callers pinned to route local or remote by pin_reason, the hints (a local or remote route with no pin_reason) and how many of those placement overrode, media:{...this machine's configured generation engines}, remote:{nim_endpoint, nim_default_model, nim_key_present}, accelerators:{...} (present only when this box lists an accelerator device, e.g. hailo-8l: its endpoint, sidecar config, owned tools and a live health probe), pair:{mode, relay?, reason?} (present only with pair_workloads_enabled: how this box reports its PAIR cards, `local ingress` | `node-info fallback` | `relay` | `off`), process:{version, build_sha256, installed_version?, stale_image?, note?} (the image THIS server process started from - the version and build hash it stamps on its rows - and whether the installed build has moved on since: stale_image true means a deploy replaced the binary and this server still runs the old image, so reconnect it with /mcp to run the installed one; stale_image is absent, with a note, when the installed file cannot be read), gpu_lease:{held, verdict, activity, queue_with} — verdict is ONE WORD for what this box's cards are doing right now: working | held-working | held-idle (a lease held over idle cards: the holder is draining, queued, loading or stalled) | held-stalled (a progress contract exists and its file stopped moving) | held-orphaned (an attended lease whose owner has been gone past the grace) | held-overdue (the declared window ended, the holder still renews) | tree-orphan (the wrapper is gone, its job still holds the cards) | loaded-idle | busy-outside | stale-holder | free (the held-stalled, held-orphaned, held-overdue and tree-orphan verdicts lead the brief line in capitals and name the takeover command; nothing reclaims or kills a lease on them); activity carries the seat's in-flight count and load state, the registered agent runs (kind, pid, origin, step, tokens), a per-card utilization sample with the processes on them (gpu_processes; the Windows desktop on a display card, whose memory nvidia-smi cannot size, is one count per card in display_card_processes_unknown instead of dozens of rows), and the holder's command. Read verdict before concluding anything from \"held\"; a held card is queued behind with queue_with, never refused. Every offload_* tool except offload_nim and offload_browse runs LOCAL — the GPU roster or a listed accelerator (free, on-box, no cloud); offload_nim is the only remote MODEL surface, and offload_browse (opt-in, present only when configured) drives the operator's own local browser with typed choices from a loopback decision endpoint the operator runs. An empty roster entry means that capability defers on this machine. On a composite box (ADR 0052) local carries tier_profile, tiers and layers[] rows (spec + live occupancy + admissibility, including a dormant display layer the operator may enable), nodes[] carry layers[] when the node publishes them, and every agent_run/agent_delegate result carries `placed` {tier, layer, role, seat, devices, reason, evicts}.",
		InputSchema: statusInputSchema(),
	}, s.handleStatus)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_summarize",
		Description: "Summarize text on the LOCAL model cascade (free, on-box, no cloud — see offload_status for the live roster). THE FIRST DOOR for one text + one mechanical question (register A-102): seconds on the entry rung, automatic climb to the escalation and reasoning rungs on a margin, schema or grounding failure; agent_delegate is for multi-document read-and-reason, and a contract whose goal is to summarize one file is the wrong door. Use for bulk/low-judgment summaries to keep tokens out of your context. Returns {summary, bullets}; if it can't do it confidently it returns deferred:true and you should summarize it yourself. Triggers: summarize / tl;dr / gist / digest / recap / condense a doc, log, transcript, article, or thread.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"text to summarize"},"max_points":{"type":"integer","description":"max bullet points (default 5)"}},"required":["text"]}`),
	}, s.handleSummarize)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_classify",
		Description: "Classify text into one of the given labels on the LOCAL model cascade (free, on-box, no cloud). THE FIRST DOOR for one text + one label set (register A-102): seconds on the entry rung, automatic climb on a low decision margin; never write an agent_delegate contract for a single classification. Returns {label, confidence}; low-confidence results are deferred back to you. Triggers: classify / categorize / label / tag / bucket / route text into one of a known set.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"text to classify"},"labels":{"type":"array","items":{"type":"string"},"description":"allowed labels (>=2)"},` + textRouteSchema + `},"required":["text","labels"]}`),
	}, s.handleClassify)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_extract",
		Description: "Extract structured fields from text on the LOCAL model cascade (free, on-box, no cloud), constrained to the provided JSON schema. THE FIRST DOOR for one text + one schema (register A-102): seconds on the entry rung, grounding-checked, automatic climb on failure; agent_delegate is for extraction that needs reading across several documents. Returns the extracted object or defers. Triggers: extract / parse / pull out structured fields from text into a schema (names, dates, amounts, entities).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"text to extract fields from"},"schema":{"type":"object","description":"JSON schema with a properties object describing the fields to extract"},` + textRouteSchema + `},"required":["text","schema"]}`),
	}, s.handleExtract)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_triage",
		Description: "Answer a yes/no/unsure question about text on the LOCAL model cascade (free, on-box, no cloud). THE FIRST DOOR for one text + one yes/no question (register A-102): seconds on the entry rung, automatic climb on a low decision margin; never write an agent_delegate contract for a single check. Returns {decision, reason} or defers. Triggers: a yes/no/unsure check on text — 'does this contain X?', 'is this relevant/spam/safe?', 'should this be flagged?'.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"text to evaluate"},"question":{"type":"string","description":"a yes/no question about the text"}},"required":["text","question"]}`),
	}, s.handleTriage)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_vqa",
		Description: "Answer a question about an IMAGE on a free local vision model (VQA). image is a local file path or a data:image/... URI; question is what to ask about it. Returns {answer}; if it can't answer confidently it returns deferred:true and you should look at the image yourself. route (default local) can place the call on a fleet node's vision seat over the tailnet when this box's GPU is busy (auto) or unconditionally (remote); meta.node / meta.placement report where it ran.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local image file path or a data:image/...;base64 URI"},"question":{"type":"string","description":"the question to answer about the image"},` + visionRouteSchema + `},"required":["image","question"]}`),
	}, s.handleVQA)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_video_describe",
		Description: "Answer a question about a VIDEO on a free local vision model. It samples frames from the video and reasons over them. video is a LOCAL file path; question is what to ask. Returns {answer} (which notes what the relevant frames show); if it can't answer confidently it returns deferred:true and you should watch the video yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"video":{"type":"string","description":"local video file path"},"question":{"type":"string","description":"the question to answer about the video"}},"required":["video","question"]}`),
	}, s.handleVideoDescribe)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_video_watch",
		Description: "WATCH a VIDEO END TO END on a free local vision model and answer a question about the whole thing. It splits the file into time windows (default 8 s), samples every window at fps (default 1 frame/s, i.e. one frame per second of the ENTIRE video), runs each window through the vision seat with absolute timestamps, then synthesizes one answer on the text seat. Use this instead of offload_video_describe whenever the video is longer than a few seconds or the answer depends on something not in the first seconds (shot list, continuity, defects at a given time, on-screen text over time). Returns {answer, duration_sec, windows_total, windows_deferred, frames_total, windows[{start,end,frames,notes|deferred,reason}]} — the per-window notes cite seconds. Slower than video_describe (one vision call per window); scale window_sec/fps for very long files. video is a LOCAL file path.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"video":{"type":"string","description":"local video file path"},"question":{"type":"string","description":"the question to answer about the whole video"},"window_sec":{"type":"number","description":"seconds per window (default 8)"},"fps":{"type":"number","description":"frames sampled per second inside each window (default 1)"},"max_frames":{"type":"integer","description":"cap on frames per window (default: the box's video_max_frames)"},"frame_width":{"type":"integer","description":"frame width in px sent to the model (default: the box's video_frame_width; raise for small text)"},"start":{"type":"number","description":"start second (default 0)"},"end":{"type":"number","description":"end second (default: end of file)"},"synthesize":{"type":"boolean","description":"also produce the final answer on the text seat (default true); false returns the per-window notes only"}},"required":["video","question"]}`),
	}, s.handleVideoWatch)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_transcribe",
		Description: "Transcribe a local AUDIO or VIDEO file to text on a free local whisper model (STT). audio is a LOCAL file path (mp3/m4a/wav/mp4/...); language is optional ('en','es', or 'auto' — default auto-detect); set hq=true for the higher-quality (slower) model on hard/noisy clips. Returns {gist (preview), language, duration_sec, num_segments, segments[{id,start,end,text}] (timestamped spans — pull only the ones you need), srt_path, text_path, json_path} plus, when the transcript carries word times (segments[].words), words_reliable and words_drift_sec: words_reliable false means the first word of some segment starts 0.5 s or more away from its segment (up to words_drift_sec seconds), so do not cut audio at word boundaries from those times. The full transcript + SRT are written to disk; read the spans/paths you need. route (default auto) places the work: this box's whisper when it is free, a fleet node's when a render or an exclusive hold would keep it waiting (every node serves the same whisper family, so that costs no quality; the audio is sent as 16 kHz mono Opus and the transcript files are written HERE, on this box); local pins it to this box, remote forces a fleet node. If it can't transcribe confidently it returns deferred:true and you should handle the audio yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"audio":{"type":"string","description":"local audio or video file path"},"language":{"type":"string","description":"en, es, or auto (default auto-detect); ignored by an openai-protocol hq tier (mtmd ASR detects language itself)"},"hq":{"type":"boolean","description":"use the configured higher-accuracy STT tier (slower) for hard/noisy/multilingual audio; note the accuracy tier may return a single full-span segment instead of timestamps. Does not apply under engine:npu (one NPU model)"},` + sttRouteSchema + `,"engine":{"type":"string","enum":["gpu","npu"],"description":"gpu (default): the local whisper seat (quality path, timestamps, long-form); npu: whisper-base on the Hailo-8L accelerator — fast preview tier (5 s chunks, no timestamps) WHERE the platform runs it; on Windows HailoRT 4.24 boxes the sidecar returns a typed platform-blocked diagnosis instead (whisper HEFs are Linux-validated upstream), so gpu remains the STT path there"},"select":{"type":"array","items":{"type":"string"},"description":"optional: return ONLY these top-level result fields (e.g. [\"gist\",\"language\",\"num_segments\",\"srt_path\"]) to skip the verbose segments[] and keep your context lean — read the full transcript/spans from srt_path or json_path when you need them"}},"required":["audio"]}`),
	}, s.handleTranscribe)
	srv.AddTool(&mcp.Tool{
		Name:        "offload_forecast",
		Description: "Forecast time series on a free local forecasting model (Timestral-209M: zero-shot, probabilistic, any sampling frequency). Give series inline or series_path (a LOCAL .csv or .json file); prediction_length, the steps to forecast ahead, is required. Returns {model, prediction_length, context_length_used, shape, quantile_levels, median, quantiles{level:[...]}, wall_ms, device} with one forecast per input series, in the input's shape. A result of more than 2,000 numbers is written as JSON to out_path (default <media_dir>/forecast-<sha8>.json) and the answer keeps shape, quantile_levels, result_path and the first and last 8 median steps of each series. route (default auto) runs it on this box's forecast model when it has one and its card is free, and on a fleet node's otherwise (every node serves the same model, so that costs no quality; the series travels with the job and the result file is written HERE, on this box); local pins it to this box, remote forces a fleet node. If the forecast can't be run it returns deferred:true with a reason (a bad input names what to fix) and you should handle the series yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"series":{"type":"array","items":{},"description":"the numbers to forecast inline: [1,2,3] is one series, [[...],[...]] several series, [[[...]]] series x variates x time; null marks a missing value; omit when series_path is given"},"series_path":{"type":"string","description":"LOCAL .csv file (a header row, then one row per step) or .json file (an array of numbers like series, or an object with forecast keys); omit when series is given"},"target_columns":{"type":"array","items":{"type":"string"},"description":"CSV only: the header names of the columns to forecast (default: every numeric column that is not a covariate); one column is one series, several are independent series, and a non-numeric column such as a date is ignored"},"past_covariate_columns":{"type":"array","items":{"type":"string"},"description":"CSV only: header names of columns known over the history only, used as past covariates"},"known_covariate_columns":{"type":"array","items":{"type":"string"},"description":"CSV only: header names of columns known over the history and the horizon, used as known covariates; the CSV must end with prediction_length rows whose target cells are empty, which carry their future values"},"prediction_length":{"type":"integer","minimum":1,"description":"steps to forecast ahead; in a CSV the trailing rows with every target cell empty are the horizon and must number this"},"quantiles":{"type":"array","items":{"type":"number"},"description":"quantile levels to return, each one of the model's 21 levels from 0.01 to 0.99; default [0.1,0.5,0.9]"},"past_covariates":{"type":"array","items":{},"description":"inline covariates known over the history only: (C,T) rows for one series, (B,C,T) for several"},"known_covariates":{"type":"array","items":{},"description":"inline covariates known over the history and the horizon: (C,T+prediction_length) rows for one series, (B,C,T+prediction_length) for several"},"out_path":{"type":"string","description":"where to write a result of more than 2,000 numbers (default <media_dir>/forecast-<sha8>.json)"},` + forecastRouteSchema + `,"select":{"type":"array","items":{"type":"string"},"description":"return ONLY these top-level fields of the result, such as median, quantile_levels or result_path, to keep your context lean"}},"required":["prediction_length"]}`),
	}, s.handleForecast)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_extract_image",
		Description: "Extract structured fields from an IMAGE on a free local model: it OCRs the image, then extracts the fields from the transcribed text constrained to the provided JSON schema (values are grounded against the OCR text). image is a local file path or a data:image/... URI; schema is a JSON schema with a properties object. Returns the extracted object or defers.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local image file path or a data:image/...;base64 URI"},"schema":{"type":"object","description":"JSON schema with a properties object describing the fields to extract"}},"required":["image","schema"]}`),
	}, s.handleExtractImage)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_assess_image",
		Description: "QA a generated IMAGE against hard exclusions on a free local vision model. Emits a grammar-constrained {has_people, has_text, matches_brief, notes}: has_people=true if any person/face/body part is visible, has_text=true if any readable letters/words/numbers are rendered, matches_brief=whether it matches the optional brief (true if no brief), notes=one short phrase. image is a local file path or a data:image/... URI; brief is optional. Returns the object or deferred:true. route (default local) can place the call on a fleet node's vision seat over the tailnet when this box's GPU is busy (auto) or unconditionally (remote) — the way to QA a batch of renders while the local cards are rendering; meta.node / meta.placement report where it ran.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local image file path or a data:image/...;base64 URI"},"brief":{"type":"string","description":"optional description the image should match"},` + visionRouteSchema + `},"required":["image"]}`),
	}, s.handleAssessImage)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_ocr",
		Description: "Transcribe ALL text in an IMAGE on a free local vision model (OCR). image is a local file path or a data:image/... URI. Returns {text} with the transcribed text in reading order; if it can't transcribe confidently it returns deferred:true and you should read the image yourself. route (default local; engine gpu only) can place the call on a fleet node's vision seat over the tailnet when this box's GPU is busy (auto) or unconditionally (remote); meta.node / meta.placement report where it ran.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local image file path or a data:image/...;base64 URI"},"engine":{"type":"string","enum":["gpu","npu"],"description":"gpu (default): the local vision model; npu: the Hailo-8L PaddleOCR path when this box has the accelerator (fast batch transcription)"},` + visionRouteSchema + `},"required":["image"]}`),
	}, s.handleOCR)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_generate_image",
		Description: "Generate an IMAGE from a text prompt on THIS machine's LOCAL image engine for FREE — no cloud, runs on the local GPU, using its configured model at its highest-quality settings (the engine is ComfyUI or stable-diffusion.cpp per machine; offload_status media.routes reports which one is bound here — e.g. HiDream-O1 bf16 at native 2048 via its official graph, SDXL on smaller boxes). QUALITY-FIRST: renders can take many minutes — that is intended; do not lower steps/resolution to speed things up unless the caller explicitly asks for a draft. prompt is required (prose sentences beat tag lists on DiT models; quoted text renders as literal text); optional: negative (active on models served with real CFG), width/height (default = the model's native resolution), steps, seed, out. It takes the shared single-slot GPU lock (and, on the ComfyUI engine, auto-starts ComfyUI), so it serializes with other local gen/inference and may wait. Where this machine configures a prompt-refiner model (imagegen_refiner_model), the prompt is first expanded with photographic detail on the free local text tier. Double-quoted text spans (straight or curly quotes) are guarded: if the refined text drops or alters one, or adds new quoted text, the raw prompt is rendered instead — same fallback as on any refiner error, recorded in the result as refine_fallback. The result then carries refined (plus refined_prompt when true); set refine=false to render your prompt verbatim. Caveat: unpaired quote marks used as inch marks can pair into unintended spans and force the (safe) raw-prompt fallback — spell out inches when a prompt also quotes text. NAMED FAMILIES: family selects one of this machine's opt-in bindings (offload_status media.image_families lists them); omit it for the default binding, which is what every call without it renders. transparent=true keeps an alpha channel (only a qwen-image-2.1 family has an RGBA VAE; any other binding defers rather than render opaque). Returns {image_path, width, height, seed, family} plus license/commercial_use when the binding declares a license — width/height are MEASURED from the written file (a family snaps sizes, e.g. qwen-image-2.1 floors to /32 and defaults to 2048x2048). On any failure (unknown family, transparent on a family without alpha, render error) it returns deferred:true — then generate the image another way.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"waiter_token":{"type":"string","description":"OPTIONAL. The waiter_token a QUEUED answer of this tool returned (err_class gpu_queued). mq-...: the ticket the harness is holding for this request, which starts on the first matching lane that frees anywhere; do NOT call again to wait for it (a repeat of the identical request attaches to the same ticket), and send the SAME request with waiter_token=mq-... only to collect the result. tk-...: a place in this machine's own line; re-send the SAME request with it within 10 minutes to resume that place, and the call brings the place into the cluster queue. Omit it on a first call."},"prompt":{"type":"string","description":"positive text prompt describing the image"},"negative":{"type":"string","description":"hard exclusions, e.g. people, text, watermark"},"out":{"type":"string","description":"output PNG path (optional; default under the media dir)"},"width":{"type":"integer","description":"width px (default 1024)"},"height":{"type":"integer","description":"height px (default 1024)"},"steps":{"type":"integer","description":"sampler steps (default: the machine binding's, e.g. 16 for flux3-klein)"},"seed":{"type":"integer","description":"RNG seed for reproducibility"},"refine":{"type":"boolean","description":"set false to skip this machine's opt-in prompt refiner and render the prompt verbatim (default: refine when a refiner model is configured; no-op otherwise)"},"family":{"type":"string","description":"OPTIONAL named image family (offload_status media.image_families); omit for this machine's default binding"},"transparent":{"type":"boolean","description":"keep an alpha channel (RGBA PNG with a transparent background; the prompt is wrapped in the model's official RGBA template). Only a qwen-image-2.1 family supports it; default false = opaque RGB"},"wait_s":{"type":"integer","description":"OPTIONAL. How many seconds this call waits for its job before it answers 'queued' with the ticket (default: this machine's gpu_wait_ms, 90 s; 0 = answer after the ticket's first look; at most 3600). The job goes on when the call returns; keep it under your client's tool-call timeout."},` + mediaRouteSchema + `},"required":["prompt"]}`),
	}, s.handleGenerateImage)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_run_graph",
		Description: "Execute an arbitrary ComfyUI API-format graph on this machine's ComfyUI (or, with a fleet configured and route auto or remote, on a fleet node's), satisfying a per-workflow node manifest (custom node packs from allowlisted repositories + model files) first. Generic: the caller owns ALL graph semantics — the harness passes the graph opaquely, provisions its environment, runs it under the shared single-slot GPU lock, and returns node-addressed outputs. Provide the graph as graph_path (a file) OR graph_json (inline API-format JSON); optionally manifest_path/manifest_json (the node manifest), out_dir (where output files land), reserve_vram (ComfyUI VRAM held back for the display). Returns {outputs:{node_id:[{path,type,kind}]}, image_path (first image, convenience alias), unverified_models[]}. On ANY failure it returns deferred:true with a typed reason (SATISFIER_UNAVAILABLE, VENV_INCOHERENT, SATISFIER_SPAWN_FAILED [a provisioning subprocess failed to START, retried once — transient/retryable, NOT a venv problem], NODE_CLASS_MISSING, PREFLIGHT_MISSING_INPUTS, MODEL_SHA_MISMATCH, GPU_BUSY, TIMEOUT, ...) — it NEVER falls back to cloud; then run the graph another way. ON A FLEET (ADR 0082 Amendment 1, ADR 0083 Amendment 1): with route auto and this machine's lane not free, a graph that names no devices becomes a TICKET: the harness holds it and starts it on the first matching lane that frees anywhere, in order of arrival, either this machine's own or an idle fleet node that holds the files the graph names. The call answers inside its window (wait_s, default this machine's gpu_wait_ms) with the ticket, its position and cluster[] saying per node why not; do not call again: a repeat of the identical graph (the same content, whatever file it is read from) attaches to the same ticket, and waiter_token=mq-... collects the result. The ticket keeps its own copy of the graph and the manifest as sent, so a file you overwrite or remove after the call (the next shot, written over the same path) changes nothing the ticket runs, and the token still collects it. Any route that places it on a node holds it to three rules, and the node must also run this machine's own harness release (its file check ships with the release; a node on another release, or one that publishes none, is skipped by name). The node must hold every model file the graph loads (any string input, under any key except filename_prefix, whose value ends .safetensors, .gguf, .pth, .pt, .ckpt or .sft and does not contain ://, found under any class directory of its model roots) and every file its input loaders read (LoadImage, LoadImageMask, LoadImageOutput, LoadVideo, LoadAudio; input files do not travel with a graph), and it refuses with 503 naming what it lacks, which marks that node not-capable for this ticket alone (it is not full, and it is not left alone for anyone else) and moves the ticket on to the next lane; a model the manifest fetches (a models entry with a source_url) is not refused. A graph that reads input files is a ticket that stays on this machine, because a node can match such a file only by name and not by content: it keeps its place and its position, is offered to no node, and every answer it gives says the fleet was not asked and why (fleet_note). A call that must leave this machine anyway (route remote, or auto on a machine with no graph lane of its own) is still placed, with the node's check by name only. The node must have exactly one card: a whole-node graph runs in the default ComfyUI instance, which sees every card, so it is never placed on a multi-card node (the display card could be the one it lands on), and a node that lists no cards is not assumed to have one. And, when min_vram_gb is given, the largest card must be at least that many GB. A graph that names devices (the operator's card-scoped graph) is no ticket: it waits in this machine's own line and is never offered to a node.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"waiter_token":{"type":"string","description":"OPTIONAL. The waiter_token a QUEUED answer of this tool returned (err_class gpu_queued). mq-...: the ticket the harness is holding for this graph, which starts on the first matching lane that frees anywhere; do NOT call again to wait for it (a repeat of the identical graph attaches to the same ticket), and send the SAME request with waiter_token=mq-... only to collect the result. tk-...: a place in this machine's own line; re-send the SAME request with it within 10 minutes to resume that place, and the call brings the place into the cluster queue. Omit it on a first call."},"wait_s":{"type":"integer","description":"OPTIONAL. How many seconds this call waits for its job before it answers 'queued' with the ticket (default: this machine's gpu_wait_ms, 90 s; 0 = answer after the ticket's first look; at most 3600). The job goes on when the call returns; keep it under your client's tool-call timeout."},"devices":{"type":"array","items":{"type":"string"},"description":"For the operator's use, optional. The card this graph may use (nvidia-smi index or GPU uuid prefix). Absent, or more than one, = the whole node, the default and the safe choice: the graph owns its own placement, so nothing else can know which cards it will use, and in the default instance it sees every card. One device runs in that card's own ComfyUI instance, which sees no other card."},"graph_path":{"type":"string","description":"path to a ComfyUI API-format graph JSON file (provide this or graph_json)"},"graph_json":{"type":"string","description":"inline ComfyUI API-format graph JSON (alternative to graph_path)"},"manifest_path":{"type":"string","description":"path to a node manifest JSON (custom node packs by repository + model files to provision)"},"manifest_json":{"type":"string","description":"inline node manifest JSON (alternative to manifest_path)"},"out_dir":{"type":"string","description":"directory for the graph's output files (optional; default under the media dir)"},"reserve_vram":{"type":"string","description":"ComfyUI --reserve-vram override (VRAM held back for the display; per-workflow)"},"min_vram_gb":{"type":"number","description":"OPTIONAL. Only a fleet node whose LARGEST card is at least this many GB may take the graph (a card counts as the size it rounds to: a 16 GB card reads 15.92 GiB). Filters nodes only; this machine's own lane is its own grant. Absent: a graph that names model_family wan22 and carries a height of 720 or more (the tallest height input of any node) gets a floor of 16, CHOSEN from the film owner's stated constraint and not measured. 0 lifts that default."},"model_family":{"type":"string","description":"OPTIONAL. The family this graph belongs to (wan22, wan2.2 or wan for Wan 2.2; also keys the footprint record). Only the default min_vram_gb floor reads it; the harness reads no other meaning into a graph."},` + mediaRouteSchema + `}}`),
	}, s.handleRunGraph)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_run_template",
		Description: "Look up or run a ComfyUI workflow template from this machine's installed package: list (filterable), get (manifest and licence class), readiness (models, nodes, licence verdict) or run (converted on this machine's ComfyUI and run through the offload_run_graph path). A run is allowed only for a non-API template with no custom node pack and a permissive licence class: a paid-API template returns deferred:true with class api_paid and never falls back to cloud, and a template with a blocked, conditional, non-commercial or unlisted licence is refused with its class. It never downloads a model: a run with a model file or an input file missing is refused with the exact list. A prompt embedding the harness cannot verify (a file of the name in another case, more than one file that could answer it, or a file that answers another name the prompt may give) is refused with the class embedding_unverified, and nothing is run on a guess. Every run stays on this machine's lane, and a template that reads input files is never sent to the fleet. A model file with no published hash is trusted on first use: its sha256 is recorded when a run first meets it, and a later run refuses a file whose hash differs.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["list","get","readiness","run"]},"template":{"type":"string","description":"Template name from list; required for get, readiness and run."},"kind":{"type":"string","description":"list filter: local, api or custom_nodes."},"tag":{"type":"string","description":"list filter: one of a template's tags, such as Image or Text to Image."},"text":{"type":"string","description":"list filter: words in a template's name, title or description."},"license_class":{"type":"string","description":"list filter: the licence class (permissive, conditional, non_commercial, unknown or none)."},"runnable":{"type":"boolean","description":"list filter: true (the default) lists the templates this release can run, false the refused ones."},"reads_input":{"type":"boolean","description":"list filter: true lists the templates that read input files, false those that do not."},"limit":{"type":"integer","minimum":1,"maximum":500,"description":"list: page size, 50 by default."},"offset":{"type":"integer","minimum":0,"description":"list: how many matches to skip."},"overrides":{"type":"object","additionalProperties":{"type":["string","number","boolean"]},"description":"run: input values to set after conversion, keyed <node id>.<input name> (a subgraph node is <instance>:<node>)."},"seed":{"type":"integer","minimum":0,"description":"run: pins the template's seed inputs."},"out_dir":{"type":"string","description":"run: directory for the output files (default under the media dir)."},"reserve_vram":{"type":"string","description":"run: ComfyUI --reserve-vram override (VRAM held back for the display)."},"waiter_token":{"type":"string","description":"run: the tk-... place a busy-lane answer returned; send the same call with it to resume that place."}},"required":["action"]}`),
	}, s.handleRunTemplate)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_generate_svg",
		Description: "Render a crisp, brand-agnostic data-viz SVG locally for FREE (no model, no GPU) — the right tool for precise diagrams/icons SDXL fakes badly. kind is one of: gauge, comparison-bar, chromatogram, icon. spec is the component's JSON (colors/data are inputs; defaults are neutral — pass a theme {fg,bg,accent,muted,font} to brand it). Optional out = .svg path (default under the svg dir). Examples — gauge: {\"value\":72,\"max\":100,\"label\":\"Purity\",\"unit\":\"%\"}; comparison-bar: {\"items\":[{\"label\":\"A\",\"value\":10},{\"label\":\"B\",\"value\":20}],\"highlight\":1}; chromatogram: {\"peaks\":[{\"rt\":2.5,\"height\":80,\"label\":\"API\"}]}; icon: {\"name\":\"check\",\"color\":\"#22c55e\"}. Returns {svg_path, width, height}. Defers only on a bad kind/spec.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string","description":"gauge | comparison-bar | chromatogram | icon"},"spec":{"type":"object","description":"the component spec (see description for fields; include an optional theme object to set colors)"},"out":{"type":"string","description":"output .svg path (optional; default under the svg dir)"}},"required":["kind","spec"]}`),
	}, s.handleGenerateSVG)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_generate_video",
		Description: "Animate a still image into a short b-roll VIDEO clip on the LOCAL ComfyUI for FREE. The graph family comes from THIS machine's videogen_family binding (the 2x16 reference box is bound to LTX-2.5, which generates joint AAC audio); pass model ONLY to override that deliberately — no cloud, runs on the local GPU. QUALITY-FIRST DEFAULT: the native two-stage recipe (no distill LoRA, 20 steps, cfg 3.5, the model's official negative) — a render takes tens of minutes and that is intended; set fast=true ONLY when the caller explicitly wants a draft (8-step lightx2v distill — visibly weaker motion). still (a local image path) + prompt describe the motion (prose, one camera move, ~80-120 words works best); optional: model (h3|hunyuan|wan), frames (16fps; 81 ≈ 5s is the native ceiling), width/height (per-machine config may default 720p), steps, seed, negative (defaults to the model's official training negative), reserve_vram, out. It auto-starts ComfyUI and takes the shared single-slot GPU lock, so it serializes with other local gen/inference and may wait for the slot before deferring. Returns {video_path, seed} plus license/commercial_use when the family declares them. On any failure it returns deferred:true — then make the clip another way.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"waiter_token":{"type":"string","description":"OPTIONAL. The waiter_token a QUEUED answer of this tool returned (err_class gpu_queued: every card was busy for the whole wait, so the call kept a place in line instead of failing). Re-send the SAME request with it within 10 minutes to resume that place with the arrival time it had; omit it on a first call."},"prompt":{"type":"string","description":"prose motion prompt (one camera move, ~80-120 words works best)"},"still":{"type":"string","description":"local path to the input still image (I2V)"},"model":{"type":"string","description":"OPTIONAL family override — omit to use this machine's configured videogen_family, which is the seated verdict. ltx25 (LTX-2.5 22B distilled, joint AV) | h3 (MiniMax-H3 joint AV — the T4-verdict opt-in: strongest prompt adherence/multi-shot direction and audio design, ~2x LTX wall; still is OPTIONAL for h3 — omit it for t2v storyboard direction) | wan (Wan 2.2 14B two-stage) | hunyuan (needs Hunyuan 1.5 files). An override changes the graph AND is recorded in the ledger as the family that rendered, so only pass it when you mean it"},"negative":{"type":"string","description":"hard exclusions (default: the model's official training-time negative)"},"out":{"type":"string","description":"output .mp4 path (optional; default under the media dir)"},"transformer":{"type":"string","description":"OPTIONAL per-request LTX-2.5 transformer file override (e.g. this machine's bf16 transformer for one hero render — the int8 default stays every other call's quality/speed tradeoff). Wins over this box's (or the resolved family's) bound file; a no-op on any other family's graph"},"frames":{"type":"integer","description":"frame count at 16fps (81 ~5s is the native ceiling)"},"width":{"type":"integer","description":"width px"},"height":{"type":"integer","description":"height px"},"steps":{"type":"integer","description":"sampler steps"},"seed":{"type":"integer","description":"RNG seed for reproducibility"},"reserve_vram":{"type":"number","description":"VRAM held back for the display (per-workflow override; default ~1.0, raise for Wan)"},"fast":{"type":"boolean","description":"OPT-IN draft mode: 8-step lightx2v distill (visibly weaker motion). The default is the native quality recipe — only set when the caller explicitly accepts draft quality"},"hero":{"type":"boolean","description":"deprecated: the native quality pass IS the default now; no-op kept for compatibility"},"upscale":{"type":"boolean","description":"post-decode upscale using this machine's configured upscale model (e.g. 720p->1080p; no-op if the machine has none)"},` + mediaRouteSchema + "," + mediaNodeSchema + "," + mediaMinVRAMSchema + `},"required":["prompt"]}`),
	}, s.handleGenerateVideo)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_animate_character",
		Description: "CHARACTER ANIMATION on the LOCAL ComfyUI for FREE — retargets the motion of a driver VIDEO onto a reference character IMAGE (WAN-Animate-2 distilled: identity-preserving motion transfer, the only route that does video-driven retargeting; the other video tools generate motion from text). ref = a full-body image of the character to animate (person, mascot, stylized figure); driver = a video of a person performing the motion (full body in frame, static camera works best); prompt describes the CHARACTER + BACKGROUND the output should show. One call renders ONE native chunk (default 81 frames ≈ 3.4s at 24fps — the distilled recipe's unit; measured ~5min warm at the 482x854 template default on the reference box). Optional: motion_prompt (describe the driver's motion), negative, width/height, frames, steps, seed, pose_strength/ref_strength (0-1 floats as strings), reserve_vram, out. The output keeps the driver's own audio track. It auto-starts ComfyUI and takes the shared single-slot GPU lock, so it serializes with other local gen/inference and may wait for the slot before deferring. Returns {video_path, seed}. On any failure it returns deferred:true — then make the clip another way.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"waiter_token":{"type":"string","description":"OPTIONAL. The waiter_token a QUEUED answer of this tool returned (err_class gpu_queued: every card was busy for the whole wait, so the call kept a place in line instead of failing). Re-send the SAME request with it within 10 minutes to resume that place with the arrival time it had; omit it on a first call."},"ref":{"type":"string","description":"local path to the reference character image (full body visible works best)"},"driver":{"type":"string","description":"local path to the driver video whose motion is transferred"},"prompt":{"type":"string","description":"character appearance + background description for the OUTPUT clip"},"motion_prompt":{"type":"string","description":"one-line description of the driver video's motion (default: a generic motion-reference line)"},"negative":{"type":"string","description":"hard exclusions (default: the model's official training negative)"},"width":{"type":"integer","description":"working width px (default 482, the official template's portrait default)"},"height":{"type":"integer","description":"working height px (default 854)"},"frames":{"type":"integer","description":"frame count (default 81 — one native chunk; longer needs multiple calls). On an animategen_engine sdcpp box (a Vulkan iGPU) the default is that box's animategen_frames, else 49, and a count over its token cap is refused"},"steps":{"type":"integer","description":"sampler steps (default 10 — the distilled lcm recipe; do not raise casually)"},"seed":{"type":"integer","description":"RNG seed for reproducibility"},"pose_strength":{"type":"string","description":"0-1: how strongly the driver's pose drives the output (default 1.0)"},"ref_strength":{"type":"string","description":"0-1: how strongly the reference image pins identity (default 1.0)"},"reserve_vram":{"type":"number","description":"VRAM held back for the display (per-workflow override)"},"out":{"type":"string","description":"output .mp4 path (optional; default under the media dir)"},` + mediaRouteSchema + "," + mediaNodeSchema + "," + mediaMinVRAMSchema + `},"required":["ref","driver","prompt"]}`),
	}, s.handleAnimateCharacter)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_generate_audio",
		Description: "Synthesize AUDIO on the LOCAL GPU for FREE — no cloud. kind=voice (default) is text-to-speech narration via Chatterbox Multilingual (commercial-safe, default Spanish; pass clone=<ref.wav> for zero-shot voice cloning, lang for the language). kind=music is a text-to-music bed via ACE-Step (style-tag prompt; seconds for length; optional lyrics). text is the narration text or the music style prompt. Optional: out (output path; default under the media dir), seed, reserve_vram (music only). It takes the shared single-slot GPU lock, so it serializes with other local gen/inference and may wait before deferring. Returns {audio_path, kind, seed}. On any failure (GPU busy, no route, worker error, timeout) it returns deferred:true — then synthesize it another way.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"waiter_token":{"type":"string","description":"OPTIONAL. The waiter_token a QUEUED answer of this tool returned (err_class gpu_queued: every card was busy for the whole wait, so the call kept a place in line instead of failing). Re-send the SAME request with it within 10 minutes to resume that place with the arrival time it had; omit it on a first call."},"text":{"type":"string","description":"narration text (voice) or music style prompt (music)"},"kind":{"type":"string","description":"voice (default, Chatterbox TTS) | music (ACE-Step)"},"voice":{"type":"string","description":"generalist | finetuned | endpoint (default generalist — or endpoint by itself on a box with tts_endpoint and no voicegen_script; finetuned requires this machine's voicegen_ft_* config; endpoint renders through the configured OpenAI-compatible speech server, e.g. VoiceStudio, no media lease — pass tts_voice to name a server-side voice)"},"tts_voice":{"type":"string","description":"voice=endpoint only: the server-side voice/profile name (default: this box's tts_voice, else the server's default)"},"clone":{"type":"string","description":"voice: local path to a reference .wav for zero-shot voice cloning"},"lang":{"type":"string","description":"voice: language code (default es)"},"seconds":{"type":"integer","description":"music: clip length in seconds"},"out":{"type":"string","description":"output audio path (optional; default under the media dir)"},"seed":{"type":"integer","description":"RNG seed for reproducibility"},"reserve_vram":{"type":"number","description":"music: VRAM held back for the display (per-workflow override)"},` + mediaRouteSchema + "," + mediaNodeSchema + `},"required":["text"]}`),
	}, s.handleGenerateAudio)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_edit_image",
		Description: "Apply a DETERMINISTIC edit pipeline to a local image — free, CPU-only (no GPU lock: runs in parallel with renders, never evicts models). ops is an ARRAY applied in order in one call: crop{x,y,width,height}, resize{width and/or height, keep_aspect}, convert{format png|jpg|webp}, composite{overlay,x,y,opacity}, text{text,x,y,size,color,font,anchor}, mask_boxes{boxes,pad?,feather?,invert?} (REPLACES the working image with a white-on-black inpaint mask at its size — ready for offload_inpaint_image), grade{levels{black,white,gamma}?,curve{points[[in,out],...]}?,wb{mode:gray_world|scale,r,g,b}?,luminance_only?} (tone/color grade — everything composes into ONE LUT per channel, single quantize, no banding; alpha untouched), lut_cube{path,strength?} (.cube 3D LUT look at strength 0-1), perspective_composite{overlay,quad:[[x,y]x4]} (warp the overlay into the destination quad — UL,UR,LR,LL winding — and alpha-composite: mockup placement), finish{sharpen{radius,percent,threshold}?,median 3|5?} (delivery sharpening, defaults tuned for post-AI-upscale web output — MUST be the LAST op, after any resize: sharpening before a resize is undone by resampling), flatten_design{} (FIRST op only: opens a .xcf/.psd via GIMP, flattens it, returns its layer list), and instantiate_design{set_text{LayerName:new copy},replace_image{LayerName:image path}} (FIRST op only: GIMP layered-template factory — sets named text layers' copy, swaps named pixel layers for new images at the same offsets, flattens; the remaining ops then run on the result — a one-call brand-variant factory). Optional renditions[] exports a platform matrix from the master out ({width/height,format,suffix} each → <out-stem><suffix>.<format>). Engines are per-machine (see offload_status media): PIL for the pipeline, GIMP only for flatten_design/instantiate_design. Returns {image_path,width,height,ops_applied,layers?,renditions?}. On any failure (engine absent, bad op, tool error) it returns deferred:true — then edit the image another way.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local input image path (.png/.jpg/... ; .xcf/.psd when ops starts with flatten_design or instantiate_design)"},"ops":{"type":"array","items":{"type":"object"},"description":"edit operations applied in order; each is {op:..., ...args} (see tool description)"},"out":{"type":"string","description":"output path (optional; default under the media dir)"},"renditions":{"type":"array","items":{"type":"object"},"description":"optional export matrix from the master out: [{width and/or height, format png|jpg|webp, suffix}] — each writes <out-stem><suffix>.<format> and is listed in the result's renditions[]"}},"required":["image","ops"]}`),
	}, s.handleEditImage)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_inpaint_image",
		Description: "Generatively INPAINT a local image on the LOCAL ComfyUI for FREE — re-renders ONLY the masked region from a prompt, leaving the rest untouched. Use to REMOVE unwanted content (gibberish text, objects, blemishes) or replace a region with new content. mask is a white-on-black image the same size as image (white = repaint). NOTE: diffusion cannot write specific legible text — inpaint-to-clean, then add real type with offload_edit_image's text op. Takes the shared single-slot GPU lock (serializes with other local gen). Returns {image_path, seed}. On any failure (no SDXL-class inpaint binding on this machine, missing files, render error) it returns deferred:true.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"waiter_token":{"type":"string","description":"OPTIONAL. The waiter_token a QUEUED answer of this tool returned (err_class gpu_queued: every card was busy for the whole wait, so the call kept a place in line instead of failing). Re-send the SAME request with it within 10 minutes to resume that place with the arrival time it had; omit it on a first call."},"image":{"type":"string","description":"local path of the image to retouch"},"mask":{"type":"string","description":"local path of the white-on-black mask (white = repaint)"},"prompt":{"type":"string","description":"what the masked region should become"},"negative":{"type":"string","description":"hard exclusions for the repainted region"},"denoise":{"type":"number","description":"0-1; default 1.0 (full re-imagination inside the mask). Values well below 1.0 can produce muted/gray fill on the stock VAEEncodeForInpaint path — prefer 1.0 unless you know the tradeoff"},"grow_mask":{"type":"integer","description":"expand+feather the mask by N px in latent space (default 16; 0 = tight mask, no dilation — seam blending comes from this, not mask feathering)"},"steps":{"type":"integer","description":"sampler steps (default: machine binding)"},"seed":{"type":"integer","description":"RNG seed for reproducibility"},"out":{"type":"string","description":"output PNG path (optional; default under the media dir)"}},"required":["image","mask","prompt"]}`),
	}, s.handleInpaintImage)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_upscale_image",
		Description: "AI-UPSCALE a local image on the LOCAL ComfyUI for FREE with an ESRGAN-family model (this machine's upscale_model binding, e.g. 4x-UltraSharp / RealESRGAN_x4plus). Use it to export a 1024-class render for delivery or print, to recover crispness before compositing, or to enlarge a small asset. It SYNTHESIZES plausible detail, so it is an enlargement tool, not a faithful photo restore; for an exact resample with no invented detail use offload_edit_image's resize op (CPU, free). Default output is the model's own factor (4x for a 4x model); scale sets the overall factor relative to the source exactly (the source is measured and the output size pinned), or pin width+height yourself. The written file's size is verified against the request before success is reported. Takes the shared single-slot GPU lock (serializes with other local gen; the render is seconds, a cold ComfyUI start adds ~1-2 min). Returns {image_path, model, width, height, factor} — factor is the MEASURED output/source ratio (factor_x/factor_y instead when a pinned size is non-uniform). On any failure (no upscale binding on this machine, missing file, half-given or out-of-range size, bad scale/method, render error, size mismatch) it returns deferred:true.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"waiter_token":{"type":"string","description":"OPTIONAL. The waiter_token a QUEUED answer of this tool returned (err_class gpu_queued: every card was busy for the whole wait, so the call kept a place in line instead of failing). Re-send the SAME request with it within 10 minutes to resume that place with the arrival time it had; omit it on a first call."},"image":{"type":"string","description":"local path of the image to enlarge (.png/.jpg/.webp)"},"scale":{"type":"number","description":"overall factor relative to the SOURCE, made exact by measuring the source and pinning the output size (so it holds for any model). Omit for the model's own factor (4x for a 4x model). Must be > 0; the output is verified against source*scale and a mismatch defers"},"width":{"type":"integer","description":"exact output width (<= 16384) — give with height; wins over scale"},"height":{"type":"integer","description":"exact output height (<= 16384) — give with width; wins over scale"},"method":{"type":"string","description":"resampler for the scale/size step: lanczos (default) | bicubic | bilinear | area | nearest-exact"},"model":{"type":"string","description":"override this machine's upscale_model — a ComfyUI upscale_models name (subfolders allowed, e.g. ESRGAN/4x.pth; never an absolute path); works even on a box that binds none (offload_status then still reports the route NOT CONFIGURED for the default path)"},"out":{"type":"string","description":"output PNG path (optional; default under the media dir)"}},"required":["image"]}`),
	}, s.handleUpscaleImage)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_edit_image_generative",
		Description: "Rewrite a local image from a TEXT INSTRUCTION on the LOCAL ComfyUI for FREE — no mask (Qwen-Image-Edit class: the model reads the source through its own vision encoder and re-renders the whole frame). This is the route for instruction edits that have no drawable region: \"make it snowing heavily\", \"turn the leather into fur\", \"make it night\", \"change the sofa to green\". Pick between the three edit routes by what you have: offload_edit_image for DETERMINISTIC ops (crop/resize/text/composite — free, CPU, exact); offload_inpaint_image when you can supply a MASK and want the rest untouched pixel-for-pixel; THIS when the change is global or diffuse and you cannot draw a mask. Note it re-renders everything, so fine detail outside the intended change will shift — prefer inpaint when a mask is possible. On the default (Qwen-Image-Edit 2511) binding the working canvas follows the source within 0.9-2.0 MP (a smaller source is scaled up to ~0.9 MP, a larger one down to 2 MP, unless the machine pins gen_edit_megapixels); preset trades speed for fidelity there: lightning8 (default, ~4x faster) or full. NAMED FAMILIES: family selects one of this machine's opt-in edit bindings (offload_status media.edit_families); omit it for the default. A qwen-image-2.1 family edits with MULTIPLE references: image stays the edit TARGET (<image1> in the prompt) and images adds up to 9 references after it (<image2>..<image10>, 10 images in all); it has no presets (40 steps / cfg 1) and transparent=true keeps alpha. Takes the shared single-slot GPU lock (serializes with other local gen); expect several minutes, most of it fixed model-load overhead. Returns {image_path, seed, family, width, height} (MEASURED output size) plus images (the count, on a multi-reference edit) and license/commercial_use when the binding declares a license. On any failure (no edit binding on this machine, unknown family, images on a single-image family, more than 10 images, missing file, preset on a 2.1 family, render error) it returns deferred:true.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"waiter_token":{"type":"string","description":"OPTIONAL. The waiter_token a QUEUED answer of this tool returned (err_class gpu_queued: every card was busy for the whole wait, so the call kept a place in line instead of failing). Re-send the SAME request with it within 10 minutes to resume that place with the arrival time it had; omit it on a first call."},"image":{"type":"string","description":"local path of the source image"},"prompt":{"type":"string","description":"the edit INSTRUCTION, e.g. 'make it snowing heavily, winter atmosphere' — describe the change, not the whole scene"},"negative":{"type":"string","description":"hard exclusions"},"preset":{"type":"string","description":"full | lightning8 | lightning4 — a MATCHED steps+cfg+LoRA triple (2511 binding only; a qwen-image-2.1 family has no presets). Prefer switching preset over setting steps/cfg by hand: half-overriding the pairing renders successfully and looks wrong"},"steps":{"type":"integer","description":"sampler steps (advanced; overrides the preset — see preset)"},"cfg":{"type":"number","description":"guidance (advanced; a Lightning preset needs 1.0 — see preset)"},"seed":{"type":"integer","description":"RNG seed for reproducibility"},"out":{"type":"string","description":"output PNG path (optional; default under the media dir)"},"family":{"type":"string","description":"OPTIONAL named edit family (offload_status media.edit_families); omit for this machine's default edit binding"},"images":{"type":"array","items":{"type":"string"},"maxItems":9,"description":"extra REFERENCE image paths after the target (qwen-image-2.1 families only; the target plus these is at most 10). Address them in the prompt as <image2>..<image10>; image is <image1>"},"transparent":{"type":"boolean","description":"keep an alpha channel in the output (qwen-image-2.1 families only)"}},"required":["image","prompt"]}`),
	}, s.handleEditImageGenerative)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_media",
		Description: "Run ONE ffmpeg av operation on local media — free, CPU-only (no GPU lock). op: trim{in,start,end|duration, reencode? (default false = fast keyframe-snapped stream copy)}, concat{inputs[] (same codec)}, extract_frames{in, fps OR count, out = directory}, convert{in (target by out extension; audio_only/video_only)}, mux_audio{in (video), audio, shortest}, probe{in} -> {duration_sec, streams[], format}. Inputs are LOCAL paths. Returns op-specific JSON. On any failure (ffmpeg absent, bad args) it returns deferred:true — then do it another way.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"op":{"type":"string","description":"trim | concat | extract_frames | convert | mux_audio | probe"},"in":{"type":"string","description":"input media path (all ops except concat)"},"inputs":{"type":"array","items":{"type":"string"},"description":"concat: >=2 input paths, same codec"},"out":{"type":"string","description":"output path (optional; extract_frames: a directory). probe has no output"},"start":{"type":"string","description":"trim: start (seconds or hh:mm:ss)"},"end":{"type":"string","description":"trim: absolute end time"},"duration":{"type":"string","description":"trim: duration in seconds (alternative to end)"},"reencode":{"type":"boolean","description":"trim: re-encode for exact cuts (default false = keyframe-snapped -c copy, fast)"},"fps":{"type":"number","description":"extract_frames: sampling rate"},"count":{"type":"integer","description":"extract_frames: total frames (resolved to fps via probe)"},"audio":{"type":"string","description":"mux_audio: audio input path"},"shortest":{"type":"boolean","description":"mux_audio: stop at the shorter input (default true)"},"audio_only":{"type":"boolean","description":"convert: drop video (-vn)"},"video_only":{"type":"boolean","description":"convert: drop audio (-an)"}},"required":["op"]}`),
	}, s.handleMedia)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_compose_video",
		Description: "COMPOSE designed motion graphics into VIDEO locally for FREE with HyperFrames — deterministic HTML/CSS -> MP4/WebM/MOV (alpha)/GIF/PNG sequence: title cards, lower thirds, kinetic type, stat cards, overlays to lay over b-roll. CPU-class: software GL + CPU encode, NO GPU lock (runs beside every render and text seat), local only, never cloud (HyperFrames' cloud/lambda/capture paths are not wired). Give ONE input: template + variables (the vetted templates on this box — offload_status media.routes.compose_video lists them; shipped: title-card, lower-third, stat-card, section-title, callout-label, checklist-card, captions-bar; variables are the template's declared text/colors/duration, typed and escaped), OR html (an inline single-file composition), OR project_dir (a local composition dir). html and project_dir are TRUSTED CODE ONLY: HyperFrames' Chrome runs without a sandbox, so never pass third-party pages. Every render is gated: lint (0 errors), check (runtime/layout/contrast), then ffprobe measures the output (codec, size, fps, duration, alpha) — the returned fields are measured, not requested. format webm (VP9) or mov (ProRes 4444) carries alpha; mp4 is opaque H.264. quality defaults to this machine's compose_quality (high = libx264 slow CRF 15). snapshots = seconds to also save as PNG frames for a visual check. Same inputs render byte-identical frames. route places the render: auto (default) runs here when this machine has the composition lane and otherwise on a fleet node; a fleet node renders a template through its template door and html or project_dir as a confined project bundle through its token-gated compose-project door (ADR 0071), and the video and snapshots are fetched back into this machine's media dir (or out). Returns {video_path | frames_dir, duration_sec, fps, frames, width, height, has_alpha, has_audio, codec, render_ms, lint:{errors,warnings}, check:{ok,findings}, snapshots[]}. On any failure it returns deferred:true with a typed reason (BAD_INPUT, LINT_ERRORS, CHECK_FAILED, RENDER_FAILED, BROWSER_MISSING, FFMPEG_MISSING, CLI_MISSING, SPAWN_EBUSY, DISK_HEADROOM, TIMEOUT, or no composition route on this machine).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"template":{"type":"string","description":"a vetted template name on this machine (e.g. title-card, stat-card, captions-bar); give exactly one of template, html, project_dir"},"variables":{"type":"object","description":"values for the template's declared variables, e.g. {\"title\":\"Q3 results\",\"accent\":\"#22c55e\",\"duration\":6}; an undeclared or mistyped key defers BAD_INPUT"},"html":{"type":"string","description":"an inline single-file HyperFrames composition (TRUSTED code only: Chrome runs it without a sandbox)"},"project_dir":{"type":"string","description":"a local composition directory holding index.html (TRUSTED code only)"},"composition":{"type":"string","description":"a composition file relative to project_dir to render instead of index.html"},"out":{"type":"string","description":"output path (a directory for png-sequence; optional, default under the media dir)"},"format":{"type":"string","enum":["mp4","webm","mov","png-sequence","gif"],"description":"mp4 (default, H.264), webm (VP9 with alpha), mov (ProRes 4444 with alpha), png-sequence (RGBA frames), gif"},"fps":{"type":"integer","description":"frame rate 1-240 (default: the composition's data-fps, else 30)"},"quality":{"type":"string","enum":["draft","standard","high"],"description":"encoder preset (default: this machine's compose_quality, shipped high)"},"resolution":{"type":"string","enum":["landscape","portrait","landscape-4k","portrait-4k","square","square-4k"],"description":"output size preset; the aspect must match the composition (4k = integer supersampling)"},"workers":{"type":"integer","description":"parallel Chrome workers 1-24 (default: this machine's compose_workers, else auto)"},"strict":{"type":"boolean","description":"fail on lint errors / a failed check (default true)"},"snapshots":{"type":"array","items":{"type":"number"},"description":"seconds to also save as PNG frames next to the output (up to 16)"},"route":{"type":"string","enum":["local","auto","remote"],"description":"auto (default): here when this machine has the composition lane, else a fleet node from delegate_remotes; remote: always a fleet node; local: always this machine"}}}`),
	}, s.handleComposeVideo)

	// offload_browse (ADR 0060) is registered only when this box configured the lane:
	// an opt-in surface that drives the operator's own browser must not appear in the
	// tool list of a machine that never bound it.
	if s.p != nil && s.p.Cfg().BrowseConfigured() {
		srv.AddTool(&mcp.Tool{
			Name:        "offload_browse",
			Description: "DRIVE THE OPERATOR'S OWN BROWSER toward a natural-language goal (OPT-IN lane, ADR 0060). A sidecar (browser-use jev-ultrafast over browser-harness/CDP) opens a background tab in the operator's already-running, logged-in Chromium browser, observes the page as an indexed element table, and executes one CLICK / TYPE_TEXT / SELECT / SCROLL / WAIT per decision. Typed decisions come from the LOOPBACK decision endpoint the operator configured (browse_decision_url — the harness holds no key; that local service may itself front a cloud decision model, and the visible page text and element labels are sent to it); field values are written by the LOCAL seat under a grammar. SAFETY: controls labelled publish / send / post / delete / pay / buy / checkout / subscribe / confirm / sign out (and similar) are removed before the model sees them and re-checked at execution, so a run ends `denied` rather than clicking one; allow_labels lifts that for named labels on THIS attended door only. allow_hosts pins the run to hosts (subdomains included): a click on an off-list link or form target is refused, and a page that navigates off-list by itself ends the run. capture = URL prefixes whose requests/responses are saved as REDACTED JSONL (cookies, authorization and token-like headers dropped; sensitive query parameters and JSON/form keys replaced). Never pass pages whose text should not leave this machine (the page text goes to the decision endpoint). DONE is the decision model's claim, not proof: verify the outcome yourself. Returns {status, model_done, final{url,title,text}, actions[], steps, step_log[], decisions, text_calls, decision_model, decision_cost_usd, capture_path, captured, removed_labels}. On any failure it returns deferred:true with a typed reason (NOT_CONFIGURED, BUSY, BAD_INPUT, CLI_MISSING, BROWSER_UNAVAILABLE, DECISION_UNAVAILABLE, TEXT_UNAVAILABLE, HOST_NOT_ALLOWED, DENIED, blocked, budget, TIMEOUT, RUNNER_FAILED) and, where the run started, the run so far as partial.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"absolute http(s) start URL"},"goal":{"type":"string","description":"the natural-language goal, with every value the run must type (1-4000 chars)"},"max_actions":{"type":"integer","description":"executed-action budget 1-60 (default: this machine's browse_max_actions)"},"allow_hosts":{"type":"array","items":{"type":"string"},"description":"bare host names the run may visit (subdomains included); empty = any http(s) host"},"allow_labels":{"type":"array","items":{"type":"string"},"description":"exact control labels to exempt from the deny-list for this run (e.g. [\"Schedule\"]); attended use only"},"capture":{"type":"array","items":{"type":"string"},"description":"up to 8 http(s) URL prefixes whose traffic is saved as redacted JSONL (capture_path in the result)"},"route":{"type":"string","enum":["local"],"description":"the lane runs only on this machine's own browser"}},"required":["url","goal"]}`),
		}, s.handleBrowse)
	}

	srv.AddTool(&mcp.Tool{
		Name:        "offload_nim",
		Description: "Send a prompt to a remote OpenAI-compatible NVIDIA NIM endpoint — NVIDIA's hosted build.nvidia.com catalog (dozens of FREE models: nemotron, llama, gpt-oss, qwen, deepseek, glm, kimi…) by default, or a self-hosted NIM via base. This is the ONLY cloud/remote tool on this server — every other offload_* tool runs on the LOCAL models (see offload_status for that roster). It is OPT-IN (the hosted endpoint needs NVIDIA_API_KEY in the server env; a self-hosted NIM via base is keyless): use it deliberately for a stronger model than the local cascade, NOT for routine grunt work. The local GBNF grammar path and the savings ledger are untouched (NIM calls are never ledgered). Set list_models=true to browse available model ids. Returns {model, content, reasoning_content, tokens_in, tokens_out, truncated}; on any failure (no key, endpoint down, bad model) it returns deferred:true with a reason and you handle the prompt yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","description":"the user prompt"},"model":{"type":"string","description":"model id (default from config; set list_models=true to browse)"},"system":{"type":"string","description":"optional system prompt"},"base":{"type":"string","description":"override the OpenAI-compatible base URL incl. /v1 (e.g. a self-hosted NIM http://host:8000/v1)"},"max_tokens":{"type":"integer","description":"max completion tokens (default from config; reasoning models need headroom)"},"temperature":{"type":"number","description":"sampling temperature (default 0)"},"list_models":{"type":"boolean","description":"list available model ids instead of generating"}},"required":["prompt"]}`),
	}, s.handleNIM)

	// agent_run (P5 drive mode "a", MCP front door): Claude commands the LOCAL
	// autonomous agent loop. The agent plans + iterates IN-PROCESS over read-only
	// tools + the offload cascade; MCP is the front door, never how the agent
	// reaches its tools (those are in-process). The agent's offload_* calls run on a
	// fresh nil-cache/nil-ledger pipeline via RunTier(record=false) so the savings
	// ledger, cache, and shadow store are untouched. Read-only here (no write/exec/
	// net); a failure is a clean defer, not a server error.
	// agent_rig (ADR 0036 P3a): the seat rigger's classifier over this box's
	// delegation-log corpus. Reads files only — no seat, no network; an
	// unknown seat is a clean defer naming the seats seen.
	srv.AddTool(&mcp.Tool{
		Name:        "agent_rig",
		Description: "The seat rigger, first slice (ADR 0036 P3a): classify THIS box's delegation-log failures for one seat onto exactly ONE failure axis each, in a published precedence order (seat-infra [incl. placement failures with no defer class] → call-deadline [a subtask the whole-call deadline cut: not evidence about the seat] → capacity [a full fleet: every node that could take it was busy for as long as the wait ran, or a node's 503/429 queue-full answer: not a seat's fault] → timeout → reasoning-starved [the completion went into the think block or came back empty] → final-tool-call [the last step offered no tools and the seat wrote a tool call, again after one re-ask: not a task too large] → budget → abstention [an 'output failed schema' reason is schema-miss/invalid] → schema-miss[two-step-grounded|invalid] → anchor-miss[two-step-grounded] → loop → long-observation → tool-misuse → unclassified), and return the triage report: per axis the hits, the ELIGIBLE rows (trace-only axes count only rows that carry a trace), the weight, up to five evidence job ids with the deciding fact, and the pre-authored remedy where the harness's closed vocabulary has a lever (agent_seed_context_reads, max_observation_tokens, max_calls_per_tool, rewrite_error; agent_call_deadline_sec / agent_placement_wait_sec for a full fleet) or the honest 'not a rule matter' where it has none (seat errors, wall timeouts, a seat that answers the last step with a tool call). A dispatch a node refused for capacity that the delegator outwaited and re-placed (the row's `refusal` marker) is no failure: it is counted on the report's own `refusals` line, outside the rows. It PROPOSES NOTHING on its own and applies nothing — adoption is a measured A/B on the seat (P3b). Reads the corpus files only; never calls a seat or the cloud. Use it after a batch of delegations to see what actually failed and why before tuning anything.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"seat":{"type":"string","description":"the seat alias exactly as corpus rows name it (e.g. qwen3.5-4b-vllm, agent-pool)"},"since":{"type":"string","description":"window back from now: <N>d or <N>h (default 7d), or an RFC3339 instant"},"node":{"type":"string","description":"only rows that ran on this node: its node id, or the id it stamped on its own result (optional)"},"markdown":{"type":"boolean","description":"also return the markdown triage table (default false; the JSON report is always returned)"}},"required":["seat"]}`),
	}, s.handleAgentRig)

	srv.AddTool(&mcp.Tool{
		Name:        "agent_run",
		Description: "Run the LOCAL autonomous agent loop on a goal: a free local model plans and iterates over read-only tools (list_dir, read_file) plus the offload_* cascade, multi-step, and returns a final answer. NOTE the advertised set may be NARROWED: a box can set a default agent_profile (small-seat tiers do, because an un-narrowed tool list measurably collapses a small planner), and a narrowed profile such as \"research\" drops search_files and the whole offload_* cascade. The response reports the profile applied and the post-narrowing tool count, so check those rather than assuming the full set. DELEGATE a bounded multi-step read-and-reason job — map how X flows through a repo, summarize a doc set, extract facts across many files — to the local stack to keep that work out of your own context. It is READ-ONLY: it cannot write files, run commands, or touch the network. The savings ledger is untouched (the agent's offload calls run record=false). Returns {output, steps, stop_reason, tools, model}; on any failure it returns deferred:true with a reason and you do the task yourself. A call that gets past argument validation and seat placement also carries {wall_estimate_sec?, min_turn_sec?, wall_note?}, answered or deferred (a call refused before a seat is chosen, by its arguments or a placement guard, and a route'd one, carries none). THE WALL IS A HARD DEADLINE on this door: a run still working when timeout_sec ends is cut and deferred (the reason says so), and a slow seat (the 27B) needs far more than the default for a many-step run. So the call is SIZED before it starts from the seat's own measured decode rate (the configured agent_seat_tok_s stands in only for the box's own agent seat: a model you name, or a seat placement picks, that has no measured rate of its own is not sized, never refused, and publishes no estimate): wall_estimate_sec is what the run needs, min_turn_sec is a cold load plus one worst-case final turn, and wall_note is the arithmetic (it says wall N s is BELOW the estimate M s when your wall is short); a wall that cannot hold even one tool step and a minimal answer is REFUSED before step 1 (deferred, defer_class budget, steps 0, nothing sent to the seat) with the wall it needs. A wall above that floor but below the estimate RUNS, and the final answer's token budget is FITTED to the wall that is left (the forced final step opens at the largest budget the remaining wall can decode at the seat's rate, never above the configured worst case and floored at 1,024; the tool steps are not shortened): when that narrowed it, the answer (or the deferral of a run that died at its wall) also carries {final_budget_fit?, budget_note?}, the budget the final opened at and the arithmetic (a run whose own final was not narrowed, because it answered early, was cut before its forced final or reached it with wall to spare, carries instead the fit predicted when its wall started, as the delegation door's result does). Size timeout_sec from wall_estimate_sec. A `model` this box does not serve is looked up on the fleet (register D-125): with no route, or route remote, it runs as ONE contract through the delegator on a node whose agent seat is that model (the response names node, placement, seat and executed_on, plus node_reported only when the node's own result named itself by another id, carries no sizing of this box, and notes read_root and judge, which do not travel), and a name no node runs is refused with the roster sentence and where the fleet looked (defer_class config when every node answered, infrastructure when one did not); a model on route auto, spread or queue is refused, because they place by capacity and not by seat name, and route local never asks the fleet. On a composite box (ADR 0052) the result also carries `placed` — which layer and seat ran it and why — and an explicitly named seat that belongs to an opt-in layer is admitted only if that layer's guards admit it right now.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"route":{"type":"string","enum":["","local","auto","remote","spread","queue"],"description":"WHERE the run goes (register C-46): omit/local = this box's agent seat with read_root (the default); remote/auto/spread/queue = one contract through the delegator's placement — read_root and judge do NOT travel (the executing node reads its own root and runs its own agent seat; the response notes what was left behind), so use it for self-contained goals and setup_actions; the response names node, placement and seat, plus node_reported only when the node's own result named itself by another id. A model rides route remote only, where it selects the nodes whose agent seat is that model; auto, spread and queue refuse a model, because they place by capacity, not by seat name"},"goal":{"type":"string","description":"the task for the local agent to accomplish"},"read_root":{"type":"string","description":"absolute directory the agent may read; it cannot read outside it (default: the server working dir)"},"max_steps":{"type":"integer","description":"hard step budget (default 12)"},"model":{"type":"string","description":"planner model id; must support tool-calling (default: the tier's agent seat (agent_model), falling back to the configured workhorse). A model this box does not serve is looked up on the fleet (register D-125): with no route, or route remote, it runs on a node whose agent seat is that model, else it is refused naming where the fleet looked; route local never asks the fleet"},"timeout_sec":{"type":"integer","description":"wall-clock budget in seconds, a HARD deadline on this door (default: the tier's agent_timeout_sec, else 180). A wall shorter than the seat's one-step-plus-minimal-answer floor is refused before step 1 with the numbers; a wall below the full-run estimate runs with its final answer fitted to the wall that is left (final_budget_fit), and the result's wall_note says by how much. Slow seats (the 27B) defer on short walls: size it from wall_estimate_sec"},"context_class":{"type":"string","enum":["","long"],"description":"long = ask for the box's biggest long-context layer (the three-card seat where a box declares one, otherwise the pair's 262k seat) under its display-floor, host-RAM and presence guards and a prefill feasibility check; omit for the default placement"},"setup_actions":{"type":"array","maxItems":8,"items":{"type":"object","properties":{"tool":{"type":"string"},"args":{"type":"object"}},"required":["tool"]},"description":"tool calls REPLAYED before the model's first turn (ADR 0036 P2): each runs through the seat's env rules and dispatch like a model call and lands in the transcript as an assistant tool_call + its result, so the first turn already holds what the model would otherwise spend its first steps fetching (e.g. read_file of the document it must digest). Not charged to max_steps; bounded to half the compaction budget (the rest are recorded not-run). A failing action is an observation, never an abort. Response carries setup_ran and step-0 trace entries with setup:true"},"profile":{"type":"string","enum":["general","edit","build","research","github"],"description":"task profile: narrows the tool list and injects worked examples. MEASURED: a small planner given the full tool set often calls NO tool at all, so a narrowed profile is the single most effective lever. Prefer \"build\" for reading and reasoning over a codebase; \"general\" advertises everything. OMITTING this falls back to the box's configured agent_profile, and only then to \"general\" — so a small-seat tier seeded with a narrowed profile gets it without every caller remembering to ask. Tools this read-only front door does not grant are dropped, along with their examples."},"thinking":{"type":"string","enum":["auto","on","off"],"description":"planner think-block policy (0.115.8). auto (default; or the box's agent_thinking): every step thinks, and an EMPTY final is re-issued once with thinking off at 4x the step budget, then the run stops as reasoning_starved/empty (a defer, never an empty answer). off: every planner call renders in non-thinking mode (chat_template_kwargs enable_thinking:false) — use for grounded extraction on a thinking seat that spends its budget in the think block. on: never send the kwarg"},"judge":{"type":"boolean","description":"end-of-run ADVISORY audit: one extra same-seat completion grading the run's flagged effects (parked/failed/unknown/self-flagged) for the operator review. Never gates anything. Default false"},"allow_browse":{"type":"boolean","description":"grant the browse tool (ADR 0060): the seat may drive THIS machine's own browser through the browse lane. Local route only; needs agent_allow_browse on this box, a configured lane and browse_hosts. Judged unattended: publish/send-class controls are always refused. Every call is audited (the audit trail defaults to the operator's agent-audit.jsonl)"},"browse_hosts":{"type":"array","items":{"type":"string"},"description":"with allow_browse: the bare host names (subdomains included) the browse tool may visit; required"}},"required":["goal"]}`),
	}, s.handleAgentRun)

	// offload_ask: the ONE-CALL delegation entry. Registered unconditionally and
	// beside agent_run on purpose — the whole point is that the cheap path is
	// cheap, and a tool gated behind a config flag is one more reason not to
	// reach for it. Measured organic adoption of agent_delegate is ~0 and three
	// rounds of steering pressure (prose, a nudge hook, a blocking gate) moved
	// it not at all; the diagnosis is arithmetic, not discipline — authoring a
	// contract costs more at the decision point than a Read does. So the
	// harness authors the contract (internal/askjob) and the caller supplies
	// only question + paths.
	srv.AddTool(&mcp.Tool{
		Name:        "offload_ask",
		Description: "Ask a bounded question ABOUT SPECIFIC FILES and have a FREE local seat answer it — the one-call form of agent_delegate. You supply question + paths and nothing else: the harness builds the whole contract (goal, {answer,evidence} output schema, and an acceptance check ANCHORED to distinctive tokens mined from the files themselves) and runs it on the local agent seat. REACH FOR IT THE MOMENT YOU ARE ABOUT TO OPEN MORE THAN TWO FILES to answer something bounded — that is exactly where reading them yourself costs more than asking. The files are read and inlined by the HARNESS under read_root, so your own context never pays for them. Good fits: \"which key sets the queue cap\" over three config files; \"which function does this handler call before dispatch\" over a handler plus its helpers; \"what changed between these two versions of the spec\". NOT this tool: unbounded exploration with no file list (use agent_run — it searches for its own files); anything that writes or runs (this lane is read-only); a multi-part job needing several contracts (agent_delegate); and above all anything whose answer is a JUDGEMENT rather than a fact the files state — security or credential review, an architecture decision, or the final does-it-actually-work verification. A free seat reports what the files say; it does not own a call you are accountable for. Returns {answer, evidence, verified, acceptance, acceptance_failures?, seat, steps, stop_reason, cache_hit, retried_on?, retry_note?} — evidence is the exact lines the seat relied on so you can spot-check instead of re-reading. verified is a CITATION check, not a correctness verdict: it asks whether the published answer quoted one of a few distinctive tokens mined from these files, never whether the answer is right. Those tokens are chosen to be things only these files would say — real identifiers wherever the files have them — but it is a heuristic, so treat verified:true as \"this answer demonstrably read the files\", not as proof of a verbatim quotation. On verified:false read acceptance_failures (which names the check that did not match) and then the evidence — it is a prompt to look, not proof the answer is wrong, and a question whose subject is a short or question-named identifier can leave nothing anchorable at all. Typical latency is 30-90 s: this buys back the context those files would have cost you, not wall-clock. cache_hit tells you how the answer was obtained: an IDENTICAL repeat within the same session — same question, same read_root, and the same file BYTES — returns the stored answer without spending the seat again, and reports cache_hit:true. A REPEAT DOES NOT RE-ROLL THE SEAT: an identical call after a verified:false answer returns that same unverified answer again, cache_hit:true, with no new seat run, and that answer already includes the one cross-seat retry a failed local first attempt gets when no route was named (retried_on, retry_note) — to get another attempt, change the question, which is a different key and always a fresh run. It is keyed on CONTENT, so editing any attached file also makes the next call a fresh run automatically; a stale answer cannot be served. Do NOT read this as a general speedup: a DIFFERENT question over the same files pays full seat time, because nothing keeps a warm model context between calls. Caps: at most 16 files, 128 KiB per file, 256 KiB total. It REFUSES (deferred:true) when the files hold no token distinctive enough to ground the check, rather than handing back an answer nothing verified. On any failure it returns deferred:true with a reason and you read the files yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"route":{"type":"string","enum":["","local","auto","remote","spread","queue"],"description":"WHERE the question runs (register C-46): local = this box's agent seat; omitted = the same, unless loading it would unload another loaded vLLM seat (then auto, and route_note names the seat); remote/auto/spread/queue = the delegator's placement (the files ride inline in the contract, so any node can answer); the response names node, placement and seat, plus node_reported only when the node's own result named itself by another id"},"question":{"type":"string","description":"the bounded question to answer FROM THESE FILES — one question, answerable from what you attach"},"paths":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":16,"description":"the files to answer from, relative to read_root or absolute inside it (<=16 files, <=128 KiB each, <=256 KiB total)"},"read_root":{"type":"string","description":"absolute directory the paths are read from; nothing outside it can be read (default: the server working dir)"}},"required":["question","paths"]}`),
	}, s.handleAsk)

	// offload_review_diff: the CLEAN-CONTEXT review lane. Registered
	// unconditionally beside offload_ask, and for the same reason — but the
	// argument for it is different in kind. Every other lane competes with
	// "read the file yourself" on cost; this one offers something the caller
	// cannot produce from inside its own context AT ALL: a reviewer that never
	// saw the work. The seat's clean context is the mechanism, not a side
	// effect, so the contract ships the task statement and the diff and nothing
	// else (internal/reviewlane).
	srv.AddTool(&mcp.Tool{
		Name:        "offload_review_diff",
		Description: "Review a code DIFF on a FREE local seat with CLEAN context — the reviewer sees only the diff and the task statement, never this conversation's history. That isolation is the mechanism: a reviewer without the author's accumulated context catches defects the author's own judgement has stopped seeing (long-window context degradation is the well-studied effect this exploits). Pass diff (inline) or diff_path (a file holding a unified diff) — exactly one — plus task, which is what the change was SUPPOSED to do: without stated intent a reviewer cannot tell a defect from a decision. Returns {findings:[{severity,file,line,claim,why,also?}] ranked severe|moderate|minor first, reviewed_bytes, seat, steps, stop_reason, note?, salvaged?, dropped_ungrounded?, dropped_echo?, dropped_hollow?, dropped_duplicate?, truncated_by_cap?, outcome, outcome_reason?, evidence, trace_error?}. note explains an EMPTY findings list in words — read it, the two cases mean different things — and a SALVAGED list (below). The five counts say what is not in the list: dropped_ungrounded named a file the diff never touched, dropped_echo handed the prompt's own template back, dropped_hollow had no severity, no file and no why (a bare claim, usually restating the diff), dropped_duplicate merges the same defect reported more than once, including findings that cite the same file and line in different words (the most severe is kept and the others' claims ride in its also[] so every claim stays readable; merging runs BEFORE the cap, so repeats never crowd out a unique finding), truncated_by_cap is what your max_findings hid. HOW TO USE THE RESULT: findings are TRIAGE INPUT, not verdicts. Read the flagged lines yourself and decide — never apply a finding unread, and treat a `severe` label from a small local model as a prompt to look, not as proof anything is wrong. Equally, an EMPTY findings list means this reviewer found nothing; it is not a verification that the change works. ADVISORY ONLY: this lane never gates a merge and never substitutes for the final does-it-actually-work check, which stays yours — as do security review, architecture judgement, and any call you are accountable for. dropped_ungrounded counts findings naming a file the diff never touched (an invented path is how a small seat fails here); they are removed and reported rather than silently kept. If NO finding survives filtering — the seat wrote nothing, or every line it wrote was hollow, echoed or ungrounded — and its raw answer does not read as an explicit clean NONE verdict, this DEFERS (defer_class abstention) rather than reporting an empty list, and the deferral carries the counts and a reason naming them: a broken or hollow run must never arrive looking like a clean diff, so review the diff yourself. Only the seat's own NONE earns an empty findings list. SALVAGE: when the seat DID write its review and only the clock ended the structuring step — its final answer was cut at the completion budget (salvaged:\"output_truncated\"), or the wall had no time left for the structured re-pack (salvaged:\"wall\") — the lane reads the complete lines of the raw answer itself (a final line cut mid-way is dropped), runs them through the same filters, and publishes what survives with the flag and a note: an output_truncated list may be INCOMPLETE (whatever the seat had not yet written is unreviewed), a wall list is the seat's whole answer. If nothing survives it defers as above. READING (salvaged:\"repack_flattened\"): the structured re-pack of a seat's answer can FLATTEN it — small seats were seen to split a severity | file:line | claim | why line into bare fields, or to answer in JSON objects after a cut-final re-issue whose re-pack kept only each claim — so the lane also reads the seat's own answer (its pipe lines, or such JSON objects) and publishes whichever reading keeps more findings through the filters; when they tie the re-pack stands. Every other deferral (gpu busy, a seat or stack failure, a re-pack that answered the wrong shape) is unchanged. Caps: at most 10 findings (max_findings only narrows it), a diff of <=256 KiB inline or <=128 KiB via diff_path — split a larger one by path (git diff -- <dir>), which also keeps each review inside the seat's context window. OUTCOME AND EVIDENCE: every result, early refusals included, carries outcome (PASS | FINDINGS | GATE_FAILED | NOT_RUN | STALE) and evidence binding it to the exact diff judged: diff_sha256, diff_bytes (a refusal before any diff was read carries diff_bytes 0 and no fingerprint), base_sha?/head_sha? (pass them if you have them: echoed, never resolved), gates (the lane's own diff-size and conflict-markers, then your gate_receipts [{name, status pass|fail, detail?, head_sha?}], attached BEFORE the seat runs), seat (the seat's receipt), recheck_sha256 (the diff fingerprint checked again right before returning: a diff_path is re-read through the same confined reader, but for an inline diff the check compares the string with itself, so it can never detect movement), head_unbound? (true when you passed no head_sha). THE LANE RUNS NO LINTS OR TESTS ITSELF: gate_receipts are results you ran and hand over, and the lane cannot verify them. When you pass head_sha, a receipt for another commit, or one that names no commit, cannot support a PASS. A review with no head_sha is tied to no commit: head_unbound is true, a receipt that names a commit is unbound (so a clean seat gives GATE_FAILED), and a clean PASS carries outcome_reason no head_sha: gates not tied to a commit. Even with head_sha nothing ties the head to the diff bytes: the fingerprint binds the bytes, the head is your word. PASS needs a seat that said NONE, a diff fingerprint with a second check that agrees, a diff inside the ceiling, green gates for the reviewed commit and an unchanged diff. NOT_RUN means no clean seat reviewed it (every result with deferred:true, an early refusal, an oversize diff, a refused binding, evidence with no diff fingerprint, and a run the node filed as a deferral whose answer the lane read as a clean NONE) and is never a PASS; findings read from such a deferred run stay FINDINGS; STALE means the diff changed during the review, so re-run it; GATE_FAILED is a clean seat beside a red, wrong-commit or unbound gate. Each review leaves a durable trace, review-evidence.jsonl in the same directory as the ledger file (no ledger row is written for the verdict), whether or not you act on it; trace_error says when that failed. On any failure it returns deferred:true with a reason and you review the diff yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"diff":{"type":"string","description":"the unified diff text, inline (mutually exclusive with diff_path; <=256 KiB)"},"diff_path":{"type":"string","description":"path to a file holding the unified diff, read by the HARNESS under read_root so your context never pays for it (<=128 KiB)"},"task":{"type":"string","description":"what this change was SUPPOSED to accomplish — the intent the reviewer judges the diff against"},"max_findings":{"type":"integer","description":"cap on returned findings (default 10, which is also the ceiling: the seat is never asked for more)"},"read_root":{"type":"string","description":"absolute directory diff_path is read from; nothing outside it can be read (default: the server working dir)"},"base_sha":{"type":"string","description":"optional: the hex id (7-64 characters) of the commit the diff is against; echoed into the result's evidence, never resolved"},"head_sha":{"type":"string","description":"optional: the hex id (7-64 characters) of the commit the diff ends at; echoed into the evidence, never resolved or checked against the diff bytes. A gate receipt for another commit, or one that names no commit, cannot support a PASS. Without it the review is tied to no commit (head_unbound) and a receipt that names a commit cannot support a PASS"},"gate_receipts":{"type":"array","maxItems":16,"items":{"type":"object","properties":{"name":{"type":"string"},"status":{"type":"string","enum":["pass","fail"]},"detail":{"type":"string"},"head_sha":{"type":"string","description":"the commit the gate ran on; without it the receipt cannot support a PASS once the review has a head_sha, and naming one on a review with no head_sha cannot support a PASS either"}},"required":["name","status"]},"description":"optional: receipts of deterministic gates (lints, tests) YOU already ran on this change and hand over; the lane runs no lints or tests itself and cannot verify a receipt. Attached to the evidence before the seat reviews"}},"required":["task"]}`),
	}, s.handleReviewDiff)

	// agent_delegate (multi-node delegation, Task 6): registration is GATED on
	// agent_delegation_enabled (roast delta 13) so tools/list stays
	// byte-identical when the delegator role is off — pinned in
	// TestAgentDelegateRegistrationGated. The s.p nil-guard mirrors the
	// badargs-test constructor; production always has a pipeline.
	if s.p != nil && s.p.Cfg().AgentDelegationEnabled {
		srv.AddTool(&mcp.Tool{
			Name:        "agent_delegate",
			Description: "Fan out 1-8 self-contained subtasks to the FREE local delegation engine. NOT the door for single-shot mechanical text: one document + one summarize/classify/extract/triage question goes to the offload_* cascade tool first (seconds; a contract costs a 20-200 s seat run) — this door is for multi-document read-and-reason with context docs, a schema and acceptance checks (register A-102). Each subtask is a contract {goal, context docs, output_schema, acceptance} run by an autonomous read-only agent loop on THIS box or on a fleet node over the operator's tailnet (never cloud). Placement is quality-first: an idle local box runs the work up to its run-cap line (fleet_max_concurrent_jobs, 4 by default) and the rest of a wider call goes to the remote nodes that have room; a remote node is also used whenever the local GPU is busy, and only when it passes the capability gate (route=auto; a local or remote route pins the call only with a pin_reason, and without one it is a hint placement may override). context_paths inlines files DELEGATOR-side (confined to read_root) so your context never pays for them. SIZE CONTRACTS FROM THE LIVE CEILING, never from a remembered or written figure: offload_status {section:\"brief\"} reports each seat's agent_ctx_tokens. Written guidance drifted to a quarter of the real window and work that fits trivially was declined as 'too big for the seat' for weeks, so a figure you did not just read from offload_status is not a number. acceptance is a machine-checkable DSL evaluated by the delegator before a result counts as done: contains:<s>, not_contains:<s>, regex:<re>, min_items:<field>:<n>, nonempty:<field> (nonempty:<f1>|<f2> passes when any one named field has a value) — a schema-valid result failing a check comes back as failed_verification, NOT a success. Returns {summary:{succeeded,deferred,failed_verification,failed,infrastructure,corpus_rows_lost,ledger_rows_lost}, results:[{node,node_reported,seat,placement,ran_on,job_id,output,structured,deferred,reason,defer_class,failed,acceptance_failures,wall_ms,acceptance_lint}]} — read summary FIRST. results[].ran_on says where a subtask ran: \"local\" is this box's own seat, \"node\" a fleet node, and the key is absent when no seat ran it (a defer, a shed, a refusal); read it, not the word \"local\" in results[].placement, which is the reason in words and can mention a busy local seat on a result a node ran. results[].node is the fleet node_id the node publishes in /fleet/health, the name results[].placement uses, for a run on a fleet node this delegator dispatched; a route:\"queue\" answer carries the id its claimant stamped on it, and a run on this box's own seat (ran_on \"local\") carries this box's own name, or the box whose engine ran it. results[].node_reported appears only when the node's own result named itself by another id (a node on a build before 0.185.0, or a local run attributed to another box's engine, where it is the runner's own stamp), and is absent when the two agree or no node took the subtask. results[].acceptance_lint (warn-only, the run still happened) flags acceptance that verifies less than it looks: PARROT-PASSABLE (every content check also matches the goal text, so an echoed question passes as verified and the retry never fires), UNGROUNDED (a contains:/regex: matching nothing in the contract's own context docs — fails right answers), or SHAPE-ONLY (nonempty:/min_items: alone — passes garbage). When present, fix the acceptance (anchor >=1 contains:/regex: to content that appears only in the docs) before reusing the contract. summary.infrastructure counts the results whose story is a broken STACK rather than the work: defers whose defer_class is infrastructure|config, plus a local placement taken while every configured remote failed its health probe. Non-zero means a node is broken or misconfigured, so do NOT read those subtasks as work the local stack honestly could not do — and when NOTHING succeeded the call comes back flagged as an error, with this same JSON body intact (a PARTIAL result, where some subtasks delivered, is a successful call: read summary.failed / lost_to_stack and each result's failed / defer_class / reason for what is missing). defer_class \"contract\" is YOUR contract, not a box: no output_schema for a remote placement, past the origin hop, or bigger than any node's advertised context — rewrite the contract and retry. abstention|budget defers and failed_verification are ordinary result shapes. The whole call has a deadline below the MCP client's abort (agent_call_deadline_sec, default 1,500 s, counted from arrival): at it the finished subtasks' results come back and every unfinished one is a budget defer whose reason opens `call deadline reached; N unfinished` — a result shape, not a failure; re-issue those subtasks in a smaller call. WRITE DOOR (0.122.0): add write_root to a subtask to hand a seat a small IMPLEMENTATION leg — it gets write_file/edit_file inside that directory of the NODE'S OWN copy of your inlined docs, and the result carries diff + diff_files, a unified patch YOU review and apply (the harness never applies it; that is what makes handing a 4B an edit safe). Needs agent_allow_write on the executing node, else the subtask is refused and re-placed, or defers with defer_class write. Caps: 8 files, 64 KiB written, 192 KiB of diff, and no delete/shell/run/network — past a cap nothing is published at all. Verify it with diff_touches:<path-prefix> and diff_max_files:<n>, which read the write set rather than the prose; neither says the change is CORRECT, so read the diff. On any refusal before placement it returns deferred:true with a reason and you do the work yourself. A subtask that fails intake (a context_paths file that is not there, a context past the cap, a blank goal) is deferred alone, with defer_class contract and a reason opening `subtask N:`, while its siblings run: results keep the order of the subtasks you sent, so read each result's own deferred field. Only a call with no valid subtask, or with more than 8, is refused whole. Set lint_only:true to preview without dispatching: the call returns each subtask's acceptance_lint and the placement the deal would give it right now, and runs no model, takes no lease and starts no job.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"subtasks":{"type":"array","minItems":1,"maxItems":8,"description":"the delegation contracts to place and run","items":{"type":"object","properties":{"goal":{"type":"string","description":"the self-contained task for the sub-agent (it sees ONLY this + the context docs)"},"context":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"},"text":{"type":"string"}},"required":["name","text"]},"description":"inline context documents (name is a flat filename; total across docs <= the box's cap: 256 KiB, which a composite box raises to its largest layer window x 3 bytes)"},"context_paths":{"type":"array","items":{"type":"string"},"description":"files to inline as context docs, read by the DELEGATOR under read_root confinement (<=128 KiB each)"},"output_schema":{"type":"object","description":"JSON Schema with a properties map; the sub-agent's final answer is re-packed into it. REQUIRED for any remote placement"},"acceptance":{"type":"array","items":{"type":"string"},"description":"machine-checkable checks evaluated delegator-side: contains:<s> | not_contains:<s> | regex:<re> | min_items:<field>:<n> | nonempty:<field> | nonempty:<f1>|<f2> (at least one named field present and non-empty) | diff_touches:<path-prefix> | diff_max_files:<n> (the last two read the write set of a write_root subtask and FAIL on an empty one)"},"profile":{"type":"string","description":"agent task profile. Default = the EXECUTING box's configured agent_profile, else general — a per-SEAT property: small tiers seed research (narrowing measured 0%->72% there), big planners run un-narrowed (research measured 94% and 5x slower vs general 100% on the 27B). Omit unless the task genuinely needs a specific toolset"},"max_steps":{"type":"integer","description":"loop step budget (default 12, cap 12)"},"write_root":{"type":"string","description":"opens the WRITE door (0.122.0): a directory RELATIVE to the run's read root that the seat may create and change files under, inside the executing node's own throwaway copy of the inlined context docs — relative because the node never sees your filesystem. The result carries diff + diff_files, a unified patch (git apply -p1) that YOU review and apply; the harness never applies it. Requires agent_allow_write on the executing node. Grants create+overwrite only: no delete, no shell, no run, no network. Caps 8 files / 64 KiB written / 192 KiB of diff — past any of them NOTHING is published and the subtask defers with defer_class write. Omit for read-only work, which is everything else"},"setup_actions":{"type":"array","maxItems":8,"items":{"type":"object","properties":{"tool":{"type":"string"},"args":{"type":"object"}},"required":["tool"]},"description":"tool calls the EXECUTING seat replays before its first turn (ADR 0036 P2), e.g. read_file of a context doc by its name — the model's first turn then already holds the document instead of spending two steps finding it (the corpus's dominant 4B failure shape). Not charged to max_steps. A node with agent_seed_context_reads on prepends one read_file per context doc itself; a node one release behind ignores this field and reports no setup_ran"},"thinking":{"type":"string","enum":["auto","on","off"],"description":"planner think-block policy on the EXECUTING seat (0.115.8): auto (default = the executing box's agent_thinking) thinks every step and re-issues an empty final ONCE with thinking off at 4x the step budget, then defers as reasoning_starved/empty; off renders every planner call in non-thinking mode (grounded extraction on a thinking seat); on never sends the kwarg. results[].calls carries per-completion finish_reason / completion_tokens / reasoning_tokens; results[].stop_note the starvation arithmetic"},"context_class":{"type":"string","enum":["","long"],"description":"long = ask for the box's biggest long-context layer (the three-card seat where a box declares one, otherwise the pair's 262k seat) under its display-floor, host-RAM and presence guards and a prefill feasibility check; omit for the default placement"},"layer":{"type":"string","description":"run this subtask on the NAMED layer's agent seat (a layer id a composite node declares, e.g. fast = a one-card box's 35B digest seat): the placement table decides FOR that layer — a node that does not declare it is ineligible for this subtask, an idle local box that does not declare it does not keep it, and when no node declares it the subtask defers naming the layer (never a silent run on the planner seat). Use for digest / extract / summarize-shaped contracts where the measured fast seat is adequate (blind coverage 4.65 vs the default's 8.53 — judgment and coverage contracts stay on the default). Omit for the default placement"},"allow_browse":{"type":"boolean","description":"grant the browse tool (ADR 0060) on the executing seat: it may drive that machine's own browser through its browse lane. Admitted only with route local, this node's agent_allow_browse, a configured lane and browse_hosts; judged unattended (publish/send-class controls always refused), audited. The grant pins the whole call to this box: its pin_reason is locality, implied when you give none"},"browse_hosts":{"type":"array","items":{"type":"string"},"description":"with allow_browse: 1-32 bare host names (subdomains included) the browse tool may visit; required"},"timeout_sec":{"type":"integer","description":"EXECUTION budget per subtask (default 300, cap 900), shared by every placement it makes: a cross-seat retry and any re-placement after a node refuses the job run only inside what is LEFT of it, and are skipped with a note under the retry floor (10 s, or the delegator's agent_retry_min_sec); a retry is also skipped after an empty final and never lands on a seat already running another job. Not an end-to-end wall: placement overhead (fleet health probe, dispatch dial) and time the job provably spent queued on a node are bounded but not charged to it, so observed wall can exceed this"}},"required":["goal"]}},"route":{"type":"string","enum":["auto","spread","local","remote","queue"],"description":"placement: auto (default; idle-local wins, busy-local considers remotes, and a local seat whose load would unload another loaded vLLM seat counts as busy; a subtask that names a layer the local box does not declare is not idle-local work), spread (deal the subtasks across the local seat AND every eligible fleet node, concurrently — use for any fan-out of 2+ contracts. The deal is deterministic: subtask 0 ALWAYS lands on the local seat, so a 2-contract spread with an eligible remote is guaranteed one local + one remote — the local+server pair, unless a subtask names a layer the local box does not declare: that one never takes the local slot. The REMOTE slots are SHAPE-SCORED from the goal text, no model call: reasoning-shaped goals (explain/why/trace/compare/across these files) take the roomiest eligible seat, mechanical ones (extract/list/count/summarize/how many) take the eligible seat expected to FINISH first (its queue wait, cold load and generation time at its measured rate; the size of its window only breaks a tie, and decides alone when no seat publishes a rate), a goal matching neither reads as mechanical, and equal seats rotate — phrase the goal with the verb you mean. Eligibility is PER SUBTASK — a contract with no output_schema, or too big for every node, silently deals local; read results[].placement to confirm the pair landed), local and remote (a PIN only with a pin_reason: local runs on this box's seat and nowhere else, never retried on another node; remote runs on a fleet node and nowhere else, its failed-verification retry goes to another fleet node or is skipped and never lands on the local seat, and it defers if none is eligible; WITHOUT a pin_reason they are hints that placement may override: local is placed as auto places it, so the idle local seat takes its run-cap line and the rest of a wider call goes to the fleet, and remote is placed fleet-first with the local seat as the fallback; each result's placement says whether the hint was honoured or overridden), queue (ADR 0030, DARK unless fleet_queue_holder is configured: submit every subtask to the consolidated pull queue and let claiming nodes take them — durability lives on the holder; requires output_schema on every subtask). A subtask whose answer fails acceptance (or abstains) is retried once on a different node and the better attempt is published (retried_on / retry_note). A box with no agent seat (a delegation client: agent_model and model both empty) is never a placement: auto and spread send every subtask to the fleet"},"pin_reason":{"type":"string","enum":["privacy","locality","measurement","operator"],"description":"why this call is pinned to route local or remote; without it those routes are only hints that placement may override. Only a pin can have a reason: route auto, spread and queue refuse one, and so does a reason the route does not take. privacy: the material may not leave this box (credentials, unreleased or brand-isolated data); route local only. locality: the work needs something only this box has (a local service, files the contract cannot carry inline, this box's own seat as the thing under comparison); route local only. measurement: the call measures a specific place (a benchmark, a bake, a seat A/B); route local or remote. operator: the human operator typed this placement in this conversation (a subagent, workflow stage or script never has it, and a claimed reason is still bounded by the pinned seat's line); route local or remote. A pin is authoritative to the end of the subtask (a remote pin's retry stays on a fleet node) and its reason is recorded on the ledger row and on every result"},"read_root":{"type":"string","description":"absolute directory context_paths may be read from (default: the server working dir)"},"remotes":{"type":"array","items":{"type":"string"},"description":"OPTIONAL narrowing of the configured fleet: base URLs (e.g. http://node-c:18811), each of which must already be in this server's delegate_remotes. A call can only narrow the fleet, never add a node: a URL outside delegate_remotes is refused and never dialled, and a box with no delegate_remotes accepts none. A list here REPLACES the roster for this call, so a one-node list confines every subtask of the call to that node. Omit it to use every configured node"},"priority":{"type":"integer","enum":[-1,0,1],"description":"scheduling band for every subtask (default 0 = production). -1 = SHEDDABLE: measurement/gate traffic that takes idle fleet capacity only — a node without an idle execution slot refuses it and the subtask is re-placed; with no idle node anywhere it is shed at once (deferred, defer_class capacity) instead of waiting or queuing before/behind production work. 1 = urgent (claimed first on every node). Band-0 subtasks that find every node full WAIT for capacity until the call's deadline less a short reserve (agent_call_deadline_sec; not charged to timeout_sec; agent_placement_wait_sec bounds only calls that have no deadline) and land on the first node that frees; summary.waited / results[].capacity_wait_sec report it"},"lint_only":{"type":"boolean","description":"Set lint_only:true to preview without dispatching: the call returns each subtask's acceptance_lint and the placement the deal would give it right now, and runs no model, takes no lease and starts no job."}},"required":["subtasks"]}`),
		}, s.handleAgentDelegate)

		// offload_research (2026-08-30): the one-call research lane. Same gate as
		// agent_delegate — it IS a delegation, with the fetch done delegator-side —
		// so tools/list stays byte-identical when the delegator role is off.
		srv.AddTool(&mcp.Tool{
			Name:        "offload_research",
			Description: "RESEARCH on the FREE local seats: hand over URLs + a goal; the harness fetches each page DELEGATOR-side (public http/https only — loopback, private, tailnet hosts are refused), strips it to text, and fans one digest contract per page across the fleet (route spread by default; a local or remote route pins the call only with a pin_reason, and without one it is a hint placement may override). The seats never touch the network; your context never pays for the pages. This is the lane for any leg that would otherwise be a cloud research subagent — WebSearch to find URLs, then call this. Each contract is written the way the seats pass (the document is named as already provided) and acceptance is anchored to a token that appears only in the page, so an echoed goal cannot pass as verified. Returns {summary, partial?, error?, results:[...agent_delegate result rows, one per usable source, in source order], result_sources:[source index per result], sources:[{index,url,final_url,title,status,bytes,text_bytes,truncated,error,error_kind,cleanup_error,doc_name,anchor,skipped}]} — the digests come before the sources on purpose. Read summary FIRST; a PARTIAL result (some pages digested, some failed) is a successful call — the error flag is set only when nothing succeeded — so read summary.failed and each result's reason for what is missing; the same whole-call deadline as agent_delegate applies (the page fetch included): at it the finished digests come back and the unfinished pages are `call deadline reached` budget defers; a source with `skipped` produced no result (fetch failed, non-public host, unsupported content type). Two text sources are read as well as pages, both fetched here with no login and no cookies: an RSS or Atom feed (its items' titles, links, dates and summaries become the text; item links are not followed, list them yourself to read them) and, only when the operator has set ytdlp_path, a YouTube watch, youtu.be, shorts, live or embed URL (its subtitles, not its audio; sub_langs picks the languages; a video with none, or a yt-dlp failure, is a skipped source whose error_kind says which; a source with cleanup_error read fine but left its scratch folder behind). A page whose contract fails intake is deferred alone the same way (defer_class contract, reason opening `source N:`) and keeps its place in results and result_sources. Default output_schema {key_facts[],numbers[],quotes[],verdict} — pass your own to shape the digest; pass questions to add per-page asks. Each digest's quotes are looked up in the page they came from: a quote found only once case, spacing, quote marks and dashes are set aside is replaced by the page's own wording, and a quote not on the page is listed in the row's quote_check.unverified (flagged, never failed or dropped). Caps: 12 URLs per call, 2 MiB fetched / 96 KiB text per page, 30 s per fetch.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"goal":{"type":"string","description":"what to extract from EVERY page, self-contained (the seat sees only this + the page text)"},"urls":{"type":"array","minItems":1,"maxItems":12,"items":{"type":"string"},"description":"public http(s) URLs to fetch delegator-side"},"questions":{"type":"array","items":{"type":"string"},"description":"optional extra asks appended to the goal for every page"},"output_schema":{"type":"object","description":"JSON Schema with a properties map for the per-page digest (default: key_facts/numbers/quotes/verdict)"},"acceptance":{"type":"array","items":{"type":"string"},"description":"optional extra delegator-side checks (contains:/not_contains:/regex:/min_items:/nonempty:) appended to the grounded default"},"route":{"type":"string","enum":["auto","spread","local","remote"],"description":"placement (default spread: pages are dealt across the local seat and every eligible fleet node). local and remote are a PIN only with a pin_reason; without one they are hints that placement may override (local is placed as auto places it, so the idle local seat takes its run-cap line and the rest of the pages go to the fleet; remote is placed fleet-first with the local seat as the fallback), and each result's placement says whether the hint was honoured or overridden"},"pin_reason":{"type":"string","enum":["privacy","locality","measurement","operator"],"description":"why this call is pinned to route local or remote; without it those routes are only hints. Only a pin can have a reason: route auto and spread refuse one, and so does a reason the route does not take. privacy: the material may not leave this box (credentials, unreleased or brand-isolated data); route local only. locality: the work needs something only this box has (a local service, files the contract cannot carry inline, this box's own seat as the thing under comparison); route local only. measurement: the call measures a specific place (a benchmark, a bake, a seat A/B); route local or remote. operator: the human operator typed this placement in this conversation (a subagent, workflow stage or script never has it, and a claimed reason is still bounded by the pinned seat's line); route local or remote. A pin is authoritative to the end of the subtask (a remote pin's retry stays on a fleet node) and its reason is recorded on the ledger row and on every result"},"timeout_sec":{"type":"integer","description":"per-page contract budget (default 300, cap 900)"},"fetch_timeout_sec":{"type":"integer","description":"per-page fetch timeout (default 30; 60 for a YouTube subtitle fetch, which is capped at 900)"},"sub_langs":{"type":"string","description":"YouTube subtitle languages, comma separated, a regex allowed per entry (default en.*); used only for YouTube URLs when the operator has set ytdlp_path"}},"required":["goal","urls"]}`),
		}, s.handleResearch)
	}

	// Accelerator tools (ADR 0024): registered ONLY when the box lists a device,
	// so tools/list is byte-identical without one — the same pin as
	// agent_delegate. Each device's table (acceltools.go) maps 1:1 to its
	// sidecar tools; ownership is exclusive (the GPU VLM never serves these),
	// and across devices the shared-name rule applies: config.Accelerators is
	// walked in order and the FIRST listed owner of a capability name registers
	// it (Coral D5; see docs/systems/accelerators.md).
	if s.p != nil {
		s.registerAccelTools(srv, s.p.Cfg())
	}

	return srv
}

// --- tool handlers (named methods so they are directly unit-testable) ---

// jsonResult marshals an arbitrary payload (NIM is not a core.Result) into a
// single MCP text-content result.
// hailoSidecar lazily builds the accelerator lane from config: one client, one
// spawn function (nil when hailo_sidecar_cmd is unset), one Sidecar shared by
// every NPU tool so concurrent first calls share a single spawn.
func (s *Server) hailoSidecar() *accelclient.Sidecar { return s.accelSidecar("hailo-8l") }

// hailoCall is the one path every NPU tool takes: ensure the sidecar, call the
// tool, pass the dict through. Transport/spawn failures become defers (the
// caller does the work another way); the sidecar's own structured refusals
// ({"error":true,"kind":...}) pass through untouched — they are results.
func (s *Server) hailoCall(ctx context.Context, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	return s.accelCall(ctx, "hailo-8l", tool, args)
}

// handleHailoTool adapts one sidecar tool to an MCP handler. The MCP argument
// names are the sidecar's keyword names, so the JSON passes through unchanged.
func (s *Server) handleHailoTool(tool, requiredArg string) mcp.ToolHandler {
	return s.handleAccelTool("hailo-8l", tool, requiredArg)
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
}

// jsonResultFirst is jsonResult with one key spliced in FRONT of the object.
//
// encoding/json marshals a map in sorted key order, so a key that must be read
// before anything else cannot be placed by adding it to the map. v must marshal
// to a JSON object; anything else is returned unchanged rather than corrupted.
func jsonResultFirst(key string, val any, v any) (*mcp.CallToolResult, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	head, err := json.Marshal(map[string]any{key: val})
	if err != nil {
		return nil, err
	}
	joined := body
	switch {
	case len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}':
		// Not an object: leave it alone rather than produce invalid JSON.
	case len(body) == 2: // "{}"
		joined = head
	default:
		joined = append(head[:len(head)-1:len(head)-1], ',')
		joined = append(joined, body[1:]...)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(joined)}}}, nil
}

func result(r core.Result) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
}
