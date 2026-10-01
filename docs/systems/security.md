# House security standard

## Purpose

Hold every security-relevant change to one standard: which layer it belongs to, what it adds, its default, and
the gate that proves it. The standard also says when a control may start blocking work.

The security standard every part of the harness is held to: seven invariants, ten layers, the gates
that prove each control, and the order in which controls are promoted. It is the "standard security
framework" of [ADR 0067](../architecture/decisions/0067-house-security-standard.md). The
vendor-neutral checklist behind it is the Cloud Security Alliance's AARM v1.0 (requirements R1–R9);
the design rules come from NVIDIA's Open Agent Safety Platform reference; the portable patterns come
from NVIDIA OpenShell. No NVIDIA software is installed or run by this standard.

A control that adds failures gets switched off. So the standard carries a reliability track (R) and
refuses any gate that interrupts work before it has counted its own false positives.

## The seven invariants

| # | Invariant | What it means here |
|---|---|---|
| I1 | **Decide below, propose above.** | The party that decides sits outside the party that asks: the policy broker outside the model, hard-rule hooks outside the agent's write reach, audit heads outside the node. |
| I2 | **Tighten-only.** | Rules, monitors and delegated contracts only remove authority (ADR 0003, ADR 0036). Nothing auto-approves; an approval is one-shot, exact-scope and expiring. Risk signals only ever reduce authority. |
| I3 | **Fail loud.** | A missing, stale or unreadable control reads RED in the posture report, never a silent pass. It fails closed only where the failure is detectable and the operator has said so. |
| I4 | **Audit, then warn, then enforce.** | Every new rule logs would-be denials first. Promotion is a pull request carrying the counted would-denies and false positives (proposed bar: at least 14 days, at least 200 decisions, zero unexplained denials). No gate interrupts work without that data. |
| I5 | **Every control has a gate that can fail.** | A test that passes with the control and goes red under a mutation that removes it, with the mutation shown applied. |
| I6 | **Additive, flag-gated, reversible.** | Capabilities default off; each change reverts with one commit or one config key. No new daemon, listener, scheduler or route without the operator's yes. |
| I7 | **Public/private split.** | Framework docs in a public repository carry no hostnames, paths or secrets; per-node facts live in the private operations notes. |

## The layers

Each layer lists what exists today, what the standard adds, its default and the gate that proves it.

| Layer | Today | Adds | Default | Gate |
|---|---|---|---|---|
| **L0 Posture of the controls** | `local-offload doctor`, hook doctor | managed settings present; paused guards listed as PAUSED, not FAIL; fleet version skew; audit chain verifies; secret-shaped values in tracked files; skill drift | on demand, read-only, warn-only | each fixture fault turns its row RED, the repaired fixture turns it green (G6) |
| **L1 Identity and auth** | loopback listeners; one shared fleet bearer on the agent, vision, chat and kv-slot lanes; tailnet transport; `--listen-trusted-network` permits ONE address, never every interface (0.144.1) | tailnet grants by node tag with a tests block; Host/Origin/Content-Type checks; per-node then per-job tokens | Host check in warn mode; tokens flag-gated with a dual-accept window | a request with a foreign Host gets 403 (G7); a probe from an untrusted node to a personal fleet port is refused (G8); an empty-host listen is refused with the flag (shipped) |
| **L2 Tool policy** | `Policy.Decide` deny > ask > allow; tighten-only rules; unattended rules embedded; every `--allow-*` false | a read kind with a built-in secret floor; egress rules in an endpoint shape with an audit mode; structured refusals; a delegation subset check | audit first (I4) | rules golden tests plus mutation; read-floor fixtures with would-deny counters (G10) |
| **L3 Filesystem sandbox** | `os.Root`; `.git` denied; audit path outside the worktree; the write door returns a diff and applies nothing | the Windows decision only | — | the cage adversarial suite (G9) |
| **L4 Process sandbox** | Linux cage (user namespaces, seccomp KILL list, Landlock, rlimits); Windows Job Object plus a low-integrity token confines writes only | a seccomp delta; an executable pin in audit mode; closing the Windows gap | `run` and `run_shell` stay operator-flag-only and off on Windows until closed | adversarial suite plus a smoke of the allowlisted programs under the new filter; the same suite under WSL2 (G9) |
| **L5 Network egress and the model path** | no network in the cage; the `web_fetch` allowlist; three netguard guards (listen, tailnet, public web); ADR 0001 (never cloud) | exact-host credential binding (shipped 0.143.1); an audit-first allowlist for `offload_nim`'s base; the bare-client lint (this change); egress rules | binding and lint on; allowlist audit-first | hostile-base credential test plus mutation (G1, shipped); the AST lint fails on a new bare client (G2, shipped); a would-refuse counter for the nim base |
| **L6 Secrets** | keys from the environment only; a write floor; env allowlists; secret scan on Edit/Write | a read floor; a managed hard tier for the guards; no live credential in tracked settings; guard-override hygiene | anything adopted ships with vendor telemetry off | tracked-file secret-shape scan; a posture row for managed settings |
| **L7 Audit and evidence** | broker audit on the CLI doors only | coverage on every agent door (flag `off / warn / enforce`); a per-run hash chain and a `verify` verb; an off-node head witness only with approval | coverage `off` until measured, then `warn` | audit allow-rows equal the effect-trace count on a delegated write-door run (G3); editing, deleting or reordering a row fails verify naming run and seq (G4); 8 concurrent writers keep every chain valid without a global lock (G5) |
| **L8 Supply chain and provenance** | pinned Go deps; installer SHA-256; `npm ci --ignore-scripts` for the compose lane; a pre-commit secret scan | a provenance manifest and drift report for skills, plugins, MCP servers and desktop apps; the admission checklist below for any new third-party software; a local vulnerability scan; any adopted binary pinned by tag and sha256, never `curl \| sh` | read-only report; admission is operator-approved, one artifact at a time | the drift check goes RED on a modified skill (G12) |
| **L9 Human approval and kill** | unattended Ask = deny and queue; no cancel route | structured refusal bodies; an authenticated cancel route and pause flag (approval item); approve-and-replay only on measured need | — | an unauthenticated cancel gets 401 or 403; an expired one-shot grant is refused |
| **R Reliability** | the busy hold (ADR 0061), the seat-cap FIFO, the parity deploy | the placement and re-pack work of the 15-change reliability plan | — | the replay gate (G11): a promotion to enforce needs the harness itself to be reliable |

## Admitting third-party software (L8)

No third-party tool, model weight, plugin, MCP server or desktop app is installed, bound or run until
it has passed these checks and the operator has approved it. The checks are read-only research. They
download nothing new (a hash is taken only of a file already fetched for that purpose, never extracted
or run), and nothing in them approves anything (I2).

| # | Check | Who |
|---|---|---|
| 1 | Maintainer identity: account and organisation age | reviewer |
| 2 | Repository age, last push, contributor count (complete or windowed) | reviewer |
| 3 | Two issue searches per repository, one for malware terms and one for telemetry and credential terms, read against their total counts | reviewer |
| 4 | Advisories: the repository's advisories page, the GitHub Advisory Database (malware and reviewed types), deps.dev advisory keys | reviewer |
| 5 | Signing in three layers: Authenticode or notarisation, update-channel signatures, build attestations or package provenance | reviewer |
| 6 | At least two hash channels for any binary, with the forge's own per-asset digest as the independent one | reviewer |
| 7 | The licence text as written, not a card tag: a class enters a licence map only from a text read in full | reviewer fetches; the operator (or counsel) reads terms that carry conditions |
| 8 | Package publisher continuity between the first and the latest versions, and lifecycle hooks in the manifest | reviewer |
| 9 | Namesake and lookalike search | reviewer |
| 10 | A pattern scan of install hooks and code for anything that runs on install | reviewer |
| 11 | A malware-scanner lookup by hash (VirusTotal), and the install approval itself | **operator only** |

The result is a verdict per artifact (no finding, concern, or reject) with the evidence for each check.
A concern goes to the operator; the reviewer never resolves it. For an MCP server the artifact is the
server's command, package and version; for a desktop app it is the installer and the binaries it
places.

## Named surfaces

- **The research-digest channel.** `offload_research` digests are written by local seats from
  third-party pages, so the text a caller reads is third-party content even though a seat wrote it,
  and an instruction planted on a page can survive into a digest. The agent's own `web_fetch` and
  browse tools fence page content as untrusted data (`fenceUntrusted`, `sanitizeUntrusted` in
  `internal/agent`); the research door does not yet label or bound its digests. Gate G13 (open): a
  digest result carries the untrusted label and a length bound, and a test fails when a page's
  planted instruction reaches the result without them.

## AARM v1.0 coverage

R1–R6 are MUST, R7–R9 SHOULD.

| Req | Coverage |
|---|---|
| R1 pre-execution interception | the broker for effectful tools; the model path through the credential binding and the bare-client lint |
| R2 context | partial: the broker sees the tool call and the rule table, not the conversation |
| R3 intent-aware policy | deliberately structural only (the rule table), never a model's judgment of intent |
| R4 decisions | ALLOW, DENY, ASK and a queued DEFER; no MODIFY (not needed) |
| R5 tamper-evident receipts | the per-run hash chain (L7) |
| R6 identity-bound receipts | per-node tokens (L1) |
| R7 drift | the posture and skill drift reports, on demand only |
| R8 standard export | deferred until a consumer exists |
| R9 least privilege | flags off by default plus the delegation subset check |

## Promotion classes

- **Default on now** (they grant nothing, only tighten or inform): the credential binding, the
  bare-client lint, this standard, the reliability fixes, the posture report.
- **Audit or warn first:** audit coverage on every door, egress rules, the read floor, Host checks,
  the executable pin, the nim base allowlist.
- **Opt-in flag, off until measured:** the audit chain, per-node tokens, the cancel route, an
  OpenShell backend, WSL2 dispatch.
- **Approval-gated, not built without the operator's yes:** any new service, listener, route or
  off-node endpoint (the head witness, the cancel route's exposure, continuous drift monitoring, an
  OpenShell gateway).

## The gates

| Gate | Control | State |
|---|---|---|
| G1 | hostile-base credential test plus mutation | shipped (0.143.1) |
| G2 | the bare-client AST lint (`bare_http_client_lint_test.go`) | shipped (this change) |
| G3 | audit allow-rows equal the effect-trace count | open (L7) |
| G4 | chain tamper tests | open (L7) |
| G5 | 8-writer concurrency, no global lock | open (L7) |
| G6 | posture check RED on injected faults | open (L0) |
| G7 | Host/Origin/Content-Type rejects | open (L1) |
| G8 | negative reachability from untrusted nodes | open (L1, needs the operator's tailnet policy) |
| G9 | cage suite plus allowlisted-program smoke, also under WSL2 | partial (the Linux suite exists) |
| G10 | rules golden tests and read-floor fixtures with would-deny counters | open (L2) |
| G11 | the replay gate for the reliability track | open (R) |
| G12 | skill manifest drift | open (L8) |
| G13 | research digests labelled untrusted and length-bounded | open (L5) |

## What was evaluated and not adopted

- **NVIDIA DOCA (SDK and services).** Every sample needs a BlueField DPU or a ConnectX adapter;
  none exists in the fleet, and its licence limits it to NVIDIA-device systems. Three designs
  transfer: baseline-and-re-attest, an off-node witness for audit heads, and default-deny
  segmentation enforced outside the workload.
- **The in-silicon monitor of the Open Agent Safety Platform.** It exists as announcements only,
  with no repository, specification or date. Its design rules transfer: decide below and propose
  above, risk signals only reduce authority, fail safely, and keep immutable records below the
  boundary.
- **The OpenShell runtime.** It targets agents you do not control. The harness's own agents already
  run through the policy broker and, for the operator-enabled run tools, the Linux cage. The runtime
  would add a gateway daemon, container-group access and vendor telemetry that is on by default.
  About ten of its patterns are ported instead (credential binding to declared endpoints, audit
  mode before enforcement, the endpoint shape of egress rules, structured refusals). A time-boxed
  trial of the runtime is an approval item.

## Source map

| Area | Code |
|---|---|
| policy broker, rule table, audit log | `internal/agent` (`Policy.Decide`, the rules and the audit writer) |
| cage (Linux) and confinement (Windows) | `internal/sandbox`, `internal/agent` run tools |
| listen, tailnet and public-web guards | `internal/netguard/netguard.go`, `tailnet.go`, `publicnet.go` |
| credential binding for the NVIDIA key | `internal/nimclient/nimclient.go` (`IsHostedNVIDIA`, `KeyForBase`) |
| bare-client gate (G2) | `bare_http_client_lint_test.go`, `bare_http_client_allowlist_test.go` |
| untrusted-content fences | `internal/agent/fetchtool.go` (`fenceUntrusted`), `internal/agent/browsetool.go`; none yet in `internal/research` (G13) |
| fleet node listeners and the fleet bearer | `internal/fleetnode/server.go`, `main.go` (`fleetServeParams`) |
| posture report | `main.go` (`doctor`) |

## Related

- [ADR 0067 — the house security standard](../architecture/decisions/0067-house-security-standard.md)
- [ADR 0001 — defer, never a cloud fallback](../architecture/decisions/0001-defer-never-cloud-fallback.md)
- [ADR 0003 — the policy broker, capability flags off by default](../architecture/decisions/0003-policy-broker-and-capability-flags-off-by-default.md)
- [ADR 0036 — the agent lane is a harnessed environment](../architecture/decisions/0036-the-agent-lane-is-a-harnessed-environment.md)
- [ADR 0042 — no bare HTTP client on a caller-named host](../architecture/decisions/0042-no-bare-http-client-on-a-caller-named-host.md)
- [systems/coding-agent.md](coding-agent.md) — the broker, the cage and the write door
- [systems/fleet-node.md](fleet-node.md) — listeners and the fleet bearer
