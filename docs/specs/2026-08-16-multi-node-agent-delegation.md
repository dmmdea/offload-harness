# Multi-Node Sub-Agent Delegation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The delegation cascade can hand agent-loop subtasks optimally to Qube alone, Qube+Lenovo, or Lenovo alone — over the tailnet, authenticated, quality-first — so a weak-but-real second local agent (lenovo-ampere6, RTX 3050 6GB) does meaningful work without ever costing delivered quality.

**Architecture:** Three additive layers on the existing fleet+agent seams. (A) *Remote seats*: any model seat can resolve to a remote OpenAI-compatible base over the tailnet. (B) *AGENT fleet jobs*: a new `agent` task type carries a self-contained, versioned **delegation contract** (goal + inline context + output JSON Schema + acceptance checks + ceilings); the remote node executes it with its own local `agent.Build` loop and returns a versioned result or the harness's structured defer. (R/T) *Routing + surfaces*: a hard capability gate decides node placement from extended `/fleet/health`; delegation is reachable from the MCP surface and (bounded, hop-limit-1) from inside the agent loop itself. The full 27b decomposing-orchestrator (research option C) is explicitly deferred — the caller decomposes; the harness routes, executes, verifies, merges.

**Tech Stack:** Go (stdlib net/http, existing internal/fleetnode + internal/agent + internal/core), llama-swap serving on both nodes, Tailscale MagicDNS transport.

**Spec:** this document, §S (spec) below. Research basis: nightshift-6 synthesis (subagent-fleet-research workflow, 2026-08-16) + the ground-truth seam map (Explore agent, same night, file:line anchors used throughout) — both summarized in `G:\My Drive\AI Ecosystem\Ecosystem\Benchmarks and Optimizations\2026-08-16-nightshift6-notes.md`.

## Global Constraints

- **Tailnet-only, authenticated (operator constraint, verbatim: "make sure a+a communication is safe and uses our tailnet"):** fleet listeners bind loopback or a tailscale address only (existing netguard posture, ADR 0005); the agent lane additionally REQUIRES `fleet_auth_token` — unset token ⇒ agent dispatch refused loudly on any non-loopback listener. Bearer token on every fleet mutating call when configured. No TLS added in v1: the tailnet is WireGuard-encrypted; record in the ADR.
- **Never-cloud (ADR 0001) carve-out stated, not weakened:** a self-hosted fleet node on the operator's tailnet is LOCAL infrastructure, not cloud. Delegation may never target anything outside the tailnet CGNAT range (reuse `ingress.go`'s `100.64.0.0/10` + loopback check for delegation targets).
- **Quality-first (operator, verbatim: "not a race about speed, its all about delivered quality and precision with efficiency"):** the weak node receives work ONLY through the hard gate (§S3); weak output merges ONLY after mechanical verification; raw remote transcripts are NEVER spliced into a caller's context (poisoning quarantine).
- **Additive + reversible:** every existing path (media fleet, local agent_run, cascade) must behave byte-identically when the new config keys are absent. Pinned by tests.
- **Hop limit 1:** a remotely-executed agent NEVER gets the delegate tool. Enforced structurally (tool not registered when depth>0), not by prompt.
- **No new deps.** Go stdlib + existing internals only.
- **Version 0.65.0** on completion (minor: new capability, additive). Bump ritual: VERSION + main.go const + .printing-press.json + CHANGELOG in one commit; `go test -count=1 .` after, unpiped.
- **Do NOT build (research-killed, do not resurrect):** llama.cpp RPC/exo tensor distribution · A2A protocol adoption (steal its state *semantics* only) · MCP-tasks node-to-node wire · durable-execution/queue clusters · learned routers · cross-machine speculative decoding · weak-node-as-judge · voting ensembles · worker re-delegation.
- **Tier matrix:** arc 1 adds NO model seats — no matrix edit. (The Lenovo agent-seat MODEL is arc 2's decision; this build binds whatever `agent_model` the tier config names, today `offload-e4b`.)

---

## ROAST RESHAPE 2026-08-16 (five-persona council; BINDING deltas — these override the §S text below wherever they conflict)

1. **Task 0 runs FIRST — the kill-shot measurement.** Paired run of a representative
   contract-shaped task: Lenovo-served e4b (existing binaries, ssh tunnel) vs Qube
   qwen3.8-27b. If the Lenovo output is not merge-worthy, the arc collapses to Phase A +
   auth + health extension, and Phases B/R/T gate on arc-2's seat decision. Measured
   verdict recorded in the nightshift-6 ledger before Task 1 begins.
2. **Depth is DERIVED on the receiving node** — any contract arriving over the fleet wire
   executes at `effectiveDepth = max(1, wireDepth)`; tool registration keys off the derived
   value; the placement gate checks the REQUESTER's depth. The wire field is advisory.
3. **Remote placement REQUIRES `OutputSchema`**, and `Validate()` must confirm the schema
   compiles under the existing gbnf package's supported subset. `Acceptance` is redefined
   as machine-checkable delegator-side assertions — v1 DSL: `contains:<s>`,
   `not_contains:<s>`, `regex:<re>`, `min_items:<field>:<n>`, `nonempty:<field>` —
   EVALUATED BY THE DELEGATOR before merge. Free-prose acceptance no longer counts toward
   the verifiability gate. (Closes the wrong-valid-schema hole: constraint-tax evidence.)
4. **Wire posture: tolerant reader.** Unknown fields are IGNORED (forward compat across
   staggered node deploys); `schema_version` mismatch defers loudly. Size/count caps stay
   strict. The Task-1 unknown-field-rejection test is REPLACED by an unknown-field-ignored
   test.
5. **Ctx honesty:** remote `MaxSteps` cap = **12** (not 24). EstTokens = tokenized
   upper bound (conservative chars/3 in v1, labeled as such), `specReserve` a named
   constant covering system+tools+per-step transcript growth × MaxSteps. The effective doc
   budget at an 8k seat (~2-4k tokens) is documented in the OPERATOR-GUIDE table. The
   256KiB wire cap stays as a transport bound only.
6. **`context_paths` on the delegator surfaces:** `agent_delegate` (MCP) and the CLI accept
   file paths; the DELEGATOR harness reads and inlines them (size-capped, confined to the
   caller-supplied read root) so the calling session's context never pays. The wire
   contract remains inline-docs (self-contained).
7. **Busy-aware routing for the DAILY cascade lane (Phase A upgraded, first-class):** new
   opt-in `cascade_remote_lanes` — when the local machine-wide GPU lease is held and a
   configured remote lane roster-serves the seat's model, cascade text calls
   (summarize/classify/extract/triage) route to the remote lane; logged per-call. This is
   the invisible adoption path the Buyer demanded.
8. **Demand side ships in-arc:** `contracts/` template library (4 canned shapes:
   docs-drift-scan, bench-log-digest, schema-extraction, research-digest) + a Task-8
   deploy-time update to `~/.claude/rules/local-offload.md` naming the concrete triggers
   (multi-doc synthesis over unread files; subtasks while a render lease is held).
9. **Telemetry:** every delegation writes a ledger row (task=`agent_delegate`, placement,
   defer reason, wall_ms) visible in `local-offload stats`; plus the full (contract,
   placement, result, acceptance verdict) tuple appended under `BaseDir()/delegation-log/`
   — the standing small-model agent-task corpus (no sub-27B agent data exists anywhere;
   this accumulates it from real work).
10. **Auth scope v1 = the agent lane only** (dispatch of `task_type:"agent"` + polling its
    jobs). Media endpoints stay tokenless in v1 — the Aorus's deployed 0.62.1 media client
    must not 401. Full-fleet token enforcement is a recorded follow-up for a
    whole-fleet-deploy window.
11. **TailnetURL tightened:** allowed = loopback, 100.64.0.0/10 literals, and hostnames
    ONLY under the house tailnet suffix `.tail38a707.ts.net` or dotless names that resolve
    (checked in a custom DialContext at dial time) into 100.64.0.0/10. Generic `.ts.net`
    is NOT allowed (Funnel spoof surface).
12. **`delegate_subtask` (in-loop tool) is PARKED to v2.** v1 surfaces: MCP
    `agent_delegate` + CLI. (The placement gate still ships — it serves the MCP surface.)
13. **`agent_delegate` MCP registration is config-gated** (`agent_delegation_enabled`) so
    tools/list is byte-identical when off.
14. **Job protocol specifics:** delegator generates `job_id = "agd-" + ULID`; re-dispatch
    of the same job_id on transport doubt (202-reack semantics per existing store); poll
    deadline = contract TimeoutSec + 60s grace, then the delegator marks the subtask
    deferred (reason `poll deadline`) and STOPS polling. `agent_delegate`'s result
    summarizes `succeeded/deferred/failed` counts at the TOP — eight quiet defers must
    read as a loud outcome, not eight green jobs.
15. **ADR 0023 additionally records:** N-node door (Aorus joins by config — say it
    explicitly), bearer-token weaknesses (shared identity, no rotation) as accepted for a
    2-node personal fleet, and the llama-swap remote-lane bind change (Lenovo llama-swap
    re-binds to the tailscale address for Phase A; llama-swap itself is tokenless —
    accepted on the tailnet, noted).

## §S Spec — the locked design

### S1. Phase A — remote seat resolution (per-model endpoint overrides)

New config key `seat_endpoints` (map model→base URL, default empty):

```json
"seat_endpoints": { "lenovo-e4b": "http://lenovo-m720q:11436" }
```

`llamaclient` gains per-model base resolution: `Client.BaseFor(model)` consults the map (exact model id or llama-swap alias), else the default base. Every completion/embedding call site resolves through it. A remote base MUST pass the tailnet guard (loopback or 100.64.0.0/10 after literal-IP parse; MagicDNS hostnames allowed and resolved lazily by the HTTP layer — the guard checks the URL host is not a public FQDN/IP literal; a non-IP hostname is allowed only when it has no dots or ends in `.ts.net`).

This alone delivers: Qube's cascade or agent planner can use a Lenovo-served model as a lane ("b alone" for cascade calls), with zero job machinery. It also lets the delegator health-check a remote seat cheaply (roster fetch against the remote base — `swapclient.FetchRoster` already takes a base).

### S2. Phase B — the `agent` fleet task

**Wire contract (delegation request), versioned, strict-decoded:**

```go
// internal/core/agentwire.go
type AgentContract struct {
    SchemaVersion int               `json:"schema_version"`          // 1
    Goal          string            `json:"goal"`                    // required, self-contained
    Context       []ContextDoc      `json:"context,omitempty"`       // inline docs, total ≤ 256 KiB
    OutputSchema  json.RawMessage   `json:"output_schema,omitempty"` // JSON Schema for structured output
    Acceptance    []string          `json:"acceptance,omitempty"`    // human-readable checks, echoed to the sub-agent
    Profile       string            `json:"profile,omitempty"`       // agent profile name; default "research"-class read-only
    MaxSteps      int               `json:"max_steps,omitempty"`     // default 12, cap 24 for remote
    TimeoutSec    int               `json:"timeout_sec,omitempty"`   // wall ceiling, default 300, cap 900
    Depth         int               `json:"depth"`                   // 0 = origin; ≥1 ⇒ delegate tool NEVER registered
}
type ContextDoc struct { Name string `json:"name"`; Text string `json:"text"` }
```

**Wire result (versioned; the ONLY thing that crosses back):**

```go
type AgentWireResult struct {
    SchemaVersion int             `json:"schema_version"` // 1
    NodeID        string          `json:"node_id"`
    Seat          string          `json:"seat"`            // resolved planner model
    Output        string          `json:"output"`          // final assistant text
    Structured    json.RawMessage `json:"structured,omitempty"` // present iff OutputSchema given AND validated
    Steps         int             `json:"steps"`
    StopReason    string          `json:"stop_reason"`
    Deferred      bool            `json:"deferred"`
    Reason        string          `json:"reason,omitempty"`
    WallMs        int64           `json:"wall_ms"`
    TokensOut     int             `json:"tokens_out,omitempty"`
}
```

No transcript crosses the wire (quarantine by construction). `Structured` is produced by asking the sub-agent to answer under GBNF from `OutputSchema` on its FINAL synthesis step — v1 mechanism: after the loop returns, one constrained-grammar completion re-packs `Output` into the schema on the REMOTE node's seat; schema-validation failure ⇒ one retry ⇒ `deferred:true, reason:"output failed schema"`.

**Node-side execution:** new `core.TaskType` `TaskAgentRun = "agent"`; `fleetTaskOrder` gains `"agent"`; `taskConfigured` requires (a) `fleet_agent_enabled: true` (new key, default **false** — opt-in per node) AND (b) a resolvable agent seat. `BuildRequest` materializes the contract (strict decode, size caps, depth check) into a `core.Request{Task: TaskAgentRun, Input: goal, Params: {...}}`. `Pipeline.Run` routes `TaskAgentRun` to a new `runAgentTask` that: writes ContextDocs to a job-scoped temp dir (the `buildPipelineJob` mkdir/cleanup discipline, tasks.go:481), calls `agent.Build` with ReadRoot = that dir, profile from contract, `Unattended: true`, NO write/run/fetch/github tools, NO delegate tool, planner = the node's `agent_model`; runs; re-packs to `AgentWireResult`; returns it as `core.Result{Data: json.Marshal(result)}`. Job lifecycle: the EXISTING accepted|running|done|error store + poll endpoint (`/fleet/jobs/{id}`) — A2A's state semantics map onto it (submitted→accepted, working→running, completed→done, failed/rejected→error); no store rewrite (reshaped from research: steal semantics, skip schema churn). The contract's TimeoutSec is enforced with a context deadline in `runAgentTask` (closes ground-truth gap #12 for this lane).

**Auth middleware:** `fleet_auth_token` (config) — when set, `/fleet/dispatch`, `/fleet/jobs/*`, `/fleet/media/*` require `Authorization: Bearer <token>` (constant-time compare); `/fleet/health` stays open (capability advertisement is not sensitive; keeps dispatcher compat). The AGENT task additionally REFUSES dispatch when the node's listener is non-loopback and no token is set — checked in `taskConfigured` so the capability is not even advertised.

### S3. Phase R — routing: the hard gate and the decision

**Health extension (additive, omitempty — dispatcher decodes loosely per FLEET-NODE.md):**

```go
// added to healthPayload
AgentSeat      string `json:"agent_seat,omitempty"`       // resolved agent_model
AgentCtxTokens int    `json:"agent_ctx_tokens,omitempty"` // tier's serving ceiling
AgentResident  bool   `json:"agent_seat_resident,omitempty"` // roster-verified at snapshot time
AgentEnabled   bool   `json:"agent_enabled,omitempty"`
```

**The delegator-side gate** (pure function, unit-tested exhaustively):

```go
// internal/delegate/gate.go
type NodeView struct { // built from /fleet/health + local knowledge
    NodeID string; AgentEnabled bool; AgentSeat string; AgentResident bool
    AgentCtxTokens int; QueueDepth int; Local bool
}
type Subtask struct { Contract core.AgentContract; EstTokens int }
// Placement: which node runs it. Returns local when in doubt — quality-first.
func Place(st Subtask, local NodeView, remotes []NodeView, localBusy bool) NodeView
```

Hard gate for a REMOTE placement (every condition must hold): remote `AgentEnabled && AgentResident`; `st.EstTokens + specReserve ≤ AgentCtxTokens`; contract has `OutputSchema` or `Acceptance` (mechanical verifiability); `Depth == 0`. Soft tie-break (only among nodes passing the gate): prefer local unless `localBusy` (machine-wide GPU lease held — renders in flight — or local queue deeper); then lower QueueDepth. `Place` NEVER load-balances for speed alone: with an idle local node, work stays local. This is the "a alone / a+b / b alone" selector: N subtasks map over `Place` individually.

### S4. Phase T — surfaces

1. **MCP tool `agent_delegate`** (mcpserver): args `subtasks: [AgentContract...]` (1..8), `route: "auto"|"local"|"remote"` (default auto). Fans out concurrently (bounded), collects `AgentWireResult`s, returns per-subtask results + placement log (`node`, `seat`, `wall_ms` each — the mis-routing hygiene requirement). Local placements run the existing in-process `agent.Build` path (same contract semantics, no HTTP).
2. **Agent-loop tool `delegate_subtask`** (registered in builder ONLY when depth==0 AND config `agent_delegation_enabled: true`): the 27b-orchestrator lane, bounded. One contract per call; placement via the same `Place`.
3. **CLI verb `local-offload delegate --contract file.json [--route auto]`** for testing and scripts.

### S5. Verification, docs, deploy

E2E acceptance on the REAL fleet (Qube delegator → Lenovo node): a schema-outputting research subtask over inline context docs; assert structured output validates, placement log correct, defer path fires when the contract exceeds ctx, auth rejection fires without token, byte-identical media fleet behavior with keys absent. Docs updated same PR: `docs/systems/fleet-node.md`, `docs/systems/coding-agent.md`, `docs/FLEET-NODE.md` (contract + result wire tables), `docs/OPERATOR-GUIDE.md` (enable recipe), new ADR `0023-agent-lane-tailnet-auth-and-locality.md`. CHANGELOG + 0.65.0 bump. Deploy: Qube bin + Lenovo binary/render-tree (established pattern) + Lenovo `fleet_agent_enabled: true` + token on both + service restart + live e2e re-run.

---

# Tasks

### Task 1: `core` wire types + contract validation

**Files:**
- Create: `internal/core/agentwire.go`
- Test: `internal/core/agentwire_test.go`

**Interfaces:**
- Produces: `core.AgentContract`, `core.ContextDoc`, `core.AgentWireResult` (fields as in §S2, exact JSON tags), `core.TaskAgentRun core.TaskType = "agent"`, `func DecodeAgentContract(r io.Reader) (AgentContract, error)` (strict fields, size/count caps, depth/steps/timeout clamps), `func (c AgentContract) Validate() error`.

- [ ] Write failing tests: decode round-trip; unknown field rejected; context total >256KiB rejected; MaxSteps clamped to 24; TimeoutSec clamped to 900; missing goal error; TaskAgentRun accepted by `TaskType.Valid()`.
- [ ] Run tests, confirm fail. Implement. Run `go test ./internal/core/ -count=1`. Commit `feat(core): agent delegation wire contract v1`.

### Task 2: per-model seat endpoints (Phase A)

**Files:**
- Modify: `internal/config/config.go` (add `SeatEndpoints map[string]string \`json:"seat_endpoints,omitempty"\``, validation at load: every URL parses, host passes the tailnet guard)
- Create: `internal/llamaclient/endpoints.go` (`func (c *Client) BaseFor(model string) string`), wire into the completion/embedding call sites
- Create: `internal/netguard/tailnet.go` (`func TailnetURL(raw string) error` — loopback, 100.64.0.0/10 literal, dotless hostname, or `.ts.net` suffix pass; everything else fails)
- Test: `internal/netguard/tailnet_test.go`, `internal/llamaclient/endpoints_test.go`, config load test

**Interfaces:**
- Produces: `netguard.TailnetURL(string) error`; `Client.BaseFor(model) string`.
- Consumes: nothing from other tasks.

- [ ] Failing tests: TailnetURL accepts `http://127.0.0.1:11436`, `http://100.127.9.110:18811`, `http://lenovo-m720q:11436`, `http://qube.tail38a707.ts.net:11436`; rejects `http://example.com`, `http://8.8.8.8:80`, `https://api.openai.com`. BaseFor returns override for exact model else default. Config with a public URL fails load with a named error.
- [ ] Implement; full package tests; pin the absent-key path byte-identical (existing client tests still green). Commit `feat(client): per-model seat endpoint overrides, tailnet-guarded`.

### Task 3: fleet auth middleware

**Files:**
- Modify: `internal/config/config.go` (`FleetAuthToken string \`json:"fleet_auth_token,omitempty"\``), `internal/fleetnode/server.go` (wrap dispatch/jobs/media handlers; health stays open)
- Test: `internal/fleetnode/auth_test.go` (httptest)

**Interfaces:**
- Produces: bearer enforcement when token non-empty; 401 JSON error body `{"error":"unauthorized"}`; constant-time compare (`crypto/subtle`).

- [ ] Failing tests: token set ⇒ dispatch without header 401, with wrong token 401, with right token proceeds; token unset ⇒ current behavior unchanged (pin); health never requires auth.
- [ ] Implement; run `go test ./internal/fleetnode/ -count=1`. Commit `feat(fleet): bearer auth on mutating endpoints`.

### Task 4: node-side `agent` task (Phase B core)

**Files:**
- Modify: `internal/fleetnode/tasks.go` (add `"agent"` to `fleetTaskOrder`; `taskConfigured` branch: `cfg.FleetAgentEnabled && agentSeatResolvable(cfg)` AND (loopback listener OR token set); `BuildRequest` case calling `buildAgentRun` — strict-decode contract from payload, materialize ContextDocs to `<base>/pipeline-jobs/<job_id>/context/` via the Task-10 discipline of tasks.go:481 with cleanup closure)
- Modify: `internal/config/config.go` (`FleetAgentEnabled bool \`json:"fleet_agent_enabled,omitempty"\``, `AgentDelegationEnabled bool \`json:"agent_delegation_enabled,omitempty"\``)
- Create: `internal/pipeline/agenttask.go` (`runAgentTask` — contract deadline ctx, `agent.Build` read-only with ReadRoot=context dir, profile lookup w/ default `research`, structured re-pack via one GBNF completion on OutputSchema, `AgentWireResult` assembly; defer shapes for: seat unserved, schema fail after retry, loop budget, timeout)
- Modify: `internal/pipeline/pipeline.go` (route `core.TaskAgentRun` → `runAgentTask`)
- Test: `internal/fleetnode/tasks_agent_test.go`, `internal/pipeline/agenttask_test.go` (fake `agent.Client` — the 1-method Chat interface makes the loop fully fakeable; fake returns scripted tool-free completions)

**Interfaces:**
- Consumes: Task 1 types; Task 3 token config.
- Produces: dispatchable `task_type:"agent"`; `core.Result.Data` = marshaled `AgentWireResult`.

- [ ] Failing tests: agent task NOT advertised when `fleet_agent_enabled` false (default) — pins additive-off; advertised when enabled+seat+token; contract >256KiB context → 400; happy path through a fake Chat client returns `AgentWireResult` with Steps/StopReason populated; TimeoutSec ⇒ ctx deadline ⇒ deferred result, job state `done` (a defer is a SUCCESS shape — assert not `error`); depth≥1 contract still executes but (Task 6 asserts) no delegate tool exists.
- [ ] Implement; package tests; commit `feat(fleet): agent task type with delegation contract execution`.

### Task 5: health extension + placement gate (Phase R)

**Files:**
- Modify: `internal/fleetnode/server.go` (healthPayload agent fields, populated from config + a roster probe with the existing swapclient, cached alongside the VRAM snapshot cadence)
- Create: `internal/delegate/gate.go` (+ `internal/delegate/nodeview.go`: build NodeView from a health GET + local config; `LocalBusy()` = machine-wide GPU lease held probe using the existing gpulease read path)
- Test: `internal/delegate/gate_test.go` (table-driven: every gate condition flips placement; idle-local always wins; localBusy + passing remote → remote; no passing remote → local regardless)

**Interfaces:**
- Consumes: Task 4 config keys.
- Produces: `delegate.Place(st, local, remotes, localBusy) NodeView`; `delegate.FetchNodeView(ctx, base, token) (NodeView, error)`.

- [ ] Failing tests as above (≥10 table rows incl. ctx-fit arithmetic with specReserve). Implement. Commit `feat(delegate): capability gate + placement decision`.

### Task 6: surfaces — MCP `agent_delegate`, loop tool, CLI (Phase T)

**Files:**
- Modify: `internal/mcpserver/mcpserver.go` (register `agent_delegate`; handler: decode subtasks 1..8, route param, fan out via errgroup-style bounded concurrency using stdlib WaitGroup+channel, per-subtask `AgentWireResult` + placement entries; local placements call the same in-process path as `agent_run` with contract semantics)
- Modify: `internal/agent/builder.go` + create `internal/agent/delegatetool.go` (register `delegate_subtask` Tool ONLY when `cfg.AgentDelegationEnabled && contractDepth==0`; Exec marshals a contract with `Depth: 1`, places it, executes, returns ONLY the wire result JSON — never a transcript)
- Modify: `main.go` (verb `delegate`)
- Test: `internal/mcpserver/` handler test with httptest fleet node; `internal/agent/delegatetool_test.go` asserts the tool is ABSENT at depth≥1 and absent by default config
- Test: `refiner_cli_test.go`-style CLI smoke for the verb

**Interfaces:**
- Consumes: Tasks 1–5 all.
- Produces: MCP tool `agent_delegate`; loop tool `delegate_subtask`; CLI `local-offload delegate`.

- [ ] Failing tests; implement; commit `feat(surfaces): agent_delegate MCP tool, bounded loop delegate tool, CLI verb`.

### Task 7: docs + ADR + changelog + bump

**Files:**
- Create: `docs/architecture/decisions/0023-agent-lane-tailnet-auth-and-locality.md` (never-cloud carve-out reasoning; tailnet=WireGuard transport, bearer token, hop limit, quarantine; quality-first placement)
- Modify: `docs/systems/fleet-node.md`, `docs/FLEET-NODE.md` (contract/result wire tables, task_type agent, auth), `docs/systems/coding-agent.md` (delegate tool), `docs/OPERATOR-GUIDE.md` (enable recipe both nodes), `CHANGELOG.md`, `VERSION`→0.65.0, `main.go` const, `.printing-press.json`
- Run docs lint test (`TestDocsLint`, `TestTierDocsAreCurrent`), `go test -count=1 .` after bump (ritual), full `go test ./... -count=1`

- [ ] All green; two commits: `docs: multi-node agent delegation` + `chore: bump version to 0.65.0`.

### Task 8: live E2E on the real fleet + deploy (execution gated on operator's standing merge-when-green grant)

- [ ] clean-ship gates: semgrep changed paths; fresh-context review (`code-reviewer` + `silent-failure-hunter` — new fallback paths exist — + `pr-test-analyzer`, consequential: network + auth surface); fix rounds until a round returns nothing new.
- [ ] PR → CI green → merge → build+deploy Qube bin; deploy Lenovo binary (+ render tree pattern), set `fleet_agent_enabled: true` + `fleet_auth_token` (generated, same value both nodes, stored in each config), restart `offload-fleet-node.service`.
- [ ] Live acceptance: `local-offload delegate --contract <research contract w/ schema>` from Qube → placement=lenovo when Qube lease held / local when idle (force both ways); structured output schema-validates; auth negative test (curl without token → 401); `local-offload acceptance` READY both nodes; media fleet regression (one render job) unchanged.
- [ ] Measured numbers recorded in nightshift-6 notes: delegation round-trip overhead, Lenovo agent subtask wall-clock + quality spot-check vs the same contract run locally (paired), break-even verdict.

## Self-review (run at execution start)

Spec coverage: S1→Task 2 · S2→Tasks 1,3,4 · S3→Task 5 · S4→Task 6 · S5→Tasks 7,8. Type names used consistently (`AgentContract`, `AgentWireResult`, `TaskAgentRun`, `Place`, `NodeView`, `TailnetURL`, `BaseFor`). Open items an implementer must resolve against live code (NOT placeholders — decision recorded, site named): exact llamaclient call-site list for BaseFor threading (grep `c.base` uses); whether `swapclient.FetchRoster` needs a token-less remote variant (it does not — llama-swap has no auth); GBNF re-pack reuses the pipeline's existing gbnf package entry point.
