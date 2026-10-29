# Changelog archive: 0.185.0 to 0.182.0

Verbatim entries moved out of the root `CHANGELOG.md` so the file GitHub renders stays under its size limit.
History, never edited. Relative links inside the entries are written from the repository root.
Newest first; the live changelog is [`../../CHANGELOG.md`](../../CHANGELOG.md).

## [0.185.0] - 2026-10-13 - the display card's cap is read live on its row and audit-yaml names the seats it cannot judge, the card-only placements compute on the faster of two same-size cards, the Wan sizing knows sage attention, and results[].node is the node id placement used

### Changed — the card-only placements choose the compute card by board power when two cards are one size, and `WAN_COMPUTE_UUID` names it

The Wan card-only placement (`--wan-placement multicard`) and the image and edit card-only placements (`--placement multicard`) picked the compute
card (`cuda:1`) by exact memory size, then by the higher UUID. A 5070 Ti reads 16303 MiB beside a 5060 Ti at 16311 MiB, so the slower card
computed, by 8 MiB. One planner serves all three placements (`resolveWanCards`), so all three change together.

- **Behaviour change: which card computes.** Two cards whose sizes are within 1 percent of the larger (`VRAM_EQUAL_FRACTION`, 0.01, chosen:
  163 MiB at 16 GiB) are one size, and then, when both boards report a design power (`power.default_limit`) and the higher is at least 1.25
  times the lower (`POWER_RANK_RATIO`, chosen), the higher-power card computes. The pair this was written for reads 300 W against 180 W (1.67
  times); measured 2026-10-12 on a 16 GB Blackwell pair with `--use-sage-attention`: about 2.5x. Every other pair, and every pair whose power
  could not be read, takes exactly the roles it took before (the larger card computes, then the higher UUID), so a failed power read
  reproduces the old behaviour and never refuses a run; for a pair within the 1 percent size band the reason says `board power not reported for idx N`. The plan's one line contains
  `; compute card chosen: <the rule>`, followed by the sage clause under sage attention.
- **The power is a second, best-effort query.** `queryCardsRanked` (the default card source of `planMulticard` and `planImageMulticard`, with a
  `spawnSync` parameter for tests) runs the unchanged display ladder of `queryCards` first, then `nvidia-smi --query-gpu=uuid,power.default_limit`,
  asked once more when it fails and joined by lower-cased UUID. Folding the power into the display query would have cost a driver that rejects the
  column the `display_attached` rungs. Every ask hides its console window on Windows (`windowsHide`, pinned by a test on all of them).
- **Added: `WAN_COMPUTE_UUID`.** An environment variable read by both planners (a flag could not reach the typed image and edit doors, whose
  runner child inherits the process environment). It takes an nvidia-smi index, a UUID or a case-insensitive UUID prefix of 8+ characters after the
  optional `GPU-`. A token that names exactly one card of the pair makes it compute, beating the size and the speed rule (the resident and peak
  checks still judge it: a named card that cannot hold the share is refused with the numbers, the 12 GiB card of a 24 + 12 GiB pair at
  `--wan-resident-gb 4` for one, while the 16 GiB card of a 24 + 16 GiB pair holds the default share of 3 at film size, 16.0 - 12.2 - 0.5 = 3.3, and is admitted). A token that names no card of the driver's
  table is refused (`WAN_COMPUTE_UUID names "<token>", which is no card in the driver's table (...), so it cannot say which of the two cards
  (idx a, idx b) computes`): an explicit setting must not run quietly on a typo or a stale id. A token that names a real card outside the pair, or
  begins more than one card of the table, is ignored with a one-line note on the plan's line (`WAN_COMPUTE_UUID names "<token>", which does not
  name exactly one of the two cards (idx a, idx b); ranked instead`), because the variable is box-level. It never adds a card: the lease or
  `--wan-cards` still decide the pair.
- **Upgrade note: stop a kept (`--keep-comfy`) multicard ComfyUI before the first run after upgrading.** The instance key follows the roles
  (`mc-<storage>-<compute>`), so a pair whose roles change gets a new key, and a kept instance on the old key refuses with `COMFY-PROFILE-MISMATCH`.
  Under a lease the holder stops the instance on release, so only a kept or orphaned instance bites.
- **Known limits, written down.** The lease sizing (`internal/hostneed`, comments only here) still judges the compute stage against the larger
  card; inside the 1 percent band the gap is at most 0.16 GiB at 16 GiB, so a share at the film-size ceiling (3.2 GiB) can be declared 0 and refused
  by the runner on the smaller card that computes (a refusal before ComfyUI starts, never an out-of-memory run); the default share of 3 is
  unaffected. Board power is a proxy for speed. `gpu_comfy_order` is the operator-declared fastest-first order, and the one place the harness holds
  such a thing; follow-ons, not in this change: seeding the compute choice from it through the grant environment, and `compute_cap` as the primary
  key for an fp8 pair that spans Ampere and Ada.
- **Tests.** Nineteen, in `render/wan-multicard.test.mjs` and `render/multicard-image.test.mjs`, written first against a behaviour-free seam
  (`queryCardsRanked`, `computeOverride` and `chooseRoles` stubs) and seen red on their assertions. The fast card is the lower-UUID card in the
  ranking fixtures, so a mutation of any one rule turns an answer. Twenty-eight mutations, each one line and each reverted byte for byte
  (`scripts/mutation-check.sh`), fail a test they name: the band as exact equality, the fraction at 0.05 and at 0.001, the ratio at 1.0, `>=` to `>`
  at 1.25, an unreported power that ranks, the roles inverted, the ranking outside the band, the override matched against the pair instead of the
  table, a no-card override ignored, a card outside the pair taken, an ambiguous token taken, a blank value untrimmed, the note reworded, the
  unread remark, `compute: computeOverride(env)` dropped in each planner, the say suffix dropped in each, each default card source back to
  `queryCards`, the power read from the wrong column, a window shown on the power ask and on the display ask, the retry dropped, the join and the
  key case-sensitive, and the wrong power field. Docs: `docs/systems/media-generation.md` ("Which card computes", the image section),
  `render/README.md`, the usage comments of `render/comfy-video.mjs`; the stale "the larger card computes" comments of `internal/hostneed`.
- **Review round.** A refused `WAN_COMPUTE_UUID` now says how a UUID prefix is counted (8 or more characters after the optional `GPU-`, so `GPU-aaaa` names nothing) and, for a
  token of digits alone, that it was read as an nvidia-smi index and that a UUID prefix made of digits needs the `GPU-` spelling (`GPU-11112222`); the token rule itself is unchanged,
  because it also serves `--wan-cards` and the lease path. The image planner's override gets the test the Wan planner had: a named card that cannot hold the resident share or the
  recipe's peak is refused on that card (two mutants that judged the larger card instead survived the suite before it, and are RED now). The 24 + 16 GiB example above was wrong
  (the 16 GiB card holds the default share at film size and is admitted); a positive control joins its test. The comments of `internal/hostneed` now split the override out of the
  1 percent bound, which belongs to the power rule alone (an override can name the smaller card of any pair).

### Changed (behaviour) — the card-only Wan placement knows sage attention: 15.2 GiB of film-size activations, in the runner and in the lease sizing

The film session of 2026-10-12 ran the film clip (720x1280x81) with `--use-sage-attention` in `COMFY_EXTRA_ARGS`: the compute card read 15,482 MiB in use
at resident 0 (15.119 GiB), and 15,216 MiB at resident 2 on 720x1280x65 (14.859 GiB, 12.859 beyond the share), 3 GiB more than the default attention's
12.2 GiB beyond the resident share. Neither the runner nor the lease sizing read the attention mode, so a sage call at the default share of 3
(3 + 12.2 + 0.5 = 15.7 of 15.9 GiB) was admitted by both and ran out of memory on the compute card.

- **Behaviour change: sage film calls must pass `--wan-resident-gb`.** Under sage the default share of 3 is refused from about 58,000 tokens up on a 15.9 GiB card
  (720x1280x61, 57,600 tokens, still admits 3.1 GiB at most; 720x1280x65 admits 2.5 at most, 720x1280x81 0.2, so pass 0 at film size), by the planner before anything starts and by
  the lease sizing (which then declares the ordinary set). A film driver that exports `COMFY_EXTRA_ARGS=--use-sage-attention` and relies on the default share changes from "runs, and runs out of memory"
  to "refused before the lease slot". The refusal puts its sage clause before the ceiling and replaces the default mode's "(the measured film-size recipe
  keeps 3)" tail with the way out; the default-mode message is byte for byte what it was.
- **Runner.** `WAN_FILM_ACTIVATIONS_SAGE_GIB` (15.2, a one-line literal the Go mirror reads). `wanActivationsGib(tokens, attention)`: the default curve, and under
  sage the straight line through the two measured sage points (12.9 GiB at 61,200 tokens, 15.2 at the film's 75,600), used from where it meets the default's cap
  (about 56,800 tokens) up and held at 15.2 above the film's size; below that sage is sized as the default, which already covers the benchmark's 6.16 GiB reserved
  with sage at 20,280 tokens (6.29). So sage departs from the default sizing only above about 57k tokens, and only 61,200 and 75,600 tokens were measured: the
  line between them is chosen. (The line through the two points would otherwise rise above the default below about 20,700 tokens, where nothing was measured:
  at 7,800 tokens it reads 4.4 GiB against the default's 2.4.) `wanAttentionMode` reads `COMFY_EXTRA_ARGS` token by token with ONE predicate,
  `isSageAttentionToken` (`render/comfy-lifecycle.mjs`): the exact flag or an argparse prefix of 8+ characters (`--use-sa` is sage; `--use-s` also begins
  `--use-split-cross-attention` and is not; a longer token is no prefix). Another attention flag (flash, split, quad, pytorch) is sized as the default: only sage was
  measured. `checkResident` and `geometryNote` take the mode (`1280x1280x121` reads "film's 15.2 GiB" and about 39.9 GiB under sage); the plan carries `attention`, and
  its one line gains `; sage attention (COMFY_EXTRA_ARGS): activations sized at X GiB`.
- **Lease sizing.** `wanActivationsSageGiB` (a test pins it to the runner's constant, and another that the two sides admit the same share at film size),
  `CardOnly.SageAttention`, read from the process environment by `sageAttention()` with the same token table (one table, `render/testdata/sage-token-table.json`, read by the tests of both languages). The sizing reads no geometry,
  so under sage it holds 15.2 GiB for any clip: a partial-share sage call at a smaller clip (the measured 65 frames at 2 GiB) declares the ordinary set where it
  declared 0 before sage was read, unless `--ram 0` states the real figure; the detail says so. **An `env COMFY_EXTRA_ARGS=...` prefix inside the wrapped command
  is invisible to the lease sizing** while the runner sees it (the lease then declares as for the default attention); export the variable around `gpu reserve`, or pass
  `--ram 0`. Both are documented next to the `--ram 0` advice.
- **Reuse (its own commit).** A kept (`--keep-comfy`) multicard ComfyUI launched with `--use-sage-attention` is not reused by a call sized for the default attention: its
  launch records which attention it was sized for, and `profileMismatch` reads the running argv with the same predicate (so an abbreviation counts), so the harness
  replaces an instance it launched and refuses `COMFY-PROFILE-MISMATCH` for one it did not. Only that direction can run out of memory; the reverse wastes a cold start
  and is reused, and a launch that does not say (the image placements) checks nothing.
- **Tests hermetic.** The two existing multicard tests of `gpu_hostram_test.go` read the process environment through `Facts.Env`, so on a host that exports
  `COMFY_EXTRA_ARGS=--use-sage-attention` their default-share assertions would fail: both now set it to "". The JS tests pass explicit env objects.
- **Tests and mutations.** Eleven JS tests (three for the reuse), four in `internal/hostneed` and one in the root package, written first against a behaviour-free seam
  and seen red on their assertions (the Go mirror test first, against the missing JS export; two JS tests are guards that were green before). Thirty-one one-line
  mutations, each reverted byte for byte, fail a test they name: the sage branch, the attention mode, the token predicate (always false, by substring, from 7 and
  from 9 characters), the attention dropped from `checkResident` and from `geometryNote`, a flat 15.2 below the film size, the line extended below the default's cap, the
  sage figure and the two points moved, the refusal's way out and clause, the summary clause, the plan's `attention`, in Go the sage figure, the detection (never, 7
  characters, substring, the wrong key) and the hint, and for the reuse the check deleted, by literal flag, inverted, applied to the old shape, the profile and the launch.
  Docs: `docs/systems/media-generation.md` ("Sage attention", with the measured table; the launch-profile table), `docs/OPERATOR-GUIDE.md`, `render/README.md`,
  the usage comment of `render/comfy-video.mjs`. Not changed: the image and edit recipes (measured with the default attention), the role rule, the geometry-blind lease sizing.
- **Review round.** The hint on a sage call's ordinary detail (`--ram 0` states the real figure) is set only when the storage card fits its stage, that is when the compute stage
  is the one the film figure pushed out; a storage-card miss no longer blames the geometry-blind sizing. The token rule's two literal copies became ONE file,
  `render/testdata/sage-token-table.json`, read by `TestTheSageTokenRuleMatchesTheRunner` and by the `wanAttentionMode` test (a row changed there turns an answer in both
  languages; the predicate mutants from 7 characters and by substring are RED in both). The refusal threshold above is stated in geometries and pinned at 57,600 tokens (720x1280x61
  still admits the default share).

### Changed — `results[].node` is the node id the placement used; the id the node stamped on its own result moves to the new `results[].node_reported`

- **The defect.** A node named itself two ways. `/fleet/health` and the queue claim take the id `fleet-serve` resolves (`--node-id`, else `fleet_node_id`, else the OS hostname), and every placement text, progress line and dispatch marker names the node by it. The result envelope's `node_id` is stamped by the node's pipeline from `fleet_node_id`, else the OS hostname, and the `--node-id` flag never reached that config. Both installers pass the id by flag only and write no `fleet_node_id`, so a node launched the installers' way published `<node-id>` in health and stamped `<hostname>` on every result. The delegator copied the envelope into `results[].node` and filled the health id in only when the envelope gave none, so one result said `<hostname>` in `node` and `<node-id>` in `placement`.
- **The rule.** `results[].node` is the id the placement used, the `node_id` the node's `/fleet/health` publishes, for a run on a fleet node this delegator dispatched to (a `route: "queue"` answer carries its claimant's stamp, and a run on this box's own seat a box's own name); the result's own id names the node only when health published none. `results[].node_reported` (additive, `omitempty`; `delegate.NodeReported`, stamped in `WireResponse` and in the response of every door that dispatches one contract through `contractOnFleetAt`: the routed `agent_run` / `offload_ask`, the named-seat door of `agent_run(model)` and `offload_review_diff`'s fence fallthrough) carries the id the node's own result named itself by, only when it differs (compared without regard to case), and never on a result no node took. It differs for a node on a build before 0.185.0 and for a local run attributed to another box's engine (register C-58); a node on this build stamps the id it advertises. The delegator-side change takes effect as soon as the delegator is deployed, against nodes of any version. Wire key names are unchanged.
- **A value change, not a rename.** For a remote result from a node whose `--node-id` differs from its hostname (and that has no `fleet_node_id`), `results[].node`, the delegation-log `node`, the ledger row's `model_tier` prefix (`<node>:<seat>`; the dispatch marker already used the id, so one job's marker and finished row now agree), the routed `agent_run` `node`, fleet-smoke's node column and the fleet overview's delegation errors change from `<hostname>` to `<node-id>`. History splits at the deploy: rows written before say `<hostname>`. `rig --node` (and `agent_rig` `node`) matches a row by its `node` or by its `result.node_id`, and the two names select different rows: the card id selects the rows a 0.185.0 delegator wrote and, for a node launched with `--node-id` and no `fleet_node_id` (the installers' form), never a row written before (it carries only the hostname); the hostname selects the rows an older delegator wrote and those a 0.185.0 delegator wrote for a node not yet on 0.185.0, and stops selecting a node's rows once it runs 0.185.0 (it stamps the card id in both fields). For a window that spans the change, run `rig` once per name.
- **Mixed fleet.** A 0.185.0 delegator names the node by the placement id whatever the node runs, with the older node's own stamp (its hostname) in `node_reported`. The queue lane (`route: "queue"`) cannot: the delegator has no health view of the claimant (the holder's poll returns the job's state and data, not who claimed it), so its `node` is the id the claimant stamped on its answer, which equals the id it claims under only once that node runs 0.185.0. The queue lane names the id only once every node runs 0.185.0; until then an answer from an older node still reads its hostname there.
- **Fixed with it.** `fleet-serve` applies the id it resolves to the config its pipeline reads (`fleetServeConfig`), so health, the queue claim and the result envelope carry one id (a box whose hostname lookup fails now stamps the literal `fleet-node` where the envelope was empty, and the GPU activity origin, `(from X)`, shows the resolved id). A local run attributed to another box's engine (register C-58) keeps that box's name instead of taking the runner's envelope name, which the row then carries in `node_reported` (a run on this box's own engine still takes the runner's, as before). The schema-miss rescue is handed the placement id. The named-seat door's withheld-answer reason names the placement id. The `agent_delegate` description lists `node_reported` right after `node` and says what both mean.
- **Tests, red first.** A seam commit adds only the `NodeReported` field and function (returning `""`) and `fleetServeConfig` (returning its config unchanged); the tests follow, and each failed on its assertion before the change, with a positive control beside it that passed. `internal/delegate/node_identity_test.go` (six): `results[].node = "host-bbbb", want "node-id-aaaa"` and `wire node_reported = <nil>` and `delegation-log node = [host-bbbb]` (the control: the placement text and the untouched envelope), `finished row model_tier node = "host-bbbb", want "node-id-aaaa"` (the marker already read the id), `origin node = "host-bbbb", want "fake-node"` (the rescue; its fixture is a budget-class schema miss, as an infrastructure-class one hands no origin at all), the same after a re-placement, and `node = "envelope-local", want the endpoint's host node-b` for a run on another box's engine (the control: this box's own engine keeps the runner's name; green before and after, as is the fallback test for a node whose health names no id). `internal/delegate/wire_node_test.go`: `node_reported is absent, want "h1"` (the table's other five rows, equal ids, case-only difference, no stamped id, no node took it and a queue-lane row, are the byte-identical contract and were green). `fleet_verbs_test.go`: `config handed to the pipeline has fleet_node_id "", want "flag-id"` (and for the hostname and the failed-hostname cases; the config-given case was green). `fleet_serve_nodeid_wiring_test.go` (an AST pin, because `runFleetServe` cannot run in a unit test): `found 0 assignment(s)` of `cfg = fleetServeConfig(cfg, nodeID)`, which must target `cfg`, take the id `fleetServeParams` resolved, and come after it and before `openPipeline(cfg)`. `internal/rig/rig_test.go`: `Build node "host-a" selected 1 row(s), want 2`. `internal/mcpserver`: the description does not carry `{node,node_reported,seat,placement,ran_on,job_id` or the sentences on what `node` and `node_reported` mean (and the existing `placement,ran_on,job_id` check still holds), `node_reported = <nil>` on a routed `agent_run`, and the withheld-answer reason named `host-bbbb`. Twenty-two mutations, each reverted in turn, fail a test they name: eight through `scripts/mutation-check.sh` on the delegator (the placement id replaced by the envelope's, the empty-health-id fallback dropped, the case-insensitive compare made exact or inert, the no-node guard dropped, the stamping dropped, `omitempty` dropped, the rescue back to the envelope id), two on the local arm's endpoint guard (dropped, inverted), three on `nodeMatches` (the stamped-id arm, the empty-node arm, the nil-result guard), five on the MCP door (the key list, its order, the sentence, the routed `node_reported`, the withheld-answer reason), and four on `fleet-serve` (the dropped value, the no-op function, the flag value in place of the resolved id, run on a second host because the root package is not built locally; and the assignment moved after `openPipeline`, which the one-line harness cannot express and was done by hand). One equivalence is left alone and said so: `NodeReported` has no separate empty-id guard, because it returns the id it compares, which is already `""`.
- **Corrected in review.** Four statements were wider than the code, and the code was right. (1) `node_reported` was said to appear for "a node launched under another name"; a node on this build cannot differ (`fleet-serve` stamps the id it advertises, and the pipeline's reloader never rewrites the config), so the field's comment, the guide, the node doc and the `agent_delegate` description now name the two cases that exist: a node on a build before 0.185.0 and a local run attributed to another box's engine. (2) The description said `results[].node` is the health id unconditionally; it now says so for a run on a fleet node this delegator dispatched to and names the queue and local-run exceptions. (3) `rig --node` was said to find the rows written before the change by either name; for a node launched with `--node-id` and no `fleet_node_id` (the installers' form), the card id never selects a row written before (it carries only the hostname), and the hostname stops selecting a node's rows once the node is redeployed, so the guide, the node doc, the `nodeMatches` comment and the "value change" bullet above now say which name selects which rows (the rig test gains a redeployed-node row as a control). (4) The docs and this entry listed `agent_run` and `offload_ask` as the doors that publish `node_reported`; the named-seat door of `agent_run(model)` and `offload_review_diff`'s fence fallthrough publish it too, and the docs now say so (a behaviour test pins the review lane). Each is pinned red first by a test that fails on the retired wording: `internal/delegate/node_reported_docs_test.go`, `internal/rig/node_docs_test.go`, `internal/mcpserver/node_reported_lanes_test.go` and the description test in `internal/mcpserver/ran_on_description_test.go`.
- **Docs.** `docs/FLEET-NODE.md` (the `fleet_node_id` row and the result's `node_id` row), `docs/systems/fleet-node.md` (the result table and a new "Which node" paragraph after "Where a result ran"), `docs/OPERATOR-GUIDE.md` (the delegate section), `docs/systems/delegation-client.md`, `docs/systems/mcp-server.md` and `docs/systems/fleet-overview.md`.

### Added — the display card's cap is reported on its row of the card table, and audit-yaml names the llama-swap entries it cannot judge against it (`gpu_display_cap_pct`)

**The gap.** `gpu_display_cap_pct` (default 60) bounded the card allocator only. A model llama-swap loads onto the display card is
not placed by the allocator, so a 3-card vLLM seat pinned across the display card put 13,281 MiB (81 percent of a 16 GB card) there
while `audit-yaml` printed OK: `servingtmpl.AuditPins` skips an entry that has no `CUDA_VISIBLE_DEVICES` (`pinOf` reports no pin, and the
loop `continue`s), which is every `proxy:` seat and every unpinned entry, and the card table was read only for a file that pins a device.

- **One arithmetic.** `gpulease.DisplayFloorGiB` and `gpulease.DisplayBudgetGiB` are the allocator's display-card floor as pure
  functions (the rest of the card past the cap, or the display layer's declared floor, whichever is larger; the budget is the total less the
  floor, never negative, with no answer without a total). The allocator's method calls the first, behaviour unchanged (its existing
  display-cap tests are green); the surfaces below read the same functions, so none can disagree with the rule a job is admitted by.
- **A live verdict on the display card's row.** `gpucards.Row.DisplayCap` (`cap_pct`, `budget_gb`, `used_gb`, `used_pct`, `over_cap`,
  `over_by_gb`, `closed`, `unknown`) is on the display card's row, plus every row of a reading that could not say which card the monitor is on (there as `unknown`), in `gpu status` (text and `--json`), `gpu cards` and the
  `offload_status` brief's `gpu_cards`: the VRAM in use now, the desktop and every process counted, against the budget. The text table keeps
  its header and gains one `display cap:` line under the display card that says what is counted and that it cannot say whose memory it is.
  `over_by_gb` is present exactly when the card is over, to four decimals of a GiB and never rounded to nothing (nvidia-smi reads whole MiB, so a
  breach of a few MiB is a real reading), and the text line says `OVER by less than 0.1 GiB` under 0.05 GiB, never `OVER by 0.0 GiB`.
  A cap of 0 is `closed` and never over; a card with no total, and every card of a reading that could not say which card the monitor is on
  (the allocator closes such a card too), are `unknown` and not judged. The rows are built by one function, `gpucards.RowsWithCap`
  (it replaces `Rows`, and `Section` takes the same two arguments), so no surface can print the table without the verdict. The cap is read from the
  config file as it is when the call is made (`LiveGPUDisplayCap`), so a `0` written while the MCP server runs shows in the next brief.
- **The brief can now say "unknown".** The brief's table is built from the nvidia-smi sample its verdict already took, which dropped
  whether `display_attached` was read, so on a reading that could not tell the monitor's card it showed no display card and no cap row at all.
  `gpuactivity.GPU.AttachedUnknown` (set by `SampleGPUs`, from `RunDisplayAwareReport`, on every card of such a reading) is passed into the table.
- **`audit-yaml`'s `display-cap-unseen` note.** For a file that has a `proxy:` seat or pins a device (the card table is read for no other file), it prints
  ONE `note:` naming the entries that may sit on the display card: the `proxy:` seats, the entries pinned onto it (by UUID, or by index under
  `CUDA_DEVICE_ORDER=PCI_BUS_ID`) and the unpinned entries of that file (at most six names per group, the groups on the one line), the display card
  (index and UUID tail of this reading), the default cap and its budget in GiB by the same arithmetic, saying that the yaml states no VRAM footprint
  so nothing is judged and entries that can run together (`matrix:`) are not summed, and pointing to the `gpu status` row. A plain index with no
  PCI order is named as unmappable, never guessed. It is never a violation and never an exit code, takes no `--config`, reads no config (the
  cap it states is the default, and says so), and `--other-box` skips it with the existing skip line. The card table is read for a file that pins a device
  or has a `proxy:` seat, once per file and shared with the pin rules; a file with neither still never runs `nvidia-smi`
  (`TestAuditYAMLDoesNotReadTheCardTableWhenNothingPins` is unchanged), so an unpinned entry alone is not named.
- **How the display card is identified.** By the driver's reading, by UUID (`display_active` or `display_attached`, `gpuprobe.Card.Display`), never an index,
  so a monitor moved to another card is followed on the next read, and an entry that was off the display card and is now on it is named without a config edit.
  A single-card or headless box flags none, as for the allocator, and the note is silent; a single-card box is silent even on a reading that could not say
  which card the monitor is on (one card has nothing to tell apart), while a box of two or more cards gets one line saying so and naming no entry.
- **What it cannot see, plainly.** The live row cannot say whose memory it is, and a model loading can flash it over for a moment. The yaml note judges
  nothing: it has no footprint to judge, and it does not read the seat env inside an engine's launcher.
- **Deferred, with the reason.** Footprint-based violations (a declared layer footprint, a vLLM `gpu_memory_utilization` times the card, a seat-env reader)
  and a `doctor` row. There is no placement-neutral footprint source: declaring the opencode-only 3-card seat as a layer would expose it to placement,
  `kv_cache_memory_bytes` voids the utilization figure, and a live yaml with no stamp would turn the session-start audit red. The layer placement
  admission (`placement.displayFloor`) keeps its own arithmetic; this change does not fold the cap into it.
- **Tests, written first and seen red.** A behaviour-free seam commit (types, fields and stubs), then the tests, each failing on its assertion:
  `DisplayFloorGiB(15.9, 60, 0) = (0, false), want (6.36, true)`; `the display card's row must carry the cap verdict`; `card 0: a reading from the fallback after an unexplained
  failure must say it cannot tell the monitor's card`; `the display card's row must carry display_cap` (brief); `pinned onto the display card = [], want [...]`;
  `exactly one display-cap-unseen note line, got 0`; `the card table was read 0 times, want 1`. Some tests were NOT red first and are proven by a named mutation instead: the two declared-display-floor tests (`gpu cards` and the brief), the held-host `gpu status` text test and the cap-95 halves of the `gpu status` and `gpu cards` tests (each call site of the table), the two float-tolerance rows of `TestDisplayCapComparesUseToTheBudgetAndAnExactFitIsWithin` and the NaN-marshal assertion of `TestDisplayCapNeverGuessesAnUnreadableTotal` (the over comparison without its tolerance, and the no-total guard).
  Mutations, each applied alone with `scripts/mutation-check.sh`, each RED: the floor
  uses the cap instead of the rest of the card, the larger floor no longer wins, the budget is not clamped or has no total check; the over comparison inverted or
  without the float tolerance, free compared instead of used, the cap applied to every row, a cap of 0 not closed, an unknown reading ignored or printed once
  per card, a missing total unguarded, the section or the declared floor ignoring the cap; `SampleGPUs` never marking an unknown reading; the brief reading the cap at load
  (killed by `TestBriefReadsTheLiveDisplayCapNotTheOneAtLoad`, which rewrites the config file after the server is built), ignoring the declared floor, or dropping the unknown reading;
  in the note: an index with no PCI order mapped as an nvidia-smi index, a proxy seat not classified, every card judged a display card, an index or UUID pin ignoring the
  display flag, no display-card guard, seven names per group, an unknown reading ignored, the budget at the wrong cap, and the audit's cmd-over-env order inverted; in the wiring (remote gate):
  the proxy seat not reading the table, the table read for every file, `--other-box` ignored, the note dropped, the declared floor not passed, and each call site of the table ignoring the cap (`gpu status` json and both text branches, `gpu cards`). One mutation survives and is equivalent
  by construction: reading the cap at load instead of live in the one-shot verbs, where the config is loaded in the same call; the live read matters in a long-lived process, which is the brief, and
  that mutation is killed there.
- **Review round: five distinct findings (the help-text one was raised twice), four fixed and one refuted.** Each fix has its test written first and seen red on its
  assertion (no new symbol, so each compiled against the commit before), then the fix, then one-line mutations, each RED.
  (1) The `audit-yaml` help and the `runAuditYAML` comment promised the note to a file with an unpinned entry; the code reads the card table only for a file that pins a
  device or has a `proxy:` seat, so the words were wrong and the behaviour stands (`TestAuditYAMLHelpDoesNotPromiseTheNoteToAFileThatNeitherPinsNorProxies`).
  (2) The list of tests written after the red commit, above, was incomplete; it now names them all. (3) A breach under about 5 MiB gave `over_cap` true with an amount of 0
  (the key dropped from the JSON) and the text read `OVER by 0.0 GiB`, which it also printed from 0.01 to 0.049 GiB: `over_by_gb` is now four decimals and never below 0.0001
  for an over card, and the verdict is unchanged (still the allocator's, on unrounded values). (4) A single-card box on a reading that could not say which card the monitor
  is on printed the "no display card known" note, against the contract that a single-card box is silent; it is silent now. The card table's `display cap: not judged` line on
  such a one-card reading is unchanged: it mirrors the allocator, which closes that card too. (5) Refuted: the cap of six names is per group, not per line. The design
  listed at most six names per aggregated line, one line per group; the note became one line, and a total of six would have left the later groups (the plain indices that
  cannot be mapped, which need an operator's eye) as a bare count behind a long list of unpinned entries. The worst case is 24 names on the line, and the docs say per group.
- **No inference placement changes.** Everything here reads; nothing loads, unloads or moves a model, and the allocator's decisions are the same.

## [0.184.0] - 2026-10-12 - a busy fleet is a place in line in every row, a re-pack that empties its answer's lists is refused, every delegate result says where it ran, offload_status names the image a server runs, and the serving audit refuses a bare --cpu-moe and an index pin on identical cards

### Fixed — integration review round of 0.184.0

Six findings from an integration review of the merged release branch, each read against the code and fixed, with its test
written first and seen red. A second look at the same fixes follows the six (the last bullets of this block).

- **The queue lane's rescue dialled the queue holder with the claimant's seat name.** `takeQueueAnswer` sets the row's dial base
  to the holder so that `ran_on` reads `node` (D-32). The rescue of a schema miss is handed an origin (the node that wrote the
  answer, to re-pack on its hot seat) whenever the row has a base, and the queue lane calls the rescue for every schema miss, so
  the rescue's first rung was a chat call to the holder's address for the claimant's seat, and its note named a node that had
  not written the answer. The row now carries `viaQueue` (a `PlacedResult` field set in `takeQueueAnswer`, which both arms that
  read a finished answer come through), and the origin is handed over only for a row that did not come through the queue.
  `ran_on` is unchanged (`node`); the queue lane's rescue goes to the delegator's own seat, as it did before the origin existed.
  Tests `internal/delegate/queue_rescue_origin_test.go`, written first: before the change the rescue was handed
  `{Base: <holder>, Seat: sim-seat, Node: sim-node}` for a budget, an abstention and a class-less defer, and again for a rescue
  that went on to fail (an `infrastructure` defer hands no origin on any route, which is why the existing queue rescue test,
  whose claimant filed one, could not see it). Three mutations, each reverted in turn, fail a test they name (the gate dropped,
  the mark dropped, the base not set, which also fails `TestRunQueueRouteEndToEnd`). Docs: `docs/systems/coding-agent.md`
  (the rescue ladder) and ADR 0055 item 11.
- **The two staleness signals disagreed on the same bytes, and the guard's start was taken after the capture.**
  `offload_status`'s `process.stale_image` compares bytes (equal bytes read fresh, pinned by
  `TestStaleImageIsFalseForTheSameBytes`); the `stale_binary` guard compared the size and modification time only. A redeploy of
  the same build rewrites the file with a later time and the same bytes, so the same answer carried `stale_image: false` and a
  note saying the image had changed on disk. The docs told two stories to match: the guard's said the version on disk is not
  read, the block's said `installed_version` is read from the installed file's build marker. There is one definition now: the
  image is stale when the file at the path the process started from holds other bytes than the image it started from. The guard
  starts from the identity the process captured first thing (`buildinfo.StartupIdentity`: the stamp read through the handle the
  capture hashed, and the path it recorded), and when that stamp has moved it asks the check the `process` block makes (the same
  hash, the same per-stat cache, the same build marker). Equal bytes are not stale. Different bytes are, and the note names
  the installed build when its marker reads. Bytes that cannot be compared (a lock on the freshly renamed file, a process
  whose own hash failed at start) leave the moved stamp standing and the note says it rests on the stamp alone, so a guard
  against a silent failure does not go quiet when it cannot look; the next call retries. A path that is gone is stale without a
  comparison. A server that holds no identity (the test seam) decides on the stamp as before, and a process that captured
  nothing starts the guard from its own file as of now. The start stamp was also read in `mcpserver.New`, after
  `loadCfgWithSource` and `openPipeline`, which re-opened the start-up window the capture closed (D-36): a deploy landing in
  between was read as the image the process runs and the guard accused nothing for the life of the process. `runMCP`'s
  source-order pin now also requires the capture to come before `mcpserver.New`. Tests `internal/mcpserver/stale_agreement_test.go`
  (eleven), written first: six of the first nine failed before the change, for example `stale_binary says "this MCP server is
  running 0.183.0 from an executable image that has changed on disk ..." while process.stale_image is false for the same bytes:
  the two signals disagree` and `stale_binary = "" for a swap between the capture and New, want the changed-on-disk note`; the
  other three guard the halves that did not change (different bytes are stale on both signals, a swap of equal bytes between the
  capture and `New`, a process with no captured identity). The commit that wrote them adds one behaviour-free seam,
  `startupIdentity`, so that they compile before the fix. Six mutations, each reverted in turn, fail a test they name (equal
  bytes no longer clear the note, an unreadable comparison silenced, the captured identity ignored, the installed version not
  named, the Server passing no comparison, and a gone path compared as well). The last survived at first, because a comparison
  of a path that is gone gives the same words; two tests with a spy on the comparison kill it. Docs:
  `docs/systems/mcp-server.md` says which signal rides which door, that they share one capture and one definition, and that the
  installed version is read from the build marker and never by running the file; the D-36 and stale-binary entries below say
  the same, and no longer say the version on disk is not read.
- **`docs/systems/mcp-server.md`'s Source map lacked `stalebinary.go`, and the page said "`mcpserver.Run` takes the identity"
  twice.** The file is listed and the second sentence is gone (the first stays).
- **`docs/FLEET-NODE.md` still stated the old quarantine rule.** It said a node is quarantined "after two results inside that
  window fail the document fingerprint"; since 0.184.0 the failures must also outnumber the node's passes of the same check,
  and an honest absence counts as neither (`docs/OPERATOR-GUIDE.md` and `docs/systems/mcp-server.md` already said so). The page
  states the rule the code has (`delegate.Quarantine.Strike` and `Pass`).
- **The `agent_delegate` tool description lists `results[].ran_on` and says what it means** (the follow-up the D-32 entry named
  for after its branch merged). `internal/mcpserver/ran_on_description_test.go`, written first: the keys were listed without
  `ran_on` and nothing told a reader to prefer it to the word "local" in `placement`.
- **The fleet node takes its startup identity as the first statement of `runFleetServe`, as `runMCP` does** (the follow-up the
  D-36 entries named). A node stamps `harness_build_sha256` on its agent answers and lives as long as an MCP server, so a deploy
  landing after it started must not be hashed, at its first answer, as the image it runs. The source-order pin of
  `mcp_startup_capture_test.go` now covers both functions; before the change
  `TestRunFleetServeCapturesTheStartupImageBeforeOpeningAnything` failed with `runFleetServe must call buildinfo.CaptureStartup()
  itself, before it opens anything`.
- **Not changed.** The queue lane's rescue still passes no strike (a rescued object that fails the document fingerprint does
  not count against the claimant), as the queue route is not covered by quarantine. It also hands the rescue no origin when the
  claimant is the queue holder itself (a node that holds the queue and claims from it): the answer names the claimant by node id
  only, and resolving that id to a dial base would mean probing the fleet from a lane that has no runner and no health view
  (ADR 0030), so the rescue goes to the delegator's own seat, which is what it did before the origin existed. A wrong origin is a
  call that fails or answers for another seat, so the lane withholds it whenever it cannot tell.
- **A second look at the six, five applied and two answered.** A silent-failure review of the fixes above found these; each has
  its test written first and seen red.
  - *The `stale_binary` note dropped why the bytes could not be compared.* It said "could not be compared" and kept the read
    error (a lock on the freshly renamed file) for `offload_status` alone; the note, and so the one log line, now ends the
    sentence with the comparison's own reason. `internal/mcpserver/stale_reason_test.go`.
  - *A guard that was not watching said so only on `offload_status`.* `agent_delegate` and `offload_research`, the doors the guard
    exists for, answered with no note, which reads as a fresh server. Both now carry the top-level `stale_binary_guard` string
    (`inactive: ...`, or `unreadable: ...` while the stamp cannot be read), read after the note's own read of the stamp, and no
    key while the guard watches. `internal/mcpserver/stale_guard_doors_test.go` (three tests, each run on both doors: two failed
    first on both doors, and the third is the control that a watching guard adds nothing).
  - *A start-up capture without a stamp re-opened the window the capture closes, silently.* The self-hash set the capture's path
    before it opened the file and took the stamp only through the open handle, so a refused open (the antivirus lock its own log
    line cites) left a path and no size, and the guard fell back to reading its file as of `mcpserver.New`: a deploy landing in
    between was taken for the running image with no line saying the fallback happened. The capture now takes the stamp from the
    path when the open fails (and logs a failed stat, which was discarded), and a guard whose capture holds no path or no stamp
    is inactive, logs one line naming the capture, and shows the reason on the doors above. Tests
    `internal/buildinfo/stale_test.go` (the locked image keeps its stamp) and `internal/mcpserver/stale_capture_test.go` (two);
    the commit that wrote them adds one behaviour-free seam, `openImage`.
  - *Settling a moved stamp by the bytes put an unbounded read on the answer path.* Two passes over the installed executable (the
    hash, then the build-marker scan) ran after every result existed, so a read that stalled held the finished results past the
    call's deadline, and a failed read was retried by every later answer. The read now runs in the background, once per moved
    file (calls that arrive while it runs wait on it and start none), and a call waits at most 2 s (`installedReadWait`): past
    that `process.stale_image` is absent, the note keeps the moved stamp and says the read did not finish and goes on, and the
    next call records what it found; a read that failed is still not cached, and a file that moved again while it was read gives
    no verdict from the older read. `offload_status` shares the bound. `internal/mcpserver/stale_bounded_test.go` (three; the
    stalled read is held 1.5 s in the test, and before the change the answers waited out all of it).
  - *The quarantine strike was the second reader of a queue row's base that is wrong for it.* The queue lane passes no strike
    today, so nothing fired; a strike wired for it later would have quarantined the holder for a claimant's fingerprint misses.
    The strike is now skipped for a row the queue lane took (`viaQueue`). `internal/delegate/queue_rescue_strike_test.go`; the
    push row's strike is the control and still passes.
  - *Answered, not changed: the PAIR card.* The note that a queue row's PAIR card (`pairevents.go`) might name the holder is
    withdrawn. `pairTerminal` runs only from the push path's `attempt`, and its guard is the model name only `pairInflight` sets,
    which the queue lane never reaches (`runQueued` is called before the runner is built), so a queue row emits no card.
  - *Answered, not changed: the holder-as-claimant origin.* The blunt `viaQueue` gate is the first bullet of "Not changed" above.

### Fixed

- **The rigger filed a busy fleet as a wall timeout and a node's "queue full" as a broken serving stack.** In one day's report
  24 of the 26 bad rows of the local agent pool were capacity defers ("capacity wait: no node had room ... the call's
  deadline"), and every one of them landed on the `timeout` axis, whose remedy is "raise `timeout_sec`": the wall-timeout
  pattern matches the bare word "deadline", and `timeout_sec` never bounds that wait (ADR 0073). Thirteen more rows were
  dispatches a node had answered 503 "queue full" and the delegator outwaited and re-placed, and `seat-infra` ("fix the
  serving stack") took them although no seat was asked. There is now a `capacity` axis, behind `call-deadline` and ahead of
  `timeout`: a row of defer class capacity lands on it whatever its words, and so does a row with no class whose error is a
  node's 503/429 "queue full" / "shed" / "no idle execution slot" answer (a row from a delegator that predates the
  marker below). Its remedy names `agent_call_deadline_sec` and `agent_placement_wait_sec`, the keys that bound the wait.
  A dial that never reached the node and a 503 about something other than room stay `seat-infra`. A row carrying the new
  `refusal` marker (`{"status":503,"retry_after_sec":N}`; delegators write it on a capacity-refused attempt) is not a bad
  row at all: it is counted on the report's own `refusals` line, outside `rows`, so a fan-out that met a full node reads as
  the place in line it was. The call-deadline and the wall-timeout verdicts are unchanged (`rig/calldeadline_test.go`). Tests
  `internal/rig/capacity_test.go`, written first: red before with, for example, `held slot, the 0.180.0 wording: classified
  "timeout"`, `no deadline word at all: classified "unclassified"` and `an unmarked 503 queue full row classified "seat-infra"`;
  five mutations (the capacity-class check off, 429 dropped from the pattern, a refused row counted bad, refusals counted in
  `rows`, the precedence swapped) each turn them red. The `agent_rig` tool description, which is what the delegating model reads,
  publishes the same order (it still listed the old one, so a caller would have filed a full fleet under `timeout` and a
  forced-final tool call under `budget`), names the wait keys as the capacity lever and the `refusals` line; a test reads
  `rig.Precedence` and holds the description to it in order (`internal/mcpserver/agent_rig_description_test.go`, red before with
  `axis "capacity" is missing from the description's precedence order`, and three mutations). Docs:
  `docs/systems/coding-agent.md` (the rigger paragraph).
- **A node's capacity refusal is a place in line in the delegation log, on the fleet page and in the rigger, and a subtask
  lost to a full fleet now has a row that says so.** The capacity wait outlasts a node's 503 or 429 at dispatch, but the
  refused attempt's corpus row was an ordinary error row (`deferred` false, `acceptance_pass` false), so thirteen of them in one
  day were filed as serving-stack failures and listed as `delegation` errors on the fleet page, while nine of the twelve
  subtasks behind them landed on another node seconds later. The other half: a wait that ended as a capacity defer after a
  refusal wrote no row of its own (`settle` returned when `attempts > 0`) and carried the refused attempt's job id, so for the
  subtasks that really were lost the refusal row was the only record, and it read as a dispatch error. Now the row of a
  capacity-refused attempt carries `refusal: {status, retry_after_sec}` (additive; the ledger row is unchanged, still deferred
  with `reason_code` `queue_full`), and a marked row is never the end of a subtask: a wait that ends as a capacity defer and a
  chain that ends as the "placement refused" failure each write a terminal row under a job id of their own (the way a cut chain
  already did, ADR 0065), and the caller is handed that id. The fleet page and `top` no longer raise a `delegation` error for
  a marked row and show it as `refused (re-placed)`. The cooldown, the re-placement and the wait are unchanged, and so are the
  rows of a refusal that is not about capacity and of a job the node acked and then took back (a queue deadline it confirmed,
  a record that says it never ran): that job was held under an id the node's record and the intent ledger know, so its row is
  not a refusal mark and the failure of a subtask nobody else took keeps that id (found by a Linux gate host:
  `TestWithdrawAgainstTheRealNodeHandler` read the node's record under the published id, and a first version of the mark had
  replaced it). The ledger now holds two rows where a lone refused dispatch with the wait off ended the subtask (the attempt
  and the "placement refused" failure; `TestOnlyAJobANodeAckedCarriesAFleetJobID` pinned one). A test that pinned the old "the attempt's row alone" ending
  (`TestASettledOutcomeThatIsNotACutKeepsItsAttemptsRow`) is rewritten to pin the new one. Tests
  `internal/delegate/refusal_row_test.go` and `internal/fleetview/refusal_test.go`, written first: red before with, for
  example, `a 503 refusal inside the wait is a bare error row, want a refusal marker`, `the terminal row = ... deferred:false
  ..., want deferred, class capacity` and `the errors feed carries the refused attempt as an error`; the delegate tests read the
  rows back through the rigger, so the writer and the reader cannot drift. Eight mutations (the marker never set, the closing row
  skipped for a refusal, the failure's terminal row skipped, the marker left off the line, the flag kept on the published
  outcome, the mark kept after a landing, the fleet page folding a marked row, its field dropped from the decode) each turn
  them red. The chain's own ending reads the placements ledger (the row of the latest attempt that wrote one, as `settle` does)
  and not only the seed it is handed: a replacement the process gate turned away returns before it writes a row, so a wait that
  began at the gate after a refusal and ended on the budget had left the refusal-marked row as the only record of the lost
  subtask (`TestAChainEndedAfterAGatedTurnAwayFilesATerminalRowWhenTheLastRowWasARefusal`, red before with `the failure carries
  no job id: the refused attempt's row is a refusal, so no row says how the subtask ended`; four more mutations, one on each
  reading of the ledger and the seed). Docs: ADR 0073 (amendment 2026-10-12), `docs/systems/fleet-overview.md`,
  `docs/OPERATOR-GUIDE.md`.
- **An MCP server whose binary was replaced after it started now says so on the doors a release changes.** A server is spawned
  once per client and keeps the executable image it started from. Two servers that had started on 0.180.0 were still stamping
  their corpus rows with it eight hours after a 0.181.0 and then a 0.182.0 binary had been swapped in on disk, so the
  capacity-wait fix those releases carry (a wait that holds a run slot gives it up and keeps waiting) could not run for any
  call they served, and nothing in the server noticed its binary had changed. (The 24 subtasks that a day's report shows lost
  to the old ending were served before that fix existed; the next saturated fan-out through a server that outlives a release is
  what this guards.) The server now takes the size and modification time of its own executable at start (the image the start-up
  capture hashed, see the integration review round at the top of this section) and compares them with the file on disk when
  `agent_delegate`, `offload_research` or `offload_status` answers: a stamp that moved, and bytes that differ from the image it
  started from (or cannot be compared), or a path that is gone, put a top-level `stale_binary` string on the answer (running
  version, the two stamps, the installed version when the installed file's build marker reads, "restart the MCP client to load
  it") and one line in the log. The work still runs and nothing is interrupted or re-executed, a stat that fails for another
  reason accuses nothing, and the installed version is read from the installed file's build marker, never by running it. The guard says
  when it is not watching, because a guard against a silent failure must not be one: a server that could not stamp its own
  executable at start (or whose path was already gone) logs one `the stale-binary guard is inactive` line with the reason, a stat
  that fails later is logged once, and `offload_status` carries a top-level `stale_binary_guard` string (`inactive: ...` for the
  first, `unreadable: ...` for as long as the second lasts), absent while the guard is watching (`agent_delegate` and
  `offload_research` carry it too since the integration review round at the top of this section). Tests
  `internal/mcpserver/stalebinary_test.go`, written first: red before with `agent_delegate: no stale_binary note in [summary
  results]` on a server whose executable stamp had moved, on all three doors, and, for the inactive guard, `log "": want one
  line saying the guard is inactive and why`; thirteen mutations (size or modification time dropped from the comparison, the
  start-stamp guard, the vanished-path branch, each door's wiring, and on the inactive guard: the log line, the once-only log, the
  remembered failure, the reported state, the gone-at-start reason, the status key) each turn them red.
  Docs: `docs/systems/mcp-server.md`.
- **A forced final step the seat answers with a tool call is re-asked once, with the offence quoted, instead of ending the run.**
  The last step of a multi-step run offers no tools and asks for the answer (D-89). One seat family wrote a tool call there,
  as text (`<tool_call><function=...>`) or parsed, in 35 of the 106 runs that reached a forced final (none of 241 on every other
  seat), 30 of the 35 with more than half their wall unspent and the evidence they needed already read: the run returned
  `budget` the moment it happened, with the seat's text thrown away and only the marker kept. A bare retry would have been a
  no-op, because the decode of that step is greedy (`temperature=0` in the logged call records), so the re-ask changes the
  prompt: a user turn that quotes the offending text (or the tool and its arguments), says nothing written as a call will run,
  forbids the tool syntax and says to begin with the answer in the requested shape; the poisoned turn is kept out of the
  transcript, the answer-now turn is not repeated, the step is not spent, and the budget and thinking-off are those of the first
  final. It is gated on the wall and bounded at one. The gate is the seat's list-cap floor where one is known and, in every case,
  the wall of the final call just made plus a quarter (`wallHoldsAnotherFinal`), because the list-cap re-issue's own gate fails
  open for a contract without an output_schema and for a seat with no known rate, and a re-ask spent there let the deadline fall
  INSIDE the call: the run ended as an error with the offence lost, and the delegator, which re-places only the forced-final
  budget defer, never gave the contract another seat. A second tool
  call ends the run on `budget` as before, now with both replies quoted in `stop_note` (`core.ForcedFinalToolCallReason` opens
  it, and sits inside the defer reason). An answer is `done` and `stop_note` says the seat first wrote a tool call and was
  re-asked once. **Not measured on a live seat** (no model was started for this change): whether a quoted-back, stricter
  instruction stops this family writing the call is an inference from the history-continuation reading and from the tail
  correlation in the corpus (44% failures when two of the last three trace entries were refused repeats, 9% otherwise); the
  fallback, if a replay of the logged contracts shows it still fails, is to render the tool turns as plain observations for the
  re-ask (a full re-prefill, about 30 s). Tests `internal/agent/forced_final_reask_test.go`, written first: red before with
  `stop="budget" output="" steps=2, want the re-asked answer as a finished run in 2 steps` and `the seat was called 2 times,
  want 3: one re-ask and no more`; the two tests that pinned the one-shot give-up (`TestForcedFinalToolCallIsNotExecuted`,
  `TestForcedFinalToolCallWrittenAsTextIsABudgetStop`) now script two offences in a row; the wall tests (a contract with no
  rate, a margin over the last turn, no deadline at all) were red before with `the run ended in an error (fakeClient: script
  exhausted) with stop "error": the re-ask was spent with 200 ms of wall left and the deadline fell inside it`. Thirteen mutations
  (the wall gate, the bound of one, the quoted text, the poisoned turn kept, each note, and on the gate: the measured turn not
  read, no margin, the seat's floor ignored, a comparison too strict, a missing deadline read as a limit, the last turn not
  passed, the original gate back) each turn them red. Docs: `docs/systems/coding-agent.md`,
  `docs/OPERATOR-GUIDE.md`.
- **A forced-final tool call is its own shape in the rigger and gets a second seat, instead of "a task too large: split it".**
  The shape (the forced final step answered with a tool call, again after the re-ask above) ended on `stop_reason` `budget`,
  and the rigger filed it on the `budget` axis, whose remedy reads "a run at the cap with no loop axis is a task too large for
  one contract: split it". It was not: in the three rows of the day the seat had read the whole document (or all that read_file
  could give it) and used 43 s of 600 s, 16 s of 299 s and 86 s of 485 s; and because `budget` outranks `loop` and
  `tool-misuse` in the precedence, the refused repeats in their traces never surfaced. The rigger now has a `final-tool-call`
  axis, ahead of `budget` and keyed on `core.ForcedFinalToolCallReason`, whose remedy says cutting the contract up cannot help.
  And the delegator treats the shape as the fifth defer worth a second placement (`delegate.FinalToolCallDefer`, beside the
  seat-down and lost-root defers): it is a property of that seat (none of 241 forced finals on every other seat did it), the
  contract is sound and the wall mostly unspent, and a plain step-budget defer, which a second seat does not fix, stays as it
  was. With nowhere to place it the caller gets `retry skipped: the seat on <node> wrote a tool call as its final answer and no
  other node could take the contract (...)` instead of an empty note. Its second placement is placed the way a seat-down
  retry is, and so is the lost-root defer's (one predicate, `delegate.producedNothingDefer`): a run that produced nothing has no
  answer to hold, so the retry is not made to stand in line for a busy retry seat for the placement wait and then skipped
  (every remote publishes one job at a time, so on a saturated fleet that was the normal outcome: "retry skipped: the retry seat
  on <node> is already running another job after waiting 5s in line"), the failed seat's `min_turn_sec` is not the retry seat's
  floor when the retry node publishes no rate (`retry skipped: 299s of the 300s timeout_sec budget left after choosing a retry
  node (floor 1112s, min_turn_sec of the seat on <node> (the retry seat published no rate))`), and a local seat whose run-cap line
  has no free slot is left for an untried remote. Tests `internal/rig/final_tool_call_test.go`,
  `internal/delegate/final_tool_call_test.go` and `internal/delegate/final_tool_call_retry_test.go`, written first: red before
  with `classified "budget" ... want final-tool-call`, `retried=0 deferred=true note=""` and the three skipped-retry notes above
  (the lost-root defer failed the busy and local-line ones the same way); mutations turn them red. Docs:
  `docs/systems/coding-agent.md`, `docs/systems/fleet-node.md`, ADR 0066 (amendment 2026-10-12).
- **A research page that is one very long line is handed to the seat in lines it can read.** The agent's `read_file` cuts every
  line at 2,000 characters (`(line truncated)`) and its `offset`/`limit` count lines, so the rest of a longer line cannot be
  reached at all. Two of the three forced-final rows of the day were such pages: one was a single 98,304-character line (the seat
  could read its first 2,020 characters), the other had two lines holding 97,544 of its 98,299 characters (4,806 were
  reachable), and no final ask can ground an answer in text the seat never saw. Across the corpus, research runs on a page with
  an unreadable line reached the forced final step in 38 of 117 cases against 36 of 1,283 for pages of ordinary lines (the
  failure rate once there is about the same, 30% against 41%, so this is what makes the cap get reached, not what the seat does
  at it). `research.Build` now wraps each context document at `research.MaxLineChars` (1,800, pinned under the new exported
  `agent.ReadFileLineCap` by a test in the MCP server package): at the last space in the final quarter of the window, in the
  middle of a run with no space (a base64 blob), never inside a rune; the space becomes the newline and nothing else is added or
  removed, and a page whose lines all fit is handed over byte for byte. The text that anchors, fingerprints and verifies quotes
  is the page as fetched, so those checks are unchanged. Not changed: a context document supplied by a caller of
  `agent_delegate` (its lines are the caller's; `read_file` still truncates them and says so). Tests
  `internal/research/build_wrap_test.go` and `internal/mcpserver/readable_context_test.go`, written first: red before with
  `line 1 is 125999 runes: the seat's read_file would cut it at 2000 and the rest would be out of reach`; five mutations (the
  wrap not applied, the early return, the break window, the hard-cut width, the cap constant) turn them red. Docs:
  `docs/systems/mcp-server.md`.
- **The document fingerprint no longer fails an honest absence or a plural/singular restatement, and no longer passes the goal
  echoed back.** The research acceptance check is a wrong-document tripwire: one any-of alternation of the page's own prose
  words, kept off the goal's vocabulary. Nine of 254 research answers in two days failed it, and every one was a faithful or
  honest-absence digest, none off-document (a digest that names the document correctly and quotes it verbatim failed for
  writing "specification" where the page's anchor was "specifications"; another was five "not on page" and a verdict). The goal
  tells the seat to write exactly that, in the goal's own words, which the anchors exclude by construction, so an honest absence
  could never pass. Each anchor is now matched in its singular (and the goal's words are excluded in the singular too), and an
  honest absence is excused by the delegator from the STRUCTURE of the digest, not from a phrase in the regex:
  `delegate.EvalAcceptance` drops the fingerprint failure of a result whose structured digest is `delegate.HonestAbsence`, an
  object in which every string at any depth (a list entry as much as a field) is empty or a short statement (60 words at most)
  that the page does not carry what was asked, with at least one such statement, no number or boolean, no second sentence that
  says what the page does hold, no contrast word ("but", "however") and no report of a failure to read the file ("could not
  read", "not found", "the file is missing"). Such a result is counted for the node as neither a strike nor a pass. An earlier
  draft of this change appended the absence phrases to the regex as a last alternative, and that was withdrawn before release:
  the goal itself contains them (the built goal says "does not contain" and "does not address it"), so every anchored research
  contract read PARROT-PASSABLE in the lint that is published on the result, the goal echoed back passed the check, and a stock
  phrase ("not specified") anywhere in a digest of the wrong document passed it. The anchors are also chosen off the whole goal
  the seat is handed (the fixed sentences, the file's name and title, the questions) and not only the caller's: the template's
  own "Always end with a one-sentence verdict" had made "always" an anchor on a page that uses the word. The cost is a smaller
  pool: a page word that also appears in the goal's fixed sentences, or in the page's own title or file name, is no longer an
  anchor (a page too thin to leave 4 usable words, `minAnchorWords`, has no fingerprint, as before). The check stays one regex
  tagged `docanchor`, holding the page's words and nothing else. Not closed, and not closable by a lexical rule: a seat that did
  not read the page and reports "the page does not address it" without saying it failed is delivered as an absence, as an honest
  one is. Tests `internal/research/anchors_abstention_test.go` and `internal/delegate/absence_test.go`, written first: red before
  with, for example, `a plain goal: the contract is flagged parrot-passable: acceptance is PARROT-PASSABLE`, `the goal echoed
  back as the answer passed the fingerprint`, `facts about another subject, one of them hedged: passed the fingerprint` and
  `could not read: "I could not read the file, so the page does not address the goal." passed the fingerprint`; twelve mutations
  (the anchors chosen off the caller's goal alone, an absence alternative back in the regex, a number or boolean allowed, the
  read-failure and contrast vetoes, the sentence rule, the vacuous-sentence guard, the word cap, the at-least-one-statement
  guard, the excuse dropped, the excuse widened to any check, an excused absence counted as a pass) each turn them red. Docs:
  `docs/systems/mcp-server.md`.
- **A node that passed most of its research work is no longer quarantined by two misses of a lexical check.** A research answer
  that fails the document fingerprint (`docanchor`) is a strike against the node that served it, and the second strike inside 30
  minutes took the node out of the session's placement for 30 minutes ("quarantined until ..."), the whole point being to stop work
  going to a node that answers about the wrong document. The check is lexical and misfired on honest answers (see the entry
  above), so two misses of a node that had passed 48 of its 51 answers that day quarantined it while the rest of the fleet stood at
  "queue full". A node that is truly off-document fails more often than it passes, so the
  quarantine now needs at least two strikes that OUTNUMBER the node's passes of the same window: `Quarantine.Pass` records a result
  that carried the fingerprint check and passed it (`runner.noteFingerprint`, at the remote result and at the delegator-side
  rescue), a result with no such check says nothing, and passes age out with the strikes. A node with no passes is quarantined on
  its second strike exactly as before. An honest absence (the entry above) is neither a pass nor a strike. A stated limit, on
  purpose: a node that answers about the wrong document less often than about the right one (4 misses beside 6 passes in the
  window) is not quarantined, because a failed fingerprint keeps its cross-seat retry, so such a node's miss is never delivered
  as verified (one whose retry cannot be placed is published as a failed verification) and each miss costs one retry elsewhere, while idling a node on a lexical check costs the fleet a whole seat for the window. The
  quarantine stays per server process and is never persisted. Tests
  `internal/delegate/quarantine_pass_test.go`, written first: red before with `two failures after three passes quarantined a node
  that answers about the right document`; six mutations (the outnumber rule, the two-strike floor, the pass record, the
  carries-the-check guard, the call site, the window) turn them red. Docs: `docs/OPERATOR-GUIDE.md`.
- **A verification retry is no longer refused for the first seat's `min_turn_sec` before a retry node is even chosen.** The retry
  of a wrong answer (or an honest abstention, or the forced-final tool call above) goes to a DIFFERENT node, and the node it goes to
  has its own floor, applied once it is chosen (`retryFloorOn`: its published `seat_rate`, the cold load and the final turn plus
  the re-pack at its own rate). But the cheap check before that read the first attempt's own `min_turn_sec` for every retryable
  shape except two (the seat-down and lost-root defers, exempted for exactly this reason), so a research digest that failed
  acceptance on a seat whose cold load and two 8,192-token turns are measured at 1,112 s (3,379 s on a slow unified-memory
  node, 889 to 951 s on the local pool) was refused its retry on a 300 s or 600 s contract before any node was asked, although
  that seat had taken 20 to 325 s to give the answer: in one day 7 of the 14 verification retries skipped were refused that way
  (the others fell to other rules: the chosen node's own floor, a busy retry seat, a fenced local seat, a rescue with no budget
  left). The check before a node is chosen now reads the
  configured floor (`agent_retry_min_sec`, else 10 s) alone; the chosen node's own floor, the budget left and the retry note
  (`retry skipped: ... after choosing a retry node (floor ...s, min_turn_sec published by <node>)`) are unchanged, and a retry
  node that publishes no rate still falls back to the first attempt's number, said in the note. Not changed, on purpose: a
  VERIFICATION retry's wait for a busy retry seat keeps its placement-wait TTL (ADR 0073 decision 3: it is an optional second
  opinion on an answer the subtask already holds); a retry of a run that produced nothing (seat-down, lost-root, forced-final tool
  call) holds no answer, so it neither waits for that seat nor is refused for the failed seat's `min_turn_sec` (see the
  forced-final entry above). Config, for the operator: with `agent_retry_min_sec` at 300 a default 300 s contract can never retry,
  since what is left is always under it. Tests `internal/delegate/retry_first_seat_floor_test.go`, written first: red before with
  `retry skipped: 299s of the 300s timeout_sec budget left after the first attempt (floor 1112s, min_turn_sec of the seat on
  node-a)`; three mutations turn them red. Docs: `docs/OPERATOR-GUIDE.md`.
- **A retry that found no node to go to is no longer published with an empty note.** When the engine considered the second
  placement of a wrong answer or an honest abstention and no other node could take the contract, the result carried an empty
  `retry_note` unless it was one of a few named shapes (a fence, a reservation, the call's deadline, a reasoned pin, or a defer that
  promises a second placement: seat-down, lost-root, forced-final tool call). It read as a retry nobody considered, and a research
  page skipped this way left nothing to log and nothing to fix. The note is now always there: `retry skipped: no other node could
  take the contract after the first attempt on <node> (<outcome>); every eligible node was already tried or is not eligible`
  (or `route local places nothing on another node`). Two tests that pinned the empty note of a bare route's skipped retry
  (`TestRunRetryNeverLandsOnASeatTheSubtaskAlreadyUsed`, `TestTheBareRemoteRoutesRetryNoteNamesNoPinOnASeatlessBox`) now pin the
  generic note, and the second still asserts that no pin wording appears where nothing was pinned. Tests
  `internal/delegate/retry_nowhere_test.go`, written first: red before with `retry_note = "", want it to contain "retry
  skipped"`; three mutations turn them red. Docs: `docs/systems/fleet-node.md`.

### Fixed — a re-pack that emptied the lists its answer held is refused, a re-pack the schema bounds is no longer skipped for the answer's length, and the delegator's rescue re-packs on the node that wrote the answer

Two defects of the structured re-pack, both seen in one delegation of three subtasks (2026-10-12): a wrong success and a lost answer. Each had a finished, correct answer in hand.

- **A wrong success: eleven stale sentences delivered as none.** A seat's final answer was itself a JSON object with keys of its own (`outdated_sentences`: eleven `{quote, release_note_says}` objects), where the contract's schema asks for `{doc_title, stale: [string] (maxItems 10), up_to_date, notes}`. The seat never sees the schema, and the cut-final re-issue ("Return the same JSON object that was asked for") names no keys. The re-pack prompt then said "Use empty values when a field is absent", and a text none of whose keys is called `stale` made every field absent: the re-pack wrote `{"stale": [], "up_to_date": true, "notes": "No stale sentences were identified in the provided text."}`. It validated; `contains:` read the answer, `nonempty:doc_title` read one scalar, and the call counted it a success. The re-pack was not given a cut or trimmed answer: its `max_tokens` of 2,315 is exactly the budget of the whole 5,411-character answer. (That the extractor inverted for this reason is inferred from the prompt and the object; the model was not replayed. A control job on a different seat, whose answer was a prose numbered list with no foreign keys, kept all six items.) A faithful re-pack would have failed too: the answer has eleven items and the schema says `maxItems: 10`, which the validator enforces and no prompt mentioned.
  - **The object is read against its answer** (`core.ListsEmptied`). A list the schema asks for that the answer also holds under the same path is compared with that list and nothing else (an answer that says `"stale": []` has nothing to lose, whatever else it lists). Only when the answer shares no list path with the schema do the lists it holds under keys the schema does not define count, and then the object must hold some item in the lists the schema asks for. A list the object carries itself is not lost. Prose answers are not judged: nothing can be counted in them, so a prose answer whose list was lost is published. The node refuses such an object on the grammar lane (before and after coercion) and on the chat lane; the attempt after a refusal carries the finding ahead of the text; every attempt refused is an abstention with the finished answer in `output`. A false refusal is not a wrong success but it is not free: an answer that lists what it checked under another key, beside a schema list that is legitimately empty, is refused too (pinned by a test), and the refusal is an abstention defer, which the delegator's rescue refuses again and the delegator then re-places once on another node: a second full agent run, which reproduces the refusal when the answer is truthful.
  - **The prompt says what the text is.** A TEXT that is itself a JSON object gets the rule "take each requested field from the key whose content matches it ... use an empty value only for a field that no key of the TEXT holds content for" instead of the empty-values rule, and a listing of its keys (name, shape and count, never content). The bound a schema puts on a list (`maxItems`) rides in the prompt on every lane: the grammar's projection of the schema drops it, so the grammar lane never told the seat. A prose answer's prompt is byte-for-byte what it was when no list is bounded.
  - **The delegator reads it too.** `EvalAcceptance` reads every result's object against its answer (a hollow object from a node that predates this is a failed verification, which the delegator retries on another node), and a rescued object is refused the same way. A result that kept fewer list items than its answer held says so: `repack_note` reads `kept 10 of 11 list items from "outdated_sentences" (maxItems 10)`.
- **A lost answer: a correct answer deferred for a re-pack the schema bounds well under the time the call had left.** A slow seat's answer of 7,126 characters was skipped by the node (`re-pack skipped: 476 s left to the call's deadline + 30 s grace at 4.9 tok/s buys 2330 tokens < the answer's 2439`), because the skip test sized the re-pack's OUTPUT as the answer's length: a token per three characters. The schema (a title, at most ten strings, a flag, a note) bounds the object near 1,600 tokens by the estimate below; this answer's re-pack never ran, and the comparable one that did (the control subtask's, six items on a faster seat) wrote 228 tokens. The delegator's rescue then tried one place, its own agent seat, found it not resident with its cards held by another session's exclusive lease, warmed it, and was cut at its 120 s allowance (`attempt 1/1 cut by its 1m59.995201s share of the wall`); the error also dropped the warm-up note.
  - **A re-pack is sized from the schema it writes to** (`repackSchemaTokens`, `repackLeast`). A free string is estimated at 128 tokens (about 400 characters; chosen, not measured), a declared `maxLength` or `enum` exactly, a list only with `maxItems`, an object only with named fields; a schema that bounds nothing keeps the answer-based figure, and the schema never raises it. A schema with one or two free-text fields is read as a restating one (the flat `{answer}` every remote `agent_run` carries is its one string, the whole answer restated) and keeps the answer-based figure too; three or more free slots, a list of ten strings counting as ten, are an extraction into parts. The skip floor, the chat lane's, the resend floor and the time the re-pack is owed (`repackOwedTokens`) use that one figure. A skip it sized says `< the object's N` instead of `< the answer's N`. The liveness allowance is left on the answer-based size on purpose: it is `max(flat bound, estimate)`, a stall bound, and shrinking it could only cut a seat that was producing. The cost: a bounded schema whose real output runs long is sent and cut at the tokens the time buys, a `budget` defer after seat time spent instead of before.
  - **The rescue goes where the seat is hot.** The delegator hands its rescue the node that ran the loop and the seat that wrote the answer (`delegate.WithRescueOrigin`, in the context: the `RescueFunc` signature, and so every door that wires a rescue, is unchanged). `RescueRepack` reads the lossless object, then re-packs on this box's seat when it is resident (or its residency cannot be read), and when it is not, makes ONE chat completion on the origin node's own seat through the node's chat door with the fleet bearer, which waits for no load here and never queues behind this box's lease; the call carries `WithLocalBusy`, so it leaves the box or does not run. If the node cannot take it (an older build, a hollow or invalid object) this box's seat is warmed and used with the budget that is left, and the rescue stops without warming it when under 20 s remain. A failed rescue names every rung it tried, the node and the warm-up included. The node is not handed over when its own re-pack failed for its seat's health (defer class `infrastructure`: a stall, an unreachable or busy seat, a seat lost under the run): the seat that just wedged is the last place to send the rescue first.
- **Tests, written first.** `internal/core/fidelity_test.go`, `internal/delegate/acceptance_fidelity_test.go`, `internal/delegate/rescue_origin_test.go`, `internal/pipeline/agenttask_repackfidelity_test.go`, `agenttask_repacksize_test.go` and `agentrescue_origin_test.go`. Red before, on `e14c5a88`: `published stale=[] as a success while the answer holds 11 list items`; `a hollow object over an answer holding 11 list items passed acceptance`; `structured re-pack skipped: 4 s left to the call's deadline + 0 s grace at 100.0 tok/s buys 403 tokens < the answer's 1064` for a three-thousand-character answer and a one-field schema; and, with this box's cards held by an exclusive lease and its seat cold, the rescue `cut by its 2m0s share of the wall` after 125 s where it now finishes in 10 ms. Each rule was mutation-checked with `scripts/mutation-check.sh` (one change reverted at a time, the mutant compiles, a named test fails).
- **Second review round (on `0cfbe788`).** Two reviewers read the branch. Each finding was scored with Jev, reproduced against the code, and then fixed red first or declined with the reason below.
  - **The retry prompt could turn a false refusal into a fabricated success.** Reproduced: `published stale=[a.md b.md c.md] as a success for an answer that lists them as checked files`, with an empty `repack_note`. A truthful answer (`{"doc_title": ..., "up_to_date": true, "checked_files": ["a.md", "b.md", "c.md"]}`) has no `stale` key, so the empty-list object is refused (the accepted cost above), and the attempt after the refusal said "write those items into the list fields" about every list the text held: a seat that obeyed it published the checked files as stale ones, contradicting `up_to_date: true`. The prompt after a refusal is built from the lists that were lost (`core.ListsLost`): a list the schema has a field of the same name for goes into that field; any other list is copied only if it is the content of a requested list field; a field the text says is empty stays empty. The refusal itself is unchanged: nothing deterministic tells a list of what was checked from stale sentences under another name.
  - **Three shapes the check could not see, each published as a success.** A list nested under an object (`{"report": {"outdated": [...]}}`) is read by its path now; an answer that is itself a JSON array (bare or in one fenced block, items that are objects, texts or lists) is one list, and its prompt says so instead of "use empty values when a field is absent"; a list declared `type: ["array", "null"]` is a list site. The cost, stated: an auxiliary list nested in an answer that has no list under any schema name is now refused the way `checked_files` is.
  - **The rescue no longer calls the seat that just wedged.** The node files a re-pack that stalled, could not reach its seat or found it busy as class `infrastructure` and flags every re-pack failure `schema_miss`, so the rescue used to call that seat first, with an allowance of at least the 120 s flat bound against a rescue budget that floors at 120 s. An `infrastructure` defer no longer hands the rescue the node; a budget defer, an abstention and a node that names no class still do. A result whose wire names no seat uses the one the run was placed on. An unreadable `/running` and an unparseable `output_schema` each say so in the log.
  - **Declined.** Partial loss stays a note, not a refusal (`kept 1 of 11`): the object is faithful as far as it goes and the answer rides beside it, and an expected-count threshold cannot be computed on the orphan path (an answer with `files_changed` of 5 and `tests` of 3 beside a schema `blockers` of at most 5 would be refused for an object that is right), which would add the false refusals this round set out to count. A share cap on the origin rung: a hot seat's re-pack of a long answer takes tens of seconds (2,315 tokens at 30 tok/s is 77 s), and half of a 120 s budget would cut it to buy a try on a cold seat that needs 125 to 250 s to load. Sizing a `["array", "null"]` property in the re-pack prompt: it follows the grammar lane's projection, which compiles a type list to a string, so changing it reaches into the grammar; a nullable list there is a loud truncated attempt, not a silent result. Not changed, below the reviewers' bar: a shared key short-circuits the orphan reading of the other schema lists, the content test counts items and not words, an object that carries an orphan list verbatim is not judged.
  - **Tests.** `internal/core/fidelity_lists_test.go`, `internal/pipeline/agenttask_repackcorrection_test.go`, and additions to `internal/delegate/rescue_origin_test.go` and `agentrescue_origin_test.go`. Red before, on `0cfbe788`: the reproduction above; `the re-pack stalled: class "infrastructure": origin handed = true, want false`; `a result with no seat of its own was handed no origin`; a hollow object over a nested list, a bare array and a nullable list published; a correction that still read "Write those items into the list fields". 26 rules mutation-checked with `scripts/mutation-check.sh`, each RED (one of the 27 mutants did not compile and was replaced by one that does).
- **Not changed, for a follow-up.** The cut-final re-issue still says "Return the same JSON object that was asked for" to a seat that was asked for no JSON; rewording it changes every schema contract's cut final, so it needs an A/B first. No deterministic key mapping from a foreign key to a schema field: picking `quote` of `{quote, release_note_says}` would invent a mapping. A prose answer whose list was lost is not judged (nothing to count). The rescue tries the origin node only, not the fastest idle node of the fleet. Docs: `docs/systems/coding-agent.md`, `docs/systems/fleet-node.md`, `docs/architecture/decisions/0055-walls-are-ceilings-liveness-is-progress.md` (item 11).

### Docs — the composite-tier guide no longer says the display card is closed to every placement until the operator sets presence

- `docs/systems/composite-tier.md` said `operator_presence` keeps the display card closed and that no placement touches it until the key is `auto`. Since 0.183.0 that holds for the display layer's twins only: the card allocator behind `gpu reserve --cards` and the media admission path reads no presence and may use the display card up to `gpu_display_cap_pct` (ADR 0039, amendment 2026-10-12). Both sentences now say which placement they mean. Found by a fleet audit of the 0.183.0 docs.

### Added — a transcript whose word times drift from their segment says `words_reliable: false` (D-38, issue #169: the flag; the cause stays open)

- **What was wrong.** Issue #169: on a long file whisper-server's per-word times lagged the segment they belong to by minutes (up to -152 s on a 730 s file). `internal/sttclient` passed the `words[]` array through and nothing compared it with its segment, so a consumer that cuts audio at word boundaries had no way to tell the times were wrong.
- **The fix.** `Transcribe` now sets `Result.WordsReliable` and `Result.WordsDriftSec` from the upstream's answer. The pure `checkWordDrift` takes the largest `|segment.start - words[0].start|` over the segments that carry words: `words_reliable` is `false` at or above 0.5 s (the issue's own threshold), `true` below it, and absent when no segment carries words (the OpenAI path never does); `words_drift_sec` is that largest gap. Both keys are `omitempty`, so a result without words marshals byte for byte as before. `true` means the FIRST word of every segment that carries words starts within the threshold of its segment's start; a later word far outside its segment still reads `true` (the field comment and the docs say so; a stricter bound on every word waits for a measurement of honest audio).
- **Published, so a caller can see it (review round).** The first cut set the two fields on the client's `Result` and nothing read them: `pipeline`'s `transcribeResult` copies the `Result` field by field and `internal/sttremote`'s `nodeResult` rebuilt a node's answer the same way, so the verdict died before the `offload_transcribe` payload, the cache entry and any log. Both structs now carry `words_reliable` and `words_drift_sec` (omitempty); the pipeline sets them from the `Result` and logs one line when the verdict is `false`; `sttremote` re-publishes the node's verdict in the answer it builds on the asking box, including when the node cut its inline list; the MCP tool description and `docs/systems/mcp-server.md` name the two keys. The keys are additive: an older node or asker ignores them, and a node older than the check simply sends none, which stays absent (absent means no verdict, never `true`). An entry cached before this build carries no verdict until the audio is transcribed again.
- **Tests.** `internal/pipeline/transcribe_words_test.go` (a real convert, a fake whisper answering a drifted, an aligned and a word-less transcript: the payload says `false` with 58.8, `true` with 0.2, and no key; the cache hit answers the same; the log names the symptom; `.segments.json` stays a bare array) and `internal/sttremote/words_reliable_test.go` (the node's verdict is re-published for a drifted, an aligned and a truncated answer, and an older node's answer gains no key), both red on the missing keys before the fix and ten mutants RED after it (`WordsReliable` or `WordsDriftSec` dropped from either out literal, the log removed or inverted, `omitempty` dropped from either key of either struct). `internal/sttclient/words_reliable_test.go` with the hand-written fixture `testdata/drifted_words.json`, written first: against a no-op `checkWordDrift` the drifted, aligned, exact-boundary, run-ahead, first-word and `Transcribe` tests failed on their assertions (not on a compile error). Eight mutants, each applied with `scripts/mutation-check.sh` and each RED: `<` to `<=` at the threshold, the first-word read replaced by the segment start, the absolute value dropped, the last segment's gap taken instead of the largest, the no-words skip removed (an index panic), the call in `Transcribe` removed, `omitempty` dropped from `words_reliable`, and the threshold constant changed to 5.
- **Not changed, for follow-ups.** (1) The check flags the symptom and does not find where the per-chunk offsets are lost, so #169 stays open until a file longer than 30 s is run through a speech seat under its GPU lease. (2) The `.segments.json` file stays a bare segment array (`sttremote` fetches a truncated list from a node as one, and dataset readers parse it as one), so it carries no verdict: a reader that takes only the file path has to read `words_reliable` from the payload. (3) 0.5 s is the issue's number, not a measurement of honest audio: the same live run shows whether a clean file ever puts a segment's start and its first word's start further apart than that, and whether every word can be bounded by its segment's span without false alarms.

### Added — a vLLM seat bound to a cache server is refused when its batch is smaller than the cache chunk (H-33)

- **What was wrong.** A seat bound to an LMCache store declares `chunk_size`, which must equal the engine's unified KV block (784 for the
  Qwen3.8-27B hybrid with fp16 KV, 1568 with fp8, which doubles it), and the engine's per-step token budget
  (`--max-num-batched-tokens`) has to cover at least one block. `Spec.Validate` checked `chunk_size > 0` and nothing else, so a seat could
  be authored, rebound through `install vllm-seat --config` (which replaces the chunk from the box's binding), and rendered with a batch
  below the chunk it was registered with. The two shipped store-backed seats satisfy it (batch 3135, chunk 1568), so nothing in the tree
  had ever shown the gap.
- **The fix.** `Spec.validate` now refuses a seat with a cache server whose declared `max_num_batched_tokens` is below its `chunk_size`
  and names both numbers and the cause (`max_num_batched_tokens 1024 is below the cache server's chunk_size 1568 ...`). One rule serves
  the tier table (`Validate`), the extra seats (`ValidateExtra`) and the render (`Artifacts` re-validates after a binding replaced the
  chunk). Equal is accepted. Docs: `docs/systems/cache-server.md` (invariants).
- **Named limits.** `ChunkSize` is declared to equal the unified block and LMCache refuses a mismatch at registration, which is why
  `batch >= chunk` is the checkable form of "batch >= block". The block itself is a model-and-dtype property known only as measured pairs,
  not a `Spec` field, so a storeless fp8 seat with a small batch (the shipped storeless seats run 2048 and 4096) stays unguarded, and an
  undeclared batch (0) is not compared. No upper bound is enforced (the measured seats run one token under two blocks).
- **Tests.** Red before the rule: `TestASeatWhoseBatchIsSmallerThanTheCacheChunkIsRefused` failed in all four subtests (`lane_seat`,
  `extra_seat`, `render`, `one_token_under`) with `a store-backed seat whose max_num_batched_tokens is below the cache chunk_size must be
  refused; Validate returned nil`; the four guards (`TestASeatWhoseBatchEqualsTheChunkIsAccepted`, `TestAStorelessSeatIsNotJudgedByTheChunk`,
  `TestASeatThatDeclaresNoBatchIsNotJudgedByTheChunk`, `TestTheMeasuredFP8SeatsKeepTheirChunk`) passed. Five single-line mutants each turn
  one named test red (`scripts/mutation-check.sh`, verdict `RED`, file restored byte for byte): `<` to `<=` (the equal-batch guard), the chunk
  read as 1568 for a storeless seat (the storeless guard), dropping `MaxBatchedTokens > 0` (the undeclared-batch guard), the chunk read as 0
  beside a store (the refusal test) and the two numbers swapped in the message (the refusal test).

### Added — `/fleet/health` `gpu_devices[]` carries each card's temperature and the clock event reasons active on it (F11)

- **The gap.** No health payload carried a temperature or a throttle state: a card that was heat-soaking, or held at a power cap, looked the same on the wire as a cool one, and the only signal a delegator had for a hot node was a slow answer.
- **What a card row gains.** `temperature_c` (integer degrees C, nvidia-smi `temperature.gpu`) and `throttle_reasons` (an array naming the active clock event reasons). An absent `temperature_c` means not read, never "cool": a node that predates the fields, a driver that answers `[N/A]`, a failed query and an answer older than 90 s all read the same way. `throttle_reasons` has three states, so an absent key is never "not throttled": the names of the reasons active; `[]` when the card was read and nothing limits its clocks (a mask of 0, or the idle bit alone); and no key when the throttle column was not read (a node that predates it, `[N/A]` or `[Not Supported]`, a driver with no throttle field, a failed query, an answer older than 90 s). The key is tagged `omitzero`, not `omitempty`, because `omitempty` drops an empty array and the two states would again look alike. A temperature outside 1 to 150 is a garbled counter and is left out. The delegator decodes the rows into the same `gpuprobe.Device`, so both fields reach `delegate.NodeView.Devices` with no change on its side. They show on a fleet only after a fleet-parity deploy.
- **A second query, never a new column.** The values come from `gpuprobe.ReadThermal` (`index,uuid,temperature.gpu` plus the throttle field), merged into the devices in `fleetnode.SmiDeviceProbe`, so the per-device query the 2 s sampler and the placement guards share never gains a column an old driver refuses, and `main.go`, `vram.go` and `server.go` are untouched. Cards are matched by UUID, never by index.
- **Cadence.** The sampler ticks every 2 s; the thermal query runs at most once per 30 s (one extra `nvidia-smi` process per 30 s per node), a failed query is not retried sooner than 60 s and is logged once per outage, an answer older than 90 s is dropped and not published, and the read is bounded at 10 s (the per-device query has no deadline of its own). The query runs on a goroutine of its own, one at a time, and the probe only reads what the last one stored: a hung second `nvidia-smi` delays neither the memory reading the placement guards use (the sampler stamps its snapshot after the probe returns, so an inline query had made the memory figures older than their timestamp) nor fleet-serve's synchronous first sample. The answer reaches the next tick, and is aged from when its query started.
- **Both driver spellings.** The throttle field is `clocks_throttle_reasons.active` on older drivers and `clocks_event_reasons.active` on newer ones (a current driver lists both names for one field). The reader asks for the older name, asks for the renamed one when the driver refuses the first, and asks for the temperature alone when it knows neither; the variant that worked is remembered, so the refusal costs one spawn once. A refusal is recognised by the text naming the field, not by the exit status, and a failure that is not a refusal (a missing binary, a wedged driver) is returned at once and walks nothing. `refusesAttached` is now a call of the general `refusesField`.
- **The bit table is NVML's, read from the header and not from memory** (`nvml.h`, CUDA 13.4, the `nvmlClocksEventReason*` / `nvmlClocksThrottleReason*` defines): `0x2` `applications_clocks_setting`, `0x4` `sw_power_cap`, `0x8` `hw_slowdown`, `0x10` `sync_boost`, `0x20` `sw_thermal_slowdown`, `0x40` `hw_thermal_slowdown`, `0x80` `hw_power_brake_slowdown`, `0x100` `display_clock_setting`, `0x200` `board_limit`, `0x400` `reliability`. `0x1` (idle) is never listed. `board_limit` and `reliability` are in the 13.4 header and not in 13.3's, so a bit above the table is published as `unknown:0x<bit>` instead of being dropped. The plan's seven named bits would have published an empty list for the three cards of the box read below, which all answer `0x400`.
- **A live read.** Against the real driver of a three-card desk box, `SmiDeviceProbe` returned a temperature for each card (84, 52 and 37 C) and `["reliability"]` for each.
- **Tests.** `internal/gpuprobe/thermal_test.go` (fifteen) and `internal/fleetnode/health_thermal_test.go`, `health_thermal_wire_test.go` (nine, one through the delegator's real health decoder), written first against compiling stubs: fourteen gpuprobe and seven fleetnode tests failed on assertions before the code, for example `15 probe calls at 2 s spacing made ... want 1` and `ThrottleReasonNames(0x20) = [], want [sw_thermal_slowdown]`; the guards (a card without a reading marshals byte-identically, a failing thermal query leaves `gpu_devices` byte-identical to a node without one) hold before and after. Sixteen mutations, each reverted in turn, fail a test they name (30 s window, 60 s back-off, 90 s age and its `<=` boundary, the 10 s bound, the once-per-outage log, a swapped bit name, the idle bit listed, the remembered variant forgotten, the ladder walked on any failure, any temperature accepted, the reasons slice aliased, match by position, a refusal matching any field, no renamed-field fallback, the display_attached refusal lost). `Device` now carries a slice, so four struct comparisons in `vram_test.go` and `gpuprobe_test.go` moved to `reflect.DeepEqual`. **Review round**, tests again written first and seen red: `internal/gpuprobe/thermal_clear_test.go` (three) and `internal/fleetnode/health_thermal_clear_test.go` (one) failed with `throttle_reasons = []string(nil), want an empty non-nil slice` and a clear read marshalling without the key; `internal/fleetnode/health_thermal_async_test.go` (five; four failed first with the probe `still running after 3s` behind a held thermal query, the fifth pins that an answer is aged from when its query started and is proved by mutation). The earlier fleetnode thermal tests now run through a settled probe (a tick, the wait for the query it started, a second tick at the same instant) so their assertions did not change. Thirteen more mutations, each reverted in turn, fail a test they name: a clear read parsed as not read, the merge keyed on length, the merge copy made nil, `omitempty` for `omitzero` (gpuprobe and fleetnode), the refresh run inline, the one-at-a-time guard removed, an answer aged from when its query returned, and the 30 s window, 60 s back-off, 90 s age (and its boundary), once-per-outage log and 10 s bound re-proved on the new structure.
- **Not changed.** Placement does not read either field; whether a heat-soaked node is demoted is a separate decision (row F11 says "decide"). The sustained tok/s half of the row was already published (`seat_rate`).
- **Docs.** `docs/FLEET-NODE.md` ("Multi-GPU: `gpu_devices[]`") and `docs/systems/fleet-node.md`.

### Added — every `agent_delegate` result a seat ran says where it ran: `results[].ran_on` is `local` or `node`, and the opencode plugin reads it first (D-32)

- **The defect.** `results[].placement` is the placement reason in words, and the opencode plugin counted the word `local` in it. A remote result whose reason said `local seat busy` read as local (10 of the 214 remote rows of one delegation log, for example `route=spread -> node-a (slot 1 of 1 ...); local seat busy: 1 in flight`), so the digest told the model the pair had not landed.
- **The field.** `ResultWire.RanOn` (`ran_on`, `omitempty`): `local` when the delegator's own seat ran the subtask, `node` when a fleet node did, absent when no seat ran it. It is stamped in one place, `WireResponse`, from `placedWhere`: the decision that already writes the hint clause and the pin tallies, so the field cannot disagree with them. Every constructor of an unplaced outcome builds its result without `ranLocal` or `ranBase`, so `placedWhere` reads it as nowhere and the key stays off the row. It names where the subtask was placed and dispatched, so a local run that deferred on the seat still reads `local`. A job a node confirmed it took back before it started, or whose own record says it never ran, reads nothing even when the call deadline then cuts the row: the cut clears the refusal flag `placedWhere` used, so it now also reads the `withdrawn` and `nodeNeverRan` marks the node's word leaves (which keeps the hint clause and the pin tallies in step); a job the deadline cut while the node still held it, with no word that it will not run, reads `node`. A `route: "queue"` answer a claiming node filed reads `node`, set where the poll and the deadline's last look read a finished answer (`takeQueueAnswer`); a job still queued, a poll that ran out and a deadline cut stay unplaced. This adds no `PlacedResult` field and leaves `run.go` untouched.
- **The plugin.** `ranLocally` takes a result as well as a placement string and reads `ran_on` first; any other value falls back to the placement text, read exactly as before, so an older node and a result nothing ran read as they always did. `delegateDigest` counts by it, and a deferred result with no `ran_on` (a defer no seat ran) is neither local nor remote any more, unless its placement text names the local seat, which a delegator that predates `ran_on` still writes for a defer on its own seat: it used to count remote, so one local run beside one defer printed "pair landed" for work nobody did. When nothing ran on a node the line says "nothing ran on a node" instead of "all local" if some rows ran nowhere.
- **Known limits.** A node's answer that the delegator re-packed itself still reads `node`. The field shows on a fleet only after the delegator is deployed.
- **Tests.** `internal/delegate/ran_on_test.go` (seven, plus a wire fixture, `testdata/ran_on_wire.json`) and five TypeScript tests in `integrations/opencode/test/plugin.test.ts`, written first: before the change `ran_on = "" (present false), want "local"` and `delegate placement: 1 local, 0 remote of 1.` where the misread placement now reads `0 local, 1 remote of 1`; the existing `ranLocally reads a placement that opens with no hint clause as it always was` stays green. Seven mutations (four Go, three TypeScript), each reverted in turn, fail a test they name. **Review round**, tests first and seen red: `internal/delegate/ran_on_cut_test.go` (three) showed `ran_on = "node"` for a job the node confirmed it took back, and for one its own record says it never ran, after the call deadline cut the row, and no `ran_on` on the queue lane (`TestRunQueueRouteEndToEnd` now asserts `node` where it pinned the gap); three more TypeScript tests, two of which failed first with `1 local, 1 remote of 2 — pair landed.` for one local run beside a defer no seat ran (the third guards the wording the fix touches). Nine more mutations (five Go, four TypeScript), each reverted in turn, fail a test they name: either half of the `placedWhere` marks dropped, the queue answer's mark dropped (and, separately, passed empty from the poll arm and from the deadline-look arm), the digest's defer rule off, its local-defer exception dropped, its node-defer exception dropped, and the "nothing ran on a node" wording replaced.
- **Docs.** `docs/systems/fleet-node.md` ("Where a result ran") and `docs/systems/opencode-integration.md` ("Wire fields the plugin reads", the table of the fields the digest reads).

### Added — `offload_status` names the image an MCP server runs and says when the installed build has moved on (D-36)

- **What was wrong.** A rename-swap deploy replaces the installed binary while every MCP server already running keeps the
  image it started from, so a session wrote rows with old behaviour for days and nothing said so: `offload_status` announced
  only the version it started with, and `gpu doctor` listed 13 older-build harness processes beside 10 current ones
  (2026-10-11) for the operator to compare by eye. Underneath, `buildinfo.BuildSHA256` hashed the executable lazily at its first
  call, so a long-lived process whose first delegation came after a swap hashed the NEW file at the path it started from and
  stamped its rows `harness_build_sha256` of a build it was not running.
- **The fix.** `buildinfo.CaptureStartup()` takes that one latch at start (the version, the hash, the path and the stat of the
  image, read through one handle); `runMCP` calls it before anything else and `mcpserver.Run` holds the identity on the `Server`. `offload_status` gains a
  `process` block `{version, build_sha256, installed_version?, stale_image?, note?}`, built from the identity the server holds
  (absent for a server that holds none, `{}` when asked for by name, as `pair` is): `stale_image` is `true` when the installed
  bytes differ from the image the process started from, `false` when they are equal, and absent with a `note` when the installed
  file cannot be read. A status call hashes the installed file only once its stat has moved from the one the process saw at
  start, and caches the result per stat. `gpu doctor` marks each running binary older than the installed build
  (`older_than_installed` in `--json`, a line and a summary with the pids in the text), reading the installed version from the
  installed file through the audit's own marker reader (`gpulease.ReadBuildMarkerVersion`, exported from the audit's scan).
  A short CLI call never captures and keeps the lazy hash, so a hook invocation does not hash the whole executable.
- **The default answer moves.** The full `offload_status` answer gains the `process` block, so
  `testdata/offload_status_default.golden.json` is re-pinned (`OFFLOAD_UPDATE_GOLDEN=1`) and `statusFixture` injects a fixed
  startup identity: the pinned answer carries constant values and never the test binary's own hash.
- **Tests.** `internal/buildinfo/stale_test.go` (the hash after a swap is the captured image's, once-only capture, the lazy path
  for a process that never captures, an unreadable image is absent and not invented), `internal/gpulease/buildmarker_test.go`
  (the exported reader, across a read-chunk edge, no marker, missing file), `internal/mcpserver/stale_image_test.go` (stale
  after a swap in both directions, fresh for the same bytes, unknown for an unreadable file, no hash while the stat is
  unchanged, the block in the full answer, the injected identity and never the test binary's, absent without an identity, `Run`'s
  identity equals the pair a row carries) and `gpu_doctor_stale_test.go` (older marked, the installed version read from the file
  and not the doctor's own constant, no marker is never older, an unreadable installed version is said and compares nothing),
  written first and seen red.
- **Not changed.** Processes started before this build show no block (they predate the field), so the real-world proof is the
  next rename-swap. The fleet node (`runFleetServe`) captures too since the integration review round at the top of this section; it runs no MCP
  server, so it serves no `process` block. A `stale_image` column on ledger rows is not added: a reader compares the
  row's hash with the deployed one.

### Added — `gpu status` and `offload_status` show the driver's fault events for the leased card (C-83-B, A-128 part 3)

- **What was missing.** A card that faults under a lease leaves one line in the driver's log (Windows: the System log, provider
  `nvlddmkm`, id 153 `Error occurred on GPUID: b500`; Linux: `NVRM: Xid (PCI:0000:b5:00): 79, ...` in the kernel log), and nothing
  in the harness showed it: 181 id-153 events since 2026-09-22 on the reference box were found by hand, weeks after the fact.
- **What it does now.** `gpuactivity.ReadDriverEvents` reads the last 24 h and places each event on its card by PCI bus and device
  (the record's GPUID `b500` is bus `b5`, device `00`; `nvidia-smi --query-gpu=index,uuid,pci.bus_id` ends `B5:00.0`; a second
  small query inside `gpuactivity`, so the card sample is unchanged). The activity snapshot keeps the events of the cards the lease
  holds (every card for a whole-node lease), newest first and capped at 20, counts those that name no card this box can place
  (`driver_events_unattributed`: the Windows id 13 Graphics Exception records, 420 in 96 h on the reference box, carry no GPUID),
  and carries a reader failure as `driver_events_note`. They print in `gpu status` (text, and `--json` under `activity`) and ride
  in `offload_status.gpu_lease.activity`. **Facts, never a verdict**: a fault and a kill look alike in the log (A-128's kill test
  tells them apart and is queued), so an event never moves the verdict, the brief line or any gate; a box with no source prints
  byte-identically to before.
- **Read-only, hidden, bounded.** Windows: one `powershell.exe` from System32 (`-NoProfile -NonInteractive`), a read-only
  `Get-WinEvent` over the System log's `nvlddmkm` provider (24 h, the newest 1000, each record as XML; a log with no match is an
  empty answer). Linux: `journalctl -o short-unix --since "24 hours ago" _TRANSPORT=kernel` (every boot in the window), then
  `dmesg --time-format iso` (this boot only, and said). Every child starts through `internal/winexec`
  (`TestEveryChildProcessStartIsHidden` stays green), the log read is bounded at 8 s and the card-table query has its own 4 s, and the
  answer is cached for 60 s in the process (failures too), so a poll of the long-lived MCP server starts no child per call (each
  `gpu status` is a fresh process and pays one read). The record's host name, user and device object
  path are not carried. The reader is called only while a lease is held and the GPU sample is on.
- **Checked live, read-only.** The production reader on the reference workstation: a 24 h window with no match returned no events
  and no error in 0.6 s; a 96 h window through the same child and pipe returned 434 records (420 id 13, 14 id 153), all parsed, the
  14 id-153 ones placed on cards 1 and 2 by the bus table.
- **Tests.** `internal/gpuactivity/driverevents_test.go` (the parsers over a fixture of two real records hand-reduced with the
  host name replaced, the kernel-log lines in journalctl and dmesg forms, the bus table in either case, the readers behind their
  seams: one System32 PowerShell that is read-only, journalctl then dmesg, the window, the card table failing, the 60 s cache and a
  cached failure; the snapshot: only the leased cards' events, whole-node, newest first and capped, unplaced counted, nothing read
  without a lease or the GPU sample, no key for a box with no source, a reader failure as a note, the verdict untouched),
  `internal/mcpserver/driver_events_status_test.go` and `gpu_status_driverevents_test.go` (both surfaces), written first and seen
  red.
- **Not changed, for a follow-up.** A-128's kill test (does killing a healthy render mid-sampling log id 153?) is queued, not
  built, so these events stay facts. The Linux `Xid` line shape is the documented one: neither Linux gate host has an NVIDIA card
  to have logged one.

### Fixed — review round on the stale-image block and the driver-event reader (D-36, C-83-B)

Seventeen review findings (twelve distinct), each read against the code and fixed below, with its test written first and
seen red.

- **A card on a low PCI bus was never placed (C-83-B).** The driver prints the GPUID as the unpadded hex of `(bus << 8 | device)`:
  a card on bus `01` logs `GPUID: 100` (read from a one-card laptop's System log, both `Error occurred on GPUID: 100` and
  `UCodeReset TDR occurred on GPUID:100`, no space after the colon). The reader matched exactly four digits, so every one-card box
  placed none of its events and called them "no card this box can place". `busOf` now reads one to four hex digits as a number and
  splits it; `TestBusOfReadsEveryGPUIDSpelling`, `TestParseWinEventsPlacesACardOnALowBus` (a fixture hand-reduced from those records).
- **A failed card table was silent (C-83-B).** When `nvidia-smi --query-gpu=index,uuid,pci.bus_id` failed, every event (the leased
  card's own fault included) was left unplaced, counted as "name no card", cached for a minute, with no note. The events now come
  back with an error that names the table and nvidia-smi's reason (`driver events incomplete: the card table could not be read
  (...)`), are not counted as unplaced-by-nature, are held 10 s instead of 60, and the table query has its own 4 s budget
  instead of what the 8 s log read left. `TestReadDriverEventsSaysWhenTheCardTableCannotBeRead`, `TestTheCardTableQueryHasItsOwnBudget`,
  `TestACardTableFailureIsRetriedSoonNotHeldForAMinute`, `TestSnapshotSaysWhenEventsCouldNotBePlacedAndDoesNotBlameThem`.
- **A short list read as the whole (C-83-B).** A record that is not XML after good ones used to end the read silently; the older
  events it cut off are now said (`the event log read stopped after N record(s)`). `TestAPartialWindowsReadReturnsTheEventsAndSaysItStopped`,
  `TestSnapshotNotesAnIncompleteReadAndStillShowsTheEvents`.
- **Linux read only the current boot (C-83-B).** `journalctl -k` implies `-b`, so a fault that preceded a reboot (an Xid 79 usually
  ends in one) was invisible under a "last 24 h" label: on a gate host with four boots in its journal, seven days read 2528 lines
  with `-k` and 4226 across every boot. The kernel transport is now a match (`_TRANSPORT=kernel`); the `dmesg` fallback, which holds
  this boot only, says so. `TestTheLinuxReaderReadsEveryBootInTheWindow`, `TestTheDmesgFallbackIsSaidToBeThisBootOnly`.
- **`gpu status` pays the read every call; the docs said it did not (C-83-B).** The 60 s cache is process memory, so it serves the
  long-lived MCP server and not the CLI, a fresh process per call (about 0.6 s on Windows, measured). The docs and the
  header comments now say so; no state-dir cache was added.
- **The brief read events it threw away (C-83-B).** `offload_status {section:"brief"}` made the lease view start the PowerShell
  read under a held lease and returned none of it. The lease view takes whether the caller returns the events; the brief passes
  no. `TestBriefMakesNoDriverEventReadButTheLeaseSectionDoes`.
- **The stale-image signal was missing from the brief (D-36).** The call the tool's description tells a caller to make first
  returned no `process` block, so a stale server stayed invisible there. The brief now carries the same block under the same key
  (absent for a server with no identity). `TestStatusBriefCarriesTheStaleImage`, and the pinned brief key set moves with it.
- **One failed read of the installed file hid the stale image for good (D-36).** The hash and marker errors were cached against
  the file's unchanged stat, so a sharing violation during a deploy read "unknown" for the life of the process. Only a read that
  succeeded is kept now, the hash and the marker apart (a retry repeats only what failed), and an unreadable marker is said in the
  note. `TestStaleImageRetriesAfterATransientHashFailure`, `TestStaleImageRetriesAfterATransientMarkerFailureAndSaysSoMeanwhile`.
- **A process whose own start-up hash failed explained nothing (D-36).** `build_sha256` was simply absent, on the stat-equal path
  with no note; the note now says the self-hash failed and its rows carry no `harness_build_sha256`. `TestProcessBlockSaysWhenItsOwnStartupHashIsUnknown`.
- **The image was captured after the pipeline opened (D-36).** `runMCP` took the capture in `mcpserver.Run`, after `openPipeline`; a
  swap landing during start-up was hashed as the image the process runs. It is the first statement of `runMCP` now.
  `TestRunMCPCapturesTheStartupImageBeforeOpeningAnything` (a source-order pin, the repo's style for a call order).
- **`gpu doctor`'s two wrong sentences (D-36).** An unresolved executable path was reported as "the installed build carries no
  build marker"; and a doctor run from a worktree or dev build compared the deployed processes with a build that is not
  installed and told the operator to restart them. The path is named as the cause now, and a doctor that is not the harness binary
  PATH resolves compares nothing and says why (a doctor that is the PATH copy, or PATH names none, behaves as before).
  `TestGPUDoctorBlamesAnUnresolvedExecutablePathNotAMissingMarker`, `TestGPUDoctorComparesNothingWhenItIsNotThePathCopy`, and the guard
  `TestGPUDoctorStillMarksWhenItIsThePathCopy`.
- **Not changed.** The residual window between the process starting and `runMCP`'s first line (the Go runtime and package
  init) is not closable from inside the process. (`runFleetServe` did not capture when this round was written; the integration
  review round at the top of this section made it the first statement there too.)

### Fixed — a bare `--` ends the flags on the media CLI verbs, so a positional that starts with `-` can be passed (CT-52)

- **The defect.** `splitArgs`, `splitTwoArgs`, `splitThreeArgs` and `splitFourArgs` (`main.go`) sent every token that starts with `-`
  to the flags, so `generate-audio out.wav "--- Intro ---"` came back with an empty text (the `--- Intro ---` was a flag) and the same
  held for a prompt that opens with a dash on `generate-video`, `animate-character` and the fifteen call sites of `splitArgs`.
  The engine side already did it right (`runnerArgs` puts `--` before the positionals); only the CLI door lost the text.
- **The fix.** A bare `--` ends flag handling in all four helpers: every later token is a positional, in order, even when it starts
  with `-`; a lone `-` is still the stdin positional. The user's `--` is consumed by the helper. A positional past the helper's arity
  still goes to the flags the caller gives `fs.Parse` (as it always did), but once the terminator was seen the helper writes ONE `--`
  of its own in front of the first such token, so `fs.Parse` stops there and can never read a leading dash as a flag. That is a
  deliberate departure from the backlog wording ("do not leave the `--` token in the list handed to `fs.Parse`"): the user's token is
  never passed on, the fence is the helper's own, and a test pins the returned flags exactly. With no `--` in the arguments every
  helper returns what it returned before. `endsFlags` and `isFlagArg` hold the two rules once. The generate-audio command line is
  factored into `parseGenerateAudio`, as generate-video already was, so the verb's own parse is testable. Usage text and
  `docs/OPERATOR-GUIDE.md` say so.
- **Tests.** `main_test.go`: one terminator test per helper plus a table over all four, the lone-dash guard, the flags-before-terminator
  guard, the over-arity fence (for every helper, and through `fs.Parse`), a no-terminator guard, a value flag that takes a following `--`
  as its value, and the two verbs' parse. Red before (targeted runs on a Linux gate host): the dash-led text
  came back empty from `splitTwoArgs` (`= ("out.wav", "", [-- --- Intro ---])`) and the other three. Five mutants, each applied to one line of `main.go` and run on a Linux gate host through
  `scripts/mutation-check.sh` (every verdict line: `MUTATION-VERDICT: RED (the mutant compiles and the tests fail: the tests catch this
  mutation)`): `endsFlags` never true and `isFlagArg` ignoring the terminator turn the per-helper terminator tests and the table over all
  four helpers red; `isFlagArg` without its lone-dash exemption turns `TestSplitArgsKeepsALoneDashAsThePositional` red; `overflowArg`
  without its fence turns `TestSplitTwoArgsOverArityAfterTheTerminatorIsHandedToParseBehindOneDashDash`,
  `TestSplitHelpersOverArityFenceTheirOverflow` and `TestParseGenerateAudioCarriesADashLedTextToTheRequest` red; a fence written before
  every overflow token turns the first two red.

### Fixed — a bound media route whose weight is missing defers typed `route_not_ready` before the lease on the local path (CT-54)

- **The defect.** A fleet node refuses a media job for a route it binds but cannot run (a weight deleted, an engine binary moved) at its
  door, with a 503 and the route's verdict. The local lanes never asked: with the fastwan `sdcpp_model` pointing at a missing file a
  local `generate-video` took the media lease, spawned the runner, and deferred `err_class` `other` ("--model not found") 31 ms later
  (the 0.173.0 live gates, 2026-10-08, as the backlog recorded them): fail-closed, but untyped, and after a lease other callers had waited behind.
- **The fix.** The five engine lanes (sdcpp video, sdcpp animate, audio.cpp voice and music, the sdcpp image lane) read the same
  `mediacap` derivation the node door, `doctor` and `offload_status` read (`internal/pipeline/igpumedia_routeready.go`), after their own
  checks that every required key is set and before they resolve the runner script and the engine binary, create the output directory, mint
  the seed, refine the prompt or take the lease. A route that derives BOUND-BUT-MISSING (a weight, the engine binary or the runner script
  is not there) defers with the new `err_class` `route_not_ready`; the reason names the route, the config key and the missing path and says
  another node may serve it. A required key left unset (nothing bound, or one key of several: mediacap reads the second BOUND-BUT-MISSING
  on the video, animate and audio routes) keeps the lane's own sentence, which names the key, and no class; that is a configuration gap,
  not a missing file, and the sdcpp image route reads it NOT CONFIGURED anyway. (Fix round: the hook first stood after the lanes' script and
  binary resolutions, so a moved binary or a gone script still deferred untyped while the docs said they were typed; it moved above them.)
  A video family that is not the default is judged on its own route (`generate_video:<family>`). The ledger files the class as
  `config` (`internal/ledger/reasoncode.go`); no circuit breaker counts it (the breaker judges the llama tiers, no media lane reaches it).
  The test fixtures that bound weights by name only (`/models/x.gguf`, `m.gguf`) now point at real files: a route with a missing weight
  is exactly what the lanes now refuse. 34 tests of the pipeline package and one of the root package (`TestFleetMeasureProbesAnSdcppVideoAndAnAudiocppMusicBox`) ran a
  lane against a stub runner with the weights named and absent: they failed on a full package run before the fixture change and pass after it.
- **Tests.** `igpumedia_routeready_test.go` (five lanes by table: the weight is deleted and the lane defers `route_not_ready` naming the
  key and the path, takes no lease, creates no output directory, never runs the runner, and the ledger row carries the class and
  `config`; the reason sentence; a non-default family; the guards that a complete route still runs, an unbound route keeps its old
  sentence, and the lane agrees with `mediacap.Routes`). Red before: every lane ran (`want a typed defer, got ok=true`) and the sdcpp
  image lane's failure was classed `other`. Nine mutants (`scripts/mutation-check.sh`, all `RED`): the derivation read as NOT CONFIGURED instead of
  BOUND-BUT-MISSING and the route name match dropped turn the lane table, the reason test, the family test and `TestAWeightThatIsThereStillRuns` red; a
  wrong `err_class` string turns the lane table and the reason test red; every family judged on the default route turns
  `TestAVideoFamilyThatIsNotTheDefaultIsJudgedOnItsOwnRoute` red; each lane's hook removed in turn (video, animate, audio, image) turns that
  lane's row of `TestEngineLanesDeferTypedBeforeTheLeaseWhenTheirWeightIsMissing` and of `TestTheLaneAndTheNodeDoorReadTheSameVerdict` red and no other; the
  ledger table without `route_not_ready` turns the lane table's ledger assertion red.
  Fix round: `igpumedia_routeready_files_test.go` `TestEngineLanesDeferTypedBeforeTheLeaseWhenTheirBinaryOrScriptIsMissing` (the engine
  binary and the runner script of the video lane, the engine binary, the depth binary and the script of the animate lane, binary and
  script of voice and of music, binary and script of the image lane; each first asserts that mediacap reads the route CONFIGURED, then
  BOUND-BUT-MISSING once the file is gone, then that the lane defers `route_not_ready` naming the key and the path, takes no lease,
  creates no output directory and files one ledger row of class `config`). Red before, on the code with the hook after the resolutions
  (run again on an extract of the red-tests commit): ten of the eleven cases came back with an empty `err_class` (the lane's "not found"
  sentence); the eleventh, the image lane's engine binary, is a guard (that lane never resolves the binary itself and already deferred
  typed). The image lane's script defer also carried the default model tier instead of its sdcpp model. `TestAnUnboundRouteKeepsItsOldDefer`
  used to unset the VAE only, a route mediacap reads BOUND-BUT-MISSING, under the name of the nothing-bound case; it now clears every
  required key of four lanes (premise: NOT CONFIGURED), and the half-bound case is `TestARouteWithAKeyLeftUnsetKeepsTheLanesOwnSentence`
  (premise: what mediacap reads for each route). Five more mutants (`scripts/mutation-check.sh`, all `RED`): the video lane's required-key
  checks removed turns the half-bound test's video row red; each lane's hook removed in turn (video, animate, audio, image) turns that
  lane's row of `TestTheLaneAndTheNodeDoorReadTheSameVerdict` red.

### Fixed — a dead ComfyUI is a typed class on the Go side, and `POST /prompt` and `GET /view` can time out (D-38, issue #72)

- **The defect.** Two of the four follow-ups of issue #72 were still open. (2) `gpugen.ClassifyErr` and `pipeline.classifyErr` had no
  class for a ComfyUI that stopped answering: `comfy-render.mjs` exits 3 on it (`RENDER_EXIT_SERVER_UNUSABLE`), and the Go side filed the
  failure `other`. (3) `rawSubmit` (`POST /prompt`) and `fetchView` (`GET /view`) passed no `AbortSignal` (`/system_stats`, `/queue` and
  `/history` always did), so a ComfyUI that accepts the connection and never answers held the exclusive GPU slot.
- **The class.** `comfy_unusable` joins `gpugen`'s runner allow-list, so `Generate` returns a `RunError` of that class for any child
  that prints `IGPU_CLASS=comfy_unusable` on a line of its own (it reads the retained output of any child, so no new parser).
  `comfy-render.mjs` prints the line after its `RENDER FAILED:` line for a `serverUnusable` error (`reportRenderFailure`) and still exits
  3; `comfy-generate.mjs` relays its child's stderr unchanged (`relayRenderChild`, moved into `batch-jobs.mjs` with the child injected so
  the crossing is testable), so the line reaches the output the Go side reads. `pipeline.classifyErr` gets the `*gpugen.RunError` branch
  `gpugen.ClassifyErr` already had, so the lanes that file a gen failure through it (the ComfyUI image, edit, inpaint and upscale lanes)
  record the typed class instead of re-guessing it from the words of the message (a "zoom lens timeout" read as `oom`). The ledger files
  `comfy_unusable` as `infrastructure`. The image `--batch` needed more (fix round): a batch that meets a dead ComfyUI stops and writes a
  row for every job (the one it stopped on, `comfy-render exited 3: ...`, and `not run: ComfyUI became unusable at job i/N (...)` for each
  later one), so `batchErrClass` never reached the batch error and read the words of each row, which name no class: a C-83 batch filed
  every row `other`. `batchErrClass` now files the not-run rows, and the exit-3 row when the batch's class is `comfy_unusable`, with the
  batch's typed class; a job that failed on its own before the stop keeps the class its words give. Only the image runner prints the line
  today; the other ComfyUI runners (the inpaint `--batch` among them) raise the same error and are a follow-up.
- **The bounds.** `rawSubmit` is bounded by `SUBMIT_TIMEOUT_MS` (120 s: the call validates and queues, it does not run) and `fetchView`
  by `VIEW_TIMEOUT_MS` (600 s), which covers the whole request, the body read included. Both take a `timeoutMs` and reject with an error
  that names the call and the bound (`ComfyUI POST /prompt timeout: no complete answer within N ms ...`); `submitGraph` takes
  `submitTimeoutMs`. A timed-out `POST /prompt` is also marked *server unusable* (fix round: the flag a timed-out call carried, `timedOut`,
  had no reader, so a `--batch` went on and paid 120 s on every later job against a ComfyUI that was wedged on that path): the render
  exits 3 with the class line, a `--batch` stops, and the Go side files `comfy_unusable`. A timed-out `GET /view` is not marked: the render
  has finished and its output is on disk, so it stays a plain failure and the Go side classes it `timeout`. The healthy paths are
  unchanged.
- **Tests.** Go: `TestGenerateReadsComfyUnusableFromAFakeRunner` (a fake runner prints the class and exits 3), `TestRunnerClassReadsComfyUnusable`,
  `TestRunnerClassStillIgnoresAnInventedClass`, `TestClassifyErrHonoursATypedRunErrorClass`, `TestImageLaneRecordsAComfyUnusableFailureWithItsClass`
  (the image lane, the result and the ledger row). JS: both calls reject at their bound against an injected `fetchImpl` and against a real
  socket that accepts and never answers (each races a sentinel timer, so the failure was an assertion and not a hung runner), both pass an
  `AbortSignal`, the body read is bounded, the relay is verbatim, and the real `comfy-render.mjs` against a server answering HTTP 500 exits 3
  with the class line on a line of its own. Red before: `Generate` returned an untyped `*fmt.wrapError` (the runner allow-list dropped the line), `classifyErr` answered `oom` for a typed
  `gpu_reset` whose message said "zoom lens timeout", the image lane's ledger class for a `comfy_unusable` failure was `oom`, 14 of 103 JS tests
  failed (all new ones; the real script exited 3 as before but printed no class line), and the three timeout tests that would hang against a
  server that never answers ended at their sentinel timer as assertions, not as a stuck runner. Nine mutants (`scripts/mutation-check.sh`, all `RED`): the runner
  allow-list without `comfy_unusable` (`TestGenerateReadsComfyUnusableFromAFakeRunner`, `TestRunnerClassReadsComfyUnusable`); `classifyErr`
  without its RunError branch (`TestClassifyErrHonoursATypedRunErrorClass`, the image-lane test); the abort signal that never fires, a POST without its signal, a GET without its signal, a changed submit
  bound (the timeout and signal tests, the real-socket test, the constants test); `comfyUnusableClassLine` always empty and
  `reportRenderFailure` without its class line (`reportRenderFailure`, the real-script test); the relay that drops the child's stderr
  (the two relay tests).
  Fix round, three findings. (1) The batch rows: `imagebatch_comfy_unusable_test.go` runs the REAL `runBatchJobs` of `render/batch-jobs.mjs`
  in a stub batch parent (job 1 renders, job 2 fails on its own with an out-of-memory, job 3 exits 3 with the class line, jobs 4-5 are never
  run) through `RunImageBatch`; red before: the four failed jobs were filed `[oom other other other]`, now `[oom comfy_unusable
  comfy_unusable comfy_unusable]`. `TestBatchErrClassFilesTheAbortRowsWithTheBatchsTypedClass` (a word trap, "zoom lens timeout", does not
  move an abort row; a job that failed on its own keeps its class) and `TestBatchErrClassKeepsReadingWordsWithoutATypedBatchError` (no
  batch error, an untyped one, a deadline batch's own row, a full-disk not-run row, and an exit-3 row under a `gpu_reset` batch are all
  unchanged). Five mutants, all `RED`: the not-run rows ignoring the typed class, the exit-3 row taking any typed class, the exit-3 row
  never typed, every failed row taking the batch's class, and the typed branch removed. (2) A timed-out `POST /prompt` is marked server
  unusable and a timed-out `GET /view` is not: `comfy-submit.test.mjs` pins both (and the class line), `comfy-render.test.mjs` pins the
  exit code 3 and the two lines; red before: the render exited 1 and printed no class line. Two mutants, both `RED`: the mark removed
  (the two POST tests), and every deadline marked (the GET guard). (3) The real-script test of `comfy-render.test.mjs` now bounds its child
  (30 s kill timer, an error handler): against a mutant that holds the child open for 90 s the old test was still running at 45 s and
  printed no result, the new one failed at 30.06 s with the bound in its message.

### Added — `audit-yaml` and the write gate refuse the every-expert `--cpu-moe` (W4)

- **The gap.** INV-1 (the cards do the inference, RAM is overflow only) sanctions one host-RAM use for experts: a measured partial
  `--n-cpu-moe N`. The bare `--cpu-moe` parks every expert layer in RAM. Nothing read a live file for it: `AuditSpill` matches only the
  partial form, and the seed gate `TestNoGPUTierParksTheMoEExpertsInRAM` reads `profiles.json`, so a hand-edited yaml could carry it
  undetected.
- **The rule.** `servingtmpl.AuditCPUMoE` (rule id `cpu-moe`) reads parsed entries (cmd, env, macros expanded) and refuses `--cpu-moe`,
  `-cmoe`, the draft-model forms (`--cpu-moe-draft`, `--spec-draft-cpu-moe`, `-cmoed`) and the environment twins
  (`LLAMA_ARG_CPU_MOE`, `LLAMA_ARG_SPEC_DRAFT_CPU_MOE`, any value but an explicit off). It never scans raw text: a stamped file records
  `"moe_26b": "--cpu-moe -ngl 999"` in its own stamp comment. The partial `--n-cpu-moe N`, `-ncmoe N` and `LLAMA_ARG_CPU_MOE=0` are not
  this rule's. It is pure and exemption-free.
- **Two callers, two sources of evidence.** `install render` runs the gate before the stamp exists, so `renderGate` exempts from the
  resolved profile: the cpu backend (no card to overflow from) and the off-matrix defaults' literal (`--cpu-moe -ngl 999`, which the
  installer still writes for a box that is not on the tier matrix). It runs after the spill refusal, so a partial placement that names no
  N keeps its more specific "no N" message. `audit-yaml` holds a file and reads its stamp, trusted only when it is a verbatim render (the
  spec hash is the hash of the basis it carries and the body still hashes to `body_sha256`): a verbatim cpu render is clean, a verbatim
  off-matrix render prints the finding as a `note:` line and does not fail the audit, and an unstamped file or one edited since its stamp
  is judged as a box with a card and the finding says so. An edited stamp exempts nothing.
- **Not changed (needs a decision).** The installer still writes the bare form in three places: the selftest R4.2 remediation
  (`setup/selftest.ps1`), the `install.ps1` unknown-profile fallback and `fallbackProfile` for the CUDA family. They are untouched, and
  the exemptions above keep each of them working; a yaml that R4.2 remediated reads as a `cpu-moe` finding on its next audit.
  `--override-tensor` placements are not read either.
- **Tests.** `internal/servingtmpl/audit_cpumoe_test.go` and the root `install_render_audit_test.go`, written first and seen red
  (`AuditCPUMoE` a no-op: the spelling table found 0 of 12 offenders; `renderGate` passed a table GPU tier carrying the flag; `audit-yaml`
  printed OK for it). Guards that were green before and after: the partial spill and the off switches, the stamp comment, the existing
  "no N" refusal, and every shipped tier on both OSes (`TestRenderedTiersCarryNoBareCPUMoE`). Mutants, each turned red by a named test:
  the `-cmoe` spelling, the environment twin, macro expansion of cmd and of env, each exemption in `renderGate`, an exemption read from a
  stamp at render time, the cpu exemption in `audit-yaml`, and an unstamped file passing. Read-only, a built binary over a live yaml
  with two `--n-cpu-moe 14` entries prints `OK`.
- ADR 0043 gains an amendment that says which evidence each caller reads.

### Added — `audit-yaml` flags an index pin on identical cards and a UUID that names no card (A-87; A-86 stays open)

- **The gap.** `CheckComposite` compares a composite tier's pins at render, and an index and a UUID cannot be shown to be the same card
  without the box's table. A live yaml that pins `CUDA_VISIBLE_DEVICES=0,2` on two identical cards records nothing about which card was
  meant, and a reordering of the PCI enumeration (the shipped triple-card template records that its board does it on power loss) moves the seat silently. Nothing asserted
  that every pin on such a box is by UUID.
- **The rules.** `servingtmpl.AuditPins` reads each entry as llama-swap runs it (macros expanded; the last setting wins, an inline one
  over the env list) against the box's card table. `pin-by-index`: a plain index that names a card whose model name another card also
  carries (the index of a card no other resembles names exactly one card and is left alone; an entry without
  `CUDA_DEVICE_ORDER=PCI_BUS_ID` is judged by the weaker form, any plain index while two cards share a name; cards with no model name are
  never called identical). `pin-unresolved`: a `GPU-` UUID or prefix that matches no card, because CUDA hides every device from the first
  pin it cannot resolve.
- **Severity follows who maintains the file.** A seed cannot carry a UUID, so the seeded index pins of a verbatim render
  (`win-triple-blackwell`) are the renderer's own output: a `note:` per entry, exit status unchanged, or the only way to satisfy the rule
  would be a hand edit, which turns the file HAND-EDITED and red under `--against-render`. The same pin in an unstamped or edited file is
  its maintainer's choice and a violation. A UUID that names no card is a violation in either. `audit-yaml` reads the table through the
  existing `renderCardTable` seam and only for a file that pins a device; with no readable table it prints
  `note: <file>: no card table: pin rules not applied (...)` and the exit status is unchanged, never a silent pass.
- **Not in this item.** A-86 stays open: comparing each resident to the card its tier intended needs the tier's layer table, which
  `CheckComposite` has at render and `audit-yaml` does not. The PCI bus id the register row also accepts is not in `gpuprobe.Card`, so
  only a UUID is the stable form. Promoting the seeded-pin note to a failure waits for an installer step that substitutes UUIDs.
- **Tests.** `internal/servingtmpl/audit_pins_test.go` and the root `install_render_audit_test.go`, written first and seen red with the
  rule a no-op (the identical-cards, unresolved-UUID, macro and inline-pin tests failed; `audit-yaml` printed OK for `0,2`). Guards: a
  unique card's index, UUID pins on identical cards, three distinct names, nameless cards, no card table, and that a file with no pin
  never reads the table. Mutants, each turned red by a named test: the stamp-state split (both directions), the shared-name condition,
  the unresolved-UUID rule and the weaker no-PCI-order form. On a three-card box a freshly rendered `blackwell-3x16` audits OK with 13
  notes and exit 0, and the live yaml of that box (19 UUID pins) audits OK.

### Changed — the hard-rule seed gates are shown to fail on synthetic fixtures (W0.1)

- **The gap.** `TestNoGPUTierParksTheMoEExpertsInRAM` and `TestEveryAgentSeatIsChosenNotDerived` read the real `profiles.json` only, so a
  gate that went blind (a skip that swallowed everything, a branch that never fires) passed for as long as the real seeds were clean. The
  0.132.4 note says all three seed gates "were RED on the previous seeds"; no mutation proof was recorded.
- **The change.** The two rule bodies are `checkNoGPUTierParksTheMoE` and `checkAgentSeatsAreChosen`, functions of a decoded document;
  the real-document tests call them unchanged in effect. `seed_rules_fixture_test.go` hands them synthetic documents: a cuda tier that
  seeds `cpu_moe` is refused and names the tier; the cpu tier, a tier that drops or fits the 26B, a `vllm_seat`, an explicit
  `config_seed.agent_model` beside a differing `resident_tier`, the workhorse read from `config_seed.model` and the harness default are
  guards; a tier that derives its agent seat from `resident_tier` is refused.
- **Mutation proof.** Deleting the `Backend == "cpu"` skip, the `cpu_moe` clause, the `explicit` branch, the `VLLMSeat != nil` branch or
  the workhorse read from `config_seed.model` each turns a named synthetic test red.

### Fixed — review round on the cpu-moe and pin audit rules (A-87, W4)

- **A pin that names no card is a finding, not a pass.** `AuditPins` judged only a `GPU-` UUID that matched no card, so
  `CUDA_VISIBLE_DEVICES=7` on a three-card box, or a typo, a bare UUID without its prefix or a UUID with a non-ASCII hyphen, printed `OK`
  while CUDA hid every device from that pin on, the same hazard `pin-unresolved` documents for a UUID (a seat on fewer cards than it was
  sized for, or on none). `pin-unresolved` now also covers an index that no card has (under `CUDA_DEVICE_ORDER=PCI_BUS_ID` the nvidia-smi
  index of a card on the table, otherwise a position past the last card) and any token that is neither a plain index nor a `GPU-` UUID.
  `PinsCUDADevices` counts those tokens, so the card table is read for a file that holds only such a pin. `-1` (it hides every card, so it
  is no pin on one) and a MIG instance id (a slice of a data-centre card, which a table of whole cards cannot resolve) are not judged. An
  index that names no card no longer also counts toward the ambiguity rule, so it is one finding and not two. It is a violation in a
  verbatim render too: a seed that pins index 2 on a two-card box is a render for a bigger box, and a note would excuse it into a dead seat.
- **`audit-yaml --other-box`.** The pin rules compare a file's pins with the cards of the machine that runs the audit, and nothing in a
  file or its stamp says which machine it belongs to. A file kept on another machine (the stamp records that machine's paths so the replay
  does not call it stale) failed `pin-unresolved` on its own UUIDs and exited 1, where before this branch the same call printed `OK`. The
  default is unchanged. `--other-box` says the files belong to another machine: the pin rules are skipped with a `note:` line that says why
  and the card table is not read; every other rule still runs. Every pin violation now ends with what it was judged against and the flag (a `note:` for a verbatim render's seeded index pin does not).
- **Four tests that passed without the rule they name.** A mutant of each survived the servingtmpl tests at the first review commit
  (`5da1e386`): dropping `--spec-draft-cpu-moe` from the spelling table, matching the `GPU-` prefix case-sensitively (the lower-case UUID case
  expected zero findings, so it passed either way), reading the env pin over the inline command pin, and treating `-1` as an index. Each now
  has a case that turns red under it: a row for the spelling, a lower-case UUID that names no card, an entry that pins in both places (in
  both directions), and `-1` on a box where two cards share a name.
- **Tests.** The no-card cases were written first and seen red (each of the first nine table cases found 0 findings: the audit was
  silent; the flag tests failed on the card table being read, the missing note and the missing hint). Mutants, each turned red by a named test: the card-count bound, the
  PCI-order lookup, the unreadable-token branch, counting such a token as a pin, the MIG exemption, the `-1` exemption, the empty-token
  exemption, an unresolved index counted as ambiguous, the flag ignored, the flag skipping the pin-less file check, the hint dropped, the
  flag silencing every rule, and a verbatim render noting every pin finding. A guard walks every tier on both operating systems against a
  box of eight distinct cards and finds no pin the rules cannot place (`TestRenderedTiersPinOnlyWhatThePinRulesCanRead`; it goes red when an
  index stops being read as one).
- **Not changed.** `CUDA_VISIBLE_DEVICES=-1` is still judged by no rule: it hides every card, which `cvd-empty` names only for the empty
  value. That is a different rule from this one and a change to `Audit`, which runs over every shipped template.

## [0.183.0] - 2026-10-12 - the display card is open to jobs up to a cap at any presence, a wrapped command pinned outside its lease is moved onto it, a dead wrapper whose job still runs keeps its card, and one bad subtask no longer rejects a whole call

### Fixed — `TestTheAnchoredClockDropsTheAdmissionAllowance` no longer fails on a loaded box for a cost that is not the poll budget

- **The defect.** The test failed on a loaded Linux gate host and passed on an idle one. Run alone with 8 concurrent workers of 25 runs each (load about 2 on 16 cores) it failed 161 of 200 at the 0.182.0 merge and 179 of 200 at 0.181.0, with elapsed times of 600.1 to 694.7 ms against its 600 ms line (wall 300 ms plus the 300 ms admission allowance). In ordinary whole-package gates it failed once in thirty or more (600.25 ms on a Windows host that was running other gates; that run was not traced).
- **The cause, measured.** The delegator was right. In 315 failing runs traced across two stress sets the clock anchored on the first running poll (2 to 14 ms after Run began), the poll loop left 350.05 to 353.98 ms later (0.03 to 4.0 ms past its 350 ms deadline) and gave up without a request, because the job was last seen running. The rest of Run's elapsed time sat in the first result a process records: `buildinfo.BuildSHA256` hashes the whole running executable once per process, and that took 245 to 335 ms (median 267 ms) for the 30 MB test binary on the loaded host, against 56 to 60 ms for the 23 MB Windows test binary on an idle workstation. Everything after it, the corpus line, the ledger row and the PAIR terminal, took 0.7 to 2.8 ms. The test compared Run's whole elapsed time with the budget, so 350 ms of poll plus 245 ms of hash crossed 600 ms and the fastest failures landed at 600.1 ms. In a whole-package run an earlier test has normally paid the hash already. As a control, the unchanged test run in one process after another test that records failed 0 of 200 under the same load.
- **The fix.** The product is unchanged. The fake node now stamps the first poll it serves (the one that publishes the started wall, where the clock anchors) and the last, and the test asserts the span between them stays under wall plus grace plus half the admission allowance (500 ms; the anchored budget is 350 ms and the allowance, if it were back in, makes it 650 ms). The lower bound, that the delegator does not abandon a job inside the node's own wall, stays on Run's elapsed time, where extra bookkeeping can only help it.
- **Tests.** Under the same 8 x 25 stress the test failed 161 of 200 before and 0 of 200 after, and 0 of 200 again with 12 CPU spinners competing (load 7.4). Two mutations turn it red: adding `admissionSlack()` back into `anchoredPollBound` (poll held 648 ms) and skipping the anchor so the dispatch-time bound with the allowance stays in force (poll held 648 ms).
- **Not changed, for a follow-up.** The first record in a process still hashes the executable on the finishing path, so the first result of a short-lived delegator process returns that much later (about 56 to 60 ms idle, up to 335 ms measured under load). It happens after the job is given up or answered, so it holds no node and no budget; hashing at process start would remove it from the answer's path.

### Changed — a command pinned to a card outside its lease is moved onto the leased card, never refused (GPU-F9)

- **What was wrong.** `gpu reserve --devices 0 -- <cmd>` pinned the command to the held card with `CUDA_VISIBLE_DEVICES`, but a command
  that carried a pin of its own (`COMFY_CUDA_DEVICE`, `COMFY_CARD_UUID`) was left alone and only warned about: ComfyUI and the render
  layer turn that variable into the card they run on and reset `CUDA_VISIBLE_DEVICES` from it, so the job ran on the card it named,
  outside the lease, and another session could be holding that card. The warning was the whole defence.
- **The fix is a move, not a refusal.** With an explicit lease (`--devices` or `--cards`), a program pin that falls outside the held
  cards, or cannot be resolved to a card, is rewritten to the held card and the command starts: `COMFY_CUDA_DEVICE` becomes the held
  card's ComfyUI device index from the card table (`gpu_comfy_order`; never the nvidia-smi index the lease is keyed by),
  `COMFY_CARD_UUID` becomes the held card's driver uuid, each in its own kind (the render layer refuses an index beside a uuid, and a
  uuid turns an unkeyed launch into a keyed instance on another port). One stderr line names the old pin, the new pin and the lease. A
  pin of k cards under a lease of n >= k cards maps to the first k held cards in the order the index is counted in; more cards than the
  lease holds, a `--cuda-device` on the command line (the program's own argv), and a held card whose device index
  is unknown (no `gpu_comfy_order` on a multi-card box: no guess) keep the old warning, now with the reason. A `--cuda-device` in
  `COMFY_EXTRA_ARGS` is not read by the wrapper (no warning, and an environment pin beside it is moved: the render layer drops the
  extra args' flag when one is set). The index is read and
  written in the order the child runs under (`CUDA_DEVICE_ORDER=PCI_BUS_ID` makes it nvidia-smi's). The pin is judged on its own beside
  an inherited `CUDA_VISIBLE_DEVICES`. The wrapper never exits non-zero for a pin, and there is no `--allow-outside-pin`.
  A flag-off host, a `--whole-node` lease and a lease derived from the command's own pin never reach the re-pin.
- **Tests.** `gpu_reserve_repin_test.go` (fourteen, including the verb run in a child process: the wrapped command exits 7 and the wrapper
  returns 7 with the command started on the held card; a frozen copy of the old `confineWrapped` is the oracle that every input without
  an outside program pin gets the old answer) and `internal/gpulease/repin_test.go` (seven), written first. Red before: the re-pin tests
  failed on the tree without the change with, for example, `the child must be started on the held card's ComfyUI device (1), got env ""`
  and, through the verb, `the command must start on the held card (ComfyUI device 1), saw "2"`; the guards (pin inside the lease, argv
  pin, the oracle, flag-off/whole-node/derived) hold before and after. Mutation-checked with `scripts/mutation-check.sh`: eleven changes,
  each reverted in turn fails a test it names. Docs: `docs/systems/gpu-lease.md` ("A pin outside the lease is moved, never refused"),
  `docs/OPERATOR-GUIDE.md`, ADR 0018 (amendment of 2026-10-12).

### Changed — a dead wrapper with a live process tree keeps its claim (GPU-I3, GPU routing P10)

- **What was wrong.** The reclaim rule read "the wrapper's pid is gone" as "the job is gone". It is not: a wrapper that was killed,
  crashed or lost its session left the command it started running on a card in use, and the next acquirer was handed that card at
  once (two jobs on one card). When a lease was lost the wrapper also stopped only its direct child, so a grandchild kept the card
  the same way. The observation side (`tree-orphan` in `gpu status`, `offload_status`, the allocator) existed and nothing produced it.
- **The wrapper records what it started.** `gpu reserve ... -- <command>` runs its command inside a containment (a job object on
  Windows, created suspended and resumed once the child is inside; its own session and process group on Unix) and writes the tree,
  the pid and start identity of each process, into the lease record (`tree`, additive and `omitempty`), rewriting it only when the
  set changes. New package `internal/proctree`; the agent shell's cage builds on the same job object type, with its kill-failure
  report kept.
- **A dead wrapper over a live tree keeps its claim, and the lease reads `tree-orphan`.** A lease whose wrapper is gone (pid dead, or
  alive but recycled) is not reclaimable while any recorded member is alive by pid AND start identity. It is bounded by observation,
  never by a timer: every read checks the members again, so a recycled pid cannot hold a card, a card frees itself at the first
  read after its members are gone, and hours past its window and heartbeat a tree-orphan is still held while its members live.
  A lease whose wrapper is alive is judged exactly as before. A lease that recorded no tree (a wrapper that predates this change, the
  pipeline's own leases, a detached holder) and whose wrapper is gone is reclaimed at once unless a live process is tied to it: a
  launch marker that names the lease epoch (or a default-instance marker that began at or after it with no other lease present) or,
  on Windows, a child of the dead wrapper in the process table that began after the lease; with none alive it is reclaimed as before. The driver's compute listing is not used: under WDDM it names no process for most.
- **A lost lease or an interrupt stops the whole tree**, not just the command, except a live ComfyUI instance whose launch marker
  names another lease (a kept instance another lease reused is that lease's job now); the wrapper says what it left running. An OS
  that refuses the containment still runs the command, uncontained, and says so. **On Unix the command now leads its own session**, so a
  terminal hang-up that used to reach it ends only the wrapper; the job keeps running and its lease keeps the cards as a tree-orphan.
- **`--die-with-wrapper` (opt-in, off by default)** ends the tree when the wrapper exits or dies (Windows: the job is created
  kill-on-close). It is off because it makes the wrapper a single point of failure for a long render; Linux has no handle whose
  closing ends a tree and refuses the flag before the reserve queues.
- **What a reader sees, and what frees it.** The `tree-orphan` verdict (it leads the precedence; the brief line leads with
  `ORPHANED`), the `wrapper: pid N is GONE, but the job it started is still running` lines of `gpu status`,
  `activity.holder.tree_pids`, the waiter's sentence, `tree_orphan` and the `tree-orphan` verdict on `/fleet/health` leases
  (additive). The waiter, `gpu status` and the brief line name the one command that frees a tree-orphan today, `gpu release
  --epoch N` (the takeover verb does not exist in this build and is not offered): it ends the claim at once, stops only the ComfyUI
  instances kept under the lease, and leaves the rest of the job running. The allocator treats a tree-orphan's cards as claimed, so a
  `--cards N` waiter is handed other cards, and a seat warm-back does not load over them.
- **The marker directory is armed in one place.** The lease readers read launch markers from `comfy_dir`; `modelaffinity.SetComfyDir`
  (already called from `config.loadArmed` with the same directory) now arms them too, so `config.go` is untouched.
- **The stop primitives a takeover needs are built and wired to nothing.** `proctree.PlanStop` and `Stop` stop only a recorded tree or a
  launch marker's process, re-check each member's start identity right before signalling and verify it dead, and report anything else
  on a card as `card still busy by pid N (not ours)`; `comfyinst.StopMarker` interrupts, frees and kills a marker's instance after the
  same fingerprint proof a lease's own holder gives; the Windows job confirms on the process handle that a pid it was listed is still
  in the job before it ends it.
- **Cross-version, and the deploy note.** A reader built before this change ignores `tree` and reclaims a dead wrapper's lease at
  once, as it always did (pinned against the preserved old reader). A host is protected only once every reader on it is this
  build: after the deploy run `gpu doctor`, restart the MCP servers and the fleet daemon, and compare build versions, because the
  audit's `FormatSignature` is not bumped. The key is additive, so nothing an older reader reads becomes wrong. Nothing is killed
  by a deadline or by another process.
- **Tests.** Ported test-first from the P10 branch (`internal/gpulease/tree_test.go`, `internal/proctree/*_test.go`,
  `internal/comfyinst/stopmarker_test.go`, `internal/gpualloc/treeorphan_test.go`, `gpu_tree_test.go`, `gpu_tree_warm_test.go`,
  `gpu_tree_hermetic_test.go`, and the Windows job tests), including a real wrapper killed with a real grandchild beside it (a scratch
  lease directory and a scratch ComfyUI directory, no card). The tests that name the command that frees a tree-orphan were written
  first against the old wording and failed on it. Mutation-checked with `scripts/mutation-check.sh`: twelve changes (Reclaimable
  ignoring the tree, the start identity dropped, a timer that frees, a wrapper that records nothing, a healthy wrapper read through the
  process table, a lost lease that stops only the direct child, the protected set unused, the waiter sent to the takeover verb, the
  Windows stop ending a pid outside the job, the marker directory not armed, a marker of another lease holding this one, the brief
  line without the release command), each reverted in turn fails a test it names. Docs: ADR 0084, `docs/systems/gpu-lease.md` ("A dead
  wrapper with a live tree keeps its claim"), `docs/OPERATOR-GUIDE.md`.

### Changed — records: the dropped pid-0 umbrella is accepted, the VRAM-charge measurement is stated with its limits, and the display card's cap is told in the lease guide (P2, F70, A-RAM-15)

- **P2: the pid-0 `meta.json` umbrella stays dropped, and that is now the decision, not a pending deviation.** ADR 0018 and
  `docs/systems/gpu-lease.md` said "Deviation from plan rev 2, pending acceptance" and that the plan's P2 acceptance was not met. The plan
  owner accepted it on 2026-10-11, after a green `gpu doctor` on the reference box (every reader it reached understood the per-epoch
  fence; nothing older than the record format was found), so the audit gate does the work the umbrella would have done. The four tests the
  plan named for the umbrella were never written and are recorded as superseded (the old-reader fixture test pins the hazard that replaces
  the last of them). The umbrella can be restored later without a format change.
- **F70: the VRAM-charge question is answered, with its limits.** The "partly measured, not settled" passage of the lease guide is
  replaced by the 2026-10-11 measurement: on the reference box's Windows driver every VRAM allocation is charged 1:1 to the process's
  private bytes and to system commit (a 4.0 GiB `cudaMalloc` raised private bytes by 4.0 GiB), the stock ComfyUI loader charges the whole
  weight file to commit (14.73 GiB on one Wan expert), and a card-only run moves system commit by tens of GiB while available RAM stays
  above 45 GiB. The guide says what is unchanged (the brake still reads commit and available RAM; a card-only recipe declares `--ram 0`,
  which is always admitted) and what is not known (one driver on one box, system-wide counters, other nodes unmeasured). Documentation
  only: no admission code changes.
- **The display card (A-RAM-15, `gpu_display_cap_pct`).** The allocator section of the lease guide and the operator guide tell the cap
  that the display-cap change introduces: the card is allocatable up to 60 percent of its VRAM at any presence (the desktop's use counted),
  only with a declared `--vram`, `0` closes it, an unknown display stays closed, and the cap bounds admission only.

### Fixed — the lease-guards review round: `--die-with-wrapper` no longer ends an instance another lease took over, a tree-orphan's host RAM is no longer charged twice, and the docs say what the code does (GPU-F9, GPU-I3 / P10)

An independent review of GPU-F9 and GPU-I3 (the P10 port) found seventeen entries over twelve distinct defects; each was read against the
source, scored with Jev, and either fixed red-first or answered. Every fix has a test that failed before it and a mutant that turns
that test red (`scripts/mutation-check.sh`).

- **`--die-with-wrapper` (Windows) killed what the wrapper had just said it spared.** A lost-lease wrapper spares a ComfyUI instance
  another lease re-stamped, but its `finish()` then closed the last handle of the kill-on-close job and Windows ended every process
  still in it, the spared one included, which ended the other lease's job. The job is now disarmed once something is spared (after
  everything else of the tree is ended), at the lost-lease stop and again at `finish()`, and the wrapper says the guarantee stops at
  that instance (`proctree.Tree.DisarmKillOnClose`). The flag's help and the lease guide carry the carve-out.
- **`--die-with-wrapper` was silently not honoured when the command could not be put in the job.** The command ran uncontained with a
  warning that never named the flag; a 20-hour render outlived the wrapper that was meant to take it down. It is refused now, as when
  the job object cannot be created.
- **A pin the wrapper cannot move was invisible beside an inherited `CUDA_VISIBLE_DEVICES`.** More cards pinned than the lease holds, or
  a held card with no known ComfyUI index, was reported only by appending to a note the generic confinement had made, and an inherited
  variable naming a held card makes that confinement silent: the command started on cards outside its lease with nothing printed. The
  trespass sentence with the reason is now printed in that case, and a moved uuid pin no longer hides an index pin that stayed.
- **A tree-orphan's declared host RAM was charged again as still to load.** The guard measured the processes below the lease's holder,
  which for a tree-orphan is the dead wrapper (the job is reparented away on Linux), so about nothing was subtracted and every later
  grant that declared RAM waited behind a host-RAM message for as long as the orphan lived. A tree-orphan now stands alone and is
  measured from its recorded live members.
- **A stale launch marker whose pid was recycled made a lost-lease wrapper spare a process of its own tree.** `PIDsOfOtherLeases` now
  applies the start-versus-marker proof the stop already used.
- **The recorder listed the console host.** On Windows the recorded tree was the job's list unioned with the process table, which added
  `conhost.exe` (a child of a command that has a console of its own, not in the job); a recorded member that is not the job can keep
  a dead wrapper's claim after the job is gone. With a contained command on Windows the record is the job's exact list.
- **The F9 docs promised a warning no code prints.** A `--cuda-device` in `COMFY_EXTRA_ARGS` is not read by the wrapper (no warning),
  and an environment pin beside it is moved and wins (the render layer drops the extra args' flag). The guide, ADR 0018, the F9 entry
  above and the code comments now say so, and a test pins both halves.
- **Four statements said a lease that recorded no tree is "free at once" while the code, the next sentence and the ADR hold it on a live
  tied process.** They now say: reclaimed at once unless a launch marker or, on Windows, a child of the dead wrapper ties a live process
  to it; with none alive, exactly as before (a judge-level test pins that). The guide, ADR 0084 and the `Reclaimable` comment also say
  what a recorded tree cannot cover: it is frozen when the wrapper dies, and the process table is read only for a lease that recorded
  none, so a process started after the last record that outlives every recorded member is not held (the accepted limit).
- **A whole-node `gpu release` could be undone by a restamp in flight (found by the Windows root gate, not by the review).** A restamp
  reads the lease record, writes a replacement and renames it over the record, all under the epoch lock; the whole-node release
  removed the claim without taking that lock, so a release that landed between the restamp's read and its rename was followed by the
  rename, which put the record back and left the lease the operator had just ended held, with its wrapper none the wiser. The
  wrapper's tree record (a restamp right after its command starts) made the window reachable, and it surfaced once the recorder no
  longer walked the whole process table on Windows (that walk took about 20 ms on the development box and more on the slower
  hosts, and had delayed the restamp past the release the handoff test issues): `TestReserveDoesNotWarmBackAfterLosingTheLease`
  failed on both Windows hosts. The release now takes the epoch lock.
- **Answered, not changed.** An overdue tree-orphan is ranked last in `/fleet/health` and is not a refusal, exactly as every overdue
  lease since 0.113.27 (a refusing node keeps the capacity wait from asking the one remote that could take the work); the field comment
  said "never freed" without saying that, and a test now pins both halves.
- **Tests.** The repin verb tests take the isolation every real-wrapper test needs (the lease and config redirects stripped from the
  driver, a scratch `comfy_dir`), the real-wrapper helper spawns are window-less with a source pin over the files that start them, the
  launch markers the lease-lost tests write carry the start time of the process they name, and the tree-orphan test polls for the claim
  to free instead of reading it once.

### Fixed — one bad subtask (or research page) no longer rejects the valid ones in the same call (D-111, part 1)

`agent_delegate` returned ONE flat `deferred: true, reason: "subtask N: ..."` for the whole call the moment any subtask failed intake (a `context_paths` file that is not there, a context past the box's cap, a blank goal), so a valid sibling never ran: a call that listed one scratch file that was missing lost the answer to the other subtask too. `offload_research` did the same for a page whose contract failed intake.

- **Each subtask is prepared on its own.** `delegate.PrepareBatchWithCap(specs, readRoot, cap)` prepares every spec exactly as `PrepareContractWithCap` does and returns the valid contracts with their original positions beside the per-position rejections; preparing one spec never depends on another. The valid contracts go to the runner as before.
- **A rejection is a contract-class defer at its own position.** The call answers one result per subtask sent, in the order sent. The rejected one is `deferred: true`, `defer_class: contract` (the caller's mistake, not a broken stack), `reason: "subtask N: <cause>"` (`source N:` for a research page, N being the page's place in the `urls` list), on no node and no seat, and it writes no ledger row. It counts in `summary.deferred` and not in `summary.infrastructure` or `lost_to_stack`, so the call is not error-flagged for it. `acceptance_lint` stays aligned with the original positions.
- **Two refusals stay whole-call.** The 8-subtask cap is read off the subtasks SENT (`delegate.CheckSubtaskCount`, the words `RunWith` uses), so nine subtasks with one bad one are refused for their number exactly as before and a rejection never makes room. A call with no valid subtask keeps its flat defer, which now names every bad subtask (`subtask 0: ...; subtask 2: ...`; one bad subtask reads as before).
- **Read `results[i]` by input position and tolerate a contract-class defer inside a delivered call.** The tool descriptions and `docs/systems/mcp-server.md` say so.
- **Not in this change: `extra_read_roots`** (attaching files from outside `read_root`). It widens the delegator-side reader with no read floor at intake, and the inlined bytes can travel to fleet nodes; it waits for that floor. The `delegate` (`--contract`) and `research` command-line verbs follow the same rule since the fix round below.
- **Tests, written first and red** against the unfixed code: the runner never ran for the valid subtask (`the runner ran [], want exactly the valid subtask`), the call carried no results, the flat defer named only subtask 0, nine subtasks with one bad one answered `subtask 0: ...` instead of the cap, and an oversize research page deferred the other two pages. Mutants, each compiled and each killed: a rejection counted as infrastructure, the rejection's class changed, the lints indexed by contract instead of by input position (both doors), the cap read off the prepared contracts, any rejection refusing the whole call, the flat reason naming only the first, the rejections not spliced back (both doors), a research page named by its contract's place, the cap lifted to the batch bound, the skipped count dropped, the valid contracts' positions lost.

### Fixed — a run whose workspace root vanished defers as infrastructure and is re-placed once, instead of answering from nothing (C-79)

When the directory under a run is taken (a startup sweep did it before C-78; a cleaner, an indexer or an operator still can), every file tool answered `workspace root unavailable`. The loop fed that back as an ordinary observation, the seat wrote a final answer from nothing ("I could not read the file"), and the delegator scored the answer `failed_verification`: a fault of the box filed as the model's.

- **A tool that cannot open the workspace root says so with a type, and the run ends on it.** `read_file`, `list_dir`, `search_files`, `summarize_file`, the write door's tools and `update_plan` return an `agent.RootGoneError` (the same words the seat has always seen), and `Loop.Run` returns it once the turn's calls are recorded, before the seat is given another turn, or before the first turn when a replayed context read finds the root gone. A file that is missing inside a root that opens is still an observation, and for a run with no write door a root lost after its last tool call changes nothing: the finished answer stands. Behaviour change, accepted: a run that had already found its answer and then loses its directory on the next tool call is stopped at that call rather than let go on.
- **The node files `defer_class: infrastructure`** with a reason that opens `workspace root unavailable: <root>: <cause>` (`core.RootGoneReason`), a trace that carries the failed call, and no answer. The delegator's in-process local placement and a fleet node's job run the same function, so both are covered. The `agent_run` door answers `deferred: true` with the reason, and files `defer_class: infrastructure` when the read root existed as the call came in; a `read_root` that never existed is a typo that `agent.Build` lets through (it only makes the path absolute), which fails the same way at the first tool call and stays classless, like every other loop error at that door. The ledger files it under `reason_code: infrastructure`, never `seat_down` (pinned for the operating systems' own causes).
- **A write-door run whose tree is gone before the door opens, or by the time it closes, defers the same way and publishes no diff.** The door's deliverable is a diff of the tree, so it asks for the tree itself where no tool is there to report the loss. A read root lost while the run waited for its seat used to file a `write` defer, the class that says another node may have opted in and that the delegator does not re-place; a tree lost after the seat's last tool call used to publish the snapshot of a directory that was not there as a diff deleting every file, beside a finished answer.
- **The delegator re-places it once on another node when one can take it** (`delegate.RootGoneDefer`, the fourth `infrastructure` defer worth a retry, after the coherence, warm-up and seat-down ones), so the better class does not take away the second chance the failed verification used to get. It matches the reason's prefix only: a node without this change keeps filing the same fault as a failed answer, and every other `infrastructure` defer keeps the retry rule it had. On top of the named-seat `agent_run` (D-125) and the final-answer fit (D-102) of this release, which own the same door and function.
- **Tests, written first and red** against this base (the new types and the defer helpers present, nothing wired): `scope.open` on a removed root returned a wrapped `*fmt.wrapError`, not a `*RootGoneError`, from every tool; the loop gave the seat a second turn and the run ended `done` with `I could not read the file.`; the replayed context read did the same; the local and node-side runs published that answer instead of deferring; the write door filed a `write` defer for a root lost before it opened and published a diff deleting every file for one lost after the last call; `RootGoneDefer` was not retry-eligible and the contract was not re-placed on the second node; `agent_run` answered a root vanished mid-run with a finished answer instead of deferring. Guards that pass by design on the unfixed code: a missing file inside an open root, a failing tool inside an open root, a root lost after the last read of a run with no door, a read root that never existed, the ledger code for the operating systems' causes. Mutants, each compiled and each killed: the typed error dropped from either dispatch path or from the replay, the replay's report ignored, `scope.open` and `update_plan` back on the raw open, any tool error ending the run, the retry rule without the lost-root case, the match without its prefix, the `agent_run` class without the existed-at-entry check or with it always false, the door's own root check dropped before it opens and before it closes, a door-less run asked for its root, the defer filed under another class, and the reason without the root it names.

### Fixed — fix round of the delegate-intake branch: what a mixed call says about itself, the CLI verbs, and the second placement of a lost-root run (D-111, C-79)

A review of the two items above found six defects at their edges. None changes what a call that went well returns; each is a place where the new behaviour said the wrong thing, or did less than the same change on the other surface.

- **Progress notifications of a mixed call speak in the caller's numbers.** The engine is handed only the subtasks that passed intake, so its events numbered them among those: for `[ok, refused, ok]` the third subtask announced itself as `subtask 2 of 2` and the call ended at `2 of 2 done` while returning three results. `PreparedBatch.MapProgress` puts each event on the subtask's input position and the call's real total and counts the refused subtasks as done from the start; the opening says `delegating 3 subtask(s), 1 refused at intake`, and the heartbeat counts them too. A call with nothing refused reports exactly what it did. Both doors (`agent_delegate`, `offload_research`).
- **A call the engine refuses as a whole still names the subtasks intake refused.** An unknown `route`, say, leaves nothing to run and no result to splice a rejection into, and the rejections were dropped: the caller fixed the route and met the malformed subtask only on the retry. The whole-call defer now ends `; also refused at intake: subtask 1: ...` (`source N:` for a research page, by its place in the `urls` list), and the two command-line verbs return the engine's error the same way. `PreparedBatch.AlsoRefused`, `cliEngineErr`.
- **The `delegate --contract` and `research` command-line verbs follow the rule too** (`main.go`, which the plan did not name: the invariant held on the two MCP doors only). Both prepare each subtask or page on their own through `cliIntake`, run the valid ones, and print the refused ones as contract-class defers at their own positions; they exit 0 for them, as for every contract-class defer. A file with nothing valid in it is refused naming every bad subtask (it named the first), a research page by its place in the url list (it was named by the contract's place), and the eight-subtask cap is read off the file before anything is prepared, so a rejection never makes room under it.
- **A lost-root defer's second placement is budgeted like a seat-down defer's.** The first-pass retry floor read the min_turn of the seat that served the run, a seat the retry never uses (it goes to another node, whose own floor still applies once it is chosen), so a flagship that records a ~270 s cold load refused the re-placement the defer is promised on a 300 s contract. The node's `admission_wait_sec` (a cold load before the contract's wall began) was charged to the retry's budget; it is credited back now, bounded by one contract wall. Nothing else is: the wall the run spent working before its directory went was work.
- **A lost-root defer with nowhere to go says so.** With one node, `route=local`, or every eligible node already tried, the defer was published with an empty `retry_note`; it now reads `retry skipped: the run on <node> lost its workspace root and no other node could take the contract (<why>)`, in the words of the seat-down defer's note.
- **A tree the write door finds gone no longer outranks the cause the loop ended on.** The node-side check ran whatever ended the run and was filed ahead of every other cause, so a write-door run that hit its ceiling (or lost its seat, or its card) and whose job directory also went was filed as root-gone: the budget signal the delegator sizes contracts from was hidden, and a seat-down defer lost its own credit. The check still withholds the write set (a snapshot of a tree that is not there reads as every file deleted) and now says so in `write_note`; it makes the lost root the run's cause only when the loop ended cleanly, and a root the loop's own tool found gone still ends the run, as before.
- **Tests, written first and red** (the delegate, mcpserver and root ones against the unfixed code, the pipeline one against the unfixed precedence): the retry skipped on `floor 387s, min_turn_sec of the seat on node-a` with the 300 s budget 299 s unspent; the credit was 0; the note was empty on both nowhere-to-go shapes; the third subtask announced `subtask 2 of 2`; the engine's error carried no rejection; a ceiling beside a gone tree was filed as `workspace root unavailable:`; and on the CLI the all-rejected file named `subtask 0` only, nine subtasks with one bad one answered `subtask 0: ...` instead of the cap, and neither verb prepared through the helper or spliced. Mutants (`scripts/mutation-check.sh`), each compiled and each killed: the lost-root case dropped from the first-pass floor exemption, from the credit and from the nowhere-to-go note; the progress mapping's index, total and done count each dropped, the refused count dropped from the reporter, the reporter's total taken from the valid subset; the rejections left off the engine's error (both doors, both verbs); the lost root made the run's cause beside a ceiling, the write-set guard dropped (the diff then deletes every file) and the note emptied; on the CLI, the cap read off nothing, a page's place in the url list dropped, the lints indexed by contract, the splice dropped from either verb, the all-rejected refusal removed, the door tag dropped, and the engine's error returned bare from the helper or from either verb.

### Changed — the display card is allocatable up to a cap at all times, and the operator's presence neither opens nor closes it (A-RAM-15, `gpu_display_cap_pct`)

- **The rule.** The operator's order of 2026-10-11: the display card may be used up to 60 percent at all times, unless he says he needs it.
  The card allocator behind `gpu reserve --cards N` and the media admission path did the opposite: it never handed the card the monitor is
  attached to to an unnamed request while `operator_presence` read present (the default), and with the key set to `away` or `auto` it opened
  the card only above the display layer's `display_floor_gib`. The presence reading is gone from the allocator; the card is bounded by a share
  of its VRAM instead.
- **The key.** `gpu_display_cap_pct`: whole percent, default 60 when unset, `0` closes the card to the allocator (the "I need it" switch, and
  not the same value as unset), `100` is the whole card, a value outside `0..100` refuses the load naming the key (and reads as closed, never as
  the whole card, where a caller goes on with the refused value anyway: `Load` hands back the Config beside its error and `gpu reserve` proceeds
  on it). The display card is
  allocatable while the VRAM in use now (the desktop's included) plus the request's declared footprint is at most that share of its total.
  That is the display layer's `display_floor` arithmetic (`free - footprint >= floor`) with the floor set to the rest of the card; a
  `display_floor_gib` the layers also declare still holds when it is the larger of the two. On the reference three-card box (display card with
  2.6 GiB in use of 15.9) a 6 GiB request can land there and a 7.5 GiB one cannot, and the skip says `gpu_display_cap_pct 60 % of 15.9 GiB`.
  `AllocInput.DisplayCapPct` replaces `AllowDisplay`, and `gpualloc.Deps.Presence` goes with its only reader.
- **The switch is live.** A process never reloads its config, so the value read at start would leave the switch dead for a `gpu reserve --wait 8h`
  already in line, a detached holder, and the long-lived MCP server that places media calls. `Config.LiveGPUDisplayCap` re-reads
  `gpu_display_cap_pct` from the file the Config was read from (`Load` records it, `Config.WithSource`) each time `gpualloc.BuildInput` builds
  the allocator's input, so the pick and the grant-time check both see a `0` written after they started. A file that cannot be read at that
  moment (an editor part-way through a save) keeps the last value read from it, never the load-time one; a key removed reads as the default 60;
  a Config with no source file answers with its own value. `GPUDisplayCap` stays the value at load.
- **What stays closed.** A request that declares no footprint (`--vram`) is not given the display card under a cap below 100 (at 100 with no
  `display_floor_gib` declared the whole card is open and nothing is held to a footprint, so a request that declares none, every media call, is
  given it with no VRAM check on that card, as on any other; `TestACapOf100WithNoDeclaredFloorTakesAJobThatDeclaresNoFootprint` pins the
  exception and the three places it stops); a reading
  that could not say which card the monitor is on keeps every card closed whatever the cap (before, an operator-away setting overrode that,
  so a degraded reading now refuses the cards of a multi-card box as it always did at the desk); `--devices` still names any card on the
  operator's word. The llama-swap twin guards (`display_floor`, `presence` in `internal/placement/guards.go`) are untouched.
- **The grant asks again.** A request queued on the display card is put to the same rule when its turn comes (`DesktopRefusals` through
  `GrantCheck`): a card that grew past the cap while the request waited, or that the operator closed meanwhile, is given back and the request
  chooses again with its place in line kept.
- **The media door.** It forces nothing on a live card table. On a table it could not refresh (F24) it still keeps the screen's card closed
  (`AllocInput.DisplayStale`; the cap stays what the config says and the skip says the card table could not be re-read, not that the key is 0),
  because the cap is judged on the VRAM in use now and a stale table misstates that; the new `mediaAllocInput` is pinned both ways. A media call declares
  no VRAM footprint, so under a cap below 100 the typed media tools still do not take the display card: that waits for a measured VRAM footprint
  to be fed to the allocator (register P13, item 2).
- **The refusal reads right.** The display card refused over its cap is skipped as `vram`, as the opened card's floor always was, so a media
  call's `gpu busy` answer would have read `card 1 vram`. `gpulease.Skip` gains `Display` (the card table's rule) and
  `gpualloc.SkipSummary` says `card 1 vram (display card)` beside any reason but `display`
  (`TestSkipSummaryNamesTheDisplayCardBesideAReasonThatDoesNot`); the full sentence, with the cap, stays in the skip's detail.
- **What you will see.** A display card with no resident seat sorts first (nothing to evict), so with the pair seat loaded `gpu reserve --cards 1
  --vram 6` now prefers the screen's card to an eviction; `gpu_display_cap_pct: 0` restores the old order. The cap bounds admission only:
  nothing limits a job after it is placed, so the footprint has to be a measured peak, and one number per request cannot describe a donor slice
  beside a compute share.
- **Tests, written first.** Red before (the field was inert, so the allocator still read the old rule): `TestAllocatorOpensTheDisplayCardWithinTheCapWithoutPresence`
  (got `[gpu-aaaa0000]`, the display card skipped as `display`), `TestAllocatorRefusesTheDisplayCardPastTheCapAndNamesIt` (footprints 6, 6.9 and 6.94
  not taken, and every refusal skipped as `display` instead of `vram` naming the cap), `TestAllocatorUndeclaredFootprintStaysRefusedOnTheDisplayCard`,
  `TestAllocatorCapZeroKeepsTheDisplayCardClosed` (the skip did not name the key), `TestAllocatorDisplayCardWithoutATotalStaysClosedUnderTheCap`,
  `TestADeclaredFloorHoldsAboveTheCapWhenItIsStricter`, `TestAMultiCardRequestTakesTheDisplayCardOnlyWhereItClearsTheCap` (2 allocatable of 3 wanted),
  `TestAHeldDisplayCardThatCouldNotClearTheCapIsNotQueuedOn` (footprint 6 not waitable), `TestDesktopRefusalsAppliesTheCapAtGrantTime` (refused within
  the cap), and `TestGPUDisplayCapPctDefaultsTo60AndRejectsOutOfRange` (`got 0`, want 60). Three guards hold before and after
  (`TestAllocatorDisplayUnknownStaysClosedUnderTheCap`, `TestTheCapTouchesOnlyTheDisplayCard`, `TestACapWithNoDisplayCardInTheReadingChangesNothing`).
  The `gpualloc`, `pipeline` and root tests could not fail on the old tree except by not compiling (the field and the dependency they use
  did not exist); each is pinned by the mutants below instead.
- **Tests that encoded the superseded rule** (the display card is open only while the operator is away): the plan counted two, there are
  more, and each is named in the commit. Rewritten on the cap: `TestTheFloorDoesNotOpenTheDisplayCardAtTheDesk`, `TestAllocatorDisplayCardIsAllocatableOnlyWhenTheCallerSaysSo`,
  `TestDesktopRefusalsNamesTheDisplayCardOnceTheOperatorIsBack`, `TestBuildInputOperatorAwayAllowsTheDisplayCard`, `TestPickAutoNeverTakesTheAttachedCardAtTheDesk`,
  `TestTheDisplayCardIsAllocatableOnlyWhenTheOperatorIsAway`, `TestAQueuedMediaCallIsNotGrantedTheDisplayCardOnceTheOperatorIsBack` and the six
  `gpu_reserve_grantcheck_test.go` tests; removed because an unknown display is now always closed: `TestAllocatorAllowsAnUnknownDisplayCardWhenTheOperatorIsAway`,
  `TestAnUnknownDisplayCardIsHeldToTheDesktopFloor`, `TestAnUnknownDisplayReadingRefusesTheCardThatWouldLeaveTheDesktopTooLittle`.
- **Mutation-checked** with `scripts/mutation-check.sh` (each mutant compiles, runs through the RAM guard, and is restored byte for byte): seventeen
  changes, each RED on the test it names. In the allocator: the cap term dropped from the floor, the used/free sign flipped, the footprint
  requirement dropped, a cap of 0 treated as open, an unknown display let through, the declared floor ignored, the grant-time check ignoring the
  cap, an unknown total let through, and the float tolerance removed (it survived the first table, which landed on the cap in a way floating
  point happens to round up; `TestAJobThatLandsExactlyOnTheCapIsAdmittedDespiteFloatError` uses 3.9 + 2.1 of 10, where 6.1 - 2.1 is
  3.9999999999999996, and kills it). In `BuildInput`: the cap not carried, the declared floor not carried. In the config key: the default
  closed, the range check dropped. At the media door: the stale-table closure removed, the card forced shut on a live table. In the summary: the display-card cue dropped,
  the skip not carrying `Display`. The root test
  (`gpu_reserve_displaycap_test.go`, and the rewritten grant-check tests) runs only on the remote gate and was not mutated.
- **Review fix round** (five findings, each fixed or refuted in its commit). Red before: a refused `600` read as a cap of 100
  (`TestARefusedGPUDisplayCapReadsAsClosedForACallerThatProceedsOnTheValue`, got 100, want 0, and its row in
  `TestBuildInputOpensTheDisplayCardAtTheCapWhateverThePresence`); the stale-table skip said `gpu_display_cap_pct is 0` to an operator whose config
  says 60 (`TestAPlacementFromAStaleTableKeepsTheDisplayCardClosed`; the allocator pin is `TestAStaleTableClosesTheDisplayCardAndTheSkipNamesTheTable`);
  the switch written after start was not seen (`TestTheDisplayCapSwitchFollowsTheFileTheConfigWasReadFrom`, `TestBuildInputFollowsTheDisplayCapSwitchInTheConfigFile`,
  `TestTheMediaDoorSeesTheOperatorsSwitchWrittenAfterTheServerStarted`; the six grant-check tests in `gpu_reserve_grantcheck_test.go` now build their input through
  `gpualloc.BuildInput` from a config file, and the three that close the card write `0` to that file, instead of flipping an in-memory counter no
  production path can touch). Ten more mutants, each RED: the refused value clamped back to 100; the stale flag unset, its skip
  detail the cap's, and the flag ignored by `displayOpen`; the cap-100 exception removed from `floorApplies`; `BuildInput` reading the snapshot
  (red on the `gpualloc` and the `pipeline` test); `LiveGPUDisplayCap` ignoring the source, not remembering the last value read, and `Load` not
  recording the file. Four comments that still described the presence rule now describe the cap.
- Docs: ADR 0039 (Amendment 2026-10-12), `docs/systems/media-generation.md` ("The allocator and the display card"), the `--cards` help text.

### Added — one lint over every test file keeps tests out of the OS temp dir, and a tripwire script shows what a run leaves behind (GPU-I8, GPU-I8c)

- **The gap.** F77 and F77b stopped test packages from leaving folders in the OS temp dir (C: on Windows), and each fix came with a lint
  that read ONE package, so the next package to do the same was covered by nothing. No check saw a temp file that production code makes
  for a test (F79's inline graph JSON), or the folder a started copy of a test binary leaves because it exits before its cleanup.
- **The lint.** `tempdir_lint_test.go` (root package) reads every `*_test.go` file of the module with `go/parser` (not a regular
  expression on text; `testdata`, `node_modules` and any directory with a `go.mod` of its own are left out, which is how the `tools/` modules go: a directory merely called `tools` or `bin` is scanned like any other) and fails on a
  direct use of the OS temp dir that `tempdir_lint_allowlist_test.go` does not list with its count and the reason its folder is gone on
  every path: `os.MkdirTemp` / `os.CreateTemp` / `ioutil.TempDir` / `ioutil.TempFile` with an empty directory, any `os.TempDir`, `TMP`,
  `TEMP` or `TMPDIR` read from the environment, and `os.MkdirTemp` / `os.CreateTemp` passed as a value (the `runIsolated(m, os.MkdirTemp, ...)`
  shape, where the empty argument is supplied somewhere the lint cannot read). `t.TempDir()` and a named directory are clean. A new use
  fails naming file and form; a use that went away fails until the count comes down. The second rule is for copies: a package whose
  tests start their own test binary (`os.Args[0]`, `os.Executable`) holds `TestARelaunchedTestBinaryLeavesNothingInTheTempDir` (checked on
  its body: it starts the binary, points TMP, TEMP and TMPDIR, and reads the folder back), or is listed in `relaunchExempt` with the
  reason a started copy leaves nothing, or in `relaunchNeverStarted` when the tests only read the path (the lint checks that no
  `exec.Command`, `exec.CommandContext` or `os.StartProcess` takes it). An exemption is void when the package's `TestMain`, `init`
  functions or package-level variables themselves use the OS temp dir, because every started copy runs them.
- **The lists, generated at the 0.182.0 tip** with `TEMPDIR_LINT_PRINT=1`: 9 uses in 9 files (4 direct calls and 5 `os.MkdirTemp` handed to
  `runIsolated`; `git grep` of the same forms finds the same 9), 5 packages that hold the proof (root, `fleetnode`, `mediaremote`,
  `pairworkloads`, `pipeline`), 10 exempt packages and 2 that only read the path. The two F77 per-package lints (`TestTestsReachForTheOSTempDirOnlyWhereItsRemovalIsOnEveryPath`
  in `internal/fleetnode` and `internal/pipeline`) are deleted; the relaunch test each file also held moved, unchanged apart from a
  `nosemgrep` note, to `relaunch_leak_test.go` like the three sibling packages. `internal/exemplars` is the one entry that could be
  `t.TempDir()` (its helper removes the folder with `t.Cleanup`, so it is listed with that reason).
- **The tripwire.** `scripts/test-temp-tripwire.sh` (PowerShell twin `scripts/test-temp-tripwire.ps1`) sets `TMP`, `TEMP`, `TMPDIR` and
  `GOTMPDIR` to a fresh empty folder, runs `go test -count=1` over `--pkgs` (default `./...`), and ends with one `TRIPWIRE-VERDICT`
  line: `CLEAN` (0), `LEAK` (1, what is left by name prefix with a count, such as `lo-x-state-*`), `PROCESSES` (2, an image running
  from the folder, with its pid; wins over `LEAK`), `TESTS-FAILED` (3) or `REFUSED` (4). `--dir` is required and a folder on the system drive is
  refused unless `--allow-system-drive` is given. A `LEAK` or `PROCESSES` run keeps the folder as evidence and never removes one a process runs
  from; a `CLEAN` run removes its empty folder. This is the check for what a static lint cannot see: production code's temp files and
  started copies of the test binary.
- **Tests, red first.** The scanner was a stub when `TestTempLintSeesEveryFormOfRawTempUse` and `TestTempLintSeesAProofOnlyWhenItReallyRelaunches`
  were first run: every form case failed (`forms = map[], want map[os.MkdirTemp(empty):1]`). With the real scanner and empty lists,
  `TestTestsReachForTheOSTempDirOnlyWhereItsRemovalIsOnEveryPath` named the 9 files and `TestEveryRelaunchingPackageProvesItLeavesNothing`
  the 12 packages. Synthetic trees prove that a leak in any package is named by file and form and that `testdata`, `node_modules`
  and nested modules (the `tools/` ones among them) are skipped while a directory that is only called `tools` or `bin` is not. The tripwire has self-tests (`scripts/test-temp-tripwire-selftest.sh` and `.ps1`, on the fixture module
  `scripts/testdata/tripwirefixture`: a clean package, a leaker, a spawner that leaves a process running from the folder, a failing
  package) that assert verdict, exit code and the disk afterwards, and that the two twins agree on verdict, exit code and leak lines.
- **Mutation-checked** with `scripts/mutation-check.sh`, each mutant RED: the lint, 16 (a dropped form, the value-pass rule, the allowlist
  lookup, the fewer-than-listed check, the hook check, relaunch detection, each of the three variables the proof must point, the read-back,
  the start check, nested-module and `testdata` skipping); the tripwire, 12 on the PowerShell twin and 16 on the shell twin (the refusal, each
  verdict, the evidence kept, `--remove`, the prefix rules, the ignore list; five of the shell ones are Linux-only rules, run on a Linux host
  where the self-test also found a real `PROCESSES` verdict). One PowerShell mutant first SURVIVED (a removal while a process ran is invisible on
  Windows, which will not delete a running image); the fixture now leaves a note file beside the image and the self-tests require it to survive.
- **What the first dry run of the whole suite found, and fixed** (test code only). The tripwire's run of `./...` on the 0.182.0 tree, with the temp
  dir on a data disk, left three kinds of entry. (1) 18 `tmp.XXXXXXXXXX` files of go test output: the sandbox that runs `ci.yml`'s step
  scripts passed the host's temp dir through and the Windows cage step starts with `log=$(mktemp)`; a step now gets `TMPDIR`, `TMP` and
  `TEMP` inside the sandbox (`TestCIScriptSandboxKeepsAStepsTempFilesInsideItself`, red first and mutation-checked). (2) One `TestRunImageBatchWaitingEndsOnInterruptAndLeavesNoLease*`
  tree: the test abandons a wait on purpose, and the wait gave the card back after the test's temp dir was gone, recreating `gpu/lease/...` under
  it (39 of 40 runs, and the same folder on the pre-0.182 base on Windows); the test now waits for the abandoned wait to leave the line,
  and with that wait removed 6 of 10 runs leave the tree again (mutation-checked with the tripwire as the test). (3) One `seat_fg.stale-mp.tests.lock.d`:
  the seat launcher shell test keeps a `flock` target in the temp dir so that two runs on one machine take turns on the scratch ports they
  share. It was first named in `scripts/test-temp-tripwire.ignore` (one `NAME-PREFIX`, a tab and a reason per line, which the tripwire
  prints and does not count, so an ignore is never silent); the review round below moves the lock out of `TMPDIR` and the list is empty.
- **Found on the way.** `TestEveryTrackedGoModuleIsScannedOrNamedAsExcluded` fails on the 0.182.0 candidate (shown by a remote run at the lint's first commit):
  `scripts/testdata/mutfixture` holds a tracked `go.mod` that the govulncheck job does not scan and `govulncheckExcluded` did not name. Both fixture
  modules (that one and the tripwire's) are named now, with the reason they ship nothing (the 0.182.0 release had already named
  `mutfixture`; the tripwire's fixture is the new one).
- **Measured** with the PowerShell twin on Windows, `go test -p 1` over the root package, `fleetnode`, `pipeline`, `pairworkloads`, `mediaremote`, `mcpserver`
  and `delegate`, the temp dir on an empty folder of a data drive. Before the leak fixes (0.181.0): 79 entries in 8 prefixes — `run-graph-*.json` 28 and
  `run-graph-manifest-*.json` 1 (F79), `lo-root-state-*` 20, `fleetnode-compose-cache-*` 13 and `lo-pipeline-lease-*` 13 (F77), `lo-pairworkloads-state-*` 2 and
  `lo-mediaremote-state-*` 1 (F77b), and the interrupted-batch tree 1. After this branch, the same packages on the same machine: nothing left, verdict `CLEAN` (1,571 s). A first run of the set ended `TESTS-FAILED` with the
  folder empty because `internal/mcpserver` failed once (54 s in); it passed in the four runs that followed (alone through the gate, twice through
  the tripwire, and in the repeat of the whole set), and which test failed was not captured, so it is open, not a temp leak. On Linux, `go test -p 1 ./...`
  over the whole module, niced, with the temp dir on a separate data disk: at the first dry run every package passed (1,844 s) and the verdict was
  `CLEAN` with one ignored entry printed with its reason; after the review round, at the branch's last code commit (`-p 1 -timeout 45m ./...`),
  every package passed (1,921 s) and the verdict is `CLEAN` with nothing ignored. On Windows a run with the same flags over `internal/pipeline` and
  `internal/vllmseat` ended `CLEAN` (720 s); a run without `-timeout 45m` had ended `LEAK` when `internal/pipeline` hit go test's
  ten-minute default and was killed mid-test, and its failure digest named the timeout and the test that was running. The Windows PowerShell 5.1 self-test (98 checks at the branch tip) and a tripwire run over twelve packages that start
  copies of their test binary or keep a state root both ended clean on a second Windows machine.

### Fixed — the temp-dir tripwire keeps the failure it saw, stops what it started when it is interrupted and says when it could not look; the lint stops skipping a directory by its name (GPU-I8, GPU-I8c, review round)

A review of the lint and the tripwire above made 11 findings (10 distinct claims). Each was read against the code: six were defects in the scripts, the lint and a
shell test, three were documentation, and one holds only once another branch is merged. Every fix below was seen red first on the unfixed script or test.

- **A failure far from the end of the output was lost.** The shell tripwire showed the last 20 lines of `go test` and deleted the
  capture, and `go test -p 1 ./...` prints one `ok` line per package after the failing one, so the failing test of a whole-module run
  was in neither (the first dry run above ended `TESTS-FAILED` and which test failed was not recorded). When the output is longer than
  the tail, both twins now print the `--- FAIL`, `panic:` and `FAIL pkg` lines, each with the indented lines under it (at most 40),
  ahead of the tail. Red: `--tail 1` over a failing package followed by a passing one showed `FAIL` and nothing else.
- **An interrupt stopped go and left the test binaries.** The shell tripwire signalled one pid; on Linux a `TERM` to the script left
  `hang.test` running from the kept folder and the capture file beside it, and no process check ran. It now runs `go test` under job
  control and stops the process group (`INT`, then `TERM`, then `KILL`, 10 s, 5 s and 5 s at most), names what still runs from the folder,
  keeps the folder, removes its capture and exits 130; a fixture test that ignores `INT` and `TERM` needs the `KILL` step. The
  PowerShell twin sat in one `WaitForExit()` that a stopped pipeline cannot interrupt: stopping it took the whole 40 s of the fixture
  test (39 s measured). It waits in half-second slices now, ends the process tree with `taskkill /T /F` and removes its captures.
- **Checks that failed open now say so.** A failed process query read as "no process" (PowerShell), a `stat` that fails or a missing
  `SystemDrive` read as "not the system drive", and a temp folder that something removed during the run read as an empty, `CLEAN`
  listing. The verdict line now carries `process check skipped`, the run is `REFUSED` when the host cannot place the folder, and a
  vanished folder exits 70 without a verdict. A folder that holds entries is not removed, not even with `--remove`, when the process
  check could not run, because a process might still run from it.
- **The lint skipped any directory named `tools` or `bin`, at any depth.** A main-module package such as `internal/tools` would have
  gone unlinted with the lint green. The real `tools/` modules each hold a `go.mod`, which the go.mod rule already skips, and no
  main-module test file lives under a directory of either name; only a `go.mod`, `testdata`, `node_modules` and `.git` leave a directory
  out now. Red: the synthetic tree's `a/tools`, `a/bin` and `bin` leaks were not found.
- **The seat launcher shell test's lock was one lock per `TMPDIR`.** `seat_fg.stale-mp.tests.sh` took its "one run at a time per box"
  lock under `${TMPDIR:-/tmp}`, so runs with a temp dir of their own (a gate run beside another agent's, a run under the tripwire) took
  different locks and could share the scratch ports. The folder is `/tmp/seat_fg.stale-mp.tests.lock.d` now (`SEAT_FG_TEST_LOCK_ROOT`
  moves it), a Linux test holds the lock at both paths and requires the SKIP line to name the one that is not `TMPDIR`, and the
  tripwire's ignore entry for it, whose reason had been false under the tripwire, is gone: `scripts/test-temp-tripwire.ignore` is empty,
  and the dry run above now ends `CLEAN` with nothing ignored. One empty `1777` folder stays in `/tmp` on a box that runs the test, as it
  did before for a run without `TMPDIR`.
- **Documentation.** The tripwire's platform notes sent an operator to the Go lint's allowlist for a leaver the tripwire reports; they
  name `scripts/test-temp-tripwire.ignore`. The headers, `CONTRIBUTING.md` and the entry above now say what the tripwire cannot see: only a
  child that inherits `TMP`, `TEMP` and `TMPDIR` writes into the watched folder, so a child with a scrubbed environment (the browse
  sidecar's) or a test that names a fixed directory (`/dev/shm`) writes elsewhere and a `CLEAN` says nothing about it. The count above
  read "12 exempt packages"; it is 10 exempt and 2 that only read the path (12 without a proof).
- **Holds only after a merge.** Two packages that other changes add start their own test binary: `internal/proctree`
  (`feat/0183-lease-guards`) and `internal/winexec` (the no-visible-windows change). Merged with this branch,
  `TestEveryRelaunchingPackageProvesItLeavesNothing` fails until `relaunchExempt` names each; this branch cannot name them first, because
  the lint also fails an exemption for a package that does not exist. The merged trees with those entries added pass the lint.
- **The relaunch rule did not see a start through `winexec`.** The no-visible-windows change starts the test's own binary with
  `winexec.Command`, the `exec.Command` whose child opens no window on Windows, and the rule only knew `os/exec`: merged with that
  change the proof tests of `fleetnode`, `mediaremote`, `pairworkloads` and `pipeline` read as stubs and the lint failed for all four.
  `winexec.Command`, `CommandContext`, `CommandDetached` and `CommandIfNoConsole` count as starts now (matched by the tail of the import
  path, so the lint names no module). Red: five synthetic cases, each a real proof started through one of them.
- **Tests.** The shell self-test runs 87 checks on Linux (one skipped: the PowerShell twin is not on that host) and the PowerShell
  self-test 98, under PowerShell 7 and under Windows PowerShell 5.1; they take `TRIPWIRE_SELFTEST_FAILFAST=1` and (PowerShell)
  `TRIPWIRE_SELFTEST_ONLY=tag,...` so that a mutation check runs seconds, not the suite. The interrupt cases stop `./hanger`, a test that
  ignores `INT` and `TERM` and starts an unwaited copy of itself: go test ends its own test binary when it is interrupted, so only a
  stop that reaches the whole process group and ends with a kill leaves nothing running. Mutation-checked with
  `scripts/mutation-check.sh` and `.ps1`, each mutant RED: 14 on the shell twin (a digest never shown, a continuation line dropped, a
  failing `stat` read as "not the system drive", an undecided guard not refusing, a signal to the leader only, no `KILL` step, a wrong
  exit code, `TERM` not trapped, no job control, the removal gate ignoring a skipped check, the skip note dropped, a vanished folder
  ignored, the capture kept, an empty folder kept), 13 on the PowerShell twin (the same rules, plus a wait that is not sliced, a
  single-process kill and no stop in `finally`), 5 on the lint (a dropped `winexec` branch, `winexec.CommandContext` known to
  `os/exec` only, a `winexec` match loosened to any import path ending in the word, `tools` skipped again, `bin` skipped again) and 1 on
  the lock (keyed to `TMPDIR` again). The leader-only signal first SURVIVED twice, and that was a gap in the tests, not a rule that did
  not matter: the interrupt cases counted any process under the scratch folder instead of waiting for the fixture's two, and with one test
  binary go test's own handling of an interrupt hid the difference; both are fixed above.

### Fixed — the media-job door answers 413 at once for a body that declares itself over the cap, instead of holding the node's only upload slot while it drains (CT-53)

- **The defect.** `POST /fleet/media-job` took the node's one in-flight slot first and, for a `Content-Length` over
  `fleet_media_inputs_max_mb` (in base64, plus 64 KiB), then read the body up to the cap before it answered `413`. A fleet-token
  holder that declared 8 GiB and sent 20 bytes got no answer for the whole 15-minute window, and held the slot meanwhile, so the
  next upload queued behind it and was answered `503` after the slot wait (seen live on a media-job door, 2026-10-08).
- **The fix.** The size check sits right after the door (`403`) and bearer (`401`) checks, before the slot: a declared length over
  the cap is answered `413 request body too large` with `Connection: close` and the body is never read. The check is strictly
  over the cap (a body of exactly the cap is still read), the wording is unchanged, and a body with no declared length (chunked)
  still meets the reader's cap as before (`413` once it passes it). `readMediaJobBody` no longer drains a declared-oversize body either
  (it returned no error and no bytes when such a body ended early); it returns the typed error the handler maps to `413`.
- **What it does not cover (stated, not fixed).** Only a body that DECLARES itself over the cap is answered at once. A body that
  declares an in-cap length (or none, chunked) and then stalls still holds the slot until its bytes arrive or the 15-minute window
  ends: that is the door's existing design, and an idle-body deadline would be new behaviour with its own risk to slow links, so it is
  left as a follow-up. More bytes than were declared are not answered `413` either: net/http hands the handler exactly the declared
  number of bytes and reads the excess as the client's next request, so the declared bytes are judged as the body (a `400` unless
  they are a media-job body). The CT-50 entry below and the `readMediaJobBody` comment said otherwise; the comment is corrected, the
  over-read probe stays for a caller that builds the request itself, and `media_job_framing_test.go` pins what happens on the wire.
- **The stt upload door had the same defect and gets the same fix.** `POST /fleet/stt` took one of its two upload slots before it
  read the body, so a token holder that declared an oversize `Content-Length` and sent 20 bytes held a slot for the 10-minute window,
  and two of them shut every upload out. A declared length over `fleet_stt_upload_max_mb` (in base64, plus slack) is now refused at
  once, after the bearer check and before a slot, the body unread, `Connection: close`. The door's answer for a body over its cap is
  `400` (it always was, and its client reads every refusal alike), so the status and the wording are unchanged and only the time
  they are given in is. Tests (`stt_upload_declared_oversize_test.go`, the media-job door's rules on this door): a raw TCP client
  declaring 8 GiB and sending 20 bytes gets the `400` inside 2 s, also while both slots are held and without taking either; a
  client that streams the body is not read (under 64 KiB); the connection is closed; a request without the bearer is still `401`
  and a body of exactly the cap is still read. Before the fix the first client got no answer in 2 s and 64 MiB of the second
  client's body had been read.
- **Tests** (`media_job_declared_oversize_test.go`): a raw TCP client declaring 8 GiB and sending 20 bytes gets `HTTP/1.1 413`
  inside 2 s, also while another upload holds the slot; a client that streams the body is not read (the server has read under
  64 KiB; before the fix it had read 128 MiB of a refused body); the connection is closed after the answer; a body of exactly the
  cap is read and one byte over is `413`; the body of a declared-oversize request is never read at the handler, and `403` and
  `401` still come before `413`. Documented in `docs/systems/fleet-node.md` (the media-job and stt upload doors).

### Fixed — the seat-rate store's lock waits out a Windows delete that is still pending instead of losing the run's sample (D-120)

- **The defect.** `seatrate.Update` takes an exclusive lock file beside the store. On Windows, removing a file whose handle is
  still open only marks it delete-pending, and an exclusive create in that window fails with `ACCESS_DENIED`, not "already
  exists". `Update` returned that raw, so the run that finished while another run's lock was being deleted lost its rate and
  cold-load sample (the caller logs `seat-rates store not updated` and goes on), and `TestUpdateSerialisesConcurrentWriters`
  failed one gate run in five (2026-09-17). Measured on a Windows PC under 4 concurrent copies of the old test: 1 of 60 runs
  failed with `open ...seat-rates.json.lock: Access is denied.`
- **The fix.** The denial waits like a held lock, inside the existing 2 s lock window (the rule `gpulease`'s claim and epoch lock
  already follow). At the deadline a held lock is still plain `ErrLocked`; a denial that never clears (a directory this process
  cannot write) returns an error that wraps both `ErrLocked` and the open error, so the cause is named after at most 2 s. Any
  other failure to open the lock is returned at once, as before.
- **A stale lock that cannot be removed ends at the deadline.** The takeover of a lock older than 30 s retried at once whether or
  not the removal worked, so a stale lock another process held open spun the caller with no deadline. It now retries at once only
  after a removal that worked (or found the file gone) and otherwise waits out the window.
- **The rename that publishes the save had the same race (review follow-up).** The store is read without the lock (`seatrate.Load`
  from the delegate, the fleet node and the pipeline), Go opens files on Windows without delete sharing, and a rename over a file a
  reader has open fails with `Access is denied`; `Update` handed that error to its caller, which drops the sample exactly as the
  lock-create denial did. Measured on a Windows PC with the retry removed: an `Update` while a reader held the store open failed at
  once with `rename ...: Access is denied.`. The rename now asks again for up to 2 s, inside `Update`'s lock, when it is refused
  with a permission error or (Windows) a sharing or lock violation, the two codes Go does not map to a permission error; any other
  rename error, and a denial that outlasts the window, is returned as the rename error itself (not as `ErrLocked`), with the
  temp file removed and the previous store untouched. 60 runs (4 workers x 15) of the `Update` tests passed on a Windows PC.
- **A lock that cannot be released is logged.** `defer os.Remove(lock)` discarded its error, so a lock a scanner held open stayed
  for 30 s and every writer in that time skipped its sample with only `store locked by another writer` to show for it. The failed
  removal is now logged once, naming the lock and the cause; `Update`'s result is unchanged, since its save may have landed.
- **The `ErrLocked` comment and the test helper.** The comment now says what the code returns (a lock not taken within the window,
  held or refused, the refusal wrapped beside it). The helper that asks again after `ErrLocked` retried a refusal that never clears
  (an unwritable directory) for its five minutes; it now reports a refusal after three lock windows in a row and still waits out
  contention (one window that ends in a denial among contention windows is not a verdict).
- **Tests** (`update_lock_test.go`, the open and the stale removal through package variables so the denial can be injected): a
  denial three times then success stores the sample once with no lock left; a persistent denial returns within the window with
  both causes and nothing left behind; a live lock is still `ErrLocked` and is left in place; any other open error is returned at
  once; an unremovable stale lock ends at the deadline with a bounded number of attempts. The concurrent-writers test asks again
  after the designed `ErrLocked`, so it measures serialisation and not how long twelve writers take on a loaded host. The rename
  and the release have their own (`save_rename_test.go`, `save_rename_windows_test.go`, `update_helper_test.go`): a rename denied
  three times then granted stores the sample once with nothing left behind; the corrupt-store replacement is retried too; a
  persistent denial ends after the window, names the rename and is not `ErrLocked`; any other rename error is returned at once; a
  reader holding the real store open for 300 ms no longer loses the sample; sharing and lock violations are retried; a failed
  release is logged once and a clean one says nothing; the helper reports a refusal after three windows and keeps waiting out
  contention.

## [0.182.1] - 2026-10-12 - the card-only qwen-image-2.1 bf16 image recipe is held back, and a node that passes a job back is paused with its own reason

### Fixed — the card-only qwen-image-2.1 bf16 image recipe is held back: ComfyUI builds its 16.33 GiB text encoder whole on the storage card, and no 16 GB pair can place it

- **The defect.** The 0.182.0 live acceptance ran a card-only `generate-image` on the `qwen-image-2.1/bf16` row of `render/multicard-recipes.json` (basis `predicted`, family `qwen-image-2.1`) and ComfyUI ran out of memory 1.4 s after the prompt began, before the sampler. Its console shows `read-only map of qwen3vl_8b_bf16.safetensors: 750 tensors, 16.33 GiB`, then the MultiGPU patch returning `cuda:0` as the text-encoder device, then `torch.OutOfMemoryError: Allocation on device 0 would exceed allowed memory` with 14.10 GiB allocated, 1.16 GiB requested and a 15.90 GiB device limit, raised while the encoder's output layer was being built. The 2511 edit passed the same acceptance.
- **The cause.** Under `--gpu-only` ComfyUI takes the text encoder's initial device from `text_encoder_initial_device`, which returns the offload device (`cuda:0`, the storage card), so the whole 16.33 GiB bf16 encoder is built on a 15.9 GiB card before DisTorch2's allocation string (`cuda:1,5gb;cuda:0,*`) is attached, after the loader returns. No resident share or peak figure in the row can change that, so the row was unplaceable on any 16 GB pair. The 2511 edit rows build an 8.74 GiB fp8 encoder whole on `cuda:0` and fit, which is why the edit ran. The row's evidence line called 13.3 GiB the encoder's share; that is the size of the bf16 DiT file (13.25 GiB), not of the encoder.
- **The fix.** The row is held back, exactly as `qwen-image-2.1/int8_convrot` already was: a `blocked` sentence in `render/multicard-recipes.json`, byte-identical to `Blocked` in `internal/mediacap/multicard.go` (`TestTheMulticardRowsAreTheRunnersRecipes` pins the two). The planner (`findRecipe`, so every builder and runner) and the typed door (`multicardNeed`, through `FindMulticardRow` and `BlockedMulticardRow`) now refuse the binding by name, with the reason, before any lease is taken; the host-RAM estimate and the doctor see a binding without a recipe. The row's data stays for the proof arm, and its evidence line carries the correction. The int8_convrot sentence no longer ends "or bind the bf16 DiT, which has a recipe" (it says the bf16 row is held back too and that the int8 row names the same encoder); the `qwen-image-2.1` family hint, the header comment of `render/multicard-recipes.mjs`, `render/README.md` and `docs/systems/media-generation.md` (the recipe table, "A row can be held back", "No recipe, no lease", "Not measured") say the same. The load-time warning of `imagegen_placement: multicard` now treats `qwen-image-2.1` as a family with no launchable recipe (it lists the families whose rows are not held back), where it used to stay silent for it.
- **Tests.** Red against 0.182.0 before the fix: `findRecipe` returned the bf16 recipe for both 2.1 bf16 DiT files; `planImageMulticard` on two 16 GB cards under the lease returned a plan; `FindMulticardRow` returned the row and the held-back count was 1; and a typed `generate-image` bound to the bf16 DiT with `imagegen_placement: multicard` leased the pair and started the runner. After: `render/multicard-recipes.test.mjs` (the held-back refusal for both file names, the blocked-id list, the row's kept data), `render/multicard-image.test.mjs` (the planner refuses before the card table is read), `render/multicard-builders.test.mjs` (the 2.1 builder refuses), `internal/mediacap` (`TestAHeldBackRowIsNotARecipeAndSaysWhy` gains the bf16 case, the held-back count is 2), `internal/pipeline` (`TestAMulticardBindingWithNoRecipeIsRefusedBeforeAnyLease`: nothing leased or started, the reason names the row, "held back", the text encoder, 16.33 GiB and 15.9 GiB, and offers only the launchable row), `internal/hostneed` and `internal/config`. Every test that used the bf16 row as the launchable image fixture now uses the 2512 Q5_1 GGUF row or the edit rows, keeping what it asserts, and a test whose subject is the bf16 row's own loaders or figures builds them from the row's data and asserts the hold.
- **The limit.** After this the only launchable card-only IMAGE row is `qwen-image/gguf-q5_1`, still predicted. The edit rows are unchanged (`qwen-image-edit-2511/fp8mixed` measured, `qwen-image-edit-2511/gguf-q5_1` predicted). A loader that builds the text encoder per card, or one measured arm that fits the pair, lifts the bf16 hold (clear `blocked` and `Blocked`).
- **Review round: the refusal sentences.** `findRecipe`'s generic refusal for a family with no row at all (what `comfy-generate.mjs`, `comfy-render.mjs` and `comfy-edit.mjs` hand it for any `--family`: each plans the placement in `main()`, and the family checks of `comfy-render.mjs` (`KNOWN_FAMILIES`) and `comfy-edit.mjs` (`EDIT_FAMILIES`) sit in the graph builders that run after it) was a hand-written list that still said the card-only placement "has recipes for qwen-image-2.1". It is now read from the table, as the typed door's refusal already was: the route's launchable row ids, then its held-back ones ("`the card-only image placement has recipes for qwen-image/gguf-q5_1 only; qwen-image-2.1/int8_convrot, qwen-image-2.1/bf16 are held back`"), so it cannot go stale when a hold is lifted. The `qwen-image-2.1` hint is the image route's; on the edit route, where that family has no row and its edit graph takes no placement, the refusal says so and then what the edit route does offer, instead of saying image rows are held back. Red before: the generic sentence named no launchable row id and named `qwen-image-2.1` as having recipes; the edit refusal said "held back". Not changed: a held-back row is still reported before its text encoder is compared (`render/multicard-recipes.test.mjs`: the hold is the answer, whichever encoder the call names); the typed door names a hold only when the whole binding matches the row.
- **Review round: coverage the hold took away.** With both 2.1 rows held back, the 2.1 builder's multicard branch and the `--placement multicard` + pool-flag refusal of `comfy-render.mjs` cannot be reached through the shipped table. `render/multicard-builders.test.mjs` now runs the builder branch against a copy of the builder and recipe module over a copy of the table with the holds removed (only the three loaders change, link for link; the DisTorch2 allocation, `eject_models: false`, the plain int8 pin), and judges the pool-flag combination per 2.1 row in the state the table has it (the hold while held back, `no pooled loader wired` once launchable). Mutations: skipping the branch, `eject_models: true`, and (with both holds lifted) removing the pool refusal each turn the matching test red.

### Fixed — a node that passes a job back is paused with its own reason, not "another job held its card"

- **The defect.** When a node took a job and passed it back, the placer paused it with a fixed cause: "it took the last call and passed it back: another job held its card". A host short of RAM refuses with the same class as a held card (`gpu_busy`), so its row said a card was held when no lease existed. On the 0.182.0 live acceptance this sent the diagnosis of a fleet node's render failures looking for a lease; the real cause was the host's committed memory. A job adopted from a process that ended was paused the same way for every class, so a lease the node could not take read "bounced" instead of "refused".
- **The fix.** One helper, `passBack` (`internal/mediaremote/place.go`), gives the pause on both paths (dispatch and adopted job): the cause carries the node's own reason (on one line, its address redacted, capped at 600 bytes, as other node text), `gpu_lease_unavailable` reads `refused`, and the adopted path names the node as the dispatch path does (address and node id) instead of by its bare id.
- **Tests.** `TestABouncedRowSaysWhyTheNodePassedTheCallBack` (new) and `TestAnAdoptedJobTheNodePassedBackGoesBackInLine` (extended, all three classes, and the name) failed before the fix and pass after; `TestAPassedBackReasonIsQuotedOnOneLineAndCapped` (new) and `TestALeaseThatCannotBeTakenMovesTheCallOnAndPausesTheNode` (extended: the pause quotes the node) pin the rest. The whole `internal/mediaremote` package passes.

## [0.182.0] - 2026-10-12 - a lease that unloads the seat is no longer held by the RAM that unload frees, Wan and image renders run card-only on two cards, agent_run reaches a named seat anywhere in the fleet, and nothing the harness starts opens a window

### Fixed — a human waiting at the CLI is told the ticket is queued even when its first scan ran before the wait began

- **The defect.** A ticket's first scan can run before the CLI's wait loop has set its `Notify`. That scan's "queued" line went nowhere, but `say` still recorded its key as said, so the wait loop's own first look at the same standing was dropped as a repeat. The human was then told nothing until the ticket started. It was a 0.181.0 race, seen on a slow CI runner as `TestCLIBlocksAndCancelsUndispatched` "timed out waiting: the human is told it is queued". The test passes 100 times out of 100 alone, at this release and at 0.181.0.
- **The fix.** `say` records a standing as said only when there is a `Notify` to say it to (`internal/mediaremote/scan.go`).
- **Tests.** `TestAStandingSaidBeforeTheNotifyIsSetIsToldOnceItIs` failed before the fix (0 lines told) and passes after. The whole `internal/mediaremote` package passes.

### Security — golang.org/x/net v0.60.0

- The module requires golang.org/x/net v0.60.0 (was v0.58.0), with golang.org/x/text v0.42.0 and golang.org/x/sync v0.23.0 that it brings. A binary scan of the 0.181.0 release (`govulncheck -mode=binary`) found five advisories in x/net v0.58.0 that the code does not call: GO-2026-6603, GO-2026-6610, GO-2026-6611, GO-2026-6612 and GO-2026-6617, all fixed in v0.60.0. After the bump, `govulncheck ./...` reports no vulnerabilities in the module, and none in the tools/llamaswap and tools/comfyui modules.
### Fixed — the seat-stub tests run on a machine whose PowerShell 7 is a Store install

- `internal/vllmseat` runs the rendered seat stubs under every PowerShell on PATH. PowerShell 7 puts its own install folder first on the PATH of what it starts; for a Store (MSIX) install that folder is the package under Program Files\WindowsApps, and a test run under such a PowerShell found pwsh.exe there and could not start it ("Access is denied"), so every scenario under it failed without testing anything. Each PowerShell is now started once with an empty command first; one that cannot be started is left out and the test log names it, while one that starts and then fails is kept, so a broken script still fails.
### Added — the Go toolchain that ships is pinned in go.mod, and CI scans the root module and both printed CLIs for known vulnerabilities on linux and windows (H-26)

- **The gap.** `go.mod` said `go 1.26.7`, which is only the oldest compiler allowed to build the module. The compiler behind every
  shipped binary was whatever each box happened to have, CI floated on setup-go's `1.26.x`, and nothing in the repository scanned for
  known vulnerabilities at all: a Go advisory fixed in a patch release would have shipped unnoticed. Measured on the workstation
  with the vulnerability database of 2026-10-08, the root module built with go1.27.1 calls eleven standard-library advisories on both
  operating systems (GO-2026-6603, 6604, 6605, 6607, 6608, 6609, 6610, 6611, 6612, 6613, 6617), and built with go1.27.2 it calls
  none.
- **The pin.** `go.mod` gains `toolchain go1.27.2`; the `go` line, x/sys v0.48.0, x/text v0.41.0 and go-sdk v1.7.0 stay. With
  GOTOOLCHAIN at its default the go command downloads that release (from the module proxy, checked against the checksum database,
  into the module cache) on the first build in this tree when the installed Go is older, and builds with it; a box on a newer Go
  keeps its own. `GOTOOLCHAIN=local` refuses the download and builds with the installed compiler, and says nothing about the line it
  ignored (measured: a build with go1.27.1 installed and the line present succeeds and reports go1.27.1), so an offline build on an
  older Go needs it. The oldest compiler the module accepts is unchanged. `build` and `installer-windows` keep `go-version:
  '1.26.x'` and switch to the pinned compiler through the same line, which holds for setup-go@v5 (its v5.6.0 source never mentions
  GOTOOLCHAIN or the toolchain directive) and not for v6, which exports `GOTOOLCHAIN=local`. Each of the two jobs then asks the go
  command itself, in a step of its own right after Set up Go (`go env GOVERSION`), and fails unless the answer is exactly the release
  go.mod's toolchain line names: a switch that stopped working (a runner image that exports `GOTOOLCHAIN=local`, a changed action)
  would otherwise leave both jobs green on go1.26.x, judging a compiler that does not ship. The step's log line names both
  releases (`go.mod names go1.27.2; the go command runs go1.27.2`). A bump to v6 must switch to
  `go-version-file: go.mod` and drop `go-version` (which would win), and `TestCISetupGoLeavesTheToolchainSwitchAlone` fails until it
  does and accepts exactly that (`TestSetupGoRuleKnowsTheMigrationFromV5` holds the rule in a table: v6 or v7 reading go.mod passes;
  v6 with a version range, with both inputs, reading another file, or pinned by a commit hash does not).
- **The scan.** A new `govulncheck` job (ubuntu-latest, the standard free runner, no module cache) reads GOTOOLCHAIN from go.mod's
  toolchain line (a go.mod without the line fails the step instead of scanning unpinned), installs `govulncheck@v1.8.0` with it,
  and scans `.`, `tools/comfyui` and `tools/llamaswap` for `GOOS=linux` and `GOOS=windows`: the printed CLIs are separate modules the
  root's `./...` never reaches, and some advisories name one OS. A finding (exit 3) or a scan that could not run (exit 1, for
  example an unreachable database) fails the step for any of the six scans, and all six always run. The log opens each scan with
  `Go: go1.27.2` and `Scanner: govulncheck@v1.8.0`. The scanner is pinned to a tag; the advisory data is not, so a new advisory turns
  the job red between releases and the fix is a bump of the toolchain pin (also raise `toolchainFloor` in
  `gomod_toolchain_pin_test.go`) or of the dependency.
- **Not called, so green: the module findings of today.** The scans report these as present in a required module and not reached by
  any call: x/net v0.58.0 in the root module's linux build graph only (GO-2026-6603, 6610, 6611, 6612, 6617, fixed in v0.60.0; the
  windows graph does not contain the module), x/text v0.39.0 in `tools/comfyui` (GO-2026-6629, fixed in v0.41.0) and x/text v0.14.0
  in `tools/llamaswap` (GO-2026-6629, and GO-2026-5970 fixed in v0.39.0). None is touched here: the printed CLIs are vendored.
- **Tests, written first and red.** `TestRootGoModPinsAPatchedToolchain` (no toolchain line) and
  `TestCIRunsGovulncheckOnEveryModuleOnBothOSes` (no govulncheck in ci.yml) failed on the tree before the change. The job test runs
  the job's own scripts under bash with stand-ins for go and govulncheck, as the runner starts a step (`bash -e -o pipefail`) and,
  for the install step, under a bare bash too: the toolchain step must read go.mod (a made-up version proves it; a CRLF file, a file
  without the line and `toolchain default` are cases), govulncheck must be built under that GOTOOLCHAIN and be on PATH, each of the
  three modules must be scanned for each of the two systems with `-show version ./...` under that GOTOOLCHAIN, and a finding or an
  error in any one of the six must fail the step while the other five still run (counted, as the runner starts the step and under a
  bare bash) and an `::error::` line names the module and system that failed. The job and each of its steps must carry no `if` and no
  `continue-on-error`, of any value (`TestCIGateSwitchesAreSeen` holds that check on parsed text): a proof switched off without being
  deleted passed every structure test.
  `TestEveryTrackedGoModuleIsScannedOrNamedAsExcluded` closes "every module": it lists the tracked `go.mod` files and fails for one
  that is neither in the scan set nor named, with its reason, among the exclusions (today the two write-door contract fixtures), so a
  new printed CLI cannot go unscanned with every check green (`TestModuleScanCheckReportsEachProblem` holds each way it can fail).
  `TestGoModPinCheckReportsEachProblem` proves each rule of the pin check (a missing, invalid, too old or below-the-go-line
  toolchain, and x/sys, x/text and go-sdk gone back) on parsed go.mod text, because a real go.mod with an older module no longer
  builds; `TestGoVersionCompareIsNumeric` holds the comparators to numbers (go1.27.10 above go1.27.2, v0.100.0 above v0.48.0).
  `TestCIJobsProveTheCompilerGoModNames` finds the jobs that set up Go and compile the root module (not the printed-CLI jobs, which have
  no toolchain line, and not the govulncheck job, which pins GOTOOLCHAIN itself), requires the proof step after setup-go, before any
  other go command, with `shell: bash` and no `if` or `continue-on-error`, and runs its script with a stand-in go: the release the
  line names passes (LF and CRLF go.mod, a go command that prints a carriage return); an older compiler, a newer one, a prefix of
  the release, a longer release that begins with it, nothing, a failing `go env`, no toolchain line and `toolchain default` fail with
  the versions named in the log.
  The shared loader and the bash sandbox are in `ci_workflow_helper_test.go`.
- **What is not shown.** No hosted-runner run of this change exists yet. The steps were run end to end on the workstation under
  Git Bash with the real go and govulncheck: all six scans exit 0 under go1.27.2 (38 s); with `toolchain go1.27.1` planted in go.mod
  the toolchain step follows it and all six scans fail (eleven called standard-library advisories in the root module, nine in each
  printed CLI); with the vulnerability database unreachable all six fail; a planted call into x/text v0.39.0 (GO-2026-6629) exits 3.
  The proof step's own script, run end to end on the workstation with the real go (installed go1.27.1, `toolchain go1.27.2`): with
  GOTOOLCHAIN at its default it prints `go.mod names go1.27.2; the go command runs go1.27.2` and exits 0; with `GOTOOLCHAIN=local`
  it prints the error line and exits 1. The first run of `build` and `installer-windows` on the go1.27 line is the first CI has seen
  of it.

### Added — the Windows cage's environment proof runs on a Windows runner on every change (D-98, phase 1)

- **The gap.** The Windows cage hands its child an explicit environment block instead of the parent's (until 0.117.7 it passed
  `nil`, which means "inherit", so every `run` child received the delegator's credentials). `TestWindowsChildDoesNotInheritParentSecrets`
  and `TestWindowsChildGetsTheAllowlistedEnvironment` prove it, and on every other system the same names are skipped stubs. The
  `build` job runs `go test ./...` on Linux, where both are skips, and the one job on a Windows runner never ran `go test`: the proof
  ran only on a developer's own Windows machine, and a regression could merge with every check green.
- **The step.** `installer-windows` runs, right after Set up Go and before the PowerShell self-tests so that an unrelated failure there
  cannot hide it, `go test -count=1 -timeout 5m -v -run '^TestWindowsChild' ./internal/sandbox/` in bash and then requires the
  `--- PASS:` line of each of the two tests. `go test` exits 0 when the pattern matches nothing and when the tests skip (measured: `-run
  ^TestNothingMatchesHere` prints "no tests to run" and exits 0), so the exit code alone proves nothing; bash, which the runner starts
  with pipefail, keeps a failing test from hiding behind the pipe to tee; the PASS line must be a line of its own, not the same words
  inside other output. Only the standard `windows-latest` label is used (a larger runner is always billed).
- **Tests, written first and red.** `TestCIRunsTheWindowsCageProofOnWindows` failed on the tree before the change (no step of any job runs
  `go test` on `./internal/sandbox`). It requires the step in a job whose `runs-on` is exactly `windows-latest`, after setup-go and
  before any PowerShell step, with `shell: bash`, and runs the step's own script, as the runner starts it and under a bare bash,
  against a stand-in `go` that plays back real `go test -v` output: both proofs passing exits 0; either one skipped, no test run, the
  PASS words only quoted inside other output, only subtests' indented PASS lines, a longer-named sibling passing while the proof is
  skipped (`-run` is a prefix pattern), no output at all, or `go test` failing after printing both PASS lines (only pipefail catches
  that) exits non-zero. It also requires `-count=1` (a cached result would replay the PASS lines), `-v`, `-timeout`, the anchored
  `-run` and the package, and that the step and its job carry no `if` and no `continue-on-error`.
- **What is not shown.** The first run of the cage (CreateProcessAsUser, the job object and the low-integrity token) on a hosted runner has
  not happened. Both proofs pass on the workstation, and the step's script run end to end there with the real `go test` exits 0, and exits 1
  with an error line per test when the pattern matches nothing; with the environment block of `CreateProcessAsUser` replaced by `nil` (the
  0.117.7 defect) both proofs fail on the workstation ("the caged child inherited GITHUB_TOKEN"). If the runner refuses the low-integrity
  spawn the step goes red, which is what it is for; a weaker test of the environment block alone would not close this item. Phase 2
  widens the step to the whole `./internal/sandbox/...` package once the tests that cover the exit-status reading are merged, and this
  test changes with it.
### Fixed — on a box with no `layers`, a media lease on one card no longer fences the text models pinned to the other cards (C-86a)

- **The defect (register C-86a, the operator's complaint that routing is per card and fleet-wide).** The load fence (`modelaffinity`:
  `Admit`, `AwaitUpstream`, `AwaitRunSlot`) narrows a lease to the cards of the seat that loads, and it learns a seat's cards from one
  place, `Config.ModelPins`, which read the declared `layers` and nothing else. The installer's tier seeds write no `layers`, so on such
  a box every harness text load waited for a media lease on ANY card: a render on card 2 held back a text model pinned to card 0. F15
  had taught the unload side to place a model by the `CUDA_VISIBLE_DEVICES` of its entry in the serving config
  (`serving_config_path`, the llama-swap yaml the box runs) and named the fence as the half it left.
- **The fence places a model the way the unload list does.** `Config.ModelPins` asks the layers first (new `Config.LayerPins`, the
  previous body) and then the serving config's env pin, through the one reader both sides share, with F15's strictness unchanged: only
  the env pin counts; a bare index only beside `CUDA_DEVICE_ORDER=PCI_BUS_ID`; a `proxy:` seat (both vLLM seats), an entry that pins no
  device in env, a macro and an alias two entries claim with different pins place nothing. A media lease on one card now fences the models
  the file pins to that card, the pairs that span it and every model nothing places, and a text load for a model pinned to another card goes
  through at once; the mirror lease fences the other half. Layers outrank the file for a model both declare, as in the unload list.
  Everything that reads the pins gets the same answer: the waiter's place in line (`SeatCards`), `PinsFor` (the cards a run records, so
  on such a box a seat pinned to one card now shares that card's run-cap line, as it does with layers), the text and vision auto routes,
  the delegator's run-cap line and the warm-back guard.
- **One reader, in a leaf package.** `ServingPins` and its parser moved from `internal/gpualloc` to the new `internal/servingcfg`:
  `gpualloc` imports `config` and `config` imports `modelaffinity`, so the fence could not import it from where it was. What counts as a
  pin did not change. `LeaseScope.Locate` reads the file through `servingcfg.Load` and asks `LayerPins` for the layers half, so its lines
  (`placed elsewhere by the serving config pin: ...`) are unchanged. A parity test walks every model of a fixture and every one-card lease
  and asks the fence and the unload list: they cannot place a model differently.
- **A reload is followed without a restart.** The MCP server and the fleet node live for days and llama-swap's config is edited under
  them. `servingcfg.Load` Stats the file on every call and parses it again only when its modification time or size changed, or when it
  changed less than three seconds before the read (a rewrite in the same clock tick, to the same size, would otherwise hide behind the
  same pair: the rule git applies to its index). An unchanged file costs one Stat, a memo is never served for a file that is gone, and the
  memo holds at most 16 files.
- **The fence's idle path pays nothing new; the other readers pay a Stat.** The fence's own readers (`ScopeToModel`, which `Admit`,
  `AwaitUpstream`, `AwaitRunSlot`, the seat yield and `SeatLease` go through, and `SeatCards`) look a seat's pins up only for a lease
  that names cards, so under an idle box and a whole-node lease (which touches every seat whatever its pin) the fence never touches the
  file. `PinsFor` and the other callers of `Config.ModelPins` are not fence reads and ask on every call, an idle box included: the agent
  run's recorded cards, the text and vision auto routes and the delegator's run-cap line. Before this change those were an in-memory
  lookup of the layers; for a model no layer places, each now costs a Stat of the serving config, and a parse on the first call, after an
  edit and within three seconds of one. (A review found the first wording of this bullet, of the `seatscope.go` header and of
  `docs/systems/gpu-lease.md` wider than the code: it said an idle box never touched the file.)
- **Fail closed, everywhere, and said once.** A serving config that is unset, missing or not YAML, a card table that cannot be read, a
  whole-node lease, a model the file does not list and every entry listed above as placing nothing keep the fence where it was: any
  lease fences them. A serving config that is set and cannot be read used to switch the narrowing off with no word, and the refusal a
  blocked load prints names only the lease holder, so it read as the fence's old behaviour. The fence's reader now says, once per
  cause and per path, in the process log: `servingcfg: the serving config at <path> cannot be read (<cause>): the load fence places no
  model by it, so a lease on any card fences every seat the layers do not place`. The same fault coming back after the file read
  cleanly is said again; an unset path is the default and says nothing, and neither does a model a layer places (the file is not read).
- **Tests, written first and red on the previous tree.** `internal/config`: the fallback for each kind of entry
  (`TestModelPinsFallBackToTheServingConfigOnABoxWithNoLayers`), a box with layers still placing the models they do not declare
  (`TestLayersOutrankTheServingConfigForTheFence`), the copy-on-read (`TestModelPinsFromTheServingConfigAreACopy`), and the fence end to
  end through `Load` and a real card-scoped lease: a card-0 lease admits the card-2 models at once with no waiter registered, fences the
  card-0 models, the pairs, the vLLM `proxy:` seats and the unlisted, and the mirror lease fences the other half
  (`TestAOneCardLeaseFencesOnlyTheSeatsTheServingConfigPlacesOnThatCard`); an edit of the file moves a model off a leased card on the next
  poll (`TestTheFenceFollowsAnEditOfTheServingConfigWithoutARestart`). `internal/gpualloc`: the fence/unload parity
  (`TestTheLoadFenceAndTheUnloadListPlaceEveryModelAlike`). `internal/modelaffinity`: an idle box and a whole-node lease look no pins up
  (`TestSeatPinsAreNotLookedUpWhenNoLeaseNamesCards`). Green on the previous tree on purpose, as guards of the fail-closed direction
  (`TestTheFenceStaysClosedWhereTheServingConfigCannotPlaceAModel`, `TestModelPinsServingConfigThatCannotBeReadPlacesNothing`; the
  mutants below are what show they bite). Added beside them and not red first, because they need the new package: the real-file wiring
  in `internal/modelaffinity` (`TestASeatPinnedByTheServingConfigLoadsUnderALeaseOnAnotherCard`,
  `TestAProxySeatAndAnUnpinnedEntryStayFencedByAnyLease`: that package cannot import `config`, so they show the fence resolves a yaml's
  full-UUID pins, not that `Load` arms them) and the parser and memo tests that moved or were added with `internal/servingcfg`, which
  the mutants below turn red.
- **Mutants.** Twenty-one small mutants (a line or a few), each shown to compile (`go vet` over the package and its tests) and to have landed, each
  restored byte for byte, each turning a named test red: the fallback removed, the serving config outranking layers, `Load` arming the
  layers' reading, `Locate` asking `ModelPins`, `LayerPins` reading the file, the pins looked up on an idle box, a fence that does not
  narrow, the memo never hitting, ignoring the size, ignoring the modification time, unbounded, with no racy window, serving a file that is
  gone, or wording a missing file as a Stat; the copy-on-read dropped, names compared case-sensitively, and each of F15's five parser
  rules (proxy, bare index, contested alias, macro, an entry that pins nothing in env). One survived at first: the copy tests ran on a file
  edited moments ago, which the racy rule never memoises, so they passed with the copy removed; they settle the file now.
- **Review round (two LOW findings, both confirmed against the code).**
  1. *The fence discarded the serving-config read error.* Said once per cause now (the "said once" bullet above). Red first on the
     previous tree: `TestPinsOfSaysOnceThatTheServingConfigCannotBeRead` and `TestPinsOfSaysItAgainWhenTheSameFaultReturnsAfterTheFileWasRead`
     heard nothing. `TestModelPinsSaysOnceThatTheServingConfigCannotBeRead` ties it to `Config.ModelPins`, the door every caller asks, and
     to the two quiet cases (a model a layer places, an unset path). `TestPinsOfSaysNothingWhereThereIsNothingWrong` and
     `TestTheMemoryOfWhatWasSaidIsBounded` were green on the previous tree on purpose, as guards of the quiet direction.
  2. *The cost claim was wider than the code.* Narrowed to the fence's own lookups (the bullet above). The implementation was right and
     the prose was wrong, so the behaviour test (`TestOnlyTheFencesOwnLookupsWaitForALeaseThatNamesCards`: `ScopeToModel`,
     `SeatLease` and `AwaitRunSlot` look nothing up on an idle box or under a whole-node lease, `PinsFor` looks up on every call) was
     green before the wording changed. What was red first is `TestTheDocsDoNotClaimAnIdleBoxNeverTouchesTheServingConfig` (root), which
     refused the four retired phrasings in this file, `docs/systems/gpu-lease.md` and the `seatscope.go` header.
  3. *Mutants.* Nine small ones, each shown to compile (`go vet`), each restored byte for byte, each turning a named test red: the note
     removed, the once-guard removed, an unset path that says too, a recovered file that is not forgotten, a memory kept for all paths
     together, a memory that is unbounded, the serving config asked before the layers, `ScopeToModel` that looks up whatever the lease,
     `PinsFor` that waits for a lease. A tenth, the three retired phrasings restored, turns the docs test red in each of its files.
- **Not changed, and not verified here.** A vLLM seat is a `proxy:` entry in the serving config (its cards are pinned in its own unit) and
  `vllm_seats` carries names, not cards, so a lease on any card still fences it, and so the agent seat on a box that serves it through
  vLLM; placing it needs the unit's pin, a separate change. The fence is not a placement: nothing here chooses a seat or a card. No
  live lease was taken. The committed reader was run read-only over the serving config of one box (21 models: eight pinned to card 0
  only, five to card 2 only, six to both, the two vLLM seats unknown) and its card table, and the end-to-end tests ran on synthetic card
  tables; the live proof (a one-card lease, a short render, `/running` before and after, a text call for a model pinned to the other card)
  is owed to a quiet card window.
### Fixed — a scanner the first ticket started no longer scans that ticket a second time at once, and the test that counts the first scan's lane questions counts the first scan's alone (found in the 0.181.0 merge gates; 0.181.0 code and test)

- **What failed, and that the merge is not the cause.** After this branch took 0.181.0, the gate of `internal/mediaremote` failed
  `TestTheFirstScanReusesTheLaneReadingOnlyWhileItIsFresh` on two Linux gate hosts (`probes 2, want 1` on one, `probes 3, want 2` on the
  other) where the same package had passed at the 0.181.0 commit on one of them. The branch touches nothing in that package. Run alone,
  4 workers of 40 runs on each host, the test failed at the 0.181.0 commit as it did at the merge: 50 and 52 of 160 on one host, 13 and
  7 of 160 on the other. It is a race in 0.181.0 (code and test), and the one pass at the 0.181.0 commit was luck.
- **The cause is in two parts.** (1) `register` started the scanner for the process's first ticket and also woke it. The wake-up is a
  token that waits in the channel, so the new scanner took the ticket in its first scan, found the token and scanned the same ticket
  again at once. That second scan found the reading the first had reused gone and asked the lane again: a second card-table read within
  milliseconds of the first, which is the read the reuse (`verdictFresh`) exists to save. At the production rhythm of 10 s, the lane had
  been asked twice 300 ms after the first scan in each of four tries. (2) The test counted every question its lane runner was asked after
  the call returned, so it saw the second scan's whenever that scan beat the test goroutine, and the tests' 40 ms rhythm brings a further
  scan about 40 ms later; its "reused" half also relied on 250 ms of wall clock between the question and the scan, which a loaded host
  does not give.
- **The fix.** `register` wakes the scanner only when the registration did not start it. A ticket made while the scanner runs still wakes
  it (a scanner asleep in its 10 s timer would otherwise leave that ticket's call waiting up to 12 s for its first scan). The test makes the
  ticket and runs its first scan by hand on a queue with no scanner: the "reused" half gives the reading a minute, the "asked again" half
  keeps 250 ms against an identity wait of 600 ms (a slower host only makes the reading older). The assertions are unchanged, exactly 1
  and exactly 2. ADR 0083 item 5 says that the ticket that starts the scanner is not woken for.
- **Tests, written first and red.** `TestANewScannerLooksAtItsFirstTicketOnce` (the scanner as production runs it, a 10 s rhythm, so a
  lane question inside 300 ms of the first scan can only come from the wake-up) failed before the fix (`probes 2 within 300 ms, want 1`)
  and passes after; `TestATicketMadeWhileTheScannerSleepsIsScannedAtOnce` pins the other half (the second ticket's call answers well
  inside the 8 s that the rhythm could make it wait). Five mutants were run, each compiled and each killed: the wake-up for a started
  scanner back, no wake-up for a running scanner, the reuse ignoring the reading's age, no reuse at all, and the age counted from the
  ticket (the defect the test was first written for). With the fix, the three tests together, 4 workers of 40 runs on each of the same two
  hosts under the same load: 0 of 160 failed on either.

### Fixed — a named-seat `agent_run` is never retried on this box's own seat, a deferral never publishes another seat's answer, and a final cut at its budget says so (D-125, D-102, register D-134)

A fix round on the two `agent_run` entries below, from two independent reviews of the branch: five findings, three defects.

- **A named seat's second chance is another node that runs it, or none; it was this box's own agent seat.** `agent_run(model: <seat>)`
  handed the delegator a bare `route=remote`, and a bare remote route keeps the engine's remote -> local retry: when the node's first
  attempt abstained (the new flat `{answer}` schema makes a cut or empty final likely) or its seat went down, the retry ran the whole
  goal on THIS box's agent seat, a different model. The door then withheld that clean answer for a seat mismatch and blamed the node
  (`answered from seat "<this box's seat>", not the model named: its agent seat changed after the fleet was read`), after this box's GPU
  had done the work, and the first attempt's real reason was gone. The contract of a named seat is now a reasoned remote pin
  (`pin_reason: operator`, the caller having chosen the placement by naming the model), which the engine already obeys to the end of the
  subtask: the retry goes to another node whose agent seat is the name, and with none the node's own deferral stays the result. First
  placement is unchanged (it never fell back to local), and `offload_status` counts the call under `pins.reasoned.operator` instead of
  `unreasoned`; an `agent_run` that names no model keeps the bare remote route and its remote -> local retry.
- **The seat check covers a deferral's finished answer.** The check that withholds an answer from a seat other than the named one sat
  behind the deferral's return, so a deferral carrying the loop's finished prose (`schema_miss`) published that prose as `output`
  whatever seat wrote it. The prose is now held back when the seat that wrote it is not the name, or is not reported; the deferral keeps
  the node's own reason and class, and the reason gains a clause saying what was withheld. A deferral from the named seat keeps its prose.
- **A final cut at its budget is published as cut.** The final-answer fit (D-134) opens the forced final narrower than the configured budget
  on purpose, and a final the seat ends on that budget (`finish_reason: length`) is marked by the loop (`OutputTruncated`) but ended
  `done`, and the local `agent_run` door never read the mark: the half-sentence was published as a clean success. It is still the `output`
  (a partial is the caller's to use), and the result now carries `output_truncated: true`, the loop's `stop_note` and a `truncation_note` that
  names the budget the loop's fit opened the final at, when it narrowed it (not the fit predicted at run start), and points at `budget_note`.
  A complete answer carries none of the three.
- **Tests, written first and red.** Each failed on the unfixed code for the reason in its finding: the named-seat retry (abstention and
  seat-down, one node and two) ran this box's seat and published the false seat-change reason; the deferral test published another seat's
  prose and an unreported seat's; the cut-final test published no flag. Mutants, each compiled and each killed: the pin reason dropped, the
  deferral's seat check disabled, the truncation flag never published, the fit clause of the truncation note dropped.

### Fixed — `agent_run` fits its final answer to the wall that is left, so a wall between the minimal-answer floor and the estimate answers instead of being cut (D-102, register D-134)

`agent_run` runs its loop under a context deadline, and the loop was built without the final-budget fit (register D-95) that the
delegation node installs. D-102 sized the run and refused a wall below one tool step and a minimal answer, and left the
register's own case admitted: a 420 s wall against a `min_turn_sec` of 807 s and an estimate of 928 s, which the INV-5 rider
(ADR 0050) forbids refusing on. That run spent its tool steps and then opened the forced final answer at the configured budget (4x
the step budget, up to 8,192 tokens): 476 s of decode on a 17.2 tok/s seat, against a wall with at most 420 s in it, so it was cut
at the deadline with no answer. This closes what D-102 left PARTIAL: the case is still admitted, and is now usable.

- **The cure is the fit, not a refusal.** At the forced final step the loop now opens the answer at the largest budget the wall that
  is LEFT can decode at the seat's measured rate, `(remaining − max(10 %, 6 s)) × tok/s`, never above the configured final and
  floored at 1,024 tokens (the D-95 arithmetic, read off the live clock). The register's case, rebuilt from the seat-rate arithmetic
  (a 17.2 tok/s seat with a 330 s cold load, an 8,192-token final, ten steps, thinking off), opens its final at no more than about
  6,500 tokens with the whole wall left, and at about 4,600 (270 s of decode) once the tool steps have spent the 121 s the estimate
  gives them, instead of 8,192 (476 s). The tool steps are untouched. Nothing new is refused, so there is no INV-5 conflict, and the
  `min_turn_sec` refusal the row offered as the alternative is not needed.
- **The rate is the one the wall gate sized the run from.** A model the caller names, or a seat that placement picks, with no rate
  of its own is left exactly as before: no fit, the configured final. The agent seat's `agent_seat_tok_s` is not lent to it.
- **Published under the delegation wire's names.** The answer, and a run that died at its wall, carry `final_budget_fit` (the budget
  the forced final opened at) and `budget_note` (the arithmetic: `final 8192 → 6501 to fit 420 s at 17.2 tok/s`), only when the fit
  narrowed something. What the loop ran at supersedes the fit predicted when the wall started, as on the node; a run cut before its
  forced final carries the prediction, and so does one that answered early or reached its forced final with wall to spare: the
  estimate charges the seat's remembered cold load against the wall and admission pays it outside the wall, so a wall just under the
  estimate predicts a narrowing the live clock never asks for (900 s against 928 s predicts a 6,174-token final, and the final opens
  at 8,192). A wall with room, a seat with no rate and a run that neither the prediction nor the loop narrowed carry neither field,
  and the wall refusal, which starts no run, carries neither.
- **One copy of the arithmetic.** `pipeline.InstallFinalFit` installs the fit on the loop and returns the run-start figure, and both
  agent doors call it, so they cannot size one run differently. The node's behaviour is unchanged (a characterisation test, run
  before the move and after it, passes on both sides), and `TestAgentRunSizesTheRunStartFinalLikeTheNodeDoes` compares the two
  doors' published sizing on the register's case byte for byte.
- **What it does not do.** The fit narrows the final answer. It does not shorten the tool steps, so a run whose tool steps outlast
  the wall still ends at the deadline, and a wall that buys fewer than 1,024 tokens of final at the seat's rate keeps the floor and
  the deadline stays the stop (`budget_note` says `the wall will be the stop`). A wall below one tool step and a minimal answer is
  still refused (D-102).
- **Observed, not changed.** The node runs its loop under the liveness ceiling (ADR 0055), at least twice its wall, so its live fit
  has room until a run nears the ceiling: on the register's case the node's forced final opens at 8,192 while its published
  run-start fit says 1,024 (measured this release on a seat that answers every step with a tool call until the forced final). On a
  run the fit left alone the node publishes the configured budget as `final_budget_fit`, where its docs say both fields are absent.
- **Tests, written first and red.** The four door tests failed on the unfixed tree (the forced final opened at 8,192 and
  `final_budget_fit` was absent: the wall-fit case, the door-against-node comparison, the run cut before its forced final, the
  prediction published when the loop's own final was not narrowed) and the two installer tests failed against a no-op installer.
  Three tests pass on both sides of the fix and guard it from the other direction: a seat with no rate, a named model with no rate
  of its own and a wall with room publish no fit; the wall refusal carries none; and the node's published run-start fit is
  unchanged by the move. Mutants, each compiled and each killed: the installer's closure returning the configured budget, the
  closure never installed, a raised budget, the answer or the deferral left unstamped, the door installing with no rate, the node
  no longer calling the shared function, the stamp ignoring its own narrowing rule.

### Added — `agent_run(model: <seat>)` runs on the fleet node whose agent seat is that model, and every routed `agent_run` carries the schema the gate requires (D-125, C-46)

Two defects shared one door. `agent_run` checked the `model` it was given against this box's llama-swap roster and refused any
name that roster lacks (`explicitly requested model … is not in the endpoint's served roster`), however many fleet nodes ran
it, so a seat another node runs could not be asked for by name from a box that does not serve it. And a routed `agent_run`
(`route: remote | auto | spread | queue`, register C-46) built its contract with no `output_schema`, which the delegator refuses on
every node as `noschema` (remote output merges only after a mechanical check) and which a node's own ack refuses too, so a
routed call could never be placed on a node. The new real-gate test reproduces it on the unchanged tree:
`route=remote: the contract carries no output_schema … node-a: noschema; node-b: noschema`.

- **A name this box does not serve is looked up on the fleet.** With no route, or `route: "remote"`, the door reads the roster's
  health once (`rosterprobe.Probe`, the read the single-shot lanes share: concurrent, 5 s a node, a down node named and not waited
  on), takes the nodes that advertise the agent lane and whose `agent_seat` is the name (without regard to case), and sends ONE
  contract through the delegator at `route=remote` with its remotes narrowed to them (a call's own list replaces the configured
  roster and is checked against it). It runs on exactly one of them, placed by the usual gate. A composite node whose layer has an
  agent-role seat of that model counts too, and the contract names the layer (register A-100); nodes that run the name as their own
  agent seat are preferred, and one contract names one layer, so the first layered node's layer wins and only the nodes that serve
  it from that layer are asked. The response names `node`, `placement`, `seat`, `model` (the seat that ran it), `route` and
  `executed_on`, and carries no sizing of this box (the wall is the executing node's expectation, ADR 0055).
- **It honours a name only when it IS a node's agent seat.** The contract names no seat, so the node runs its own agent seat, and
  no wire field and no node changed: it works against the nodes as deployed. A name a node merely serves (an alias, a non-agent
  seat) is refused naming the seat that node does run; nothing is guessed. The answer is checked against the name: a node that
  changed its agent seat between the read and the dispatch runs another seat, and an answer whose `seat` differs is withheld
  (`defer_class: config`). Running a seat that is not a node's agent seat needs the contract to name it, a wire change that widens
  what a fleet-token holder can make a node load; it is not here.
- **A name nobody runs keeps the roster sentence and says where the fleet looked,** one clause per node (its id, its agent seat,
  a name it serves without running it as its agent seat, an agent lane that is off, a node that did not answer). `defer_class` is
  `config` when every node answered and `infrastructure` when one did not, and the reason names that node; an entry the tailnet
  guard never dials is the roster's to fix and stays `config`. A box with no `delegate_remotes` refuses byte for byte as before.
- **The fork is narrow, and a model that cannot be honoured is no longer dropped.** A name this box serves runs here and never
  reads the fleet; `route: "local"` never asks it whatever the name is; a local roster that cannot be read leaves the call on this
  box. `route: "auto"`, `"spread"` and `"queue"` with a model are now refused (`defer_class: contract`, `steps: 0`, nothing asked of
  the fleet, the reason names `route=remote`): they place by capacity, not by seat name, and the model used to be ignored without a
  word. `route: "remote"` with a model asks the fleet whatever this box serves. `allow_browse` cannot travel (the browse tool drives
  this machine's own browser) and is refused when the name could only run on a node.
- **Every routed `agent_run` now passes the gate.** The goal goes through the delegation intake as a contract with a flat
  `{answer}` `output_schema` (flat and string-only, like `offload_ask`'s): the wire's own ceilings (12 steps, 900 s) apply at the door, a field
  the intake refuses (a thinking policy, a setup action) is refused there naming it (`defer_class: contract`), and a call with no
  `timeout_sec` lets the executing node size the wall. The prose is still the `output` and the node's `{answer}` object rides
  beside it as `structured`. `read_root` and `judge` do not travel, and the response's `notes` name each one the caller passed. A
  deferral carries the node's `defer_class`, its `node` and `placement`, its steps and, when only the structuring failed, the
  finished prose as `output` flagged `schema_miss`, as `offload_ask` does; the delegator's rescue of such an answer stays unwired
  on this door.
- **Tests, written first and red.** The fixture drives the real door at a real local roster and real scripted fleet nodes
  (`/fleet/health`, `/fleet/dispatch`, `/fleet/jobs/{id}`) through the real delegator, and reads the engine's arguments through the
  dispatch seam where a test needs them. With the door change removed, 23 of the 27 tests fail (the roster refusal for every named
  seat, `noschema` for every routed call, a model on `auto`/`spread`/`queue` dispatched and ignored, no class, no notes, no
  placement) and the other 4 pass on both sides: three guards (a locally served name and `route: "local"` never read the fleet; a
  box with no nodes refuses as it did) and the unit test of the ledger guard's reading. The fixture keeps the ledger and every state path in a directory the test owns, checks
  that configuration, and checks that the operator's real ledger took no row of the run (a fixture that sets its home after
  the default configuration has fixed the ledger path writes to the real ledger; the package's `TestMain` records one such
  incident). 24 mutants were run, each compiled and each killed after two survivors led to two more tests (the preference for
  plain nodes over layered ones, and which layered node's layer is taken):
  matching `served_models` instead of the agent seat, the engine handed the whole roster, a withheld answer published, no
  schema, a model on `auto` ignored, forced local asking the fleet, an unreachable node classed `config`, the layer never named, the
  fleet asked for a served name, notes dropped, a browse grant let through, the roster check dropped, the model not published,
  a guard-refused entry classed `infrastructure`, an agent lane that is off still a target, the last layer taken, the
  `route=remote` class forced to `config`, the contract error ignored, the deferral class, placement or finished answer dropped,
  and the two refusal texts.
### Fixed — a test that earned a remembered incoherent-seat verdict no longer leaves it for the next test on the same loopback address (F78)

- **The hole.** The incoherent-seat memo (`internal/pipeline/coherence.go`) is one table for the whole process, keyed by endpoint and
  seat for ten minutes, and a run on a seat that is already resident defers on a remembered verdict without asking the seat anything.
  A test's fake server gets a loopback port the OS can hand to a later fake, and the tests that earn a broken verdict
  (`TestRunAgentTaskDefersAnIncoherentSeatAfterColdLoad` and `TestNoWallIsReportedUntilTheWallStarts` in `internal/pipeline`;
  `TestAgentRunDefersAnIncoherentSeat`, `TestAgentRunDefersAWarmRunOnTheRememberedIncoherentSeat` and
  `TestAgentRunIncoherentSeatDefersCarrySizing` in `internal/mcpserver`) left it behind; only the tests of the memo itself cleared
  theirs. The register names `TestTheWarmUpKeepsItsBudgetAfterAWaitInLine` as the victim: it takes the warm path against a fresh fake
  and would defer on a verdict that was earned elsewhere.
- **The fix (test code only).** `agentFake.server` drops every verdict filed under its server's endpoint, whichever seat it names,
  when the test that asked for it ends (the endpoint is matched whole, so a server on `:1234` never takes `:12345` with it), and
  `coherenceSeatServer` clears the seat it serves the same way.
- **Tests, written first and red.** `TestAnIncoherentSeatVerdictDoesNotOutliveTheTestThatEarnedIt` in both packages runs the cold load in
  a subtest, asserts the verdict IS remembered there (so the check after it cannot pass vacuously) and that it is gone once the subtest
  has returned. Red before: `the verdict on seat "seat-x" outlived the test that earned it` (pipeline, and for a second seat on the
  same endpoint) and `... "agent-pool" outlived ...` (mcpserver). The three tests the register names pass 30 repetitions in a row; that
  does not force the port reuse, so the new tests are the proof.

### Fixed — the root, pairworkloads and mediaremote suites left a state-root folder in the OS temp dir for every copy of their test binary that a test re-launches (F77b)

- **The defect.** Each of the three packages makes a throwaway state root in its `TestMain` (`lo-root-state-*`, `lo-pairworkloads-state-*`,
  `lo-mediaremote-state-*`) and removes it after `m.Run`. They also re-launch their own test binary (the helper processes of the gpu verb
  tests, a child that sends a frame and is killed, the holder of a queue lock). A copy runs `TestMain` too, and a copy that is killed or
  ends by `os.Exit` from inside a test never reaches the removal, so every launch left a root of its own. Counted with the temp dir pointed
  at an empty folder on Windows, one run of `internal/pairworkloads` left 2 and one run of `internal/mediaremote` left 1, and one run of the root package's tests left 20 (on Windows and on Linux alike).
- **The fix (test code only).** The suite process exports `LO_ROOT_SUITE_STATE_ROOT`, `LO_PAIRWORKLOADS_SUITE_STATE_ROOT` or
  `LO_MEDIAREMOTE_SUITE_STATE_ROOT` once its root exists, set to that root's path, and every re-launch site already passes the process
  environment on. A copy shares the root of the suite that launched it and makes none, but only when the environment agrees three ways:
  the variable names a root, `LOCAL_OFFLOAD_STATE_DIR` points at the same one, and it is one of the package's own throwaway roots and
  still on disk; anything less gets a root of its own, as before (the net exists to keep the suite off the machine-wide root, so it
  fails closed). The root package's copy still runs on the same known host reading as the suite.
- **Tests, written first and red.** `TestARelaunchedTestBinaryLeavesNothingInTheTempDir` in each package re-launches the test binary at a
  helper that ends with `os.Exit(0)` from inside the test, with `TMP`, `TEMP` and `TMPDIR` on its environment pointed at an empty folder,
  and requires the folder to stay empty; `TestACopyTrustsTheSuitesStateRootOnlyWhenTheEnvironmentAgreesWithIt` holds the three conditions.
  Red before (Windows): `a re-launched copy of the test binary left 1 entries in its temp dir, want none: [lo-root-state-…]` (and
  `lo-pairworkloads-state-…`, `lo-mediaremote-state-…`). `TestMainIsolatesTheStateRoot`, `TestMainIsolatesTheHomeAndTheLedger` and
  `TestRunIsolatedFailsClosed` are unchanged.
- **Review round: a re-launched copy of the root suite also checks the home and the origin.** The copy skips `runIsolated`, which is
  also what pins `LOCAL_OFFLOAD_HOME` (to the root's `home` folder) and `LOCAL_OFFLOAD_ORIGIN` (to `go-test`), the ledger guard of
  2026-10-03 (1,204 fixture rows written to the operator's real ledger); the copy read both from the environment of whichever test
  launched it, unchecked, so a launcher that had cleared or moved them would have run `config.Default()` on the real home. A copy now
  trusts the suite's root only when the home and the origin also agree (five conditions); otherwise it makes a root of its own, which
  pins them, as it did before F77b. Red before: the home-not-set, home-outside-the-root, home-is-the-root, origin-not-set and
  origin-of-another-writer cases of `TestACopyTrustsTheSuitesStateRootOnlyWhenTheEnvironmentAgreesWithIt` returned `trusted=true`;
  deleting either check turns its cases red.
- **Review round: `internal/pairworkloads` now fails closed.** Its `TestMain` logged and ran the suite anyway when `os.MkdirTemp` or
  either `os.Setenv` failed, so a suite whose setup failed ran against the machine-wide state root, while its comment and
  `docs/systems/pair-workloads.md` called it fail-closed (the F77b suite variable had copied the idiom). It now goes through a
  `runIsolated` like the root, `internal/delegate`, `internal/mcpserver` and `internal/mediaremote`: any of the three failures returns 1
  without running a test and removes the directory it made. `TestRunIsolatedFailsClosed` is new there; red before: `code = 0, ran = true;
  want 1, false` for the mkdir, state-dir and suite-variable cases (and the removal assertions fail when the deferred removal is dropped).
  The docs paragraph no longer lists `internal/pipeline` among the fail-closed packages: its `TestMain` fails open on purpose (its own
  comment says so) and the paragraph now says that.

### Fixed — `offload_run_graph` and `run-graph` remove the temp files an inline graph or manifest became, whichever way the call ends (F79)

- **The defect.** The runner reads files, so an inline `graph_json` / `manifest_json` (the MCP door) or `--graph-json` /
  `--manifest-json` (the CLI) is written to a temp file (`run-graph-*.json`, `run-graph-manifest-*.json`) and its path is threaded on.
  Nothing removed it: every inline call left one file (a graph) or two (graph and manifest) in the OS temp dir for good, on every exit
  of the call (an answer, a deferral, a refusal, a ticket queued), and the CLI left them too when the pipeline could not open. A write
  that failed left the file it had just made, and a manifest that could not be written left the graph file made before it. One run of
  the `internal/mcpserver` tests left 26 of them (25 `run-graph-*`, 1 `run-graph-manifest-*`) and one run of the root package's tests
  left 3, counted with the temp dir pointed at an empty folder on Windows. The fleet node's twin (`fleetnode.buildRunGraph`) already returned a cleanup; the door and the CLI did not.
- **The fix.** `materialize` (MCP) and `materializeArg` (CLI) return a cleanup that is never nil and removes only the file THIS call
  made: it does nothing for a path the caller named (`graph_path`, `manifest_path`, `--graph`, `--manifest`, or a `graph_path` given
  together with a `graph_json`), nor for an empty one. `handleRunGraph` defers each cleanup right after the file exists, so an answer,
  a deferral, a refusal inside `RunWith`, a manifest that cannot be written and a panic all remove both files; `runGraphParams` returns
  one cleanup for the pair and `runRunGraph` defers it before the pipeline opens, so the exit at `openPipeline` is covered too. A write
  that fails closes its file and then removes it (Windows does not delete a file that is still open: `os.Remove` of an open file fails
  with "being used by another process", read on Go 1.27.1), and a manifest that cannot be written removes the graph file with it.
- **Why removing them is safe.** A graph ticket keeps its own copy of the graph and the manifest as the plan holds them
  (`body-<id>-graph.json`, `body-<id>-manifest.json`) and its key, its node payload, its local run and an adopter all read those, never
  the caller's paths; a graph queued by a call that has returned still runs.
  `TestAGraphTicketStillRunsOnThisMachinesLaneOnceTheDoorHasRemovedItsInlineFiles` admits a graph, removes the caller's files the way the
  door does, frees the lane and requires the lane to run the bytes the call was made with; it fails when the ticket reads the caller's
  path instead of its copy.
- **Tests, written first and red.** `internal/mcpserver`: `TestRunGraphDoorRemovesItsInlineTempFilesOnEveryExit` (six exits),
  `TestRunGraphDoorHandsTheRunnerTheInlineBytesWhileItRuns` (removal is not early), `TestRunGraphDoorNeverRemovesACallersOwnFiles`,
  `TestMaterializeCleanupRemovesOnlyTheFileThisCallMade`. Root: `TestRunGraphParamsInlineJSONIsRemovedByItsCleanup`,
  `TestRunGraphParamsLeavesACallersOwnFilesAlone`, `TestRunGraphParamsRemovesTheGraphFileWhenTheManifestCannotBeWritten`,
  `TestMaterializeArgRemovesItsOwnFileWhenTheWriteFails`, `TestRunGraphCLIRemovesItsTempFilesWhenThePipelineCannotOpen`. The write
  failures are made with a one-line seam (`createTemp`) that hands back a file opened for reading only. Red before, root package on a
  Windows PC against a cleanup that removes nothing: `the cleanup left [run-graph-….json run-graph-manifest-….json] in the OS temp dir`,
  `the file of a failed write was left`, and `run-graph left [...] in the OS temp dir after it failed to open the pipeline`.
  After: 0 `run-graph*` entries left by the `internal/mcpserver` tests on Windows (26 before) and by a whole run of the root package's
  tests on Windows and on Linux (3 before).
- **Review round: a delete that is refused for a moment is waited out, and one that still fails is named.** The cleanup made one
  `os.Remove` and dropped the error, so on Windows a scanner (or a runner that had only just exited) holding the file for a moment
  brought the leak back with nothing to say so; the write-error branches dropped it the same way. Both doors now delete through one
  helper (`removeInlineTemp`, in `internal/mcpserver` and in the root package) that tries again for about half a second (20 attempts,
  25 ms apart, as `removeRunGraphEnvelope` does for the envelope), treats a file that is not there as done, and, when the file still
  resists, writes one line naming it and the reason (to the log in the MCP server, to stderr in the CLI). Red before, on a Windows PC,
  in `internal/mcpserver` (and the same four as `TestMaterializeArg...` in the root package): `TestMaterializeCleanupWaitsOutAFileHeldForAMoment`
  (a delete refused three times left the file), `TestMaterializeCleanupNamesAFileItCannotRemove` (no line),
  `TestMaterializeRemovesTheFileOfAFailedWriteEvenWhenItIsHeldForAMoment` and
  `TestMaterializeCleanupOutwaitsAHandleThatIsClosedAMomentLater` (a real open handle, no seam: the cleanup returned and left the file).
  One attempt, no line, a missing file counted as a failure and the failed-write branch left out of the helper each turn their own test red.
- **Not covered.** Files an earlier release left in the OS temp dir stay until someone removes them (this is a one-time cleanup, not
  code), and a call that is killed (a second Ctrl-C, a kill) leaves the files of that one call.
### Added — a mutation harness whose RED has to compile (register H-17)

- **`scripts/mutation-check.sh` and its PowerShell twin `scripts/mutation-check.ps1`** are the one proof that a test can fail. They run the
  unmutated control arm (which must pass), apply a single-line mutation and assert it landed (line endings compared normalised, so an LF
  pattern cannot fake a pass on a CRLF file), compile the test binary first (a mutant that does not build is `BAD-MUTANT`, never `RED`),
  report `RED` or `SURVIVED`, and restore the file byte for byte on every path, including a signal or Ctrl-C; each prints the applied
  mutation and a final `MUTATION-VERDICT:` line. The self-tests (`scripts/mutation-check-selftest.sh`, `.ps1`) show every verdict on the
  fixture module `scripts/testdata/mutfixture`; `CONTRIBUTING.md` and `docs/systems/security.md` (invariant I5) point at the harness. No
  Go package is touched.
### Fixed — the fleetnode and pipeline tests left a folder in the OS temp dir for the suite and one for every copy of the test binary they re-launch

- **The defect.** `internal/fleetnode`'s tests made their compose cache folder in an `init()` (`os.MkdirTemp("", ...)`) and nothing removed
  it; `internal/pipeline`'s `TestMain` made a throwaway state root and removed it only after a normal run. Both packages re-launch their own
  test binary (the stand-in ffmpeg, the dead-owner helper, the browse sidecar stand-in). A copy runs the package's `init()` functions and
  `TestMain` too, and a copy that exits from a test, or is killed, never reaches a cleanup, so every launch left a folder: one run of each
  package left 13 (counted with the OS temp dir pointed at an empty folder), and a workstation that runs the gate all day collected them on
  its system drive. Test code only: no production code makes either folder.
- **The fix.** Each fleetnode test that needs a compose cache takes its own `t.TempDir()` through `ComposeCacheDir` (taken before
  `newTestServer`, so the server's drain finishes before the folder goes), and `init()` makes nothing but a table entry. The pipeline's
  `TestMain` recognizes the re-launched browse sidecar stand-in by its argument (the sidecar's environment is an allowlist, so nothing the
  suite sets can reach it) and starts no state root for it; the suite still removes its one root. `TestARelaunchedTestBinaryLeavesNothingInTheTempDir`
  (one per package) launches a re-launched copy the way the suite does, with the temp dir pointed at an empty folder, and requires it to stay
  empty; `TestTestsReachForTheOSTempDirOnlyWhereItsRemovalIsOnEveryPath` fails on a new direct `os.MkdirTemp("", ...)`, `os.CreateTemp("", ...)`
  or `os.TempDir()` in a test file of either package unless it is listed with the reason its folder is removed on every path. Each was shown
  to fail without the fix (the lint in pipeline by adding one raw call).
- **Measured** on Windows with TMP, TEMP and GOTMPDIR pointed at an empty folder, `go test -count=1 -v`: fleetnode 13 folders left before, 0
  after (three runs); pipeline 13 before, 0 after. The same tests are there and pass: fleetnode 880 passed and 5 skipped before, 882 and 5 after;
  pipeline 1497 passed and 1 skipped before, 1499 and 1 after (the two new tests each).
### Fixed — the render drain no longer waits on a seat that cannot have a request in flight (F81)

- **The defect.** `quiesceLlamaSwap` (`render/gpu-lock.mjs`) probed `/upstream/<id>/slots` of every seat `/running` listed, whatever its
  state. llama-swap lists a process in `starting`, `ready` or `stopping` (the source of `internal/process`: `StateStarting`,
  `StateReady`, `StateStopping`; `stopped` and `shutdown` are not listed). A `starting` seat is not serving, and llama-swap holds
  a request to it for the whole load, so the probe sat until its own 5 s deadline: three rounds, two polls and a grace, 17.6 s
  measured against a fake llama-swap with one starting seat (`17588 ms`, three upstream requests), added to every render start while
  any seat was loading. No request could be in flight on that seat, so there was nothing to wait for. A `stopping` seat is worse than
  slow: the router queues a request behind the stop and then starts the seat again (`EnsureReady`: "stopping -> waits for the stop to
  finish, then starts"), so the probe would load the model the drain exists to clear, the hazard the `/running` gate was already
  written against.
- **The fix.** The drain keeps the state it reads from `/running` and skips a seat in `starting` or `stopping`: it counts as nothing
  to drain and is not named unknown. The same drain over the same seat: `26 ms` and no upstream request (`stopping`: `45 ms`).
  Everything else is probed exactly as before: a `ready` seat above all (a ready seat whose `/slots` never answers still costs the
  17.6 s and is still named unknown), a seat listed with no state (older builds) or with a state this code does not know. The unload
  that follows is unchanged; what changes with the skip is what the drain says. The Go-side drain reads both states without probing
  the upstream (`internal/seatload`) and waits a load in progress out, because the request that triggered the load is waiting on the
  engine. The render helper does not wait, by decision (register F81), and before the skip it waited only as long as its probes lasted:
  by the code's path (register D-92 says llama-swap holds the request for the load; not observed live) a load that finished inside the
  three 5 s deadlines let the probe read a real slot and the drain then waited for that request, and
  any other starting seat was unloaded after the same 17 s with `could not read /slots for: <seat>`. With the skip alone a starting
  seat ended the drain as verified idle and the unload went out with no line, so the drain now returns the seats it skipped for a
  load in progress as `starting` (the last round's, also when it ends at its deadline), and `freeLlamaSwap` logs one line for them:
  `unloading <seat> while its load is in progress (the drain does not wait for a seat that is starting); a request waiting on it is
  aborted`. A stopping seat says nothing: it is already leaving. The one outcome that differs from before is a load that would have
  finished inside the old 17 s window with a request in flight: that request is no longer waited for, and the line is its trace.
- **Tests.** `render/gpu-lock-drain-state.test.mjs` (17 tests) runs the real drain and `freeLlamaSwap` against the fake llama-swap
  below: starting and stopping seats (four spellings of the state) are not probed and end the drain at once, and the starting ones are
  named in the result; a ready seat with a busy slot is waited for until idle (the control); a ready seat that never answers is probed
  and named unknown; a starting and a stopping seat beside a busy ready one leave the ready one waited for; a seat that finished
  loading while the drain waited on another is probed from then on and not named; a seat with no state, an empty state, an unknown
  state and `ready` is probed as before; a render start behind a starting seat takes under a second, still sends the unload and says
  so in one line; two starting seats are named in the plural; a stopping seat and a ready one say nothing; a drain that ends at its
  deadline still names the starting seat. Red before: of the first twelve tests six failed, each with the upstream requests it should
  not have sent, and of the fourteen that stood when the naming was added eight failed. Reverting the skip fails nine of the
  seventeen; skipping `ready` too fails six; a rule written as `state !== "ready"` fails three of them (the no-state, empty and
  unknown-state controls) and six tests of `gpu-lock.test.mjs`, whose fake seats carry no state: nine in all. Each part of the naming
  (recording a starting seat, leaving a stopping one out, the per-round list, the list on the deadline return, the log line, its
  plural) has a mutant that turns a test red.

### Fixed — the render tests no longer reach the llama-swap or the GPU lease of the machine they run on (F80)

- **The defect.** `comfy-keep.test.mjs` ("a runner given keep-comfy exits after its job while the instance it launched stays up") spawns
  a runner that calls `withGpuSlot({ noLock })` with the default `freeLlamaSwap`, so the runner drained the llama-swap named by the
  parent environment (`LLAMA_SWAP_API`, default the machine's own). A seat that was still starting held the drain's probe for 3 x 5 s,
  which put the test over its own 15 s wall (15.7 s, red against a fake llama-swap with one starting seat), and a drain that finished
  sent a real unload of a live seat. The audit found the same gap in six more test files that spawn a runner (`audiocpp-generate`,
  `comfy-edit`, `comfy-instance`, `comfy-render` with three spawns, `sdcpp-animate`, `sdcpp-video`) and in four that call `withGpuSlot`
  in process (`comfy-instance` and `lease-progress` reached the default `freeLlamaSwap`; `comfy-keep`, `comfy-unkeyed-api` and
  `lease-progress` took the default lease, which is the parent's `GPU_LEASE_*`, while `comfy-instance` already passed `lease: null`).
  A spawned runner inherited `GPU_LEASE_*` too, unless the test
  scrubbed it by hand (`comfy-edit` and `comfy-render` did, for the lease only).
- **The fix.** `render/test-isolation.mjs`: `isolatedRunnerEnv(extra, base)` gives a child `LLAMA_SWAP_API=http://127.0.0.1:9` (a port
  nothing listens on: the drain gets a refused connection and handles it at once), drops every `LLAMA_SWAP_*` and `GPU_LEASE_*`
  variable, and lets what the test passes win. The spawning tests use it; the in-process calls name `freeLlamaSwap: async () => {}` and
  `lease: null`. `render/fake-llama-swap.mjs` is a small llama-swap on a loopback port for tests that need behaviour: per-seat state,
  `/slots` that answers, hangs or changes per call, every request recorded, `close()` destroys the sockets a hung request holds.
- **The guard.** `render/test-isolation.test.mjs` scans every render test and the test support that spawns runners: a runner spawn
  whose own call does not pass an isolated env, an in-process `withGpuSlot` that names neither `freeLlamaSwap` nor `lease`, and a
  `freeLlamaSwap` or `quiesceLlamaSwap` call with no endpoint or fetch, fail. Each runner spawn is judged on the env its call passes
  (the helper or both closings in the call, a name declared in the same test or at the top of the file, a parameter judged by what
  every caller hands it, or a builder the file declares): isolation written for one spawn vouches for no other, so a runner spawn
  added to a file that already isolates one fails, and node spelled as a name bound to `process.execPath`, as `"node"` or as `fork()`
  counts as a node spawn. An env that reaches the call through a member (`sb.env`) or an import is not traced and is judged by the
  file as a whole, as far as the first version of the scan went. Its fixtures pin what it catches and lets through. Run on the tree
  before the fixes the first version listed twelve violations and the per-spawn scan lists fourteen (`comfy-render.test.mjs` spawns
  three runners); with the isolation of `comfy-keep.test.mjs` put back to the old line it fails on that file.
- **Proof.** With the parent environment pointing at a fake llama-swap whose one seat is starting and never answers `/slots`, the old
  test fails at 15.7 s (three `/slots` requests on the fake), the fixed test passes at 1.7 s with no request on the fake, and the old
  environment line put back fails at 15.6 s again.
### Fixed — a lease that unloads the agent seat is no longer held back by the host RAM that unload frees (F82)

- **The deadlock.** `gpu reserve --ram N --drain --unload-seat` compared `N` to the host's committed memory inside the grant, and
  drained and unloaded the seat only after the lease was held. When the memory it needed was the memory the seat held, the grant
  waited for commit that its own unload would have freed: a film lease that declared its host RAM waited about six hours while the
  3-card vLLM seat held 67 GiB of the host's commit. Same class as the DRAINING stamp of 0.117.0 (a gate in front of the step that
  clears it).
- **The order now (a reorder, not a looser check).** A lease that asks for `--unload-seat` and declares host RAM takes its place in
  line and its cards first, declaring nothing (the figure travels as `Options.HostRAMWaitGiB`, stamped on the record as
  `host_ram_wait_gib`), is drained and unloaded as before, and only then is the figure put to the host (`Manager.AwaitHostRAM`),
  still holding the cards, inside the same `--wait`, with the grant's own sentence printed once. The comparison is the grant's
  function over the same reading, headroom and not-yet-loaded sum, and the check and the stamp that turns the wait into the
  lease's declared need are one critical section under the epoch lock, so two leases cannot both take the last headroom. If the
  unload frees too little the lease keeps its cards, keeps waiting, and ends with the host-RAM refusal; its command never runs and
  the seat's warm-back stays owed. A need no state of the box could ever admit is still refused at once, before the seat is touched.
  The order applies to a lease whose unload can free memory for it and whose host can be read; every other lease compares in the
  grant exactly as before: one without `--unload-seat` (`--drain` alone included), one whose unload reaches no model on its cards
  (a `--devices` lease on cards that neither the agent seat nor any other model the roster lists is placed on: the reserve says so
  and the lease queues without holding them), and one whose host memory cannot be read (the look after the unload would refuse for
  the same reason, with the seat cycled and the cards held).
- **Every form.** The wrapper, `--detach` (the parent asks the host after `detachMaintain`; the hidden holder is started with
  `--ram 0 --ram-wait N` and compares nothing in its grant) and `--devices` / `--cards` (the allocator's host term and the queue's
  pass rule see a request that declares nothing). `gpu hold` has no unload of its own and declares what it is given. `gpu status`
  and the lease rows show `host_ram_wait_gib` ("host RAM N GiB asked, not yet admitted") until the host admits it. A signal during
  the wait ends it with "while waiting for host RAM". `--cards` chooses its cards after the grant, so it cannot ask beforehand
  whether the unload reaches them and still defers: an allocation that lands on cards the unload does not reach holds them while
  the host is short (the wait line says the drain and the unload are done, which is true for it). A detached holder whose wait for
  host RAM fails stays held, as after a failed drain, but its exclusive stamp is taken off and the error names `gpu release
  --warm-seat`, because the seat is cold.
- **Stated consequences.** A lease that declares nothing in the grant passes a front waiter that waits only on host RAM (the G6 rule)
  and a grant that fits on other cards goes ahead of it while it waits; the media admission in `internal/pipeline` is not changed.
  A shortage the unload does not cure (another lane or a foreign process holds the memory, or the seat was already cold and there
  was nothing to free) costs the cards held, exclusive, and the
  seat cold until `--wait` ends it, where the grant used to queue without touching either. No screen before the grant can tell the
  two apart: how much of the host's commit the seat holds is not measured here (`agent_seat_host_ram_gib` is a floor the operator
  states, its default a chosen 21 GiB, and the film seat held 67 GiB), and a prediction taken when the reserve starts is stale when
  the grant comes, so one that sent the lease back to the grant would bring the deadlock back whenever the cause changed in between.
  Only a need above physical RAM less the headroom is refused beforehand.
  Details: `docs/systems/gpu-lease.md`, "A lease that unloads the seat is compared to the host after the unload".
- Tests: `gpu_reserve_unload_hostram_test.go`, `gpu_hostram_await_test.go`, `internal/gpulease/hostram_await_test.go`.
### Fixed — a card-only generate-image, edit-image and batch answer names the two cards it ran on

Only the queued answer of a call on the card-only placement carried the leased cards (`devices`); the answer of a call that ran, the one a caller reads, did not.
The grant now holds the pair's lease ids (`mediaGrant.Devices`), set where the pair is granted: `grantCards` for a plan the placement named (`mediaPlan.pair`, set by
`planMulticard`, so a pool or a declared lease that holds two cards is not it) and `acquireInheritedPair` for a parent lease on exactly two non-display cards. The
success answers of `runGenerateImage` and `runEditImage`, and every `ImageBatchItem` rendered here, add them as `devices` (the key and the lease-id spelling the
queued answer uses); the CLI's batch output layers the item's own JSON under its row, so `generate-image --batch` prints them too. A grant without the pair adds
nothing: every other call, with or without `imagegen_placement` / `gen_edit_placement` set to `single`, answers byte for byte as before. Documented in
`docs/systems/media-generation.md` ("What the typed door does").
Red before (the call succeeds, the key is missing): `TestAMulticardImageBindingLeasesTwoNonDisplayCardsAndPassesThePlacement`, `TestAnInheritedLeaseOnTwoCardsServesAMulticardImageCallOnBothCards`,
`TestANamedFamilyThatAsksForMulticardLeasesThePairAndTheDefaultStaysOnOneCard`, `TestTheEditDoorLeasesTwoNonDisplayCardsForAMulticardBinding`,
`TestAnImageBatchOnAMulticardBindingLeasesThePairOnce`. Guards for the other side: a plain edit, the default binding of the same node, a pooled two-card call and a batch
without the placement carry no `devices` key (`TestAnImageBatchWithoutThePlacementNamesNoCards`, `TestPoolRouteRequestsPoolCards`). Seven mutants of the fix compile and
each turns a named test red (grantCards, acquireInheritedPair, the generate and edit answers, the batch items, a two-card gate, an every-lease gate).

### Fixed — the card-only image and edit placement's review round: a smaller resident share, the canvas cap, a held-back int8 row, the console watch, the tiled decode, the loader preflight and a zero that was never said

Seven findings from the review of the branch, each proved by a test seen failing for the right reason and by a mutant of its fix (the mutant compiles, the test goes
red, the file is restored byte for byte).

- **`--resident-gb` below a recipe's share moves the bytes to the storage card.** Both planners shifted only the compute peak, so `--resident-gb 5` on the fp8mixed
  edit (19.1 GiB DiT, 9 GiB resident, 11.0 GiB on the storage card) declared 0 host RAM and was accepted, although 15.0 GiB does not fit the 14.9 GiB a 16 GB card has left, and
  the whole DiT on one card ran out of memory at load in the benchmark. `recipePeaks` (JS) and `estimatePlaced` (Go) now treat the DiT's total as the invariant: a smaller
  share adds the difference to the storage peak, a larger one does not lower it (no run measured that). The Go declaration keeps the ordinary estimate and the runner
  refuses, naming the card and the figure.
  Red before: `TestTheResidentShareIsReadFromTheImageCall` (a 5 GiB share declared 0.0 GiB with the storage card at 11.0), "a resident share below the recipe's moves the same
  bytes onto the storage card" (`--resident-gb 5` and `0` were accepted).
- **The edit canvas cap is enforced.** `maxMegapixels` was read only by the Go declaration; the runner honours `--megapixels`, `COMFY_EDIT_MEGAPIXELS` and
  `gen_edit_megapixels` verbatim, so a card-only edit pinned at 4 MP held both cards and ran out of memory. `planEdit` now refuses a pinned canvas above the recipe's cap before
  anything starts, and the typed door refuses `gen_edit_megapixels` above it before any lease. The 2511 GGUF edit row, which claimed the 2.0 MP cap while keeping its 1.47 MP
  compute figure, is scaled to the cap like the fp8mixed row (activations 1.9 -> 2.6 GiB, compute peak 4.9 -> 5.6 GiB). Said in the finding and not done: a warning at config
  load, because `internal/config` cannot read the recipe table (`mediacap` imports it); the door's refusal says the same before any lease.
  Red before: "a canvas the call pins above the recipe's cap is refused before ComfyUI starts", `TestAMulticardEditBindingThatPinsACanvasAboveTheRecipesCapIsRefusedBeforeAnyLease`
  (a 4 MP and a 2.01 MP canvas leased the pair), "a row with a canvas cap holds the activations of that cap".
- **The 2.1 int8 convrot row is held back.** It puts a comfy_kitchen int8 DiT on `cuda:1`; the repo's notes record an illegal memory access in that path off `cuda:0`
  (upstream issue 220, unfixed), and the benchmark arms the row cites ran the int8 DiT on `cuda:0` with the text encoder on `cuda:1`, the reverse. A row may now carry a
  `blocked` sentence (`render/multicard-recipes.json`, `Blocked` in `internal/mediacap/multicard.go`, pinned together): `findRecipe` refuses it by name for every caller,
  `FindMulticardRow` does not return it (so the estimate, the doctor and the typed doors see no recipe), and the door's refusal says which row is held back and why. One int8 arm
  with the DiT on `cuda:1` lifts it. Red before: "the int8_convrot recipe is held back" (JS), `TestAHeldBackRowIsNotARecipeAndSaysWhy` (Go), the pipeline refusal case "2.1 int8 DiT, held back".
- **The runners watch the console.** A DisTorch2 out-of-memory error that kills ComfyUI's prompt worker leaves `/history` empty and `/system_stats` answering, so the poll
  waited out its whole budget with both cards leased. `generate()` (`comfy-render.mjs`) and the new `renderEdit()` (`comfy-edit.mjs`) make the
  console cursor of the pair's instance before the submit and hand the poll a `watch`: `IMAGE-PLACEMENT-OOM` ends the wait at once, with the console line, the recipe and its
  per-card peaks. The pieces are the Wan runner's own (`comfyLogCursor`, the `watch` hook of `pollOutputs`, `oomWedgeLine`, `createOomWatch`), taken from that branch
  verbatim so the two merge as one change. Red before: `console-watch.test.mjs` (the pieces, which did not exist here), "the poll's watch is the placement's out-of-memory watch".
- **A tiled decode is not delivered as a clean success.** ComfyUI retries a VAE decode that ran out of memory tiled, and says so only in its console; the Wan runner reports it,
  the image runners did not. The finished image is now kept beside the requested path as `<name>.tiled.<ext>` and the run fails with `IMAGE-DECODE-FALLBACK` (the Wan runner's
  rule: nothing is thrown away, nothing seamed is shipped as asked); a console that cannot be read to the end says `IMAGE-DECODE-UNVERIFIED` and delivers. The same decode
  fallback on the single placement stays as it was. Red before: "a decode that ComfyUI retried tiled is not written to the requested path".
- **The recipe's loaders are checked before the POST.** `verifyImagePlacement` asked only for `OffloadFreeForDecode`, so the `MISSING_NODE` pack hints the branch added for
  `UNETLoaderMultiGPU` and `CLIPLoaderDisTorch2MultiGPU` were unreachable and a missing loader surfaced as a bare rejected POST after the pair was leased. It now asks `/object_info`
  for the recipe's loader classes (a missing one is `MISSING_NODE` with its pack hint, an unanswered question a refusal), refuses a loader that no longer declares an input the
  recipe sets (`IMAGE-PLACEMENT-NODE-INPUT`: ComfyUI drops an undeclared input without a word, so a renamed `expert_mode_allocations` would run with no split), and the graph's classes,
  the LoRA loader included, are preflighted just before the POST. `comfy-nodes.mjs` gains `fetchNodeInfo`, which `missingNodeClasses` now rides on. Red before: "verifyImagePlacement asks for the recipe's own loader classes too" and its three siblings.
- **A measured card-only zero is said.** `declaredRAM` logged only a figure above 0 and `gpu reserve` printed only one, so the zero a measured recipe earns was never explained
  anywhere (the docs promised it "declares 0, and says so"), and `calibratedRAM` leaves 0 alone, so nothing could correct a false one. `hostneed.Need` gains `CardOnly`; the
  pipeline's log (once per key) and `gpu reserve` print the zero with its reason. A card-only image run also records its VRAM peak and host growth under its own footprint key
  (the quant carries `+multicard`) instead of the single-card one, whose p90 a declaration is raised to. Red before: `TestOnlyAMeasuredCardOnlyDeclarationIsMarkedCardOnly`,
  `TestAMeasuredCardOnlyEditCallSaysItDeclaresNothing`, `TestAMeasuredCardOnlyDeclarationOfZeroIsSaidInTheLogOncePerKey`, `TestTheCardOnlyPlacementHasItsOwnFootprintKey`.
- **Not fixed, said once.** The typed edit door passes no sampling, so it records no footprint or host peak and nothing cross-checks a measured zero against the host's real
  growth; the single-card edit route never did either, and sampling a door is a feature, not a fix of this branch (open, in the report). The new watch, verdict and input
  check are proved against fakes and the benchmark's log lines, not a live wedge.
- **Testing seam.** `generate()` (`comfy-render.mjs`) and `renderEdit()` (`comfy-edit.mjs`) take their outside world as an `io` object whose defaults are the real calls, the way
  `comfy-video.mjs` does, so `multicard-image-run.test.mjs` pins the order of a run (verify, stage, preflight, cursor, submit, poll, write, finalize, unstage) instead of grepping
  the source. Without a placement the order and the poll's options are today's.

### Added — `--placement multicard` for Qwen-Image generation and edit: the text encoder and VAE on one leased non-display card, the DiT on the other, and the typed image and edit doors that lease the pair (`imagegen_placement`, `gen_edit_placement`)

- **What it is.** The Wan card-only placement (below) generalised to the image routes. `comfy-generate.mjs`, `comfy-render.mjs` and `comfy-edit.mjs` take
  `--placement single|multicard`, `--resident-gb` and `--cards` (the Wan flags without the `wan-` prefix). Under `multicard` the loaders of the graph are the
  ComfyUI-MultiGPU ones, per the recipe of `render/multicard-recipes.json`: the text encoder and VAE on `cuda:0` (storage), the DiT on `cuda:1` (compute) whole or
  split by a DisTorch2 allocation (`cuda:1,<N>gb;cuda:0,*`), all through the pair's keyed ComfyUI instance and the read-only safetensors reader the Wan
  placement ships. Five recipes: Qwen-Image-2.1 int8 and bf16, Qwen-Image 2512 Q5_1 GGUF, Qwen-Image-Edit 2511 fp8mixed and Q5_1 GGUF. krea2, 2512 bf16,
  sdxl, hidream, the 2.1 edit, an int8, int4 or nf4 text encoder, a text encoder the recipe was not built for and a `--graph` file are refused by name before anything starts.
  `single`, the default, is the graph, argv and launch it was (the fourteen graphs built before are pinned node for node by a golden test).
- **Measured and predicted, said per row.** Only the 2511 fp8mixed edit was measured with the read-only loader (145.5 s at 1.47 MP on two 16 GB cards, 1.87 GiB of
  host growth). Every image arm of the 2026-10-11 benchmark ran the stock loader, so the other four rows are `predicted` (`basis` in the JSON, `Measured` in
  `internal/mediacap/multicard.go`, pinned together by a test) and declare the ordinary host-RAM estimate; a live run with the read-only loader promotes a row.
- **The typed doors.** `imagegen_placement` and `gen_edit_placement` (`""`/`single`, or `multicard`; unset by default, family-overlay keys) take a new need kind in the media
  admission: the box's TWO non-display cards, named as a set, never the display card (at the desk or away) and never a card whose display state is unknown. A held card of
  the pair is a place in line (the token names both); two multicard calls serialize and a multicard call never shares a card with a single-card one; the batch
  takes one pair. A box without exactly two such cards, a host that does not lease cards and an unreadable card table answer `gpu_lease_unavailable` with the
  reason and the way out (`gpu reserve --devices`), and run nothing: never one card, never the whole node. A binding with no recipe is refused before any
  lease, naming the key. The runner gets `--placement multicard` and `GPU_LEASE_DEVICES`, and no per-card instance, pin, dynamic VRAM or `--pool-*`. Under
  a parent lease the call runs on both its cards if it holds exactly two non-display cards, and is refused with the reason otherwise. The lane probe and the
  place-in-line keeper read the same plan, and the differential table of lane states gains the card-only states. A binding without the key takes exactly the lease,
  argv and declaration it always did (pinned).
- **Host RAM.** `hostneed.estimatePlaced` sizes the declaration on the pair (`Facts.Placement`, `ResidentGiB`, `Megapixels`): 0 only for a measured recipe whose
  per-card peaks fit the two cards less `--reserve-vram`; one card, three, no fit, an edit canvas over the recipe's cap, a predicted row and no recipe keep the
  ordinary estimate, with the reason in the declaring line. `gpu reserve` around the three runners reads the same recipe from their flags.
- **Config.** Both keys are validated (`""`, `single`, `multicard`), carried by named-family overlays, and warned about at load when the placement cannot work: no recipe for
  the family, the sd.cpp engine, pool keys that are ignored, a `comfy_cuda_device` pin or `comfy_dynamic_vram` it does not apply, a host with card-scoped leases off.
- **Not measured.** The read-only reader on the int8 convrot DiT and the bf16 text encoder (the tests use fakes of the loaders); the bf16 split shares and the 2512
  Q5_1 peaks (arithmetic from the benchmark's arms); the port fallback of a pair instance with neither `--api` nor `COMFY_API`; the installed ComfyUI-MultiGPU is a newer
  commit than the benchmark's pin (the loader classes the recipes name are present); the lane probe covers the image route, not the edit route; a box with three
  non-display cards is refused until `gpu reserve --devices` names the pair.
- **Docs.** `docs/systems/media-generation.md` ("Card-only image and edit"), `render/README.md`, ADR 0039 (Amendment 2026-10-11).

### Added — `comfy-video.mjs --wan-placement multicard`: the card-only Wan 2.2 placement on the two non-display cards of a lease, with no host RAM

- **What it is.** One Wan 2.2 14B fp8 expert is 13.3 GiB and a film-size clip (720x1280x81) needs 12.2 GiB more for the sampler beyond the 3 GiB of the
  expert kept resident (the film-size run's compute-card peak, 15.18 GiB, less those 3; the 4-step benchmark before it had read 9.7 to 11.0), so
  the clip fits no single 16 GB card, and the single-card way out (ComfyUI's dynamic VRAM) streams the experts through host RAM and costs
  the host the whole file. `--wan-placement multicard` spends a second card instead: `cuda:0` stores the text encoder, the VAE and the
  donor blocks of the expert in use, `cuda:1` computes with 3 GiB of the expert resident (`--wan-resident-gb`), and each expert loads
  through DisTorch2 with `cuda:1,3gb;cuda:0,*`. `single` stays the default and its graph, argv and launch are byte for byte what they were
  (pinned by tests). Measured on two RTX 5060 Ti 16 GB, 2026-10-11: a 4-step benchmark of the film recipe (250 s per step, 13.3 and
  11.1 GiB, 2.3 GiB of host growth), then the full film-size clip in one media lease with `--ram 0`, run on commit 090fbd95, the tip of this branch before
  the second review round (that round's fixes were not re-run on the cards): 720x1280x81, 20 steps, plain decode, 250.5 s per step on both experts, runner
  wall 5136 s (85.6 min), the plain decode 31.5 s after the free node released both cards in 1.1 s; 15.18 GiB peak on the compute card (95.3 %, with 3.0 GiB
  of the expert resident; 14.9 steady) and 10.86 GiB on the storage card; host growth 2.33 GiB steady beyond the VRAM charge. Frame check CLEAN: the chroma
  step into the joins 24, 48 and 72 is z 1.92, 1.17 and 0.0 where the tiled decode measures z 6 to 18, the frame differences at the joins 24 and 48 are 0.69x and
  0.82x the clip's median, no frozen or repeated frame, the walk continuous through both joins. The
  Qwen-Image-Edit 2511 call at the film's full preset also runs on the pair (9 GiB of the DiT resident on the compute card, 15.98 s per step, 10.5 and
  10.6 GiB per card).
- **The two cards are the lease's.** They come from `GPU_LEASE_DEVICES` (else the lease's record); `--wan-cards <a>,<b>` names them only
  when no lease does, and under a lease it can only confirm them (a card the lease does not hold is refused). They are read against the
  driver's card table and refused unless there are exactly two, neither drives a display (`display_active` or `display_attached`) and
  the driver reports the display state of both (an unknown one is refused, never assumed screenless). The larger card computes; ComfyUI
  numbers its cards in the order of `CUDA_VISIBLE_DEVICES`, which the runner sets from the two UUIDs storage first (of two equal cards the
  higher UUID computes, the order the lease sorts in), and the running
  ComfyUI's device list and the harness's node pack are checked before anything is submitted.
- **The launch.** The runner starts its own keyed ComfyUI (`mc-<uuid heads>`) with `--gpu-only --disable-pinned-memory`, the harness's
  node folder through `--extra-model-paths-config` and the custom-node whitelist the measurement ran with. The caller's
  `COMFY_CARD_UUID`, `COMFY_CUDA_DEVICE` and `COMFY_DYNAMIC_VRAM` are dropped for that launch (a single-card pin and dynamic VRAM
  contradict it), and `--wan-loader` is not read (one stderr line when `native` was asked). `ensureComfy` takes a `launch` and records
  the pair in its launch marker, so a running ComfyUI is reused only when it is this instance on this pair in this order. Refused before
  the lease slot, llama-swap or ComfyUI are touched: `--fast`, a GGUF or unnamed expert (`--high-unet` / `--low-unet` are required), another
  family, a `--graph` file, a `--wan-resident-gb` that one of the two cards cannot hold at the call's geometry (3.2 GiB of an expert at most at film size, from the film-size
  run's measured compute-card peak; the storage card must hold the rest of the expert beside the VAE), a UUID prefix that names two cards, and the placement's own flags lost to an `=` (`--wan-placement=multicard`, or `--seed=7 --wan-placement multicard`: the runner's
  parser does not split it while the lease sizing does; write each flag as `--name value`).
- **`render/comfyui-ext`, the harness's own ComfyUI code (the shared install is not written to).** A read-only safetensors reader
  (`OFFLOAD_RO_LOAD=1`): stock ComfyUI maps a safetensors file copy-on-write, which Windows charges to commit in full (+13.3 GiB per
  expert); mapped read-only it charges nothing. And `OffloadFreeForDecode`, a pass-through latent node between the last sampler and the
  plain `VAEDecode`: it saves the sampled latent, frees both cards of the experts (with `--gpu-only` ComfyUI's own unload frees nothing; the
  experts leave when their patchers lose their last reference), logs each card's memory, and stops the run with `OFFLOAD-DECODE-NOFIT`
  and the numbers when the decode card cannot hold a plain decode, because ComfyUI would retry it tiled with its own temporal chunking,
  the seam the plain decode exists to avoid. A plain decode whose console shows that retry fails the run (`WAN-DECODE-FALLBACK`, the clip kept beside the requested path).
  `--wan-decode tiled` and `auto` keep their meaning, and a tiled decode under multicard says once where its temporal tiles meet (frames 24, 48 and 72 at the film's size). The
  runner asks for the pack's node before every run, under any decode (`WAN-PLACEMENT-PACK`), because the reader that keeps the experts out
  of host commit lives in the same pack; a missing-node message for the harness's own pack says it failed to import, not "install it".
- **Host RAM.** `gpu reserve --devices <a>,<b> -- node render/comfy-video.mjs ... --wan-placement multicard` declares 0 when each leased
  card fits its own stage (`hostneed.estimateWith`, new `Facts.CardsGiB` / `LeasedCardsGiB`): the larger card, the resident share of the
  expert (`--wan-resident-gb`, default 3) plus 12.2 GiB of film-size activations and a chosen 0.5 GiB margin for the CUDA context, the other the rest of the expert
  plus 0.8 GiB inside its size less `--reserve-vram`; one card,
  three, no card table, no fit, another family or no flag keep the ordinary estimate. The pipeline's media admission has no multicard door
  yet (every typed door leases one card).
- **Tests.** 45 new render tests (44 in `wan-multicard.test.mjs`, and `comfyui-ext.test.mjs`, which runs the 14 Python tests of the node
  pack) and the host-RAM tests, written red first, and mutation-checked; the review round of 2026-10-11 (the entry below) found mutants that survived and closed them. The one
  existing test that pinned `generate()`'s source takes the plan as a fourth argument. An independent review before the first run found
  the `=` spelling, the pack check under the tiled decode, `--wan-cards` over a lease, late refusals, an unknown display state and a
  sum-based host-RAM fit; each is fixed here. Docs: `docs/systems/media-generation.md`, "Card-only Wan".

### Fixed — the card-only Wan placement, the second review round: what the merged branch got wrong or could not prove

An independent review of the merged branch found sixteen things (two HIGH, six MEDIUM, eight LOW; three pairs were the same defect reported
twice). All are fixed or answered here. Each change of behaviour was written red first; the findings that are about a missing test (the Wan-only
guard's test, `main()` and `generate()`, the host-RAM branch that reads the card table) are pins that pass on this tree, each proved by the one-line
mutant that makes it fail. Every fix was mutation-checked: reverted in turn it fails a test it names, the mutant parsing or compiling, the restore
byte-identical.

- **A flag spelled with `=` that swallowed the placement's own flag, in the `=` spelling (HIGH).** `--seed=7 --wan-placement=multicard` and
  `--steps --wan-placement=multicard --wan-decode plain` reach this runner's parser (which does not split `=`) as the value of the flag before
  them, so the placement flag was lost and the single placement ran, while the lease sizing (which does split it) declared the card-only 0 GiB: no
  launch, no `--gpu-only`, the experts streamed through host RAM under a lease that declared none. `refuseEqualsSpelling` now matches the
  swallowed token as `--wan-x` and as `--wan-x=...`, for all three flags.
- **An out-of-memory wedge ComfyUI does not report (HIGH).** After an OOM inside the DisTorch2 distribution ComfyUI's node handler logs `Got an OOM,
  unloading all loaded models`, the unload runs out of memory as well and the prompt worker thread dies; `/history` stays empty, `/system_stats` and
  `/queue` keep answering and the lease keeps beating, so the poll waited out its whole budget with both cards leased. `pollOutputs` takes an optional
  `watch`; the multicard run hands it a reader (`comfyLogCursor`, made before the prompt is submitted so an earlier run's lines in a kept instance's
  log do not count) of the instance's console that matches only the two lines that mean the worker is gone, never the recoverable ones in the same
  log (`WAN-OOM`; a console it cannot see is said once, `WAN-OOM-WATCH`).
- **The wait budget (MEDIUM, reported twice).** The 20-step film recipe was projected at about 5,170 s (the film-size run then took 5,136 s of runner wall) and the single-card default left 4.5 percent. A
  card-only run waits 7,200 s unless `--wait-sec` or `COMFY_WAIT_SEC` says otherwise, and says which budget it used (`WAN-WAIT-NOTE`, which names 5,170 s as the projection and 5,136 s as the measured runner wall).
- **The resident-share guard (MEDIUM, reported twice) and the storage card (LOW).** The guard admitted 5.1 GiB at film size where the benchmark says 4 is
  too thin, and scaled the activations linearly where the benchmark measured about twice that at mid sizes. It now holds both cards to the figures the
  lease declares against (the film's activations, the benchmark's reserved allocator peaks as an envelope below it, the storage card's 13.4 - share + 0.8
  GiB inside its size less `--reserve-vram`): 3.9 GiB at most at film size on the 11.0 GiB and the reserve of that round, 3.2 GiB on the film-size run's
  12.2 GiB and the 0.5 GiB context margin of the entry below. A pair such as 24 + 8 GiB is refused before anything starts, with the share that would fit.
  A Go test fails when the runner's and `internal/hostneed`'s figures drift apart. A UUID prefix that names two cards is refused (it resolved to the first).
- **A plain decode that ComfyUI retried tiled (MEDIUM).** It wrote the seamed clip to the requested path and exited 0, from the tail of a log that a
  kept instance shares with earlier runs, and read an unreadable log as "no fallback". It is now judged by this run's console: the clip is kept as
  `<name>.seamed<ext>`, the latent is named and the run fails (`WAN-DECODE-FALLBACK`); an unreadable console says `WAN-DECODE-UNVERIFIED`.
- **The tiled decode's seams (LOW)** were named as "frames 24 and 48"; `tiledSeamFrames` reproduces ComfyUI's tiling (24, 48 and 72 at the film's size)
  and the note names the boundaries of the clip being made.
- **A failed prompt's exception message (LOW)** was cut by a 500-character slice of the status; it now reaches the error whole, for every runner that
  polls through `comfy-submit.mjs`.
- **Equal cards (LOW)** took their roles from the order they were named in, so `--wan-cards 0,2` gave the opposite of the lease's split; the lower UUID
  now stores, as the lease's sorted order did in the benchmark. The runtime device check also compares the model name (the display card is another
  model of the same 16 GB class) and its docstring says what it cannot prove (two cards of one model).
- **`--enable-dynamic-vram` in `COMFY_EXTRA_ARGS` (LOW)** re-enabled streaming beside `--gpu-only`; it is dropped from a multicard launch, and a
  running ComfyUI whose argv turns dynamic VRAM on is not reused.
- **Tests that could not fail (MEDIUM, LOW).** `TestTheMulticardFlagMeansNothingToAnotherFamily` used two 16 GiB cards where the Wan-only guard changes
  nothing (it passed with the guard deleted); it now uses two 24 GiB cards. The branch of `resolveReserveHostRAMWith` that reads the card table itself
  had no test. `main()`'s wiring and `generate()`'s post-run steps had none: `planRun` and `generate` take their calls from tables and
  `render/wan-multicard-run.test.mjs` drives them (seven one-line mutants the reviewer found surviving are killed; the source pin of `generate()`'s order is
  a behaviour test now). 34 new render tests (33 net: a source-text pin was replaced by a behaviour test) and 2 Go tests.
- **Answered, not changed.** The reviewer's claim that the CHANGELOG's "every mutant fails a test" was overstated was right for the first round; that
  sentence now points here. No live run was made in this round: everything above is proved with fakes and with the benchmark's logs, not on the
  cards. The 20-step acceptance run is the one in the entry above, made on 090fbd95, the tip before this round (720x1280x81, 20 steps, plain decode,
  250.5 s per step, 5136 s of runner wall, host growth 2.33 GiB, frame check CLEAN, 15.18 GiB on the compute card and 10.86 GiB on the storage card); this round's
  changes were not re-run on the cards.

### Fixed — the card-only Wan placement follows the film-size run: 12.2 GiB of activations, a 0.5 GiB context on the compute card, 3.2 GiB of share at most

The independent check of the fix round found that the run's numbers contradicted the guard's. The film-size run (commit 090fbd95, the tip before the
second review round) peaked at 15.18 GiB on the compute card with 3.0 GiB of the expert resident, so the sampler side is 12.2 GiB, not the 11.0
(the top of the 4-step benchmark's 9.7 to 11.0, which ended at about 1000 s, before the peak at about 2485 s). The acceptance criterion follows the
measurement, in both places that draw this line.

- **The figure.** `WAN_FILM_ACTIVATIONS_GIB` in `render/wan-multicard.mjs` and `wanActivationsGiB` in `internal/hostneed` are 12.2 (a Go test pins them
  together, and the new `WAN_COMPUTE_CONTEXT_GIB` / `wanComputeContextGiB`, 0.5, the same way `wanResidentDefaultGiB` is pinned).
- **The compute card's margin is the CUDA context, not `--reserve-vram`.** Both the runner's resident-share guard and the lease sizing held the
  compute card to its size less `--reserve-vram` (1.0 GiB by default), ComfyUI's model-load margin and not an activation ceiling. With 12.2 GiB
  that refuses the measured call: 15.9 - 12.2 - 1.0 = 2.7 GiB against the runner's own default of 3 (the guard), and 3.0 + 12.2 = 15.2 > 14.9 (the
  sizing, which would then declare the whole 33 GiB set for a call measured to run with `--ram 0`, and the host-RAM brake holds a grant that size back
  for hours). The compute card now needs the share + 12.2 + a 0.5 GiB context margin (chosen, not measured: the 12.2 already holds a context, and only a 3.0 GiB share was ever run) to fit its size: 3.2 GiB of share at most at film size on a 15.9 GiB card,
  and the guard and the sizing admit the same shares (a Go test draws the ceiling from the runner's constants and checks both sides of it).
  `--reserve-vram` is the storage card's margin and nothing else; that check is unchanged (the run's 10.86 GiB sits inside the 11.2 it predicts).
- **Red before.** The new render assertions failed with the figure at 12.2 and the old margin (the measured recipe refused, `at most 2.7 GiB`); the Go
  tests failed with 11.0 (a 3.3 GiB share declared 0 where the runner refuses it, the mirror test saw 12.2 against 11) and, with only the constant
  raised, with the measured call itself declared as the ordinary 33.0 GiB. Mutants, each compiling and restored byte for byte: the Go figure back to
  11.0, the old reserve check, no context term, a 0.4 context, the compute check against the smaller card, no reserve on the storage card (red in 5, 5,
  4, 4, 2 and 1 Go tests); the render margin back to `--reserve-vram` (red).
- **Behaviour change to know.** A `--reserve-vram` above the default no longer shrinks the compute card's share (`--reserve-vram 2` with the default
  share used to be refused at film size; the film-size run itself ran at the runner's default reserve of 1.0, so that a larger reserve leaves the compute card's peak where it was is assumed, not measured); it shrinks the storage card's room, as before.
- **A clip bigger than the film's is said to be unsized.** The planner holds the sampler's activations at the film-size run's 12.2 GiB from 75,600 tokens up, because nothing
  above the film's size was measured, while the activations are expected to grow with the tokens. It still admits such a clip (the documented intent is that a bigger clip may run), but prints `WAN-GEOMETRY-NOTE` before the resident-share
  check: the clip's size, the film's, the figure it holds, what a proportional extrapolation gives (about 32.0 GiB at 1280x1280x121), and the advice to lower `--wan-resident-gb`
  and to pass `--wait-sec`. Nothing is said at the film's size or under. The lease sizing reads no geometry and declares against the film's figure for any clip (its comment says
  so). Red before: the new note test and the `WAN_FILM_TOKENS` test failed (no note, no constant); mutants, each parsing and restored byte for byte: the note never said, said only
  after the check, said at the film's own size, an always-empty note, the extrapolation unscaled, the film's token count off by one, the `--wait-sec` advice dropped (red in 1 to 2 tests each).

### Fixed — `gpu reserve` says why its declared figure is the media class default, and that `--ram` states the real one (F71)

- **The defect (register F71).** A `gpu reserve` around a command the estimator does not recognise (a driver script that runs several
  renders, a Python wrapper), around a recognised helper that runs its own `--graph`, or around a recognised helper whose weight files
  cannot be sized, declares the media class default: the largest ComfyUI family the box binds, counted in full as if it streamed from RAM
  (Qwen-Image 2512 on the reference box: 38.0 + 8.8 = 46.8 GiB). The declaring line said "class default" inside its parenthesis and nothing
  else, so a caller whose weights sat on the cards (the card-only Wan placement above) could not tell that the figure was not theirs,
  declared 46.8 GiB, and waited a queue cycle for host RAM it never used.
- **The fix.** The declaring line is unchanged. When its figure is the class default and the operator gave no `--ram`, ONE more line
  follows it with the reason (no command is wrapped, as in a `--detach` hold; the wrapped command is not a recognised render helper, the
  helpers named from the table that recognises them; a recognised helper runs its own `--graph`; or a recognised helper's weight files
  could not be sized) and the way out: `--ram <GiB>` states the real figure, `--ram 0` when the weights sit on the cards. No line when the
  operator gave `--ram` (0 included), when the figure was estimated from the helper's own files, for a text lease, or on a box whose class
  default is 0. The line never contains the word the declaring line is grepped by, and no figure, lease record or queue behaviour changes.
  `internal/hostneed` gains `RenderHelpers` and `RenderCall.RunsOwnGraph`, the two answers the line reads (`Estimate` asks the same
  function about `--graph`, so the line and the estimate cannot disagree).
- **Tests.** `gpu_hostram_hint_test.go` (seven: the four reasons, the cases that print nothing more, the invariant that the hint changes
  no figure, and one through `runGPUReserve` on stderr) and `internal/hostneed/helpers_test.go` (two), written first. Red before: the five
  that pin the line failed on the tree without the fix, each with "want the declaring line and exactly one line of why, got 1 line(s)"; the
  other two hold before the change and guard it. Mutation-checked: fourteen changes (ten to the line and its guards, four to the two
  hostneed answers), each reverted in turn fails a test it names. Docs: `docs/systems/gpu-lease.md` ("Host RAM"),
  `docs/systems/media-generation.md` ("Card-only Wan") and `render/README.md`.

### Added — the single placement of the Wan graph is pinned to the graph the builder made before placements existed (no product change)

- **Why.** The merged `wan-multicard.test.mjs` compares `placement: "single"` with the default placement, which shows the two agree with
  each other and not with what was built before: with the single graph's stock `VAELoader` replaced by the multi-GPU loader, every test of
  the four files that cover the builder and the scratch scan still passed. A differential run of the base builder against the merged one over
  103 argument sets (309 builder comparisons, 24 decode-chooser cases and the exported constants) found no difference.
- **The pin.** `render/wan-single-golden.test.mjs` builds nine graphs (GGUF and fp8 experts; the auto, native and forced DisTorch2 loaders;
  tiled, plain and auto decode; fast; an upscale; a short clip with a negative prompt, steps, cfg, boundary and shift) with no placement
  argument, with `placement: "single"` and with a `residentGb` the single placement must ignore, and requires the graph held in
  `render/testdata/wan22-single-graphs.json` (made by the builder as it stood at the 0.181.0 release tip) and no warning. The VAE and
  text-encoder loaders swapped for their multi-GPU twins in the single graph each fail all nine of its graphs (the VAE one passed the old tests).
### Fixed — nothing the harness or its tests start opens a console window on the desktop

- **The defect.** On Windows a console program started by a process with no console of its own (a runner or a test child started
  detached, a service, a launcher started hidden) gets a NEW visible console window unless it is started hidden, and with Windows
  Terminal as the default terminal that window is a Windows Terminal window on the operator's desktop. A window watcher saw them
  while the render suite ran: Windows Terminal hosting windows and one titled `taskkill.exe`. The cause was 86 child-process calls in
  `render/` that passed no `windowsHide`: 25 in production modules (the engine's `killTree` `taskkill`, its `ps` probes, ffmpeg and
  ffprobe in `audio-qa`, `igpu-qa` and the animate/video runners, the `nvidia-smi` and sd-cli listings, the attached ComfyUI launch,
  the manifest installers, the tts and sdcpp attached runs) and 61 in the tests and their support, among them the scripts the tests
  write into a string and run in a detached child. On the Go side the `taskkill` calls (`gpugen`, `mediaops`, `nodeswap`), the starts
  of the render runners and the tool probes (`nvidia-smi`, ffmpeg, the hardware detectors) were built with a bare `exec.Command`,
  and the accelerator sidecar launcher set `HideWindow` but not `CREATE_NO_WINDOW`. Three more places started console programs the
  same way and were found by the review of this fix: the Windows cage (`internal/sandbox`), which the agent's `run` tool reaches
  whenever `--allow-run` is set, with `go`, `python`, `node`, `npm`, `cargo` and `git` on its allow-list; the children of 48 Go test
  files (ffmpeg, ffprobe, python, node, the built binary itself); and the companion-CLI runner of the two printed CLIs under `tools/`.
- **The rule in Go (`internal/winexec`): a hidden child stays on its parent's console unless the parent has none.** `HideWindow`,
  `Command` and `CommandContext` always hide the child's own window (`SW_HIDE`) and add `CREATE_NO_WINDOW` only when
  `GetConsoleProcessList` says this process has no console to hand on (a hidden console, which `GetConsoleWindow` would miss,
  counts as one). The first version of this fix put `CREATE_NO_WINDOW` on every child, which moves the child to a console of its own:
  the operator's Ctrl-C and a closed terminal no longer reached it, so an interrupted `local-offload` (the `npm ci` of
  `install hyperframes`, any media runner on a verb without a signal handler) left a hidden process running with no window to show it.
  A window can only appear when the parent has no console, so that is the only case that needs the flag. Two siblings cover the
  children that are not simply waited for: `HideWindowDetached` / `CommandDetached` always give the child a hidden console of its own
  (the detached GPU holder, which must survive a closed terminal, and the accelerator sidecar launcher), and
  `HideWindowIfNoConsole` / `CommandIfNoConsole` leave the command an operator names after `--` in `gpu reserve` exactly as before
  when there is a terminal (it may be interactive or open a window they want) and hide it when there is none, which closes the
  exemption that let a wrapped console program open a window under a console-less parent. The tests start real processes (helpers
  with a hidden console or none at all, so none of them can open a window) and read `GetConsoleProcessList` in the child.
- **The cage.** `sandbox_windows.go` called `CreateProcessAsUser` with `CREATE_SUSPENDED|CREATE_UNICODE_ENVIRONMENT` and
  `STARTF_USESTDHANDLES` only. It now also passes `CREATE_NO_WINDOW` and `STARTF_USESHOWWINDOW` with `SW_HIDE`: the caged command
  has every handle piped and is ended by its job object, never by a Ctrl-C, so it is always alone on a hidden console. An earlier
  version of this entry said the cage was Linux only and untouched; that was wrong.
- **The fix in `render/`.** `windowsHide: true` on all 86 calls, every other option unchanged (in `manifest-satisfy.mjs` the flag
  follows the caller's spread, so a caller cannot drop it; in `igpu-engine.mjs` and `igpu-qa.mjs` it now comes after the timeout
  spread). The Windows stub launcher that `igpu-stub.mjs` compiles starts its target with `CREATE_NO_WINDOW` and `HideWindow` as
  well: a launcher started detached has no console to hand on, and the node child it starts would open a window of its own.
  `comfy-keep.test.mjs` had pinned the defect (`windowsHide` undefined for the attached ComfyUI launch); it now asserts the house rule
  for that launch too. `windowsHide: true` does not move a child that inherits stdio off the operator's console (libuv adds
  `CREATE_NO_WINDOW` only when no stdio is inherited), so the attached sd-cli, tts and comfy-render runs keep their Ctrl-C;
  `render/console-share.test.mjs` measures it with real processes and pins that the three runs inherit stdio.
- **The fix for the Go tests and `tools/`.** 73 constructors in 48 Go test files of the root module (plus 3 in `internal/winexec`'s
  own tests) are `winexec.Command` / `winexec.CommandContext` now, and `cmd/leakdigest` builds its `git` children the same way, so it
  needs no exemption. `tools/comfyui` and `tools/llamaswap` (separate modules, which cannot import `winexec`) hide the
  `cobratree` companion-CLI runner through a `newCLICommand` + `hideWindow` pair and hide the `go build` of their test helper;
  the two edits to generated `shellout.go` files are recorded in each tree's patch register (`0006-hide-companion-cli-window.patch`,
  `internal-mcp-cobratree-shellout.go.md`), with the upstream template change still owed. `glueOpenBrowser` builds one command
  instead of three, so its single hide call covers it.
- **Locked in.** `render/no-visible-windows.test.mjs` (scanner: `render/spawn-scan.mjs`) reads every source under `render/`, the
  scripts held in strings included. The options of a `spawn`, `spawnSync`, `execFile`, `exec`, `fork`, `*Sync` or injected
  `spawnImpl` call (the last argument that is not a callback) must be an object literal whose own `windowsHide` ends up `true`, with
  no spread or computed key after it, or a name that resolves in the call's scope to a `const` object literal that does. The scanner
  follows `import cp, { spawn }`, `{ spawn } = cp`, `= require("child_process").spawn`, backtick specifiers, `spawn?.(`,
  `spawn.call(`, aliases chosen by `?:` / `??` / `||`, and `promisify(execFile)`; a child_process function or the module used as a
  value (`use(spawn)`, `.bind`, `.apply`) is refused. `TestEveryChildProcessStartIsHidden` (`internal/winexec`) reads every Go file
  of the repository that builds for Windows, tests and the `tools/` modules included, and fails on a call of `exec.Command` /
  `exec.CommandContext`, one of them used as a value, an `exec.Cmd` literal, `os.StartProcess` / `syscall.StartProcess` or a
  `CreateProcess*` call unless its own function hides it (a hide call per start, or both `CREATE_NO_WINDOW` and `SW_HIDE` for a
  `CreateProcess*`) or it is listed with a reason; two `internal/winexec` test helpers are the only entries.
- **Red before.** The render lint fails on the 0.181.0 tree with 86 calls; the first version of the Go lint failed with 31
  `file:function` sites. Against the reviewed head the scanner's six new test groups (every call shape above) fail on the old
  scanner, and the stricter scanner flagged two real sites (`igpu-engine.mjs`, `igpu-qa.mjs`: a spread after the flag). The extended Go
  lint listed 75 `file:function` sites (87 starts: 80 in 53 test files, 7 in production files), and the new `winexec`, cage and
  `cobratree` tests failed on the unconditional helper, the unhidden cage and the unhidden runner. Mutation checks, each red with a
  mutant that compiles or parses: for the Go rule, `CREATE_NO_WINDOW` always or never set, dropped from `HideWindowDetached`,
  `SW_HIDE` dropped, `processHasConsole` always true, `HideWindowIfNoConsole` always hiding, `Command` or `CommandDetached` left
  unhidden; for the cage, each of `CREATE_NO_WINDOW`, `STARTF_USESHOWWINDOW` and `SW_HIDE` dropped; for the scanner, 15 mutants (each
  rule switched off); for `console-share`, the inherited probe piped, the piped probe inheriting, and each of the three attached runs
  piped.
- **Live.** `scripts/window-watch-run.mjs` (with `scripts/window-watch.ps1`) polls the desktop with `EnumWindows` (every 30 ms or so) while a
  command runs and exits 3 when a console window appeared; its `--selftest` makes a child create a hidden window and requires the log to
  list it. The review measured 22 new console windows on the 0.181.0 tree in 130 s and none on the reviewed head. At this head, with
  zero new windows each time: the render suite (1325 tests, 1302 passed, 23 skipped, 0 failed; 2765 polls over 119 s; started with a
  hidden console, because `node --test` starts every test file with a plain `spawn` and opened a Windows Terminal window per file when
  it was started with no console at all, an effect of the runner and not of the suite), and the compiled test binaries of 22 Go packages,
  each started with no console of its own (`cmd/leakdigest` and `internal/` `agent`, `audioio`, `comfyinst`, `composebundle`, `datahome`,
  `fakeexe`, `fleetnode`, `gpuactivity`, `gpugen`, `gpulease`, `gpuprobe/smitest`, `mediacap`, `mediaops`, `mediaremote`,
  `pairworkloads`, `sandbox`, `sttremote`, `videoio`, `vllmseat`, `winexec`, `writedoor`; 9,825 polls over 424 s). That run also found a
  defect in a new test: the Ctrl-C test failed in a runner started with `CREATE_NEW_PROCESS_GROUP`, which switches Ctrl-C off for its
  whole subtree; the test's helper now switches it back on. Not watched: the root package, `internal/pipeline`, `internal/mcpserver`,
  the `tools/` modules and the other Go packages, which the lint and the flag tests cover but nothing has watched. The 0.181.0 tree was
  not re-run live, because that would put windows on the screen.
- **Not covered.** The two printed CLIs outside this repository owe the same `shellout.go` patch and the generator template the
  same change (the registers say so). Python and shell programs (the accelerator test servers, the setup browse runner, the
  reference `ffdump.py`) are developer tools run from a terminal and are not scanned.
