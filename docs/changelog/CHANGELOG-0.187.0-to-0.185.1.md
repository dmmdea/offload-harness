# Changelog archive: 0.187.0 to 0.185.1

Verbatim entries moved out of the root `CHANGELOG.md` so the file GitHub renders stays under its size limit.
History, never edited. Relative links inside the entries are written from the repository root.
Newest first; the live changelog is [`../../CHANGELOG.md`](../../CHANGELOG.md).

## [0.187.0] - 2026-10-14 - a lease that unloaded the seat no longer holds its cards idle while the host cannot meet its RAM, one measurement gate decides what is adopted, the compact tool schema cuts the advertised tool prose by a third, doctor names the engine answering each lane and warns on LAN-literal endpoints, offload_ask retries on another seat, offload_research reads feeds and subtitles, an ingress guard fronts the fleet node, two optional compaction rungs and a long-horizon context bench, a failure register and a trace-corpus readout, and a template-to-graph converter beside the builder drift guard

### Changed - the model-affinity gate's per-process limit is recorded as accepted in ADR 0025, with where it stops and what reopens it

- **What was open.** ADR 0025 named the gate's limit (two harness processes on one box do not see each other's in-flight
  model) but never decided it, and the comment in `internal/modelaffinity/affinity.go` said "nothing here narrows that gap"
  after two machine-wide halves had landed beside the gate.
- **The decision.** The limit stays, on purpose. ADR 0025 gains an amendment dated 2026-10-14 that says what is per process
  (the drain of a base's in-flight batch and the batching of same-model requests), what is machine-wide today (the run cap
  and its line over the run registry, and the card fence that makes a text load wait for a media or exclusive lease; the
  seat-inflight markers are counted across processes but never gate), why accepting it is right now (the cross-process harms
  the box has shown are the machine-wide ones, closing the swap costs what ADR 0018 refused for text, and no measurement of
  cross-process switches is recorded), the trigger that reopens it (a measurement of processes with in-flight requests naming
  different models on one base at the same time, set against llama-swap's own model-load events, not a process count) and the
  shape of the change if it is reopened (the in-flight model set machine-wide and keyed by base, measured against the text
  budget first).
- **Also changed.** The `CROSS-PROCESS LIMIT` paragraph in `affinity.go` now says the same thing (comment only, no code), and
  invariant 6 of `docs/systems/offload-pipeline.md` states the scope of the gate.
- **Tests.** None: documentation and a comment only. The root package's docs, changelog and gofmt-sensitive gates and the
  `internal/modelaffinity` package were run on the branch.

### Fixed - offload_ask's description names the cross-seat retry and its two result fields

- **The description is current.** It said a second attempt means changing the question; a failed local first attempt now already gets one cross-seat retry when no route was named, and an identical repeat returns that answer with its retry. The `Returns` line now lists `retried_on?` and `retry_note?`. The compact profile's keep and dropped lists carry the two reworded sentences.

### Fixed - offload_ask's docs say what a named `route:"remote"` does with the engine's retry

- **A named remote keeps the local fallback.** The cross-seat retry change kept an ask's own retry off this box, and an ask
  that names `route:"remote"` still reaches the engine as an unreasoned call: its first placement is a fleet node and the
  engine's retry may come back to this box's seat, like every other door without a reason channel (ADR 0078). The
  `offload_ask` section of `docs/systems/mcp-server.md` now says so, and points a caller that needs a strictly remote run
  at `agent_delegate` with a `pin_reason`. Docs only.

### Added - ADR 0090, capability review 2026-10: dispositions and reopen triggers (Proposed)

- **What it records.** One decision record for the eight outside links named for mandatory installation or adoption
  (OpenSpec, REA, the Comfy-Org workflow templates, Headroom, the CLM paper, the CLM repository, multi-harness-rl,
  GitSwarm). Each row ends in one terminal state (INSTALLED, PORTED, REPLICATED, DECLINED or PARKED, or a conditional
  pair) with its reason and its reopen trigger. Three rows read pending, naming what decides them: the Headroom rungs
  (per-rung gate verdicts), the CLM paper (the CLM measurement and the edit-tool decision) and GitSwarm (the swarm pilot).
  The clean-room provenance rules for licences this repository cannot take are written down once.
- **The Parked table.** Fifteen items the review looked at and did not take (DeepGEMM, the Headroom resident proxy for
  Claude Code, the CLM repository and its serving engine, OpenEnv and Harbor, the SoL-Pi loop, Dream-RSI, Odysseus,
  Ouroboros, the GitSwarm code release, Agent-Reach, a local Claude Code launcher with cache-prefix pinning,
  Ternary-Bonsai-2-27B, NVFP4 beyond the DeepGEMM trigger, GLM-OCR and LightOnOCR-3, Cohere Transcribe), each with a
  reason and a reopen trigger. A short list of items skipped outright follows it.
- **DeepGEMM section.** Why it is parked (its kernels cover FP8-block, FP8xFP4 and BF16 only; the vLLM seats are
  weight-only 3-bit, 2-bit and INT4 with `fp8` as the KV dtype; Ampere is excluded; sm_120 exists only on an upstream
  dev branch and a vLLM fork with open correctness issues; Mega MoE is SM100-only), the reopen trigger (a Blackwell
  sm_120 tier adopts an FP8-block or FP8xFP4/NVFP4 vLLM checkpoint), the checklist questions that watch for it (ADR
  0089's NVFP4 arm, and the cascade bake checklist once it is committed), and the procedure if it fires (CUDA 13.2
  toolchain, wheel built in a scratch copy of the seat environment, a one-card A/B with output equality first and
  throughput as a side column only, explicit operator approval).
- **Scope.** Documentation only: the ADR and its index row. No code, no install, no config change.
- **Tests.** `TestDocsLint` (frontmatter, relative links), `TestADRNumbersAreUnique` (0090 is unique and linked from
  the index), `go run ./cmd/changelog check`, and the speed-term scan of the ADR and this fragment.

### Changed - the review lane's page states what each defer gets: what is retried, what is waited for, and what a cold seat costs

- **What was open.** `docs/systems/mcp-server.md` described `offload_review_diff`'s fence handling and its filters but never said
  what the door does with a defer, so a caller could not tell a capacity defer it should re-call from an abstention it should not,
  and nothing named the waits and the one retry that already surround the door. The page also still said that, with the fleet
  declining, the local leg waits out the lease and then defers. It stopped doing that when the in-run fence check landed: while the
  fence holds, the local leg defers `capacity` at once, naming the holder and its declared expiry, and only a fence taken after that
  check meets the cordon wait.
- **The policy, written down.** A block closing the lane's section: the door has no retry loop and no backoff of its own, and a retry
  never goes to the same seat. It lists the four bounded things that already exist around it so nobody adds a fifth (the admission
  budget, the in-run contention ladder, the fleet's capacity wait, the fleet's one retry), then one row per defer shape with its class,
  what the door does, whether it retries, and what the caller does: a foreign fence (offered once to the fleet, then deferred at once), a
  fence taken after the door looked, a full run line, a seat unfit to answer, a failed stack, an abstention, a ceiling and a refused
  call. A cold seat is stated as not a defer, with the reason the lane neither keeps a seat warm nor warms one ahead of a call.
  A plain text reservation is stated as not a fence, so the review runs on the seat.
- **Where the code is behind the policy.** The page says so, stated as not built: a local-leg defer that is a fact about
  the seat or its cards is not offered once to the fleet the way a fence seen on entry is, and the review's result carries no admission
  block. No code changed.
- **What ends a review.** The block states it from the node's own rules. A stall is `infrastructure` (reason `stalled:`, not retried
  by the delegator) and sits in the row for a seat unfit to answer. The wall is the run's expectation and not a deadline, and a run
  still producing is ended only by the liveness ceiling, max(3 x the seat's estimated run time, 2 x the wall, the 1800 s floor) and
  never above 4 h, which files `budget` or, when the run only sat behind the seat's other work, `capacity`. The ceiling row names its
  real stoppers (the ceiling, the step budget, a completion cut at its token budget, a re-pack the clock skipped or cut), and the
  patience bullet says the call has no fixed upper bound at the wall: the admission budget, the wait in the seat's run line and then
  the run to its ceiling. The 30 to 90 s figure is `offload_ask`'s and is not quoted for a review.
- **The retired fallthrough wait, everywhere the page said it.** The salvage paragraph still said a deferred fleet review is
  salvaged "instead of falling through to a local wait behind the very lease that fenced it". With the in-run check that leg defers at
  once, so the page now says so there too, and ADR 0041 carries a dated amendment on its 2026-09-15 note.
- **Tests.** `review_policy_docs_test.go`, red before the page changed: the policy block must exist with one row per defer shape and
  the bounds it quotes taken from the constants they come from (the admission budget, the contention budget, the placement wait, the
  wall ceiling, and the liveness ceiling's floor and cap); the ceiling row must lead with `budget`, name the ceiling, the step budget
  and the `capacity` filing, and say nothing of a stall; the unfit-seat row must carry `stalled:`; the patience bullet must not bound
  the call at the wall; and neither retired sentence about the fenced fallthrough waiting may be on the page.

### Added - ADR 0089, Proposed: Strata is an opt-in agent-seat backend that earns its place through the gate

- **What it rules.** Strata, a local MoE inference engine, is a candidate for the agent class only: never a text-cascade rung
  (ADR 0027 stands), and it displaces no declared `vllm_seat` (ADR 0048). Every tier gets a dormant slot (`strata_seat` or
  `strata_debt`), rendered and seeded by one predicate (`tierseed.StrataEligible`, the ADR 0080 pattern). Its inputs (NVIDIA
  driver 580 or newer, compute capability 7.5 or newer, 12 GB of VRAM with 8 GB a slow opt-in, AVX2, host RAM by quantization
  arm) were re-read from the project's install and model guides on 2026-10-14 and are hard-coded only by the predicate change,
  each with its `source` and `verified_at`; the spike pin is release v0.1.44.1, re-read at spike time. Only `blackwell-3x16`
  names a checkpoint.
- **The reversal it records.** The default checkpoint (Flash-Next GSQ-RCO IQ3_S, experts spilled to host RAM) is the shape release
  0.115.4 removed. It is admitted under the operator's later standing rules of 2026-10-07 (spill only for a model that does not
  fit its card, three measured conditions) and 2026-10-10 (RAM-dependent services allowed), and only if the spike measures that
  Strata's split matches the clause: attention and dense layers on the cards, only expert matmuls on the host, per-process CPU
  share within 10 percentage points of the `--n-cpu-moe` arm. The rule also keeps the KV cache on the cards, which Strata's
  default does not at 64K of context and above, so the clause arm pins `--kv-streaming off` and every arm records where the KV
  cache lives. The Coder arm is the fallback, but its pre-spike VRAM estimate was for all three cards, not the pair, so it is
  rule-clean only if the spike measures the fit. It is recorded as applied by default under the
  operator's 2026-10-13 instruction and confirmed on 2026-10-14 (his answer on the weights' licences: "internal-use measurement
  OK"); the ADR stays Proposed. The measured history (18/18 at 1828 s against 271 s; the 20-contract production path 15/20
  against 20/20) is cited as history and is never a gate.
- **Shapes and exceptions.** The spike is hand-launched, lease-gated, outside llama-swap, with `idle_unload_s=300`; the production
  shape is `internal/strataseat` plus `install_strataseat.go` with `ttl: 300` and a generalized seat-guard roster. A Strata seat
  launched by llama-swap is a harness seat, the established exception to the no-hosting rule. The pair is resolved by role and
  pinned by GPU UUID prefix at run time, never an index and never a UUID in a public file.
- **Licences and the ladder.** The Qwen Community License 1.0 and the Swift Open License 1.0 are tagged `commercial_use:
  unverified` (an internal-use measurement is accepted; the licence reading is pending), and the tier-default rung needs that
  reading recorded. ADR 0058 is accepted by the operator with a third licence state, `conditional`; its own edit lands with the
  template-route change, not here. Promotion is a ladder (spike, opt-in
  seat, tier default, main seat) through the one measurement gate (ADR 0088) and ADR 0080's resource gates; quality is the only
  gate, wall time is a side column, and the feasibility floor is tagged `speed-lint: allow feasibility floor`.
- **Scope.** Documentation only: one ADR and its index row. No code, no configuration and no seat changes; the follow-up work
  (preflight and spike, production-path A/B, predicate and coverage test, seat package, re-pack lane, promotion record) is not
  built here. ADR 0090 holds the DeepGEMM reopen trigger that the NVFP4 arm of the spike links to.
- **Checks.** `go test -run 'TestDocsLint|TestADRNumbersAreUnique' .` (links resolve, frontmatter, index row), `go run ./cmd/changelog check`,
  the leak-gate scans on the staged files, and the H-03 speed-term scan over the ADR and this fragment, which reports no hit.

### Security - an ingress guard on the fleet node, `fleet-ui` and `local-agent --serve` (register SF-16)

A web page in the operator's browser could reach the harness's own listeners. Measured before the guard:
- The fleet node answered 200 to a DNS-rebinding `Host` (`attacker.example`) and to a foreign `Origin`.
- The queue lane took a cross-site `text/plain` POST, which needs no CORS preflight.
- `local-agent --serve` decoded JSON from any `Content-Type`, so such a POST could drive its tool-using agent.

A new key, `fleet_ingress_guard`, puts one guard (`netguard.IngressGuard`) in front of all three listeners:
- `Host` must pass the rule a configured endpoint passes: loopback, a tailnet address, a dotless MagicDNS name, a
  name under **any** zone in `tailnet_suffix` or `tailnet_suffixes`, or the address the connection arrived on. An
  empty name is refused, and so is the bare zone, which names no node.
- Any `Origin` is refused. `fleet-ui`, a browser page, admits its own origin.
- A POST must be `application/json` (fleet node and `local-agent`).

`warn` (the default, so `enforce` stays opt-in) logs what `enforce` would refuse, once per kind of violation, peer and
offending value in each 5 minutes, with values quoted so a request cannot forge a log line, and serves the request.
`enforce` refuses with 421, 403 or 415, naming the key. `off` is the old behaviour. A mistyped mode fails the load, and
resolves to enforce, never off, when a verb keeps going after a load error. `local-agent --serve` and `fleet-ui`
refuse to start on a config that failed to load.

- **Ported, not merged.** This is the unmerged `feat/sf16-ingress-guard` branch (three commits, written against 0.164.0)
  applied as a patch onto 0.186.0, minus its changelog hunk, with its two RFC 1918 test fixtures moved to the
  documentation range.
- **One zone fix on top of the port.** The branch judged the Host against the primary zone only
  (`TailnetSuffix()`), while a config can list several (`tailnet_suffixes`, ADR 0074: a node shared in from another
  tailnet is named under that tailnet's zone). Under `enforce` such a node got 421, and under `warn` it filled the log
  with false violations, which would have kept the warn window from ever showing the zero unexplained denials that
  promotion to `enforce` waits on.
- **One name test for both guards.** `HostAllowed` and `TailnetURL` now call the same function
  (`tailnetNameAllowedIn`: a suffix match on a label boundary, or a dotless name), so they cannot disagree about a name.
  Review found three shapes where they did: nested zones (`sub.zone` is a host under `zone`, yet the guard refused it
  as the bare inner zone), a second trailing dot (`zone..` passed, the bare zone included) and a port that is not a
  number (`localhost:evil.example` passed as loopback). A browser cannot send any of them and no exploit follows, but
  the parity claim was false. A test now feeds both guards 2394 generated hosts (19 names, 0-2 trailing dots, 7 port
  shapes, 6 zone sets). Two differences remain on purpose: a name made only of dots is refused here and admitted by
  the endpoint rule, and neither guard checks that a port fits in 16 bits.
- **The warn log no longer goes blind.** Its dedupe set was never emptied and any peer could fill it with 4096
  distinct `Host` values, after which a legitimate client's violation was only counted. It now empties every 5
  minutes, gives one peer at most 64 of the 4096 slots in a window, writes a count left by a burst when the minute
  ends instead of waiting for another violation, and keys the dedupe on a hash of the whole value, where two long
  values sharing 200 bytes and a length used to be one offender. Lines are still clipped to 1 KB.
- **The `enforce` refusal does not name the tailnet zones.** The 421 body carried the verdict, which lists every
  configured zone and the keys that set them, to a requester that under DNS rebinding is a page able to read it. It
  now says which `Host` it refused (clipped to 200 bytes) and which key relaxes the guard; the zone list stays in the
  log line. `Origin` and `Content-Type` echoes are clipped too.
- **The mode is said where the operator looks.** `warn` serves what it would refuse, so a stock install is not
  guarded, and only `local-agent --serve` printed the mode. `fleet-serve` and `fleet-ui` now print `ingress guard:
  <mode>` at start and `doctor` prints it. A missing `--config` file still starts a node on the defaults, as before
  (a fresh box is legitimate); it now shows `warn` there beside the built-in-defaults banner.
- **Red first.** Netguard tests (nested zones, a second dot, a bad port, 2394 generated shapes; the log's window,
  per-peer share, pending count and key; the body), a root test of the mode line and its three call sites, and seven
  docs pins were committed failing before each fix. Mutations: dropping the port check, trimming a second dot, admitting
  the bare zone, refusing nested zones again, dropping the colon rule, removing the rotation, the per-peer share, the
  timer or the whole-value key, echoing the verdict in the body, and removing each call site of the mode line each turn
  a test red.
- **Docs.** The `fleet_ingress_guard` row of the operator guide and the fleet-node operator guide, the `--serve`
  section (its sample banner now shows the mode), the troubleshooting tables, the fleet-node system page (the log's
  window, four edges to know before `enforce` including `local-agent --serve` clients such as a containerized GUI) and
  the coding-agent page (invariant 6b and the `--serve` interface) name the key.

**What this does not do.** It is a browser-CSRF and DNS-rebinding guard, not authentication. A tailnet peer that is not
a browser (no `Origin`, a JSON POST, a `Host` naming the node's address or a dotless name) is admitted exactly as
before, and a dotless `Host` still lets a LAN resolver steer a name to the node's GET routes. It does not close the
unauthenticated non-loopback bind: per-node tokens and tailnet grants are separate, later work, and the media lane
stays tokenless until they land.

### Added - `install vllm-seat` renders windows-wsl seats from the committed tier, and what the two Blackwell tiers render is pinned by goldens

- **The gap.** `local-offload install vllm-seat` hardcoded the `linux-systemd` templates directory, so for the two tiers whose seat starts through WSL (`blackwell-2x16`, `blackwell-3x16`, launch `windows-wsl`) no shipped command could render the seat env the engine reads. `Spec.TemplatesDir`, which picks the directory by launch, had no caller outside tests; every windows-wsl render in the tree was a test with a synthetic runtime. The docs promised the command renders into `seat.env`. A box's seat env was therefore maintained by hand, and "does it match the tier" had no mechanical answer. The Windows installer is unchanged (it binds the tier's fallback seat); this closes the render path, it does not wire the installer.
- **The change.** The command takes its templates from the seat's launch (a `linux-systemd` seat renders byte-identically to before) and a windows-wsl seat writes `<seat id>.env`, the shipped chat template under `templates/` when the tier names one, and `seat-cmd.ps1`, `seat-cmdstop.ps1`, `hidden.vbs` and `register-seat-tasks.ps1`, and never a systemd unit or a polkit rule. The box's half arrives as flags, wired through `vllmRuntimeFlags`/`resolve` into the existing `Runtime` fields and never into the tier: `--distro`, `--wsl-seat-dir` (default `/opt/seat`), `--lmcache-overlay`, `--cache-mount-src`, `--cache-mount-opts` and `--host-prefix`. The output after a windows-wsl render says what is left (copy into the distro, register the tasks elevated, add the llama-swap entry) instead of the sudo, systemctl and polkit steps.
- **Refusals name the flag, before anything is probed or written.** No `--distro` for a windows-wsl seat; an fp8 KV seat with a cache server and no `--lmcache-overlay` (stock LMCache restores fp8 pages corrupt, upstream PR #4253; `Spec.NeedsLMCacheOverlay` is now the one statement of that rule, used by `Artifacts` and by the command; `Artifacts`' own refusals stay as the backstop); a `--venv`, `--hf-home`, `--wsl-seat-dir` or `--lmcache-overlay` that is not a path inside the distro (their defaults hang off `--home` and `$HF_HOME`, which are Windows paths here, and would be written into an env bash cannot open); and a windows-wsl-only flag given for a seat that starts through systemd. A failed prerequisite check on a windows-wsl seat points at `--host-prefix`.
- **Fixed on the way: `Runtime.HostPrefix` never worked.** It maps the engine's paths into this process's filesystem for the prerequisite check, and no caller set it. `Resolve` listed the HF snapshots directory with the engine's path instead of the mapped one, and `hostPath` tested for a leading `/` after `filepath.Join` had made backslashes on Windows, so the prefix was never applied there. Both map now; the `ModelPath` that reaches a rendered file stays the distro path.
- **Fixed: the hand reference `setup/templates/vllm-seat/llama-swap-entry.yaml` had drifted from `Spec.Entry` and nothing read it.** It carried `ttl: 1800` (the house idle window is 300), no `concurrencyLimit` (llama-swap's per-model default of 10 answers 429 to a 32-slot seat from the eleventh stream), no `useModelName`, and a bash-through-`wsl.exe` command that cannot start the distro from the SYSTEM account llama-swap runs as. It now declares `ttl: 300`, `concurrencyLimit: 32`, `useModelName` and the PowerShell stubs, and `TestVLLMSeatReferenceEntryDeclaresWhatEntryRenders` keeps it equal to what the renderer writes, key for key and value for value.
- **Goldens over the committed tiers, not fixtures.** `internal/vllmseat/committed_tier_env_test.go` reads `setup/templates/profiles.json`, renders `blackwell-3x16` and `blackwell-2x16` through `windows-wsl/seat.env` with a synthetic runtime, and pins the `SEAT_*` operating point line by line and the exact `SEAT_EXTRA_ARGS` tail (`--enable-auto-tool-choice --tool-call-parser qwen3_xml --reasoning-parser qwen3 --kv-cache-dtype fp8`, plus `--chat-template` and `--enable-prompt-tokens-details` on the 3-card tier); that the tail names the agent contract once and cleanly (one `--kv-cache-dtype`, no value ending in a comma); that the chat template the tail names ships with the render; and that the windows-wsl llama-swap entry's `concurrencyLimit` equals `max_num_seqs` (32). `profiles.json` is untouched. These goldens characterise what the tiers render today; they are not proof that any running box matches, which needs the diff of that box's seat env against this render.
- **Docs.** `docs/OPERATOR-GUIDE.md` gains "Rendering a Windows/WSL seat" (the flags, the example, what the command does not do); `docs/systems/cache-server.md` names the command; the two windows-wsl tier pages carry a note saying only `install vllm-seat` renders their seat (emitted by `internal/tierdocs`, `docs/tiers` regenerated, only those two pages change).
- **What this does not do.** It does not touch the device indices the tiers declare (`0,2` on the 3-card tier, `0,1` on the pair) or any other `profiles.json` value; whether a running box's devices, utilization or argument list match its tier is the operator's diff to take. It does not copy into the distro, register tasks or write the llama-swap entry on Windows, and the Windows installer still binds the fallback seat.
- **Tests, red first.** A seam commit declared the six flags unwired and added the tests; red lines against it included `install_vllmseat_wsl_test.go:120: ... needs an LMCache overlay (Runtime.LMCacheOverlay)` (the render could not be reached), `refusal must name "--distro"`, `-distro was accepted for a linux-systemd seat`, `hostprefix_test.go:62: ... no vllm entry point at /opt/seat/vllm-env/bin/vllm` and `reference ttl = 1800, but Spec.Entry renders 300`; the committed-tier goldens were green from the start (they characterise). The tier-page note had its own red commit (`wsl_seat_render_test.go:23`).
- **Mutations.** Twenty-three one-line mutants, each restored byte for byte (sha256 compared), twenty-two red on the first run and the surviving one (`Distro` hardcoded in `resolve`, which the test's own distro name matched) red after the test was given a distinct name: the templates directory pinned back to `linux-systemd`; the `--distro`, overlay, windows-wsl-only-flag and distro-path refusals each disabled; each of the six `resolve` fields dropped; the launch-specific next steps disabled; the `--host-prefix` hint disabled; `Resolve` and `hostPath` back to their unmapped forms; `--kv-cache-dtype` doubled, `--reasoning-parser` dropped and `concurrencyLimit` forced to 10 in the renderer; `ttl` and `concurrencyLimit` broken in the reference entry; `NeedsLMCacheOverlay` narrowed; the tier-page note disabled; `enable_prompt_tokens_details` flipped in the 3-card tier.
- **Review round: what the first review found and this branch now fixes.**
  - *The seat env was written with the template's line endings.* `windows-wsl/seat.env` had no `.gitattributes` rule, so a Windows checkout with autocrlf held it as CRLF, `Artifacts` copied the bytes, and bash in the distro (`seat_fg.sh` sources the file) read a CR as part of every value: `SEAT_PP=` became a non-empty string and a tensor-parallel seat started as a pipeline seat (`--pipeline-parallel-size` with a CR), `SEAT_PORT` and the KV dtype were corrupt, and a parity diff differed on every line. The renderer now ends the lines of any `.env` it writes with LF, and `.gitattributes` pins the template to LF. The Windows scripts keep their endings and the linux-systemd artifacts are untouched. The tests that read the env trimmed the CR, which is why nothing saw it; `TestWSLSeatEnvIsLFWhateverLineEndingsTheTemplateCheckoutHolds` renders from a template forced to CRLF and counts CR bytes.
  - *`--venv` and `--model-path` were checked and never rendered.* The seat env had no `SEAT_VENV` or `SEAT_MODEL`, so `seat_fg.sh` ran `<seat dir>/vllm-env` and its hard-coded repo id while the prerequisite check vouched for the venv and snapshot the flags named. The env now carries both (`SEAT_VENV` is the venv the check looked in, `SEAT_MODEL` the snapshot it resolved or `--model-path` pinned), the goldens pin them, and `--model-path` must be a path inside the distro. Expect these two lines in the first diff against a hand-kept env; the guide says what to confirm after the first start.
  - *The failure text was wrong in four places.* A prerequisite failure threw the stat error away and told an operator to build a venv that was already there; it now keeps the operating system's reason, reports a `--host-prefix` that cannot be read as that (no advice to build anything), and points at `--host-prefix` whenever the probe went through one. Each path flag's refusal gives its own example instead of `/hf` for all of them.
  - *Flags that read as decisions and were dropped are refused.* `--cache-mount-src` and `--cache-mount-opts` for a seat with no cache server (the tier declares none, or `--config` binds it storeless); `--lmcache-overlay` for a seat that starts through systemd, whose templates carry no LMCache wiring (an fp8 KV seat bound to a store cannot be rendered there at all, as before this branch, and the error now says why instead of asking for the flag); a `--host-prefix` that is a `wsl.localhost` or `wsl$` share naming a distro other than `--distro`.
  - *The goldens now pin the cache-server half.* `SEAT_L2` (store type, path, writer count, capacity), `SEAT_L2_MOUNT_SRC`, `SEAT_L2_MOUNT_DIR`, `SEAT_L2_MOUNT_OPTS`, `SEAT_L2_MIN_MBPS`, `SEAT_L2_PRUNE_GB`, `SEAT_L2_MOUNT_SRCADDR` and `SEAT_MP_HTTP_PORT` are now pinned line by line, as literals taken from the tiers' `cache_server` blocks, so a drift in the template or in a tier's capacity or prune target turns them red.
  - *Mutations for this round.* Twenty-four more one-line mutants (the LF normalisation and the `.gitattributes` rule off; `SEAT_VENV`, `SEAT_MODEL`, `SEAT_L2`, `SEAT_L2_MOUNT_DIR`, `SEAT_L2_MIN_MBPS`, `SEAT_L2_PRUNE_GB` and `SEAT_MP_HTTP_PORT` broken in the template; a tier's `prune_gb` and `max_capacity_gb` drifted; `Detect` dropping the stat cause, ignoring an unreadable prefix and accepting a file as a prefix; the `--model-path`, per-flag-example, distro cross-check (also case-sensitive and without the `wsl$` form), storeless mount-flag, systemd overlay and systemd fp8 refusals disabled; the two `--host-prefix` hints disabled), each restored byte for byte (sha256 compared). Twenty-one were red on the first run; the other three were a mutant that did not compile (not a proof, replaced) and two that survived because the distro cross-check test could also be satisfied by the unreadable-prefix refusal, so the test now requires the cross-check's own message and rejects the other one, and both mutants are red.

### Added - authority-graded acceptance: an optional qualifier on contains:, regex: and not_contains:

- **What it does.** `contains:`, `regex:` and `not_contains:` take an optional authority qualifier at the head of the
  argument: `@answer:` (the answer text the model wrote), `@field=<name>:` (the values of one structured field) and
  `@grounded:` (in the answer and verbatim in a context document; `not_contains:` has no grounded form, since documents
  cannot corroborate an absence). The levels are ordered answer < field < grounded. Evidence carries an authority, a
  weaker level's evidence never closes a stronger level's claim, and an empty or absent evidence set grades `unknown`:
  the merge gate counts it as a failed check, and the reason reads `unknown at <level> authority: ...` so an empty
  answer is told apart from a wrong one. `core.AcceptanceCheck.Grade(result, docs)` returns pass, fail or unknown;
  `core.ClosedAt` and `delegate.GradeAcceptance` report the authority a contract closed at (its weakest check, and only
  when all passed). `delegate.EvalAcceptance` now grades through the same path.
- **The closed-at authority is published.** `delegate.EvalAcceptanceGraded` returns it beside the failures, every
  site that grades a result records both through one helper, and the result a caller reads carries
  `acceptance_authority` (`answer`, `field` or `grounded`), as does the corpus row. It is the weakest authority among
  the checks, stated only for a result accepted whole: one unqualified check, a failed or unknown check, a
  list-fidelity failure or an excused document fingerprint leaves the key absent (omitempty), so every result that
  is not closed at an authority publishes byte-identically to before. A weaker authority cannot close a stronger
  claim: a contract of one grounded and one answer check closes at answer.
- **Why a qualifier and not a verb.** The verb set is closed and pinned (`reviewedAcceptanceVerbs`), and every verb
  must be shown to read no clock. The qualifier lives in the argument tail, so no verb is added, a node one release
  behind still parses the line (it only parses acceptance at the ACK), and an older delegator reads it as a literal
  substring (the Skew bullet says what that does to each verb). The intake lint adds `AUTHORITY-UNREACHABLE` for a
  grounded check on a contract with no context documents (a blank document is none, as Grade reads it) and for a
  field check the `output_schema` does not declare; a field-level check joins the schema's `required` list through
  `Fields()`.
- **What did not change.** An unqualified check is byte for byte what it was, failure text included. That includes
  `not_contains:` passing over a fully empty result. No file under `contracts/` (the write-door contracts included)
  uses `not_contains:`, and an empty final answer is deferred before acceptance, but the case is not shown unreachable
  (results also arrive from nodes, the queue and the rescue path) and external contracts cannot be seen, so the
  stricter reading applies to the qualified form only. `min_items:`, `nonempty:` and the diff verbs already fail
  closed on empty evidence and are unchanged. Some previously valid lines now fail to parse, by a rule and not
  a list: the reserved prefixes are exactly `@answer:`, `@grounded:`, `@field=` and `@field:` (the last is refused,
  since the field separator is `=`), and after one a missing field name, a missing colon or an empty tail is a parse
  error, and so is `not_contains:@grounded:` (documents cannot corroborate an absence). A substring that starts with a
  reserved prefix on one of the three verbs reads as qualified; any other leading `@` is still a literal, so
  `contains:@field`, `contains:@fieldname:x` and `contains:@fieldx` are plain substrings.
- **The document-fingerprint excuse.** The research lane excuses a FAILED document fingerprint when the digest is an
  honest absence. That excuse now drops only a failed check, never an unknown (an unknown stays a reported failure),
  and the quarantine reads the fingerprint through the same grading as the merge gate, with the contract's context
  documents: a grounded fingerprint the documents carry is a pass for the node, not an excused absence. A fingerprint
  the contract can never close (the unknown the `AUTHORITY-UNREACHABLE` lint warns about, such as a grounded check on a
  contract with no context documents) is a defect of the contract: it is still reported as a failed check, but it is
  neither a strike nor a pass for the node (no quarantine) and is not retried on a second node. An unknown the
  contract could have closed, such as an empty answer beside the documents or a declared field missing from the result,
  strikes like a failure, unless the digest is an honest absence (below). Only a FAILED fingerprint can be excused (by an honest absence). An unknown
  over an honest-absence digest (a qualified field fingerprint on an empty field, the absence sentence in another
  field) strikes nothing either, since an honest absence is neither a pass nor a strike, and a fingerprint the node
  passed beside a contract-caused unknown is still recorded as its pass.
- **The rig.** The failure-class register keeps classifying a qualified check's failure by its verb prefix, so an
  unknown (empty or absent evidence) is counted as `anchor-miss`, not `schema-miss`; the empty-versus-wrong distinction
  lives in the evidence column (`unknown at <level> authority`), not in the axis (docs/systems/coding-agent.md).
- **Skew.** A node one release behind admits a qualified contract (a literal substring to its parser). A delegator one
  release behind, or a misspelt qualifier, reads the line as a literal: `contains:` and `regex:` fail closed. On a
  delegator one release behind, `not_contains:@answer:SECRET` is read as the literal string "@answer:SECRET", so the
  check passes even when the answer contains SECRET and the forbidden-content check never fires. Qualified checks need
  the whole fleet on 0.187 or later.
- **Tests.** Red first (the tests commits do not compile, or fail, until the implementation lands): parse table
  including the literal-`@` and malformed cases, the verdict table (pass, fail and unknown for every level and
  verb), the ordering and the weaker-never-closes-stronger table, full-evidence pins (every match of a grounded
  pattern, every context document, boolean and number field values), `ClosedAt`, a pin of every unqualified case
  including the vacuous `not_contains:`, the delegator path with the contract's documents, the fingerprint excuse and
  its quarantine reading, the lint warnings (a blank document included), the published `acceptance_authority` on the
  local and remote paths and the corpus row, a source guard that every grading site records it, the rig reading real
  graded failures as anchor misses, and root tests that the verb set is unchanged and a speed term behind a
  qualifier is still found. Docs: `contracts/README.md` (authority-graded checks), the operator guide and the
  `acceptance` row of `docs/systems/fleet-node.md`. The tool descriptions in `internal/mcpserver` still list the verbs
  without the qualifier and the result keys without `acceptance_authority`; they are left to the schema-profile work
  that owns that file.
- **Known limits.**
  - "Not retried" holds only for the research door: a deterministically unreachable authority (such as
    `contains:@grounded:` with no context documents) is still retried on a second node. The better fix is an intake
    refusal with a contract-class defer.
  - A qualified `not_contains` on a legitimately empty field grades unknown, so pair it with a presence check.
  - `Authority.AtLeast` and the exported `delegate.GradeAcceptance` have no production caller yet.

### Added - offload_ask retries a failed local first attempt on another seat

- **The defect.** `offload_ask` had no cross-seat retry: a first attempt that ran on this box's seat and failed its
  generated acceptance was final, while `agent_delegate` retries a failed verification on another seat. The run, the
  grading and the publish all live in the ask handler, so nothing downstream ever gave the ask its second chance.
- **What it does.** When the first attempt ran locally and `acceptance_failures` is non-empty, the handler sends the same
  contract once through the delegator at `route=remote`, which never places on this box's seat, so the retry runs on a
  different seat by construction. The retry is decoded and graded exactly like the first attempt (the decode-and-grade
  step is now `gradeAskWire`, shared by both) and only a clean one is published: a verified retry is the answer, with
  `retried_on` and a `retry_note` naming the first attempt's verdict; a retry that also fails leaves the FIRST answer,
  `verified: false`, with `retried_on` and a note that the retry also failed; a fleet that takes nothing leaves the first
  answer with `retry skipped: <why>` and no `retried_on`. A deferred first attempt and a routed ask are unchanged (the
  routed path's engine already retries). The stored cache entry is the answer as published, so a cached unverified
  answer already includes its retry and a repeat never retries again.
- **The retry floor.** The retry lives inside the contract's own `timeout_sec`, as the delegator's does: it is skipped,
  with a note, when what the first attempt left of the 900 s an auto-sized ask owns is under the retry floor
  (`agent_retry_min_sec`, at least 10 s), and otherwise runs with what is left as an explicit `timeout_sec`.
- **A routed fleet result now says it retried.** `contractOnFleetAt` dropped the delegator's `retried_on` and
  `retry_note`, so a retry the delegator ran inside a routed call (and one it considered and skipped) never reached the
  response. Both fields are surfaced now on `offload_ask`, `agent_run` and the review lane, additively: they are absent
  when no retry was considered.
- **Tests.** `ask_retry_test.go`, red before on the unchanged handler: a clean retry is published with `retried_on`; both
  attempts failing publishes the first answer with an "also" note and caches it as published; a declining fleet leaves
  `retry skipped` and no `retried_on`; a verified first attempt never reaches the fleet (the guard, green before); the
  retry floor skips a retry with a note and lets a configured floor through when enough is left; the fleet's own retry
  note is kept after the ask's; and a routed fleet result carrying a retry surfaces both fields.
- **A named `route: "local"` never retries.** It is how an ask says its material stays on this box (the tool has no
  `pin_reason`), so a failed first attempt then stands with `retry_note: retry skipped: route local was named; the ask
  stays on this box`; the retry applies only to an ask that named no route.

### Added - builder drift guard: the template widgets each ComfyUI workflow builder copied are hashed and checked on both sides

- **What it does.** Eleven `render/wf-*.mjs` builders were copied from templates of the
  `comfyui-workflow-templates-json` package, and a template widget that changes under a builder that copied it
  (upstream workflow_templates PR #1308: a pose-end percent wired to the pose-start input) went unseen. New
  `render/builder-drift.mjs` (library and CLI: `check`, `explain`, `record`), `render/builder-drift-specs.mjs` (which
  widget of which template each builder copied, and where it sits in the builder's graph) and the committed
  `render/testdata/builder-drift.json`: per builder and view, the template name, the package versions, a sha256 of
  the recipe widgets as the template carries them and as the builder's output graph carries them, and one digest
  pair per widget with a status (`same`, `deliberate`, `unreviewed`, `template-only`). Names and hashes only; no
  template JSON, text or widget value is stored.
- **Two checks.** Builder side, offline, always in `node --test`: one test per builder (`builder drift: wf-krea2`)
  rebuilds the graph with fixed arguments and compares each widget, and a moved widget fails naming the builder
  and the widget. Template side, when a templates directory is named (`--templates-dir`, `--comfy-dir`,
  `COMFY_TEMPLATES_DIR` or `COMFYUI_DIR`, resolved in that order by the CLI and the tests alike): the same widgets
  are read from the template JSON, and a change fails naming the builder, the template and the package version the
  builder was last verified against; with every widget unchanged the recorded `templateSha256` must equal the
  recomputed hash, so a hand-edited manifest fails. A wiring item keeps the `_distilled` Wan Animate 2 template's
  start and end percent on separate subgraph inputs, the shape of the PR #1308 defect.
- **Fails closed.** A flag that takes a value and gets none (`--templates-dir "$UNSET"`, `--templates-dir=`, `--comfy-dir`,
  `--manifest`) exits 2, as does any argument that starts with `-` and is not a known flag (`--template-dir`, and the
  single-dash `-templates-dir` or `-x`), a value given to a switch (`--strict=1`) and a second positional after the verb;
  none of them can skip the template side. A directory named only through `COMFY_TEMPLATES_DIR` or `COMFYUI_DIR` that is
  unset still leaves it skipped, with a note. `check --require-templates` makes a missing template side an error (exit 2)
  for upgrade runbooks. `check --strict` also fails (exit 1) on
  every `unreviewed` manifest entry. A template or manifest file with a byte-order mark reads; a file that is not
  JSON, a `null` file or a template with no `nodes` array fails with a message that names the file and, on the
  template side, the builder. A wiring check tells two sources apart by node id and output slot, not by node type
  (two separate nodes of one type are two sources; the message still names the type).
- **Coverage.** A new `wf-*.mjs` file must be guarded or listed in `NOT_TEMPLATE_DERIVED` with a reason, or a test
  fails. Not guarded, with reasons in the spec: `wf-hunyuan15-i2v` (its recipe is not the one template of the
  package that shares its family), `wf-sdxl-inpaint`, `wf-upscale`.
- **Where the value is read.** A widget whose input is linked keeps a stale serialized value; the value that runs
  comes through the link. A locator that points at a linked widget therefore throws, naming the builder and the
  key, and the spec points at the source instead: the widget the root subgraph instance promotes, the primitive
  that feeds it (read on the branch being checked), or, for the legacy form in which the instance lists the widget
  in `proxyWidgets`, carries no value at that position of the list and has nothing wired to its input for the
  widget, the interior widget once those facts are checked. A pass-through math
  node (the LTX frame rate) is pinned as a template-only item.
- **First run on package 0.1.96.** Eleven builders, eighteen views; every guarded widget equals its template
  except six entries, recorded and left for a person to judge. Unreviewed: the `wf-qwen-image-edit` full preset
  (template CFG 4, builder 3.0, probably a real builder defect, recorded as an open item) and the `wf-qwen-image-21` edit encoder resolution (template 0, builder 1024).
  Deliberate and stated in the builder headers: the `wf-qwen-image-21` step count (both views) and the
  `wf-wan-animate2` pose-cache device and dtype. The `wf-h3-av` turbo path equals its template (the instance
  promotes 8 steps, the builder runs 8). No builder recipe was changed.
- **Tests.** `render/builder-drift.test.mjs`: the committed manifest against the builders; a synthetic package
  written from the specs (no upstream data) in which every widget of every builder is moved once on each side and
  must turn the check red naming the builder; the PR #1308 wiring shape; a missing node, type or widget; a package
  version that moved with every widget unchanged (a note); a link planted on the widget every locator names; an
  input entry with `link: null` (three serialized shapes) on the node of every locator, which must not read as a
  link; the legacy proxied form, its promoted value looked up at the widget's position in `proxyWidgets` and a link
  into the instance's input refused; a hand-edited `templateSha256`; the directory order; the coverage and manifest-shape
  branches, each checked by removing the branch in a scratch copy. The review round added tests for node identity in
  the wiring check, flags without a value, `--require-templates`, `--strict`, byte-order marks and `null` files, the
  builder graph's class check, the subgraph-name filter of the node lookup, and a literal list for the template-only
  widget locators; each new test was shown red against its mutant (key by node type, key by node id only, empty value
  accepted, missing value read as true, unknown flag accepted, each flag ignored, strict failing `deliberate` too,
  byte-order mark not stripped, nodes array or manifest shape unchecked, class check removed, name filter removed,
  template-only locator renamed) and the file restored byte-clean. The second round added tests for single-dash
  arguments, extra positionals, a dash-led flag value, the empty `--flag=` form, a value on a switch, the reason `check
  --strict` quotes, and the id/slot order of a wiring key (the pairs (node 1, slot 2) and (node 2, slot 1)), each shown red
  against its mutant. Red then green was shown with planted effective
  values (root-instance steps, LoRA strength and frame rate) in a scratch copy of the package: the guard fails
  naming `wf-h3-av` and `wf-ltx25-i2v`, and a guard that read the stale widgets stays green on the same copy.
  `node --test "render/**/*.test.mjs"`.
- **Known limits, stated in the docs.** Node mode is never read (a bypassed or muted node still reads as live); wiring
  is mostly unguarded (one wiring item; the `wf-wan22-i2v` pairs share locators, for the steps, the cfg and the
  step split; a `feeds` locator would cover it);
  `W` and `I` ignore `proxyWidgets`; the two `unreviewed` entries are green in a plain `check`.
- **Docs.** `docs/systems/media-generation.md` (Builder drift guard) and `render/README.md`.

### Added - an optional log-collapse rung for the compaction ladder, default off

- **What it does.** `compactOpts.LogCollapse` (default false) adds a rung between the GCF and skeleton rungs. In an OLDER result of
  a build or test command it folds each run of three or more consecutive near-identical lines into the first line, a count and the
  last line. Lines match after every run of digits, `0x` literal, UUID, long hex run and terminal control sequence is masked (the
  footer says digit runs are ignored) while the letters around a digit run still count (`x1` and `y2`, or `md5` and `sha256`, are
  different lines); every blank line, every line that looks like an error, warning, failure or test summary, every `path:line` or
  `path:line:col` diagnostic, and the first 8 and last 4 lines stay verbatim. "Looks like" includes CamelCase exception names (`TypeError`, `AssertionError`,
  `NullPointerException`), a killed process, a segfault, a non-zero `exit status` and npm's `ERR!`, and it is tested on the line as
  written and with its terminal colour removed. The body opens with `[log-collapse: original N chars, M lines]` and ends with a
  footer saying what was folded. A result whose tool-call id is declared by more than one call has no known producer and is left
  alone. The rung is one new file (`internal/agent/rung_logs.go`) plus, in `compaction.go`, a field, a call site and the
  original-size reporting described below. No flag, `Loop` setter, config key or replay option enables it yet, so nothing on a
  production path changes.
- **The gate reads the call.** A result is folded only when the call that produced it is a build or test invocation: `go test`,
  `go build`, `go vet`, `cargo build`/`test`/`check`, `npm test` (or `npm t`) or `npm run build`/`test`, `pytest`, `python -m pytest` or
  `python -m unittest`, and `make`
  through `run_shell` (the `run` tool's allowlist has no `make`). The first word is read after any `NAME=value` words and the command
  must be one pipeline: an unquoted `;`, `&&`, `||`, a lone `&`, a newline or a carriage return disqualifies it, and a pipe
  qualifies only into a grep-family program or a pass-through filter (`head`, `tail`, `sort`, `uniq`, `wc`, `cut`, `sed`, `awk`,
  `tee`, `cat`), so `xargs` or a linter after a pipe disqualifies it. A search listing, a `cat` or `sed` of a data file, a
  linter's output, a file read and a result whose call has an empty or reused id are left alone, so the docs' statement that search
  and file reads are never touched is now true of the code. A later pipe stage is judged by its program name alone, so a filter
  handed a file operand (`go test ./... | cat data.csv`) still qualifies and would fold that file's lines; operands cannot be refused
  outright because `| tee out.log` is a pass-through with one, and the docs state the limit.
- **Beside the search rung.** The search-results rung (`compactOpts.SearchResults`) and this one never claim the same body, because
  each reads the call that produced it (a build or test invocation here, a search there). With both on, this rung runs first and the
  search rung only when the transcript is still over budget: this rung keeps every distinct line and the search rung drops most of a
  listing, so the gentler edit gets the first chance to fit. The two gates share one quote-aware command scanner and one set of
  pass-through filters (`searchCommandScan`, `searchPassThroughFilters`, `searchGrepPrograms` in `rung_search.go`), so they cannot drift
  apart; this rung adds its own positive build or test test of the first word, and refuses a quote left open and an empty stage. The
  search rung writes its disclosure line through `skeletonHeader`, which this rung's size parser checks against, so a later elision
  marker names the original size of a thinned listing as well as of a folded log.
- **A rung that is off changes nothing.** `IsCompactionArtifact` keeps its three clauses; the always-on dedupe rung and the live
  loop's re-execute decision both ask it, so recognising the new header there would change the default ladder for any tool body that
  quotes it (a saved collapsed log read back with `read_file`). A separate `IsLogCollapseArtifact` recognises the header, and the
  trace harvest asks both so it still refuses a transcript the rung has compacted. Every parser of the new header is gated on the
  option. Wiring the rung into the live loop later must also teach `resultDestroyed` the new predicate.
- **Honest markers.** With the rung on, the skeleton prefix, the elide marker, the reactive last-resort marker and the dedupe
  reference of a folded body name the size the body had before the fold (parsed from the header, as the skeleton's size already was),
  so a marker never says that less was lost than was. `emergencyShrinkWith` carries the ladder options to the last resort;
  `emergencyShrink` keeps its signature and means "no optional rung". The header is believed only when the rung could have
  written it: its exact spelling, a size above the body's own length and within the sizes the rung folds, and a line count a fold
  needs. A result that merely quotes a header with a negative, zero, oversized or too-small number is named by its own length;
  a quoted skeleton header gets the same test (exact spelling, a size above the body's own length and between the smallest body a
  skeleton prunes and the largest the rung folds), so it cannot set a marker's size either. A
  quoted header with merely plausible numbers is indistinguishable from the real one in the text and is still believed.
- **Why.** Build, test and server output is mostly runs of lines that differ only in a timestamp, an id or a counter, and the line a
  later step needs is rarely inside one. This rung keeps every line the skeleton rung would keep and every distinct line besides, so
  it is gentler than a skeleton; because the ladder stops at the first rung that fits, turning it on keeps more of an older log
  than the rungs after it would at the same budget. The idea of compressing build and test output while keeping the failures and
  the boundary lines is the documented behavior of the Headroom project (Apache-2.0); the mechanism is this repository's own,
  clean-room, and `NOTICE` records it.
- **Properties, each pinned by a test.** Deterministic; idempotent (the output's prefix is recognised by the rung itself and by
  `IsLogCollapseArtifact`); never longer (the original string comes back unless the result is
  strictly shorter, checked per run, per body and again in the wrapper); fails closed (too small or too large, invalid UTF-8, a NUL
  byte, a JSON document, another rung's marker or a GCF block, an unknown or ambiguous producer, a panic); confined to the ladder's
  own window, so the protected preamble, the recent turns, pinned results (also through a dedupe reference) and tool-call pairing
  are untouched; placed after the GCF rung and before the search and skeleton rungs (a budget the fold alone meets leaves no
  search thinning and no skeleton, a budget GCF alone meets leaves the logs raw); the header, markers and footer stay out of the `signalLine` vocabulary so the drop rung treats
  a folded body like any other.
- **Tests.** Red first: the default ladder's output over the shipped mini corpus, four synthetic log transcripts and two transcripts
  of bodies that only quote the header is pinned by sha256 digests captured on the unmodified ladder
  (`TestLogsRungDefaultLadderBytesPinned`, which fails if the digest map is empty), then the rung's tests were committed against a
  no-op stub, then the rung. A mutation run of 33 edits (guards, windows, thresholds, the masking rules, the call site, the pin
  check, the default) killed all 33 of the first version; each later fix was mutation-checked on its own (16 further edits: the
  ladder position both ways, the pin set, the stripped-line test, the error-name and failure-shape patterns, each size-reporting
  site, the option gate on the size parser, the header predicate in the harvest, the reused-id check, the restored artifact clause),
  and the one that first survived (the last-resort pass over a body beyond the skeleton rung) got a test. The size parser's bounds
  are pinned at each edge, and the skeleton branch of the size helper, which only the last resort's pass over a newest body that is
  already a skeleton reaches, has a test that fails when the branch is deleted.
- **Review fixes, each red first.** The producer gate (above); the digit mask keeps the letters around a digit run, so two compiler
  errors or assertions that differ in an identifier no longer share a key; a `path:line:` or `path:line:col` diagnostic is kept
  verbatim and never joins a run; a folded run keeps its last line as well (first line, a count of the N-2 between, last line,
  with the marker in the singular for one line); a call and a result that both carry an empty id are not paired. Tests use the real
  `run_shell` framing (`formatShellResult`, including the cap's `...(truncated)` marker and a killed-by-signal exit line) and the
  context trim's elision marker, and a transcript with a grep listing and a build log runs with both rungs on and checks that each
  edits only its own body, in the order above, and that the last resort names the original size of both. (That test's first version
  passed without the search rung ever running: its listing had a match on line 0, which the search rung rejects, and its budget
  was met by the fold alone. The listing now counts from 1, is checked to be one the search rung thins, and the test runs at a
  budget the fold meets and at one that needs both rungs; moving the search rung ahead of the log rung fails it.) After the
  merge with the search rung, four more edits to the gate (the open-quote refusal, `npm t`, `python -m unittest` and the
  pass-through filter set) were each killed by a test. A mutation run of 49 edits to the gate, the scan, the mask, the diagnostic shape and the fold killed 47; the
  two survivors each remove one of two guards that both reject an empty call id, and removing both is killed. The golden-fixture
  discrimination test now needs 5 changed fixtures instead of 6: the shipped tool-text fixture's single body folds to one line
  (its last) more than before, which no longer fits its 60% budget.
- **What the evidence does and does not cover.** The savings figures printed by `TestLogCollapseMeasuredSavings` come from fixtures
  whose tool calls are renamed to `run`; the shipped mini replay corpus records call ids only, so the producer gate leaves it
  untouched and the rung is a no-op there. `ReplayOpts` does not expose the option, so the compaction eval cannot measure the rung
  yet. The later measurement-gate run needs a corpus whose tool calls carry names and a replay option. Digits being masked means a
  lone ` 500 ` access-log line can fold into a run of ` 200 ` lines; that is the specified matching, and the footer states it.
- **Docs.** `docs/systems/coding-agent.md` describes the rung with its option name and "default off". Whether to adopt it is left to the
  measurement gate.

### Added - an optional search-results rung in the compaction ladder, default off

- **What it does.** `compactOpts.SearchResults` (internal option, off by default) adds a rung to the agent loop's compaction ladder
  that thins older search listings: the grouped `search_files` output and bare `path:line:text` lines (as `grep -rn` or ripgrep
  print), alone or inside the `exit=` / `--- stdout ---` / `--- stderr ---` framing of `run_shell` and `run`. Each line is scored
  with BM25 against the objective (the last user message of the protected preamble, taken as its first 48 distinct terms in order
  of appearance; the line's own path counts as part of the line), error-looking lines get a fixed boost, and up to 30 lines, 5 per file and 15 files are kept (chosen to match the values shown in the upstream configuration example), chosen round-robin across files
  and written back in listing order under a counted footer (a 100-match listing over 20 files ends `[... 70 of 100 lines elided, 5 of 20 files elided ...]`).
  The tool's own "more matches" or "truncated" note is kept. The rung is gated on the call that produced the body: the result's tool-call id is mapped to its call, and only `search_files`
  (grouped form) and a `run_shell` or `run` call whose command is a search (flat form) qualify. A search starts with `grep`, `rg`, `ag`, `ack` or `git grep`; the `run` tool's allowlist holds only `git` of those, so through `run` only `git grep` is reachable and the others come through `run_shell`. A search piped through pass-through filters only (`head`, `tail`, `sort`, `uniq`, `wc`, `cut`, `sed`, `awk`, `tee`, `cat`, or another search) still qualifies; a pipe into anything else does not (`rg -l x | xargs flake8`), and neither does a command that chains another one after the search with an unquoted `;`, `&&`, `||`, `&` or newline, since the body then holds that other command's output too. So
  the `path:line:col:text` diagnostics of flake8, ruff, `shellcheck -f gcc` or rubocop, a body with no matching call and an id claimed
  by two different calls are left untouched. Anything that is not, line for line, one of those shapes (the `web_search` list,
  `mode:"files"` output, test output, an error result, binary or non-UTF-8 text) comes back untouched, and so does a result that is
  not strictly shorter.
- **How it sits in the ladder.** One struct field and one call site, after dedupe and GCF and before the generic skeleton, over the
  same older span as the skeleton rung: the protected preamble, the kept-recent tail and pinned bodies are never touched, only a tool
  message's body changes, and pairing is intact. The result opens with the skeleton disclosure line, so the existing idempotence
  check, the re-execute-on-repeat path (ADR 0016), the corpus harvest refusal and the elision marker's original size all treat it as
  the compaction artifact it is, with no change to any of them. There is no CLI flag or loop setter yet, and `compaction-eval` cannot
  replay the rung yet; whether it ever becomes a default is for the measurement gate to decide on a harvested corpus, and an adaptive cap, a production
  entry point and the measurement of its interaction with the generic skeleton must land first, in a separate change.
- **Why.** A long listing is the commonest bulk tool result on a small window, and the generic skeleton keeps its first and last
  lines whatever the task asks. Ranking by the task keeps the lines the task is about. The idea is documented by the Headroom
  project (Apache-2.0); this is a clean-room implementation of that documented behaviour, credited in `NOTICE`.
- **Tests.** The default ladder is pinned byte for byte before the rung existed (`internal/agent/testdata/ladder_default_golden.json`:
  the mini corpus plus frozen search-shaped and mixed transcripts, three budgets, five option sets, sha256 of messages, tool calls and
  the fit verdict; recorded from the unmodified ladder and unchanged by this change). The rung's tests were committed red first and
  pin determinism, idempotence, monotonicity, failing closed, the artifact contract, ranking (relevance, path, rare terms, line length,
  error boost), the caps, file diversity, the footer counts, the shell tools' real framing and truncation note, and the rung's place in
  the ladder (older tool bodies only, pins and the recent tail untouched, off by default, inert on the mini corpus). A 1,500-input
  mutated-listing sweep (a fuzz target runs the same properties) found one real defect before commit (a flat listing whose files
  interleave was regrouped by file); 27 source mutations were each caught by a test, and every one was proved applied and restored
  byte-clean (one more, deleting the early return after the rung, changes no output, because every later rung guards on the budget
  itself, so no test can tell it apart).
- **Measured, side note only.** On the frozen fixtures the 20 listing bodies keep about 51% of their tokens under the rung and about 46%
  under the generic skeleton, so the rung is not a size win (`TestSearchRungBodySavingsReport`); its case is what it keeps: with three
  task-relevant lines planted in each of 40 listings, the rung kept 120 of 120 and the generic skeleton 48 of 120
  (`TestSearchRungKeepsPlantedLinesReport`, a synthetic probe and not a corpus verdict). The committed mini corpus holds no listing, so
  the rung changes none of its outputs.

### Added - `compaction-eval contextbench`: ContextBench long-horizon tasks driven against one seat and graded from the context the harness kept

- **What it does.** A new subcommand of `compaction-eval` generates tasks from four ContextBench families
  (`kv_store`, `log_triage`, `needle_retention`, `sudoku_sketchpad`), streams their operations to one agent seat,
  keeps the conversation inside a token budget with the production compaction ladder (`agent.CompactReplay`),
  records the context after every step, and grades it. The result is a results JSON whose per-task outcomes use the
  measurement gate's seven words (`pass`, `task_fail`, `cheat`, `infra_error`, `timeout`, `judge_error`, `not_run`).
  It is the instrument for the context-editing decision; the compaction ladder and the context pager are what it
  measures. Flags: `--family`, `--level`, `--tasks`, `--seed`, `--endpoint`, `--model` (`--seat` is an alias),
  `--route local`, `--out`, `--budget`, `--reserve`, `--delivery`, `--gcf`, `--skeleton`, `--max-turns`, `--pass-at`,
  `--max-out` (reply cap in tokens), `--request-timeout` (transport deadline of one seat request) and `--config`
  (the config file that supplies the default endpoint and model). A value the run would silently replace with a
  default is refused with the flag's name and range: `--budget`, `--reserve`, `--max-out` and `--request-timeout`
  must be at least 1, `--pass-at` must be above 0 and at most 1, `--max-turns` at least 0.
- **Ported, not downloaded.** The four generators and graders are ported to Go from ContextBench (Apache-2.0, pinned
  commit named in each ported file; credited in `NOTICE`). Tasks are a pure function of (family, level, seed), so no
  dataset is fetched. The Go generators reproduce CPython's random stream (Mersenne Twister, `choice`, `sample`,
  `shuffle`, `choices`): a test pins the sha256 of each family's operation stream against the original generator's
  output for twelve parameter sets, so seed 42 is the published seed-42 instance, and `scripts/contextbench_parity_regen.py`
  regenerates those digests from a checkout of the pinned upstream commit. The operation mix is ported, so the
  streamed operation texts are the original's byte for byte; only digests are committed, no upstream data, and the
  agent's instruction text is written for this repository.
- **Levels.** Without `--level`, each family runs the smallest published level whose uncompacted transcript (the
  instruction, every operation and, for sudoku, the board panel the agent prints after each move) exceeds the usable
  budget: `R1000`, `L1600`, `K4-C24` and `t60`. The sudoku stream alone never overflows (about 12k estimated tokens at
  `t150`), so its default is the first level whose panels do; a test pins each default as the smallest one over.
- **Delivery.** Operations arrive as the result of a synthetic `next_op` tool call by default, because the ladder
  shrinks tool-result bodies before it drops turns; `--delivery user` is the upstream shape and reaches only the last
  rung. The report names the delivery, the ladder rungs and the size estimator (the ladder's own, not the original's
  tokenizer), so two reports are never compared by accident.
- **No seat is not a failed task.** A seat that cannot answer (refused connection, no endpoint, a reset mid-run) makes
  the task `infra_error` and it is never graded; a task that reaches `--max-turns` is `timeout`; a grade that cannot be
  computed is `judge_error`. A graded task below `--pass-at` (default: every item right) is `task_fail` and keeps its
  reward. The report has no timestamps and a reward is null unless the task was graded. With nothing graded the verb
  exits non-zero and says nothing was measured. A budget too small for the instruction plus the first operation is
  refused before any seat call (as a library call it ends `infra_error`, never a graded `task_fail`), an interrupt
  that lands while a seat request is in flight is `not_run`, and a context over the budget is never recorded as a
  snapshot, so the needle grader cannot credit operations the seat never received at a budget wall.
- **A wall before any query is not a grade.** When the context stops fitting the budget after the seat has answered
  but before it was handed a single query (`kv_store`, `log_triage`), no frame can hold an answer, so the task ends
  `timeout` (the attempt hit its budget) with `term_reason` `budget_wall_before_query` and no reward, not a graded
  `task_fail` of zero. A wall once a query has been handed over, and a wall in the two families that ask nothing, is
  graded on the frames the task has. The task record also carries `ops_sent`, the operations the seat was handed,
  beside `ops_delivered`, which counts the over-budget one. One published cell ends this way for a perfect agent, on
  purpose: under the default delivery the production ladder cannot hold `log_triage` at `L12800`, because its
  preserve guard keeps every tool result that still matches a signal word and the synthetic log lines carry
  `[WARN]`, `timeout` and `refused`. The level is left as published, the oracle ends there as `timeout`, and a test
  pins it as a signal about the ladder. `--delivery user` passes that cell.
- **Graders.** A `kv_store` answer is matched on its key without quotes or case, and its value must stand as a whole
  (the characters around it are not letters or digits), so the same words with a longer tag no longer count. A
  `log_triage` count answer is read after a repeated `Query N` or `qid=N` label is removed and digit groups such as
  `12,800` are joined, and it still must state exactly one number.
- **Tests.** Written before the code they cover, committed failing, then green: the CPython random stream; generator
  parity digests and per-seed determinism; each grader on a known-good and a known-bad snapshot set (needle reads only
  the last snapshot, the others every one; each key, tag-boundary, label and separator rule on its own rows); the
  driver against fake seats (an oracle that passes every family under both deliveries, the same oracle on every
  family's largest level under the default delivery with the one documented exception, a seat that is down, one that
  dies mid-run, a turn cap, an invalid gold, a wall before and inside the query block, an honest seat whose lost
  values fail the task once the ladder engages); the seven words and no clock field in the JSON; and that the bare
  instruction scores nothing. The verb is covered by root tests with an injected seat and by one run against an
  unreachable endpoint, by flag tests that fail if `--delivery`, `--budget` or `--reserve` stop reaching the run or a
  silently defaulted value is accepted, and by a test that every flag the verb defines is in its docs. Every published
  level at seed 42 is pinned by operation and gold digest, so a knob change in the level table fails a test.
- **Docs.** `docs/systems/coding-agent.md` describes what each family measures, how to run it, the snapshot field the
  grade uses and the outcome table.

**What this does not do.** It runs no measurement and downloads nothing; no result is recorded and no decision follows
from the bench alone. In this reply-only setting the agent has no tool to fetch evicted content, so the stored
pager report has basis `not_exercised` and a null re-fetch rate: it shows evictions only. Scores are not comparable with published ContextBench
numbers (different instruction wording and delivery); they compare harness configurations with each other.

### Added - `active_backend` per lane and per seat in `doctor` and `/fleet/health`: which engine answers, the chain it was picked from, the probe behind it, and why

- **What it does.** Every lane (workhorse, agent, triage, escalation, reasoning, vision, ocr, stt, stt_hq) and every seat now carries four engine-neutral fields: `active_backend` (an engine kind: `llama.cpp`, `vllm`, `rkllm`, `whisper.cpp`, `unknown` when nothing says the engine, when a probe timed out before the seat answered or when another node serves the seat, or `none`), `backend_chain` (the engine kinds it may use, in preference order), `backend_probe` (`process`, `endpoint` or `completion`) and `backend_reason` (why that backend was chosen, or why none was). A lane is a route `doctor` already diffs against the roster that has an effective seat: a route with no effective seat is not a lane, `embed_model` never is, and `agent` and `ocr` report the seat they fall back to when their own model is unset. Image, video and music generation are not llama-swap seats and have no lane here. `local-offload doctor` prints the fields as text rows in a new `backends:` section after its own checks, whatever they returned, so an unreachable llama-swap still gets rows that say `none` and why. `doctor` has no JSON mode: the text rows live in `doctor`, and the JSON of the same fields is `lane_backends` and `seat_backends` on `/fleet/health`. Both are additive and `omitempty`; the existing `backends` key (hardware tags, ADR 0054) is unchanged.
- **On a lane, `active_backend` is the engine of the seat the lane is bound to.** The agent lane of a box installed as a tier that declares a vLLM agent seat has the chain `[vllm seat, llama.cpp fallback]`, but the lane routes to whichever seat `agent_model` binds, so that seat's engine is what answers. A preferred seat that is up while the lane is bound to another is named in the reason (`the preferred vllm seat X is available (...) but does not answer for this lane, which is bound to Y`) and is not the active backend; a preferred seat that failed is named before the answer (`vllm seat X unavailable: ...; fell to llama.cpp seat Y: ...`); a bound seat that fails is `none`, with the other seat reported as available but not bound. It reports what answers for the lane as configured; it does not route.
- **One rule names a seat's engine, for its lane and for its own row.** From the box's own statements: `vllm_seats` or the tier table for a vLLM entry (whose command is a wrapper script), then the command llama-swap reports it spawned when the seat is loaded (a live command that names no engine does not erase the file's), else the command the serving config runs for it, naming `llama-server`, `rkllm` or `whisper-server`, then the kind the tier table declares for the seat, else `unknown`. The live command is read and never emitted. A seat that `seat_endpoints` sends to another node reads `unknown` with `served by another node` instead of `none: not in the llama-swap roster`.
- **The default probe loads nothing.** It reads llama-swap's roster and running list, and asks a seat that is already loaded and ready for its own health endpoint (the entry's `checkEndpoint`, `/health` when it states none) at the seat's own address, never through `/upstream` (which loads on demand and resets the idle timer). A seat that is in the roster and not loaded is reported available on demand and is not asked anything. No code path of the default probe sends a completion.
- **When the probe cannot see, the row says what it did not see.** A non-2xx answer is named by its status (`HTTP 503`) whichever read it came from, and a read that got no answer says `unreachable` once. A probe that timed out saw nothing, so the row reads `unknown` with the timeout named, never `none`; a refused connection is still `none`. The serving config is read up to 4 MiB and a larger file is not read, with the rows saying so.
- **`doctor --probe-completion` is the only door to a completion, and `doctor -h`, the usage line and the usage a mistyped flag prints list it.** A flag the verb does not accept is refused with the flag package's error line, the same usage as `doctor -h` and exit status 2, before any check runs (the verb's own flag set is checked before `runDoctor`, whose flag set knows only `--config`). A completion goes only to a seat that is already running, which loads nothing and so evicts nothing, or, on a box where nothing is running, to at most one seat in the run, however that completion ends. Whether a seat is running is read from the running list immediately before each completion, not taken from an earlier reading, because a seat can stop between two readings and a completion to it would then load it over whatever runs now. Any other seat is not probed and its row says over which running seat the load would have been (`not probed: would load over llama.cpp seat X`), whatever the running seat's engine: a running llama.cpp seat is protected as much as a running vLLM seat. Every row's starting state comes from one reading of the running list taken before any completion, and a later reading, taken immediately before each completion is decided, only decides whether a completion may be sent. No seat is sent a completion when the running list cannot be read just before it. A seat with no chat route (the configured embedder or speech-to-text seat, a seat whose own command carries an embedding or rerank flag, a whisper server) is never sent a chat completion, which it answers with a 404, and keeps the reading default mode gives it with `not probed: not a chat seat`. `/fleet/health` never runs it.
- **`/fleet/health` never waits for the report, and it is not private.** It is a cached background read like the residency answer: the first request after start-up answers without the keys (absent means unknown, never `none`) and kicks one read, a read older than 30 s is refreshed behind the next request, and one older than two minutes is no longer published. The route is unauthenticated on every node: `fleet_auth_token` does not gate it, and the ingress guard, which is not authentication, serves even the requests it would refuse under its default `warn` mode, so a stock node answers any peer that can reach its port. The new keys reach those readers. They add no address, path, URL or IP, but they do add the status key of each lane, engine kinds, probe levels and seat names (model ids and aliases), which `served_models` publishes only on a node with the agent or chat lane on. Reasons carry no address, path, URL or command line.
- **Engines register in one place.** A new engine adds one row to the classification table in `internal/backendchain/classify.go` and changes no field.
- **Code.** New package `internal/backendchain`; `servingtmpl.SeatEntries`; `seatload.Occupant` carries the spawned command and `/running`'s non-2xx answer is a typed error; `swapclient.HTTPStatus`; `internal/fleetnode/activebackend.go`; `doctor_backends.go` (the `doctor` verb wrapper, so `main.go` carries one changed dispatch line and one usage line, and a tier-table hook installed in `init`, so the fleet wiring costs `main.go` nothing).
- **Tests.** The completion rule, the opening snapshot, the chat-route skip, the live command, the whisper kind, the status and timeout readings, the pinned seat, the probe memo, the single classification, the bounded config read and the max-stale age each have a test committed failing before the code that fixes it, and each was shown to fail again when the line it pins was removed; the reading of the running list immediately before every completion, also for a seat the stored list calls running, has its test committed failing before the fix, and was not mutation-checked. The rest (the resolver table, a fake llama-swap that fails the test on any completion, any `/upstream` path and any POST, run by `Collect`, by the `/fleet/health` refresh and by the `doctor` verb, the additive JSON shape, doctor's flag parse and its help, and that no reason carries an address, a path or a route across every failure clause) pin earlier behaviour. `newTestServer` stands the collector down, so a test server never probes the llama-swap of a default config.
- **Docs.** `docs/systems/fleet-node.md` (the new section, the health route row, the source map), `docs/FLEET-NODE.md`, `docs/OPERATOR-GUIDE.md`.

### Added - doctor warns when an endpoint names a box by a private-range or tailnet IP literal instead of a name

- **What it does.** `local-offload doctor` prints a `lan ip:     WARN` row for each configured endpoint the harness dials
  whose host is an IP literal in a private range (RFC 1918, the IPv6 unique-local block), a link-local literal, or a
  tailnet CGNAT literal (that row says to use the MagicDNS name). Loopback, every name and a public literal say nothing.
  The fields read are `endpoint`, `seat_endpoints`, `cascade_remote_lanes`, `delegate_remotes`, `fleet_queue_holder`
  and the explicit entries of `pair_workloads_relay`. `nim_endpoint` and `nim_bases` are left out on purpose: a
  self-hosted NIM on the LAN is what that allowlist is for. A `kv_cache_server` address is left out too: the config
  documents a literal on the direct LAN or the tailnet as a valid store address and prefers it to a name (bulk KV over
  MagicDNS measured several times slower, and the `offload_status` reachability readback of a store dials literals only,
  so a name switches it off), so a row asking for a name would undo the better setting with no way to silence it.
- **Why.** Cross-machine reach uses tailnet names. A literal works until the box it names moves or its firewall is
  scoped to the tailnet, and then the endpoint is dead with nothing in the file that says why. The load already refused
  a literal outside the tailnet in `seat_endpoints`, `cascade_remote_lanes` and `fleet_queue_holder`, and
  `delegate_remotes` already failed `config findings`; `endpoint` and `pair_workloads_relay` loaded clean in
  silence, and a tailnet literal was accepted everywhere.
- **Shape.** Configuration only, so a serving layer that is down does not hide it. The row names the field with its list
  index or map key and never prints the address. A config with nothing to warn about prints nothing, and the row never
  changes doctor's exit code. It lives in `doctor_lanip.go`; `internal/config` is unchanged.
- **Tests.** Red first (`doctor_lanip_test.go`, committed against a stub): every field above warns and the output never
  contains the address, each range is tested at both edges (RFC 1918 class B and the tailnet CGNAT block, link-local,
  unique-local, and an IPv4-mapped IPv6 literal of a private and of a tailnet address, the latter pinning the unmap that
  the tailnet prefix test needs), names, loopback and public literals are silent, a cache-server store address of any
  range is silent, the relay's `auto` / `off` are not read, rows come out in a fixed order, and `doctorRun` prints
  the row and still returns nil. Every address in the tests is built at runtime.
- **Docs.** `docs/OPERATOR-GUIDE.md` (the `doctor` rows beside `capacity:`) and `docs/systems/cache-server.md` (why the
  store address is not read).

### Added - one measurement gate (local-offload gate)

- **What it does.** `local-offload gate <freeze|check|verdict|schema>` and `eval.Gate` put every adoption (a seat bake, a
  compaction change, a cascade cut, a skill proposal) behind one statistic and one set of rules. `freeze` writes a
  pre-registration (mode, primary metric with its margin and, for a `custom` class, its score range, the judge, efficiency
  metrics, `min_pairs`, bootstrap size and seed, held-out ids, declared tasks, an optional frozen incumbent) as canonical
  bytes, hashes the corpus into it and prints the baseline hash; the output is created exclusively, so a changed
  pre-registration is a new file with a new hash. `check` refuses a baseline that is not byte for byte what `freeze`
  writes: a one-byte edit of a frozen baseline either fails `check` or changes its hash, and `check --expect-hash` and the
  input's `baseline_hash` refuse it. `verdict` compares a candidate arm with the incumbent arm under that baseline and
  prints ADOPT, BLOCK or INCONCLUSIVE with `delta`, `lo`, `hi`, `n_used`, the excluded pairs and share, per-arm outcome
  counts, the cheat counts, `imputed_attempts` and per-arm `unusable_attempts`, one paired interval per efficiency metric
  (`delta`, `lo` and `hi` are null, never 0, below two resolved pairs, plus `over_unresolved_cap`, `regresses` and the
  `level` it was read at), the held-out read (`missing` when no pair is usable), a power note, both runs' stamps, the judge,
  an era marker, the baseline and corpus hashes and the verb's own build hash, with no timestamp (the same input twice is
  byte-identical). `schema` prints the gate-input JSON Schema, whose `outcome` enum is the Go list and whose receipt hashes
  are 64 lower-case hex. The primary is the mean of the paired task differences over the usable non-held-out pairs in
  sorted id order, with a 95% interval that is Tango's score interval for a paired difference when every pair is a tie, a
  win or a loss of the whole range (pass/fail scores: three counts, and a variance fitted under the hypothesised difference
  instead of read from the sample, so a margin of 0.05 of the range is certified by 73 ties and not by 72) and the score form
  of the Student-t interval (`delta -+ c * sn / sqrt(N - c^2)`, `c = t(N-1, .975)` from a table) over the differences plus two
  pseudo-pairs at each end of the range, centred on the observed mean, for every other score (see Calibration). An interval
  end within 1e-9 of the range of its boundary is neither an ADOPT nor a BLOCK, so float rounding cannot tip it.
  `eval.BootstrapDeltaMean` is unchanged (its results are pinned by a test)
  and the paired bootstrap reads the efficiency metrics.
- **Scoring.** Every candidate attempt enters its task's mean: a `pass` or `task_fail` with its score; a `cheat`, a
  `timeout`, an `infra_error`, a `judge_error` and a `not_run` at the worst score of the primary range (the bad end given
  the direction: 0 or 1, 0 or 10, or an end of the declared custom range); a declared task the candidate lacks, or holds
  with no attempt, as one `not_run`. A candidate cannot raise its mean by not finishing what it would fail. The incumbent
  is not imputed: its cheat enters at the worst score, its unfinished attempts are skipped, and a task with no usable
  incumbent attempt is an excluded pair. The excluded share is the largest of the excluded-pair, incumbent-attempt and
  candidate-attempt ratios; above 0.20 the verdict is INCONCLUSIVE, as is fewer than `min_pairs` usable pairs. Held-out
  pairs are scored the same way and can veto an ADOPT; with none usable an ADOPT becomes INCONCLUSIVE ("held-out evidence
  missing"), not BLOCK. Cost follows whether the attempt ran: a candidate's `cheat`, `timeout`, `infra_error` or
  `judge_error` that recorded a metric contributes that cost to its task's mean (it ran and spent it), a `not_run` carries
  nothing, the incumbent's unfinished attempts are skipped cost included, and an unresolved cost is JSON `null` end to
  end and never 0. A candidate's `timeout`, `infra_error` or `judge_error` that recorded no cost leaves its whole task
  unresolved on that metric, so a candidate cannot keep the savings of the attempts around it by leaving the cost of what
  timed out unrecorded (over the unresolved cap that blocks a non-inferiority ADOPT). A candidate that passed 24 of 25 attempts at 900 tokens and timed out on the 25th after burning 20000
  (1664 tokens a task, 1.66 times the incumbent's) is no longer an efficiency win. The incumbent exclusion ("the candidate
  does not run those") holds for a frozen or trusted incumbent; one supplied in the input can use it as a lever, up to the
  cap.
- **Verdicts.** Superiority adopts iff `lo > 0` and blocks iff `hi < 0`. Non-inferiority blocks iff `hi < -m` and adopts iff
  `lo > -m` with a declared efficiency metric (tokens, money or memory in GiB, finite and not negative) improving, none
  got significantly worse, and none was left unmeasured. With k declared metrics each is read on the `1 - 0.05/k`
  interval (Bonferroni); a metric regresses when its plain 95% interval lies wholly on the worse side, read from two
  resolved pairs (only an improvement needs `min_pairs`; memory ten times worse on 15 of 18 pairs and null on 3 vetoes
  now); a metric whose unresolved pairs exceed `max_excluded_share` cannot improve and blocks the ADOPT. Superiority
  reports efficiency and ignores it for the verdict. The efficiency reads stay on the percentile bootstrap; their false
  improvement rate on cost nulls is 3.5% to 5.9% at 16 tasks (a nominal 2.5%), documented, and a false improvement
  adopts nothing alone. The gate has no effect-size floor: a caller's own bar stays that caller's rule. A model
  judge needs a passed receipt for the same scorer.
- **Calibration of the primary.** The percentile interval of the paired bootstrap, which the gate read first, was measured
  anti-conservative: over 20000 simulated datasets per cell it adopted in 3.8% (uniform scores), 3.7% (scores in tenths) and 4.1%
  (x^3-skewed scores) of superiority runs at 16 tasks and in 6.1% of non-inferiority runs at exactly -margin with 20 tasks and pass
  rates .90 and .85, against a nominal 2.5%. Five candidate intervals (studentized bootstrap, BCa, the percentile widened by t/z,
  the paired t, the lower of two) did not stay at or under 4% in every null cell, and the score form of the t interval, the
  best of them, was the primary of gate version 2. A fifth review measured it where the arms barely disagree: a candidate that
  loses 5% of the incumbent's passes, at exactly the margin, was adopted in 4.4% of samples at 61 tasks, 17.7% at 62, 16.4% at 64,
  11.8% at 100, 6.5% at 250, 5.6% at 500 and 4.3% at 1000 (exact binomial sums), pass rates .99 and .94 in 11.1% at 62 tasks, and
  the same shapes at a margin of 0.10 in 15.6% at 32 tasks. Gate version 3 compares the two constructions the review named:
  pseudo-pair augmentation for every pair of scores (half a pair at each of four cells, one pair and two pairs at each end) and
  Tango's score interval for pass/fail pairs with the augmented form as the fallback, on three null grids (the 63 earlier cells, 54
  sparse-regime cells from 16 to 1000 tasks at margins 0.05 and 0.10, 49 stress cells for scores that are not pass/fail) and the
  power rows. Pass/fail cells are exact multinomial sums (no sampling error); the rest are 20000 datasets (0.11 to 0.14 points
  of standard error near 2.5% to 4%). Worst cell and cells over 4%: version 2 8.6% (2 over), 17.7% (29 over), 15.5% (25 over);
  half a pair at four cells 8.6% (2), 8.6% (12), 7.2% (18); one pair at each end 2.9% (none), 4.6% (3), 4.4% (2); two pairs at each end
  2.3% (none), 3.4% (none), 3.2% (none), but 4.0% at 444 tasks off the grids; **Tango with two pairs as the fallback, the
  choice, 3.0% (none), 2.4% (none), 3.2% (none)**. For pass/fail pairs the worst exact rate over every size from 16 to 500 and
  eleven shares of disagreeing tasks is 3.0% at margin 0.05, 3.1% at 0.10 and 2.7% for superiority. The nested-loss candidate
  reads 0% up to 72 tasks, 1.7% at 80, 0.6% at 100, 1.2% at 160, 1.3% at 250, 2.0% at 500 and 2.1% at 1000. No minimum-disagreement
  floor was needed. The price is power: identical outputs now need 73 tasks and not 61; on pass/fail scores power is higher at 16
  tasks (non-inferiority at pass rates .90/.90 1.5% to 4.6%) and a little lower from 24 to 250 against a strong incumbent (.95/.95: 16.4% to
  12.5% at 40 tasks, 36.5% to 29.7% at 100); on every other score it is much lower below 100 tasks (a 0..10 judge that is truly
  non-inferior at 40 tasks 27.8% to 2.7%, a mean of 5 attempts 60.2% to 0.5%, a true +0.10 on uniform scores at 24 tasks 19.5% to 6.7%).
  Not covered: scores that are not pass/fail with a coarse shape the 49 stress cells do not hold, sizes below 16 tasks, and the
  efficiency reads. Open, dated 2026-10-14 in the ADR: a fallback that does not pay for coarse lattices it cannot see, the edge
  between the two constructions (one half-credit task among a thousand moves a dataset from one to the other), and the weights,
  which were chosen on a grid and not derived. `TestCalibrationStudy` and `TestSparseCalibrationStudy` print the grids and are off
  unless `OFFLOAD_GATE_STUDY=1`.
- **Pre-registration floors and the frozen judge.** Freeze and check refuse `bootstrap_b` below 2000, `min_pairs` below 16
  or above the non-held-out tasks (the calibration was measured from 16 tasks up; on cost nulls at 16 tasks a metric
  falsely improved in 8% to 13% of runs at `B = 10` and 42% to 50% at `B = 1`, against 3.5% to 5.9% at 2000), a
  non-inferiority margin at or above the width of the primary range, and a `custom` primary without a valid range. The
  judge (kind and identity) is frozen in the baseline and the input must match it; `corpus_hash` is an echo, and the
  adapter that turns runs into arms is the trust boundary for corpus content. The gate version is 3: a baseline of another
  version is refused first, for its version, with the advice to re-freeze it, and that now covers a baseline frozen by the version
  2 gate as well as version 1 (version 2 was never released, but its primary interval changed, and verdicts moved on data that is not
  sparse too: on a 0..10 judge scale 16 to 40 pairs whose primary sat just inside a margin of 0.5 no longer clear it). A baseline
  frozen by the version 1 gate used to get "edited by hand?".
- **Exit codes and the rest.** 0 a verdict was produced (and `-h`, as the other verbs do: read the verdict from stdout,
  not the exit code alone), 1 a non-ADOPT under `--require-adopt`, 2 a refusal, a result that could not be written, or a
  usage error; a global `--config <path>` before `gate` is accepted and ignored. Freeze refuses any efficiency class but `tokens`, `money` and `memory_gib`, and any
  metric or primary name that carries a wall-time word, citing INV-5. The attempt enum is `eval.AttemptOutcome` (the name
  `eval.Outcome` is already the cascade eval's per-case struct), with `AllOutcomes` and `OutcomeFromRecord`, which maps an
  external record's outcome word (`not_checked` is `not_run`). `main.go` gains one case pair and one help line; the rest
  is `gate_cmd.go` and `internal/eval`.
- **Why.** The program's earlier proposals used three different pass rules, and this repository holds the pieces of two
  (`BootstrapDeltaMean` with a `B` and seed taken from each call site and its interval read by each caller, and ADR 0080's
  0.5 effect-size bar with no interval), so the same evidence could adopt under one and block under another, and a rule
  chosen after the numbers are seen is not a rule. The bootstrap already existed; the rules around it (scoring,
  margins, small n, binding the rules and the judge to the result) did not, and its percentile interval turned out not to be
  the right one for the primary. `agent-trajectory-gate` and the compaction A/B
  (`compeval.RunAB`) keep `B = 2000`, `seed = 42` and are listed as legacy, not migrated. Docs:
  `docs/systems/measurement-gate.md` (new, linked from `docs/README.md`) and ADR 0088 (Proposed).
- **Tests.** Red first, five times: the gate's types and a stub were committed with the rule tests failing, then the gate;
  after an adversarial review, a second red commit of attack tests (unfinished attempts, few resamples, a swapped judge, an
  unmeasured or costlier metric, a wide margin) before the fixes; after a third review, red tests for an unfinished
  candidate attempt's cost, a regression on few resolved pairs, the false-ADOPT cells and the refusal a version 1 baseline
  gets, each before its fix; and after a fourth, red tests for a sample with no spread (ties, constant wins and losses on
  both score ranges and both modes, and the sizes 60 and 61 where the rule of three crosses a margin of 0.05) and, in a
  second red commit, for an interval end that touches the margin by float rounding; and after a fifth, red tests for the exact
  false-ADOPT rate of pass/fail pairs where the arms barely disagree (reference figures from scipy), the reference ends of both
  constructions, the 73-tie threshold and a real version 2 baseline that must be refused for its version. `internal/eval` tests: a verdict table, boundary rows built from exactly representable
  numbers (16 pairs, scores 8.0, 7.5, 7.0), the refusal rows, imputation on every class and both arms, the cost of each
  attempt class on each arm, the three-ratio excluded share, held-out veto and missing evidence, the floors, the frozen
  judge and receipt shapes, the efficiency vetoes and the Bonferroni read, a byte-for-byte pin of `BootstrapDeltaMean`,
  determinism, the era marker, the power note, freeze then check byte-identical, every byte of a frozen baseline edited, the
  schema's enum and properties pinned to the Go types, and a baseline as the version 1 gate wrote it (raw bytes, hash
  pinned, through `CheckBaseline` and through the verb) and one as the version 2 gate wrote it. Calibration: `Gate` run end to
  end at the floors on eleven null cells (at most 4% over 4000 to 10000 trials, fixed seeds, skipped under `-short`; 0.0% to 2.5%
  measured), the interval itself over 50000 datasets on fifteen cells (0.0% to 2.5% measured), the exact sums of the false-ADOPT rate of
  pass/fail pairs on 30 sparse rows (at most 2.3%, pinned to scipy), the nested-loss candidate's rate as an exact binomial sum at 20
  sizes from 16 to 1000 tasks (0% to 72 tasks, 2.4% at 73, pinned to scipy), Tango's ends against scipy and the likelihood against a
  ternary search over thousands of draws, the augmented interval against numpy and against an explicitly augmented sample, the exact
  sum against simulation, the t table against scipy, and the study harness pinned to `BootstrapDeltaMean` bit for bit, to scipy's
  quantiles and to the moments of its generators. The `internal/eval` tests take about 10 s with the calibration cells (about
  6 s with `-short`) on a 36-core machine; the cells
  split their trials over `GOMAXPROCS`, so a 4-core runner takes a few times that. Root tests drive the verb
  in-process (freeze, check, verdict round trip, exit codes 0, 1 and 2, a failed write of the result on each verb, `-h`,
  write-once freeze, the INV-5 refusals, `schema`, the version 1 refusal). Mutation checks, each proved applied (`git diff
  --quiet` exit 1 after the edit, exit 0 after the restore): the imputation switch, the cheat score, the held-out zero-pair
  rule, both floors, the `min_pairs` bound, each judge mismatch and each receipt hash check, the negative-metric refusal,
  the unresolved cap (flag, verdict and the forced no-improve), the regression veto in both directions, the Bonferroni
  level, the margin bound and each of the four write-error wraps: all 26 turned a test red; and in the third round a
  candidate's unfinished cost dropped, a `not_run`'s cost counted, the incumbent's unfinished cost counted, a null cost
  counted as a zero in the divisor, a regression read only from `min_pairs`, the primary back on the percentile bootstrap, the zero-variance value read from a sum, the
  plain t interval, a nudged critical value, the version probe disabled, the version probe blind to trailing bytes, and
  three in the harness (percentile index, generator bias, quantile bisection): all 14 turned a test red; and in the fourth
  round the zero-spread interval read as the point, with the range width forgotten, held one-sided, not clipped to the
  range, and with a numerator of 4 and of 2; the verdict tolerance off, missing on the BLOCK side, missing on the ADOPT
  side and missing in superiority; equal differences compared exactly; and the unrecorded-cost rule off and its flag never
  set: all 13 turned a test red (each proved applied and restored with `git diff --quiet`); and in the fifth round Tango's
  critical value nudged, its upper end read at the wrong sign, its lower bisection started at the wrong place, one pseudo-pair
  at each end instead of two, the pseudo-pairs' degrees of freedom dropped, the clipping removed, the augmented interval centred on the
  augmented mean, a loss recognised from half a range away, the verdict tolerance removed and the gate version set back to 2: all
  turned a test red; two others survived (a clamp in the variance that the larger root of the quadratic makes dead code, removed,
  and the comparison of pairs with no rounding tolerance, killed by a new test).

### Added - `ledger --failures`: a failure-class register that groups the ledger's failed jobs by class and kind and joins each class to a human-maintained root cause and fix

- **The gap.** The ledger already says why every deferred row failed (the closed reason-code set) and `ledger` already prints the
  five most frequent reasons of the last week, but nothing counted failures by class across door, seat and model, and nothing kept
  what is known about a class next to its count. A failure that recurs was a log line read again each time. Of the register's
  columns, the counts and the example are facts the ledger holds; the root cause and the structural fix are a person's
  judgement, and code must not invent them.
- **The change.** `local-offload ledger --failures [--since DAYS] [--include-test-traffic] [--table FILE]` prints one JSON object
  and nothing else on stdout. A failed job row (deferred, a failing reason code, or a failed acceptance check; cache hits, dispatch
  markers, inner rows with a parent and refused attempts never) is classed by its reason code, which already holds the seven
  wire defer classes. A row that predates the stamped code is classed at read time by the ledger's own `DeferCode`, and counted in
  `class_derived_rows`. Each class carries its count and share, its kind, the most frequent short reason, and its rows counted per
  door, per seat, per model and per (door, seat, model). The ledger has no seat or model column, so the seat is `model_tier` as
  recorded; on a delegation row (task `agent_delegate`, `<node>:<seat alias>`) the model is the part after the last colon, which
  is the seat alias, a seat and not a model, and on every other row `model_tier` is a model alias and stays whole, a colon of its
  own included (`family:size`); a missing door reads `(unknown)` and a missing `model_tier` reads `(unrecorded)`. The report
  names its window in `since_days`. `ledger --failures=false` is the savings summary again; it used to exit 2 with a usage dump,
  because the summary's flag set did not know the flag.
- **A refused dispatch is not a failed job.** A node that answers a dispatch with a capacity refusal (a 503 or a 429) is a place in
  line: the delegator waits and places the subtask again, and the subtask's outcome is another row. The refused attempt wrote a
  deferred ledger row of its own, so the register counted a subtask that landed elsewhere as two failures and inflated
  `queue_full` and `job_rows`. The cause was in the writer: the refusal mark was set only on the delegation-log line. The
  ledger row now carries an omitempty `refusal` column from the same flag, and the register sets a marked row aside in
  `refused_attempts` (same window, same test-traffic rule, never a job and never a failure). A column was chosen over a new
  `phase` value because a phase that `CountsAsJob` skips would change what every job counter in the ledger calls a job (the
  savings summary, the share readers, the report, `loupe`), while the delegation log and the rigger already treat a refusal as
  a mark that a reader sets aside. The column is absent on old rows, so a ledger written before it counts its refusals as the
  failures they were then recorded as. `ledger_readers_lint_test.go` only governs callers of `ledger.ReadAll` and says
  nothing about columns; what a new column owes is `omitempty`, a comment on what absence means, and a row in the ledger-row
  table of `docs/FLEET-NODE.md`, all done. The other job counters set a refusal row aside as well; see "A refusal row is not a job" in this release.
- **Kinds.** Capacity, caller and policy defers sat in `failure_rows` beside stack faults, against `reasoncode.go` (congestion
  is a place in line, not a failure) and `agentwire.go` (the defer classes keep a node's capacity, the caller's contract and a
  broken stack apart). Every class now has a kind, closed and written in code (`internal/ledger/failurekinds.go`): `stack_fault`,
  `capacity`, `caller`, `ceiling`, `work`, and `unclassified` for `other` alone, a sixth kind added to the brief's five so that
  a failure nobody could classify is never counted as the stack's. A test fails for a failure class with no kind and for a
  kind written for a code the set does not hold; the kinds table on the page is held to the code by a second test. The report
  gives each class's kind, the failure rows per kind in `kinds`, and the stack-fault share on its own line
  (`stack_fault_rows`, `stack_fault_share`, null with no failure row). The classes that straddle two kinds
  (`no_eligible_node`, `poll_deadline`, `write`) are filed with a comment on their line saying which way and why.
- **No silent drops.** A row that fails to decode, a line cut off mid-file and a last line still being written are counted in
  `unparsed_lines` (a blank line is none). A ledger that is not there answers `ledger_found: false` beside its zeros, exit 0,
  and both conditions also print a note on stderr, so a missing ledger no longer reads as "no failures".
- **The human half.** `docs/systems/failure-register.md` holds the table (class, meaning, root cause, structural fix), one row
  for every failure member of the closed reason-code set. The page is embedded in the binary; `--table` reads another copy.
  Classes are joined to it by name: a class with no row says `undocumented` in every human column, an empty root-cause cell reads
  `not recorded`, an empty fix cell reads `open`, and a table row for a class the ledger does not hold adds nothing. The root
  cause and structural fix cells previously held restatements of the code's own comments for all 31 classes, so a row existed
  for every class, `undocumented_classes` was always empty and the gap detector could not fire. They now ship empty: no cell of
  the table was investigated by a person, so none is kept. A class counts as investigated only when its row holds a root cause
  or a fix a person wrote (a cell reading the defaults "not recorded" or "open" is not), `classes[].investigated` says so, and
  `uninvestigated_classes` lists the classes in the ledger that are not. A test fails for a non-empty cell on the shipped page
  until the class is named in the test with what the finding rests on. A test fails when the closed set gains a member the
  table has no row for, and when a wire defer class is filed under a different code than the ledger files it under.
- **Honest numbers.** Classes come out by count descending, then class name, and the output holds no clock reading: the same ledger
  and table give the same bytes. A class that has no rows in the window is not listed. A token total no row measured is `null`,
  never `0`: a row measured its card work when the ledger stores a figure above 0 for it, when its older token columns hold work,
  or when its stored 0 is the row's own (its reason code says no node ran the job, it names no node, or it is the node's own row).
  The classes that say no node ran the job are ten: `node_busy`, `capacity_wait`, `queue_full`, `no_eligible_node`,
  `queue_withdrawn`, `shed`, `dispatch_refused`, `config`, `contract`, `bad_input`. `write` was in that set and is not any more:
  a node refuses the write door before a run (`agent_allow_write` is false) or after one (the finished write set broke a door
  cap), and the class does not say which, so a delegator's stored 0 on a write row that names a node is a gap and reads `null`.
  A delegator's stored 0 for a job a node owned (`poll_deadline`, `node_unreachable`, a failed verification that came back from a
  node) is likewise not a measurement, because the node's work is not on that row. `rows_with_tokens` says how many rows the sum
  covers, so a total over fewer rows than `count` reads as a floor. Test, bake and measurement traffic is left out and counted,
  as in every production view of the ledger.
- **Surface.** `main.go` changes by two lines: the `ledger` case calls `runLedgerMode`, which sends `--failures` to the register and
  everything else to the verb as it was (taking a `--failures=false` out of what the verb parses), and the `ledger` usage line
  names the flag. The operator guide and the README list the mode. The reader lives in `internal/ledger/failures.go`. It scans the
  file itself (a 16 MiB line buffer, where `ReadAll` takes 1 MiB) because it has to see whether a row carried the `cards_tokens`
  key, which `Entry` cannot hold. `ledger_readers_lint_test.go` only inspects `ledger.ReadAll` calls from outside the package,
  so it does not see this reader: the test-traffic rule here is held by this feature's own tests. The one new ledger column,
  `refusal`, is written by `internal/delegate`.
- **Tests, red first.** The acceptance tests and a synthetic fixture ledger and table were committed before the reader existed
  and failed to build. They pin: deterministic bytes on the fixture (a committed golden file, flag order irrelevant), the class
  order including count ties, an unresolved total serialized as `null` and never `0` (the fixture lines are hand-written, because
  `Record` always writes the key), a zero the row owns kept as `0` and a delegator's zero for a job a node owned read as `null`,
  classes with no rows absent (test, bake, marker, inner, cache-hit and passing rows make none), the join by class name with
  `undocumented` for a class without a row and `not recorded` / `open` for empty cells, the table parser (CRLF, an escaped pipe, a
  second table ignored, malformed tables refused), the embedded page against the closed set, the verb's `--since` window (rows
  stamped relative to the clock), the default path with no `--table`, and that `ledger` without the flag is unchanged. The
  delegator-zero tests were written first and failed on an assertion, against a golden file written by hand from the rule. For the
  review round, each fix has its failing test committed before the fix: the writer stamping the refusal column (fails on an
  assertion: the ledger row of a refused attempt carries no mark), the register setting a refusal aside, kinds, the unreadable
  lines and the missing ledger (these three fail to build), `--failures=false` run end to end in a child process because the
  refusal is an `os.Exit` (exit 2 with the usage dump), the cut of `model_tier` on non-delegation rows, `since_days`, `write` as
  a class where a node's stored 0 is a gap, and the investigated rule with the shipped page's empty cells (a build failure, then
  an assertion on the page).
- **Tests, green by design.** A table test that sends a stored-0 row naming a node and a fleet job through each never-ran class was
  added after the reader and passed when written; it was added because dropping a class from the set left the suite green, and
  the mutation runs below are what show it pins them. The kinds table of the page against the code was written after the page and
  passed on its first run; the mutation below is what shows it pins it. Two assertions were added after a mutant survived: the
  stack-fault count against a capacity count that had tied it, and a leading colon kept on a non-delegation row.
- **Mutations.** Eighteen one-line mutants (the token pointer rule, the measured-zero rule, both sort keys, the run-class filter,
  the job rule, the acceptance rule, the example-reason tie-break, the orphan inner row, the window, the group order, the cache-hit
  rule, the three join defaults, the flag parser, the duplicate-row check and the dispatch itself), each restored byte for byte
  with `git diff --quiet` exit 0 afterwards. One mutant survived: a marker guard that the job rule makes unreachable, which was
  removed rather than tested. The cache-hit rule got its own test row and its mutant is red. A later round ran twenty-one more, on the
  rules added after review: the window's sign and unit, the default table and the last-colon cut (four); each of the eleven
  classes the never-ran set then held, dropped from the set one at a time (eleven); and each term of the zero-ownership rule, which
  is the never-ran lookup, the served-door term, the requester term and the three conjuncts of the no-node term (six). Seven of the
  eleven set members survived at first, because no test pinned them and the suite stayed green when one was dropped, and the table
  test above was written for them; after it every one of the twenty-one mutants was red.
  The review round ran thirty-seven more, each restored byte for byte with `git diff --quiet` exit 0 afterwards, and two survived
  at first. The refusal column: the stamp deleted from the writer, and the register's refusal branch, its counter, the
  run-class guard on the excluded count and its `continue` each removed (five). Kinds: one class refiled, the `other` entry
  removed, the stack-fault count read from the capacity count (survived, because the test's two counts tied; the test now has
  them differ), the kind tie-break reversed, the empty-register guard widened and the kind constant fixed (six). Unreadable
  lines: the counter, the found flag, the whitespace trim and the stderr note (four). `--failures=false`: the stripped
  arguments, the last-word rule and the strip itself (three); `since_days` (one); the model cut made unconditional and the
  leading-colon trim made unconditional (the second survived until a non-delegation row with a leading colon was added) (two).
  The never-ran set: `write` put back (one), and each of the ten classes it holds dropped one at a time (ten). The investigated
  rule: the default-text comparison removed, the uninvestigated list keyed on the row instead, and the fix cell left out of the
  test (three), a class moved in the page's kinds table and a restated cell put back on the shipped page (two). A mutant that did
  not build (an unused import, an unused parameter) was discarded and rerun in a form that does, and is not counted. No mutation
  log is committed; the tests are what keep the rules pinned.

### Added - `local-offload gate selftest`: the verb that writes the self-test receipt a model judge needs

- **What it does.** `gate selftest --pairs PAIRS.json --scorer ID [--out RECEIPT.json]` reads a JSON array of control pairs
  (`{name, good, degraded}`, the compaction eval's `ControlPair`), ranks every pair with the named scorer through
  `compeval.SelfTest`, and prints the receipt the gate's rule R7 asks of a model judge: `scorer_id` (the `--scorer` id, which
  is the judge identity the baseline freezes), `pairs_hash` (the SHA-256 of the pairs file's bytes), `passed`, and
  `build_sha256` (the SHA-256 of the running executable, the hash `verdict` prints as `gate_build_sha256`). The printed JSON
  is the `receipt` object of a gate input as it stands. The pairs file is read as strictly as the gate's other inputs: an
  unknown field, trailing data or an empty list is refused, and a leading byte-order mark is tolerated but still hashed.
  `--out` also writes the receipt to a file and replaces one that is there, so a failing run does not leave an older passing
  receipt behind. `internal/eval` still does not import `internal/compeval`: the verb is in the root package, and the
  gate only reads the receipt, as before.
- **Exit codes.** 0: the scorer ranked every pair. 1: it mis-ranked one (a tie counts); the receipt is still printed, with
  `passed` false, the mis-ranking goes to stderr, and the gate refuses that receipt. 2: the run never reached a finding, so
  no receipt is printed: no `--pairs` or `--scorer`, an unreadable, malformed or empty pairs file, an unknown scorer, a
  scorer that returned an error (a failed receipt would read as a statement about a scorer that was never asked), a build
  that cannot be hashed (the gate would refuse a receipt without a 64-character hash), or a `--out` that could not be
  written. So exit 1 of the gate verbs now means a finding: a verdict that is not ADOPT under `--require-adopt`, or a
  scorer that failed its self-test.
- **Scorers.** One, `compaction-outcome`: the outcome scorer of `compaction-eval ab` (accept, then entity recall over a
  summary of the transcript), through the configured seat. It moved, unchanged, out of the `ab` case into
  `compactionOutcomeScorer`, and both verbs call it. `selftest` is the one gate subcommand that reads `--config` (the others
  still ignore it). The scorer never returns an error: a seat that is down scores every side 0, which is a tie and fails
  the self-test with exit 1. The receipt binds the scorer id, the pairs and the harness build; it does not name the model
  that answered, which the runs' stamps carry. The scorer factory is a variable (`gateScorerFor`), so the tests put fakes
  where the seat would be.
- **Tests.** Red first: 16 new tests failed before the verb existed. They cover a pass (the four receipt fields, no stderr,
  no file written), the hash of the file's bytes (indented, compact and BOM-prefixed forms of the same pairs hash
  differently), a failure by an inverted scorer, a tied scorer and a second pair that mis-ranks (receipt printed, exit 1,
  the pair named), 17 refusals each with its own reason, a scorer error (exit 2, no receipt, cleanup still run), the cleanup
  count, `--out` (same bytes as stdout, a stale passing receipt replaced), five unusable build hashes, the real executable's
  hash, the config path reaching the factory from before and after the subcommand, the real factory's one known id, the
  usage text, and the round trip: a passing receipt (read from the `--out` file) is accepted by `gate verdict` for a model
  judge with the same identity and a model-judged input is refused without one, a failing receipt is refused with exit 2, and
  a receipt for another scorer is refused. The scorer's own numbers are pinned (deferred 0, entity-free 0.2, recall, the cap
  at 8). Twelve mutants of the verb were run, each confirmed applied and each killed: the failure exit changed to 2,
  `passed` forced true, the hash taken of the decoded pairs, a scorer error treated as a finding, the build check removed,
  the cleanup skipped, an empty list allowed, `--out` not written, lenient JSON decoding, a wrong scorer id and a wrong
  build hash in the receipt, and the receipt not printed. No test starts a process or needs a seat; the real scorer was
  not run against a seat.

### Added - the admitted third-party software table, and the leak gate pinned over the OpenSpec tree

- **What it does.** `docs/systems/security.md` gains the public table "Admitted third-party software": one row per artifact and
  pinned version, with its licence, what it is used for, how its telemetry is switched off before the first run, and where its
  data goes. The first row is the OpenSpec CLI (`@fission-ai/openspec` 1.14.1, tarball sha256 pinned). `.gitignore` keeps the
  per-tool skills and commands that `openspec init` and `openspec update` generate under `.claude/` and `.opencode/` out of the
  tracked tree; the `openspec/` tree itself stays tracked.
- **Why.** OpenSpec writes spec and change prose about this repository, the likeliest place for a machine name to reach the public
  tree. The tree gate already scans every tracked path with no filter, so the gate needed no new scope, only tests that keep it
  that way, and the generated tool copies needed to stay out of the tree (they would add tool prompt text and drift from the
  tool version that wrote them).
- **Tests.** `TestScanTreeCoversTheOpenSpecTree` (`internal/leakgate`): bodies and names under `openspec/` are flagged, YAML and
  Markdown alike; a mutation adding an `openspec/` path filter to `ScanTree` turns it red (the mutation was proved applied by a
  non-empty diff, then restored byte-clean). `TestOpenSpecGeneratedToolFilesStayUntracked` (root): red first on the five generated
  paths, green with the `.gitignore` lines, and it also fails if the `openspec/` tree or another tool file were ignored.

### Added - the openspec/ tree, a forked harness-change schema with an optional ADR artifact, and the pinned, lockfile-backed install and validate scripts

- **What it does.** `openspec/` holds the spec and plan artifacts of a change for the pinned OpenSpec CLI (1.14.1). `config.yaml`
  names the project-local schema `harness-change`, carries the repository's standing invariants as the `context` every artifact
  is written under (quality outranks speed with no wall-time term in a pass rule, no CPU inference, idle models unload at 300 s,
  no daemon or scheduler, public-repo privacy with node letters only, one changelog fragment per change, ADR for a decision,
  red-first tests, the leak gate), and adds per-artifact `rules` (a design names the ADR it needs, tasks start with a failing
  test and name their verifying command). The schema is a fork of `spec-driven` that keeps proposal, specs, design and tasks and
  adds `adr`, an ADR draft that requires `design` and that nothing requires, so it never blocks tasks or apply. The OpenSpec
  artifact schema has no `optional` key (an unknown key is accepted and ignored), so the optionality is the shape of the graph,
  pinned by a test; the generated archive skill still asks for confirmation when a change has no `adr.md`, and the system page
  says to confirm and move on. `scripts/openspec-install.sh <prefix>` fetches the tarball with `npm pack
  @fission-ai/openspec@1.14.1 --ignore-scripts`, refuses on any sha256 other than the admitted one, stages the committed
  `scripts/openspec/package.json` and `package-lock.json` beside that verified file and runs `npm ci --ignore-scripts` there,
  so the CLI and all 69 dependencies are exact versions under sha512 integrity values and none can drift between installs. It
  checks the staged CLI's version before moving `node_modules` into the prefix, replaces an earlier install of this CLI there
  and refuses a `node_modules` that is something else's, keeps npm's cache, temp files and the CLI's config home under the
  prefix, and prints the four environment exports (telemetry off, update check off, config home).
  `scripts/openspec-validate.sh` sets the three switches and runs `openspec validate --all --strict --no-interactive` plus
  `openspec schema validate` for each local schema, which `validate --all` never reads. `docs/systems/openspec.md` is the new
  system page, linked from the admitted-software row in `docs/systems/security.md` and from the docs index; CONTRIBUTING's
  pre-pull-request list now names the validate step for a change under `openspec/`, because `go test ./...` runs the real
  validator only where `OPENSPEC_BIN` is set.
- **Why.** Specs and plans for a change had no fixed shape and no validator. The pilot gives them both without touching how a
  change is executed or shipped: OpenSpec owns spec and plan artifacts only, and no other workstream depends on it. Pinning the
  tarball alone left the dependency set to the registry at install time; the lockfile closes that.
- **Tests.** Red first (the tests were committed before the tree, the scripts and the lockfile existed). The structural tests
  need no CLI: the config names a schema that exists, the spine is intact and `adr` requires exactly `design` and is optional
  by the graph, rules name real artifacts, the context keeps each invariant (including the no-cloud-fallback line), and the
  lockfile holds exact versions, sha512 integrity values and registry URLs for every package. The script tests run both scripts
  against stand-in `openspec` and `npm` programs: the switches are forced whatever the caller exported, the strict arguments
  are exact, a failing validator fails the gate, a tarball with the wrong hash is refused before any install and leaves no
  scratch directory, the script's pins equal the admitted row, and a matching tarball installs through `npm pack` then
  `npm ci` in a staged copy of the committed lockfile project with npm's cache and temp folders under the prefix in both steps
  even when the caller exported others, a wrong installed version installs nothing, an earlier install is replaced and a foreign
  `node_modules` is left alone. `TestOpenSpecInstallScriptInstallsTheRealPackage` runs the script for real (npm and the
  network) when `OPENSPEC_INSTALL_TEST=1`, with every home, data and temp folder pointed at an empty folder beside the prefix,
  and fails if any gains a file or if the installed packages differ from the lockfile; it skips with the reason elsewhere.
  `TestOpenSpecTreeValidates` runs the real validator where `OPENSPEC_BIN` names it and skips with the reason elsewhere.

### Added - `offload_research` reads RSS/Atom feeds and, when an operator names a yt-dlp, YouTube subtitles

- **What it does.** Two text sources join pages, both fetched delegator-side with no login and no cookies, and both end in the
  same digest path, `internal/untrusted` fencing and public-web guard a fetched page uses. A response whose content type is an
  RSS or Atom media type, or whose body sniffs as a feed, is parsed with `encoding/xml`: item titles, links, dates and summaries
  become the text, at most 50 items and 1,000 bytes of summary each, and item links are never followed (a relative link is shown
  resolved against the feed's `xml:base`, else the address it was fetched from). A YouTube video URL (`watch?v=`, `/shorts/`,
  `/live/` and `/embed/` on `youtube.com`, `www.`, `m.` and `music.youtube.com`, `/embed/` on `youtube-nocookie.com`, and
  `youtu.be/<id>`) goes to the yt-dlp named by the new config key `ytdlp_path`, run once on the canonical
  `https://www.youtube.com/watch?v=<id>` with subtitles-only flags (`--skip-download --write-subs --write-auto-subs
  --sub-langs=<sub_langs, default en.*> --sub-format=vtt/srv3/best --no-cookies --no-cookies-from-browser --ignore-config
  --no-playlist --no-cache-dir --socket-timeout 15`, zero retries, warnings left on) in a per-call directory under the state
  root (else the OS temp dir), with a scrubbed environment, bounded by the call's deadline. WebVTT, SRV3 and SubRip are converted
  to plain text with timestamps, cue settings, tags and the rolling duplicate lines of automatic captions removed.
- **Off by default, and how far the path is trusted.** `ytdlp_path` is empty by default, which leaves the adapter off and a
  YouTube URL an ordinary page. The harness never searches `PATH` for the binary and never installs it; a relative path is
  refused. `ytdlp_path` is trusted as written: nothing checks the file unless the optional `ytdlp_sha256` is set, and then the
  file is hashed before every run and a mismatch refuses to run it (`ytdlp_path` error, nothing started). On Windows the file
  must be an `.exe` or a wrapper `.cmd` (the admitted zipapp is a shebang script that runs directly only on POSIX), and with a
  wrapper the hash covers the wrapper, not the interpreter or the script it calls. `offload_research` gains an optional
  `sub_langs` input (letters, digits and `. * _ - ,` only, so no `cmd.exe` metacharacter reaches a wrapper); its sources rows
  gain `error_kind` and `cleanup_error`.
- **A deadline ends the whole process tree.** The documented Windows shape is a wrapper `.cmd` that runs the interpreter, so the
  program that holds the per-call directory is a grandchild, and killing only the wrapper left it running with the directory as
  its working directory. A deadline or a cancellation now ends the tree (`taskkill /T /F`, started hidden, on Windows; `SIGKILL`
  to the process group on POSIX, through the new `winexec.KillTreeOnCancel`), the child is started through `winexec` so it opens
  no console window, one run is capped at 15 minutes, and a binary that exits 0 while a child still holds the output pipe is read
  as the success it is. The directory is removed with a bounded retry of a few hundred milliseconds; a removal that still fails
  is recorded as `cleanup_error` on the source row instead of being dropped, and each call first sweeps `ytdlp-*` directories
  directly under its own scratch root that are older than any run can be. A child whose parent had already exited when the run
  ended stays out of reach of the Windows tree walk; its hold shows as `cleanup_error` until the sweep takes the directory.
- **Feeds are read as they are served.** The text is decoded as the response's `charset` parameter names, else the XML
  declaration, else UTF-8: windows-1252 and cp1252 feeds (and iso-8859-1 and latin1, read as windows-1252 the way browsers do)
  keep their curly quotes, euro signs and dashes instead of turning into C1 control characters. An invalid UTF-8 byte or a
  character XML forbids no longer ends the parse at that item with every later item lost. A document that stops before its end
  says so first in its header, is marked `truncated`, and does not count the item it stopped in ("Items: 2 of 3 items shown" for
  four items is gone). A feed with no item or entry, and an RDF document that does not declare the RSS 1.0 namespace, are read
  as the ordinary document they were before. `golang.org/x/text` moves from an indirect to a direct requirement for the decoder
  (`go.sum` is unchanged).
- **A site break is a typed error, never a retry.** A non-zero exit is `subtitle_site_break`; an exit with no file is
  `no_subtitles`, and its reason ends with the last line yt-dlp printed, because it reports a refused subtitle download (an HTTP
  429 rate limit) as a warning and exits 0; a subtitle file of blank cues is `empty_subtitles`; a canonical URL that fails the
  public-web guard is `url_refused`; and `ytdlp_path`, `bad_sub_langs`, `unsupported_subtitle_format` and `subtitle_timeout`
  name the rest. A SRV3 track that ends inside a cue (cut by the read bound or interrupted) keeps the cues read before the cut
  and is marked `truncated`. The batch keeps its shape and the other URLs are untouched.
- **Security.** Every URL still passes `research.ValidateURL` before anything is fetched or run, and the canonical
  `https://www.youtube.com/watch?v=<id>` handed to the binary is validated as well: a hostname that resolves to a private,
  loopback, link-local or CGNAT address is refused before any process starts. The matcher is exact (a lookalike host, a userinfo
  trick, a trailing-dot name, look-alike letters, an encoded backslash or another port is an ordinary page), the adapters build
  no HTTP client, and after that pre-flight name check yt-dlp resolves and dials on its own, outside `netguard.PublicTransport`,
  which is stated in `docs/systems/mcp-server.md` and is why the adapter is off until configured. Audio transcription is not part
  of this change. `docs/systems/security.md` gains the yt-dlp rows in "Admitted third-party software": the zipapp (tag
  2026.08.19, its sha256, Unlicense) and the Windows `.exe` or wrapper shape, whose hash the operator pins in `ytdlp_sha256`.
- **Tests.** Red first (the failing tests were committed before the code, and the process-tree tests were also run red on Linux):
  a fake yt-dlp (the test binary playing the part, no network and no real yt-dlp anywhere) returns a subtitle digest;
  rolling-caption dedupe; RSS 2.0 and Atom fixtures, sniffing and the item cap; the SSRF refusal with a hostname resolving
  private through the `netguard.LookupIP` seam, for both the written and the canonical host; lookalike hosts never reach the
  binary and the adapters add no HTTP client; the adapter is off with an empty path; a site break gives a typed error with the
  fake invoked exactly once; the child environment is scrubbed; the digest input of subtitle text equals that of the same text
  fetched as a page (no model is called); a wrapper script that leaves a grandchild running is ended with the tree and the
  scratch directory goes with it (a `.cmd` on Windows, a shell script on POSIX); stale scratch directories are swept; a removal
  that fails is retried and then recorded; a hash mismatch or a malformed `ytdlp_sha256` starts nothing; `sub_langs` refuses
  every `cmd.exe` metacharacter; and the feed cases above (cp1252 bytes 0x80, 0x93, 0x94 and 0x96, a stray byte, a body cut
  mid-item, relative links with `xml:base`). Each fix was checked by one-line mutants that were red on the suite and restored
  byte for byte, among them the tree kill, the wait-delay success, the sweep's name, age and file-type checks, the retry, the
  hash comparison, the charset, the byte repair, the cut header and the new URL shapes; a repair that `bytes.Map` duplicated and a
  format check the mismatch path duplicated each survived the first tests and got tests that kill them.

### Added - `offload_review_diff` results are bound to the diff they judged: SHA-bound evidence, a NOT_RUN outcome, gates before the seat, a second fingerprint check and a durable trace

- **What it does.** A review used to say what a seat thought and nothing about what it thought it about, and a finding the caller skipped left no record. Every `offload_review_diff` payload, deferrals and early refusals included, now carries an `outcome` and an `evidence` block (new file `internal/reviewlane/evidence.go`). `evidence` holds the sha256 of the diff bytes (`diff_sha256`, `diff_bytes`), the optional `base_sha` and `head_sha` the caller passes (hex, echoed and never resolved: the lane has no git access, and the bytes cannot be mistyped the way a commit id can), `prompt_sha256` (the exact prompt the seat was shown), `gates` (the deterministic gate receipts) and `seat` (the seat receipt: seat, node, executed-on, steps, stop reason, whether the node filed the run as a deferral, the digest of its raw answer, no timing). `outcome` is one of `PASS`, `FINDINGS`, `GATE_FAILED`, `NOT_RUN` or `STALE`, chosen in one function (`reviewlane.Decide`), with an `outcome_reason` for everything but `PASS` and `FINDINGS`.
- **NOT_RUN, and what PASS needs.** `NOT_RUN` means no clean seat reviewed the diff, and it is never a `PASS`: every result with `deferred: true`, a runner error, an early refusal (both `diff` and `diff_path`, neither, a `diff_path` that cannot be read or is over its cap, a bad `read_root`, a working directory that cannot be read, arguments that do not decode), a refused binding (malformed commit id or receipt), a result with no seat receipt, an answer that read as NONE while every line it wrote was discarded (the `dropped_*` counts become the filtered total inside `Seal`), a diff over `MaxDiffBytes`, and a run the node filed as a deferral whose answer the lane read as a clean NONE. The salvage still publishes what such a run wrote and findings read from it stay `FINDINGS`; only its clean NONE is never an all-clear, and `seat.deferred` records the attempt. A review refused before any diff was read is a diff-less `NOT_RUN`: `diff_bytes` is 0, there is no `diff_sha256`, the refusal is `evidence.refusal` and `outcome_reason`, and the deferral shape and reason are the ones the door always gave. The oversize rule lives in `Decide` as well as in the contract builder, so a caller that skips the builder cannot reach `PASS` with an unreviewed diff. `PASS` needs a seat with a receipt, a diff fingerprint and a second check equal to it (a result with no `diff_sha256` is `NOT_RUN`), a diff inside the ceiling, every gate receipt green and for the reviewed commit, an unchanged diff at the second check, and the seat's own NONE.
- **Gates run first, and the lane runs no lints or tests itself.** `evidence.gates` is built before any seat is contacted, so even a refusal after the diff was read is bound to the bytes it refused. The lane's own gates (`diff-size`, `conflict-markers`: a merge-conflict marker on an added line) come first, then the `gate_receipts` the caller hands over, the results of lints and tests it ran itself (`name`, `status` pass or fail, `detail?`, `head_sha?`, at most 16). Those receipts are the caller's word, which the lane cannot verify, and it executes nothing a caller names, so the review door does not become a command-execution surface. A receipt for a different head commit is marked `stale` and cannot support a `PASS`; once the review names a head, a receipt that names no commit is marked `unbound` and cannot either. A review with no head is tied to no commit: `evidence.head_unbound` is true, a receipt that names a commit is `unbound`, and a clean `PASS` carries the reason "no head_sha: gates not tied to a commit". Even with a head, nothing ties it to the diff bytes: the fingerprint binds the bytes and the head is the caller's word. A red gate does not stop the seat (the lane stays advisory and the findings of a diff that fails a lint are still worth reading); it turns a clean answer into `GATE_FAILED`.
- **Second fingerprint check.** Right before the result is returned (`reviewlane.Seal`), a `diff_path` is re-read through the same confined reader and compared with the first fingerprint (`recheck_sha256`). A difference, or a file that can no longer be read, makes the result `STALE`: the findings are about bytes that no longer exist. For an inline diff the check compares the string with itself, so it cannot detect movement and `recheck_sha256` equals `diff_sha256` there by construction.
- **Advisory findings always leave a trace, and the trace is the durable record.** Each sealed review appends a `review` row and one `finding` row per published finding (severity, file, line, the claim and why cut to 512 bytes, and a digest of the whole finding) to `review-evidence.jsonl`, whether or not anyone reads or applies the findings. The path is derived from the configured ledger file (`reviewlane.TracePathFor`: the ledger's directory, the harness home when no ledger path is set), so it always sits beside the ledger wherever the operator placed it. A failed write is reported as `trace_error` on the result. No ledger row is written for the verdict, on purpose: every ledger reader counts a row as a call or a job, the pipeline already writes the run's own row (door `offload_review_diff`) before the outcome is known, and the ledger keeps hashes where this trace keeps the short finding text.
- **Stays advisory.** An outcome is a label on a result a lead reads; nothing here blocks a merge or a commit. All new JSON is additive (new keys, `omitempty` inside `evidence`), the deferral shape and the findings are unchanged, and the seat's prompt is still the task and the diff and nothing else.
- **Tests.** Red first (commits `test(reviewlane)` and `test(mcpserver)` before the implementation, each time: the feature, then its evidence-binding fixes). `internal/reviewlane/evidence_test.go`: NOT_RUN with no clean seat (five shapes), a diff-less refusal, a node-deferred clean NONE (and its findings staying `FINDINGS`), the `dropped_*` counts through `Seal` one key at a time, the seat receipt's provenance through `Seal`, PASS refused when oversize (and allowed at exactly the ceiling), the fingerprint, a recheck that disagrees is STALE, gates attach before any seat receipt and a red one blocks `PASS`, a receipt for another commit or naming none cannot support `PASS`, the lane's own gates and the closing conflict marker, refused bindings and the receipt and commit-id bounds, the trace path beside the ledger, distinct `review_id` values, the seat receipt carries no timing, the evidence JSON omits what is unknown, a trace row exists for an advisory finding (and for a NOT_RUN), the recheck runs once inside `Seal`, an unwritable trace is loud, trace text is bounded and rune-safe, and the owner-only trace file mode (a Unix-only test). `internal/mcpserver/reviewevidence_test.go` and `reviewfence_test.go`: the door end to end (bound result and trace, clean PASS, every deferral NOT_RUN, every refusal exit sealed (the seven early refusals, three of them, the unreadable working directory, the `read_root` that cannot be made absolute and the read that returns no document, through a file seam, `internal/mcpserver/reviewdiffio.go`, because no real file system causes them on every platform; the refused binding, the builder's refusal and the runner error are covered beside them), a second read of the `diff_path` that returns no document is STALE, oversize, refused bindings never reach the seat, red and unbound gates, a `diff_path` rewritten or deleted during the review is STALE, the trace beside a ledger kept away from the home, the advertised schema) and both fenced-seat call sites (the fleet's delivered review and the salvaged deferral, with executed-on, node and trace rows). Nineteen mutations of `evidence.go` are each caught by a named test, except the file-mode one whose test only runs on Unix; and each of the handler's ten refusal exits, mutated into an unsealed return, fails a named test.
- **Where.** `internal/reviewlane/evidence.go` (new), a hunk in `handleReviewDiff` and `internal/mcpserver/reviewdiffio.go` (the file seam of the `diff_path` branch; three optional inputs, the seal at each exit including the early refusals, `publishReview` folded into a local `publish` step, the trace path from the ledger path), the tool's input schema and description, `docs/systems/mcp-server.md`, and the `README.md` row.

### Added - template-to-API converter: a ComfyUI UI-format workflow in, the API-format prompt graph out

- **What it does.** `render/template-to-graph.mjs` converts the UI-format workflow a template ships (`nodes[]`, `links[]`,
  `definitions.subgraphs`, `widgets_values`) into the `{"<id>": {"class_type", "inputs"}}` graph that `POST /prompt` and the
  render builders take. It drops muted nodes, passes bypassed nodes through by type (the i-th output of a type takes the i-th connected input of that type, and unequal counts are refused as `ambiguous_bypass`), expands subgraphs (ids
  `<instance>:<inner>`) with their input and output rewiring, resolves `Reroute` and `PrimitiveNode`, skips `Note` and
  `MarkdownNote`, and maps `widgets_values` to named inputs in the `/object_info` listing's input order, counting the hidden
  `control_after_generate` slot, the upload slots and the dotted names of dynamic combos and autogrow groups. `--seed` pins the
  seed inputs and warns about a seed-named input it left alone (`seed_not_pinned`). A template whose `minComfyUIVersion` exceeds the ComfyUI version passed in is refused with a typed reason (the
  version is passed in, never assumed, a known minimum with no version given is a refusal too, and a version given with no minimum to compare it with is a `version_check_skipped` warning); an unknown node class, a
  widget layout that does not add up and an input type with no rule are typed refusals, never a guess. A JSON CLI prints the
  graph or the refusal (`--template`, `--object-info`, `--comfy-version`, `--seed`, `--index`). It is a pure function of the
  template, a listing the caller supplies and options: no ComfyUI, no GPU, no network, no file written, and
  `render/comfy-lifecycle.mjs` is untouched. Wires are followed in a loop with one visited set, behind which a bound on the hops of one resolution (the wires of every
  scope entered; a longer walk has used a wire twice) refuses a loop as `link_cycle` even if the set failed, so a chain of 99,998 `Reroute`
  nodes resolves without a stack overflow; expansion is bounded (more than 100,000 nodes of any kind, subgraph instances
  included and every expanded scope counted, more than 1,000,000 links over every scope entered, or subgraphs nested more than 64 levels, is a typed `graph_too_large` refusal; at most 1,000 warnings are kept and the rest are counted in `warnings_dropped`), and a node id longer than 256 characters, a node id two scopes both give or a subgraph definition id
  that appears twice is a `bad_template`, never whichever came last. Nothing calls it yet. Clean room: the GPL-3.0 frontend and ComfyUI's server code
  were not read as source.
- **Why.** `run-graph` and `POST /prompt` take API-format graphs only and the templates catalog could list and classify
  templates but not run one. This is the harness-side converter the catalog's docs said was missing, without a
  third-party custom node.
- **Tests.** `node --test render/template-to-graph.test.mjs` (97 tests, plus two that read private data and skip
  unless `TEMPLATE_GOLDEN_DIR` or `TEMPLATE_SURVEY_DIR` is set): one per behaviour, on hand-built UI-format graphs, a synthetic
  listing and two hand-worked goldens; a mutated widget order turns a golden red, the version refusal and the unknown-class
  refusal are pinned. Written red first (the stub commit fails 51 of 55). A run of 24 one-line mutations of the converter,
  each applied to a copy and proved applied, turned the suite red every time; a review then found twelve that survived, the fixes
  for its findings were written red first (8 of the new tests failed before them), and a later run of 34 one-line mutations, those
  twelve among them, turned the suite red every time too. A release review then found the subgraph output-slot match
  unpinned (dropping it left every test green and put 37 wrong wires into 9 converted templates of the survey below); a synthetic
  test with three outputs of three types read in crossed order now pins it, the graph check compares each wire's output type with
  the type the target input declares, and tests pin cycle detection through bypassed nodes, a bypassed input with no `link` key,
  99,998-hop chains, an expansion bomb, the nesting bound and both id collisions; each of those tests was shown red against a
  one-line mutant of the converter. An independent check then found the expansion cap charged only live nodes and instances, so a
  template of a few hundred kilobytes (nested definitions stuffed with `Note` nodes, 65,535 instances) ended in an `internal_error`
  after gigabytes of memory; every node met is now charged first, and tests pin the Note-, muted-, bypassed-, Reroute- and
  primitive-heavy bombs and an exact boundary of mixed kinds, the three undefined-link guards, which guard found a loop (the hop
  bound must not hide a visited set that stopped working), and a 99,998-hop chain that runs on a worker under a test-runner timeout.
  A last independent check found that a hostile template could still exhaust memory or time in ways the node cap does not see;
  one test each, written red first, pins the wire cap (a definition of one `Note` and 2,000 links instanced 501 times is refused,
  500 convert), the 1,000 kept warnings with the rest counted in `warnings_dropped`, and the 256-character node id.
- **Limits.** The caps bound the nodes of every kind, the wires, the hops of one wire resolution, the nesting depth, the
  warnings kept and the length of a node id. They do not bound the `widgets_values` copied per instance, nor the scans of a
  definition's inputs and links that each instance, and each read of one of its outputs, makes, so a hostile template can still
  use time and memory in proportion to the size of its values and definitions times the number of instances. The converter is
  meant for trusted template packages.
- **Checked against real data, privately.** On two saved workflows with an API-format file beside each, the converted graph
  matched value for value (50 of 50 and 20 of 20 nodes, one a subgraph template with dynamic combos). Over the 593 files of
  `comfyui-workflow-templates-json` 0.1.96 with an `/object_info` listing saved on 2026-08-14 (older than the package), 405
  convert and every converted graph is a sound graph (each wire reaches a node in the graph and an output slot its class
  has, and carries a type the target input accepts); the other 188 are typed refusals (unknown classes newer than the listing, schema skew, custom frontend widgets, files
  that are not workflows, two bypassed nodes whose outputs cannot be paired with their inputs). A fuzz of 29,928 malformed-field
  variants of the convertible ones ends in a graph or a typed refusal every time, never an `internal_error` (921 on the first version).
- **Not done.** The acceptance comparison, at least 10 non-API templates node by node against exports of the frontend, is a
  follow-up (the two private pairs are API-format files whose origin is unverified, and every node in both is mode 0, so no
  comparison covers a bypassed or a muted node): it needs a running ComfyUI for a current `/object_info` and a scripted frontend export, which is work under a
  GPU lease, and it was not run. Docs: `docs/systems/media-generation.md` ("Template-to-API converter"), `render/README.md`.

### Added - `tools_schema_profile = "compact"`: the advertised MCP tool list with every tool and parameter kept and the prose cut to a reviewed keep-list, with a gate that reads every sentence

- **The defect.** A client that sends every MCP tool schema on every call (opencode has no deferred tool search) pays the whole advertised list as prompt tokens on every turn, and the list had grown: re-measured at this build with the Qwen3.8 tokenizer (vocab-only, no weights, no GPU), the 31 tools of a delegation-enabled box are 31,582 tokens in the seat's chat-template rendering, against the 20,868 tokens (35 tools) recorded at 0.137.0, and the plugin's twelve `recon` tools are 12,068 against 8,265. Both old figures no longer hold.
- **The change.** A new config key `tools_schema_profile`: `"full"` (the default, and what an unset key means) advertises every description as written; `"compact"` advertises the same tools with the prose cut. `"compact"` keeps every tool, in order, and every parameter name, `type`, `enum`, `items`, `required` entry and numeric bound at every depth, so a call valid against the full list is valid against the compact one. It cuts each description (a tool's and each parameter's) to its first sentence, grown to the next sentence when the first is under 28 characters and shortened at a clause boundary when it runs past 320 characters (tool) or 200 (parameter) and the dropped part carries no rule word, plus the sentences a reviewed per-tool keep-list names, verbatim and in the order the description has them. It drops `examples`, `example`, `title` and `$comment`. A value other than `full` or `compact` fails the config load by name, and the message describes compact by its reviewed keep list; a hand-built `Config` with one reads as `full`. The profile is read once, when the MCP server is built (the rewrite is a middleware installed at that moment), so changing the key takes an MCP server restart or a client reconnect; the operator guide and the config comment say so.
- **Where it is wired.** `internal/mcpserver/toolsprofile.go`: a receiving middleware on `tools/list`, installed only when the profile is compact; the keep-list is `internal/mcpserver/toolsprofile_keep.go` (144 entries, each with its reason and the sentences it cannot be read without). The result's tools alias the registry, so each is copied before it is changed; the registered schema, the handlers and the call-time checks are untouched, and anything the rewrite cannot parse stays as it was (the failure mode is a full schema, never a broken one). Schemas are rewritten with an order-preserving JSON walk, so parameters keep their order. `config.example.json` is regenerated (`go run ./cmd/genexample`), and `docs/OPERATOR-GUIDE.md` has the key's row.
- **What `full` changes: one tool, three characters.** `offload_status`'s description carried three clauses that tell a caller how to read the live roster ("Trust it over any rules file or written figure", "idle_agent_nodes > 0 means paid-for capacity is sitting unused", "Read verdict before concluding anything from held; a held card is queued behind with queue_with, never refused") inside one 2,000-character `Returns {...}` sentence that compact drops whole as a result shape: the sentence sits in one open brace, so the sentence splitter never cut inside it. The source text now closes that brace before the clauses and ends the shape's second half as a sentence of its own: one `}` moved, two `;` became `.`, no word changed, and the description keeps its length. Everything else in every `full` list is the text main has. The SHA-256 of the delegation-enabled default `tools/list` result is `8505e9ca79f68602...` on main (main's research text-sources change moved it from `81c5bc5e06868cbd...`) and `8eb5f938438ad426...` on this branch (`8b340c5629c73f58...` before main's change was merged in); the token count of that list is 31,582 on both. Checked again after the merge over ten surfaces (the bare default box, the delegation-enabled box, the 45-tool surface, the fleet-accelerator variant, and each of the three devices alone as a local and as a fleet accelerator): in each of them `offload_status` is the only tool whose bytes differ from main's, except on the two fleet surfaces that list the Hailo-8L, where `offload_text_embed` differs too (the forwarding-note fix below, which was already on the branch); with the three characters put back, and on those two surfaces that note, every one of the ten lists is byte-identical to main's. `offload_research`, the one description main changed in the range, is byte-identical to main's on all ten. With no middleware installed for the default, the profile code itself changes nothing; `TestDefaultProfileIsFull` pins that an unset key, `"full"` and `Default()` agree and that compact differs.
- **What compact keeps is a reviewed list, not a word rule.** The first version kept every later sentence that held one of a closed list of safety words. A review of the live descriptions showed it wrong in both directions: a mutation sweep removed each of its 18 words in turn and 15 left every compact test green, it dropped sentences a caller relies on (the only cloud tool and its opt-in rule; the video generator's "pass `model` ONLY to override that deliberately" and "set `fast=true` ONLY when the caller explicitly wants a draft"; the write door's caps and grants; the "if it can't ... it returns `deferred:true` and you should do it yourself" sentence of five lanes; "an EMPTY findings list ... is not a verification"), it kept a "Non-zero means a node is broken" sentence cut loose from the `summary.infrastructure` sentence it is about, and it kept by accident 49 distinct sentences that are result shapes or queue mechanics. The second version replaced it with a reviewed keep-list and a gate over every sentence that held a trigger word, and a review of that gate found it was still a keyword list.
- **The gate reads every sentence.** `TestEveryConstraintSentenceIsKeptOrDroppedWithReason` reads the live full descriptions of every tool and parameter, exactly as `tools/list` emits them, on eight surfaces: the delegation pair and the browse lane on, with the three accelerators together as local devices (the 45-tool surface), the same three together as fleet accelerators, and each of the three alone, once as a local device and once as a fleet accelerator. For every sentence outside the opening one(s), whatever words it holds, it requires that the sentence is in the compact description of that tool and parameter verbatim or is a row of the dropped list (`toolsprofile_dropped_test.go`: 240 rows covering 296 tool and sentence pairs, each with one of eleven reason classes: result, mech, valid, schema, restated, enum, rationale, descr, defer, choice, advice). Four rules reworded without a listed word ("Always pass the token twice before sharing it.", "You should send the audio as mp3 first.", "Avoid sharing the token with anyone.", "Make sure the key is rotated after use.") had been dropped with the gate green; each of them, and a fifth sentence with no rule word at all, now adds exactly one problem when it is inserted into a live description (after the opening sentence of a tool's, at the end of another tool's, after the opening sentence of a parameter's) and when it is inserted into the source of `offload_summarize`. The trigger words (the constraint words, `defer*`, `always`, `should`, `avoid*`, `make sure`, `prohibit*`, `ensure*`, and the words that say the caller's data leaves the box: `forward*`, `travel*`, `fleet node`, `tailnet`, `sent`, `cloud`) are the second check: a dropped sentence that carries one needs a reason of its own on its row (30 characters or more that restate no class), and a long opening sentence is shortened only where the part it drops carries none. The production lead guard has the same words and a test pins that every word of the gate is a word of the guard. The rows were generated once from the classification compact had (344 tool and sentence pairs outside the keep-list, 279 distinct sentences) and read one by one; twenty of them hold a rule or warning that no keyword had named and are on the keep-list instead (what a list in `remotes` does, that `allow_browse` pins the whole call to this box, how to resume a queued place with a `waiter_token`, that the browse lane runs in the operator's logged-in browser, the preset, denoise and re-render warnings of the edit tools, the first-door sentences of `offload_extract` and `offload_summarize`, `wait_s` under the client's timeout, the quote and inch caveat and the prompt-writing sentence of `offload_generate_image`, the research caps, `offload_video_watch`'s cost on a long file, and how to size a contract with `section:"brief"`, among them). Thirteen more were moved by the two later checks below (the enum-value explanations and the result-reading sentences). One limit remains, and it is a keyword list: an opening sentence long enough to be cut (over 200 characters for a parameter, over 320 for a tool) is cut only at a `; ` or a spaced em dash, and its dropped tail is guarded by the trigger words alone, so a rule worded without any of them in the tail of such a sentence is dropped with no row to review. The remedy is to split the source sentence; the gate cannot find one by itself.
- **The deferral protocol of every lane.** Compact dropped the "on any failure it returns `deferred:true` and you do the work yourself" sentence of fourteen tools (`agent_run`, `offload_animate_character`, `offload_generate_video`, `offload_generate_audio`, `offload_edit_image`, `offload_media`, `offload_nim`, `offload_review_diff`, `offload_ask`, `offload_assess_image`, `offload_compose_video`, `offload_edit_image_generative`, `offload_inpaint_image`, `offload_upscale_image`), plus `offload_browse`'s and the `remote` value of the transcribe route, while the operator guide, the opencode page and the config comment said compact keeps it. None of those sentences holds a trigger word of the first gate. Twenty keep entries (one per phrasing, including the "or defers" forms of classify, extract and triage and `agent_run`'s hard-deadline and timeout sizing sentences) restore them, and `TestCompactKeepsTheDeferralProtocolOfEveryTool` compares, for every description of every tool on every surface, the count of the literal `deferred:true` and of the sentences that hold it between the full and the compact list. `defer*` joined the gate and the lead guard so a new variant turns the gate red.
- **Acceptance rules are keep-only.** The sentence that says what `acceptance_lint` flags (`PARROT-PASSABLE`, `UNGROUNDED`, `SHAPE-ONLY`: a lone `nonempty:` passes garbage) and the remedy that follows it (anchor at least one `contains:`/`regex:` check to content that appears only in the docs) are kept verbatim. The gate reports a sentence that `acceptanceRule` matches as an error if compact drops it, even with a reason, and a row for one on the dropped list is a defect.
- **`agent_run`'s remote model placement.** A `model` this box does not serve is looked up on the fleet and, with no route or route remote, the run goes as one contract through the delegator to a node whose agent seat is that model, so the goal and `setup_actions` leave the box (`read_root` and `judge` do not travel). Both the tool-level sentence and the parameter's own had been dropped as call-time validation; both are kept now.
- **Enum values.** `TestCompactExplainsEveryEnumValueTheFullListExplains` finds, for every parameter with an enum and for every parameter whose description opens by listing its values with no enum keyword (`full | lightning8 | lightning4`), each value the full list explains and requires that the compact list explains it too, unless it is on a short reviewed list. It reads the parameter's own description and the tool's, and counts `value = meaning`, `value: meaning`, `value (meaning)`, a member of an `a|b|c` list that a parenthesis explains (`birds|insects|plants (MobileNet v2 iNat)`) and a quoted value that the prose then speaks of (`Prefer "build" for ...; "general" advertises everything`); a value that only running prose explains ("route local or remote") is not read, and the quoted shape is loose the other way (it read the route value `local` in the sentence about the result field `ran_on`, which is kept). The first version read only the parameter's own description and the first three shapes and found the scheduling band `1 = urgent`, the `measurement` and `operator` pin reasons and the transcribe route's `auto` and `local` lines. Widened, it found 22 values: the `build` and `general` profiles, the classify-image domains, the compose-video `high` quality, the zero-shot spaces, the edit preset `lightning8` (which preset is the default and what the preset trades; the preset has no enum, so only the value-list reading sees it), the per-kind spec examples of `offload_generate_svg` (the only place that shows what a gauge, a comparison-bar, a chromatogram or an icon takes) and seven `offload_status` sections. Every sentence is kept except the sections. The dropped-enum list has nine entries, each with its reason: the transcribe route's `auto` and `local` (the kept tool-level route sentence names all three values) and the status sections `local`, `fleet`, `media`, `remote`, `accelerators`, `pair` and `gpu_lease` (a section names its block, and the block's contents are a result shape; the clauses that say how to read the roster are kept).
- **Result-reading sentences that stop a misreading.** Seven sentences the first classification dropped as result reading are kept whole, each because it stops a caller from misreading a result it already holds: `offload_transcribe`'s `words_reliable false` (do not cut audio at word boundaries from those times), `offload_research`'s "a PARTIAL result is a successful call" (read `summary.failed`), `agent_delegate`'s `results[].ran_on` (where a subtask ran: read it, not the word local in `results[].placement`), `defer_class "contract"` (your contract, not a box: rewrite it and retry) and the call deadline (re-issue the unfinished subtasks in a smaller call), and `offload_ask`'s two (`verified:false` is a prompt to look, not proof the answer is wrong; a repeat does not re-roll the seat). `TestCompactKeepsTheResultReadingSentencesThatStopAMisreading` finds each in the full description by a phrase and requires the whole sentence in the compact one. The `Returns {...}` result shapes are still dropped.
- **Main's research text-sources change.** Merging main added one sentence and two result field names to `offload_research`: it now reads an RSS or Atom feed and, when the operator has set `ytdlp_path`, a YouTube watch, youtu.be, shorts, live or embed URL by its subtitles. The gate named both new texts as on neither list. The text-sources sentence is KEPT, because it guides a call and bounds what is read (a feed's item links are not followed, list them yourself; a YouTube URL is read only when the operator has set `ytdlp_path`; nothing is fetched with a login or a cookie; a video with no subtitles or a yt-dlp failure is a skipped source whose `error_kind` says which, and a source with `cleanup_error` was read fine). The reworded `Returns {...}` shape stays a dropped result-shape row. The new `sub_langs` parameter and the longer `fetch_timeout_sec` description are single opening sentences, which compact keeps. No other description changed in the range, and the enum, deferral-count, result-reading and acceptance checks needed nothing.
- **Why eight surfaces, and the data-leaves-the-box words.** The first gate read two surfaces and both list the three accelerators together, so the shared-name rule gave each tool name to one device: the Coral's and the RKNPU's `offload_image_embed` descriptions ("space=efficientnet-edgetpu-s - NOT a CLIP space" and "vectors from different devices are never comparable") are advertised on a box that lists only that device and were read by no test; both are now kept, as the siglip2 pairing sentence already was. The first gate also classified a sentence only if it held a prohibition word, and the fleet-accelerator note that `registerAccelTools` appends (the call is forwarded to another node and the image bytes travel with the job) holds none: compact dropped it from all 13 forwarders of the fleet surface (18 tool and device pairs over the three devices) while the kept opening sentence still said "on the LOCAL ... NPU (free, on-box ...)". The note is now kept, one entry per device. The vision tools' `route` parameter ("the image is read on THIS box and travels with the job"), transcribe's route sentence (the default route sends the audio to a fleet node when this box's whisper is held), browse's typed-decision sentence (the visible page text goes to a decision endpoint that may front a cloud model) and `offload_compose_video`'s route sentence (the render, and an inline composition or a project directory as a project bundle, goes to a fleet node when this machine has no composition lane) are kept for the same reason.
- **Measured (31 tools, 0.186.0 with the review lane's SHA-bound evidence and main's research text-sources change, plus this change; llama.cpp b11146 `llama-tokenize`, vocab-only, over the Qwen3.8-27B GGUF).** Chat-template rendering (`<tools>`, one `{"type": "function", "function": {...}}` per line): every advertised tool 31,582 full and 20,830 compact (10,752 saved, 34.0%); the twelve `recon` tools 12,068 and 8,302 (31.2%); the other nineteen 19,521 and 12,535 (35.8%); the four Tier-1 tools 1,232 and 993 (19.4%). On the largest surface, the same 31 plus the browse lane and the three accelerators (45 tools, measured, not estimated): 34,577 and 23,400 (32.3%). The `tools/list` JSON the server writes: 29,468 and 18,718 (31 tools), 11,353 and 7,588 (`recon`); with compact separators the 31-tool template lists count 29,652 and 18,902. With the three accelerators as fleet accelerators (45 tools, 13 forwarders) the lists measure 35,282 full and 24,107 compact. Against the figures before main's research change was merged in (31,381 and 20,634 for the 31 tools, 11,867 and 8,106 for `recon`, 34,376 and 23,204 for the 45 tools, 35,081 and 23,911 for the fleet variant, all reproduced to the token with the same tokenizer first), the full columns grew by 201 tokens and the compact columns by 196, all in `offload_research` (1,163 to 1,364 in full, 754 to 950 compact): 131 for the text-sources sentence, 65 for `sub_langs` and the longer `fetch_timeout_sec` text, and 5 for the two new field names in the full list only. No other tool moved. Before the previous round (the enum check widened, seven result-reading sentences kept) the compact columns were, in the template rendering, 19,873 (31 tools), 7,662 (`recon`), 12,218 (the other nineteen), 993 (Tier-1), 22,372 (45 tools) and 23,080 (the fleet variant), and as `tools/list` JSON 17,766 (31 tools) and 6,953 (`recon`); compact grew by 761 tokens (3.8%) on the 31 tools: 310 for the enum-value explanations (`offload_generate_svg` +98, `offload_edit_image_generative` +87, `agent_delegate` +82 for the `ran_on` sentence, `offload_compose_video` +23, `agent_run` +20; the classify-image domains and the zero-shot spaces add 71 more on the 45-tool surface) and 451 for the six other result-reading sentences (`agent_delegate` +121, `offload_ask` +116, `offload_transcribe` +109, `offload_research` +105). Before the review of every sentence the compact columns were 18,007, 6,682, 11,332, 847, 20,371 and 21,079, reproduced to the token with the same tokenizer; compact grew by 1,866 tokens (10.4%) on the 31 tools: 529 for the deferral protocol, 856 for the twenty sentences the review moved to the keep-list, 481 for the rest (the three `offload_status` clauses: 46; the remote model placement; the urgent band and two pin reasons; the acceptance sentences). The largest per-tool growth is `agent_run` +381 and `agent_delegate` +267; `offload_status` is +90 (44 for the `section:"brief"` sentence the review kept, 46 for the three clauses).
- **Against 0.137.0, like for like where possible.** The same twelve `recon` tools: 8,265 then, 12,068 now in full, 8,302 compact (37 above the old full figure; 159 below it before main's research change). All tools is not the same set (35 then, 31 now, because the set is per-box): 596 tokens per tool then, 1,019 per tool in full now, 672 compact; at 45 tools compact is 23,400, 520 per tool, which is above the 20,868 of the old 35-tool full list. Per tool, where 0.137.0 recorded a figure (then in full; now in full and compact): `agent_delegate` 2,903; 4,173 and 3,154. `offload_status` 660; 1,074 and 328. `offload_compose_video` 991; 1,153 and 886. The four Tier-1 tools 847; 1,232 and 993. No other per-tool figure was recorded, so the rest cannot be compared one by one. The table, the method and the re-measure command are in `docs/systems/opencode-integration.md` (Tool schema size).
- **Also.** The sentence splitter no longer cuts at an ellipsis: it cut the `waiter_token` collect sentence in two ("... only to collect the result" was dropped and the kept half ended mid-sentence). The independent structure comparison in the tests no longer strips a parameter that is called `title` or `example`, and the full-surface guard names the browse lane and one tool of each accelerator instead of a bare count.
- **Tests.** The gate above and its companions: `TestGateFiresOnANewConstraintSentence`, `TestGateReportsEveryUnclassifiedProbeInALiveDescription` (the five probes), `TestDroppedRowsNeedAReasonClassAndARuleWordsNeedAnExplicitReason`, `TestKeepListEntriesAreLive` and `TestDroppedEntriesAreLive` (every entry matches a live sentence outside an opening position), `TestKeepAndDroppedListsAreDisjoint`, `TestKeepListEntriesSurviveWhereverTheyAppear`, `TestLeadCutNeverDropsAConstraintWord`, `TestLeadGuardCoversTheGate`; `TestCompactKeepsTheDeferralProtocolOfEveryTool`, `TestStatusGuidanceIsItsOwnSentencesAndSurvivesCompact`, `TestCompactKeepsAgentRunsRemoteModelPlacement`, `TestAcceptanceRuleSentencesAreKeepOnly`, `TestCompactExplainsEveryEnumValueTheFullListExplains` with `TestDroppedEnumValuesAreLiveAndReasoned`, `TestEnumValueExplainedReadsTheShapeOfAnExplanation`, `TestEnumCheckReadsTheToolLevelDescriptionToo` and `TestEnumNodesReadAValueListThatHasNoEnumKeyword`; `TestCompactKeepsTheResultReadingSentencesThatStopAMisreading`; `TestCompactKeepsNamedSafetySentences` and `TestCompactKeepsNamedParameterSentences` (each pin looked up in the full description first, so a rewording fails loudly), `TestCompactKeepsTheReferentOfAKeptSentence`, `TestCompactKeepsEveryToolAndParameter` (same tools in the same order; each input schema equals the full one minus prose, compared by a naive independent strip), `TestCompactRequiredParametersSurvive`, `TestDefaultProfileIsFull`, `TestCompactDoesNotTouchTheRegistry`, `TestCompactProfileLeavesCallsAlone` (a bad call is answered identically under both profiles), the sentence splitter and cut tables, a property named `description`, `title` or `example` that must not be mistaken for the keyword, an unparseable schema that must come back unchanged, and the config tests for the default, the closed set and the refusal by name (with the message pinned). For the eight surfaces and the data-leaves-the-box words: `TestEveryDeviceTableIsReadByTheGate`, `TestGateDevicesCoverEveryAcceleratorTable`, `TestCompactKeepsEveryToolAndParameterOnEverySurface`, `TestCompactKeepsTheFleetForwardingSentence`, `TestCompactKeepsTheSharedNameSpaceWarnings`, `TestGateFiresOnAnAppendedSuffix`. Every behaviour above went in red first (the failing test committed before the change that turns it green).
- **Mutations.** Each new test was run against a one-line mutant, each restored byte for byte (`git diff --quiet` exit 0) and each red: `defer*` removed from the lead guard and from the gate, one deferral keep entry removed (the `offload_media` one and the transcribe route value); the gate reverted to read only sentences with a trigger word, the widened words removed from the gate, `ensure*` removed from the lead guard, one dropped row removed, a rule-word row without its reason, a row that restates its class, a row with an undeclared class, and each of the five probes inserted into the source of `offload_summarize`; the status split reverted and two of its keep entries removed; each of the two `agent_run` model entries removed; the `1 = urgent` and operator entries removed, a dropped-enum entry removed, and the explanation matcher narrowed to `value =` only; the acceptance case removed from the gate, from the dropped-row check, and the lint entry removed. Earlier, against the keep-list mechanism: removing a keep or a dropped entry, adding a sentence with `never`, dropping `never` or `only` from the lead guard, removing a referent, removing the ellipsis rule from the splitter, making the cut ignore the keep-set, and indexing the keep-set under the wrong tool name; and against the eight surfaces: the Coral's fleet entry, the forwarding note's wording, `travel*` and `cloud` in the lead guard, a device missing from the gate, the RKNPU's space warning and the browse sentence unlisted, and the data-leaves-the-box words removed from the gate.

### Fixed - the fleet-accelerator note of `offload_text_embed` named an argument the tool does not have

- On the fleet surface `offload_text_embed`'s forwarding note said `image_path is read HERE and its bytes travel with the job`, but the tool takes `text` (its required argument) and reads no image. The note is now built from the table row's own required argument (`fleetAccelNote`): an image tool keeps the sentence it had byte for byte, `offload_text_embed` says `text travels with the job`. `TestFleetAcceleratorNoteNamesTheToolsOwnArgument` pins it over all 18 forwarders (the row's required argument appears in the note, and no `*_path` argument the tool does not take is named). Compact keeps the new sentence under its own keep entry. Taken alone, this fix leaves the default `tools/list` as main has it (SHA-256 `8505e9ca79f68602...`), the 31- and 45-tool lists too; only the fleet-accelerator variant changes (13 notes: 705 tokens, 13 fewer: 35,295 on main, 35,282 here). The `offload_status` split described above is what moves the default list to `8eb5f938438ad426...`.

### Added - `trace-corpus`: a read-only readout over recorded agent runs, with the pager report and compaction pass count now recorded

- **What it does.** `local-offload trace-corpus <harvest|readout|freeze|check>` (new root file `trace_corpus.go`,
  one `case` line in `main.go`) is shaped like `compaction-eval`. `harvest` reads the standalone agent trace files and the
  delegation log (never the ledger, so `ledger_readers_lint_test.go` does not apply), reduces each run to numbers and enums,
  redacts the strings that remain with the redactor and vet table the compaction harvest uses, and writes a hash-stamped,
  machine-local corpus. `readout`, `freeze` and `check` are deterministic and carry no timestamps; `check` demands the frozen
  report back byte for byte and refuses a baseline from another corpus. The readout gives the trace-bearing run count, the
  pager re-fetch rate pooled over the runs that evicted something against the 10 % gate of R2-13 (`agent.PagerGate`, now
  exported so the readout and the instrument judge by one number), the compaction fire rate and passes per run and per step,
  and prefill tokens with KV reuse (with the per-run spread, nearest-rank p50, p90 and max, of prefilled tokens over the
  measured runs and of the mean prompt size per step), for the whole corpus, by source and by seat.
- **Only runs that executed.** A deferred subtask (capacity, infrastructure, budget, config, contract) is logged with zero
  steps and nothing else. Harvest skips such a row under the named reason `delegation row never ran (zero steps, nothing
  recorded)`; any step, step trace, prefill or seat-token accounting, or pager object keeps a row a run. The first real
  readout had counted rows that never ran an agent as runs: 24 % of the runs it counted, a denominator about a third
  larger than the true one, which inflated every unrecorded count and would have kept pager.unrecorded_runs above zero
  after the recording shipped. That those rows never reached the loop is inferred, not observed: before the recording, a
  deferral raised before the loop ran and a run the loop returned from in zero steps with nothing else wrote the same row,
  so the skipped count is an upper bound on pre-loop deferrals. A deferred row with zero steps and only a setup-action trace stays a run: the setup actions
  execute inside the agent loop before its first model turn, a budget or infrastructure deferral takes the loop's pager
  report, and the compaction ladder can run before the first model call, so such a run is not unrecorded once the recording
  ships. From a build with the recording, every run that reached the loop carries a pager object, a write-class deferral
  (the write door did not close cleanly) included, so its unrecorded count reaches zero.
- **Only trace-shaped files.** A file in the traces directory is a run only if it holds a transcript and either steps or a
  stop reason. Any other JSON object there (a checkpoint, a queue file) is skipped under the named reason `trace record has
  no transcript, or neither steps nor a stop reason` and counted, apart from files that are not JSON objects at all.
- **Seat attribution is explicit.** A pool placement logs the pool's alias as the seat, so those runs cannot be attributed to
  a seat. `by_seat` holds the runs a seat can be named for (each seat the record names, and under the key `(unrecorded)`
  the runs whose record names no seat, every agent trace) and not the alias runs; alias runs are the `pool_alias` bucket
  and `pool_alias_by_node` (keyed by the node id the row carries, `(unrecorded)` when none), and the new `attribution`
  block gives `seat_named_runs`, `pool_alias_runs`, `seat_unrecorded_runs` and `unattributed_share`. The corpus gains an
  optional `node` field (an older corpus without it still loads).
- **Paths are redacted.** The shared vet table has no path class, so a seat, node id or stop reason that is a local path (a
  drive path, a rooted path, anything with a backslash) is replaced at harvest with a stable `[redacted-path-N]`
  placeholder (counted under `redactions.path`), and the strict loader refuses a corpus that still carries one.
- **Why.** The gate that closes or opens the whole pager family, and the rate at which the ladder fires, could not be read from
  anything on disk. The loop measured both and the trace file and the wire result dropped them, so the family sat at
  `insufficient_data` and the compaction items had no baseline. The fields below make future runs record them; the verb reads
  them back.
- **Unrecorded is never zero.** Every rate is a pointer that serializes as `null` when nothing was recorded, with the count of
  unrecorded runs beside it. A corpus of rows written before this change reads `NO_DATA` on the gate and null on the fire
  rate; a run that recorded a pass count of 0 is a real 0. A pager object that contradicts itself is read as unrecorded and
  noted. A run that observed steps but reported zero prefilled and zero cached tokens (an engine with no prefill timings) is
  counted apart as `no_token_counts_runs` and adds nothing to the token sums, so it cannot read as a measured zero.
- **Recording (additive, `omitempty`).** `PagerReport` gains `compaction_passes` (the number of ladder passes in the run: a
  proactive pass, the harder pass after a context-overflow rejection and the emergency shrink each count one). The standalone
  per-goal trace gains `pager` and `prefill` objects, and `AgentWireResult` gains `pager` (`core.AgentPager`, same key names as
  the loop report), which the delegation log stores. Present means measured, zeros included. Absent means a record from
  before the field, from a node that predates it or from a delegator that predates it (the delegator that writes the log
  decodes the node's answer into its own wire result and encodes it again, so both ends must carry the field;
  `delegator_version` on the row and `harness_version` on the result name the builds; the node stamps `harness_version`
  only after a loop that returned without an error and a write door that closed cleanly, so a pre-loop deferral, a
  loop-error deferral, a lost-workspace-root deferral and a write-class deferral lack it even where they carry the pager
  object), or a run that never reached the loop. The object is attached right after the loop returns, ahead of the write
  door, so a write-class deferral (the write door did not close cleanly) keeps it. `internal/compeval` exports its
  redactor (`NewRedactor`, `PIIClasses`) in a new file so a second harvester cannot redact a different set of classes.
- **Fixed on the way.** One loop drains a whole queue and its pager and prefill instruments accumulate for the loop's life, so
  the stderr line and the new trace fields of goal N were the running total of goals 1 to N, and a payload evicted in an
  earlier goal could count as re-fetched in a later one. `Loop.ResetRunInstruments` (serial reuse only; a loop shared by
  concurrent runs keeps one set of accumulators by design) starts them over before each goal.
- **Tests.** Red first for the counter, the trace fields, the wire field, the per-goal reset, the never-ran skip, the path
  redaction (seat, stop reason and node id), the pool-alias bucket, the per-run spread, the write-class deferral's pager
  object and the trace shape check (each committed failing, then fixed). `trace_corpus_test.go` runs
  harvest, freeze and check over synthetic fixtures in `testdata/tracecorpus` and requires byte-identical corpus and report
  across two harvests and across CRLF endings, pins every derived number against hand-worked values, and covers redaction
  (literals built at run time, and the fixture check builds its banned words at run time too), the unrecorded-never-zero rule,
  the gate boundary (the instrument's `rate < gate` rule), the strict loader, the seat and day filters and the `main.go`
  registration. The pager-object validator is pinned at every guard (null and absent pass count, unknown and empty basis,
  each negative count, insufficient data with evictions or distinct payloads, refetched above evicted), an unusable object is
  one note and one unrecorded run from a delegation row and from a trace, only tool turns count as ladder artifacts, and
  `agentPagerWire` keeps an unrecorded pass count absent on the wire and maps every field when it is recorded. A write-class
  deferral is pinned end to end as a run with a step trace and a pager object of real zeros (attaching the object after the
  write door again fails it), and a traces-directory file that is valid JSON of another kind is pinned as skipped under the
  named reason.
- **Mutations run and killed.** An unrecorded rate returned as 0, the gate comparison widened to `<=`, redaction removed
  (twice: with and without the exit gate), the check made to always pass, a self-contradicting pager object trusted, a zero
  pass count read as a fire, a token-less prefill read as measured; each term of the never-ran predicate removed in turn;
  each alternative of the path pattern removed, the path redaction removed for the seat, for the stop reason and for the node id, the loader's
  path refusal weakened to one field; the pool-alias pattern loosened three ways, the node-less bucket key changed, the
  unattributed share dropped to aliases only, the node field dropped or left unredacted; a nil pass count read as a recorded
  zero, the negative-count terms and each of the other guards removed, the tool-role filter dropped; the wire mapping
  returning an object of zeros for an absent count, and each field of it swapped; the pager object set after the write door
  again; the trace shape check's transcript term, its steps-or-stop-reason term, each of the two alternatives inside it, and
  its OR made an AND; the quantile rank rounded the wrong way, the
  p90 taken as the p50, the maximum taken one short, the sort removed, a zero-step run divided into the prompt size, the empty
  set not reported as such. One mutant is equivalent and survives by construction: removing only the `distinct_evicted < 0`
  term, because a negative distinct count is always also caught by the refetched checks beside it.

### Fixed - a lease that unloaded the seat no longer holds its cards idle while the host cannot meet its RAM: the wait after the unload is bounded, a detached hold releases itself even if its reserve dies early, and a class-default figure is warned about

- **What it did.** On 2026-10-14, on the reference box, `gpu reserve --wait 8h --drain --unload-seat --class media --for 40m -- <a command that
  is not a recognised render helper>` with no `--ram` declared the media class default (46.8 GiB, the largest render family). F82 takes the
  cards, drains, unloads the seat and only then asks the host, still holding the cards, for the whole `--wait`. The seat was not even
  loaded (the unload freed nothing), committed RAM was 84.6 of 127.7 GiB with 8 GiB of headroom, so the figure could not be met, and the
  lease held all three cards idle for about 50 minutes while a production lease queued behind it, renewing its window with no work running.
- **The bound.** F82's order is kept (the drain and the unload first: it breaks a real deadlock); only the hold after it is bounded.
  New config key `gpu_hostram_hold_max_sec` (default 600, chosen and not measured; `0` = the old wait inside `--wait`; a negative value or
  one above 7 days is refused by name, the accessor clamps before it converts to a `Duration` so a huge value can no longer wrap into "no
  bound", and the reserve says once that it clamped) and `gpu reserve --ram-hold-max <duration>`, which overrides it for one reserve (`0` is
  a value). The bound is counted from the moment the unload is done and is not raised to the drain floor. When it passes without the host's
  admission the lease is released (the seat warmed back as for any release), nothing runs, nothing re-queues, and the reserve exits non-zero
  with one line that names the need, the host's committed / physical / headroom figures, how long the cards were held, and what to do (pass
  a smaller `--ram` when the figure is the class default, or wait for the host). The text says only what is true when it is built ("the
  lease is being released"); a release that then fails prints `LEASE NOT RELEASED (epoch N): ...; run gpu release [--warm-seat] --epoch N`
  and the error the command returns ends with the failure (`the lease is STILL HELD`). `--warm-seat` is advised only when a warm-back is
  owed, because the manual release has no guard against loading a seat nothing unloaded.
- **A detached hold cannot outlive its reserve.** The bound used to be enforced only by the foreground reserve: if it died the hidden holder
  kept the cards for good. The record now carries a moment by which the lease is released: an OUTER bound, now + the hold bound + 60 s,
  stamped as soon as the holder has the lease and re-stamped every 30 s by the LIVE reserve while it drains and unloads (the drain's own
  deadline is `--wait` away, 8 h by default, which is no bound), so a reserve killed during the drain or the unload, however long `--wait`
  is, is covered within about the bound; replaced by the wait's own end (the earlier of the bound and `--wait`) when the wait begins, and
  taken off only when the host admits the figure (a wait that ends any other way leaves it, so a reserve that dies the instant its wait ends
  is covered too). The same re-stamp closes the too-EARLY direction: an unload is not bounded by the drain's deadline (the maintenance
  client allows 15 minutes per request), and a long unload used to make the holder release a live reserve's lease mid-unload, leaving the
  seat owed and cold; a live reserve now loses its lease to its holder only if it cannot re-stamp for more than the bound + 60 s + 30 s. A failed drain's lease, which the detach form keeps held, is released by the holder at the last stamped moment unless the
  operator frees it sooner, and the error says when. With the hold bound at 0 the outer stamp is the `--wait` deadline (moved ahead of a live
  reserve whenever it would fall behind), so a dead parent's lease ends at `--wait` instead of being held until `gpu release`; the stamp
  carries its phase (`host_ram_hold_phase`: `outer`, `wait`, `release`; none = an older writer's, the wait's), and `gpu status` and the lease
  row say it, so the drain and the unload no longer read as "released at HH:MM if the host still lacks the room" when the host has not been
  asked and a live reserve never releases then. The hidden `gpu hold` reads its record
  on every poll and releases its lease 30 s after that moment if the lease still asks for the host (instances stopped, the seat warmed back
  only if the lease owes it); a live reserve pushes the moment out before its own minutes-long release so the two never race or double the
  warm-back, and a reserve that dies in the middle of that release is still covered. The detached wait **fails closed** on its stamp: tried
  four times before the host is looked at, and if it never lands the wait is not run and the lease is released (an outer bound already on the
  record stays). A detached wait that `--wait` ends first used to keep the lease, idle and non-exclusive, indefinitely: it releases now, as
  the wrapper form always did. The hidden `gpu hold` verb has no wait of its own and takes no flag.
- **The holder's own failure paths.** A fence read that fails is no longer taken for a lost lease: `Lease.Check` tells a record it could not
  READ from a verdict (gone, fenced out), the holder confirms a failed fence with further looks, retries a failed read for 30 s with one
  line, and says what it let go on. Its own release is retried four times; when it gives up it prints `LEASE NOT RELEASED`, marks the record
  (`holder_gave_up_ms`, additive), and exits non-zero instead of exiting 0 into a log in the temp directory, and `gpu status` says that
  nothing polls the lease any more, also once the holder has exited and the record reads as a stale holder (`gpu status` printed "free"
  and the stale-holder line with no word of the marker).
- **A wrapper no longer kills its command over a record it cannot read for a moment.** On Windows under load a heartbeat that raced an atomic replace
  of the lease record read the lease as gone: the wrapper's tick printed `LEASE LOST` and killed the wrapped command, the drain's
  heartbeat cancelled the drain, and the drain loop released the lease and queued again. One shared helper (`confirmFence`, the hidden
  holder's, now used by the wrapper's tick, the drain heartbeat and the drain loop) looks again, tells a verdict from a failed read, and
  waits out a window; within the window nothing is killed, released or requeued. A heartbeat write that fails while the lease is still ours
  no longer kills the command either (a persistent heartbeat-write failure with a readable record is tolerated, and the lease then goes
  stale and is reclaimed by the ordinary rule).
- **An unreadable record is held while its keeper is alive, and the keeper gives up first.** The acquirer side and the keeper side used to
  disagree about a record that exists and cannot be read for an I/O reason (not corrupt bytes): the sweep removed it, with its claims,
  once it was ten seconds old, and the whole-node path treated an unreadable `meta.json` as debris, while the wrapper in its retry window
  kept running its command and wrote no heartbeat, so the next reserve got the cards with about 25 s of overlap. Now the sweep, the
  whole-node and card grants and `verifyNoDeviceLeases` leave such a record and its claims alone while its holder's heartbeat (or the
  record's own write) is within the heartbeat TTL (120 s), and take it only once that liveness is gone; and the wrapper, the drain
  heartbeat, the drain loop and the hidden holder FAIL CLOSED once their window passes with the record still unreadable (the wrapper stops
  its command, the drain ends, the holder lets go), each with one line saying why, each within a silence shorter than that TTL (pinned by
  a test), so a keeper has stopped before the sweep can hand its cards out.
- **A record that exists but cannot be read is a held lease.** `Inspect()` and `Leases()` judge who holds the cards and skip a record they
  cannot read, and the release paths believed them (reproduced on Windows by replacing the record with a directory). `Manager.RecordState`
  looks at the record itself (absent, held, unreadable; unreadable is held) and now decides every "the lease is gone": the release
  confirmations, the holder's own release (it can no longer return nil and exit 0 over a lease it could not touch), `gpu release`'s
  post-check, and the whole-node `Lease.Release`, which returns a wrapped `*RecordUnreadableError` instead of nil. Bare `gpu release`
  with no `--epoch` on a card-scoped host names an unreadable card record (`epoch N: its record cannot be read; run again or pass --epoch
  N`) instead of printing "no lease is held" and exiting 0. `Restamp` keeps the cause (`%w`) and says "cannot be read" rather than "the
  lease is gone"; the host-RAM wait looks again three times, 100 ms apart, before it believes a missing lease is gone; the detached
  reserve's release proceeds over an unreadable record (stop the instances, warm back, release, report). The holder's release keeps its
  error while a claim of the epoch remains (a card lease removes its record before its claims, so "the record is absent" alone is not
  "released"), and the stop line over a record it cannot read says so instead of "the lease is no longer ours". A release's answer "no lease was held" is believed only if the epoch is off the record: the detached
  reserve, the wrapper's final release, the holder and `gpu release` no longer report a held lease as released or as "no lease is held",
  and `ReleaseByEpoch` answers "nothing held" only for an absent record. The holder's grace before it steps in is 30 s, so a reserve stalled
  under paging is not given a parallel warm-back.
- **Visibility.** The waiting line says when the unload freed nothing on the lease's cards (the seat was not loaded) and names the bound; the
  bound still applies, a cold seat is not failed at once. A failed unload of another resident is counted and said ("N unloads failed"), never
  read as "freed nothing". A stamp that cannot be written is reported once. The class-default line is now a `WARNING` that the declared
  figure may hold the cards while the host cannot meet it, naming `--ram <GiB>` (the figure itself is unchanged). `gpu status` says how long
  a waiting lease has held its cards and when it is released if the host still lacks the room; past that moment it says "the holder, if one
  is running, releases the lease at its next poll; `gpu release --epoch N` frees it now" for a detached lease (`host_ram_hold_until_ms` on
  the record, additive; `host_ram_hold_until`, `host_ram_hold_phase` and `holder_gave_up` on the lease row; `holder_gave_up_at` is
  `omitzero` in the status JSON, since `omitempty` never omits a struct and the published verdict schema had changed). `Restamp` and
  `restampLocked` choose between a card record and a whole-node one through the same stat as every other check, so a stat that fails for a
  reason other than absence goes to the card path and says "cannot be read", never "the lease is gone". The ComfyUI instances kept under a lease are stopped
  only by a caller that fenced the epoch first, and a source test now pins each caller by the function that holds the call across the whole
  package.
- **Open, stated.** A reserve that dies in the few milliseconds between the holder taking the lease and the first outer stamp leaves no
  stamp; with the hold bound at 0 the backstop is `--wait` itself (the operator's choice), so a dead parent's lease is held until then;
  the reader still reads an unreadable record as absent, so `gpu status` shows the card free while a reserve is refused for it, and a
  keeper that dies leaves its unreadable record held for up to the heartbeat TTL after its last sign of life; the windows (10 s, 30 s)
  and the TTL are chosen figures, not measurements; `confirmFence` has no cancel channel (a window runs to its end when the process is told
  to stop) and `readCardClaim` does not map an I/O error from a claim read; a holder cannot write its give-up marker onto a
  record it cannot read, so over such a record the exit status and its log are the only word; the reserve measures its wait on a monotonic
  clock and the holder compares wall-clock milliseconds, so after a suspend the holder can release sooner (never later) than the stamp
  says; the warm-back's guard still abandons a warm on a failed `Check` (it kills and releases nothing; the seat stays owed); the
  hidden-holder process itself is exercised in-process, not as a spawned child.
- **Docs.** `docs/systems/gpu-lease.md` (the F82 section: the order, the hold bound, the outer bound, "Releasing on its own", each form, the
  third consequence) and the OPERATOR-GUIDE config row (in the guide's one key table; it has no GPU-key table) and host-RAM paragraph. F82 is
  recorded in no ADR (ADR 0009 and ADR 0080 were checked: their host-RAM and unload passages are about other mechanisms), so there is no
  ADR amendment.
- **Tests, red first.** Every commit pair is a failing-test commit followed by its fix: the bound (a fake host whose RAM never fits and a
  reserve with `--unload-seat` that must release the cards, warm the seat back, run nothing and exit non-zero inside a 200 ms bound, wrapper
  and detached, through the flag and the config key; red ran 251 s with every bound test failing because the reserve held the cards for the
  whole wait, 123 s for the one that keeps the production 2 minute floor; green 22 s), the ceiling, failed unloads, stamp errors, the
  detached round (the holder releasing its own lease after the stamp passes, the reserve that dies mid-release or the instant its wait ends,
  `--wait`-ended detached waits, a cold seat never warmed, a failed release reported, `gpu status` after the stamp passes, the fence
  confirmation), and the third round (the outer bound, the fail-closed stamp, the holder's failure paths, the confirmed release, the
  production grace, the clamp line), and the fourth (the unreadable-record round: `RecordState`, bare `gpu release`, the wait and restamp,
  the stale-holder line, the live outer bound, and an instance-stop scan over whole declarations), and the fifth (the status JSON
  schema, the restamp dispatch, the hold stamp's phase, the unreadable-record rule with a real wrapper, the `--wait` backstop at bound 0).
  The two tests that pit the holder
  against a live parent hold the holder's clock back and hold the reserve AT its push-out (a hook, not a poll of the record), and the
  holder reads its clock before its record, so no scheduler delay can decide them.
- **Mutations** (each compiled, was proved applied by a changed sha256, turned a named test red, and was restored byte-for-byte): bound
  ignored; bound error swallowed so the command runs; lease not released (wrapper, detached); seat not warmed back (detached); warning
  dropped; config 0 read as the default; `--ram-hold-max 0` ignored; unload always counted as freeing something; bound never stamped; stamp
  cleared as a wait ends (whole, and only when the end comes before its moment); status never naming the bound; the holder ignoring a passed
  stamp; the holder acting on an admitted lease; a `--wait`-ended detached wait keeping its lease; the automatic release warming a seat it
  does not owe (parent and holder); a failed release not reported (wrapper, detached); the failure claiming the release; no clamp, no ceiling,
  no flag ceiling; no push-out before the parent's release; the stamp not written at the start of the wait or only for the bound; a failed
  stamp said at every look or not reported; a failed unload not reported; the holder letting go on one failed fence read; and, for the third
  round: no outer stamp, an outer stamp without the hold bound, an outer stamp with the bound off, the holder exiting 0 on a failed release,
  not retrying it, leaving no marker, status promising a dead poll, a failed read taken for a verdict (card and whole-node leases), no retry
  of a failed read, the holder letting go without a word, the released bool ignored, `gpu release` claiming no lease is held, a record that
  cannot be examined read as nothing held, the detached wait not failing closed, a required stamp tried once, the returned error not ending
  with the failed release, `--warm-seat` always advised, a stamp landing on an admitted lease, the production grace back to 5 s, the clamp
  silent, and a stray direct stop of the ComfyUI instances in another function, in a file of its own, and through a lease object in an
  unlisted function.
  The fourth round ran no mutation battery, by order: its proofs are the red-then-green pairs, the reviewer's slow-unload probe (red on the
  tree before the live re-stamp), one stray `stopKeptInstances` in a package-level initializer turning the instance-stop scan red, and a
  starvation tally for the push-out race.

### Fixed - The operator guide's engine + lease row says what the code does: a generation route's lease window is its own timeout (register C-33)

- **What the row said.** "A 5,400 s video run outlives the default lease; size the lease `--for` window to the job", with
  the 3,600 s lease TTL set "against" the media timeouts, one deployed machine's figures (`imagegen_timeout_sec` 600,
  `videogen_timeout_sec` 5,400, `gpu_wait_ms` 600,000) quoted as if they were the defaults, and `gpu_wait_ms` read as a
  lease length. The doctor paragraph called the 600,000 ms "the deployed" value. The first rewrite fixed that but was
  still wider than the code in three ways, which review caught: it said every "media call" leases for its own timeout
  (two lanes take no media lease), it named two of the seven timeout keys, and it quoted only the code defaults, where a
  seeded install runs much longer ones.
- **What the code does** (read in `config.go`, `pipeline.go`, `igpumedia.go`, `ttsendpoint.go`, `mediaadmit.go` and
  `gpulease.go`, not taken from the row). Every generation route that takes the media lease (image, sdcpp image, edit,
  inpaint, upscale, batch, run-graph, video, animate, audio, and the iGPU video, animate and audiocpp lanes) acquires it
  for the call's own timeout (`timeout` is passed to `acquireMediaLease` as the TTL, on the card-scoped writer and on the
  whole-node one) and the render is killed at that same value (`gpugen.Spec.Timeout`), so a render cannot outlive the lease
  the pipeline took for it. Each route reads its own key: `imagegen_timeout_sec` (image, sdcpp image, batch, run-graph),
  `videogen_timeout_sec`, `inpaint_timeout_sec`, `upscale_timeout_sec`, `gen_edit_timeout_sec`, `animategen_timeout_sec`,
  `audiogen_timeout_sec`. A batch takes the per-image timeout times its jobs. The two exceptions take no media lease: the
  `tts_endpoint` voice lane (the speech server owns its card) and a `pipeline-job` (only the in-process media slot; the
  stages it runs lease for themselves). The lease library's `DefaultTTL` (3,600 s) is only the fallback for a lease
  acquired with no window, which no generation route does. `gpu_wait_ms` (code default 90,000) is the queue wait, not a
  lease length. The code defaults are `imagegen_timeout_sec` 720 and `videogen_timeout_sec` 1,500. The one place a window
  can be shorter than the job is a `gpu reserve` wrapper: a call run inside it inherits the wrapper's lease and its
  `--for` window (default 45 minutes), and a window that ends first is renewed or labeled expired, never a release.
- **What a seeded install runs.** The tier seeds in `setup/templates/profiles.json` are not the code defaults:
  `imagegen_timeout_sec` is 3,600 on nine seeds (blackwell-16, blackwell-32, blackwell-2x16, blackwell-3x16,
  blackwell-48, blackwell-72, ampere-16, volta-16 and the high-RAM arm of ampere-8), 2,400 on blackwell-8 and 900 on
  ampere-6 and the AMD tiers; `videogen_timeout_sec` is 5,400 on those tiers except ampere-8 (which seeds no video), 4,500 on
  blackwell-8 and 7,200 on amd-gcn. Because the window equals the timeout, the seeded value is how long a wedged render holds the
  machine-wide media lease: an hour for an image on those nine seeds. The repo records no decision that makes that a lease
  length. The 3,600 s and 5,400 s seeds first appear in v0.17.0's quality-first seed, whose changelog entry is about the render
  poll ceiling (`COMFY_WAIT_SEC`) not killing legitimate quality renders, not about a lease length; and the frozen
  roadmap's T6 called the same 3,600 a defect when it found it deployed on one machine (a 60-minute machine-wide lease for a job that finishes in one to two
  minutes; cut to 600 there). Whether to resize a seed is a measurement decision, owed separately, between how long a
  render legitimately runs and how long a wedge may hold the lease; this change does not make it. The guide row now
  quotes the seeded maxima (read from the profile file by a test), so the number a reader sizes from is the seeded one.
- **What changed.** The row and the doctor sentence now say all of that, in code defaults and seeded maxima, with the
  `gpu reserve` wrapper window named as the one place a window can be shorter than the job. Nothing in the code
  changed, and the tier seeds in `setup/templates/profiles.json` are untouched. `docs/tiers/*.md` were read for the same
  claim and carry none.
- **Tests.** `TestOperatorGuideLeaseRowStatesTheMediaLeaseIsTheTimeout`, `TestOperatorGuideQuotesCodeDefaultsForMediaTimeouts`,
  `TestOperatorGuideLeaseRowNamesEveryRoutesTimeoutKey`, `TestOperatorGuideLeaseRowNamesTheLanesThatTakeNoLease` and
  `TestOperatorGuideLeaseRowQuotesTheSeededTimeouts` (internal/pipeline) were red on the earlier row and are green here:
  the row must exist (blind-gate control), refuse the retired and over-wide claims, say what the code does, name every
  route's timeout key (the table in `mediaadmit_lease_routes_test.go`, held to `gpuLeaseCases` and to real config keys),
  name the two lanes that take no lease, quote defaults that equal `config.Default()` and `gpulease.DefaultTTL` (the
  fallback in its own sentence), and quote seeded maxima read from the profile file. The window and the kill are guards,
  green on the code as it stands. `TestAVideoCallsLeaseWindowIsItsOwnTimeout` and `TestAnImageCallsLeaseWindowIsItsOwnTimeout`
  pin the lease half for ComfyUI image and video on both lease paths.
  `TestEveryMediaRouteLeasesForItsOwnTimeoutAndIsKilledThere` pins both halves on all 17 GPU tasks: the route's own key is
  set to 2 s and every other timeout key to 90 s, a stub runner holds the render, the one live lease's declared window must
  be 2 s and the render must be killed at "timeout after 2s" (gpugen names the Timeout it ran under).
  `TestAnImageBatchLeasesAndIsKilledAtThePerImageTimeoutTimesItsJobs` holds a three-job batch at 1 s per image to a 3 s
  window and a 3 s kill. (`COMFY_WAIT_SEC` is the render script's poll budget, not what kills, and the guide does not
  claim it; the kill is pinned where it happens.)
- **Mutations, each restored byte for byte (sha256 compared) and each turning exactly the matching tests red.** Lease and
  kill: video `Spec.Timeout` plus ten minutes; audio lease TTL to zero; run-graph reading the upscale key; batch
  multiplier removed; the iGPU lease TTL set to an hour and the iGPU `Spec.Timeout` doubled; the ComfyUI image
  `Spec.Timeout` doubled; the batch `Spec.Timeout` doubled; the sdcpp image lease TTL to zero and its `Spec.Timeout`
  doubled; edit reading the inpaint key. Earlier rounds: image TTL to zero, video TTL to zero, a one-hour clamp in
  `acquireMediaLease`, the card-scoped TTL to zero and the whole-node TTL to zero. Row: each of the three code defaults
  replaced by a deployed figure, the 3,600 s fallback sentence reworded, each retired or over-wide claim put back, the
  sentences on the call's own timeout, the queue wait, the fallback and `gpu reserve` removed, one route key removed, each
  lane that takes no lease un-named, the seeded maximum changed in the row and (the other direction) in the profile file,
  and the doctor sentence reverted to "the deployed".

### Fixed - the root test suite no longer leaves an empty state root in the OS temp dir for every copy of its test binary that lost the state dir, and a suite that cannot remove its throwaway root now says so and fails

- **The defect.** Plain `go test` runs left `lo-root-state-*` folders in the OS temp dir: 58 on one machine at a census on 2026-10-14,
  57 of them empty (the 58th held a ledger file). A few `lo-delegate-state-*` and `lo-mcpserver-state-*` folders had also been seen; no
  complete run of those two suites left one, and what made them was not measured. Two causes were on the table: (a) a re-launched copy of
  the root test binary makes a state root of its own and ends by `os.Exit` inside a test or is killed by its parent, so the removal in
  `runIsolated` never runs; (b) the removal fails (a process still holding a file or the folder on Windows) and `_ = removeAll(dir)` drops
  the error. The measurement decided it: the root package's gpu tests alone left 13 roots in one run, every one with no contents (a failed
  removal of a used root leaves its contents, a root nobody used is empty), while a complete run of `internal/delegate` and of
  `internal/mcpserver` left nothing. The tests that start a real `gpu reserve` wrapper take `LOCAL_OFFLOAD_STATE_DIR` out of the copy's
  environment (`treeEnv`) or empty it in their own (`isolateFromMachineState`), so the copy fails `inheritedSuiteRoot` and, by design, gets
  a root of its own (the net fails closed). That root lived in the OS temp dir, and the copies it belongs to never reach the removal.
  Cause (a) is the leak. Cause (b) was not seen in any measured run (no removal failed in the runs made with the report in place:
  `internal/delegate`, `internal/mcpserver` and the root package's gpu tests), but the old code dropped the error, so a failed removal could
  not have been seen either; it is now retried, reported and fails the package.
- **The fix (test code only).** In the root package `runIsolated` now makes a copy's root inside the throwaway root of the suite that
  launched it (`relaunchRootParent`: a `relaunch` folder in the root `LO_ROOT_SUITE_STATE_ROOT` names), when that variable names a
  throwaway root of the package that is still on disk; the suite removes it with its own root when it ends. The copy keeps a root of its
  own and pins the state dir, home and origin exactly as before, so the safety net is unchanged; with no live suite root named it still
  goes to the OS temp dir, and when the variable names a throwaway root that cannot be used (gone, not a folder, no room for the
  `relaunch` folder) the fallback says why on stderr instead of happening without a word. In all six suites that make a throwaway root
  (the root package, `internal/delegate`, `internal/mcpserver`, `internal/mediaremote`, `internal/pairworkloads`, `internal/pipeline`) the
  removal is now tried once more after 250 ms, and a root that is still there is named on stderr with the error. A suite that passed then
  exits 1 and a suite that failed keeps its own code: `go test ./...` and `go test .` print the stderr of a package that passed only with
  `-v` (measured on a probe module), so the line alone reached no one, and a stuck copy of the test binary can now hold the whole suite
  root, because its own root lives inside it. The copies the other suites start are covered by their own
  `TestARelaunchedTestBinaryLeavesNothingInTheTempDir` (`internal/delegate` and `internal/mcpserver` start none), so the nesting is the
  root package's only.
- **Tests, written first and red.** `TestARelaunchedCopyThatLostTheStateDirLeavesNothingInTheTempDir` starts a copy with the state dir
  taken out or emptied, once ending by `os.Exit` from inside a test and once killed by its parent, with the temp dir pointed at an empty
  folder: red before with `left 1 entries in its temp dir, want none: [lo-root-state-...]` in all four cases, and `not inside the suite's
  root`. `TestRunIsolatedRetriesARemovalOnceAndReportsWhatStaysBehind` (in the root, `internal/delegate`, `internal/mcpserver`,
  `internal/mediaremote` and `internal/pairworkloads`) and `TestFinishIsolationRetriesARemovalOnceAndReportsWhatStaysBehind` (in
  `internal/pipeline`, with `TestMainRemovesItsRootThroughFinishIsolation` pinning the wiring of its `TestMain`) were red with `the
  removal was tried 1 times, want 2` and `code = 0, want 1`; `TestRelaunchRootParentSaysWhyItFellBackToTheTempDir` was red with `the report
  "" should contain "root tests:"`. `TestACopyWithNoLiveSuiteRootKeepsARootOfItsOwnInTheTempDir` and
  `TestRelaunchRootParentFollowsOnlyALiveThrowawayRoot` pin the fail-closed side. Green evidence: the same gpu test selection, run with the
  temp dir pointed at an empty folder, left 13 roots before and none after. Mutation-checked once per rule, each red and restored byte for
  byte: nesting off, the prefix check removed, the existence check removed (root); in each of the six suites no retry, a retry when the
  first removal worked, no report, a report when the retry worked, no exit code for a root left behind, and an exit code that hides a
  failing suite's own; the `internal/pipeline` wiring; and, in the root, no report for each of the three ways the fallback happens and for
  the error text.
- **The top-level `Test<Name><n>` folders.** Those are `t.TempDir()`'s own parent folders, left when a test's process dies before its
  cleanup runs. No test was found that makes one without cleaning it, and no source makes one by name (no `os.MkdirTemp` call takes
  `t.Name()`). Measured with the temp dir and `GOTMPDIR` (where `go test` puts the `t.TempDir()` parents) both pointed at empty folders
  and both listed at the end, which a probe test that exits after `t.TempDir()` shows the census does see: a whole run of every other
  package of the module with both folders listed (115 packages with tests, `internal/pipeline` included; in it one timing test of
  `internal/delegate` failed under load on the unfixed tree, and that failed run left nothing either), and the root package's gpu tests (before and after the fix, plus the five tests of
  `gpu_signals_harden_test.go`) left no `Test<Name><n>` folder. The one inspected on the machine that found the leak
  (`TestARefusedUnloadOfAResidentSeatWritesNoDebt<n>`) still held a live lease file, so its test was killed mid-run; a complete run of that
  test leaves nothing. The rest of the root package's tests were not run (the whole suite is too heavy to run locally), so they are
  unmeasured, not cleared.
- **What this does not cover.** A suite killed from outside (a `go test` timeout, a kill, Ctrl-C) never reaches any
  deferred removal and still leaves its root and the `t.TempDir()` folders of the tests it was running; no test code can clean that, and
  the tripwire (`scripts/test-temp-tripwire.sh`) is what reports it. A copy started with an environment that lacks
  `LO_ROOT_SUITE_STATE_ROOT` keeps its root in the OS temp dir. The one retry waits 250 ms: a process that holds the root longer no longer
  passes silently, it fails the package.

### Fixed - offload_ask's cross-seat retry no longer comes back to this box

- **What happened.** A failed first attempt of `offload_ask` is retried on the fleet at `route=remote`, whose first
  placement never lands on this box. Inside that call the delegator has a retry of its own, and for a bare remote route
  it falls back `remote -> local`. So when the fleet node's answer failed the same acceptance, the only place left was
  this box's seat, the one the first attempt had just failed on: the goal ran a third time on this box's GPU, the note
  still read `retry on <node>`, and a caller who never named `route: "local"` got work back on the box the retry exists
  to move it away from.
- **What changed.** `RunOptions` gains `RemoteRetryOnly`, and only `askRetry` sets it (through a new last parameter of
  `contractOnFleetAt`; `agent_run`, the review lane and every other door pass `false` and keep the bare route's
  `remote -> local` retry). With it, `alternativeNode` takes the branch a reasoned remote pin takes: the retry goes to
  another fleet node or is not run, never to the local seat, and the retry-skipped note says the caller asked for it.
  It is a separate predicate (`retryOffLocal`) beside `remotePinned`, not a pin: no `pin_reason` is stamped, the ledger
  rows and results read as before, and `offload_status` still counts the ask's remote route as `unreasoned`. A synthetic
  pin reason would have claimed an operator decision nobody made. The ask's note now ends with the fleet's own skipped
  retry (`the fleet's own retry: retry skipped: ...`) when there is no other node. `agent_run`'s named-seat call is
  unchanged and keeps `PinOperator`. `docs/systems/mcp-server.md`, `docs/systems/fleet-node.md` and ADR 0078 no longer
  list the ask's retry among the callers that keep `remote -> local`.
- **The test.** `TestOffloadAskRetryNeverReturnsToThisBoxWhenTheFleetNodeAlsoFails`
  (`internal/mcpserver/ask_retry_stays_off_box_test.go`) drives the real delegator, with no fleet seam, at a scripted
  fleet node whose answer cites nothing, and a local seat that counts its runs. It asserts one run on the seat, one job
  on the node, the published first answer, `retried_on`, the skipped-retry note and the unreasoned tally. On the parent
  it fails with `ask_retry_stays_off_box_test.go:97: this box's seat ran 2 times, want 1: the ask's retry came back to
  the box the first attempt failed on`.

### Fixed - builder drift guard: the wiring locator's unlinked-input throw is tested, a proxied value is no longer sent to the wrong locator, and the strict runbook states its two expected failures

- **A wiring check over an unlinked input.** The `distinct` locator throws when either input of the node has no link
  (`... node N has an unlinked input in [a, b]`). No test covered it: removing the throw survived the whole suite. A
  test now unlinks each input in turn, and drops one entry, and expects the throw naming the builder, the key and the
  inputs; two mutants (the throw removed, `||` turned into `&&`) turn it red.
- **The proxied-widget message named the wrong locator.** A legacy proxied widget whose instance carries a value was
  refused with "read it with I", but `I` indexes the instance's `widgets_values` by the order of the subgraph's
  widget-typed inputs, not by `proxyWidgets`, so it reads the value that runs only where the two positions agree.
  The message now states the position in `proxyWidgets` and says "read it with I" only when that position is also the
  widget's place among the widget-typed inputs; otherwise it says that no locator reads that position and what `I`
  would read instead. Where the subgraph input is not widget-typed (the wildcard type, a combined type) `I` does not
  count it at all, so there is no other position to name, and the message says that no locator reads the position
  because the input is not a widget-typed input, instead of "I would read position -1". A wiring in the root graph is
  now checked before the carried value: an instance that carries a value and also wires the input used to be sent to
  `I`, which then threw "is linked", and is now sent to the source that feeds the input. `I` itself is unchanged:
  making it follow `proxyWidgets` would change what the real templates read, and no package is available here to
  prove the committed manifest still holds. Three mutants of the first branch turn its tests red, and one mutant for
  each follow-up does too (the not-widget-typed wording dropped, the carried value checked before the wiring again).
- **The runbook's strict command.** `check ... --require-templates --strict` exits 1 on the committed manifest because
  of the two `unreviewed` entries (`wf-qwen-image-edit [full] ksampler.cfg`, a probable builder defect, and
  `wf-qwen-image-21 [edit] encoder.resolution`). Both pages (`docs/systems/media-generation.md`, `render/README.md`) now
  say so, with the count and the two names, and a test keeps the pages in step with the manifest. Neither entry was
  resolved: the CFG one stays open, and the resolution one rests on a builder header that contradicts the template, which
  is a decision for a person. The builder recipes are unchanged.

### Fixed - builder drift guard: a package root given as the templates directory is refused, with the path to pass

- **The mistake.** `check --templates-dir <pkg>`, with `<pkg>` the package root (the folder that holds `templates/`)
  instead of `<pkg>/templates`, matched no template file and printed one `template X is not in
  comfyui-workflow-templates-json` failure per guarded template, each ending `installed unknown`. Those lines blame the
  package, not the argument, and the runbook command is easy to mistype that way. A reviewer hit it against the
  published package.
- **What happens now.** `--templates-dir` and `COMFY_TEMPLATES_DIR`, for `check`, `explain` and `record`: a directory
  that has no top-level `.json` file but holds a `templates/` folder exits 2 (the code every other usage error uses)
  with `... is the package root ...; pass <dir>/templates`, before any template or manifest is read or written. It is
  refused, not followed into `templates/`, because the manifest records which directory was read. A correct templates
  directory, a missing or empty directory, a directory that holds template files next to a folder called `templates`,
  `--comfy-dir` and `COMFYUI_DIR` behave as before. The runbook in `render/README.md` and the builder drift section of
  `docs/systems/media-generation.md` say which folder to pass.
- **Tests.** Red first: with a package root, `check` exited 1 with one such failure per guarded view. Six mutants each turn a
  test red: the detection removed, the no-top-level-`.json` clause dropped, the folder test dropped (a file called
  `templates` counted), the flag unguarded, the environment variable unguarded, and the message without the path.

### Fixed - the chat-lane write-deadline test can no longer hang the fleet-node test package behind a lease fence an earlier test left armed

- **`TestChatLaneOutlivesTheBlanketWriteTimeout` hung until the package timeout.** The chat lane forwards through the GPU-lease fence, and a request for a model that is not resident waits at the fence for up to `ChatProxyTimeout` (ten minutes) while the host holds a lease. `TestFleetNodeAdmitsAHostUnderEveryConfiguredTailnetZone` loads a config through `config.Load`, which arms that fence process-wide at the machine's real lease directory, and never put it back, so on a host that was holding a lease the later chat-lane forward sat in `awaitLease` until the 10-minute package timeout panicked the whole run (alone, nothing armed the fence and the test takes under a second).
- **The fix (tests only).** The test that arms the fence now restores it when it ends (`restoreGPUFence`), the write-deadline test runs unfenced because it is about the write deadline, and its blocking read has a bound of its own (`t.Deadline()` minus a margin, else 30 s) that fails with a message naming a blocked lane instead of waiting for the package timeout.
- **Proved.** Red, deterministic: with a lease held, the unfixed pair panics at the package timeout with the stack in the chat lane's lease wait; fixed, the pair passes in under a second. Under CPU starvation the fixed test passed 160 of 160 runs; a mutant where the lane obeys the blanket write timeout and one where it blocks forever were red in every run, the second within the test's own 30 s bound.
- **Not changed.** `config.Load` sets other process-wide state too (seat pins, card order and more) that the same test does not restore; only the fence was proven to leak into another test, so only the fence is restored here.

### Fixed - the keep-comfy render test no longer leaves its folder behind when the stub's teardown is slow, and the render test folder removal rides out a busy folder

- **The defect.** The test "a runner given keep-comfy exits after its job while the instance it launched stays up" failed once in a full render suite run on a loaded Windows machine: its cleanup killed the detached stub with `taskkill` and removed the folder at once, `removeTree` gave up after about 2.75 s of linear retries (10 tries, 50 ms apart and growing), the removal threw `EPERM` and the `comfy-keep-*` folder survived. Something still held the folder when the removal started (a process that is still exiting keeps its working directory and its open files); what held it in that field failure is not confirmed. Two such folders from earlier runs were found in the test temp dir, each with a console log that ended mid-line (a stub killed, a folder that stayed).
- **The fix (test support only).** `render/test-scratch.mjs` gains `killTree(pid)` (the tree is listed before the kill, with the same `descendantsOf` the runners use, or read from `taskkill /T` output on Windows) and `awaitTreeGone(pids, { timeoutMs })` (polls the killed pids for up to 10 s, prints one line if the bound passes, never throws). `removeTree` now retries `EPERM`, `EBUSY` and `ENOTEMPTY` from 50 ms doubling to 1 s per step for about 15 s, and still throws when the folder cannot go and at once on any other error. `scratchDir(t, prefix, { teardown })` runs a teardown ahead of the removal, which is how the keep-comfy test's folder now comes from `scratchDir` while the kill still happens before the removal; its entry left the raw `mkdtemp` allow-list. It was the only render test with the kill-then-remove shape.
- **Tests, red first.** `removeTree: a folder a child keeps busy for 4 s is removed once the child lets go` (a child holding the folder as its working directory plus an open file, Windows only) failed 3 of 3 on the old `removeTree` with `EPERM` and passes on the new one. The back-off sequence, the three transient codes, the bound, the immediate throw on another code, the `awaitTreeGone` wait and its bound line, and `killTree` of a parent with a child are pinned with a fake clock or a real process tree.
- **Under load.** With busy-loop CPU starvation (6 burners on two cores, below-normal priority) the unchanged code failed with the field signature, `EPERM` from the removal in the cleanup hook and a leftover folder whose console log ended mid-line, in 2 of 80 sequential runs; a version with no wait and no retry failed 2 of 80 the same way; the fixed code passed 80 of 80 and then 60 of 60 runs interleaved with the other two (which showed no failure in those 60). At about 2.5% per run the load evidence is consistent with the fix but does not separate the two halves: the busy-folder test above is the proof of the back-off. Measured under that shape, `awaitTreeGone` returned in 0 to 1 ms in every run, so the killed pids were already gone when `taskkill` returned and the wait is insurance against a lag not observed here. Other load shapes produced only failures of the runner's own 15 s start budget, a different fragility.

### Fixed - the compose "running card" tests no longer race the client's first poll: the fake node holds the render until a poll has found it running

`TestRemoteComposeCardTurnsRunningWhenTheNodeSaysTheJobStarted` failed once in CI on an unrelated change
(`cards = map[completed:... queued:...], want queued, running, completed`) and passed 40 of 40 on a workstation, on main and
on that branch. The product was right: a job that begins and ends between two polls is never seen running, and its card
rightly never turns running. The tests were wrong.

- **Root cause.** The fake node's render took a fixed time (600 ms) and the client polls once straight after dispatch, then
  every second. When that first poll reaches the node before its scheduler has claimed the job it reads "queued"; the next
  poll, a second later, finds the render over, so `h.Running()` never fires and no running frame is written. The node claims a
  job in a goroutine of its own, so a loaded box can lose that order and a quiet one almost never does. The held-defer test
  had the same shape with a 300 ms render, but its assertions accepted both outcomes: it passed either way and checked a
  different branch from run to run (the "no running frame" half only ran when the race was lost).
- **The change (test code only; no production code changed).**
  - The node's render no longer takes a fixed time. It waits until the node has sent one poll answer saying "running" (the job
    is already running on the node's books while it is held), bounded at 20 s so a client that never gets there fails its test
    and does not hang it. The render-time field of the fake runner is gone.
  - The node can answer the client's first poll as a node whose scheduler had not claimed the job yet would (queued, whatever
    the job's real state). The running-card test runs that case on every run, beside the poll order it gets by itself, so the late
    claim is covered and not left to the load.
  - The held-defer test runs each of its three err_classes in both orders and asserts the one that applies: a poll found the job
    running (a running frame is required, and the close keeps its start), or none did (the node reports the running job as queued
    throughout: no running frame, no start for a held call, a start for a call that ran and broke). The second order was already
    asserted but ran only by accident; it now runs by construction.
- **Other tests of the same shape.** None. The text, vision and stt lanes answer "running" from a fake server that decides it
  (`runningFirst`), and the media lane already holds its render on a channel until the client has polled.
- **Proved on a Linux node (the compiled test binary run in stress mode: W concurrent copies by N runs each).**
  - **Not reproduced by load alone.** The unfixed tests passed 400 of 400 runs (8 workers by 50 runs, the two tests) and 600 of
    600 under starvation (24 workers by 25 runs, four CPU spinners, `GOMAXPROCS=1`, 16-core idle node). The failure needs the
    first poll to beat the scheduler, which a CI runner did once and this node never did.
  - **Red, deterministic.** The unfixed running-card test with the first poll answered queued: 80 of 80 runs failed (8 workers by 10
    runs, `-test.run=^TestRemoteComposeCardTurnsRunningWhenTheNodeSaysTheJobStarted$`), each with the CI message (queued and
    completed frames, no running one).
  - **Green.** The same command on the fixed tests: 0 failures in 80 runs. The two tests under the starvation shape above: 0
    failures in 600 runs.
  - **Mutant.** Only the fix reverted (the held render goes back to a fixed 600 ms), hook and assertions kept: the same two
    commands failed 80 of 80 and 600 of 600 runs, each on the subtest where the first poll reads queued, with no running poll
    served.

### Fixed - the queued-credit test and the cut-answer test no longer fail on a starved CPU, and still fail when the rule they pin is broken

`TestRunQueuedCreditExcludesNonQueuedIntervals` and `TestTheCutSaysWhatTheNodeAnsweredToTheWithdraw` each failed once inside a
full-package run on a loaded Windows machine. Both pass alone; the product was right in both, and each test read a slow runner as a
rule violation. The change is to the two tests only.

- **Root cause, queued credit.** The old test laid its 5xx gap on a clock started when the fixture was built (200 ms to 700 ms) and
  held the give-up to a 1350 to 2500 ms window, timed from the start of the call to the return of the call. A timeline of the old
  test under starvation (37 of 192 runs failed) shows three shapes. 20 failed at the ceiling check (`gave up after 2.5s to 3.1s - the
  wait is no longer bounded`) with the give-up itself on time: the last poll was 1.58 s to 1.65 s in, and the call returned 1.0 s to
  1.45 s later, which is the withdraw request, the telemetry rows and the intent close-out of a starved runner. 14 never saw the gap:
  the first poll reached the node 0.7 s to 1.56 s into the fixture's clock, after the 5xx window had passed. 3 saw the delegator go
  silent for 0.48 s across the gap, so the node answered the 5xx window fewer than five times.
- **Root cause, cut answer.** Each of the seven cases gave its call a 300 ms deadline in which the delegator had to probe the node,
  place the job, dispatch it, record the intent and read its first `accepted`. The cut can only ask a node to take back a job the
  node holds, so a deadline that passed before the dispatch was acked left nothing to ask: no DELETE reached the node, the reason
  said the subtask was never placed, and the case failed on `the node was asked [], want exactly one request`. In all 74 failing
  runs the health probe was logged as cut by the call deadline before it answered.
- **The change, queued credit.** The gap now opens after one `accepted` answer and closes only once the node has answered it five
  times and half a second has passed since its first 503, so the delegator always reads 5xx between its two sides, and a stall only
  makes the gap longer. The floor is exact and read from the node's own timestamps: from the first `accepted` to the give-up the
  delegator spent the credited time (at least the 3.02 s bound, or it would not have given up) plus every uncredited interval, and
  the gap is one of those, at least as long on the delegator's side as the node's stamps of it. A give-up sooner than the bound plus
  the gap banked part of the gap, however slow the runner was. The give-up is read where the node sees it, at the request that asks
  it to take the job back, which the delegator sends the moment it decides, and not at the return of the call. The ceiling is the
  call's context, a hang guard, and the give-up's own words name the bound it gave up at. The contract is given 3 s instead of 1 s,
  because the execution deadline is the budget plus the credit: the time spent outside the backlog (the gap, the first poll, a stall
  beside either) has to stay under the budget, and it had 0.5 s to spare in a 1.02 s budget and has 2.5 s in a 3.02 s one. A call
  that still ends on the execution deadline after the node saw the delegator silent for a second is a stall and skips, the way the
  reserve tests do; one that ends there with no such silence is a credit that was not banked, and fails.
- **The change, cut answer.** The call window is 2 s. The job stays queued on the node for as long as it is polled and the call always
  ends at its own deadline, so nothing the cases assert depends on how long the window is. The seven cases all end at their own
  deadline, so they run in parallel and the test takes about as long as one of them; the unwind allowance and the poll cadence they
  change are set once, before they start, and restored after the last of them. The unwind allowance is 5 s, so a withdraw answered
  late is still inside its bound, and each call has a 30 s hang guard.
- **Still the same rules.** The queued-wait credit counts only the intervals the call actually spent queued: an interval with a 5xx
  answer inside it is not banked. The published cut says what the node answered to the withdraw, on the result and in the corpus row,
  in the same words for each of the seven answers.
- **Proved under starvation.** The stress shape was 24 concurrent copies of one test, 8 runs each, 6 CPU spinners and `GOMAXPROCS=2`
  on an 8-core idle Windows host. The old queued-credit test failed 11 of 192 runs there, every one on the ceiling check; at 1 and 3
  copies (20 and 60 runs) it never failed and at 8 copies it failed 1 of 160. The old cut-answer test failed 74 of 192 runs there,
  every failing case on the same assertion; on a 16-core host it never failed, in 20, 60, 160 and 200 runs at 1, 3, 8 and 40 copies
  with 6 to 8 spinners. The new tests passed 192 of 192 runs each at the 24-copy shape, and the new queued-credit test would have
  skipped in none of them (a copy that turns the skip into a failure passed 192 of 192). Both passed 60 of 60 on a Linux host (6
  copies, 10 runs, 4 spinners). The full `internal/delegate` package passed on a loaded workstation. Two mutants, each committed on a
  throwaway branch and run at the 24-copy shape: the 5xx poll arm leaving the queued span open (the gap between two `accepted`
  answers credited) failed the new queued-credit test in 192 of 192 runs, each on the floor assertion with the give-up 3.03 s after
  the first poll against a 3.5 s floor; a give-up that drops the node's answer from the cut failed the new cut-answer test in 192 of
  192 runs, six cases each, all on the "carry the node's answer" assertion.

### Fixed - three call-deadline tests no longer fail on a starved CPU, and still fail when the rule they pin is broken

`TestNeverStartedRowCarriesTheJobIDTheCallerWasGiven`, `TestAnAbandonedSubtaskIsPublishedWithAJobIDAndARow` and
`TestRunBatchedDefersLaterChunksAtTheDeadline` failed under load on the untouched base and pass alone. The product was right in all
three; each test read a slow runner as a rule violation. The change is to the tests only.

- **Root cause.** Each case gave its call a 300 ms or 400 ms window and expected the subtasks that are meant to finish to have
  answered by its end. A subtask counts as answered only after its seat returns and its corpus line and ledger row are written (the
  first row of a process also hashes the test binary), and the call freezes its unfinished count the first time it looks at its
  deadline. On a starved runner the fast subtasks were still writing their rows, so they were counted as unfinished, by design.
  Under starvation (24 concurrent copies of one test, 8 runs each, 6 CPU spinners and `GOMAXPROCS=2` on an 8-core idle Windows
  host) the old tests failed 178, 146 and 183 of 192 runs. The batched case read between 3 and 17 unfinished where it expects 2; the
  never-started row read "17 unfinished" in 112 of its failures; the abandoned-subtask case failed 47 times on its summary (two
  defers) and 99 times on finding the call's own row, which carried the words of the published reason with a count of 2 where the
  case expected 1.
- **The change.** The three cases share one call window, `startWindow`, of 4 s. Timing the start with a long window at the same
  shape, the last of 15 pages answered at a median of 0.3 s and a worst of 1.6 s over 192 runs, and the fast subtask of a
  two-subtask call at a median of 0.3 s and a worst of 1.4 s. Nothing the cases assert depends on the window's length, because the
  slow subtask ends only at the deadline. Two assertions were pinned to a count that is a snapshot, so they now compare the ledger
  row with the reason the caller received instead (the count itself stays pinned by the batched case, which has a window sized for
  it). The never-started case is bounded by a context and the other two by a 30 s hang guard, so a call deadline that stops ending
  the call fails with a message and does not hold the suite.
- **Still the same rules.** A page that never started is answered, and its row has the job id and the call-deadline words the caller
  was told. A seat that ignores its context is published with a job id and a row of its own, and its late row is under the same id.
  A batched call's deadline covers every chunk: a later chunk does not start after it, and the count in every reason spans both.
- **Proved under starvation.** At the shape above the new tests passed 192 of 192 runs each, and 216 of 216 each at 36 copies. On
  a Linux host (6 copies, 10 runs, 4 spinners) they passed 60 of 60 each. Four mutants, each committed on a detached head and run at
  the 24-copy shape, failed 192 of 192 runs each, every run on the assertion the mutant breaks: the never-started row recorded under
  a second job id, the same row with different words, the abandoned result published under a fresh id so its late row is under
  another, and a batched call that stops sharing one deadline across its chunks. The full `internal/delegate` package passed on a
  workstation under normal load.

### Fixed - an installed.json recorded under an older layout is brought to the detected tier, and no longer hides the config's tier from doctor and report

- **What it does.** `install.sh` now brings an existing `installed.json` to the tier the run detected and rendered the serving
  config for. A recorded `profile` that differs is rewritten together with its `backend` (a jq merge through a temp file);
  `accelerators`, `alt_backends` and every other key stay as recorded, an equal profile leaves the file byte for byte, an unreadable
  file is left alone with a note, and `--dry-run` prints the rewrite it would make. A Linux box that gained a card used to keep
  `dual-gpu` or `blackwell-2x16` in the manifest for good. `install.ps1` already rebuilt the file whole on every run, so the two
  installers now agree. On the readers' side, `doctor`'s `capacity:` row considers two tiers, the config's `tier_profile` and the
  installer's, and a tier counts only when the box's `agent_model` is its declared vLLM seat or that seat's llama.cpp fallback, so
  the `dual-gpu` manifest (which declares no vLLM seat) no longer erases the seat the config's tier owns. When both tiers count (the
  alias `agent-pool` is shared by `blackwell-16`, `blackwell-2x16` and `blackwell-3x16`) the row names the tier whose declared seat
  is the entry of the serving config that `agent_model` resolves to, and when that entry names neither the config's `tier_profile`
  wins over the manifest. `local-offload report` keeps answering the tier line from the manifest, and when the config names another
  tier it shows both ids, what the config composes, and says they disagree; so the report and the capacity row describe two views on
  purpose.
- **Why.** The two installers wrote the manifest differently (`install.ps1` rebuilds it whole on every run, `install.sh` wrote it
  once), and neither rewrites an existing `config.json`. So a box could carry an older id in the manifest next to a newer one in
  the config, and an alias several tiers share cannot tell them apart by itself; the serving config's own entry can. The node itself
  was never affected: a manifest of any recorded layout reads clean, keeps its alternate backends and accelerators, and resolves the
  same vendor, architecture and memory model for all three ids.
- **Not changed.** `config.json` is still never rewritten, so the seeds of a newer tier reach a box only through a regenerated
  config. `setup/selftest.ps1` still lets `installed.json` win in its profile reader: it is the Windows stack, where `install.ps1`
  has just rebuilt the file.
- **Tests.** Red first, each committed against the unfixed code. `setup/install.tests.sh` (run by `TestInstallShDryRunRules` on
  Linux) drives `install.sh` live against a stub binary: a manifest recorded under another tier comes back with the detected profile
  and backend and its other keys intact, an equal profile and an absent manifest behave as before, and a dry run names the rewrite
  and writes nothing. `doctor_fleetcapacity_test.go` pins the capacity row's order for a shared alias in three cases (the serving
  entry is the config tier's seat with an older manifest, the entry is the manifest tier's seat with an older config, the entry
  names neither), and the earlier cases pin the `dual-gpu` and `blackwell-2x16` manifests with the negative controls (no
  `tier_profile`, an agent seat the config tier does not own). `report_test.go` and `internal/fleetnode` pin the report line and
  that a manifest recorded as any of the three ids reads with its backends, accelerators and identity intact.
- **Docs.** `docs/OPERATOR-GUIDE.md` (the `capacity:` row and the capability report) and `docs/systems/setup-installer.md` (the
  manifest across a layout change).

### Fixed - the lease-renewal test no longer fails on a starved host: its helper now beats until the test has seen the renewal it needs, instead of for a fixed run

- **The flake.** `TestWrapperRenewsTheLeaseOfAHelperThatKeepsBeating` wraps a helper (a separate process) that beats a progress file every
  100 ms under a lease with a 1 s term and a 1.5 s stall window, and the helper ran for a fixed 3.5 s. On a CPU-starved host the wrapper is
  slow to notice that the helper exited; a term check landed after the helper had stopped writing and before the wrapper saw the exit, read
  a file silent for longer than the stall window, and labelled a healthy lease ("its progress file has been silent for longer than its 1.5s
  stall window"). Reproduced under a heavy parallel load (40 concurrent copies of the test binary on a single P, six CPU spinners): 6
  failures in 160 runs on the unfixed test, every one with four or five renewals logged first and then the label, which is the signature of
  the helper's end and not of late beats. A first attempt that only widened the windows around the fixed run was worse at the same load
  (27 and 41 failures in 160), because a later term check then landed in the same gap.
- **The fix (test code only; no production code changed).** The helper beats until the test writes a stop file (`LO_HELPER_STOP_FILE`),
  beats once more and exits at once; its run length is only a safety cap, and the other tests that use the helper are untouched. The test
  waits, bounded by the test deadline, until the lease record shows a term end stamped by a check made more than one stall window after the
  progress file was created (stall window 3 s, term 5.5 s), then stops the helper. The lease is judged only up to that moment, while the
  helper is beating (never labelled expired); what the wrapper does with the silence after the helper is gone is the rule of
  `TestWrapperLabelsTheLeaseOfAHelperThatStoppedBeating`. A failed run stops the helper and collects the wrapper, so no wrapper is left
  running with `os.Stderr` redirected into the next test.
- **Why it proves the rule.** The progress file's mtime is its creation time, so before this change a helper that never beat still read
  fresh at the first term check and the test was partly vacuous. Now a term end `E` can only come from a check made at or after `E` minus
  the term, in both branches of `nextEnd`; waiting for `E` minus the term to pass the file's creation plus one stall window means that
  check found a file written to after it was created, which only a beating helper provides.
- **Proved** (same load shape for each row, 160 runs, `GOMAXPROCS=1`): unfixed test 6 failures; fixed test 0 failures; mutant, product (the
  progress age read from the lease's acquisition time instead of the file's mtime) 160 of 160 red; mutant, test (the helper never beats)
  160 of 160 red, and 3 of 3 red on an idle host. Linux, 8 workers by 4 runs with four spinners and `GOMAXPROCS=2`: fixed test 0 failures.

### Fixed - the guide and the node page say where a media call's lease window comes from and what holds it

- **The open question.** A 5,400 s video timeout against a 3,600 s lease. The tree has no such pair: every generation
  route takes the media lease for its own timeout and the render is killed at that value (pinned in `internal/pipeline` by
  the lease-window fragment already in this release), and the 3,600 s `DefaultTTL` is only the fallback for a lease taken
  with no window. The code defaults (`imagegen_timeout_sec` 720, `videogen_timeout_sec` 1,500, `audiogen_timeout_sec` 720,
  `gpu_wait_ms` 90,000) are what `config.example.json` and `setup/templates/config.json` carry, and `profiles.json` raises
  them per tier on purpose. No seed value and no code default changed.
- **What holds a media lease, read in the code.** The acquirer stamps the lease's end as now plus its whole window: for a
  generation call that is the route's own timeout, or the per-image timeout times the jobs of a batch
  (`record` in `internal/gpulease`, `RunImageBatchWaiting` in `internal/pipeline`). The window is granted whole, whatever its length.
  `gpu_max_term_min` is not a bound on it. Taking a lease only records the term (`stampTerm` writes `TermMs`,
  `RequestedMs` and `MaxTotalMs`), and the term is read only as renewal arithmetic (`nextEnd`) by the term check
  (`AdvanceTerm`), whose one caller is the tick of a `gpu reserve` or `gpu hold` holder. A pipeline media lease never runs
  that check, so nothing labels it expired, and the yield asker, which also lives only in those holders and needs an
  expired or due term, never asks it to yield. What keeps it is its heartbeat, renewed every 15 s while the render runs:
  a lease whose holder is alive is reclaimable only when its heartbeat is stale and its window has ended. Past the window the same timeout is the
  render's own deadline, so the render is killed and the lease released. Only an explicit takeover, or the death of the
  process that holds the lease, frees the card sooner.
- **The cost of a long timeout.** How long a wedged render holds the machine-wide media lease: the seeded figure, up to
  3,600 s for an image and 7,200 s for a video. That is the only cost, and the guide already says to size a resize from
  the seeded figure. An image batch is no different: its window is the per-image timeout times its jobs, granted whole,
  kept by the same heartbeat. Whether to resize a tier seed stays a measurement decision per tier.
- **What changed.** `docs/FLEET-NODE.md` says a fleet media job's lease window is its route's own timeout key on the node
  that runs it (the payload carries none), that the window is granted whole and kept by the job's heartbeat, that
  `gpu_wait_ms` (90 s by default) is the queue wait and not a lease length, and that `gpu_max_term_min` does not bound it.
  The guide's engine + lease row says the same, names `gpu_max_term_min` only as the renewal term of a `gpu reserve` hold
  (renewed by that hold's own holder), and tells a `gpu reserve` wrapper to size `--for` from the key of the route it wraps
  (a seeded 5,400 s video wants `--for 90m`). The node page points at the guide's timeout chain section in prose, not
  through an anchor.
- **Tests (root package).** `TestTheDocsStateWhereAMediaCallsLeaseWindowComesFromAndWhatHoldsIt` requires the statements
  above in the guide row and in the node page paragraph (quoting the 90 s from `config.Default()` and all seven route
  keys) and refuses, by name, the retired claim that a media timeout must fit one lease term.
  `TestTheMediaTimeoutKeyListMatchesTheConfig` reflects over `config.Config` and fails when a `*_timeout_sec` key is in
  neither the media list nor the list of clocks that are not lease windows, or when a listed key is gone from the config, so
  a new generation route cannot slip past the docs and template checks. `TestTheSetupTemplateRestatesTheCodeDefaultsOrNothing`
  pins the template's clocks to `config.Default()`. `TestSeededGPUWaitsPassTheDoctorsVisionWaitRule` holds every shipped
  `gpu_wait_ms` to the doctor's 3x vision-wait rule, with a blind-gate count and its negative controls.
- **Red-first.** The docs test was committed red against the docs as they stood before this fix (the guide row and the
  node page lacked "granted whole" and the heartbeat, and still carried the term-bound and batch-cap wording), with the other
  three tests of the file green in that commit; the docs commit that follows turns it green.

### Fixed - a refused placement is no longer a job in the savings summary, the top defer reasons or the other job counters, and a literal `null` ledger line is an unparsed line

- **A refusal row is not a job.** A node's capacity refusal at dispatch (a 503 or a 429) did no work: the delegator waited and
  placed the subtask again, and the subtask's outcome is another row. The failure register already set the marked row aside as
  a `refused_attempts`, but its own page said "the savings summary's job counters still count a marked row", and they did: each
  refused placement added one call and one deferred job to the summary, put "dispatch: status 503: queue full" beside the real
  reasons in the top defer reasons, gave a session a row it never ran, and raised the defer rate of the task it was filed under.
  `ledger.CountsAsJob` now returns false for a row carrying the refusal mark, beside the dispatch marker and an inner row whose
  parent is present, and every reader that counts jobs follows it, so they agree. The register reads the same numbers as before
  through its own predicate (a refusal row that is a record of its own is a refused attempt; an inner row carrying the mark is
  still neither a job nor a refused attempt), and the summary now skips a refusal row before its inner-row branch so a refusal
  can never add saved tokens. A ledger written before the `refusal` column existed has none marked, and its refusals still count
  as the jobs they were then recorded as.
- **Readers whose numbers move** (on a ledger that holds refusal rows; nothing moves on one that holds none): the savings summary
  of `local-offload ledger` (`calls`, `deferred`, `by_task`; `excluded_non_production`, where a test-class refusal is no longer an
  excluded job); the top defer reasons it prints (`top_defer_reasons_7d`); `ledger.JobRows` and so `local-offload stats` (`n`, `deferred`, the defer rate
  per task), `loupe` (rows, deferred, the defer reasons, the per-task tallies) and `audit-sample` (a refusal is no longer a hard
  case to sample); `ledger.ProductionRows` and so the count of excluded jobs every production view reports; and the session split
  (`ledger --by-session`, where a session that only had refusals has no row and a session's `rows` drops by its refusals; the token totals
  do not move, a refusal row carries none). Not moved: the failure register itself, and the readers that take the production
  rows without applying the job rule (health, router and calibration, confhead) since they never skipped a marker or an inner row
  either. The earlier failure-register entry chose not to change the other counters in its own pull request; this is that change.
- **A literal `null` line is unparsed.** `encoding/json` decodes `null` into a struct without an error and without touching it, so
  a ledger line holding only `null` read as a row with every field at zero: a job row that did not fail in the register, a
  completed call of no task in the summary. A ledger row is a JSON object, and the register now counts any line that is not one
  (`null`, an array, a bare number or string, garbage) in `unparsed_lines`, so a partly unreadable ledger still does not look like
  a quiet one. The ledger's own file readers (`ReadAll` and the savings summary) skip the same line, so the summary and the
  register agree on what a row is. `{}` is an object and stays a row: it counts as a job that did not fail, in the register and in
  the summary alike, and is not in `unparsed_lines`. Every column of the ledger is optional (a row from before a column existed
  lacks it, and the writer omits what is empty), so a reader cannot tell a minimal old row from an empty one by looking inside,
  and what an object must contain is a schema rule that every new column would have to be remembered in, not a parse rule.
- **Comment only.** `dispatch_refused` files a 401 or 403 answer as the caller's (`internal/ledger/failurekinds.go`). A comment
  at that line now says a credential refusal between the fleet's own nodes, such as a stale fleet token after a rollout, is a
  configuration fault, and that splitting 401 and 403 into a class of their own is a possible later change. No behaviour changes.
- **Tests.** The red tests fail on the parent commit. `TestARefusalRowIsNotAJobInAnyJobCounter` (a ledger with three real jobs, one
  refusal row and a test-class pair) reports 4 calls and 2 deferred where 3 and 1 are right, lists the 503 among the top defer
  reasons, counts 2 excluded test jobs where 1 is right, and gives the refused-only session a row; the same test holds the
  register's refused-attempt, job and failure counts as they were. `TestStatsDoNotCountARefusalRowAsAJob` is the stats report.
  `TestALiteralNullLineIsUnparsedAndAnEmptyObjectIsARow` (a ledger holding `null`, `{}`, a valid failure row and a truncated line) read `unparsed_lines` 1 where 2 is right and 3 job rows
  where 2 is right; `TestTheOtherLedgerReadersSkipANullLineToo` read 3 calls and 3 rows where 2 is right. Two guards passed on the
  parent and pin what must not move: `TestAnInnerRefusalRowIsNeitherAJobNorARefusedAttempt` and the register's refused-attempt
  subtest. `docs/systems/failure-register.md` and the `refusal` row of `docs/FLEET-NODE.md` say the new rule.

### Fixed - the two call-reserve start-order tests no longer fail on a starved CPU, and still fail when a subtask is started inside the reserve

`TestASubtaskIsNotStartedInsideTheReserve` and `TestALaterChunkIsNotStartedInsideTheReserve` failed once on a loaded
Windows machine (`Succeeded:3 Deferred:2`, and "started 4 times, want 8"). Both pass alone; the product was right.

- **Root cause.** The launch loop refuses to start a subtask once the call has no more left than the reserve
  (`callDeadline.noRoom`, read at `time.Now()` through `timeLeft`), and the first test gave the first wave a 0.8 s window
  (a 2 s call, a 1.2 s reserve) to start in. A starved CPU that delayed the first starts past that window made the
  reserve refuse them, correctly, and the test read a timing accident as a rule violation. The second test had the same
  shape: its second wave had to start within 1 s of the first wave ending, because both waves ran for a fixed 2 s.
- **The change (tests only).**
  - The seat no longer runs for a fixed time. It holds at a barrier that opens just after the call's own reserve begins
    (`callDeadline.waitHorizon`, the instant the product computed), so the slot of every held subtask frees inside the
    reserve whatever the runner's speed was. The second test lets its first wave go on an event (the fourth start), not a
    sleep.
  - The call's deadline state is built up front, the way `RunBatched` builds it (`RunOptions.call` already honours a
    prebuilt one), so the test reads the call's own span instead of assuming it.
  - The windows are wider (4 s with a 2 s reserve, 6 s with a 3 s reserve), because the clock the reserve is read from has
    no seam and the first wave's start remains the one thing that depends on it. A barrier alone does not widen that
    margin, which is why the windows are wider as well: the first wave now has 2 s and the second wave 3 s, where
    they had 0.8 s and 1 s.
  - A refused subtask is checked against the launch loop's own decision: its reason carries how much of the call it found
    left, and that must be no more than the reserve. A run whose first wave was refused by the reserve for real (a stall
    longer than the margin), or whose freed slot was answered by the call's deadline, is skipped as a stall; it can no
    longer fail.
- **Still the same rule.** A subtask whose slot frees inside the reserve is not started, is an unplaced capacity defer with
  the same reason text, and is not the call-deadline cut. The barrier opens only inside the reserve, so a start of the
  fifth subtask (or of the third wave, or of the second chunk) is a failure on any runner; the start count can no longer be
  made right by a slow one.
- **Proved under starvation.** A stress shape of 40 concurrent copies of the two tests, 6 CPU spinners and `GOMAXPROCS=2` on an
  8-core idle machine: the old tests failed in 38 of 200 runs (all of them the first test; the second never failed in 200), where
  3 and 8 copies (60 and 160 runs) never failed. The new tests passed 200 of 200, with 0 skips, and 20 of 20 on a Linux host. A
  mutant that makes `noRoom` always report room (a subtask started inside the reserve) failed both new tests in 200 of 200 runs,
  each on the "started N times" assertion and not on a skip.

### Fixed - the warm-watch reload test no longer fails on a starved host: its stand-in now holds the load until the watch has polled it twice, instead of for a fixed 300 ms

- **The flake.** `TestWarmKeepsWaitingForALoadItSawStartingThroughOneUnreadableRunningAfterAReload` pins that one unreadable `/running` poll in the
  middle of a load the watch saw starting does not end the warm. It needs two polls of the starting seat, the second one failing, and its
  stand-in kept the seat "starting" for a fixed 300 ms. On a starved CI runner the watch polled once inside the hold, the load had landed by the
  next poll, and the test's own check fired ("the stand-in never failed a poll mid-load (polls while starting=1): the test proved nothing") on a
  docs-only run. The check was right to fire, since the test then proves nothing. The product was not wrong; the test depended on a race.
- **The fix (test code only; no production code changed).** The stand-in (`warmOrderSwap`) gets `holdPolls`: a load that outlives its health
  request lands only after that many `/running` reads have been answered while the seat was starting. The read is counted after its answer is
  written, so the poll that decides the test is served before the seat changes. A wall-clock cap (`holdCap`, 10 s) ends the hold anyway, so a
  watch that never polls fails the test instead of hanging it. The test sets `holdPolls: 2` and runs at two poll cadences, 10 ms and 400 ms; the
  second is slower than any fixed hold the load could have had, which is the starved host made deterministic and keeps the stand-in from going
  back to a fixed hold unnoticed. Its check now counts the poll the stand-in actually failed. A test that does not set `holdPolls` keeps the fixed
  `warmHold`, so the other tests that share the stand-in behave as before.
- **Proved** (stress mode of the remote gate runner on an idle node, `--stress 24:8 --burn 6 --env GOMAXPROCS=2 --test-args '-test.run=^TestWarmKeepsWaiting'`,
  192 runs):
  - Red, deterministic: the unfixed test with the watch's poll interval set to 400 ms (a hook for the measurement, not committed) failed 5 of 5
    locally and 192 of 192 on the node, every one with the message above. Load alone did not reproduce it: the unfixed test passed 192 of 192 on
    that command and 384 of 384 on `--stress 48:8 --burn 12 --env GOMAXPROCS=1`, because it takes a stall of about 300 ms between the answer
    and the next poll, which a busy shared runner produces and these loads did not.
  - Green: the fixed test, 0 failures in 192 on the same command and 0 in 384 on the heavier one. The one other user of the changed path,
    `TestAnUnloadSeatAcquireStartsOnAnEmptyCardWhenTheWarmOutlastsTheHealthWait`, 0 failures in 48 (`--stress 12:4 --burn 4`).
  - Mutant, product: the unreadable-poll case of `watchWarm` loses its "last reading found the seat starting" condition, so the watch calls the
    state unknown and gives up on the poll and the load is sent again. The fixed test failed 192 of 192 at both cadences ("health requests=3
    (want 2) loads=2 (want 1)") and 3 of 3 locally. The mutant was built as a scratch commit and never touched the branch or the working tree.
  - Mutant, self-check: a stand-in that never fails a poll, and a stand-in whose cap ends the hold at once, each failed 3 of 3 locally at both
    cadences on "the stand-in never failed a poll mid-load".
- **Same family, not changed.** The other tests of `gpu_warm_watch_test.go` and `gpu_warm_reload_test.go` that wait out a timer (a goroutine
  that flips the seat after 100 to 150 ms, a 400 to 800 ms hold against a 100 to 150 ms client timeout, a floor on the number of re-sends inside
  a grace) depend on a clock, not on a count of polls, and have no check that goes vacuous; they are left as they are.

### Fixed - a run the write door refuses (a write-class deferral) keeps its stop note, response shape, final-budget fit and prefill accounting instead of dropping them

- **What was lost.** `runAgentTask` copied the loop's `StopNote`, `OutputTruncated`, final-budget fit (`FinalBudgetFit` and `BudgetNote`), `FinalReissue`, `ResponseShape` and the prefill accounting (`PrefillSteps`, `PrefillTokens`, `CacheTokens`, `PrefillMS`) onto the wire result, and the prefill figures onto the node's ledger meta, only after the write door closed. A cap breach at the door returns a write-class deferral on the spot, so a deferral of that class carried none of them: the wire result and the ledger row read as a run that was never measured. The comments in the function already stated the rule (run fields are set before every defer branch and before the write door), and the pager object had been moved ahead of the door for that reason; these fields had been missed.
- **Why it mattered.** A run whose write set breaches the cap is usually a long one with many steps, which is the expensive kind the prefill accounting exists to measure (and the stop note and response shape are the evidence for why such a run ended and how its seat answered). Dropping its numbers left only the cheap runs in the corpus and biased the measurement.
- **The fix.** The assignments moved, with no second copy, to sit with the other run fields set right after the loop, ahead of the door. Nothing between the old and the new position reads them. The comments that state the rule now name the write door.
- **Test.** `TestWriteClassDeferralKeepsTheRunsStopNoteResponseShapeAndPrefill` scripts a seat that makes one real `write_file` call while eight other files appear in the write root (a cap breach), reports server timings on every step, and ends its loop on the step budget. It asserts the write-class deferral carries the stop note, the response shape and the prefill accounting on the wire, and the same prefill figures on the ledger meta.
- **Red first.** On the unfixed code the test fails on all four assertions; the first line is `stop_note = "", want the loop's forced-final evidence on a write-class deferral`, followed by `response_shape = ""`, `wire prefill = steps 0, prefill tokens 0, cache tokens 0, prefill ms 0` and `ledger meta prefill = steps 0, prefill tokens 0, cache tokens 0, prefill ms 0`. It passes with the move.

### Fixed - the context-bench adapter's outcome words are pinned to the measurement gate's taxonomy

- `internal/compeval` writes the gate's seven outcome words (`pass`, `task_fail`, `cheat`, `infra_error`, `timeout`,
  `judge_error`, `not_run`) as its own strings, because the adapter landed before the gate's enum did. A class the gate
  added, dropped or reordered would have left the adapter's results unreadable by the gate while every adapter test stayed
  green. `TestContextBenchOutcomesAreTheGatesTaxonomy` now compares `CBOutcomes` with `eval.AllOutcomes()` in order, and with
  the literal the run tests grade results against. Test only; no behaviour changes. A mutant that renames one word fails it.

## [0.186.0] - 2026-10-14 - every tool result of a stale MCP server says so, a video, animation or audio call can name the node that runs it, a kept ComfyUI frees its models after 300 s idle, the installer stops writing the every-expert --cpu-moe, the display guard can read the monitor's card, the asker's session and run class ride the fleet wire, an overdue lease's holder can be asked to yield, a speed term in a contract's acceptance fails the build, and the changelog is one file per change

### Added - the changelog is one file per change: `changes/unreleased/` fragments, assembled at the release step by `go run ./cmd/changelog`

- **The defect.** Every branch edited `CHANGELOG.md`, so two branches could not meet without a conflict in it: 864 of the 2,566 commits since 2026-09-01 touched the file, the 0.184.0 release merged ten branches in a row, and the hand edits that resolved those conflicts overwrote the release heading above the new entry twice (`fa2e8866` restored the 0.185.0 heading, `8029f3d3` the 0.183.0 one) and once joined two entries with no blank line between them (`de777159`). A test that read only `## [Unreleased]` was also blinded by the bump that empties it (`387334a5`).
- **The change.** A change adds one new file, `changes/unreleased/<branch-slug>.md`, whose body is exactly the text the changelog will carry (what changed, why, the red-first note, the mutation list, as the entries here are written), and never edits `CHANGELOG.md`. Two branches add two files and cannot conflict. The release step is one command that moves the files under the new heading and cannot eat the one above it. Format, rules, release order and recovery: `changes/README.md`; `CONTRIBUTING.md` (Versioning, the new "one file per change" section, the pull-request steps) and `CLAUDE.md` (two rows of "Where things live") say the same.
- **A fragment.** The file name matches `[a-z0-9][a-z0-9._-]*.md` (`README.md` is the one non-fragment); the body is valid UTF-8 with no control byte but tab and LF, and a CR only as half of a CR LF pair (a lone CR ends a line in Markdown, and every line rule here splits on LF, so a forged heading could hide behind one); it is not empty; its first non-blank line starts `### ` (new: 121 of the 387 existing release entries open with a bullet or a lead paragraph, and a loose bullet would be glued under the previous fragment's section); no line starts `# ` or `## `, even inside a code fence (a `## [x.y.z]` line in a fragment is the forged heading the tool exists to prevent); and no line carries a release date between two separators (the shape ` - YYYY-MM-DD - `, or the same with em dashes), because the tree check refuses such a line anywhere but a release heading, and a `### Fixed` title or a bullet that copied that shape would otherwise be assembled, deleted, and then fail the changelog's own check. The kind words (`Added`, `Changed`, `Fixed`, `Removed`, `Security`) and the absence of a version number in a heading are conventions, not checked. Any other entry in the directory (a subdirectory, a `.txt`, an upper-case name) is an error that names the file, because a skipped fragment is a lost entry; so is anything in `changes/` but `README.md` and `unreleased/` spelled exactly so (a fragment saved one level up, or in an `Unreleased/` directory that a case-insensitive checkout reads and Linux does not), and a file where `changes/` or `changes/unreleased/` belongs; a directory that does not exist holds no fragments.
- **The assembler.** `go run ./cmd/changelog assemble -title "<one line>" [-date YYYY-MM-DD] [-root DIR] [-dry-run]`. The version is the `VERSION` file's, so the order is bump the three carriers first and assemble LAST (it writes the top of the changelog; a merge of `main` or a renumbering after it conflicts in the hunk the tool wrote). `-title` is required; the date defaults to today; `-dry-run` prints the section and writes and deletes nothing. It reads and validates everything and splices and verifies in memory before it touches a file, writes `CHANGELOG.md` through a temp file in the same directory and a retried rename, deletes each consumed fragment with a retry (Windows holds files), and prints one line, `Deletes: changes/unreleased/a.md, ...` (forward slashes), for the release commit. The new section is `## [VERSION] - DATE - TITLE` under an emptied `## [Unreleased]`; fragments come in file-name byte order, one blank line between them; the line ending is the `## [Unreleased]` line's own (a Windows working tree is CRLF, the index and Linux are LF) and the rest of the file is never rewritten. It refuses a bad version, date or title, a version that already has a heading (the overwrite guard), zero or two `## [Unreleased]` headings and an empty release; the result is checked against the old file (bytes before the heading unchanged, everything from the old top release heading to the end byte for byte, the new heading exactly once, the block blank, and no line of the section it wrote that the tree check would refuse for carrying a release date) before it is returned. Any refusal before the write leaves the changelog and every fragment byte for byte as they were (exit 1); a usage error exits 2; `check` runs the tree rules (exit 0 or 1). If a delete still fails, it exits 1 and names the files, and the duplicate guard below then refuses the survivor until it is deleted. Whatever an old-style branch left under `## [Unreleased]` is moved to the top of the new section, unvalidated, never dropped: the 0.186.0 release itself depends on this path.
- **The tree rules and the gate.** `internal/changelog.CheckTree` is the one rule set: the fragments are well formed and `changes/` holds nothing else but `README.md` and `unreleased/`; the `## [Unreleased]` block of `CHANGELOG.md` is blank ("entries go in changes/unreleased/"); no fragment's text is already in `CHANGELOG.md` (the duplicate a failed delete leaves behind); every semver release heading appears once; no line but a heading carries a release date between separators (a heading glued onto another line; the older em-dash separator counts too). The last two hold on the current file and would catch the incident class even when the tool is bypassed. `TestTheChangelogTreeRulesHoldOnTheRealTree` (root package) is a thin call of it, and `check` is the same call; the tracked-tree lints (conflict markers, control bytes, operator identity, the leak gate) already read every fragment. Only the root `CHANGELOG.md` is read or edited; the vendored `tools/*/CHANGELOG.md` ledgers never are.
- **The prose gates see a fragment.** The four tests that read `CHANGELOG.md` for retired wording or a required sentence (`TestTheDocsQuoteTheSIGTERMCardTextTheCodeProduces`, `TestTheDocsDoNotClaimAnIdleBoxNeverTouchesTheServingConfig`, the CHANGELOG half of `TestTheDocsNameNodeReportedOnEveryDoorThatPublishesIt`, `TestTheProseSaysWhichNameSelectsWhichRows`) now read it through `changelog.Combined(root)`, the file with the fragments spliced into its `[Unreleased]` block, so a retired phrase that rides in on a fragment fails on the branch that wrote it and not first on the release branch after the assembly. Each check became a function of a root that returns its problems, so a test can run it on a root of its own.
- **What this does not do.** Two releases still conflict on `VERSION`, `internal/buildinfo/buildinfo.go`, `.printing-press.json` and the top of the changelog: only the feature-branch-versus-feature-branch conflict and the heading overwrite are gone. Assemble does not bump the version carriers (`TestVersionSourcesAgree` already guards them). A stale branch that carries its own release heading still conflicts by hand. There is no ADR (a process rule, carried by `CONTRIBUTING.md` and `changes/README.md`), no CI step that demands a fragment per pull request, and no place yet for a release-level lead paragraph.
- **Behaviour change.** `go test` now fails when anything is written under `## [Unreleased]`, and says to write a fragment instead. A branch written the old way goes red once it merges this change; the fix is mechanical (cut the text into a fragment), and if the release comes first the assembler absorbs the text. `CONTRIBUTING.md` states both ways a change reaches `main` (a direct pull request that is also a release, which adds its fragment, bumps and assembles as its last commit, and a release branch that merges the fragments, merges `main`, renumbers and assembles last) and the recovery when `main` moves after the assembly (revert the release commit, merge, renumber, assemble again). A `Deletes:` line is needed only when a fragment reached `main` earlier through a fragment-only pull request that did not bump the version.
- **Tests, red first.** A seam commit added the exported surface returning zero values (and, in a second one, `readFragmentsFS`), then the tests, each failing on its assertion before the implementation (line numbers as of the red commit): `changelog_test.go:80: release text differs` (got ""), `changelog_test.go:166: no error, want one naming ["0.186.0" "already"]`, `changelog_test.go:381: "a.md" with body "" is accepted, want a problem naming "is empty"`, `changelog_test.go:423: order = [], want [0-x.md a-10.md a-2.md a.md b.md]`, `tree_test.go:113: no problem reported, want one naming [... "already in CHANGELOG.md"]`, `main_test.go:134` (the changelog unchanged), `main_test.go:250: exit 0, want 1`. A review of that build found three more holes, each fixed in its own red-then-fix pair: a release date between separators in a fragment line, which `assemble` wrote into the changelog and the next `check` refused with the fragment already deleted (`changelog_test.go:415: "a.md" with body "### Fixed - ..." is accepted, want a problem naming "release date"`, `main_test.go:258: exit 0, want 1`); a lone CR hiding a forged heading from every LF-based rule (`changelog_test.go:421`, `main_test.go:262`); and an entry of `changes/` beside the fragment directory, or a file where a directory belongs, which nothing read (`tree_test.go:120: no problem reported, want one naming ["changes/stray.md"]`, `main_test.go:265`, and, after a behaviour-free seam `strayEntriesFS`, `tree_test.go:172`). Fifty-two tests (175 with their subtests) across `internal/changelog` and `cmd/changelog`, on fixtures that spell out their CRLF, LF and mixed endings. The four prose gates took a seam commit (each check a function of a root, reading the plain file), then one fixture test each, red (`node_docs_fragments_test.go:39: a fragment carrying the retired wording is not refused`, `changelog_fragments_test.go:74`, `:82` and `:102`, `node_reported_fragments_test.go:36`), then the reader change. NOT red first: `TestTheChangelogTreeRulesHoldOnTheRealTree` passes on `main` by construction (the block is empty and no fragment exists), so it is proven by a named mutation (the call pointed at a directory with no changelog) and not by a red commit.
- **Mutations.** One hundred and four one-line mutants through `scripts/mutation-check.sh`, each restored byte for byte, every one `MUTATION-VERDICT: RED`: the fragment reader (the README skip, the directory refusal, the absent directory, the sort), each fragment rule (name, UTF-8, the control-byte test and its tab, CR and DEL exemptions, the lone-CR test and its offset and line number, empty, the `### ` opening, the `#`/`##` pattern at each level and for a tab, the release-date test, its em dash and its line number), the entries of `changes/` (the README exemption, the exact-case directory name, the absent `changes/`, a file in place of `changes/` or `unreleased/`, a failing `Stat` and a failing `ReadDir`, the root they are read from), the block (exactly the heading, any bracket heading as the end, CR tolerance, the end moved one line down, which is the overwrite), `Splice` (one heading, the ending taken from the heading, legacy text, fragment normalisation, no blank line between sections, the empty release, the version, date and title checks and their spellings, a heading matched by prefix, the heading format, a heading at the end of the file), `verify` (each of its five checks, the release date among them, and the section it judges), `CheckTree` (the heading count, the blank-block test, the legacy allowance, the duplicate guard and its two normalisations, the repeated-heading count, the date rule and its exclusion and its em dash), `Combined` (the splice dropped), the assembler (dry run, validation, the version trim, the clock, the slash in the `Deletes:` line, the retry count and wait, the delete and rename failures, a fragment already gone, the title, the extra argument, `check`, `help`) and the readers (the root, `internal/mcpserver` and `internal/rig` gates back on a plain read, and the real-tree call). The first pass left two survivors (the release that vanished entirely was only caught together with another check; a dated non-semver heading was not in a fixture) and five spellings that did not compile; two test cases were added and the five respelled, and all seven then ran RED. The review's fixes left one more (a lone CR as the first byte of a fragment was in no row), also killed by a new row. Not mutated, and said so: the file mode kept on the rewritten changelog.
- **Checked on a scratch repository.** Two branches that each append to `[Unreleased]` conflict on merge; two branches that each add a fragment merge clean into a release branch. After the bump, `check` passed and `assemble` printed the `Deletes:` line; the diff of `CHANGELOG.md` against `main` removed no line; `scripts/check-deletions.sh` against `main` exited 0 (the fragments never were on `main`). After `main` moved, reverting the release commit brought the fragments back, `main` merged without a conflict, and a second `assemble` under the renumbered version gave the right heading. A fragment that had reached `main` in a fragment-only change and was consumed by a later release made the guard exit 1 without the `Deletes:` line and 0 with it.

### Added - The asker's session and run class ride the fleet wire, and `ledger --by-session` splits a session's tokens by where they ran

- **The gap.** Every ledger row says which session asked (`origin_session`, ADR 0046) and whether the run was real work (`run_class`, register
  F48), and both stopped at the box's edge: a job a delegator dispatches to a fleet node is run by the node's own process, whose row named
  neither. A bake dispatched to a node wrote production-looking rows there, the node's ledger could not say whose work it ran beyond the asker's
  box name (`requester`), and nothing could read a session's tokens by where they ran. The delegator's own ledger already holds that split (its
  rows carry `origin_session`, `cards_tokens` and `fleet_job_id` or `node`); only the reader and the node-side stamp were missing.
- **The wire.** Two request headers, `X-Offload-Origin` (the session label) and `X-Offload-Run-Class` (the class), sent only when non-empty, never
  `production`, on every request that creates work on a node: `pairworkloads.Emitter.SetWireHeaders`, the one chokepoint the delegation dispatch,
  the queue submit and the accelerator, compose, vision, text, media and stt lanes already pass. A header and not an envelope field, because the
  dispatch envelope is a strict decoder and a new field would `400` on every node one release behind; a node that predates them ignores them and
  an asker that predates them sends neither, so a mixed fleet behaves as before. A delegation pinned for `measurement` sends the class
  `measurement` when its process has none of its own (the process class wins, as in the ledger's own `settleRunClass`), so the node's row for it is
  a measurement too.
- **One cleaning rule at both ends.** `ledger.CleanOrigin` (printable ASCII, at most 96 bytes, interior spaces allowed) and `ledger.CleanClass`
  (a short `[a-z0-9_-]` token, lower-cased, `production` is no class) are exported. The sender sends only a value they accept; the node, the queue
  holder and the claim loop keep only a value they accept. A label valid in the asker's own `Record` is therefore never dropped on the wire, and one
  the ledger would refuse never reaches a row. A value over the cap is refused whole, not cut like the dispatch tenant is: a cut session id would
  silently attribute the row to a different session.
- **Both admission doors stamp them.** A pushed job: `admit`, which every lane joins, reads the headers and puts `Request.OriginSession` and
  `RunClass` on the request the runner builds. A pulled job: the holder's submit handler stores them on the queued job (`fleetqueue.Job.Origin`,
  `RunClass`, omitted when empty, so an old holder and a new one read each other's records), and the claim loop cleans them again, since they came
  over the network, and stamps the same two fields. The pipeline carries them as `Meta.AskerSession` and `AskerClass` (named apart from the node's
  own process origin) into the row. All four fields are `json:"-"`: a request body cannot name its own session or class, and `Meta`, which a node
  marshals whole into its poll results, does not echo the asker's session id back.
- **The class is believed only from a caller the node can authenticate.** A labelled row is left out of every production view, so a class from a
  caller the node cannot identify would let any peer that reaches the port hide the node's rows. A token-gated lane has already passed its gate
  (a bearer when a token is configured, else a loopback listener); a lane that is open on purpose (the media task types and the legacy
  path-taking stt lane, which deployed tokenless clients use) is trusted only when a token is configured and the request presented it. For any
  other caller the class is dropped, and so is a session label that carries a class of its own (`bake:run-7`, the `go-test` label), because
  `ledger.ClassOf` reads a class off such a label when the row names none; a plain session label is kept. The queue submit route sits behind the
  holder's own guard, so a class that reaches it is already authenticated.
- **`stampOrigin` keeps a named session and fills the pids.** A caller that names only a session (the node writing a job's row for the asker's
  session) used to get no pids; it now gets the pids of the process that recorded the row, which is what the columns mean. A caller that names a
  pid keeps both, as before. A row whose asker sent no session falls back to the node process's own origin, as before.
- **The reader.** `ledger.SessionShares` and `SessionSharesFile`, and `local-offload ledger --by-session` (optionally `--session <id>`, which implies
  it): a `sessions` array added to the existing JSON (the default output is byte-identical), and a line saying what the number is. Per session:
  `remote_tokens` (rows that carry a fleet job id or a node), `local_tokens`, `cards_tokens`, `remote_share`, and `served_rows` / `served_tokens` for
  the rows this box ran as a fleet node (door `fleet`). Served rows are never added to the session's own totals, so a ledger shared by a delegator
  and a node does not count a job twice. Production rows only unless `--include-test-traffic`, dispatch markers and inner rows are not jobs, and
  rows with no session are one entry apart, never merged into a named one.
- **What the share is.** It is the card-side placement split, not the share gate's cards/(cards+cloud), which needs the session's transcript and stays
  in the hook; the fields are named apart (`remote_*`, not `fleet_*`) so the two are not confused. It is a floor on the remote part: a cascade call
  routed to a node over the chat lane writes no node row and reads as local, and a delegation sent with `route: "queue"` writes no row on the asker's
  ledger at all (the claiming node holds it, as a served row), so it is in neither number. A pushed delegation is not a floor: its finished row's
  fleet job id comes from the node's ack, whatever becomes of its started marker. Tests pin both shapes. It reads one box's ledger; a view across the
  fleet is a reader of several.
- **Known limits, on purpose.** Accelerator rows served by a node stay unattributed (`RecordAccel` writes from no `Meta`); the `cli:fleet-measure`
  door sends no class from a box with no process class, because the media and vision lanes know the config, not the door; the session id now
  leaves the box, to every node the asker dispatches to, beside the asker's name, with no opt-out; and the share gate hook, which sums by session
  with no door filter, would count a job twice on a box that is both session host and node (it is paused; the fix is one line there, outside this
  repository).
- **Docs.** ADR 0087 (Proposed; linked from the index and from ADR 0046's Consequences, which had named this change), the attribution header table and
  the node's recording rules in `docs/systems/pair-workloads.md` and `docs/systems/fleet-node.md`, the `run_class` row of the delegation ledger table in
  `docs/FLEET-NODE.md`, the ledger paragraph of `docs/systems/offload-pipeline.md`, and `docs/OPERATOR-GUIDE.md` (the `origin_session` column, a bake
  dispatched to a node, and `ledger --by-session`).
- **Red first.** A behaviour-free seam first (declarations, stubs, and the two flags registered and ignored, because the ledger flag set exits the
  process on an unknown flag), then tests that failed on their assertions against it, for example: `X-Offload-Origin = "", want sess-abc`;
  `originOf = ("", ""), want ("sess-abc", "bake")`; `queued job = origin "", class "", want sess-abc and bake`; `session-only row = session "sess-abc"
  pid 0 ppid 0, want ... this process`; `written row = origin_session "" run_class "", want the asker's sess-abc and bake`; `no entry for session
  "sess-abc" in []`; `--by-session printed no sessions key`. Then the behaviour. Three additions made after review are green at birth, because the
  code they pin was already right: they are scored by the mutants named below, which turn each of them red (the windowed-index test in the ledger, the
  empty-bearer case on a node with no token, and the queue lane writing no asker row).
- **Mutation proofs.** 48 single-line mutants, each scored RED by `scripts/mutation-check.sh` (the mutant compiled, the named test failed, the file
  was restored byte for byte). Ledger: `stampOrigin` restored to return early on a named session; the reader reading a fleet job id
  or node as local, counting a served row as the asker's own, ignoring the run class filter, counting markers and inner rows as jobs, dividing by
  zero for an idle session, reading a row without `cards_tokens` as free, ignoring `--since`, reversing the order, merging unattributed rows into a
  named session, the parent index or the class index built from the rows inside `--since` instead of the whole ledger; `CleanOrigin` without its cap; `CleanClass` accepting `production`; `ClassOfSession` naming no class. Sender: the origin header
  not sent, the class header not sent, either value not cleaned, the environment not read. Queue: the stored job dropping the origin or the class,
  the submit handler storing either uncleaned, the origin key written when empty. Core: `Request` or `Meta` carrying the session on the wire. Node
  (run on the remote gate runner): the stamp deleted on the push door and on the claim loop, the request always trusted, the bearer not checked, a
  session cut to length tenant-style, a class-carrying label kept for an untrusted caller, the class not cleaned, the queued session or class not
  cleaned again, the configured-token guard of the trust check deleted (an empty bearer then matches a node with no token), the token-gated arm of
  the trust check made unreachable. Pipeline: the copy into `Meta` deleted, `entryFrom` dropping the session or the class. Delegate: a measurement
  pin sending no class, overriding a process class, and the queue lane writing a row on the asker's ledger. `ledger`: `--session` not narrowing, not
  implying `--by-session`, the array emitted unconditionally, the key losing `omitempty`.

### Added - A video, animation or audio call can name the one fleet node that runs it, and a node publishes the family rows of those lanes

- **What changed.** `offload_generate_video`, `offload_animate_character` and `offload_generate_audio` (CLI `--node` on `generate-video`,
  `animate-character`, `generate-audio`) take `node`: a node id (the `node_id` its `/fleet/health` publishes, in any case) or one of the
  `delegate_remotes` URLs. The named node is the only candidate. A box that has the lane itself still sends the call there, no other
  node is tried and this machine's lane is not asked; `route local` with a node is refused, and so is a node on an image or a graph call made
  through the library or the CLI (the image and graph verbs have no `--node`; the image and graph MCP tools list no `node` and drop one sent to
  them like any unlisted argument, so no door advertises a parameter it would drop). A call with no `node` is placed
  exactly as before and reads no row. `meta.placement` reads `remote: named node <name>`.
- **Refused before anything is sent.** A node that cannot take the call is refused by a word in `data.refusal`, beside `node`, `lane`,
  `family`, `missing[]` and `roster[]`, before the POST, so no job exists on it. `node_unknown` (it lists the ids that answered) and
  `node_is_self` are `contract` defers. `node_lacks_route` (the lane is not served or its route is not CONFIGURED, naming the files of its
  row), `node_lacks_media_job` (input files and the door is closed), `node_lacks_model` (the row of the family the call renders lists a file
  with `bytes` -1, or no such row) and `node_below_min_vram` are `capacity` defers with `err_class` `no_node_configured`, the pairing that
  err class already has, narrowed to the node the caller chose. A node that does not answer, or holds a text lease, is an ordinary
  `capacity` defer with no err class (it may take the call later), and so is a node id that no answering node has while some roster entry
  that was dialled did not answer. A roster entry the tailnet guard refuses is never dialled and has no id, so it does not count as
  one: an id nobody has is `node_unknown` however many entries the guard refuses, and the reason names them; a URL named that is on the
  roster but that the guard refuses is `capacity` with `err_class` `no_node_configured`, with no refusal word. A named call on a machine with
  no `delegate_remotes` never reaches a roster and is the plain `config` defer, also with no refusal word. Nothing falls back to another
  node or to a local run. A named call that ran on a node whose row judged it adds
  `rendered_family` and `rendered_files` to the result.
- **The rows.** `/fleet/health` gains `media_recipes[]`, additive: one row `{lane, name, default, engine, digest, files[{role, name,
  bytes}], license, commercial_use}` per family a ComfyUI or sd.cpp binding of the node names (video: the default family always, another
  only when `videogen_families` binds it or every file is on the node, an sd.cpp family by its own name; `wan-animate2`; `acestep`), with
  `bytes` -1 for a file the node does not hold. The ComfyUI rows exist only on a node that has a ComfyUI install (`comfy_dir` is a directory
  on it), because the shipped default config binds the three scripts on every machine: a node without the install and without an sd.cpp
  video family omits the key, and one with the install lists every lane its config binds, with -1 for what it lacks. A row is a statement of
  sizes, not an identity: no fingerprint, an empty `digest`, a stat per file, memoised for a minute; ComfyUI names keep their subfolders,
  an absolute, drive-lettered or `..` name is published as its file name, and an sd.cpp file is published by its bare file name.
  Voice, the audio.cpp engines and the sd.cpp animation lane publish no row, which a reader takes as unknown: a lane the node publishes no
  row for goes to the node's own door, as a forced call does. A `model` is matched by row name exactly and otherwise folded the way the node
  folds it (`mediacap.VideoBinding`), `ace` has no row. `delegate.NodeView.MediaRecipes` decodes it.
- **`min_vram_gb` on video and animation** (`--min-vram-gb`): the caller's number filters the remote candidates of any placement that leaves
  the machine by the node's largest card, as a graph's does; a malformed value is refused by name on every route. The Wan 720p default floor
  of a graph is not applied here.
- **Why.** The standing order for the fleet is that any machine holding the model takes a video, animation or audio call. That is three
  releases of work, and naming a node needs no identity, so it comes first: a caller who knows which node has the weights can say so, and a
  node that lacks them is refused in under a health read instead of by that node's own 503 after the job was sent. D2 is recorded as
  decided in ADR 0082 Amendment 3 (these lanes overflow like images, on by default, `route local` the escape); the identity release (a
  digest of the files' content and the settings that change pixels, the busy-lane probe, one-shot overflow) and the release for tickets with
  input files are written down there and are not built here. A node that has not opened the media-job door stays as its operator left it: that is
  a `node_lacks_media_job` for a call with input files, not something this change designs around.
- **Tests, written first.** `internal/mediacap/lane_rows_test.go`, `internal/fleetnode/media_recipes_test.go`,
  `internal/delegate/nodeview_mediarecipes_test.go`, `internal/mediaremote/named_test.go`, `internal/mcpserver/medianode_test.go` and
  `main_node_cli_test.go`, committed red on behaviour-free seams before the fix. Red before: `one family bound and present, want one video row,
  got []`; `rows per lane = map[] (rows []), want one video, one animate and one music row`; `rows = []` (the delegator's decoder);
  `node "render-b": {OK:true ... "node":"render-a"` (the call went to another node); `a named node must take the call even when this machine
  has the lane: {OK:true ... Data:{"local":true}}`; `want a deferred answer, got {OK:true ...` for every refusal; `min_vram_gb 24: ok=true,
  want false`; `offload_generate_video: node advertised=false, want true`; `flag provided but not defined: -node`. On the tests as first
  committed: 21 failing tests in `internal/mediaremote`, 11 in `internal/mediacap`, 3 in `internal/fleetnode`, 1 in `internal/delegate`, 4 in
  `internal/mcpserver`, 5 in the root package. One of them (the fleetnode memo test) panicked on an empty list in its first red run; it was
  changed to fail on its assertion and its changed form run red against the same seam. **Not red first, and the history says so:** the
  test that exactly one default video row exists when the default family is sd.cpp was committed red before the fix; the pins that the
  default row is the one marked default, that an older node is not judged for a model, and that an sd.cpp family is matched by its own
  name were added green after the fix, to give three mutants a test to die on; the redaction of a URL typed with a credential was written
  with its fix in one commit (its test is red under mutant `mr33`); and the task-list half of `node_lacks_route` got its test after a mutant
  survived (`mr10`). Guards that pass before and after by design: a blank node is no node, a call with no node never reads a row, a node of an
  older build decodes to nothing, a node with no lane publishes no key.
- **Mutation checks.** 74 single-line mutants through `scripts/mutation-check.sh`, run on
  `git archive` copies of the committed tree (each landed on exactly one line, compiled, and was restored byte for byte), the last verdict of each
  `MUTATION-VERDICT: RED`; the 33 `internal/mediaremote` mutants were run again in full against the final Go code, all RED. First pass: 59 RED; 9 did not compile (`mc16`, `mr04`, `mr17`, `mr20`, `rt10`-`rt14`: a variable left unused) and 4 did
  not land because the marker matched more than one line (`mcp02`, `rt07`-`rt09`), all replaced by mutants that do; 2 survived, `mr33` because the
  run set omitted the new redaction test, and `mr10` (the task-list half of `node_lacks_route`) because no test held it, which a test now does.
  By rule. mediacap (17): the size of a missing file (-1 to 0); the default family always listed; another family only when bound or complete
  (guard removed, guard disabled; the all-files-present test disabled); an sd.cpp family replacing a ComfyUI one of its name; an absolute or
  climbing name cut to its file name; a digest on a row; the animation and music engine guards; the music and video binding gates; the license
  pair; the default flag; the sd.cpp default taking the ComfyUI default; the role prefix; an sd.cpp file published by path; the animation row
  name. fleetnode (3): the minute memo, the publication, `omitempty`. delegate (1): the decode. mediaremote (33): `auto` with a node
  bypassing a local lane; route local with a node; a node on an image or a graph call; a node that is not a string; this machine's own node
  (case-sensitive); the id and URL match (case-sensitive, URL dropped); an id nobody answered to while an entry is down; the roster list; the
  task and route refusal, the files it names, the media-job refusal, the model refusal; a lane with no row judged; the model fold, `ace`
  judged, the exact-name match, the default row, the music-only rule; `min_vram_gb` on a named node, in `refuses`, on video and animation, and
  its precheck; the text lease; `rendered_family`; the typed data, the err class and the defer class of a refusal; the named branch of
  `callWith` and the placement string; the URL redaction. mcpserver (6): the `node` and `min_vram_gb` schemas, `node` on the image tool, the two
  plumbing lines, `min_vram_gb` on the audio tool. Root (14): the two plumbing lines, the `--min-vram-gb` check, the value-flag entry of each
  of the three verbs, the three usage lines, the parsed fields of each verb.
- **Docs.** ADR 0082 Amendment 3 (the rules, the six refusals and their classes, what a row is and is not, the named limits, and the two
  releases that follow), `docs/systems/media-generation.md` ("Naming a node"), `docs/systems/fleet-node.md`, `docs/FLEET-NODE.md`, the
  glossary and the operator guide.
- **Review fixes, before the merge.** A review of the first build found, and this entry's code now fixes, in order of weight:
  (1) a roster entry the tailnet guard refuses was filed with the entries that did not answer, so a mistyped node id on a roster that
  held one was a retryable `capacity` defer ("one of them may be it") on every attempt; it is `node_unknown` now, naming the refused
  entries, and a dialled entry that did not answer still keeps the id a capacity defer; (2) every node that loaded the shipped default
  config published a default `wan22`, `wan-animate2` and `acestep` row with every file at -1, nodes with no ComfyUI at all included (the
  default binds the three scripts on every machine, and the docs and tests said a node without the lanes omits the key); the ComfyUI rows
  are now built only when `comfy_dir` is a directory on the node, and the docs say what a node with the install publishes, an image-only
  node included; (3) a named URL that is on the roster but that the guard refuses was filed under the `config` class, which no document
  named; it is `capacity` with `err_class` `no_node_configured`, as `pickNodeFor` files such an entry; (4) an sd.cpp file was published
  with the subfolders of a relative path though the contract is the bare file name; (5) the ADR called `mediacap.VideoBinding` the one
  resolver of the video family, but three folds exist and nothing tied mediacap's copy of the runner's closed set to the pipeline's: a
  pin test now does (`TestMediacapFoldsAVideoFamilyTheWayThePipelineRendersIt`, in `internal/pipeline`) and the ADR says so; (6) the CLI
  prints a refusal's word only with `--json` (the DEFERRED line carries the reason), which the operator guide and the usage text now say;
  (7) a named call on a machine with no `delegate_remotes` is the plain `config` defer with no refusal word, now stated and pinned;
  (8) two comment fixes (the doc comment of `videoFlags` had been left above another function, and `vramTask` named a graph it does not
  cover). Not changed: the image and graph MCP tools still list no `node` and drop one sent to them, a choice this entry documents and
  the design left open; no door advertises a parameter it would drop.
- **Review fixes: red first, and the mutants.** Four tests were committed first and failed on their assertions: `a refusal must carry
  typed data, got "null"` (an id unknown on a roster with a refused entry came back as a capacity defer); `a refused entry is a
  configuration fact: ... DeferClass:config ...` with an empty err class; `names = [models/sub/fastwan-q8.gguf high.gguf
  models/sub/wan-vae.safetensors t5.safetensors], want fastwan-q8.gguf,high.gguf,wan-vae.safetensors,t5.safetensors`; and `a node with
  no ComfyUI install published rows` (three rows with every file at -1), in `internal/mediacap` and in `internal/fleetnode`. **Not red
  first, and the history says so:** three pins were added green because the code already had the behaviour and no test held it: the
  four weight roles of an sd.cpp row (dropping the high-noise expert, or the T5, was SURVIVED on the tree before this fix, so the whole earlier suite missed it),
  a named call on a machine with no fleet, and the cross-package family fold; each was then proved by a mutant. 17 more single-line
  mutants through `scripts/mutation-check.sh` on a `git archive` copy of the committed tree, each landed on one line, compiled, was
  restored byte for byte and ended RED: mediacap (9), the four sd.cpp roles one by one, the bare-name `path.Base`, the ComfyUI gate
  (its definition, and its use on the video, the animation and the music lane); mediaremote (6), refused entries filed with the
  silent ones, the refused entries left out of the reason, the class and the err class of a named URL the guard refuses, the class of
  the no-fleet defer, a named call with no fleet running here; pipeline (2), `h3` removed from mediacap's closed set and `hunyuan`
  removed from the pipeline's.

### Added - An overdue lease's holder can be asked to yield (`gpu_on_overdue = request`): it runs the job's own `on_yield`, and nothing is killed, released or taken

- **The gap.** A lease that outlives its declared window while its holder is alive is surfaced as `held-overdue` and nothing else
  happens to it. Somebody waits for the card, the holder is overdue (its term was not renewed, or it reached the maximum total), and
  the only recourse was a human running `gpu release --epoch N`. For a wrapper that command is a hazard: the claim goes, the
  wrapper's next tick prints `LEASE LOST` and stops its own job within about 15 seconds while the card is already free and a second
  job can be loading onto it. For a detached job it frees the claim and leaves the job running on the card. Jobs already record, at
  the start, what they would like to happen when asked to stop (`--on-yield`, `--yield-grace`), and until now nothing read it.
- **The policy.** One flat key, `gpu_on_overdue`. **Unset keeps today's behaviour: nobody is asked.** `request` asks the lease's
  holder. `hold` says in words that nobody is. The value is trimmed and lower-cased; any other (`yield`, `kill`, `takeover`, a
  typo) is refused by name when the config loads, and, because a one-shot verb goes on after a load error and the load installs
  the policy on every exit, it is installed as `hold`, the direction that asks nobody; the holder's first tick says why no request
  is made. **There is no automatic mode:** no timer, no daemon and no waiter acts on a lease; only the holder's own tick can ask. The
  plan names `request` as the default; the operator decided that asking is opt-in, so the default stays what it was (ADR 0086 records
  the deviation).
- **Who asks, and when.** The holder's own tick (the wrapper of `gpu reserve`, and the detached holder of `gpu reserve --detach`),
  not a blocked waiter. The preconditions are checked cheapest first, from the lease record alone: the policy is `request`; the
  record is labelled expired or is past its declared end (a lease the same tick renewed is not asked); the holder was not asked
  yet (an in-memory flag, so a vanished file cannot make it ask twice); the record says this holder runs the request; the lease has
  read overdue, on every tick, for the holder's settle interval (below); and only then are the waiters read. Demand is a live waiter that is not the holder's own process, not waiting only on host RAM, and wants a
  card the lease holds. **The same waiter (pid and start identity) must be seen on two consecutive ticks**, so a short probe
  that comes and goes cannot stop a long job through its `on_yield`; the persistence is an in-memory counter on the holder's own
  cadence and adds no time constant. **A label has to have survived a look.** The term check looks again at a lease it labelled
  expired once a minute, and a look at the cards that fails (most likely when they are busiest) labels a lease that a second look
  would renew; the ask cannot be taken back, so both holders settle for that same interval (the term check's own
  `termRecheckEvery`, handed to the ticker, not a new constant) before they ask, and the waiter is counted from the first tick
  after it. A lease the look renews is never asked, and a renewed lease that is labelled again settles again.
- **The request.** A sidecar beside the lease record, `yield.<epoch>`: when it was asked, by which waiter pid, why (the basis in
  words), up to three waiters and the queue length, the policy, the job's declared grace, and a second write of what the holder
  did about it (`on_yield running`, `ran on_yield`, `on_yield failed` with the exit status, or `no on_yield recorded`). It is
  written under the epoch lock after the writability probe and the liveness check the orphan marker uses, with tmp and rename; both
  the ask and the later action update check the lease is still live, so a late update cannot bring the file back after a release;
  both sweeps that end a lease remove it. A binary that predates the request ignores it (the preserved old-reader fixture pins that).
  If the ask cannot be recorded nothing is run, and the holder says so once. What the holder did about it is written the same way;
  a write that fails is said once (`could not record what its --on-yield did`) and tried again on the next heartbeat, with the
  time of the event, and a request file that exists and cannot be read is an error, not "never asked".
- **What the holder does.** It runs the `--on-yield` it was STARTED with, from memory, never the line the record holds (the record is
  shared and can be rewritten), once, in its own working directory, through `sh -c` (or `cmd.exe /d /s /c` hidden on Windows), with
  its output discarded (a wrapper's stdout is the job's), in a goroutine so the heartbeat goes on, and never waited out or cut short.
  The wrapper's command is told `GPU_LEASE_YIELD_FILE`, the path of the sidecar (which exists only once the holder is asked), so a
  job can poll it between items; the line itself gets `GPU_LEASE_EPOCH`, `GPU_LEASE_DIR` and `GPU_LEASE_YIELD_FILE`. A detached job is
  not the holder's child, so under the request policy `gpu reserve --detach` prints the path (and `yield_file` under `--json`) and
  says whether the holder runs an `--on-yield` or none was recorded, and `gpu status --json` carries it; under the default, `hold`
  and an unrecognised value nobody is asked, and both are what they were before the key existed (a status call names the file once
  a holder was asked, whatever the policy says by then). A waiter's reason is free text typed by any local process, so line breaks
  and control characters in it become spaces before it reaches the holder's one-line stream. **A detached holder never returns on a yield:** its job is a separate process, and releasing would free the card under a live
  job (the 2026-09-07 incident). A holder started with `--release-at-expiry` stamps nothing and is never asked.
- **A record gains an additive `yields` stamp** (`wrapper` or `detached`), set at acquire by a holder that runs the request. The
  version cannot say whether a holder would answer (none of the older builds ticks this), so a record without the stamp is never
  asked and reads, on every surface, as "its holder does not run the yield request (a build from before it, or an in-process holder
  such as the media pipeline's): ask whoever owns it". Two kinds of holder carry no stamp: every job started before the upgrade, and
  the leases this build's own media pipeline takes in process, which never tick the request.
- **What a waiter and a status call read.** Read-only everywhere (a test pins that no status surface writes the sidecar):
  `ExplainHeld` (the refusal text of a blocked request), `gpu status` and its `holder.yield` object (asked at, waiters, grace, the
  action, the exit status, whether the ask is unanswered), the one-line brief, and the overdue, stalled and orphaned notes all say
  what became of the request. **No sentence aimed at a waiter names `gpu release --epoch N` for a lease whose wrapper is alive.** For a
  wrapper the words are: nothing is killed; ask whoever owns it, or stop its job by hand (the wrapper's pid); the claim frees when
  the wrapper ends. Only a detached holder's sentence names the release command, with what it does: it frees the claim at once, and
  the job keeps running on the card until it ends. An ask is called unanswered once the lease still holds its cards past the job's
  declared grace, or past a display-only ten minutes when the job declared none; nothing keys off that figure, which is chosen and
  not measured. A lease that was asked and then renewed (the sidecar stays until the lease ends) is no longer overdue and is never
  called unanswered: it reads "has since been renewed and is not overdue now".
- **Not changed, on purpose.** Nothing is killed, signalled, released or taken over: a static test reads the source of the yield files
  and fails on a kill, a signal, a process-tree stop, `taskkill`, `os.FindProcess` or a second process start. A claim whose holder
  is alive is never freed by the harness. The term check still renews or labels and never frees. No `gpu yield` or `gpu takeover` verb
  is added, and plan P12 (claim-until-dead routing, `--abandon-tree`, quarantine) is not part of this change.
- **Limits, stated.** (1) A hung job that ignores the request keeps its card: the lease stays `held-overdue` with its heartbeat and
  fence intact, and only its holder ending or a human acting frees it. (2) A progressing job is not asked: under the term rules a
  renewal moves the declared end, so only a lease labelled expired or one that reached `gpu_max_total_min` is asked. (3) The request is
  node-local: the fleet node's lease standing wire does not carry it. (4) A seat waiter registered for the whole node (the card table
  was unreadable when it queued) conflicts with every lease and can ask an expired lease on a card it does not need; the two-tick
  rule bounds that. (5) A holder started before the upgrade carries no stamp and is never asked, so a host is covered only once
  its `gpu reserve` and `gpu hold` holders are this build; restart the long-lived processes so their surfaces say the same thing. The
  leases this build's own media pipeline takes in process never carry the stamp and are never asked, before or after a restart.
  (6) A holder whose command ends before its `on_yield` returns leaves the action at `on_yield running`; the file goes with the
  lease. (7) The earliest ask comes about a minute and a half after the term ended: a label is asked about only after it survived
  the term check's second look, and the waiter must then stay two ticks.
- **Record and docs.** ADR 0086 (status Proposed) amends plan invariant I5 and the plan's description of the takeover: here the holder's
  own tick runs the job's own pre-declared `on_yield`, instead of a takeover command running it. The file-only alternative (the holder
  writes the sidecar and the job polls it) is a one-line change in the ticker, and the choice between the two is the operator's to
  confirm. `docs/systems/gpu-lease.md` has the new section, the corrected `on_yield` / `yield_grace` rows (they were described as
  read by the takeover command), the `yields` row, the source map and the held-overdue verdict row; `docs/OPERATOR-GUIDE.md` has the
  sentence; `--on-yield` and `--yield-grace` help say who runs the line; `config.example.json` carries the key (the generator
  and the file agree).
- **Red first, then proofs.** A seam commit added only the types and signatures (the policy parser returning the unset value, the
  asker never asking, the view empty, the shell runner doing nothing); the tests were committed next and failed on their
  assertions, then the code followed. Recorded failures: `ParseOnOverdue("request") = "", <nil>; want "request"`, `ParseOnOverdue("kill")
  accepted a value that is neither request nor hold`, `the holder of an overdue lease with a waiter on its cards was not asked after
  two ticks`, `installed "kill" reads "", want hold`; in `internal/config` `request: Load installed "", want "request"` and `"yield"
  was accepted: there is no automatic mode, and a typo must not pass`; in `internal/gpuactivity` `yield_file = "", want ...`, `note lacks
  "was asked to yield"` and `note lacks "gpu release --epoch 41"`; in the root package `on_yield ran 0 times, want exactly once`,
  `a detached holder stamps its form: ""`, `the report lacks "release with: local-offload gpu release --epoch 9"`, `--on-yield help lacks
  "gpu_on_overdue"` and `exit 0, err <nil>: want the line's own status 3 and no error`; and in `internal/mcpserver`
  `the assessed note carries the yield sentence verbatim`. Two fix-ups of the tests themselves came after the red run and are separate commits with their reasons (the wrapper
  was queued behind the waiter the test planted too early; the policy has to come from the config file the verb loads, because every
  `gpu` verb re-installs it). **Tests that were NOT red first, and the history says so.** Four absence assertions passed on the behaviour-free seam, as an
  assertion that nothing happens must: `TestTickerSilentUnderTheDefaultPolicy`, `TestStatusNeverWritesTheYieldSidecar`,
  `TestGPUOnOverdueUnsetKeepsTheBehaviourFromBeforeTheKey` and `TestDetachHolderThatReleasesAtExpiryRunsNoRequest`. Four more were
  written after the red commit and passed on their first run against code that already did the right thing:
  `TestAskedOnceEvenIfTheSidecarVanishes`, `TestATreeOrphanHasNoHolderToAsk`,
  `TestGPUReserveUnderAnInvalidPolicyStillRunsAndAsksNobody`, and the strengthening of `TestLateActionAfterReleaseLeavesNoFile`
  (it re-plants the file after the release, because the first version passed even with the liveness check removed). Each of the eight
  is proven by a named mutation that makes the forbidden thing happen, below.
  Sixty-six single-line mutations against the final tree, each run through `scripts/mutation-check.sh` (it proves the line landed alone,
  that the unmutated tree passes first, that the mutant compiles, and that the file is restored byte for byte), and each `RED`: **in
  `internal/gpulease` (37)** a lease still inside its window asked, a renewed lease asked, a waiter on another card counted as
  demand, a host-RAM-only waiter counted, the holder's own pid counted, an unset policy asking (twice: by the sidecar tests and by the
  ticker test), a hold or unrecognised policy asking, the ask repeated when its file vanished, two askers both writing, a holder
  without the stamp asked, the two-tick rule dropped, the ask touching the claim, each of the two release sweeps leaving the
  sidecar behind, a late action update bringing the file back, an ask made when it could not be recorded, the record's `on_yield`
  run instead of the holder's own, `on_yield` run on every tick, run in line with the heartbeat, a failure not recorded, a missing
  `on_yield` not said, the unrecognised-policy note said every tick, the failure-to-record note said every tick, a kill call added to
  the request files (the source pin), the refusal text of a blocked request without the sentence, a live wrapper's sentence
  naming `gpu release`, a detached holder's sentence naming it without what it does, the unanswered threshold ignoring the
  declared grace and then the display figure, a holder without the stamp not said to predate the request, the hold policy not said,
  the stamp not written to the record, not mirrored by `Info`, the parser accepting a value that is neither `request` nor `hold`, an
  unrecognised value installed as unset, the value not trimmed and lower-cased, and an old reader tripping on the new field and
  sidecar; **in `internal/config` (6)** the install behind another key's refusal, `Load` not installing, an unknown value not
  refused by name, an unset key reading as `request` (by the accessor and by a stale earlier load), and the accessor not falling back
  to `hold`; **in `internal/gpuactivity` and `internal/mcpserver` (7)** the holder without its yield file or sentence, the standing
  notes without the sentence, `holder.yield` absent once asked, its `unanswered` flag, a tree-orphan reported as awaiting an answer,
  the brief clipped over the sentence, and a status snapshot that writes the request; **in `internal/winexec` (1)** the runner
  started through a raw `exec.Command`; **in the root package (15)** the wrapper's command not told the yield file, the wrapper not
  ticking, the detached holder not ticking, the detached holder returning on a yield, the wrapper running the record's `on_yield`,
  the wrapper not stamping its form, `--release-at-expiry` stamping, the detached form not stamped, a Windows command line that
  mangles a quoted path, the lease environment missing, the line's exit status lost, the detach report (text and `--json`) without
  the yield file, the help text not saying who runs the line, and a verb under an unrecognised `gpu_on_overdue` falling back to
  `request`. First attempts that did not count: one mutant survived (the liveness check in the late action update was redundant
  while the release sweep had already run, so the test was strengthened, see above), and three first attempts did not compile
  (BAD-MUTANT) and were replaced by mutants that build; none of those is in the sixty-six. One redundancy was left alone on purpose: the unlocked stat
  before the epoch lock in the asker is only a shortcut, and the in-lock re-check is what is pinned.
- **Second pass, before it shipped.** A review of the first version found six defects in behaviour and wording, and tests that proved
  less than they claimed. All are fixed, each with its test first. (1) An ask that a renewal had since answered was still called
  unanswered, on every surface and on `holder.yield.unanswered`: it needs the lease to be overdue too, and reads "has since been
  renewed". (2) A lease labelled on one tick could be asked about on the second, 15 seconds later, before the term check had
  looked at the label again (a look at the cards that fails labels a lease that a second look renews): the holders now settle for
  the term check's own recheck interval. (3) The three updates that record what the holder did discarded their errors, so a
  sidecar could say "started its on_yield, which has not ended" for the life of a lease about a command that had finished: a
  failed record is now said once and retried on the next heartbeat, with the time of the event. (4) A waiter's free-text reason
  could split the holder's one-line stream or inject a terminal sequence into it: it is collapsed to one line. (5) A holder that
  does not run the request was said to "predate" it, which is false of the media pipeline's in-process leases, and the rollout
  sentence promised coverage that those leases can never have: the words now name the cause, and the rollout is narrowed to the
  `gpu reserve` and `gpu hold` holders. (6) The default policy still printed, on `gpu reserve --detach` and on the status wire, that
  a holder would run its `--on-yield`: the detach report and `yield_file` now appear only under `gpu_on_overdue = request` (or once
  a holder was asked), and say there is nothing to run when no `--on-yield` was recorded. The status test that planted a waiter with
  a pid nobody has (pruned as dead, so no demand ever existed) now plants a live one and shows the holder's own asker asks on that
  same fixture.
  **Red first, the second round.** Tests committed before the fixes and failing on their assertions: `a lease that is not overdue is
  not an ask nobody answered`, `asked 15 s after the label, before the holder's term check looked at it again`, `a lease the recheck
  renewed was asked`, `the holder says once that the result of its on_yield could not be recorded, got 0`, `an unreadable request
  must be an error naming it, got <nil>`, `a clean ask is one line on the wrapping session's stream, got 2`, `the sentence must say
  "does not run the yield request"` and `the default policy asks nobody and so advertises no yield file`; in the root package
  `the holder was asked about a label that had not survived a recheck`, `a lease with no --on-yield: the holder runs nothing and the
  report says so` and `gpu_on_overdue = "" asks nobody, and the report still talks of a yield request`. **Tests that were NOT red first,
  proven by mutation instead:** the sidecar saying `on_yield running` while the command runs, the sentences for a failed command and
  for one that cannot be started (exit -1), the waiters being read last (a dead waiter's record survives every tick that stops
  earlier), the in-lock liveness check on the first write, the line's output never reaching the holder's streams, a shell that cannot
  start being an error, the settle interval starting again for a renewed lease that is labelled again, the retried record carrying
  the time of the event, the failed-look scenario of the settle rule, the live-waiter status test, and a test that a renewable
  lease is never asked whichever of the holder's two checks runs first within a tick (the order is immaterial, because the asker never
  asks on the first tick a lease reads overdue).
  **Thirty more single-line mutations**, each run through `scripts/mutation-check.sh` and each `RED`: **in `internal/gpulease` (22)**
  an unanswered ask by time alone, the sentence never learning the lease was renewed, the old "predates" wording, the settle interval
  dropped (against the silent-progress label and against the failed-look label), the settle interval not starting again, a failed
  record not said, a failed record not retried, the running action not written, an unreadable request file not an error, a retry
  stamped with the time of the retry, the waiter's reason raw on the stream, control characters surviving, the waiters read before the
  policy, the window, the once-flag, the form stamp and the settle interval (five), the ask not re-checking liveness under the lock,
  a command that could not start not recorded as exit -1, and the running and failed sentences dropped; **in `internal/gpuactivity`
  (3)** the default policy advertising the yield file, a request that was made not named once the policy changed, and a status
  snapshot that ticks the real asker; **in the root package (5)** the holders passing no settle interval, the detach report
  promising a request under every policy, the detach report claiming a command that was not recorded, the helper's output reaching
  the holder's streams (on a Linux host and a Windows host), and a shell that cannot start reported as success (both hosts).

### Added - a test refuses a speed term in any contract's acceptance array and pins the acceptance verb set as clock-free (H-03, INV-5)

- **The rule and the gap.** INV-5 and ADR 0050 say quality outranks speed and that "Acceptance criteria and pass rules stay
  wall-time-free". Nothing in the repository enforced that for the shipped contracts: `TestContractFixturesAreWellFormed` reads
  three of the twelve JSON files under `contracts/`, and the four templates, the four write-door contracts and the node list were
  parsed by no test. The eleven files that hold an `acceptance` key carry 87 strings and none has a speed term today (a scan of the
  raw text and of the text with regex escapes undone), so this is a tripwire for the next edit, not the fix of a live violation. The
  out-of-repo scan that did exist (a lint of the operator's measurement drivers and plan register) captured an `acceptance` array up
  to its first `]`, so `["regex:[0-9]", "regex:within 90 s"]` showed it only `regex:[0-9` and the speed term went unseen.
- **The gate.** One new root-package test file, `contracts_speedterm_test.go`; tests and docs only, no internal package is touched
  and no node runs different code. `TestContractAcceptanceCarriesNoSpeedTerm` walks every `*.json` under `contracts/` with a real
  JSON parse and reads the key `acceptance` at any depth and in any case (a single template, a top-level array of contracts, an object
  with a nested `subtasks` list; `parseContractFile` decodes with `encoding/json`, which fills the field from `Acceptance` and
  `ACCEPTANCE` too, so those are live lists and a gate that compared the key exactly would not see them). It fails on an `acceptance` value that is
  not a list of strings, on a string `core.ParseAcceptanceCheck` does not
  parse (the closed DSL has no time verb, so a pseudo-check such as `wall_ms:1500` is refused, and the parse coverage of the shipped
  contracts goes from three files to all of them), on a string that carries a speed term, and on a file that is not valid JSON. Only the
  key `acceptance` is read: ADR 0047's "No speed term enters the choice", quoted in a context text, stays legal. Non-vacuity: the
  scanner must have read exactly the files that a separate, case-insensitive byte search for `"acceptance":` finds, and at least one
  string, so a walk that reads nothing cannot pass.
- **The pattern.** `speedTermPattern` is the pattern of the out-of-repo classifier, compiled under RE2 (the 30 vectors of
  `TestSpeedTermVectors`, 16 positive and 14 negative, give the same result under Python's engine): `slower`, `faster`, `speedup`, `tok/s >`, `wall <= 1.5x`, `<= 1.5x
  the`, `2x the K=1`, and a bare figure with a unit after `near`, `within`, `under`, `below` or `at most`. A `regex:` check is also read
  with its escapes undone (`wall\s*<=\s*1\.5x` is `wall <= 1.5x`), so the escaped spellings do not hide. That is best effort and the
  README says so: a figure spelled `\d+`, `[0-9]+` or `(?:a|b)` evades it. The literal is a raw string so the out-of-repo lint can read
  it and fail when the two drift ("change both or neither").
- **The verb pin, which does not depend on spelling.** `TestAcceptanceVerbSetIsReviewed` reads `internal/core/agentwire.go` with
  `go/parser`: the constants typed `AcceptanceKind` and the case labels of `ParseAcceptanceCheck`'s switch (a constant name, a string
  literal or an `AcceptanceKind("x")` conversion) must equal a reviewed set of seven verbs that gives each one the reason it reads no
  clock, and so must the verb list in the unknown-verb message. Names a clock verb would carry (`wall`, `wall_ms`, `max_wall`,
  `latency`, `ttft`, `tok_s`, `within`, `under`, `faster`, `slower`, `elapsed`, `duration`) must be refused as unknown. A label the
  reader cannot resolve is an error, never a skipped verb. A change that adds a verb must add it to `reviewedAcceptanceVerbs` with its
  reason. This replaces parsing the hand-kept error message alone (which is already stale in one place and would pass a verb added
  without updating it).
- **Not done, on purpose.** `core.ParseAcceptanceCheck` does not reject speed words (that would change what a node accepts when it
  ACKs a contract, and rolling-upgrade compatibility with it); `delegate.LintAcceptance` gains no category (its comment says it is kept in
  lockstep with a classifier outside the repository, so a new category would double the surface); there is no allowlist for a content
  check that quotes a speed phrase: it anchors on the figure instead (`regex:90`), which `contracts/README.md` now says.
- **Docs.** `contracts/README.md` states the rule beside the DSL table (no verb reads a clock, what the two tests refuse, that the scan
  is a lint and the closed verb set is the real defence) and in the paragraph on `TestContractFixturesAreWellFormed`.
- **Tests, red first.** A seam commit adds the pattern, the finding type and four helpers that return nothing; a test-only commit
  follows and fails on its assertions (`speedTermIn("contains:faster") = ("", false), want a speed term` for all 16 positive vectors;
  `got 0 findings, want 1` for each fixture; `the scanner read [], the byte search finds an acceptance key in 11 files`;
  `AcceptanceKind constants = [], reviewed = [contains, diff_max_files, ...]`). Four subtests were green from the start by design: a
  file with no `acceptance` key is not counted, only `.json` files are read, the unknown-verb message lists the reviewed set, and
  clock-shaped names are refused (they pin behaviour that already held). The fix commit fills the helpers. A test-only commit
  found while choosing mutants: the scanner and the byte search shared one extension-filter line, so it differs now; the scanner no
  longer sorts a list nothing observed; and a parser with no switch on the kind got its fixture. A review round then found the key was
  compared exactly while the production reader matches it without regard to case: four fixtures (`Acceptance`, an upper-case key in a
  nested subtask, both spellings in one object, a capitalised bare string) failed on `got 0 findings [], want 1` and two byte-search
  cases on `filesWithAcceptanceKey = "", want "c.json"`; `strings.EqualFold` and `(?i)` fixed them, and a premise test that runs the
  real `parseContractFile` on three spellings (green from the start by design) records why.
- **Mutation proofs (39, each one changed line, `scripts/mutation-check.sh`, all RED with the control arm green and the file restored).**
  Pattern: `speed-?up` dropped, `near` dropped, the `tok/s` alternative, the `wall <= 1.5x` alternative, the `<= 1.5x the` alternative,
  the `2x the K=1` alternative, the trailing word boundary of the time term, the trailing boundary of the first group. De-escape: the
  regex argument scanned as written, `\s+` not read, `(?i)` not dropped, `\.` not read. Scanner: nested objects not walked, nested
  arrays not walked, the parse step off, the speed step off, a bare-string `acceptance` not refused, a non-string element
  stringified, the byte order mark not trimmed, invalid JSON skipped, non-`.json` files read, keys other than `acceptance` read, the key
  compared exactly, the byte search without `(?i)`, the live
  walk pointed at the wrong directory, subdirectories skipped, files never counted. Source reader: constants not collected,
  conversion labels unread, an unresolved label accepted, a missing parser function tolerated, a missing switch tolerated. Verb pin on
  the real parser: a literal `case ..., "budget_ms":` (the case arm only), a declared constant with no case (the constant arm only), a
  clock name rewritten before the parse (the name probe only), a verb dropped from the error message (the message arm only). Live data:
  `regex:within 90 s` in a template, `regex:near 22 s` in a nested write-door contract, `wall_ms:1500` in a template.
- **Outside this repository.** The operator-side speed-term lint is now a hard gate on the same pattern (an untagged hit, an
  `acceptance` list with a speed term read with a real JSON walk, or a drift from `speedTermPattern` fails the session-start audit, and
  a surface that scanned zero files, a file it cannot read, or an acceptance directory in which it met no `acceptance` key is
  unchecked, not clean; the key is matched in any case there too). Nothing in this release's code depends on it.

### Added - Every tool result of a stale MCP server carries the stale-binary notice, a stale server's read no longer blocks a rollback, and the deploy report lists the sessions still on an old image

- **The gap.** An MCP server keeps the executable image it started from, so a rename-swap deploy leaves every session that was
  open before it on the old build. The stale-binary guard told a session on three doors only (`agent_delegate`,
  `offload_research`, `offload_status`); a session that called `offload_summarize`, `offload_ask`, a media door or any other was
  never told, and went on stamping ledger rows with old behaviour for as long as its client stayed open.
- **The notice.** `staleNotice` (`internal/mcpserver/stale_notice.go`), a receiving middleware registered after the config gate,
  appends the very note `offload_status` carries under `stale_binary` as a second text block (`stale_binary: <note>`) to the
  result of every `tools/call` that does not carry the signal already, on every call (no cap, no config key), with `Content[0]`
  untouched so the door's JSON still parses for a client that reads only the first block. The decision is the result's, not
  the door's name: the three doors that carry the key in their own answer (`agent_delegate`, `offload_research`,
  `offload_status`) do so only on the path that ran the work, so a first block that is a JSON object holding `stale_binary` or
  `stale_binary_guard` at its top level is left alone with no check, and every other result gets the block, those three doors'
  refusals (bad arguments, no subtask, no url, an unknown status section) and the config gate's deferral included. A summary
  that merely quotes the key is still told. A test reads the call sites in `mcpserver.go` and keeps the list of the keyed doors
  in step, the result is copied before the block is appended (a handler may keep what it returned), and a failed call, a result
  with no content and a typed-nil result pass through as they are. A guard that is not watching rides the same way as `stale_binary_guard: ...`, on the first call that
  finds it so and then on every 20th, because that is a standing condition of the process and not news. The note now names
  `/mcp` (select `local-offload`, Reconnect) beside the restart, the way out that works for a stdio server the client does not
  reconnect by itself. **Reach:** a server that started before this release has no middleware, so only a reconnect or a restart
  of the client puts it on this one; the first real proof is two deploys away.
- **The read.** The installed executable is hashed, and its build marker read, through `gpulease.OpenShared`, which grants
  `FILE_SHARE_DELETE` on Windows (Go's `os.Open` grants only read and write sharing); the audit's own opener (`openAudited`, which
  `gpu doctor` reads every backup through) is the same one. With the notice on every door, the first call of any door on a stale
  server reads the freshly installed file, and a read that did not share delete made a rollback or a second swap landing in
  that window fail on a sharing violation, with node-swap's holder diagnosis unable to see the stale server (its image path
  had been renamed away).
- **The record.** ADR 0085 (status Proposed) records the decision: the stale server is told, not replaced; a stable stdio shim
  and a loopback HTTP endpoint the client reconnects by itself were weighed and neither is built, with the Windows rename and
  lock facts, an acceptance matrix and a go/no-go gate (two deploys with sessions still stale after a day despite the notice).
  `docs/systems/mcp-server.md` documents the notice, the shared read and the rule for counting a rollout: only ledger rows whose
  `harness_build_sha256` equals the deployed build's hash. The operator's deploy script also reports, at the end of each Windows
  leg and in its deploy record, the MCP servers it finds on an image older than the deployed build (pid, start time, session
  when the registry names it); that report is information only, it never changes a node's verdict or stops a process, a check
  that could not run says "not counted" and never 0, a Linux leg says "not probed", and a dry run says what a swap would leave.
  It compares start times with the swap's own time (the `install-new` stamp node-swap writes, so a server started while the
  node waited for idle, before the swap, is still told apart); only a result without that stamp falls back to the preflight
  clock, and the report line then says its start-time check is partial. A deploy record's `summary.json` is written even if
  the report's own file is unreadable (the field is null). The script lives outside this repository.
- **Not changed.** Nothing is interrupted and nothing is killed. No tool is added or removed (`tools/list` is byte-identical on a
  stale server, pinned), and a client that reads only the first content block sees exactly what it saw before. How each client
  renders a second block is for the release's live check to record; it is not assumed.
- **Red first, then proofs.** The tests were committed before the code and failed on their assertions, in two steps. The build:
  eleven in `internal/mcpserver` (every other door answered with one block after a swap, for 28 doors; no notice on every call;
  the error result, the shared result, the config deferral, the unreadable stamp, the guard cadence, the verbatim note, the
  stalled read and the `/mcp` wording all missing), and on Windows two in `internal/gpulease` (the installed image could not be
  renamed aside while a reader held it open, through `OpenShared` and through the audit's opener). The review of the build found
  that the three keyed doors answered a stale server with no signal on every refusal before the line that adds the key and on
  the config gate's deferral, because the middleware skipped them by name; its tests failed first (the eight early returns of
  the three doors and the invalid-config deferral of two of them answered with one block where two were wanted, and a result
  that carries the key at its top level was changed under another door's name), then the middleware was made to decide from
  the result. The proofs: twenty-three single-line mutations on the final code, each run through `scripts/mutation-check.sh`
  and each killed (RED): the registration dropped; the registration made conditional on a valid config; the key check removed;
  its cheap test misspelled; the guard key alone not recognised; a mere mention of the key counted as the signal; a first block
  that is not JSON counted as the signal; the old skip by name put back for one door; the stale condition removed; the notice
  capped at the first three calls; the typed-nil guard removed; the no-content guard removed; the result appended to in place
  instead of copied; the error flag lost in the copy; the guard block on every call; the guard block dropped; the note not
  verbatim; the wording without `/mcp`; a note that ranks two builds of one version; the share-delete bit removed; the audit
  opener back to `os.Open`; the installed-image opener back to `os.Open`; the hash opening the file directly. The Windows-only
  control (a plain `os.Open` blocks the rename that `OpenShared` allows) tests the platform and has no mutant in the code; as a
  test it dies when its opener is swapped.
- **The deploy script (outside this repository).** Its mock tests were written against a behaviour-free seam of the old
  script first and failed on their assertions (34), then passed against the edited one together with the whole existing mock
  suite and the script's own `--selftest`, which also runs the snippet for real against a hidden dummy MCP server (listed by
  image path, by start time and by neither; the session joined from an owners registry; a bad swap time answers `ok=false`).
  The review round added tests that failed first (nine assertions: the swap time that rides, its classifier, the fallback
  caveat, and a deploy record whose stale-mcp file is not JSON) and ran `write_record` for real, which no test had executed.
  Twenty single-line mutations of the final script, each killed: the swap time reverted to the preflight clock on either leg;
  the classifier reading node-swap's start, the wrong step, or leaving the fraction of a second in; the fallback caveat
  dropped or always off on either leg; no swap time passed on either leg; a dry run handed a swap time; either leg's report
  call dropped; either leg counting after a swap that did not complete; the record's merge raising on a bad file, writing
  nothing, or keyed on the wrong name; the record's builder writing an empty object; the snippet's date parser narrowed to the
  old offset-less format (killed by the `--selftest` lines that hand real PowerShell a stamp with an offset and one in UTC).

### Fixed - a kept or warm ComfyUI frees its own models and caches after 300 s idle, from inside the engine

- **The defect.** The rule is that no idle model stays loaded: every engine unloads after 5 minutes. llama-swap does it with its own
  `ttl`; ComfyUI has no idle timer (its parser takes no idle or timeout argument), so a ComfyUI that nothing frees holds what it
  keeps. The exposure is narrower than "a kept instance lives to the end of its lease" suggests, in two ways. Every runner POSTs
  `/free` at the end of its job, kept or not (`render/gpu-lock.mjs`), the Go side frees after every run, and the holder of the lease
  stops a kept instance at release. And the harness launches every ComfyUI with `--disable-smart-memory`, with which ComfyUI itself
  calls `unload_all_models()` when a prompt ends, on success and on a handled error (`execution.py` after the node loop; read in
  the ComfyUI checkout of the reference box, its version in `docs/systems/media-generation.md`), so a kept single-prompt instance (`--cache-none`) is already bare between jobs and the pack finds nothing to free
  there (it does not fire and logs nothing). What survives is (a) a warm (`--batch`) session's executor cache, which keeps the
  loader outputs (the weights the unload moved to host RAM) until a free resets the executor, so a runner killed before its cleanup
  (a tree-kill skips it) or a session that stalls holds host RAM until the lease ends, and (b) a hand-started instance run without
  `--disable-smart-memory`, which keeps its models in VRAM after a prompt. Both are read from ComfyUI's code and a fake queue, not
  yet seen in a live ComfyUI.
- **The fix, in the engine.** A kept (`--keep-comfy`) or warm launch now carries a harness-owned pack,
  `render/comfyui-ext/idle-nodes/offload_idle`, standard library only. It exports an empty `NODE_CLASS_MAPPINGS`, because ComfyUI
  reports a folder with neither that nor `comfy_entrypoint` as a custom node that failed to import. One daemon thread polls every
  5 s. A poll is busy when a prompt is queued or running (`get_tasks_remaining`) or `task_counter` moved (a prompt that began and
  ended between two polls); both are read under the queue's mutex. After 300 s of polls that were not busy, if the engine holds
  something (a loaded model, or the executor cache is on, which is the warm case), it sets `unload_models` and `free_memory`, the two
  flags `POST /free` sets, in ONE hold of the mutex after re-checking that nothing is queued and no prompt ran, once per busy period.
  ComfyUI's own worker thread drains them (unload all models, reset the executor, collect garbage); the pack never touches a model. A
  prompt that lands in the microsecond after the check runs first and the flags drain after it: one avoidable reload, never a
  corrupted run. The pack never raises: a failure is logged once as `[offload_idle] NOT ACTIVE` and the next poll retries, and an
  engine it cannot ask what it holds is freed anyway (a `/free` is always safe). It serves `GET /offload/idle` (`limit_sec`,
  `idle_sec`, `armed`, `freed`, `last_free_unix`, `error`), registered at import, which ComfyUI serves because custom nodes import
  before `add_routes`. This is the llama-swap `ttl` analogue: the clock belongs to the engine, the harness runs no timer or watcher
  (plan I5) and acts on no other process.
- **The fix, launcher** (`render/comfy-lifecycle.mjs`).
  - `resolveIdleBound`: `COMFY_IDLE_FREE_SEC` unset is 300, `0` or `off` is none (one `COMFY-IDLE-OFF` line), a whole number of seconds
    from 5 up is that, anything else is 300 with a `COMFY-IDLE-WARN` naming the value. It reaches the pack as `OFFLOAD_IDLE_FREE_SEC`
    and is fixed when an instance is launched: a runner that reuses a running instance cannot change it.
  - `launchFlags({ idle })` adds the idle yaml (`render/comfyui-ext/extra_model_paths_idle.yaml`, apart from `nodes/` so an ordinary kept
    launch never imports the fail-closed Wan pack) to `--extra-model-paths-config` once, before the operator's args, and puts the pack
    INTO the last `--whitelist-custom-nodes` value list. That flag is `nargs='+'` with no append action, so a second flag would replace
    the multicard packs, and `--whitelist-custom-nodes=a b` reads `b` as a stray argument, so that spelling is rewritten to the space
    form; argparse abbreviations are read as the flags, down to the shortest ComfyUI's own parser accepts (measured on the reference box's checkout: `--wh`,
    `--disable-al`; `--w` and `--disable-a` are ambiguous there and stay unread); with `--disable-all-custom-nodes` and no whitelist
    one is added; with neither, every custom node loads and nothing is. Only when the yaml and the pack file exist: ComfyUI exits at
    boot on an `--extra-model-paths-config` file that does not, so a missing file is one `COMFY-IDLE-WARN` and a launch without it. With
    `idle` null (the default single render, which its runner kills) the argv is byte for byte what it was.
  - `ensureComfy` asks for the bound only for `keep` or `warm`, passes `OFFLOAD_IDLE_FREE_SEC` in a COPY of the child env made after
    the multi-GPU identity check (the parent env and the pinned-memory decision are untouched), and after the instance answers asks it
    once (`GET /offload/idle`): no answer, an `error`, or a `limit_sec` other than the one asked for (the pack never read
    `OFFLOAD_IDLE_FREE_SEC`) is a `COMFY-IDLE-WARN` and the render goes on; an answer that agrees is one `COMFY-IDLE` line.
  - A running instance reused under `keep` or `warm` whose argv lacks the idle yaml (hand-started, or kept by an older build) is
    reused, never restarted or refused, with one `COMFY-IDLE-WARN` per runner start, from at most one `GET /system_stats` per runner
    start (a keyed instance's ownership proof and the warning share the answer; the unkeyed default instance, which asked for none
    before, asks once, up to 8 s on a slow instance); an argv ComfyUI does not report is unknown, not missing. A
    hand-started instance opts in by adding the yaml to its own launch (`docs/OPERATOR-GUIDE.md`); the harness injects nothing into an
    instance it did not start (ADR 0009).
- **What this does not do.** It bounds models and executor caches, not the process: the Python process and its CUDA context live until
  the lease holder stops the instance at release, and an instance whose holder died lives until an operator stops it. A self-exit
  is deliberately not built: from inside ComfyUI a runner in a long step that is not ComfyUI's (ffmpeg, a QA pass, an LLM call) looks
  the same as an abandoned instance, and a stopped instance breaks that runner's next prompt where a `/free` only costs a reload; a
  pack-side check of the lease epoch is unsafe too (reuse re-stamps the marker while the child's env keeps the old epoch). So the
  register rows that say a kept instance lives to the end of its lease (A-RAM-3, E-4, D-2d.4-5) stay `partly`: models and caches
  bounded, process lease-bounded. Back-to-back batches are not touched: a running prompt, even a 550 s one, is never idle, the gaps
  inside a batch are seconds, and a driver that pauses more than 5 minutes between items pays one reload, which is the rule.
- **Docs.** `docs/systems/media-generation.md` (the new bullet with the live check, the log and "Who stops it" paragraphs, the source
  map), `docs/systems/gpu-lease.md` (the sentence that said nothing here is a timer now says the harness runs none and the engine
  carries its own), ADR 0009 (a dated "Extended" note), `docs/ROADMAP.md` (INV-2), `docs/OPERATOR-GUIDE.md` (the hand-started opt-in),
  `render/README.md`, and the package comment of `internal/comfyinst` (comment only).
- **Tests, red first.** `render/comfyui-ext/test_offload_idle.py` (standard library only, a fake queue with the signatures of ComfyUI's `PromptQueue` and a
  real `RLock`, a hand-set clock, no sleep: 30 tests) and `render/comfy-idle.test.mjs` (43 tests: the argv, the whitelist splice, the
  guard, the env copy, the reuse warning, the probe, the real `fetchIdleProbe` against a loopback server). They were committed
  failing on their assertions against an inert seam before the fix (24 failures in the Python file; 30 in the JavaScript file plus the
  test that runs the Python file, every one an `ERR_ASSERTION`). A review round then added five tests and changed two, again red
  first: the shortest abbreviation `--disable-al` failed on its argv comparison (no whitelist for the pack), a pack that reports another
  bound than the one asked for failed on `0 !== 1` warnings (it printed a plain `COMFY-IDLE` line), and the doubled `/system_stats`
  request of a keyed reuse failed on `2 !== 1`. The already-carried-yaml test, which failed only in a checkout whose path has a space
  (measured red from a copy under a folder named `sp ace`, 1 of 38), now builds its case from a path without whitespace, or says it
  cannot. `render/comfyui-ext.test.mjs` runs the Python file under any Python 3 (the ComfyUI interpreter when it exists, else `python`,
  else `python3`, each probed with `-I -c`), so CI runs it through `python3`. The three existing tests that reach the post-launch
  probe now inject it, so none can ask whatever listens on the default port.
- **Mutation proof.** 90 single-line mutants, run one at a time through `scripts/mutation-check.sh` (a syntax check first, so a mutant
  that does not build is `BAD-MUTANT` and never a kill; the unmutated tree is the control; the file is restored byte for byte), every one
  `RED`: 38 in the pack and 52 in the launcher. Pack: the limit comparison (`<` to `<=`, one second early, one second late); either
  flag dropped; the once-per-busy-period latch; `task_counter` ignored; a running prompt ignored; the recheck under the mutex removed,
  and its counter half removed; the fire, and the snapshot, made outside the mutex; the 5 s floor; the 300 s default; `0` not
  disabling; the thread not a daemon; a bad value not 300; the cache gate and the loaded-models gate dropped; an engine that cannot be
  asked held instead of freed; a tick that raises; an error logged on every tick; the route path; the poll clamp and the loop's wait;
  a failed route registration made fatal; the missing-queue line; the counter baseline; `NODE_CLASS_MAPPINGS` not `{}`; `torch`
  imported at module scope; `cache_on` and `models_loaded` inverted; the queue attribute; the status fields; the announce line.
  Launcher: idle on by default; the existence guard (the yaml, then the pack file); idle requested always, never, for keep only, for warm
  only, or still added under `COMFY_IDLE_FREE_SEC=0`; the yaml added twice, not looked for in the operator's args, or placed first; the
  FIRST whitelist chosen instead of the last; the `=` spelling not rewritten; the pack listed twice or placed after later flags; a
  whitelist always added; the abbreviations of the two flags unread; the child env built from `process.env`, written into the parent's,
  or set when nothing was launched; "applied" decided without looking at the argv; the reuse check run always or never, refusing, or
  warning on an unknown argv; the yaml's path spellings; the probe never or always asked, a probe that throws, an `error` answer not said,
  a 404 or an array taken as an answer, the probe path; the 5 s floor, `off`, and the default of `resolveIdleBound`; the pack name,
  the yaml name and the re-export in `wan-multicard.mjs`. The 24 mutants whose text carries quotes, backslashes or an empty
  replacement were run again through generated scripts after a first pass in which Windows command-line quoting had altered them.
  Review round (11 more, all `RED`, each killed by the test named for it): the `--disable-all-custom-nodes` floor 12 to 13 and to 11,
  the `--whitelist-custom-nodes` floor 4 to 3, the reported-bound mismatch branch off, its comparison inverted, its number check
  dropped, the asked bound left out of its warning, the `COMFY-IDLE` line printing another number, the shared `/system_stats` answer
  removed, and either of its two readers given the raw request function; the "yaml already carried" mutant was run again against the
  rewritten test.
- **Not measured.** The tests use a fake queue; no ComfyUI was started and no card was used, so the whole mechanism is read from
  the reference box's ComfyUI code (`main.py` `prompt_worker` drains the two flags, `execution.py` `PromptQueue` holds the mutex, the counter and
  `get_tasks_remaining`, `server.py` `post_free` sets the same two flags) and not seen running. The live check is written out in
  `docs/systems/media-generation.md`: it first confirms that a harness-launched kept instance is already bare in VRAM when a prompt
  ends (so VRAM cannot show the bound there), then measures the warm case in the ComfyUI process's host RAM at t+0, t+240 s and
  t+330 s, and the smart-memory case (a hand-started instance) in per-card memory. It also settles whether `/free` returns VRAM under
  `--gpu-only` (the multicard Wan placement), where ComfyUI's own unload frees nothing and a weight leaves the cards when its patcher
  loses its last reference. A warm launch now carries the pack, so the S3 batch acceptance is to be re-run. Other nodes' ComfyUI
  versions were not read; the gate (a loaded model, or a cache that is on) reads the engine at run time and does not depend on it.

### Fixed - the display layer's guard can read the card the driver reports for the monitor (`display_device: "auto"`), and the blackwell-3x16 seed ships it

- **The defect.** `layers[].display_device` named a card: the seed said index `1`, a live box may pin a UUID. The `display_floor` guard
  cross-checks that declaration against the driver's own reading of which card drives the monitor (`display_active` / `display_attached`)
  and refuses on a contradiction, but it could only refuse. The 60 % display cap (`RowsWithCap`, the allocator) and the floor the
  allocator holds an opened display card to already follow that same reading and carry no index, so the one place that could not follow
  was the display layer's own guard: with the monitors moved to another card the layer refused forever on the seeded declaration
  ("the driver reports the monitor on index 0"), and the display watcher in `fleet-serve`, which asks the same guard about a loaded
  twin, unloaded any twin it found on every check.
- **The fix, config.** `config.DisplayDeviceAuto` (`"auto"`) and `config.IsAutoDisplayDevice` (case and surrounding whitespace do not
  matter). No new key and no new wire field: `layers[].display_device` already travels in `layers` rows and may now read `auto`. The
  sentinel cannot be taken for the other forms: an index is all digits and a UUID pin starts `GPU-`. `ValidateLayers` is **not** given a
  shape check on `display_device` on purpose: a load error strips every layer from the config `Load` hands back, and the runtime resolves
  any prefix of a dashed GPU UUID (`gpuprobe.IndexOf`), so a stricter rule would turn a working UUID-pinned box into one with no composite
  tier. A typo such as `atuo` is therefore not a load error; it fails closed where it is used, the display layer refusing by name.
  The load-time footprint rule, which can only match a plain CUDA index, does not run for `auto` (admission enforces the same rule on the
  card it derives, as it does for a UUID pin).
- **The fix, guard.** With `auto`, `resolveDisplay` takes the card from `Live.ScreenCards` and returns it as both the key the free-VRAM
  reader is asked and the CUDA index, so every number the guard reads is the derived card's; the admission guard, the node's published
  layer rows and `ResidentVerdict` (the watcher) all read it through the one function. It refuses, saying why, when the reader is absent,
  when the driver flags no card (no NVIDIA card has a monitor attached: a headless box, or a screen on an iGPU; a screen asleep on a
  driver that does not report `display_attached`; a transient failure that left `display_attached` unknown), and when it reports the
  monitor on more than one card: it does not choose, because the tolerance a declared card gets for a
  second monitor is trust in the operator's word and there is none here. Reasons print `display device auto (index N)`.
  **Seat-pin coherence, sentinel path only:** a seat that declares `display_footprint_gib` is meant to sit on the display card, so under
  `auto` it is refused when its pin does not include the derived card, because a floor judged on the monitor's card while the twin loads on
  another would guard nothing. This is a policy choice and it refuses a harmless-looking admission on purpose: its job is to make a stale
  twin pin loud. It keys on the declared footprint, so a seat that declares none is judged free - 0 as a seat off a declared card always
  was. The seat's own `device` stays a literal pin (an index or a UUID): the sentinel is only meaningful in `display_device`, because
  `DeviceList()` feeds lease fencing and placement, where an unresolvable pin reads as every card.
- **The literal and UUID paths are unchanged, byte for byte.** One label helper names the device in every message, including the two
  no-reader messages that printed the raw config string; for a declared card it returns the text each site already printed. A characterization
  test pins the old sentences word for word, a literal layer next to an `auto` layer in one config, a typo, and a seat pinned off a declared
  card.
- **The fix, seed.** `setup/templates/profiles.json`: the `blackwell-3x16` display layer ships `"display_device": "auto"`, and the `_fields`
  text for `layers[].display_device` and `layers[].seats[].device` says so; `docs/tiers/blackwell-3x16.md` is regenerated (`go run
  ./cmd/gentiers .`), and now reads "display device auto". The seeded twin seat pin stays `1` (and the template's
  `CUDA_VISIBLE_DEVICES=1`): on the arrangement the table describes, a monitor on card 1, behaviour is the same; on a box whose monitor is
  elsewhere the twins now refuse with their pin and the monitor's card named, a visible re-pin step, instead of the guard naming the wrong
  card.
- **What this does not do.** It does not make the layer follow the monitor on a live box. `config.json` is the operator's and the seed never
  re-applies to it; the layer reads the driver's card only after the operator sets `display_device` to `auto` there **and** re-pins the
  twins (the seat `device` in `layers`, and the pin on each twin in the llama-swap yaml, which the renderer takes from the template, not
  from the layer). Until then a display layer declared with a literal card on a box whose monitor is elsewhere still refuses, with the
  same sentence as before. Benchmark drivers outside this repository that pin the display card by index are not changed here.
- **Behaviour changes to know about.** (1) `auto` is stricter than a declared card where the driver is silent: a literal card is trusted
  when the driver cannot say which card has the monitor and tolerated when a second monitor sits on another card, so a fresh blackwell-3x16
  install on a headless box, one whose screen is on an iGPU, one whose screen sleeps on a driver without `display_attached`, or one with
  monitors on two cards now refuses the display layer at admission until `display_device` names a card. The strictness is not only at
  admission: the display watcher in `fleet-serve` asks the same guard of a loaded twin on every check, dormant layer or not, so under `auto`
  a reading that names no single card unloads a loaded twin on that check, however much VRAM is free and with no drain (a request in flight
  on the twin ends with llama-swap's 502), where a declared card trusts its declaration through the same reading and keeps it. That is the
  watcher's existing fail-closed rule (a missing device sample unloads a twin too) applied to a card the config no longer names. (2) The display layer's whole spec is hashed into the render stamp, so a blackwell-3x16 llama-swap config rendered and
  stamped before this change reports `STALE` under `audit-yaml --against-render` until it is re-rendered (nothing served is wrong; the
  stamp is behind). (3) On a box whose only card drives the monitor, or whose screen card reports no UUID, the card table flags no display
  card while `auto` still follows the screen reading (the card table and the screen reading derive the display card with different
  exclusions); a characterization test pins it. (4) The seed edit changes the `layers` value `audit-config` compares as a whole: a live
  config whose `layers` were merged from the earlier table (`display_device` `1`) now reports `layers` as `DIFFERENT`, and `audit-config`
  exits non-zero while any seed-owned key differs, until its display layer matches the seed's (a box whose twins are re-pinned off index 1
  differs from the seed by those pins as well).
- **Docs.** `docs/systems/composite-tier.md`: the `auto` paragraph under the `display_floor` guard (what it reads, the three refusals, the
  seat-pin rule, the card-table edge) and the watcher's unload under `auto` (in "The guards keep holding after the load") and the seed step (the layer
  arrives with `auto`, the twins are pinned to index 1 and must be re-pinned, the `STALE` stamp note, the `audit-config` drift).
- **Tests, red first.** A behaviour-free seam commit (`IsAutoDisplayDevice` returning false), then the tests, which failed on their
  assertions for the stated reason before any fix: `IsAutoDisplayDevice("auto") = false, want true`; every guard case refused with `display
  device auto cannot be resolved to a CUDA index (not in the probe, or ambiguous)` where it should have admitted or named the monitor's card;
  the watcher unloaded a twin whose monitor card held its floor, and for a low one gave the unresolvable-card refusal instead of naming
  the card; the shipped table named `display_device "1"`; the docs lacked the paragraph. Cases that pass before and after by design are
  the characterization tests above, no shape check at load (a dashed full UUID, a short UUID prefix and a typo all validate and a UUID or
  `auto` config round-trips through `Load` with its layers), the delegator's carried verdict standing in where it holds no reader, and the
  floor read by the allocator not depending on the device string. New: `internal/config/layers_auto_test.go`,
  `internal/placement/guards_auto_test.go`, `resident_auto_test.go`, `auto_docs_test.go`, `internal/displaywatch/displaywatch_auto_test.go`,
  `internal/tierseed/tierseed_displayauto_test.go`, and `audit_config_displayauto_test.go` in the root package.
- **Tests added by review.** (a) The promise that the sentinel's case and surrounding whitespace do not matter was proved at the helper and at
  the label only, so two mutants survived the placement and watcher suites: `resolveDisplay` and the seat-pin rule each comparing with
  `== "auto"` (a layer written `Auto` then refused as an unresolvable UUID, or kept deriving the card while the stale-pin protection silently
  dropped away). `TestAutoDisplayDeviceIsRecognisedInAnySpellingAtEveryDecision` runs `Auto`, `AUTO`, ` AUTO ` and `\tAuto\n` through the
  derived card, the seat-pin rule, the no-card refusal and the resident verdict. Production was already right, so it is green; the proof
  that it can fail is the two mutants, SURVIVED before it and RED with it (`"Auto" must read the monitor's card (index 0) and admit, got
  ... display device Auto cannot be resolved to a CUDA index`; `"Auto" must keep the seat-pin rule ..., got an admit with footprint 0.0`).
  (b) `TestTheWatcherReadsASilentDriverAsAViolationOnlyForAnAutoLayer` pins the watcher's unload under `auto` for a reading that names no
  single card (nothing flagged, a sample that cannot tell, monitors on two cards), dormant layer or not, against a declared card that keeps
  its twin through the same readings. (c) `TestAuditConfigReportsLayersMergedFromTheTableThatNamedTheDisplayCard` pins the `audit-config`
  drift (exactly the `layers` key, exit non-zero) and a node carrying today's seed auditing clean. (d) The docs gate gained the retired
  `remote-desktop` and eight required phrases (the probe's causes, the watcher's unload, the `audit-config` drift), red on their
  assertions before the page was fixed. (e) Two test names that said the layer follows the monitor were renamed to say what is true, that
  the guard reads the card the driver reports for the monitor.
- **Mutation proofs** (`scripts/mutation-check.sh`, one changed line each, every one RED, file restored byte for byte): `IsAutoDisplayDevice`
  without `TrimSpace`, case-sensitive, a different constant; `resolveAutoDisplay` returning card `1` for both the key and the index, or the
  index of the derived card with card `1` as the key (placement tests and the watcher tests both), the multi-card threshold moved from more
  than one to more than two, the empty-reading and no-reader checks removed; the seat-pin rule deleted, applied to declared cards too, or
  keyed on a footprint of zero or more; the sentinel compared with `== "auto"` in `resolveDisplay` and in the seat-pin rule, and the seat-pin
  rule comparing with a whitespace-sensitive `EqualFold`; the label helper returning the raw string, matching only the lower-case spelling,
  or the raw string in either no-reader message; the auto branch in `resolveDisplay` disabled; the seat label losing its model; the
  multi-monitor wording; the watcher skipping dormant layers; `ScreenCardIndexes` ignoring an unknown `display_attached` reading; the
  shipped table's `display_device` put back to `"1"` (the tier-seed test, and in the root package the `audit-config` test and the generated
  tier page's currency test); and each sentence the docs test requires removed, `remote-desktop` put back included.

### Fixed - the installer stops writing the every-expert `--cpu-moe`: a CUDA box off the tier matrix renders no 26B, and the selftest no longer edits the yaml

- **The defect.** INV-1: the cards do the inference and host RAM is overflow only, so the one sanctioned expert placement in RAM is a
  MEASURED partial `--n-cpu-moe N` up to the tier's `n_cpu_moe_max`. The rule audit (`servingtmpl.AuditCPUMoE`, `renderGate`,
  `audit-yaml`) shipped in 0.184.0 and refuses the bare `--cpu-moe` (every expert layer in host RAM), but the installer still wrote it in
  five places, and said so under "Not changed (needs a decision)": (1) `fallbackProfile` rendered `--cpu-moe -ngl 999` plus the 26B for
  every `--fallback-backend cuda|cuda-resident|dual-cuda` render, which is what `install.ps1` asks for on a box with no recognised tier;
  (2) `moePlacement` returned the bare form for `moe_26b: cpu_moe` on any backend; (3) and for `n_cpu_moe` naming no N ("rather than emit
  a broken flag"); (4) `selftest.ps1`'s `Invoke-Remediate26B` inserted ` --cpu-moe` into the live `llama-swap.yaml` on a 26B allocation
  failure, the only one of the five that wrote onto a real file; (5) the selftest's `Invoke-ProbeLoad -CpuMoe` launched a standalone 26B
  with the experts in RAM, from step 3 (the 26B decode number), the cold-swap probe and, unnoticed until now, the ctx probe of step 2:
  `$residentIsMoe = ($residentId -eq 'gemma4-26b-a4b')` is true on the nine tiers whose resident is the 26B, so the Windows selftest
  recorded "ctx MEASURED ok" for a placement the yaml does not serve. `install.ps1` held a dead PowerShell shadow of (1)-(3)
  (`$defaults.cuda.moe`, `cpu_moe`, and `n_cpu_moe` with no N turned into `cpu_moe`) that nothing consumed except `include_26b`, which
  decided a 14 GB download for a seat the render then served on the wrong terms.
- **The decision (D1, option a, "stop").** An unmeasured placement gets no flag at all. An off-matrix CUDA box has no tier, no
  measurement and no declared fit, so it is not claimed to hold the 26B on its card (the same outcome as `ampere-6`, `ampere-8` and
  `blackwell-8`, which are `moe_26b: drop`); the alternative, `-ngl 999` with the 26B kept, is an entry that fails at load on any small
  card. The cpu backend has no card to overflow from and keeps its exemption (its `--cpu-moe` text is inert: no cpu template carries the
  token in a command). The tier notes and template header prose that still say `cpu-moe` are not touched, because editing a tier entry or
  a template re-stamps every file on it (see Not changed).
- **The fix, renderer (Go).** `fallbackProfile` returns `moe_26b: drop` for the CUDA family (ctx, KV and flash-attn unchanged); `vulkan`
  keeps `-ngl 999` and `cpu` keeps its inert literal. `moePlacement` returns the every-expert form for the cpu backend only, and nothing
  for `n_cpu_moe` with no N (it used to fall back to the every-expert form). New `moeDefect`, called in `deriveRender` before any flag is
  computed, refuses a tier that includes the 26B and names `cpu_moe` on a backend with a card, or `n_cpu_moe` with no N, by tier name,
  INV-1 and what to use instead, at any RAM tier (before: a `min` box silently lost the 26B, and the others were refused by the write gate
  after the flag was computed). `spillViolations` loses its no-N clause (`moeDefect` owns it); `cpuMoeExempt` is the cpu backend only (the
  `moeLiteral` half was a concession to the installer writing the literal); `auditCPUMoEFile` turns a verbatim off-matrix render that
  still carries the flag, which only a binary at or before 0.185.1 wrote, from a `note:` into a violation whose text says to re-render.
  The first walk of `win-cuda-resident` and `win-dual-cuda` with the 26B dropped (no table tier does that on those two templates) renders
  cleanly. Comments only in `internal/servingtmpl` and `internal/hwdetect`.
- **The fix, `selftest.ps1`.** `Invoke-Remediate26B` is deleted. A 26B tier that fails to load is recorded `status:"fail"` with
  `tiers[].detail` (a new field, `null` on every other row, documented in `SETUP-AGENT.md`) from the pure `Get-MoEFailureAdvice`: RAM is
  overflow only so selftest moves nothing and edits no yaml; set the tier's `moe_26b` to `drop` (a tier whose `resident_tier` or agent seat
  is the 26B needs those changed too, or pick a tier whose resident is not the 26B, because `TestNo26BAgentSeatOnRAMDroppable26B` refuses
  the drop alone), or declare a measured `n_cpu_moe` with `n_cpu_moe_max`, then re-run the install. `Invoke-ProbeLoad` loses `-CpuMoe`
  everywhere (the ctx probe, the cold-swap probe, step 3); step 3 and the `moe_full_offload` canary are retired to a fixed
  `skipped` receipt entry (the key stays, the shape does not change; the canary trial was unreachable on every shipped tier, since the
  only tier with the 26B and a non-gpu placement is `cpu`, whose backend skipped it). `receipt.remediations` keeps one writer, the ctx
  downshift. The ctx probe's downshift hangs on `Test-AllocFailure` and on the probe's whole-log scan, and neither recognised the
  literal llama.cpp message `CUDA error: out of memory` (both patterns read `CUDA (error )?out of memory`, with no colon): both take
  `(error:? )?` now, and `Test-AllocFailure` has a case table (it had no test after the remediation test was retired). The chat tiers
  the selftest exercises are now the ones whose gguf is on disk AND which the active yaml defines (`Select-ServedTiers`): a box
  re-rendered without the 26B keeps the 14 GB file, and the selftest used to ask llama-swap for the model it had dropped, print
  advice that blames the card, and load the unserved file in the cold-swap probe.
- **The fix, `install.ps1`.** The unknown-profile fallback table drops the `--cpu-moe -ngl 999` literal and the 26B include for `cuda`
  (`include_26b` false: the 14 GB download follows), mirroring `fallbackProfile`; a tier naming `cpu_moe` on a card backend, or
  `n_cpu_moe` with no N, throws by name at any RAM tier (mirroring `moeDefect`); and `Test-YamlHasEveryExpertFlag` joins the Step 6 SKIP
  probe, so re-running the installer over a yaml an older installer rendered (which passes every other probe) re-renders it instead of
  keeping the flag the audit refuses. The detector's pattern is a character class (`--cpu[-]moe`) so the source pin does not read the
  detector as a writer, and its comment guard is a negative lookahead: every serving template puts the placement token FIRST on its own
  continuation line, so a yaml the old renderer wrote has the flag at the start of a line, and a guard that had to consume one
  character before the flag never matched it (the first version of this probe was blind to exactly the yamls it exists for; it was
  checked on a yaml rendered by a 0.185.1 binary built from the tagged source, where the first version answered no and this one yes).
  A fresh `config.json` for a box off the tier matrix now gets `escalation_model` and `reasoning_model` set to `""`
  (`Get-OffMatrixRungSeed`, merged by Step 8 when no profile row resolved and the render serves no 26B): the config template binds
  both rungs to the 26B, every tier that drops the 26B reseeds them, and an unknown profile has no row to do it, so without this a
  fresh install shipped two bindings its own yaml does not define. Comments only in `install.sh` and `detect.ps1`.
- **Behaviour changes to know about.** An off-matrix CUDA box gets no 26B (and downloads none); `audit-yaml --against-render` on a render
  made by 0.185.1 for such a box reports the `cpu-moe` violation with the re-render text and `STALE(params.include_26b, params.moe_26b)`,
  and re-running the installer re-renders it (checked piecewise on a real 0.185.1 render, without a full install: the SKIP probe's
  detector sees the flag, and `install render` over that file then passes `audit-yaml` with OK and MATCH). A fresh
  `config.json` on such a box has `escalation_model` and `reasoning_model` blank, so the cascade has no escalation and no reasoning
  tier there, as on `ampere-6` and `amd-gcn`. An EXISTING `config.json` is never rewritten, so a box that installed under 0.185.1 or
  earlier keeps its two bindings to the 26B after the re-run: set both to `""` by hand (`doctor` names them otherwise).
  The Windows selftest's ctx probe now loads the card-resident 26B at the projected window on the nine tiers whose resident is the 26B,
  so it can OOM and take the downshift path (`remediations[]` then holds `downshift ctx a->b`, never an edit). A card too small for the
  26B (`amd-rdna3-dgpu` on 12 GB is the case the old `R4.2` text named) now fails its 26B tier with advice instead of limping on
  `--cpu-moe`; there is no automated path past it, and the advice says so.
- **Docs.** `setup/SETUP-AGENT.md` (tier table cells, the AMD chapter without the `--cpu-moe` floor and the full-offload promotion row,
  the receipt schema, the failure tables), `docs/OPERATOR-GUIDE.md`, `README.md`, `docs/systems/setup-installer.md`, ADR 0043 (the W4
  bullet is amended: no second exemption; "what stays open" is closed), `_fields.moe_26b` and `_fields.n_cpu_moe` in `profiles.json`
  (not hashed, not rendered into `docs/tiers`; no tier entry changed), and in the fix round `docs/systems/setup-installer.md` (the
  retired `moe_full_offload` canary) and a paragraph in `SETUP-AGENT.md` on what an unknown profile renders and the by-hand step
  for an existing `config.json`.
- **Not changed.** (1) The tier notes in `profiles.json` that still mention `cpu-moe` (`amd-rdna3-dgpu`, `dual-gpu`, `ampere-8`,
  `blackwell-8`, `ampere-6`, `amd-rdna3`, `cpu`) and the header prose of the `win-cuda`, `win-vulkan`, `win-dual-cuda`, `linux-vulkan`
  and `linux-cpu` templates: editing either re-stamps every file on that tier or template (STALE until `install render` is re-run) and
  regenerates `docs/tiers`, so they ride a release that already touches them. Until then the generated `docs/tiers/amd-rdna3-dgpu.md`
  page still carries the `amd-rdna3-dgpu` note's now-false sentence that selftest's standing remediation switches the 26B to the
  every-expert flag (the remediation is gone; a 12 GB card now fails its 26B tier with advice). (2) The `cpu` tier keeps `moe_26b: cpu_moe` and its inert
  `--cpu-moe -ngl 999` stamp parameter; changing the string would STALE every stamped cpu-tier file. (3) The vulkan off-matrix fallback
  still keeps an unmeasured 26B at `-ngl 999` (never an expert flag; the same reasoning as the CUDA family, left for a decision).
  (4) `audit-yaml` still never applies the spill ceiling (`AuditSpill`) to a live file. (5) The fleet read-back of W4's own acceptance
  (re-running the audit over every node's live yaml with this binary) is not part of this branch and was not run.
- **Tests, red first.** The seams (`Get-MoEFailureAdvice`, `Test-YamlHasEveryExpertFlag`) were committed behaviour-free, then the tests,
  seen failing on their assertions against 0.185.1: `TestOffMatrixFallbacksRenderNoEveryExpertFlag` (the cuda family carried the flag and
  the 26B on every OS and RAM tier), `TestMoePlacementNeverReturnsTheEveryExpertFormOnABackendWithACard`,
  `TestDeriveRenderRefusesAPlacementThatWouldPutEveryExpertInRAM` and `TestDeriveRenderKeepsTheSpecificNoNRefusal` (derive accepted
  both defects), `TestCPUMoeExemptIsTheCPUBackendOnly`, `TestRenderGateExemptsOnlyTheCPUBackend`, `TestRenderedTiersCarryNoBareCPUMoE`,
  `TestAuditYAMLReadsTheExemptionFromTheStamp` (a legacy off-matrix render was a note and exited 0), the source pins
  `TestInstallerSourcesWriteNoEveryExpertFlag` and `TestSelftestHasNoCpuMoeSwitchAndNoRemediator` (20 and 12 offending lines), the
  inverted last block of `TestInstallRenderRefusesNCPUMoEAboveTheTiersMeasuredSpill`; and in PowerShell `selftest-no-cpumoe.test.ps1`
  (renamed from `selftest-remediation.test.ps1`: 26 assertions) and 14 new assertions in `install-config-seed.test.ps1` (the unknown-profile
  fallback, both throws, the SKIP probe's detector against yaml fixtures, the selftest source pins), plus `render.tests.ps1` for the
  unknown-profile render. Review fix round, same order: the detector against every shipped serving template rendered with the
  every-expert placement (9 assertions red against the first detector: the line-leading fixture and the 8 templates that carry the
  token); the fresh-config rungs (`Get-OffMatrixRungSeed` seam first; 7 assertions in `install-config-seed.test.ps1` and the
  `render.tests.ps1` property that every chat model the fresh config names is served by the rendered yaml, red for cuda, with the
  vulkan and cpu fallbacks as controls); the selftest tier filter (`Select-ServedTiers` seam first; 8 assertions) and the
  `Test-AllocFailure` case table plus the whole-log scan (red on `CUDA error: out of memory`). One assertion of the no-N refusal test
  could never fail (every error return of `deriveRender` is a zero result) and was removed.
- **Mutations.** Each rule has a single-line mutant proved RED with `scripts/mutation-check.sh` (the mutation lands on one line, the
  unmutated tree passes first): the CUDA fallback back to the literal plus the 26B; `moePlacement` returning the flag for `cpu_moe` on any
  backend, a flag for `n_cpu_moe` with N=0, and nothing for a named N; the `moeDefect` call deleted, each of its two conditions flipped and
  its `Include26B` guard removed; `cpuMoeExempt` regaining the literal and losing the cpu backend; `audit-yaml` turning the finding back
  into a note and losing the off-matrix discriminator; a `-CpuMoe` argument, a `Invoke-Remediate26B` function and the `--cpu-moe` literal
  restored in the scripts (Go source pins); and in PowerShell the cuda fallback's include and flag restored, either throw disabled, the
  SKIP-probe clause removed or stripped of its cpu exemption, the detector reading comments, the advice call removed and the advice
  naming the flag. Fix round: the detector's consuming guard restored; the rung helper returning nothing, losing its `Include26B`
  guard, losing its profile-row guard, the Step 8 call removed and the Step 8 merge removed; the tier filter removed, either fail-open
  arm removed, the MAIN call removed, the skipped-tier note changed, the alias branch and the key anchor of `Test-YamlServesModel`
  broken; and in the allocation classifier the colon removed from either copy, an alternative dropped and an over-broad `timed out`
  added.

## [0.185.1] - 2026-10-13 - the TTS sampling flags reach the generalist engine and an unknown tts.mjs flag refuses

### Fixed — the sampling flags of `render/tts.mjs` reach the generalist engine, and an unknown or malformed flag refuses instead of vanishing (0.185.1)

- **The defect.** `render/tts.mjs` documents `--temperature --cfg-weight --exaggeration --repetition-penalty` in its usage header, but
  `buildWorkerArgs` forwarded the sampling flags only when `--engine finetuned`. The generalist worker `tts_chatterbox.py` had no argparse
  entry for them and never passed them to `ChatterboxMultilingualTTS.generate()`, so `node render/tts.mjs out.wav "text" --cfg-weight 0.3`
  on the default engine ran, without a word, at the model's own default (a downstream user hit it narrating in production). And
  `parseArgs` accepted any `--name`, so a misspelled or unknown flag was dropped the same way, a value flag with no value (last on the
  line, or an unset shell variable) likewise, and a `--flag` that took the next flag as its value ate it.
- **The fix, launcher.** `buildWorkerArgs` forwards the six sampling flags (`--temperature --cfg-weight --exaggeration
  --repetition-penalty --min-p --top-p`) to BOTH workers under the same names, only the ones given.
  The new `parseCli` / `checkFlags` run before the interpreter is resolved, the lock is taken or a worker starts, and refuse with exit 2,
  the reason and the usage line (which now lists all six): an unknown flag (named; `--flag=value` gets the spelling to use, and
  `--no-lock=1` says that flag takes no value), a value flag with no value (last, empty, or followed by another flag), a sampling value that is
  not a finite decimal number (`abc`, `nan`, `inf`, `1e999`, `0x10`; every sign, fraction and exponent spelling `float()` reads is accepted,
  negative ones included), an `--engine` other than `generalist|finetuned`, `--model`/`--base-dir` without `--engine finetuned` (the generalist
  would ignore them), `--engine finetuned` without both of them (`tts_chatterbox_ft.py` requires both and exited 2 after the lock, a failure to
  the Go wrapper and not the exit-3 defer; the Go lane defers before it spawns unless both are set), and a third positional (an unquoted text
  used to be cut after its first word). The flags object has no prototype, so `--__proto__` is a flag like any other. **Behaviour change:**
  a command line that carried a stray, misspelled or valueless flag, an unknown `--engine`, or `--engine finetuned` without its model, used
  to run (or fail after the lock) and now exits 2.
- **The fix, worker argv.** Every option reaches a worker as ONE token, `--name=value` (`--text=hola`, `--ref=...`, `--temperature=-1e-2`),
  never as a flag followed by a separate value token. argparse reads a separate value that opens with a dash and has no space as an OPTION
  (`---`, `--Intro--`, `--1`, `-x`, `-hola`, a path `-ref.wav`, an out path `-o.wav`, a `--lang -es`, and on Python 3.11 the negative numbers
  `-1e-2`, `-5.`, `-1E3`, `-5.e2`, `-.5e1`) and the worker exited 2 ("expected one argument") after the lock was taken and llama-swap
  drained, for a command line the launcher had accepted. After `=` the token is always the value, on every Python. The launcher no longer
  carves out the negative spellings (an earlier round of this fix refused `-1e-2` and `-5.`, with a message false for them), so the number rule
  is one rule: a decimal `float()` reads, finite. Both workers are unchanged here: argparse reads either spelling.
- **The fix, parsing.** A token is a flag only when it is shaped like one (`--` and a name of letters, digits, underscores and inner dashes), so a
  text that opens with dashes, such as `"--- Intro ---"` (which `generate-audio out.wav -- "--- Intro ---"` keeps intact, CT-52, and the Go
  lane puts second, before its flags), `---` or `-x`, is the text; before, a `--`-led one was read as a flag that swallowed the next flag, and the
  narration was whatever positional was left. A bare `--` ends the flags (`tts.mjs out.wav -- "--hola"`). A text that is itself flag-shaped and
  arrives from the Go lane (which sends no `--`) is refused as an unknown flag, loudly, not narrated wrong.
- **The fix, worker.** `tts_chatterbox.py` reads `--temperature --cfg-weight --exaggeration --repetition-penalty --min-p --top-p` (the same
  flags and `dest` names as `tts_chatterbox_ft.py`, `type` a finite float so `nan`, `inf` and `1e999` fail in argument parsing before the
  model loads, default `None`) and passes only the ones given to `generate()` under the model's keyword names (`temperature`, `cfg_weight`,
  `exaggeration`, `repetition_penalty`, `min_p`, `top_p`): an unset flag keeps the model's default, a given `0` is passed, and the log line
  says what was passed. The fine-tuned worker already read the six flags and is unchanged (a test runs the launcher's argv through it); it is
  still a skeleton that validates its arguments and defers (exit 3, `FT_ENGINE_NOT_VENDORED`) and never calls `generate()`, so on
  `--engine finetuned` the six flags are accepted and forwarded and take effect once that engine is vendored.
- **Every flag the harness sends stays accepted.** `internal/pipeline/pipeline.go` builds this command line for the voice lane
  (`--clone --lang`, and `--engine --model --base-dir --clone --lang` plus the `voicegen_ft_*` recipe); `render/tts.test.mjs` reads the Go source,
  checks that the scan still finds those flags and that `parseCli` accepts each, alone and together (the finetuned arm always with both
  `--model` and `--base-dir`, the only shape the Go lane sends), and pins the two argv shapes through to the workers' argv. No Go file changed.
- **Docs.** `render/README.md` (flag table, the strict command line) and `docs/systems/media-generation.md` (entry points, source map).
- **Tests.** `render/tts.test.mjs`: the launcher cases above through `parseCli`; three process-level cases (exit 2, the flag named, the usage line
  lists all six) that run with `--no-lock`, an isolated environment and an interpreter path that does not exist, so a command line that is
  not refused cannot reach a model, a GPU or the machine's llama-swap; and the worker cases, which run the REAL `tts_chatterbox.py` and
  `tts_chatterbox_ft.py` under `python -I` against stub `torch` / `torchaudio` / `chatterbox` modules (the stub `generate()` has the real
  signature, so a keyword the model does not take is a `TypeError`, and prints the keywords it was given). Nothing loads a model. They run
  the argv `parseCli` builds for a dash-led text, `--clone`, `--lang` and out path, and for every negative spelling, through the real
  worker; `TTS_PY` on a Python 3.11 interpreter (the one that read `-1e-2` as a flag) runs the same file against it.
  **Red first, in four stages that each have their own test-only commit before the fix:** on a behaviour-free seam (`parseCli` returning
  today's decisions), 14 assertion failures, among them `buildWorkerArgs` (`actual [ --out out.wav --text hola --lang es --ref /r/ref.wav ]`,
  no sampling flag), `--temperature "abc" (default engine) was accepted`, `--bogus was accepted`, `a misspelled engine silently ran the
  generalist worker`, both process cases (the run went on to the worker lane instead of exiting 2) and four worker cases (`usage:
  tts_chatterbox.py [-h] --out OUT --text TEXT [--lang LANG] [--ref REF]`, exit 2); then `--model with the default engine was accepted and
  then ignored`; then three parser cases (`unknown flag --- Intro ---`, `unknown flag --`, the `--no-lock=1` hint); then, after review,
  17 failures for the worker argv and the finetuned rule: `tts_chatterbox.py: error: argument --text: expected one argument` (text `---`),
  `argument --out: expected one argument` (out path `-o.wav`), the same for `tts_chatterbox_ft.py`, `--temperature must be a finite number,
  not "-1e-2"` (a message false for that value), `finetuned with neither --model nor --base-dir was accepted; the worker would exit 2 after
  the lock`, and the pinned `--flag=value` argv. **Two parts were not red first, and the history says so:** the refusal of the negative
  spellings `-1e-2` and `-5.` was written and run red against the tree before its fix but committed together with it (the `=` form later
  removed that rule and its test), and the test that an empty value for a value flag refuses was added green, to pin an arm no test held
  (its mutant is red). Guards that pass before and after by design: the finetuned argv, the `SAMPLING_FLAGS` pin, the Go-lane acceptance
  tests, a worker run with no sampling flag, the fine-tuned worker accepting the six forwarded flags, and the fine-tuned worker requiring
  `--model` and `--base-dir`.
  Mutation proofs: 47 single-line mutants through `scripts/mutation-check.sh` against the final tree (each landed on exactly one line,
  compiled, and was restored byte for byte; 46 were `MUTATION-VERDICT: RED` on the first pass, and the one that survived, the finetuned
  message naming `--model` for both flags, showed a test that matched the explanation in the message and not the flag it says it needs; that
  test now matches `needs --<flag>`, added green, and the mutant re-run is RED), the suite run under the hidden window watcher. In
  `tts.mjs`: the original bug (the forwarding guarded by `flags.engine === "finetuned"` again), a name dropped from `SAMPLING_FLAGS` and one
  from `VALUE_FLAGS` (`base-dir`, which the Go lane sends), the unknown-flag test made always true, the finite-number test and, separately, the
  decimal-shape test removed, the engine check, the `--model`/`--base-dir` check, the `--engine finetuned` requirement removed and its message
  made to name `--model` for both, the `startsWith("--")`, empty-string and `typeof` arms of the missing-value check, the third-positional
  check, `Object.create(null)` replaced by `{}`, exit code 2 changed to 1, the number regex narrowed three ways (no `+` sign, no exponent, no
  leading-dot form) and widened to hex, a name dropped from the usage line, the `--model` and `--base-dir` forwarding removed, the terminator,
  the flag shape widened to `/^--/`, the terminator honoured after it was seen, both `--flag=value` hints, and the `--name=value` helper
  (and each of its call sites: out, text, lang, ref, model, base-dir, the sampling loop) reverted to a separate value token, the helper and
  the sampling loop also with the suite run on Python 3.11.15. In `tts_chatterbox.py`: the `generate()` update removed, the presence test
  made a truthiness test (a given `0` lost) and removed (an unset flag passed as `None`), a name dropped from `SAMPLING`, a `dest`
  misspelled twice, the finite test and the `nan` fallback removed, and `type=str`.
