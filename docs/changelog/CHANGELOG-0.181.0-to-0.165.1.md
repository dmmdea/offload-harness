# Changelog archive: 0.181.0 to 0.165.1

Verbatim entries moved out of the root `CHANGELOG.md` so the file GitHub renders stays under its size limit.
History, never edited. Relative links inside the entries are written from the repository root.
Newest first; the live changelog is [`../../CHANGELOG.md`](../../CHANGELOG.md).

## [0.181.0] - 2026-10-11 - a busy graph waits as a ticket anywhere in the cluster, image batches run across the fleet, a family is identified by its content, the text lane keeps its structured answers and its place in line, and the cages say what they could not do

### Fixed — a render on one card no longer unloads the seats placed on the others, and a seat that comes back onto a leased card leaves before the next render (F15)

- **The defect (register F15, from the 0.176.0 notes).** The render helper cleared "every non-keep-set model whatever card it sits
  on", and a media lease was said not to be exclusive, so a text seat reloaded under it between renders. The first half traced to
  code: the unload list for a lease on some cards (`gpualloc.UnloadModels`, the rule `gpu reserve --unload-seat` uses) placed a
  model only through `layers` (`Config.ModelPins`), and read everything it could not place as "on the leased cards"; a roster that
  could not be read, or a host with no llama-swap endpoint, left `GPU_LEASE_UNLOAD_MODELS` unset, which `render/gpu-lock.mjs` read
  as today's rule, every model off the memory stack. The installer's tier seeds write no `layers`, so on a box nobody wrote them
  for, every model was "unknown" and a render on card 2 emptied card 0 too. The second half is not a missing fence:
  `blocksLoadOne` already gates a media lease, per card, through `Admit`, `AwaitUpstream` and `AwaitRunSlot`; it never read the
  exclusive flag for it (a text-lease field). Two doors stay open: the holder's own process tree (`insideLease`, by design) and
  clients that are not the harness.
- **A model is placed by `layers`, else by its pin in the serving config.** New `internal/gpualloc/serving.go` reads
  `serving_config_path` (the llama-swap yaml the box runs) and takes, per model id and alias, the env pin
  `CUDA_VISIBLE_DEVICES=<uuid or index>[,...]`, matched against the card table. `LeaseScope.Locate` tries `layers` first, so a model
  both declare keeps the `layers` reading. Only the env pin counts; device flags in `cmd` are never read. A model whose entry names
  no device in env (an unpinned dual-card `-sm layer` / `--tensor-split` entry), a `proxy:` seat (a vLLM engine: its cards are
  pinned in its own unit, not in this file), a pin that is a macro, an alias two entries claim with different pins, and a bare
  index when the same entry does not also set `CUDA_DEVICE_ORDER=PCI_BUS_ID` (without it a CUDA index is not an nvidia-smi
  index) stay UNKNOWN, each with its reason. The `layers` path trusts a bare index without that check; the serving-config path
  is stricter on purpose, and its cost is only that an index pin without the order variable is unloaded instead of kept. In
  `setup/templates/profiles.json` the multi-card tiers (`blackwell-2x16`, `blackwell-3x16`) carry `CUDA_DEVICE_ORDER=PCI_BUS_ID`
  in `gpu_env`, which the renderer appends to every model's env, so an index pin rendered from them places; the single-card tiers
  pin `CUDA_VISIBLE_DEVICES=0` without it, and a lease on their only card is the whole node anyway. Whether the live yaml of any
  box carries it was not checked. If the file cannot be read, or `serving_config_path` is unset, every model `layers` do not declare
  is unknown and the lane says so once.
- **What a lease unloads.** `LeaseScope.Judge`: a model placed on a leased card goes; **a model placed on another card, by `layers`
  or by the serving config pin, stays** (the F15 gain); a model whose placement is unknown goes, exactly as before F15, because
  the render needs its card (a `proxy:` seat with no `layers` entry is unloaded under any card lease); the memory stack never goes.
  That judgement runs on a roster that was READ. A roster that could not be read is not judged at all: see "A card lease gets a
  list" below.
  A lease that names every card the table lists is still the whole node (`holdsAll`: a one-card box's only card, all three of
  three). `Touches` is the same reading, so the drain and the agent seat agree with the unload. The card table unreadable: every
  model is unknown and goes, as before.
- **The lane says what it did and why.** One line per reason, from `gpualloc.Explain`, in the wrapper's stderr and the media
  admission's log: `unloaded, placed on a leased card by layers: ...`, `left resident, placed elsewhere by the serving config
  pin: ...`, `unloaded, placement unknown (it is a proxy: seat, ...): ...`. The render helper logs the models it left on a list.
- **A card lease gets a list, unless the roster could not be read.** `wrapperUnloadEnv` (`gpu reserve --devices`) and the media
  admission's `unloadEnv` export `GPU_LEASE_UNLOAD_MODELS=-` when there is nothing to unload (no endpoint, nothing the verdicts
  select) instead of leaving the variable unset. A whole-node lease has no list, and neither has a card lease whose llama-swap
  roster could not be read: that is not an empty roster, nothing says what is resident, and a list built from the config alone is
  never complete (with `layers` only, a resident model neither they nor the serving config name would never be judged and would
  stay on the leased card; with neither, the list is empty and the lane unloads nothing). `gpualloc.UnloadModels` takes the
  roster read's error and returns no list for it, whatever the config declares, so the variable stays unset and
  `render/gpu-lock.mjs` keeps its own rule, every model off the memory stack, exactly as before F15; the wrapper and the media
  admission log that they fell back because the roster could not be read. A roster that was read and is empty means nothing is
  resident and is judged like any other. `gpu reserve --unload-seat` judges the other residents with the same verdicts.
- **Between renders.** The unload is elected once per lease, so a seat that came back onto the leased cards stayed for the rest of
  the lease. A job that lost the election under a card lease with a non-empty list now reads that list against `/running` again
  before it submits (`withGpuSlot` `reclear`); `freeLlamaSwap` unloads a listed model only when it is resident, so a card that is
  still clear costs two reads and no unload. A whole-node lease, a lease with no list and the none sentinel keep the
  once-per-lease election. A card lease handed no list at all (an older wrapper, a hand-built environment) unloads every model off
  the memory stack, and says that it had no list.
- **Tests, written first and red** (all but one: `TestTheServingConfigPlacesWhatLayersDoNotDeclare` in the pipeline passed against
  the previous positive-only rule too, because that rule also kept the unpinned seat, so it is proven by a mutant only, the one
  that ignores the serving config pins). `internal/gpualloc`: a model nothing places is on the list, the serving config's pins place a
  model `layers` do not (UUID, index with and without the PCI order, alias, a pair spanning the leased card), `layers` outrank the
  serving config, a `proxy:` seat, an entry with no env pin and a cmd-only pin are unknown, an unreadable or unset serving config
  is said once, an unreadable roster makes no list whatever the config declares (nothing placed, `layers` only, `layers` and a serving config) while a roster read and empty still has one, no card table unloads every model, the memory stack stays out
  even when placed on a leased card, a lease on every card is the whole node. Root: the same through `unloadModelsFor`,
  `--unload-seat` (layers and the serving config), the wrapper form end to end, the roster-unreadable and no-endpoint wrappers,
  the unplaceable pin and the unreadable table. Pipeline: an unreadable roster hands the call no list and says why; the serving config
  keeps a model on card C off card A's list. `render/`: a later job re-clears under a card lease with a list and under no other,
  a card lease with no list says so and unloads as before, a list names what stays. `internal/modelaffinity`: a media lease fences
  its own cards through every door exactly as an exclusive text lease does and lets a free card through, and the fence starts when
  the drain ends. Each rule has a mutant that turns its test red (unknown stays, serving pins ignored, the proxy rule, the PCI
  order rule, the serving config outranking `layers`, aliases, the hold-every-card reading, the memory-stack filter, the verdicts
  in `--unload-seat`, the unset variable on a missing endpoint, the roster-error branch of `UnloadModels` and each caller dropping the roster error, the media
  clause, the draining clause and the per-card scoping of the fence, the Node no-list log, the re-clear and its card condition,
  the left-resident log).
- **Not changed, and not verified here.** First, a limit of the fix: the roster read is the one input the unload cannot check. When
  it fails, F15's gain is off for that lease: the render lane keeps its own rule and unloads every model off the memory stack, the
  seats placed on other cards included, and says so; this is the safe direction (a card that stays occupied fails the render, a
  seat that is reloaded costs a warm-up), chosen over a list built from the config, which can leave a resident model on the leased
  card. A roster that was read and is empty is trusted to mean nothing is resident. Second, for a box without `layers`: the unload now knows from the
  serving config which card a model is on, and the load fence does not. The fence (`modelaffinity`, armed from `config.ModelPins`)
  still places a model through `layers` only, so on such a box a text load waits for any media lease, whatever card it is on,
  exactly as before this change; reading the serving config there is a separate change. Also: a client that is not the harness can still load a model onto a leased card; the re-clear removes it
  before the next render, it does not stop it, and it unloads what the first job's unload would have (a model whose placement is
  unknown, in-flight requests drained first, then forced). The serving config is a path in the harness config that no default
  fills in: a box that does not set `serving_config_path` has no model placed by it. Two parallel jobs of one lease can both read the
  same resident model and both ask for its unload (idempotent; the second may log a failed per-model unload). Nothing here was run
  against a live node, a ComfyUI instance or a llama-swap; the render helper was exercised against stand-ins for `/v1/models`,
  `/running` and the unload routes only, and the yaml reader against fixtures shaped like the rendered config.

### Fixed — the ledger row of an adopted ticket named no node (found in the 0.180.0 live acceptance, A6)

- **The defect.** A `placed` ticket whose owner process ended is adopted by the next identical call (ADR 0083). The adopter never
  dispatches, so that its handle opens no second PAIR card, and the node, node id and fleet job id of a remote call's ledger row came
  only from the dispatch: the adopted call's row said it ran remotely and not where. The one other place the node appeared, the
  placement note's `; ran on <node>` tail, does not reach the ledger, which keeps the first 120 bytes of a placement, and the lane
  sentence ahead of that tail (the cards the lane was held on, the holder's reason and declared term) is longer than that. In the
  recorded run the adopted row had an empty `node`, `node_id` and `fleet_job_id`, and a placement cut inside the lane sentence.
- **The fix.** The adopter tells its attribution handle the node and the job the ticket recorded before it collects the result
  (`core.NameNode`, the optional `core.NodeNamer`, `pairworkloads.RemoteCall.Ran`), so the one row carries `node`, `node_id` and
  `fleet_job_id` exactly as a dispatched call's does, and still opens no card (`TestAnAdoptedTicketsRowNamesTheNodeItRanOn`,
  `TestRanNamesTheNodeOnTheRowAndOpensNoCard`). A handle that was dispatched keeps the node it was dispatched to; an adopted job the
  node passed back still writes no row. The placement note is unchanged. A reader that looks for where a remote call ran should read
  `node_id`, not search the placement text.

### Fixed — a nested `output_schema` no longer fails on llama.cpp seats: objects and lists of objects are constrained to their own shape (F43, C12)

- **The defect.** The schema-to-grammar step (`internal/gbnf`) knew no object and no list of objects: `FromJSONSchema` read every
  array as a list of strings and every object as a string, so the grammar a llama.cpp seat was sent forced `"labels":["..."]` where
  the contract's schema wanted `"labels":[{...}]`. The seat obeyed; the schema's validator then rejected all three re-pack attempts
  (`at '/labels/0': got string, want object`), the finished run deferred as `output failed schema`, and the grammar-free chat
  attempt, shown only "array of objects" with no keys, wrote objects with their required keys missing. About 20 jobs in 48 hours on
  four llama.cpp nodes; deterministic for any nested schema. The same flattening sat in `JSONSchema`, the vLLM rendering of the
  extract task's fields, and in the unconstrained-seat shape instruction.
- **The fix.** `Field` is a tree now: `TObject` carries its own `Props`, `TArray` carries its `Items`. `FromJSONSchema` reads a
  nested object's own `properties` to any depth (ordered by that level's `required`, then by name) and arrays of objects or of
  arrays; `Object` writes each as a rule of its own, named by position (`obj-N`, `arr-N`) because llama.cpp's parser takes only
  letters, digits and dashes in a rule name and a key such as `key_facts` is not one. The root production stays on one line (the
  thinking wrapper reads it), and a flat schema compiles byte for byte as before (`TestFlatGrammarIsByteIdentical`). An object the
  schema leaves open is admitted as any JSON object, not a string. `JSONSchema` carries the nested shape and round-trips it
  (`TestNestedJSONSchemaRoundTrips`).
- **The prompts say the keys.** The re-pack prompt, on the grammar lane and the chat lane alike, names each nested object's fields
  with their types (`array of objects, each with fields "name" (string), "score" (number)`), and the type rule tells the seat that an
  object field holds exactly the fields listed for it. The shape instruction for a seat that cannot take a grammar states nested
  keys, and its strict schema enforces them.
- **How it is tested.** `internal/gbnf/gbnftest` is a small reference interpreter of the GBNF subset the package emits: it answers
  "does this grammar admit this text", and refuses a rule name llama.cpp's parser would. The grammar built from a nested schema is
  checked to admit the nested object and to refuse the strings it used to force
  (`TestNestedSchemaGrammarAdmitsTheNestedAnswer`), and the re-pack test runs a seat that obeys the grammar it is sent
  (`TestRepackOnALlamaCppSeatConstrainsANestedSchemaToItsOwnShape`): before the change it failed with the three attempts above, now
  the first attempt delivers. The grammar was not compiled by a live llama.cpp in this change; the first nested request on a seat is
  the confirmation.
- **Still read as strings**, now stated where the schema limits are documented (ADR 0002's amendment, `contracts/README.md`,
  FLEET-NODE.md, the operator guide): a list of numbers, booleans or enum values (quoted by the grammar, converted back by the
  re-pack's coercion), a type list, `anyOf`, `oneOf`, `allOf` and `$ref`. Every property a schema lists is required by the grammar,
  at every depth, whether or not the schema's own `required` names it.
- **The coercion follows the schema to every depth.** The conversion above was promised at every depth and done at the top level only
  (`coerceToSchemaNoted` read the schema's top-level properties and the items of top-level lists), so a numeric or boolean list inside an
  object or inside a list of objects could never validate on a seat that obeys the grammar: every grammar attempt failed `at
  '/meta/scores/0': got string, want number`, and the delegator's one-completion rescue could not deliver such a schema at all. It now
  reads every property and every list item along the schema's `properties` and `items`, with the same lossless rule and the same
  full re-validation, and the note names a nested field by its path (`meta.scores (2 items)`, `rows[].scores (3 items)`, `meta.owner.id`);
  the top level is named as before. Red before: `internal/pipeline/agenttask_repackcoerce_nested_test.go` (the table; the premise that the
  grammar quotes the list and refuses the real one; a seat that obeys the grammar, through `repackStructured` and through a run). Six
  mutations of the fix were each caught.

### Fixed — a structured re-pack is owed the time its own answer needs, so a loop that overran a short wall no longer skips it (F44/C13)

- **The defect.** The wall is an expectation (ADR 0055 item 2): the loop ends on a stall or the ceiling, not on the wall. The auto wall is
  the seat-rate estimate clamped to `[300, 900]` s, and on a slow seat (3 to 6 tok/s) the estimate is 1,500 to 3,400 s with the final answer
  and the re-pack both priced at the 8,192-token cap, so the log line `wall 900 s is BELOW the estimate 3371 s` appeared on 54 of 55 agent
  tasks of one node. The loop ran past the wall, and the structured re-pack, which `repackOpts.fit` held to the wall's end plus the 30 s
  grace, was told `0 s left to the wall + 30 s grace ... buys 0 tokens < the answer's 1463` and skipped with a finished answer in hand: 23
  jobs in 48 h, each a `budget` defer left to the delegator's rescue (the ledger also holds rows where that rescue of such an answer ended `call deadline reached`).
- **The fix.** The bound a re-pack call runs under is the later of the wall's end and `OwedEnd`, the instant the call began plus
  `1.5 x the answer's expected tokens / the seat's decode rate` (`repackOwed`), then the grace; the run's ceiling still caps it. The 1.5
  is `agent.RepackMargin`, the margin the re-pack's own stall allowance pads its generation estimate with (now exported), so no number is
  new. `OwedEnd` is set once per call: the resend and the chat lane share it and get no fresh allowance, so ADR 0055 item 9's "time only
  runs out" stands. Where the wall has room nothing changes. The gate is unconditional (it does not read `est.Below`): the loop spending
  the re-pack's share is the condition, and an explicit short `timeout_sec` produces it as well. A re-pack the ceiling cannot hold is still
  skipped, and the note now names what ran out: `left to the ceiling` or `left to the re-pack's owed time`.
- **Capped at the call's deadline.** The owed time also stops at `agent_call_deadline_sec` (ADR 0065, 1,500 s by default, counted from the
  wall's start with this node's own setting standing in for the delegator's: a chosen approximation). `repackOwed` sizes the answer at no
  more than the 8,192-token re-pack cap, so the largest answer on a 3.4 tok/s seat is owed 1.5 x 8,192 / 3.4 = 3,614 s: under the ceiling
  and far past the call's 1,500 s. That re-pack is skipped (`left to the call's deadline`) and the delegator's rescue keeps the time it needs,
  as before this change. Replaying the arithmetic on the ledger's 149 distinct starved skips (median 3.4 tok/s and 961 expected tokens, owed 27
  to 1,009 s): 105 end inside 1,500 s when the re-pack starts at the row's recorded latency and gain, 44 do not and are skipped as before. An
  estimate, not a measurement: the row's latency is the delegator's view and the node's wall started later.
- **Not changed.** The wire's 900 s cap on a declared `timeout_sec`, the estimate's worst-case terms (why the wall reads as below it), the
  rate order (`repackRate`), and the delegator (the re-pack publishes its allowance when the phase starts, so a poll that follows progress
  follows the extension up to the node's ceiling). A re-pack that runs on its own time moves work from the delegator's rescue to the slow
  seat; the rescue still catches what the ceiling refuses.
- **Tests.** `internal/pipeline/agenttask_repackowed_test.go`: a 2 s wall and a loop that ends at 2.4 s (red before: `skipped: 0 s left to
  the wall + 0 s grace`), a slow seat with an instant loop (red before: `60 s left ... buys 29 tokens < the answer's 69`), the ceiling that
  cannot hold it, the owed time not renewed for a later attempt, `repackOwed` and the later-of-two-ends arithmetic. Two tests whose premise
  was the defect (`TestRunAgentTaskRepackIsSkippedWhenTheCeilingCannotBuyTheAnswer`, the review-lane subtest `a re-pack the wall could not buy`)
  now pin the ceiling-bound skip. Four mutations of the fix were each caught. ADR 0055 item 10, `docs/systems/fleet-node.md`.

### Fixed — a re-pack that loops is asked differently the next time, and a looped list is not published (C14)

- **The defect.** 28 re-packs in 48 h were cut at the budget: 11 degenerate repetition, 17 "ran on past the budget" (`the answer needs about
  725 tokens, inside the 1173-token budget`), and the register's own rows show the chat lane looping on the same text after the grammar lane
  did. In the code: the seat decodes greedy (`temperature` 0), the request carried nothing that discourages repeating, the grammar's list rule
  is unbounded (and does not compile the schema's `maxItems`), and the attempt after a degenerate cut was the chat lane with the same
  sampling and the same prompt, which a greedy seat answers the same way. A loop that closed itself before the cap was a complete,
  schema-valid object that would have been published as the answer (inferred from the code path: successful ledger rows carry no reason,
  so it is not counted). The tail test looked for a repeated block of up to 64 bytes in the last 256, so two
  alternating list items of 80 bytes read as "ran on" and were escalated to the 8,192-token cap.
- **The fix.** A cut that is no budget problem (a degenerate tail, or the seat ran on past a budget the answer fits in, and the clock did not
  set that budget) marks the call. The next attempt is a second grammar attempt (the lane that constrains the shape) that carries the
  sentence `repackNoRepeatNote` and `frequency_penalty` 0.5 (`llamaclient.WithFrequencyPenalty`, the OpenAI-standard field both engines on
  the fleet read), and the chat lane after it carries both. The first attempt never does, and a cut that more tokens cure (the resend at
  the cap) does not. The lane on the wire reads `grammar+no-repeat`, `json_schema+no-repeat` or `chat+no-repeat`. A complete object whose
  list repeats one item (or a block of two or three) at least 6 times in a row, where the answer it was written from holds the item fewer
  times, is not published: the attempt fails as `re-pack repeated the list item "x" N times in a row ...`, marks the call, and ends as an
  abstention naming the item when every attempt does it. An item may be a string, a number, a boolean, an object or a list (F43 lets a
  llama.cpp seat write a list of objects, and a loop closes inside those as well): an object or a list is compared by its compact JSON with
  the keys sorted, one that holds nothing but empty values (`{}`, `""`, `0`, `false`) is never a loop, the answer is taken to hold an object
  as often as it holds the longest string in it (an object with no string in it is judged by its repeat alone), a finding clips a long item
  to 100 bytes, and every list is read at every depth, named by its path (`meta.labels`, `rows[2].tags`). The detector keeps its two original
  tests and their order and adds a third pass for a block of 65 to 192 bytes in the last 1,024 (whitespace still reads the last 256).
- **Chosen, not measured, and unverified.** `frequency_penalty` 0.5, the run of 6, the period of 3, the 1,024/192 window. No seat was
  replayed with the penalty: that it ends a loop on the fleet's seats is the mechanism's expectation, and its fidelity cost on an honest copy
  (paid only on an attempt after a failure) is unmeasured. llama.cpp's sampler (count in the last `penalty_last_n` tokens times the penalty,
  subtracted from the logit) was read in its source; the server's mapping of the request field `frequency_penalty` to it was not probed on a
  live seat. The real heads and tails of the 28 cuts were not recorded, so which of the 17
  "ran on" cuts were loops and which a restating schema's honest length is not known; a restating schema now gets one more attempt of the
  same length before it defers. Compiling `maxItems` into the grammar was left out: it cannot be verified without a live llama.cpp compile.
- **Tests.** `internal/pipeline/agenttask_repackloop_test.go` (a greedy-seat fake that loops until the request is conditioned: red before on
  the register's own shape, `attempt 2/3 ... chat re-pack truncated ... degenerate`; the published repeated list; the detector),
  `internal/pipeline/repack_objectloop_test.go` (a list of objects, a list of lists and a list at depth: the detector's table, and a seat that
  obeys the grammar and loops inside it, end to end: red before, the eight-object list was published as the result),
  `internal/llamaclient/frequencypenalty_test.go` (the key only on request, the option-free body byte-identical). Three existing tests
  pinned the old flow (a loop goes straight to the chat lane) and now pin the new one; the chat lane's time-skip test moved to a cut that is
  no loop. 18 mutations of the fix were each caught (the run threshold survived once because its test was written in terms of the constant,
  and is now pinned by literals). ADR 0055 item 10, `docs/systems/fleet-node.md`.

### Fixed — a research digest's quotes are the page's own words, or they are flagged (F4)

- **The defect.** A research goal that asked for exact wording (UI labels, a button's text) got digests that paraphrased it, so a
  fact-checker could not find the "quote" on the page. Nothing asked a seat to copy, the goal's length rule ("at most 18 words
  each") invites shortening, and nothing looked a quote up in the page afterwards.
- **The goal.** `Build` adds one sentence to the goal of any schema that has a `quotes` list, the default digest's included:
  exact wording the goal asks for (a quotation, a label, button or menu text) goes in `quotes`, copied character for character
  from the file, never shortened, reworded, translated or re-cased, and a shorter exact stretch beats a paraphrase. It sits
  before the verdict ask, which stays the goal's last sentence. A schema without a `quotes` list gets nothing new.
- **The check.** After the engine returns, the delegator looks every quote up in the page its digest was written from
  (`research.VerifyQuotes`, applied by `offload_research` and the `research` verb through `research.AnnotateWire`). A quote that
  is already the page's bytes is left alone. Otherwise it is looked for with case, runs of white space, typographic quote marks
  and dashes, no-break and zero-width characters set aside and, failing that, with all punctuation set aside too (a colon the
  seat added after a label, a table's cell borders), always on word boundaries; an ellipsis is a gap between parts that must each
  be found in page order. A quote found only that way is replaced in the published digest by the page's own span, so it is the
  page's bytes by construction. A quote found nowhere (a paraphrase, or words stitched together from separate places) stays
  where the seat put it and is listed in the result row's new `quote_check` block (`checked`, `verbatim`, `corrected`,
  `unverified`, `note`). The block is absent when the digest has no quotes to look up, and no other door publishes it. **A
  flagged quote never fails the digest and is never dropped**: an acceptance failure discards a whole run (see 0.171.0), and a
  new acceptance verb would be refused at the ACK by every node one release behind, so the check lives on the delegator,
  outside the contract.
- **Measured.** Replayed over the 112 research digests one delegator logged on the day the defect was filed (104 carried
  quotes): of 877 quotes, 479 were already the page's bytes, 211 were found only through the tolerant forms (the seat had
  changed a mark, a space, a case or some punctuation) and now publish as the page's own span, and 187 (21 %, in 49 digests)
  are on the page nowhere as written. The replay is of what the seats had already written, so it measures the check, not the
  new goal sentence; the sentence's effect on the seats is unmeasured.
- **Limits.** The page is the text the fetch kept: navigation, header and footer are stripped before a seat sees the page, so a
  label that lives only there cannot be verified. The field is found by its name, so a caller's schema that keeps its verbatim
  text in a field not called `quotes` is not checked. The delegation-log corpus and the ledger rows are written inside the
  engine, before this step, and keep the seat's raw answer; the published body carries the corrected one. On the MCP door the
  body's untrusted pass (`internal/untrusted`) runs after this step: it drops the hidden characters a page carries, neutralizes
  chat-template markers and caps each string at 16,000 characters, so a published span can differ from the page by those only.
- **F45 (the `nonempty:verdict` failures of the research lane) needed no code change.** 0.171.0 had already replaced the check
  with `nonempty:key_facts|numbers|quotes|verdict`, and every recorded failure precedes that release's deploy. What was missing
  is an end-to-end pin, added here: through the real handler, a digest with populated lists and an empty verdict succeeds, and
  a digest that said nothing still fails verification.

### Fixed — a job queued on a busy node stays in line at its queue budget when nothing better is on offer, instead of being withdrawn to the back of the same line (C16)

- **The defect.** The queue budget (`clamp(1.5 x ETA + 30 s, 60 s, patience)`) is derived from the node's ETA, which is the median of
  its recent walls, so a one-worker node mid-way through a job that runs three times its median publishes a wait that is a fraction of
  the one a new job faces. One box's ledger shows nine delegated jobs withdrawn at their queue budget (60 s to 11 min) in a day and a
  half. A withdrawn job is filed as a capacity refusal (ADR 0064); with no other node to take it, it goes back to the capacity wait,
  which does not exclude the node it came from and asks it again after its cooldown, putting the job at the back of the same line:
  the wait already spent is thrown away to land where it started.
- **The fix.** At the budget the delegator asks before it withdraws (`stayInLine`). It reads the node's health once more; the job
  **stays** when the node's workers are all running (a published `max_concurrent_jobs`, that many `jobs_running_capped`: the job
  waits for a worker, which is a queue and not a wedge) and no other place has room for it now (`betterPlaceThan`: another eligible remote that
  passes the re-placement test, or the local seat when the route allows it and no lease reserves or fences it and its run-cap line
  has a slot). It is asked about again one step later (60 s, chosen: the budget's own floor), up to the patience the contract was
  given, which is where the ETA-derived budget was already capped. A node with a free worker that still has not started the job, a
  node whose health cannot be read, a node that publishes no worker ceiling or no capped count (an older node), and a job with a better
  place on offer are withdrawn at the budget exactly as before. The failure text says `kept in line N time(s) past the derived budget, up to the contract's patience:
  <why>` when the wait was extended.
- **`jobs_running_capped`.** `/fleet/health` gains it (additive, agent lane only, a pointer so `0` is published): the part of
  `jobs_running` that counts against `max_concurrent_jobs`. `jobs_running` counts every job on the node, so a node with four renders
  running and free agent workers reads "all busy" by it, and a queued job there is not waiting for a worker.
- **A queued job keeps its place in the call's width** (unlike a capacity wait, which gives its slot up, ADR 0073): it is an open
  attempt on a node that is already full. A call with jobs staying in line keeps its width for up to the patience where those jobs used
  to be withdrawn at the shorter derived budget. `betterPlaceThan` does not see the subtask's own tried/excluded sets (a known gap, ADR 0063).
- **Not changed.** The ETA itself (`new_job_wait_sec`) still prices a new job at the median wall whatever the running job has done, so
  placement can still send a job to a node that is mid-way through a long one; it now waits there instead of being reshuffled.
- **Tests.** `internal/delegate/queue_stay_test.go` (a busy node that starts the job at 3 times its budget is waited on to success;
  a node that stays busy is waited on to the end of the patience and no further, and the failure says so; a node with a free worker is
  still withdrawn at the budget; a node whose health cannot be read, or whose running jobs are renders, or that does not publish the
  capped count, is withdrawn too; another node with room, or no other node,
  is told apart, and the job leaves for the node with room end to end). `TestQueueRefreshFailureIsNamedOnTheDeadline` now counts the
  question at the budget among the bounded health reads; `internal/fleetnode/jobs_running_capped_test.go` pins the new health key
  (a render does not count, an agent job does, a published 0, absent with the lane off). 10 mutations were each caught; an eleventh
  (treating a failed read as readable) is equivalent, since a failed read decodes to a view with no worker ceiling. ADR 0063
  Amendment 2026-10-10, `docs/FLEET-NODE.md`.

### Fixed — a capacity wait that holds a run slot gives it up at its bound and keeps waiting, so a subtask in line for a node is not refused while the call has time (C16)

- **The defect.** A fan-out on a saturated fleet ended subtasks as `capacity wait: no node had room within 2m0s (waited 2m0s;
  agent_placement_wait_sec=0; ... 4 subtask(s) of the call had not started, so this wait could not hold its run slot to the call's
  deadline)` while the call still had time left. ADR 0073 decision 9 capped a wait that holds a run slot while a sibling
  has not started at its old TTL so that waiters could not hold every slot to the deadline, and the cap **ended the wait**: the
  subtask paid for the protection of its siblings. A node freeing a minute later would have taken it, and the operator's rule is to
  queue and never refuse (a capacity wait is progress).
- **The fix.** At the bound the wait gives the subtask's place in the call's width up (when a subtask has not started, so that one
  can begin) and runs on to the call's horizon like the last subtask to start (`extendHeld`). It keeps probing the fleet every tick
  and takes a place again only to dispatch (`runSlot.acquire`, bounded by the horizon, reading the fleet again if it had to wait for
  one); a refused attempt gives the place back before the wait sleeps. The attempts a call has open at once are therefore still its
  width, which is why ADR 0073 had rejected releasing the slot outright; the process gate and the cooldowns pace the asks as before.
  A wait with nobody behind it keeps its place. Every subtask of a saturated call now ends at the horizon with the defer that says the
  call bounded it, none at its TTL.
- **Not changed.** A call with no whole-call deadline (`delegate` CLI, `agent_run` / `offload_ask` with a route) still waits
  `agent_placement_wait_sec` (default 120 s): it has no horizon to extend to. The composite decision's eviction wait and the
  verification retry's wait keep their TTL. A chunk of a batched call whose waits run to the horizon leaves its later chunks to the
  existing no-room defer (decision 10).
- **Tests.** `internal/delegate/callwait_yield_test.go` (all five subtasks of a saturated call land when the node frees after the
  TTL, and the fifth began while the node was still full; the node never holds more of the call's jobs than its width when eight
  subtasks wait on a node that publishes no queue ceiling; a wait holds no place between refused attempts, keeps it for its TTL,
  and holds one when it lands; the last subtask to start keeps its place). `callwait_slots_test.go` and `deal_width_test.go` pinned
  the old ending and now pin the new one. 7 mutations were each caught. ADR 0073 Amendment 2026-10-10, `docs/systems/fleet-node.md`,
  `docs/OPERATOR-GUIDE.md`.

### Fixed — the prefill stall clock is sized from the whole prompt and the seat's slow recent rate, so a long prefill on a heat-soaked seat is not read as silence (F10)

- **The defect.** A 4,613-token prefill on a heat-soaked seat showed "silent 59 s of 60 s allowed" while the seat was working,
  one tick from a prefill stall. The allowance (`pending ÷ prefill rate × 1.5 + 30 s`, floor 60 s) was sized from two wrong
  inputs. **The prompt was the transcript alone**: the loop passed `estimateTokens(msgs)` (4 bytes a token) and left out the
  tool-spec block that ships with every request, which the loop already measures for compaction (`specReserve`, 1-6k tokens on a
  full build). On a seat measured at about 180 tok/s a 4,613-token prompt allows 68 s, and any transcript estimate under about
  3,600 tokens allows the 60 s floor, so the undercount alone turned a thin allowance into the floor. **The rate was an
  average**: `prefill_tok_s` is an EMA over runs, so a run on a seat that had heat-soaked was averaged with the cool ones.
- **The prompt.** `agent.prefillPending`: the transcript estimate at the run's token density plus the tool-spec block, never
  under what the server counted for the previous call's prompt (the transcript only grows). The density is `assumedPromptDensity`
  1.3 until a call reports (the low end of the 1.3-1.4 the budget-calibration fit already measured), then the density the call
  showed, clamped to 3 (chosen) so one odd usage reading cannot make the clock unbounded. Run-local, so `--serve` handlers sharing
  one `*Loop` do not share it.
- **The rate.** The seat-rates store keeps the last 5 solo prefill rates (`prefill_rates`; 5 is chosen, not measured: one run folds
  in up to two samples) and their slowest (`prefill_slow_tok_s`), the rule `cold_load_sec` already follows (the slowest of its
  window: a stall bound must hold the slow case). `pipeline.LivenessPolicyFor` sizes the prefill clock from
  `Seat.StallPrefillTokS()` (the slowest, else the EMA of a store written before the window). A shared-seat sample still teaches
  nothing (ADR 0066). `/fleet/health` `seat_rate` publishes `prefill_tok_s` and `prefill_slow_tok_s` (additive, omitted until a
  solo prefill is measured), so the number a "silent N s of M s allowed" line rests on can be read.
- **An unmeasured seat waits longer on its first run.** Its rate is the assumed 100 tok/s, and the prompt now includes the tool block,
  so a full build with about 3,000 tokens of tool JSON earns 3,000 / 100 x 1.5 + 30 s = 75 s before the transcript is counted, where the
  transcript alone earned the 60 s floor. The first run measures the rate. The silent-seat fixture of `internal/pipeline` now names a
  measured prefill, as the cold-load tests do.
- **A wedged seat still stalls.** The allowance is still bounded by the prompt and the slowest recent rate, and the busy hold's
  flat bound still ends a seat whose engine does no work (`TestWedgedSeatStillStallsUnderTheSizedPrefillClock`).
- **Tests.** `internal/agent/prefill_pending_test.go` (the spec block is in the clock; the server's count is a floor; the density is
  learned; the first call uses the assumed density; a wedged seat stalls), `internal/seatrate/prefill_window_test.go`,
  `internal/pipeline/liveness_prefill_slow_test.go`, `internal/fleetnode/seatrate_prefill_health_test.go`. 12 mutations of the
  sizing, the window and the wiring were each caught. ADR 0055 Amendment 2026-10-10, `docs/FLEET-NODE.md`, `docs/OPERATOR-GUIDE.md`.

### Fixed — the first vision call after an idle unload no longer fails on the seat's cold load (F49)

- **The defect.** The vision seat unloads after its idle ttl. The next vision call (`vqa`, `ocr`, `assess_image`,
  `video_describe`, each `video_watch` window) met a cold seat, and llama-swap holds that request open for the whole load, so the
  client's single `request_timeout_sec` (120 s) had to cover the load AND the inference. A 32B vision seat's cold load plus a
  many-frame video prefill outlasted it: the call failed with `Client.Timeout exceeded while awaiting headers` (`err_class: timeout`,
  120.3 s in the ledger, a 499 at 2m0.016s in the router's log) while the load carried on: the router logged the seat's health check
  passing after the cancel. The next vision call, seconds later, succeeded in 29.6 s. The pipeline already knew a first call after an idle gap was likely cold, but only to keep
  that timeout out of the circuit breaker (`breakerFailure`); the call itself still failed.
- **The fix.** `runVisionGen` reads `/running` once after its GPU-lease gate. A seat not listed ready is warmed first with the
  agent door's warm-up (`warmSeat`): one passthrough request that triggers the load, passes the GPU-lease fence like every
  deliberate load, and holds until the seat is up, for at most the new `vision_coldload_wait_sec`. The vision request is sent only
  afterwards, so it keeps its full `request_timeout_sec` for inference alone. Every vision door shares the one function, so all of
  them get it. A seat already ready costs one `/running` read and nothing else; a model routed off this box (`seat_endpoints`, a
  cascade lane) is not warmed on the local router; `vision_coldload_wait_sec: 0` restores the old behaviour.
- **The bound is chosen, and it is a bound.** Default 180 s, **chosen, not measured as one figure**: first requests to a cold seat
  took 14 to 84 s, load plus their own inference (the first chat completion in the router's access log after each of the last 25
  cold starts, 21 of which completed), and one load outlasted the 120 s client timeout; 180 s is about twice the slowest completed one and far inside the router's own 600 s health-check
  ceiling. It limits how long the caller is held, not whether the call is made: when a load outlasts it the call goes on and the
  load carries on behind the request, and a defer after a wait says so (`(the vision seat was cold: waited Ns for its load before
  sending)`). A cold call's worst case is now the wait plus `request_timeout_sec` (300 s with the defaults, which equals the
  delegator's fleet vision budget), and `vision_gpu_wait_sec` before both when a render holds the card first. Config: `vision_coldload_wait_sec`; `docs/systems/offload-pipeline.md`, `docs/OPERATOR-GUIDE.md`.
- **Proof.** `TestVisionColdSeatIsWaitedForBeforeTheRequestIsSent` has a router that starts the load on the first request for the
  model and holds the request until the seat is up (the real behaviour), with a load three times the request timeout: it failed
  with the production error before the change and passes after, and the vision request arrives only once the seat is ready.
  `TestVisionColdLoadWaitIsBounded`, `TestVisionResidentSeatIsNotWarmed`, `TestVisionSeatRoutedElsewhereIsNotWarmedLocally` and
  `TestVisionColdLoadWaitOffLeavesTheLoadToTheRequest` pin the bound, the free resident case, the off-box case and the off switch;
  stubbing the wait out, removing its bound and removing the off-box guard each fail the test that names them.

### Fixed — test, bake and measurement traffic no longer lands on the production ledger views or the PAIR dashboard (register F48, job-error class C23)

- **The defect.** In 48 hours, 42 PAIR cards for a seat named `bake` that no endpoint served (`agent seat "bake" is not in the
  endpoint's served roster`, connection refused on the bake's own port) and 444 ledger rows for a fixture seat reached the
  operator's production surfaces, so every error rate and the fleet-share figure read off the ledger mixed test traffic with real
  work. Nothing marked a row or a card as a bake, a measurement or a test.
- **A row now carries its run class.** `run_class` (`test`, `bake`, `measurement`, or an operator label) is settled in `Record`, the
  one path every writer takes, and is absent on every production row, so an ordinary row is byte-identical to before. It comes
  from, in order: the writer; the process (`LOCAL_OFFLOAD_RUN_CLASS=bake`, any clean lower-case label, or a class-prefixed
  `LOCAL_OFFLOAD_ORIGIN` such as `bake:run-7`; `production` sets nothing); a test binary that opened a ledger outside its own
  throwaway directory (`test`: this is how test rows reached the real ledger); and what the row declares, so rows already on
  disk read the same way: a call pinned with `pin_reason: measurement`, the `cli:fleet-measure` and `cli:fleet-smoke` doors, and
  the `go-test` origin the repo's suites set. `ledger.ClassOf` is the one reading of it. **Nothing is guessed from a seat name**: a
  seat that is not in the served roster is a real misconfiguration (a mistyped `agent_model` fails exactly like the bake's seat) and
  stays on every view.
- **The views leave it out and say so.** `ledger`, `loupe`, `stats` and `audit-sample` skip the labelled jobs by default and report
  how many they skipped per class (`excluded_non_production` in the `ledger` and `loupe` JSON, a `left out:` line, a note on stderr
  for `stats` and `audit-sample`, whose stdout keeps its shape); `--include-test-traffic` counts them again. The inner `agent` row of a
  labelled job is skipped with its parent, so a measurement's savings do not leak into `tokens_saved`. `ledger.SummarizeFile`
  and `TopDeferReasons` default the same way (`SummarizeFileWith` / `TopDeferReasonsWith` include them), `ledger.ReadProduction`
  and `ledger.ProductionRows` are the filtered readers, and `ledger.ReadAll` stays the raw one. The confidence calibration, the
  tier health report, the router and the correctness head are fitted on production rows only: a fixture's margin would move the
  live thresholds. The calibration, the health report and the router read the file with their own scanners, and they judge the rows
  TOGETHER (`ledger.ProductionRows`), so the inner `agent` row of a measurement-pinned job, which names its parent and carries no
  class of its own, leaves with its parent as it does in the views above (a first version judged each row alone and counted it).
  `TestEveryRawLedgerReaderFiltersTestTraffic` fails when a new report reads the raw rows without dropping them.
- **PAIR.** A process with a run class opens no card (`pairworkloads.Config.Suppressed`, through the gate every card already
  uses) and asks no fleet node to card its jobs: an asker with no emitter sends `X-Offload-Pair-Card: node`, which would have put
  the card back on the node, so a suppressed asker sends the asker name and no card header. A delegation pinned for `measurement`
  opens none and its dispatch deletes the card header (a thin client with no emitter would otherwise have the serving node open
  the card), `Begin` and the remote-call card refuse the `fleet-measure` door (the asker's row is still written), and the ledger
  observer cards no row of a class. `LOCAL_OFFLOAD_PAIR_TEST_CARDS=1` keeps the cards of a labelled run
  (to test PAIR itself); `fleet-smoke` keeps its card; the `go-test` origin labels rows only, because the repo's own tests build
  real emitters against fixture ingresses.
- **Not covered.** (1) A run is labelled only when its tool says so: nothing is guessed from a seat name, because a seat that is
  not in the served roster is exactly what a mistyped `agent_model` looks like and must stay on every view, so the bake tool that
  produced the 42 cards has to set `LOCAL_OFFLOAD_RUN_CLASS=bake` (or pin its calls for `measurement`). (2) The class does not
  ride the fleet wire: a fleet node's own ledger row for work it ran for a labelled asker is unlabelled (its card is not opened,
  because the asker sends no card header), and the media and vision lanes' asker headers know the config, not the door, so a
  `fleet-measure` render from a box with no PAIR emitter that overflows to a node still has the node open its card. (3) The
  harness-share gate and any other reader outside this repository that sums the ledger itself must skip rows with `run_class` set
  (and `origin_session: go-test` for rows written before the label).
- Tests: `TestOriginResolvesTheRunClassFromTheEnvironmentThenTheOriginPrefix`, `TestClassOfReadsTheLabelThenTheOriginThenWhatTheCallerDeclared`,
  `TestRecordStampsTheRunClassOnEveryRowAndLeavesProductionRowsByteIdentical`,
  `TestATestBinaryWritingOutsideItsTempDirectoryLabelsItsRowsTest`, `TestSummarizeFileExcludesNonProductionTrafficByDefaultAndSaysSo`,
  `TestProductionRowsDropsLabelledRowsAndTheInnerRowsOfLabelledJobsAndCountsThem` (ledger);
  `TestFromConfigOpensNoCardsForANonProductionRun`, `TestASuppressedAskerDoesNotAskTheNodeToCard`,
  `TestTheLedgerObserverCardsNoRowOfANonProductionClass`, `TestBeginOpensNoCardForTheMeasureDoor`,
  `TestARemoteCallThroughTheMeasureDoorOpensNoCardButStillWritesItsRow` (pairworkloads);
  `TestAMeasurementPinnedDelegationOpensNoPairCard`, `TestAMeasurementPinnedDispatchAsksNoNodeToCard` (delegate);
  `TestLedgerCommandLeavesOutTestTrafficAndSaysSo`, `TestLoupeLeavesOutTestTrafficAndSaysSo`, `TestStatsAndAuditSampleLeaveOutTestTraffic`
  (root); the four `...ProductionRowsOnly` tests of the health, calibration, router and confhead packages, and the inner-row twins of
  three of them (`TestHealthLeavesOutTheInnerRowOfALabelledJob`, `TestRouterTrainsWithoutTheInnerRowsOfALabelledJob`,
  `TestCalibrationLeavesOutTheInnerRowsOfALabelledJob`: red before, three mutants each caught).

### Added — a busy whole-node graph is a ticket in the cluster queue, and a node's "I do not hold this file" is about that ticket only (S5, ADR 0083 Amendment 1)

- **What changed for a graph, in one place (0.180.0 to this release).** *Changed:* (a) a busy `auto` graph that names no `devices`, on a
  machine that has the lane, a fleet and a queue, is a ticket: it starts on the first matching lane that frees anywhere, in order of
  arrival, and the call answers with the ticket, its position and `cluster[]` instead of the local lane's queued answer with a `tk-`
  token to re-send; (b) the bound of three nodes is gone; (c) a node's 503 for a file the graph names carries `refusal: "not_capable"`:
  the ticket marks that node not-capable for itself and puts no pause on it (0.180.0 put the placer's 5 s pause on the node for every
  call); (d) a graph that reads input files is a local-only ticket with a place and a position, and every answer carries `fleet_note`
  (0.180.0 annotated only a deferral); (e) **a graph that carries a `tk-` `waiter_token` no longer stays on this machine**: its place
  comes into the ticket and it may go to a node, as an image's does (0.180.0 never moved it); (f) the deferral a graph ends in after a
  local failure no longer carries `cluster[]` (the per-node reasons live in the queued answers, as for an image); (g) **on a machine
  whose lease root is refused, so that it keeps no queue, a busy graph is no longer offered to the fleet** (an image never was; 0.180.0
  offered it with only the in-process hold between two calls); (h) the MCP tool takes `wait_s` and `run-graph` blocks on its ticket.
  *Unchanged, and held on the ticket path by tests of both kinds:* a graph and an image ticket share one per-node claim, taken under
  the directory lock, and one order, in which an older waiting ticket is first in line; a lane the pipeline could not read is a ticket
  and the fleet is looked at first; the queue lives under the lease's state root, so a config with no `state_dir` has it; a node's file
  check at its door, the one-card rule, `min_vram_gb` and the release proof; a graph that names `devices`, or runs under a lease its
  process inherited, stays on this machine and is no ticket.
- **The behaviour.** A `run_graph` call (`offload_run_graph`, `run-graph`) that names no devices, on a machine that has the lane, a
  fleet and a queue, whose lane is not free (or could not be read), was offered in 0.180.0 once to at most three idle nodes and then
  waited in this machine's own queue, joining it late, with a `tk-` token the caller had to send again. It is now a **ticket**, as an
  image call is: the harness holds it and starts it on the first matching lane that frees anywhere, here or on an idle single-card node
  that holds the files it names, in order of arrival, with one answer that says per node why not (`cluster[]`) and no caller loop. The
  MCP door answers inside `wait_s` (new argument; else the machine's `gpu_wait_ms`) and the ticket goes on after the call returns;
  `run-graph` on the CLI blocks until the job is done, prints on stderr where the ticket stands, and the first Ctrl-C cancels a ticket
  that sent nothing. **This closes the limit 0.180.0 stated for graphs** ("a graph does not wait in the cluster queue yet"). **A graph
  whose lane could not be read is a ticket too**: 0.180.0 already marked that lane `Unjudged` (a lease directory that would not open, a
  card table that did not read inside its 4 s bound; the card table is slowest to read exactly when a heavy render holds the card) and
  offered such a graph to the fleet once, running it here when no node took it. As a ticket, its first scan looks at the fleet, and
  the run on this machine's lane is the answer only when no node can take the graph
  (`TestMediaLaneFreeGraphIsFreeWhenTheCardTableCannotBeRead`,
  `TestMediaLaneFreeGraphIsUnjudgedWhenTheLeaseDirectoryCannotBeOpened`, `TestAnUnreadLaneStillLooksAtTheFleetForAGraph`,
  `TestAnUnreadLaneWithNoNodeRunsAGraphHere`).
- **A graph is the same call when its content is.** The key is the sha256 of the task and the parameters without the `waiter_token`,
  with the graph and the manifest taken as canonical JSON (keys sorted, whitespace dropped, numbers as written) in place of their
  paths: the MCP door writes an inline `graph_json` to a new temp file on every call, so a key made of the path would never attach,
  and a file edited under the same path is another graph. `out_dir`, `reserve_vram`, `min_vram_gb` and `model_family` are part of the
  key. One ticket per identical request, from either of two processes; `waiter_token=mq-...` collects it. The ticket mints no seed and
  sends no recipe: the graph owns its sampling.
- **The ticket is the bytes it was admitted with.** The key was the graph file's content at admit, and the node was sent that content,
  but this machine's own lane was handed the file's PATH and read it when the lane freed, up to eight hours later: a caller that reuses
  one file (shot 1 written, "queued", shot 2 written over it) got shot 2 rendered under shot 1's key and shot 1 never rendered, a
  different result depending on which lane served the ticket, and a finished result that could not be collected by its own token once
  the file was removed or edited (the key is recomputed from the file at every call). The ticket now writes the graph and the manifest
  as the plan holds them into its queue directory (`body-<id>-graph.json`, `body-<id>-manifest.json`), takes its key from those bytes,
  and the node's payload, the local run, the need and an adopter all read them; the copies go with the ticket (finished, canceled, swept
  after an owner that died before sending, the loser of a create race), stay while a dead owner's placed ticket can be adopted, and are
  never swept while a ticket may still be being made. A graph ticket also records a path key (the same paths and parameters, no file
  read): a `waiter_token` is accepted for a request with the same content or the same path key, so the result stays collectable and a
  dead owner's placed job is adopted from the copies after the file is gone or overwritten, while a repeat WITHOUT a token attaches by
  content alone (a file overwritten with the next shot is a new job and inherits no arrival).
- **It keeps its place in this machine's line.** `LeaveMediaPlace` answered "only image generation is probed" for a graph; it now
  leaves the whole-node token the real admission leaves for a waiting graph (reason `run-graph`) at the ticket's arrival time, brings
  along a `tk-` place the call already held, and the final local run spends it through `waiter_token`. A host that leases no cards keeps
  none and the answer says so.
- **A node's refusal of a file is "not this graph", not "full".** The 503 a node answers when the graph names a file it does not hold now
  carries `refusal: "not_capable"` in its body (a full node's 503 and a route-not-ready 503 do not). The ticket marks that node
  not-capable for **itself** with the node's own words, **puts no pause on the node** (before, the 5 s the placer sets for any 503 that names no `Retry-After` hid the node from every image ticket and every other graph it could serve; 0.180.0 had already stopped calling that pause "full"),
  does not ask that node again for 5 minutes (chosen, not measured: the longest pause the placer puts on any node) and tries the next
  candidate at once. A node held not-capable is not one the ticket matches, so it holds it against nobody. `cluster[]` rows quote the
  node's sentence from the body (cut at 600 bytes) instead of the first 300 bytes of the raw body.
- **A graph that reads input files is a local-only ticket.** Never offered to a node, it reads no roster and matches no node, but it is
  one ticket per request with a place and a position, and runs when this machine's lane serves it; every answer carries `fleet_note`.
- **What it closes, and what it keeps.** Closed: the late local join, the lane that frees after the graph's one look at the fleet, the
  repeat that was a second call (a ticket's identity is the graph's content), the bound of three nodes, the `tk-` token a caller had to
  re-send, and the abandoned job (a graph a dead process sent is adopted by the next identical call, polled and fetched, never sent
  again). The result's placement text names the graph lane (it said "image lane" for every task). Kept, as the ticket's own rules and no
  longer a second copy of them: the per-node claim taken under the directory lock, so two processes (two MCP sessions, or a CLI beside
  an MCP server), or two calls of one process, never put two graphs on one node and a graph and an image never share one, and the
  order, in which a node an older waiting ticket of the machine matches is that ticket's, whichever kind it is. (0.180.0's integration
  fix round had given the one-shot call both; they are the ticket's own now, and `tryGraphOverflow`, `placeGraph`, `overflowOutcome`,
  the in-process hold and the writer of the graph-kind claim are removed. The claim of that kind is still READ, below.) One behaviour
  changes on purpose, as the image ticket already has it: the one-shot path added `cluster[]` to the deferral the LOCAL run ended in (a
  graph that, after its wait, failed here with a typed code); the final result of a ticket is the lane's own, and the per-node reasons
  live in the queued answers.
- **Tests, mutation-checked.** Written first and run against the one-shot path as it stood when this work began (before the 0.180.0 integration fix round): 17 of the 19 graph-ticket tests of the delegator failed or
  timed out, and 3 of the 4 place tests of the pipeline failed (the three that passed held a behaviour that survives: the move to the next
  node on a file refusal, the shared refusal word, and a graph that names devices keeping no place). Tests that encoded the old limit were rewritten for the ticket (a graph is a ticket; a bouncing graph is not bounded to three nodes; two calls of one process and two processes never stack). The tests the fix round wrote for the one-shot call were carried to the ticket path, so that each guarantee it gave is still held and tested: the claim and the order, in both directions (`TestAnImageTicketNeverSendsANodeAGraphTicketHasAJobOn`, `TestAGraphTicketLeavesANodeAnotherTicketHasAJobOn`, `TestAGraphTicketLeavesANodeAnOlderTicketIsFirstInLineFor`, `TestAnImageTicketLeavesANodeAnOlderGraphTicketIsFirstInLineFor`, `TestTwoProcessesNeverStackTwoGraphsOnOneNode`), the unread lane, the lease's state root (`TestAGraphTicketReadsTheTicketClaimsOfTheDefaultStateRoot`, `TestABusyGraphIsATicketOnAMachineWithNoStateDirKey`), a refusal at the door that is not "full" (`TestAFileRefusalIsNotCapableForThatTicketAndNeverFull`), and the claim a 0.180.0 process wrote for a graph call (`TestAnOlderReleasesGraphClaimHoldsWhileItsOwnerLivesAndForTheGraceAfter`, `TestATicketNeverSendsANodeAnOlderReleasesGraphCallHasAJobOn`). 27 mutations of the rules above, each applied once to the LF text of one file, proven applied, run through the
  RAM guard, restored byte for byte: all 27 were killed when they were run. One survived the first set of tests (a need re-read at every
  scan, which a graph file that has gone would turn into a lost VRAM floor) and was killed by the test added for it. That test stopped
  being able to tell it apart once the ticket kept its own copy of the graph (the bullet "The ticket is the bytes it was admitted with":
  the copy was still there to be read), so for a time the count of 27 was not true of the code; the polish round below makes the test
  remove the copy as well and shows it red with the mutant applied. One is killed by a timeout and not by an assertion (a refusal that
  is not remembered makes the ticket POST in a loop). The order of two lines in the not-capable path (the refusal is kept before the
  ticket goes back in line) is a race too narrow to fail on demand and is not mutation-checked.
- **Review fixes, mutation-checked.** The two defects above (the local run reading the live path, and the unread lane) were reproduced
  by the reviewers and written as tests first: the lane ran the overwritten file's bytes, the token was refused as "a different request"
  after the file was removed or edited, the adopter could not plan after the file was gone, and, on this branch's base, the graph probe returned `Unjudged: false` for an unreadable table and an unopenable lease directory (the 0.180.0 integration fix round made the same correction on main; the merge keeps one implementation and the tests of both); every one failed before the change. Nineteen mutations of the new
  rules (no repointing to the copies, the token ignoring the path key, the adopter planning from the caller's path, a losing create or
  a finished ticket or a removed one keeping its copies, the sweep taking a young orphan, an in-flight ticket's copies or never an old
  orphan, a key that is not the plan's bytes, no path key recorded, an arrival inherited for other content, five choices of the
  `Unjudged` flag, and the two places that would run an unread lane here) were each proven applied, run through the RAM guard and
  restored byte for byte: all 19 are killed. The first set left two surviving (a removal covered only by a redundant one, an inherited
  arrival) and four that did not compile; the tests added for the first two and valid forms of the rest are the 19.
- **Compatibility.** No config key or file format changed: a ticket file gains an optional `fleet_note` and an optional `path_key`, the
  queue directory `body-*` files while a graph ticket lives, the 503 body an optional `refusal`, `offload_run_graph` an optional
  `wait_s`. A 0.180.0 process that is still running keeps the one-shot path until it restarts: it writes a graph call's claim (kind `graph`), which this release's scanners read and honour (`TestAnOlderReleasesGraphClaimHoldsWhileItsOwnerLivesAndForTheGraceAfter`), and it reads this release's graph tickets as it reads any ticket (their claims, their place in the line), so a node is not sent two jobs across the two releases. A node on the build before this one answers its file refusal without the word, which a delegator of this build reads as a 503 for a full node (the old behaviour); graph placement already requires
  an equal `harness_version`, so such a node is not a candidate while the numbers differ.
- Not changed, and said: the file check is by name, not content, and the bytes of a graph's input files are no part of its key (a
  finished result nobody collected, kept up to six hours, answers a repeat of the same graph even if the caller has put other bytes
  under the same input name: set another `out_dir`, which is part of the key, or collect first); the ticket remembers a node's refusal
  in the owning process only; a waiting graph ticket's place is the whole-node token, so every call that arrives after it queues behind
  it on this machine's own lane, an image call whose own card is idle included (it can still go to an idle node), until the ticket
  runs, is canceled or ends (8 h at most), which is the seniority asked for and also what it costs. No VERSION or buildinfo bump.
  **Nothing here was run live**: live acceptance A9 (the workstation lane and the headless node held, a graph call gets one ticket
  answer and then runs on the first node released; with the workstation's card table slow to read while a render holds it, the same
  holds, because an unread lane is a ticket) is the first evidence.

### Fixed — the graph ticket's queue files are owner-only, a run-graph call leaves no result envelope behind, and the mutation count is true again (S5, polish round)

- **The queue's files are readable by their owner only.** The queue wrote every file 0644 into a 0755 directory, although `ticket.go`
  says a request body that carries a prompt is kept out of the store. The one body it does keep, a graph ticket's copies of the graph
  and the manifest (`body-<id>-graph.json`, `body-<id>-manifest.json`), carries the graph's prompt text, and a finished ticket's file
  carries the call's result, an image's refined prompt among it; on a POSIX host both were readable by every user of the machine. Now
  every file of the queue (the tickets, the copies, the node claims and the lock) is 0600 and the queue directory is 0700 where the
  queue creates it. It does not touch the state root above it (the GPU lease's, open to every user of the machine on purpose: a parent
  the queue has to create is made the way the lease makes it), and it does not change the mode of a queue directory that exists (an
  older build made it 0755): a file an older build left keeps 0644 until its ticket is written again, which a live ticket's owner does
  at every scan. Written first and red before (files 0644, the directory 0755; unix build tag, run on Linux): `TestEveryFileOfTheQueueAndTheQueueDirectoryAreOwnerOnly`, `TestAFinishedTicketsFileHoldsItsResultAndIsOwnerOnly` and `TestAQueueDirectoryThatExistsKeepsItsMode` (red on its second assertion, that what the queue writes into a directory it did not create is owner-only; its first, that such a directory keeps its mode, held before the change and guards the fix). On Windows a mode means nothing: the ACL of the state root decides who can read the
  copies there, and that root is readable by the machine's users by design (a named limit).
- **A run-graph call leaves no result envelope behind.** `run-graph` names its result envelope under the media dir when the caller gives
  no `result_path`, and named it from the paths of the graph and the manifest ("so a re-run reuses one file"). A caller that hands a
  graph file of its own for every call therefore left one `run-graph-<hash>.json` per call that nothing removed: every graph ticket that
  ran on this machine's lane (its graph is the ticket's own copy, new for every ticket), every MCP or CLI call with an inline graph
  (a new temp file per call) and every graph a fleet node ran for a delegator. The envelope is the runner's note to one call and is read
  once, so it now has a name of that call's own (random, in the media dir) and is removed when the call ends, whatever the run came to
  (done, a typed defer in the envelope, a runner that fails after writing it); a caller that names `result_path` keeps the file it
  named. (A name made of the graph's content would not end the growth: the content moves with the seed, so each shot is another file.)
  Envelopes an earlier build left in a media dir, and the one a process killed in the middle of a run leaves, are not removed by this
  change. Red before: `TestARunGraphLeavesNoEnvelopeBehind` (three runs left three envelopes, in each of the three endings). The other two were not red before, and are named for what they are: `TestARunGraphKeepsTheEnvelopeACallerNamed` passed on the old code (nothing was ever removed, so it guards the new removal against taking a file the caller named) and `TestRunGraphEnvelopeNamesAreOneToACall` had no function to call (the random name is new).
- **The mutation count is true again.** The entry above says all 27 mutations are killed, and one of them (a need re-read at every
  scan) had stopped being killed when the ticket began to keep its own copy of the graph: `TestAGraphsNeedIsReadOnceWhenTheTicketIsMade`
  removed only the caller's file, and with the copy still there to be read, a need read at every scan and a need read once gave the same
  answer (the test passed with the mutant applied). It now removes the ticket's copy as well, after checking it is there; with the
  mutant applied it fails (the 8 GB node is sent the Wan graph its default floor keeps it off), with the code it passes. The two code
  comments that gave the caller's vanished temp file as the reason (`liveTicket.need`, `liveTicket.req`) name the copy.
- **A contention test of this change no longer races itself.** `TestTwoProcessesNeverStackTwoGraphsOnOneNode` started eight goroutines
  per queue, so one ticket was stepped by several scans at once, which no process does (a process has one scanner): one step could
  write the ticket's file with the state it had before its claim while the other process judged that claim, and the node was sent two
  graphs, 13 runs of 30 on one Windows box, with no product code involved. Each queue's scans now run one after another and the two
  queues against each other: 0 failures in 60 runs, and a claim that ignores a live holder still fails it (10 of 10 runs). The image ticket's equivalent, `TestTwoProcessesNeverDispatchTwoTicketsToOneSingleJobNode`, was built the same way and failed the same way on this branch's base (13 to 17 of 30 runs on that box, 2 of 30 on a Linux one); the 0.180.0 fix round fixed it in the same manner.
- **Mutations of this round: twelve, all killed.** Each applied once to the text of one file, proven applied (its hash changed and
  `git diff --stat` showed it), run, and restored byte for byte (the hash checked): the need re-read at every scan (it passed the old
  test), no removal of the envelope, removal also of a caller's `result_path`, removal only after a success, one envelope name for every
  call, the queue's files 0644 again, the lock file 0644, the directory 0755, a chmod of a directory that exists, the state root stamped
  private, a claim that ignores a live holder (against the reworked contention test), and a door that asks for `cfg.StateDir` itself instead of `storeFor` (against `TestABusyGraphIsATicketOnAMachineWithNoStateDirKey`, which only a door that goes through the lease's state root can pass). The tests of the modes ran on
  Linux, where a mode means something; the others through the RAM guard.
- **Named limit (parked): a narrow re-key race.** The door keys a request from the graph file as it reads it (`admit`, `reqKey`), and
  `create` keys the ticket again from the bytes the plan holds (`payloadKey`). A caller that overwrites its file between those two reads
  gets a ticket for the bytes it holds, which is what runs (never another graph), but the attach and arrival-inheritance decisions made
  between the two reads were made on the first read: it can attach to a live ticket of the first read's bytes, or inherit the arrival
  of an abandoned ticket of other content. The window is the time between two reads of one local file.
- **Named limit (parked): a create that stalls for more than a minute loses its copies.** `create` writes the graph's copies first and
  the ticket's file after the place is left and the directory lock is taken, and another process's sweep treats a copy with no ticket as
  debris after `tmpStale` (one minute). A create stalled longer than that between the two (a suspended process, a lease read that does
  not return) can have its copies swept: the ticket then fails loudly (its run on this machine's lane cannot read its graph and ends in
  a deferral, an adopter cannot plan; a dispatch to a node still carries the bytes the plan holds in memory) and a repeat of the
  request makes a new ticket. It never renders other bytes.
- **Compatibility.** No config key, tool argument or file format changed. The queue's files are written 0600 and its directory 0700
  where the queue makes it, and the result envelope's name is random instead of a hash of the paths and the file no longer outlives
  the call. No VERSION or buildinfo bump. Nothing here was run live.

### Added — `generate-image --batch` across the fleet: a family, a verdict per job, a results file, a resume, and one ticket per job when the lane is held (F28, ADR 0083 decision 13)

- **The behaviour.** `local-offload generate-image --batch jobs.jsonl [--family F] [--route auto|local|remote] [--results r.jsonl] [--resume]
  [--jobs N]`. Where the jobs run is decided once, at the start, and never mixed. If this machine's lane is free (or the route is local, or there is no fleet, no queue (a state root the lease refuses) or an inherited lease) the whole batch goes through the warm session as before (one change, below: an interrupt). If the lane is held
  (or could not be read) and at least one roster node renders the batch's recipe, **each job is one ticket** of the ticket queue (ADR 0083): it starts on
  the first matching lane that frees, here or on a node, in file order, at most N in flight (default: this machine's lane plus one per matching
  node, at most 8, a chosen cap; `--jobs` overrides), and the command blocks until they have all ended. A node is claimed by the ticket that has a
  job on it, so a batch never sends two jobs to one single-job node at once (`TestBatchNeverDispatchesTwoJobsToOneSingleJobNodeAtOnce`).
  Each job reaches the node as written (`refine=false` only when the job says so, the seed, the negative, `width`, `height`, `steps` exact; the
  family under the node's own name) and its output is fetched, checked against the sha256 the node published, to the exact `out`
  (`TestBatchJobsPlacedThroughTickets`). No mixed local/remote work stealing and no batch task on the fleet in v1.
- **The honest limit.** krea2 and the 2512 build have no remote match (the nodes hold no such weights), so a batch of either never goes to the
  fleet: it keeps its place in the lease line for this machine's lane (up to the queue's 8 h ceiling, a chosen constant, not the 90 s `gpu_wait_ms`;
  Ctrl-C ends the wait) and runs warm on it, and the command prints, and the JSON carries as
  `cluster`, which node does not match and why (`TestBatchWithNoRemoteMatchRunsWarmOnThisMachineAndSaysWhy`). Per-job tickets would be a cold
  start per picture on the same lane. Where no claim protects a node (`--route remote`, a machine with no image lane) the jobs run one at a time.
- **A row per job** in the printed JSON and in `--results`, kept in `<results>.partial` as each job is final and written whole, in job order, over
  `--results` when the batch ends (an earlier results file survives a batch the command refuses or one that dies; the warm path's rows arrive when its
  session ends, because the render script runs to the end before the batch sees a result):
  `{i, out, seed, ok, ms, error, node, family, sha256, verdict}`, the first six being what `parseBatchResults` already reads. `verdict` is
  `rendered`, `skipped-existing`, `failed` or `queued` (the command ended before the job did, or no lane or node had a place for it). A job counts as rendered only when the file at `out` is there,
  not empty, and hashed; a lane that said it rendered a missing or empty file, or a node's file with no published sha256, did not render it.
  **Exit 4** when any job failed (`imagegen.BatchExitJobsFailed`; also when a deferred warm batch already had a failed row), **1** when it stopped,
  was interrupted or a job is `queued` for want of a place, so a wrapper tells "some failed" from "stopped"; **0** also when a busy lane deferred the whole warm batch (its rows are `queued`).
  A job whose `out` folder cannot be made fails alone before any lane is asked for it.
- **`--resume` is fail-safe** (the 21 zero-byte PNGs of 2026-10-09): a job is skipped only when the file at its `out` is not empty, **decodes
  whole as a PNG** and has the width and height the job names; a zero-byte file, a file cut short, another size, not a picture and a missing file are
  rendered again (`TestBatchResumeSkipsOnlyExistingAndVerifying`, `TestBatchResumeAfterTicketsPlacesOnlyWhatIsMissing`,
  `TestGenerateImageBatchResultsFileAndResume`). A job with no `out` is never skipped, and a size off the family's grid (2.1 floors to 32)
  never matches: both are said in the docs.
- **A busy place is waited for, never failed; a place that can never come is not waited for** (review of this step, and its second round). An answer that
  says no lane or node had a place for a job ends the job `queued` with the true reason, never `failed` (exit 1, `--resume` renders it). On the ticket path the
  queue's own end of a ticket (its 8 h ceiling, or a ticket it could not take over) is no lost race and is not sent again as a new ticket; before, it was taken for
  "the lane was taken a moment after it read free", re-sent twice with another 8 h wait each, and the row failed after about 24 h. A lane that read free and was taken a
  moment later is still asked again, at most twice, and a card that stays held ends `queued` (`TestBatchTicketThatExpiresIsQueuedAndNotSentAgain`,
  `TestBatchRetriesALostLaneRaceAndOnlyThat`). Where jobs go one at a time (`--route remote`, a machine with no image lane) a held card or a fleet with no node
  free to take the job is asked again after a pause (5 s, 15 s, 1 min, then every 5 min; chosen) until the job is placed, Ctrl-C ends it, or the queue's 8 h
  ceiling passes and the job is `queued` (`TestBatchSerialWaitsForAPlaceAndNeverFailsTheJob`, `TestBatchSerialEndsQueuedAtTheCeilingNeverFailed`,
  `TestBatchSerialWaitsOnlyForAPlace`, `TestBatchSerialInterruptEndsTheWait`). A fleet that cannot take the job as configured (every node left out because it does
  not serve image generation, a route is not configured on it, it cannot carry `refine=false`, or the tailnet guard refused its roster entry; none merely busy,
  text-leased or out of reach) is not waited for: `pickNodeFor` files `core.ErrClassNoNodeConfigured` (the defer class is unchanged) and the job fails at once,
  naming each node's miss (`TestBatchSerialFailsAtOnceWhereNoNodeCanEverTakeTheJob`, `TestPickNodeFilesNoNodeConfiguredOnlyWhenEveryNodeIsLeftOutForConfiguration`);
  one node that may take it later keeps the wait (`TestBatchSerialStillWaitsWhereAnyNodeMightTakeTheJobLater`). A node chosen and then lost when the job is sent, or
  after, and every other answer, is final at once. Once one job has waited the ceiling for a place, the jobs not yet sent are queued at once, on both paths,
  instead of each waiting the ceiling in turn (200 jobs, 200 ceilings: `TestBatchSerialCeilingQueuesTheJobsNotYetSent`, `TestBatchTicketCeilingQueuesTheJobsNotYetSent`),
  and the exit-1 line says which cause ended the batch (`TestBatchExitSaysWhyTheJobsAreUnfinished`). The warm batch of a busy lane no node can relieve holds its place in line instead of giving up at 90 s
  (`pipeline.RunImageBatchWaiting`; the wait ends the moment the context does and a grant that arrives after is given back:
  `TestRunImageBatchWaitingKeepsItsPlaceUntilTheCardFrees`, `TestRunImageBatchWaitingEndsOnInterruptAndLeavesNoLease`).
- **What a one-at-a-time row says.** The node's recipe is not compared there, so a row carries the family, licence and `commercial_use` the node that
  rendered it reported (nothing if it reported nothing), the printed header names none, and a family only the nodes define is carried by name under
  `--route remote` or on a machine with no image lane (`TestBatchSerialRowsCarryTheNodesOwnFamilyAndLicense`,
  `TestBatchCarriesAFamilyThisMachineCannotResolveWhereItCannotRenderLocally`). `refine_fallbacks` is printed for a warm batch whose warm call ran and answered only
  (`TestBatchPayloadReportsRefineFallbacksOnlyWhereTheyWereMeasured`).
- **A failed job leaves no file of its own at its path.** An empty file a lane left is removed, and a node's file with no published sha256 is moved aside as
  `<out>.unverified` (the picture is kept, the row names it) so a later `--resume` cannot take it for finished (`TestBatchAJobIsRenderedOnlyWhenItsFileIsThere`).
  A picture that was already at `out` before the batch is not a failed job's to remove: a render that fails keeps it (the render helpers write atomically), and only the
  node's file the fetch has already put at `out` replaced it. `--results` may not name the
  `--batch` file, and an earlier results file is replaced only when the batch ends (`TestResultsFileNeverCostsAnEarlierRecordUntilTheBatchEnds`,
  `TestGenerateImageBatchResultsFileIsNeverTheJobsFileNorLostToARefusal`). A folder as `--results`, and on Windows a read-only file, are refused when the file is opened, before any
  lane time is spent (`TestOpenResultsRefusesAFolderAtOnce`); the `--resume` run that recovers a killed batch no longer truncates the `<results>.partial` that batch left: it is
  set aside as `<results>.partial.prev` (one is kept, the newest), the command says so, and a refused run puts it back
  (`TestOpenResultsKeepsAnEarlierBatchsRecordAside`, `TestGenerateImageBatchKeepsTheRecordOfAKilledBatchBesideTheRecoveryRun`). One file spelled two ways (relative and absolute, another case where the file
  system ignores it) is one `out` (`TestBatchOutsThatNameOneFileTwoWaysClash`). `--resume` reads the PNG header first and renders again, without decoding,
  a canvas that is not the named size or is over 64 Mpx (a chosen ceiling; `TestResumeDoesNotDecodeACanvasItCannotUse`).
- **`RunImageBatchAs(ctx, family, jobs)`** is the family seam: `RunImageBatch` is it with no family. Every read of the binding goes through the
  family's resolved config, not `p.cfg`: the lease is asked for on the family's card and with its timeout, the launch is its pin, the runner script,
  ledger label and every item's tag and license are the family's (`TestRunImageBatchFamilySeam`, `TestRunImageBatchAsAsksForTheFamilysCard`,
  `TestRunImageBatchAsLeasesForTheFamilysTimeout`). A family on stable-diffusion.cpp has no warm session and is refused by name. The family is now
  part of a default `out`'s hash, as it is of the single render's; no family hashes exactly as before.
- **Changed, said.** The warm session now runs under the verb's interrupt context; before, `generate-image --batch` ran it under
  `context.Background()` and a Ctrl-C killed the whole process. Now the first Ctrl-C cancels the render: `gpugen.Generate` builds the child with
  `exec.CommandContext` and a tree kill (the child and its descendants are killed), and the process stays up to run the deferred lease release
  and teardown; the second Ctrl-C is the default one. The ticket path needs the same context (a ticket that sent nothing is canceled by it).
  `--family` and `--route remote` are no longer refused on a batch (`--transparent` still is, `family_cli_test.go` follows);
  `--results`, `--resume` and `--jobs` without `--batch` are refused by name. The printed JSON keeps its shape and adds `mode`, `skipped`,
  `cluster` and a verdict, hash and node on every item; a warm batch a busy lane deferred now lists its jobs as `queued` items (its `count` was 0)
  and still exits 0 (4 if a job had already failed). Two *different* pictures for one `out` are refused before anything runs (the second used to replace the first); an empty
  prompt still refuses the whole batch with `job N: empty prompt`.
- **Not changed, and said.** An interrupt leaves a job already on a node running (ADR 0064); the same command adopts it only if its jobs carry
  explicit seeds. A node job pays a ComfyUI cold start. Nothing here was run against a live lease, a live node or a render: the tests use the real
  fleet-node servers, ticket files and results file with fakes for the pipeline and the renderer. The 8-ticket cap and the two retries of a job
  whose lane a sibling took are chosen constants, not measurements.

### Added — a family's identity includes the content of its weight files (F59, ADR 0082 Amendment 2)

- **The hole.** A recipe identified a weight file by its name and byte size, so a same-named file of the same size with different
  weights on another node digested alike and matched; the node's 412 re-check recomputed the same size-based digest and could not
  catch it. Each present weight file now carries a **content fingerprint** in `mediacap.RecipeFile` (`fingerprint`), and it is in
  the digest, which is now version 2: a digest of 0.180.0 or any release before it never equals one of this release.
- **What the fingerprint is.** A sha256 of a scheme tag, the file's size and a SAMPLE of its bytes: 64 blocks of 1 MiB at evenly
  spaced offsets, the first at the start of the file and the last ending at its end, or the whole file when it is 64 MiB or smaller
  (block size and count are chosen). A sample and not a full hash: on one workstation, page cache not controlled (read these as
  warm-cache figures), it read 0.17 to 0.87 s per file for files of 0.36 to 42 GB, against 389 MB/s for a whole-file sha256 on a core
  (about 50 s for 20 GB and 110 s for 42 GB, extrapolated), minutes per node after every release or model change in which the node
  cannot be matched; that this window is too costly is a judgement, and RAM was not the reason. Two real 42 GB builds that share a
  size differed in 58 of the 64 sample blocks. **It does not see a change confined to the stretch between two sampled blocks (the
  blocks are evenly spaced, so that stretch is about a sixty-third of the file: roughly 0.3 GB of a 20 GB checkpoint and 0.65 GB of
  a 42 GB one; a layer-wise merge or a baked-in VAE can sit inside it), nor a replacement with the same size and the same
  modification time while the process that cached the old reading runs** (the cache is in memory, by path, size and
  modification time, and a restart reads again; a one-shot CLI call is a fresh process, so on a busy lane it reads its family's
  samples on every call, an estimated one to three seconds from the rates above, before it makes its ticket, and on a free lane it
  reads nothing: the lane is asked first); compare whole-file hashes after a merge you swap in or a copy you doubt.
- **Health never waits for it.** `/fleet/health` reads the fingerprint cache; a file not hashed yet starts one background read (one
  file at a time in this process) and is published `"fingerprint":"pending"` (`"unreadable"` when it cannot be read, retried after 30 s). **A row
  with any such file publishes an empty `digest`**, so two nodes that were both pending cannot share one. The row memo (60 s) is
  rebuilt as soon as a read settles. The node's 412 re-check and the delegator's own recipe wait for a fingerprint they do not have.
- **A wait for a fingerprint ends, and a stalled read is given up on.** The 412 re-check and the delegator's door wait through `AwaitContext`, once for each file: a wait ends when the request's context does and, whatever the context, after 20 s (chosen; the bound is per file, so a recipe whose files are all unread and stalled can wait up to its file count times 20 s, four files 80 s, if the request lives that long), and a file still unread is
  named `pending` (the 412 names it and carries the cause `pending`, which the delegator reads as a wait; at the door the family has
  no identity for that call and it runs on the local line). A read that does not finish in 60 s (chosen) is given up on: that file reads `unreadable`, the single
  reader serves the other files, the file is not read again while the first read is out, and the harness logs it once; until then a
  stalled read holds the reader and the other files stay `pending`. The "send family=X to use its Y" hint reads this machine's other
  families from the cache without waiting (a family not hashed yet is left out), and a call that has no fleet overflow because a file
  of its own family could not be fingerprinted logs which file and why, from the door and from the adopter of a dead owner's ticket.
  A file still being copied is read again on every health refresh (the 30 s retry is keyed on its size and time).
- **A recipe that is not fully fingerprinted is never compared.** This machine's family with such a file has no identity (the call
  runs here, as for a missing file); a node whose rows are not all hashed is a `not-capable` row that says it has not finished (a
  candidate once it has), whether or not the row carries a digest; two all-pending recipes are never taken for one match; and a
  same-named, same-size file with other bytes is a miss that names the file: `imagegen_ckpt: <name> has the same size on both and
  different content (fingerprint <12 hex> vs <12 hex>)`. A node whose file cannot be read answers the 412 with the file and its state.
- **A free lane reads no fingerprint.** In 0.180.0 the door built this machine's identity (what its family is: three or four stats) before it asked the lane. With a content fingerprint that step waits for each fingerprint it has not got (up to 20 s per file), so every free-lane call on a box with a fleet would have paid the reads first, against the door's own promise that a free lane runs the call here exactly as it always did. The lane is asked first, and the identity is built only when the lane read busy (or could not be read) and a ticket is about to be made (an image only: a graph has no recipe and builds no identity)
  (`TestAFreeLaneNeverWaitsOnAFingerprintRead`: every read held, the call returns and no weight file is read;
  `TestABusyLaneReadsTheFingerprintsOnlyAfterTheLaneWasAsked`). A call without an identity logs why only when its lane was not free.
  One consequence, pinned by `TestARepeatOfALiveTicketAttachesWhileThisMachineHasNoIdentity`: a repeat of a request identical to a live
  ticket attaches to it even when this machine cannot read its weight files at that moment, where it used to run a second copy here.
- **A 412 says why, and a wait is not counted as a bounce.** A node answered every 412 the same way and the delegator read each one
  as "its files changed since it published them": a minute's pause, a longer one on the next, each counted as a bounce. Since F59 a
  node also answers 412 when it has not finished reading its files and cannot say yet whether they changed, which is a wait. The
  refusal's JSON body now carries a `cause`
  beside `status` and `error`: `pending` (every file it lacks a fingerprint for is still being read), `unreadable` (a file is there
  and cannot be read, so it cannot vouch for the family) or `changed` (the family no longer digests to the digest sent, or the node
  no longer serves it). A delegator that reads `pending` leaves the node alone for a minute (the first bounce step; chosen, and
  about as old as the node's published rows can be) and does not count it as a bounce, so a run of them never reaches the
  five-minute step and a later real change still starts at one minute; `unreadable` and `changed` are bounced as before, and a
  refusal that names no cause this build knows is bounced like `changed`, its row saying the node did not say why. The cluster row
  says which cause it was. The node's half is pinned in `internal/fleetnode/image_recipe_cause_test.go`, the delegator's in `internal/mediaremote/recipe_refusal_test.go`.
- **Deploying.** Every node that should take overflow must run this release: the digest is version 2 and a recipe is compared only
  between the same release (`harness_version`), so a fleet part-way through the upgrade matches only inside each release, and an
  overflow between releases falls back to the local line and the ticket. A dispatch goes only to a node whose health row carries a
  digest, which it publishes once it has read its files, so the first dispatch after a node restarts finds them cached and is not
  refused for want of them. One that meets a file the node has not read (it changed after the row was published) waits for it (until the request ends and, whatever the request, 20 s at most for each file; a few seconds for a family of four files on the machine measured) and is admitted when the digest then matches; only a read still unfinished at that bound is answered 412 with the
  cause `pending`, which the delegator reads as a wait. After the upgrade the three live matches of 0.179.0 are read again (`image_recipes` on both nodes of each pair); a pair that no longer
  matches is a finding about the files, and the cluster answer names the file. Nothing here was run against a live fleet.
- **Named limits of the polish round (F59), one line each.**
  - The one-minute pause for a `pending` refusal is chosen, not measured: it is the first bounce step and about the age of the rows a
    node publishes (a 60 s memo).
  - `changed` is one cause for three things the node's own sentence tells apart (a file replaced, a family the node no longer
    serves, a binding that is not ComfyUI); no finer word was added.
  - The delegator's tests serve `pending` and `unreadable` from the fixture in the node's own shape and drive a real node to a real
    `changed` only: a real node answers `pending` after a 20 s wait that no test outside `mediacap` can shorten. The node's half is
    pinned apart, in `internal/fleetnode`, on the same constants.
  - A call with no identity (an unknown family, an sd.cpp binding, a file this machine lacks) now asks the lane before it runs on
    its own line, where it ran at once: one read-only question, bounded, and the price of not reading the files on a free lane.
  - The adopter of a dead owner's ticket still builds the identity before it knows whether the job will be passed back (a
    passed-back job re-enters the scan and needs it); only the free path was changed.
  - The log line for a read that is given up on is once per stalled read; the line for a caller that stops waiting (the 20 s bound of one file) is one per file it stopped waiting for, and no test pins that one.
  - `mediacap.ImageRecipe` has no production caller (its comment says so); it stays because tests and documents name it, and
    deleting it is a separate cleanup.
- **Found in the integration review, fixed.** (1) The lane reading a call hands its ticket's first scan was dated from the ticket and not
  from the question: the door asks the lane, then waits on the identity (up to 20 s for each weight file), then makes the ticket, so a lane
  that freed during that wait was still taken for held and the ticket waited one more scan or went to an idle node. The reading now carries
  the instant it was made and the scan reuses it only inside the 2 s `verdictFresh`
  (`TestTheFirstScanReusesTheLaneReadingOnlyWhileItIsFresh`, three mutants caught). (2) The batch's order, the lane first and the
  identity only after a held lane, was pinned for the single call and not for `generate-image --batch`: moving the identity above the
  lane question passed every test of the package. `TestBatchOnAFreeLaneNeverWaitsOnAFingerprintRead` (every read held, the batch returns,
  zero reads) and its control `TestBatchOnABusyLaneReadsTheFingerprintsOnlyAfterTheLaneWasAsked` now pin it, and the mutant that builds the
  identity first is killed by both.

### Fixed — the hardening batch: a console close at any stage of `gpu reserve`, `gpu owner-flags --config`, voice cards, honest cause wording, and the host-RAM guard's loose ends (F55, F56, F57, F58, F60, F63)

- **`gpu reserve` answers a console close at every stage, not only once its command runs.** The SIGTERM/Ctrl-C handler was
  registered after the card was opened, the queue wait (`acquireQueued`, `acquireAutoCards`) and the drain/unload loop, so a
  close during `--wait 8h` or a long drain found no handler: Go ended the process on the spot, its PAIR card stayed open until
  a sweep closed it "harness process exited before the job finished", and its waiter record stayed in the line. The handler
  (`signalWatch`) is now the first thing the wrapper does, and each stage returns on it: a queued `Acquire` ends at its next
  poll through the new `gpulease.Options.Cancel` (`ErrCancelled`; the waiter leaves through `Acquire`'s own defer, nothing is
  claimed, a cancel closed before the call asks for nothing even on a free card), `--cards` refuses the allocator's next read
  and wakes its sleeps, the drain's context is cancelled, and checkpoints sit before the card opens and before the command
  starts. The card closes failed with the stage and the cause ("ended on SIGTERM while queued (...); never started"), the lease
  is released, and the "NOT warming the seat back; the warm stays owed" sentence is printed only when a warm is owed (a signal
  during the drain unloaded nothing, so none is). Pinned at each stage with the signal delivered through the wrapper's seam
  (`TestASignalWhileQueued...`, `...DuringTheDrain...`, `...BeforeTheLeaseIsAskedFor...`, `...WhileChoosingCards...`,
  `TestACancelledAcquire...`, and, from the review of this batch, `...BetweenTheDrainAndTheCommandStartsNothing` for the checkpoint
  between the drain and the command's start, which deleting left every test green, and `...WhileQueuedAgainAfterALostLeaseNamesTheSignal`
  for the re-queue after a lease lost during the drain, whose cancel used to come back as "the wait for the card was cancelled by its
  caller" with no signal named); a real console close still cannot be driven from a test. Two more faults of the same stage work, found
  by the same review: a signal that cancels the **unload request** can arrive after llama-swap has already taken the seat down, and
  the debt was written only after the request returned, so the seat stayed cold with nothing owing its reload (a cancelled unload
  of a seat that was resident now records the debt; a warm of a seat that is in fact still loaded costs one health request,
  `TestASignalDuringTheUnloadRequestLeavesTheWarmOwed`); and the SIGTERM card text for the pre-start stage was 141 characters
  against the 140 the card keeps, so it ended "never start..." (the cause clause is shortened and every stage text is asserted to
  fit whole, `TestEveryStageTextFitsTheCardWhole`). The stage tests became deterministic or reached further: the pre-card
  checkpoint test waits for the watch to record the signal (`wrapperBeforeCard`; before, it relied on the scheduler running the
  feed and the watch first, and failed with a delay injected into the watch), `--cards` is driven through `gpu reserve` itself
  (`TestASignalWhileChoosingCardsThroughTheReserveEndsTheWait`: the lease line's cancel and the allocator's wake-up sleep are wired
  by the wrapper, which the loop-level test above could not see), and the re-queue is driven from a cancelled unload to the signal
  (`TestASignalWhileQueuedAgainAfterACancelledUnload...`), the one path where a wrapper that queues again owes a warm.
  **A second signal is still dropped, and that is now said instead of justified
  by the OS:** the OS ends the process after its grace for a console close, a logoff and a shutdown only, so a second Ctrl-C or a
  plain kill during a stage that never consults the signal (the plan's card-table read, up to 20 s; the stop of the ComfyUI instances
  kept under the lease, up to 60 s; the seat's warm-back after a Ctrl-C, minutes) waits for that stage, where before this batch the
  stages ahead of the lease had no handler at all and Ctrl-C ended the process with its card open. Ending the process at once on a
  second signal is not done here (an exit skips the deferred release and leaves the instances running).
- **`gpu --config <path> owner-flags` no longer exits 2.** `runGPUOwnerFlags` defined only `--pid`, so the hoisted `--config`
  (F35) was refused by its flag set while the other six verbs took it. It accepts and ignores the flag, and
  `TestEveryGPUVerbAcceptsTheHoistedConfig` runs every verb of `gpuVerbs` in a child with `--config` before and after the verb
  (a flag set exits the process, so it cannot be caught in-process).
- **A voice render on the `tts_endpoint` lane marks its PAIR card running.** The lane takes no media lease (the server owns its
  GPU) and so never reached `core.MarkWorking`, which every other lane reaches through its lease: its card was queued for the
  whole synthesis and a sweep of a card whose process died mid-synthesis said the call never started. `runVoiceEndpoint` marks
  the card right before its request, and a call that defers on its input marks nothing. `TestOnlyTranscribeNeverMarksWorking`
  said it pinned the list of lanes that never mark "in both directions" and counted marks (">= 5"), which the tts lane passed
  through; it now names the lane functions of every long-call task (checked against `longCallTasks` both ways) and requires each
  to reach a mark by name, transcribe's to reach none. Reachability says a lane marks somewhere, not on every path, so the lease
  path has a behavioural test of its own (queued behind another lease: not marked; holding the card: marked once).
- **Cards and logs no longer claim a cause they cannot know.** `signalWhy` and the warm-skipped sentence said the console was
  closed, or the session was ending, or the machine shutting down, for any SIGTERM; a plain kill sends the same signal (and on
  Linux it is the only way it comes). They now name the signal and give the causes as likely ("ended on SIGTERM: likely a console
  close, a logoff or a shutdown; a plain kill sends it too"). The sentence the wrapper prints in place of the seat's warm-back says
  what the signal ended: "the command was ended on SIGTERM" only when a command ran; for a signal that found the wrapper earlier it
  says the wrapper was ended at that stage ("the wrapper was ended on SIGTERM while draining the seat, before its command started",
  likewise "while queued, ...", and "the wrapper was ended on SIGTERM before its command started" for the last stage, which names
  that itself), where it used to say the command was ended even when none had ever run (`warmSkippedOnTerm`). The sweep's log line
  counted every card it closed as
  "harness process exited before the job finished", which is false of a seat-watch stretch (closed completed since C04) and of a
  terminal frame its producer built and could not post: the count is split by kind, and a sweep that closed orphans alone logs the
  line it always did.
- **F55: the lane probe reads a slow card table once more before it gives up, and says so when it still cannot.** The probe
  wrapped the caller's context in its 4 s bound and called `gpualloc.Deps.CardTable` under it; `CardTable` retries only a read
  that ran out of time while the caller's context was still good, and the caller's context was the 4 s one, so the admission's
  retry (5 s, then 15 s) was unreachable here: a `nvidia-smi` that was merely slow made every probe answer "not judged" at 4 s,
  silently. Both probes (image and `run_graph`) now read through `laneCardTable`: 4 s, one retry under 10 s, one `media lane probe`
  log line when the table is still unread (none when the caller's own context ended: a client that disconnected mid-probe is not a
  card-table fault, `TestTheLaneProbeLogsNothingWhenTheCallerWentAway`). A table that stays unreadable fails every question the
  same way, and a ticket scan asks once per waiting ticket every 10 s, so the line is written for the first occurrence and then once
  a minute per probe and cause while the fault lasts (`laneFaultLog`, kept per process;
  `TestTheLaneProbeLogsAPersistentCardTableFaultOncePerMinute`). **The 10 s is chosen, not measured** (shorter than the
  admission's 15 s because a ticket scan asks the lane question once per waiting ticket: worst case 14 s per question, not 20).
- **F56: the host-RAM calibration reads a high percentile of the last twenty runs, clamped, and says so once per key.** It took
  the all-time max of a key's resident peaks, so one inflated run raised a family's declaration until `footprints.json` was
  edited by hand, a figure above what the box can ever admit made the lane read "can never admit" for every later call (`gpu
  reserve --ram` is no remedy for a pipeline call), and the log note carried the run count, so each render added a map entry and
  a log line. The store keeps the last 20 runs per key (`host_recent`, additive) and `HostPeak` answers with their 90th percentile
  by nearest rank (under ten runs that is the maximum, so outlier rejection starts where there is something to reject against; a
  bad run leaves the window after twenty more); the declaration is clamped at twice the model files' sum and says which percentile
  of how many runs it read and whether it was clamped; the note is made once per key and figure. A store written before the window
  lets its maxima stand in, once, for the runs it does not hold; the wire fields stay the all-time maxima. `RecordHost` now merges
  before it folds its own run (merging after replaced the fresh run with the other process's window, and lost its run count).
  **Window 20, percentile 90 and clamp 2x are chosen, not measured.**
- **F57: a card-scoped grant takes its host readings before the epoch lock.** The check ran in full inside the machine-wide lock: the
  host's counters and, for every live lease that declared a need, a walk of its holder's process tree with a memory read per process
  (about 26 ms median, 63 ms worst for one holder, by the reviewer's measurement, on every grant attempt and every waiter poll,
  inside a lock everyone else spins on for 2 s). `readHost` takes them before the lock and `hostRAMCheckWith` compares them inside:
  the decision is still made against the leases live then, so a grant that lands between the reading and the lock is counted
  (`TestAGrantThatLandsAfterTheReadingIsStillCountedInsideTheLock`, through a test-only hook), and the early parts err the
  conservative way. `TestTheHostReadingsAreTakenBeforeTheEpochLockAndOnlyComparedInside` counts the reads made while the lock file
  exists: none. A held card now costs no readings at all.
- **F58: askRAM is sized on the cards the plan names, and the two orderings it rests on are pinned.** The admission and the probe
  sized the figure on the largest card of the whole table even for a pin, a pool or a declared device, while the lease the call then
  takes declares on those cards: on a mixed box a krea2 render pinned to a 16 GiB card declared 32.8 GiB at the grant and 0 in the
  pre-filter, so a waiter that waits only on host RAM was not counted ahead of it (`askRAMFor`). The grant already asked the cards
  before the memory on both paths, and `BuildInput` already fed the allocator what granted leases have yet to load, but nothing
  pinned either (the swap and the deleted line left every test green); both are pinned now. The admission's half of the sizing is
  pinned by the size it asks for (`TestTheAdmissionSizesItsFigureOnTheCardsThePlanNames` drives the real admission and records the
  VRAM of each sizing request), not by its answer: the queued answer is the same whichever card the figure was sized on, so no test
  of the answer can tell the two apart (the earlier pin searched the source text for the call).
- **F60: the sd.cpp lanes declare the weights they park in RAM.** The class default is the largest ComfyUI family a box binds, so an
  sd.cpp-only node declared 0 for its image, video and animate lanes and the 5.5 to 6.2 GiB `--offload-to-cpu` parks in RAM there
  were outside the brake. `hostneed.ForSdcpp` sizes the bound files in full when the lane's extra args carry the flag (nothing
  without it), and each lane declares the larger of the class default (unchanged) and its own bytes. A file that cannot be sized
  (missing, empty) does not take the estimate with it, which the review of this batch showed it did: the class default is 0 on an
  sd.cpp-only box, so one mistyped VAE path put the whole spill back outside the brake with no line. The files that can be sized
  are summed and worded as a **lower bound** (`Need.Unsized` names the rest by binding key), and the lane says once per lane and set
  of files which it could not size; when none can be sized the class default stands and the lane says that once too.
  **It is an estimate from the files' sizes, not a measurement, and says so**; the need carries the lane's footprint key, so
  three measured runs raise it like a ComfyUI family's. Not covered (the named limits, last bullet of this batch): the audio.cpp lanes, the animate lane's depth model, and
  `gpu reserve` around an sd.cpp helper (the CLI recognises only the ComfyUI helpers).
- **F63: the deletion-guard test no longer depends on which shell started `go test`.** On a plain PowerShell PATH the name `bash` is
  the WSL launcher, which drops the backslashes of the Windows script path, so the test failed from PowerShell and passed from Git
  Bash (reproduced both ways). It finds Git's own bash from `git --exec-path`, puts Git's `usr/bin` first on the guard's PATH
  (started by path from a PowerShell it finds no `cut`), passes the script with forward slashes, and skips with the reason when the
  only bash is the launcher.
- **`gpu doctor` reads this build's version whatever literal the linker lays next to its marker.** The build marker
  (`offload-build-version=0.179.0`) was embedded with no terminator, the audit reads the version as the longest run of
  `[0-9A-Za-z.+-]` after the prefix, and the linker packs string literals back to back: a new error format beginning "interrupted"
  made the built binary read back as `0.179.0interrupted`, which `TestBuiltBinaryCarriesTheFormatSignatureAndBuildMarker` caught in
  the final gate of this batch. The embedded marker now ends in a NUL (`embeddedBuildMarker`; the doctor's JSON still shows it without),
  which every reader, old and new, stops at; before, any literal added anywhere could have done the same.
- **Not changed, and said.** The `--detach` form's hidden holder queues in its own process and has no signal handling of its own
  here. F60's three limits, one by one: the audio.cpp voice and music lanes keep the class default (0 on a node that binds no
  ComfyUI family), so what they park, if anything, is outside the brake; the animate lane's depth model (a separate, small binary)
  is not counted in its figure; and `gpu reserve` around an sd.cpp helper declares the class default or what `--ram` says, not the
  weights `--offload-to-cpu` parks (the CLI recognises only the ComfyUI helpers). The comment on `keepLoop` in
  `internal/mediaremote/scan.go` now says a lane question is bounded by `laneProbeBound` plus
  the retry (a comment-only change). Nothing here was measured on a node: every figure
  above that is new (10 s, 20, 90, 2x, a minute) is chosen, and no real `gpu reserve`, ComfyUI, PAIR or llama-swap was touched.

### Fixed — a re-placement after a refusal never reuses a fleet snapshot whose probe began at the refusal's own clock reading (F66)

- **The bug.** A subtask refused for capacity is offered to another node from a fresh read of the fleet: the run's cached probe
  (2 s) is reused only when it was taken after the refusal, so a node the run's own siblings had just filled is not picked again.
  The comparison was "at or after". On a clock that ticks coarsely (Windows: a millisecond or so) a probe that ended before the
  refusal reads the same as the refusal when both fall in one tick, and the stale picture was reused. It is now "strictly after":
  a probe that began at the refusal's own reading is read again, one more probe in a rare tie. `TestRunSpreadReplacementReprobesTheFleet`
  failed 46 and 50 times in 400 on a Windows PC and never in 800 on Linux; `TestAProbeMemoThatBeganAtTheInstantOfTheRefusalIsNotReused`
  pins the boundary on the readings themselves.

### Fixed — tests that depended on the machine that ran them (no product change)

- **`internal/sandbox` `TestWindowsLowILWriteConfinement` passed only when the temp directory sat under the user profile.** The cage lowers a worktree's mandatory integrity label for a run, which needs `WRITE_OWNER` on it, and owning a directory does not grant that. On a drive whose root ACL gives Authenticated Users "Modify" and the user nothing by name, the label change is refused ("Access is denied"), `Run` goes on without lowering (best effort, fails safe) and a low-integrity child cannot write the worktree, so the test failed on any temp directory off the profile. The test's worktree now comes from `labelableTempDir`, which grants the owner an explicit full-control entry and leaves the labelling to `Run`, so the test proves the cage's labelling and not the temp directory's inherited ACL (disabling `applyLowIntegrityLabel` fails it on every drive). `TestWindowsRelabelRevertedAfterRun` takes the same directory: on such a drive it had passed without ever seeing a label to revert (a no-op revert passed it). That entry changes no product code: the cage still proceeds unlabelled when the label change is refused, and as of the next section it says so.
- **The root package's `TestGenerateImageBatchExitCodesThroughTheBuiltBinary` failed whenever the machine's committed memory was high.** The test's config named a family the host-RAM guard cannot size but bound no ComfyUI install, and on Windows an unset `comfy_dir` is the reference install, so the unsizable family fell back to the media class default (the largest render family the box binds, 33.1 GiB of Wan 2.2 weights). The built binary is a child process and reads the real host, so at 89.9 GiB committed of 127.7 it deferred the batch as "waiting for host RAM" and exited 0 where 0, 4 and 1 were expected. The fixture now sets `comfy_dir` to `""` (`hostneed.ClassDefault`: no ComfyUI install bound, nothing to size, so the batch declares no host RAM and the brake has nothing to judge), and the test fails on every host, not only a loaded one, if the child ever logs a host-RAM declaration again. Config only: no production seam was added, and the in-process batch tests, which already ran on a stand-in host, now declare nothing as well.
- **`internal/sandbox` `TestCageMasksDotGit` failed on any host that refuses the cage's unprivileged mounts.** The mask it proves is a tmpfs mounted over `<worktree>/.git` inside the worker's user and mount namespace, and a host can refuse that mount while the rest of the cage works. The test skipped only when `GITHUB_ACTIONS` was `true`, so on a host whose AppArmor restricts unprivileged user namespaces (`kernel.apparmor_restrict_unprivileged_userns=1`, the Ubuntu 24.04 default; measured on one of the two Linux hosts the cage tests were gated on, where the mount fails with `permission denied`) the write reached the real `.git` and the test failed with `SECURITY:`, on the base commit of this branch as well. It now asks the cage whether the mask was refused (`hostRefusesTheCagesMounts`, the control `TestCageMasksTheDotGitFileOfALinkedWorktree` uses), checks that the note is the one callers are promised (`[sandbox] ... NOT masked read-only (mount: ...)`), and skips with that note as the measured reason; on the host above it went from a failure to a skip, and on the host where the mount works it still runs for real. The skip depends on the note: a host that refuses the mount and says nothing is not skipped and fails the assertion as before (with `maskDotGit` made to swallow the refusal, the test fails on that host), and `TestCageSaysWhenDotGitIsNotMasked` holds the cage to saying so everywhere. The blanket skip on `GITHUB_ACTIONS` is gone: a runner that refuses the mount skips with the cage's note, and one that allows it now runs the assertion. Test only.

### Fixed — the cage said nothing when it could not make a folder writable, put a label back, end a command at its deadline, or mask `.git`

- **The defect.** On Windows `Run` lowers the mandatory integrity label of the worktree (when it is writable) and of the scratch folder so its low-integrity child can write them, and restores the label afterwards. Each step can fail, and each failure was dropped. Setting a label needs `WRITE_OWNER` on the folder, which owning a directory does not grant and which a data drive whose root ACL names the user nowhere does not either: the lowering was refused, `Run` went on, and the child could not write a folder the caller had asked to be writable. The caller saw the child's own `Access is denied` and nothing that explained it. A restore that was refused left the folder at low integrity, writable by every low-integrity process on the machine, with nobody told, and the next run read that leftover label as the folder's own and restored it again, so it never healed: that is the worst of the failures. A prior label that could not be read was taken for "no label", so the restore removed whatever label the folder had. A kill of the command's job at its deadline that failed left `Run` waiting for the command to finish by itself, with the caller's time limit not in force, and the timeout note then said the command had been killed.
- **The fix.** Each is now a note. `Run` puts a line starting with `[sandbox] ` ahead of the command's own stderr in `Result.Stderr`, naming the folder (or the limit) and the cause; appends the same text to the error of a run that fails before the command starts (there is no stderr then); and logs it once (`sandbox: ...`). The wording says what the caller lost (a folder it cannot write, a label that was not restored, a deadline that was not enforced), not "confinement is off": a refused lowering fails safe, because the child runs at low integrity either way. A failed kill no longer claims a kill. A run with nothing to report is what it was, byte for byte. **Whether the command runs is unchanged**: it still runs when a label is refused or a kill fails. Failing closed there is a separate decision, parked with the operator. The Win32 calls go through three package variables (`setLabelSACL`, `getLabelSD`, `terminateJob`), so `TestWindowsLabelRefusalIsReportedToTheCaller` and six more refuse them on demand and assert the exact cause and folder in `Result.Stderr`, in the error and in the log, instead of depending on the ACLs of the machine that runs them (a clean run asserts the opposite: nothing on stderr, nothing logged).
- **The same defect on Linux.** The read-only tmpfs over `<worktree>/.git`, which keeps a shell with a writable worktree from planting a git hook that fires on the host later, is a mount, and the worker dropped its error. Where the kernel refuses it (an unprivileged user namespace that may not mount, as in some container and CI sandboxes) the shell ran with a writable `.git` and nobody was told. The worker now writes `[sandbox] <worktree>/.git is NOT masked read-only (mount: ...)` to stderr after its last refusable step and right before the exec (`TestMaskDotGitSaysWhenTheKernelRefusesTheMount`, and `TestCageSaysWhenDotGitIsNotMasked`, which ties the note to what the kernel actually did on the host it runs on). `Result.Refused` reads through such lines, so a worker that wrote one and then failed to exec is still a refusal and not a command that exited 127 (`TestRefusedByReadsThroughNotes`). The mask is still not fail-closed.
- **Looked at and left alone, with the reason.** Linux: the `/proc` mount and the mount-propagation call (defense in depth behind Landlock, which denies `/proc` whether or not the mount worked); the two `setrlimit` calls (documented best effort; a failure leaves the stricter limit already in force); `IgnoreIfMissing` on the Landlock rules (a skipped grant only removes access). Windows: the inheritance flag on the capture pipes' read ends and the `TerminateProcess` of a child whose job assignment failed (it never ran); the pipe reads and the handle closes (result fidelity, not confinement).

### Fixed — the Linux cage left a linked worktree's `.git` file writable, so a command could aim the host's git at a directory of its own

- **The defect.** The cage masks `<worktree>/.git` for a shell with a writable worktree, so that it cannot plant a git hook or rewrite a config that runs later, on the host, the next time git is used there. It did so only where `.git` is a directory. In a linked worktree, which is how the harness's own worktrees are made (`git worktree add`), `.git` is a FILE holding `gitdir: <repo>/.git/worktrees/<name>`, and nothing masked it. The git directory it names is outside the cage's write roots, but the pointer sits in the writable worktree: a command could rewrite the file, or rename another over it, to name a directory it controls, hooks and config included, and the next git command run on the host in that worktree would follow it. Measured on two Linux hosts before the fix: rewrite, append, truncate, a hard link written through, rename over and rename away of the file all succeeded, and the cage said nothing.
- **The fix.** For a writable worktree whose `.git` is a regular file, the worker bind-mounts the file onto itself and remounts that bind read-only (`maskDotGitFile`), in the cage's private mount namespace, so nothing on the host changes. Measured with the worktree under the temp directory and under a shared-memory mount: the file still reads (git run in the cage over the worktree reports its status and its history), and every attempt above is refused: rewrite, append and truncate with `EROFS`, a link to it with `EXDEV` (the read-only flag belongs to the mount, so a second name on the worktree's own mount would be a way around it, and a link across the two mounts is refused), rename over, rename away and remove with `EBUSY`; the file is byte for byte what it was. The remount names read-only, nosuid, nodev and noexec and no atime flag, because a user namespace locks those flags on every mount it inherits and the kernel refuses a remount that clears one (measured: with the nosuid, nodev and noexec left out, the remount of a worktree under the shared-memory mount, which is nosuid and nodev, fails with `operation not permitted`), and a remount that names no atime flag keeps the mount's own. A refused bind or remount is the same kind of note as the directory's, `[sandbox] <worktree>/.git is NOT masked read-only (bind mount: ...)` or `(read-only remount: ...)`, written before the exec. The command still runs, as it does when the directory mask is refused, and failing closed stays a separate decision.
- **Tests.** `TestCageMasksTheDotGitFileOfALinkedWorktree` builds the linked worktree with git itself, makes seven attempts from inside the cage, and fails with `SECURITY:` unless every one is refused and the entry is still the same file with the same bytes; it runs on the temp directory and on a shared-memory mount, where the locked flags bite. `TestCageSaysWhenTheDotGitFileIsNotMasked` ties the note to what the kernel did (a write reaches the file exactly when the cage says it is not masked) and runs on every host. `TestCageStillReadsALinkedWorktree` runs `git status` and `git log` in the cage over the masked worktree. `TestMaskDotGitFileBindsItReadOnly`, `TestMaskDotGitFileSaysWhenTheKernelRefuses` and `TestMaskDotGitFileStaysQuietWhenThereIsNothingToSay` stand in for the kernel and pin the two calls and their flags, the note for either refusal, and the cases that make no mount. A host that refuses the cage's unprivileged mounts, as a CI runner or a host whose AppArmor restricts unprivileged user namespaces does, cannot show the mask: the first test then asks the directory mask whether the host mounts at all and skips with the cage's note when it does not, and fails when the host mounts over a `.git` directory and refuses the file.
- **Looked at and left alone, with the reason.** A `.git` inside a nested repository below the worktree root, which the mask never covered, directory or file. (A `.git` that is a symlink is reported, below.) The cage grants no read of a linked worktree's git directory (`run` and `run_shell` pass no `ReadDirs`), so `git status` in the cage over a linked worktree reports `fatal: not a git repository: <git directory>` unless the caller grants that directory (measured with the mask in place; the mask is on the pointer file, not on the git directory, and the tests above grant it as a caller that wants git to work there must): that predates the mask, and granting reads is a separate decision.

### Fixed — a Windows run whose exit status could not be read reported exit 0, which every caller reads as success

- **The defect.** `Run` reads how the command ended with `GetExitCodeProcess` and set `Result.ExitCode` only when the call succeeded. When it failed, the command had run and its output was in hand, and `ExitCode` stayed at its zero value: the run read as a success, which is what every caller takes 0 to mean (`run` and `run_shell` print `exit=0` for it). A command that had failed, or that the job had ended, was reported as having worked, with nothing to say otherwise.
- **The fix.** The call goes through a package variable (`readExitCode`), like the three Win32 calls the notes above cover. When it fails, `ExitCode` is -1 (`noExitStatus`, the value `os.ProcessState.ExitCode` gives a process it has no code for) and the caller gets a note ahead of the command's stderr, `[sandbox] the command's exit status could not be read (GetExitCodeProcess: ...): its exit code is reported as -1, not 0, because a run whose outcome is unknown is not a success`, which is also logged once. That is how this file already reports a run that went ahead with a caveat (a command that exits non-zero comes back as a code and no error, one ended at its deadline as a code and a note), so the run is otherwise as it was: no error, no refusal, the output kept, and `internal/agent` shows the model `exit=-1` with the note in its stderr. `Result` documents the value.
- **Tests.** `TestWindowsUnreadableExitStatusIsNotTakenForSuccess` makes the read fail on demand and asserts the exact code, the cause in `Result.Stderr` (leading) and once in the log, and that the output is kept; `TestWindowsExitCodeReachesTheCaller` is the control: with the real call, exit codes 0, 1, 3 and 42 come back as they are, with nothing on stderr and nothing logged. Both have skip stubs on the other platforms. Each of three mutations (the error ignored again, the note dropped, the code left at 0) fails the first test; the same overlay with the unmutated file passes.

### Fixed — a `.git` symlink in a writable worktree was unprotected, and the Linux cage said nothing

- **The defect.** `maskDotGit` mounts over `.git` where it is a directory or a regular file and returned nothing for anything else, and a symbolic link was something else: a mount follows the link, so none can protect the link itself, which sits in the writable worktree. A command in the cage could remove it and put a `.git` of its own in its place (a link or a directory), and the next git command run on the host in that worktree would follow it, as it would have followed a rewritten pointer file. A link to a directory was only half covered (the tmpfs went over the target, which protects the target and not the link), and for a link to a file or to nothing the cage did nothing at all. Neither `git worktree add` nor `git init` makes such a link, and nothing told the caller one was there.
- **The fix.** The cage reports it: `[sandbox] <worktree>/.git is a symbolic link: it cannot be masked read-only, so a command in this cage can replace it and point git at a directory of its own`, to stderr just before the exec like every other note, for a link to a file, to a directory and to nothing. The command still runs, and what the cage did before it still does: a link to a directory keeps the read-only tmpfs over its target, and a refusal of that mount is reported too, in the same line. A worktree the command cannot write and a worktree with no `.git` get no note. The check is on the link itself (`Lstat`), so a link to a regular file is no longer taken for the file it points at.
- **Tests.** `TestMaskDotGitSaysWhenDotGitIsASymlink` stands in for the kernel and pins the note, the mounts made (none for a link to a file or to nothing, one for a link to a directory) and the quiet cases. `TestCageSaysWhenDotGitIsASymlink` runs the real cage for each kind of link, asserts that the note leads stderr and that it is true: the command removes the link and puts one of its own there, and the host sees it afterwards. It does not depend on what the host's kernel allows. The case that pinned "no mount and no note" for a symlink in `TestMaskDotGitFileStaysQuietWhenThereIsNothingToSay` is gone with that behavior.

### Fixed

- Tests on Windows: the fake `ffmpeg` of the `audioio`, `fleetnode`, `pipeline` and `sttremote` tests was a hard link to the running test binary, which some Windows hosts will not unlink ("Access is denied" in the `TempDir` cleanup, 51 cleanup failures on one host); it is now a copy installed by the new `internal/fakeexe` test helper, which removes its own directory and starts the stand-in once so its slow first start is outside a test's time budget (test-only, no product code changed).
- The `internal/fakeexe` test helper keeps a stand-in's folder inside the calling test's own `t.TempDir` (removed first, with a bounded retry on Windows) instead of a folder of its own in the shared temp directory, and a stand-in that runs tests instead of answering as a tool cannot start another (the warm-up start exits in the helper's `init`; one that reaches `Dir` or `Install` exits with status 97), so a killed run leaves nothing outside its own temp root and the helper cannot recurse (`TestAHelperCallStaysInsideTheTestsOwnTempDirAndIsGoneWhenItsTestEnds`, `TestAWarmUpStartExitsBeforeAnyTestRuns`, `TestAStandInThatRunsTestsRefusesToInstallAnother`).

### Fixed — a run of the render tests left about 150 folders in the OS temp dir, and three pieces of render code left scratch behind

- **The defect.** `node --test render/*.test.mjs` made its scratch folders with a bare `mkdtempSync(join(tmpdir(), ...))`. Most tests
  never removed theirs and some removed theirs with an `rmSync` on the success path only, so one run left 149 folders (counted with
  the OS temp dir pointed at an empty folder), and a workstation that runs the suite all day collected thousands of them on its system
  drive (4,674 matching folders, about 0.9 GB, in one OS temp dir).
- **The fix, in the tests.** New `render/test-scratch.mjs`: `scratchDir(t, prefix)` makes the folder and registers its removal with the test
  that asked for it, so it goes when that test ends. node:test runs a test's `after` hooks whether the test passed or failed, so a failing
  run leaves nothing either (`render/test-scratch.test.mjs` proves it with a child run that fails on purpose). 72 raw `mkdtempSync` calls in
  21 test files use it, and the fixture builders (`fixture`, `keyedHarness`, `cliFixture`, ...) take the test's `t` and pass it on. A call
  without `t` is refused before any folder is made. The same file fails on a new raw `mkdtemp` in a render test or in one of its two
  support modules unless it is listed there with the reason its removal is on every path (22 remain, each removed in a `finally`, by an
  exit hook, or by the test's own after-hook).
- **The fix, in the code.** Three places in `render/` made scratch and did not remove it on every path. `igpu-stub.mjs` (test support)
  built its Windows launcher into one per-machine cache folder, `igpu-stub-launcher-v1`, that nothing removed, and every process that
  raced to build it left its own `build-<pid>-<ms>` folder inside it; it is now built per process into a folder that goes when the
  process ends (a cold build measured 0.8 s). `manifest-satisfy.mjs`: `resolveDeps` wrote the host-constraints file and the
  unified pack lock as pid-named files in the OS temp dir and never removed them, so every provisioning run left two; both now live in a
  folder of the step's own that is removed when the step ends, passed or failed. `compose-hyperframes.mjs`: the render work dir was
  made a few statements before the `try` whose `finally` removes it, so a throw in between left an empty `job-*` folder in the cache for
  good; the `try` now opens right after the folder exists. Each has a test that fails without the change.
- **Measured (Windows).** With the OS temp dir pointed at an empty folder, one run of `node --test render/*.test.mjs` left 149 folders before
  and 0 after (twice); the 1,286 tests that were there are all still there and pass (23 skipped, as before), and the run has 12 more.

## [0.180.0] - 2026-10-10 - a busy image call waits for the first matching lane anywhere in the cluster, graphs overflow to a node that holds their files, and a live render keeps its lease

### Fixed — the ticket queue was off on any machine whose config sets no `state_dir`, which is the usual config (ADR 0083)

- **The defect.** The ticket store opened only when `state_dir` was set (`storeFor`), and nothing resolved the platform default the
  way the GPU lease does, so on a machine that leaves `state_dir` unset a busy image call fell back to the local line, a whole-node
  graph took no claim, and `offload_status.media_queue[]` never listed a ticket. Every test of the queue set `state_dir` in its
  fixture, so the default every real config takes was never exercised.
- **The fix.** The queue, its tickets and both kinds of node claim (a ticket's and a graph call's) live under the state root the GPU
  lease resolves, through the one function that resolves it (`gpulease.ResolveStateRoot`: `state_dir`, else `LOCAL_OFFLOAD_STATE_DIR`,
  else `%ProgramData%\local-offload` on Windows or `/var/lib/local-offload` elsewhere), so the tickets, the claims and the lease's
  place tokens of a machine share one root and no config key is needed (`TestTicketQueueNeedsNoStateDirKey`,
  `TestAGraphReadsTheTicketClaimsOfTheDefaultStateRoot`, `TestTicketStoreAndLeaseShareOneStateRoot`). A root the lease refuses (a
  cloud-synced folder) keeps no queue, and says so once in the log with the lease's reason (`TestARefusedStateRootKeepsNoQueueAndSaysSo`);
  before, a `state_dir` in such a folder opened a queue the lease itself would have refused. The package's tests now run against a
  throwaway state root (`TestMain`), as the other packages' do, so a fixture with no `state_dir` cannot write tickets under a real one.
  No other S2 or S4 code keys on `state_dir`.

### Added — a busy image call is a ticket the harness holds until the first matching lane frees, anywhere (the P0's "even when every machine is full", ADR 0083)

- **The behaviour.** An `auto` image call (`offload_generate_image`, `generate-image`) on a machine that has the lane and a fleet, whose
  lane is not free, used to overflow once to an idle matching node and otherwise fall back to the local line, joining it late; a
  caller that looped on the queued answer re-sent the same request until something freed (85 answers in one recorded loop). It is now
  a **ticket**: the harness holds the job and starts it on the first matching lane that frees, this machine's own or a node's, in the
  order the call arrived, with no caller loop. A free lane runs the call here exactly as before; a box with no `delegate_remotes`
  asks nothing; `route local` and `route remote` are unchanged.
- **One request, one ticket.** A ticket is a file, `<state root>/media-queue/mq-<id>.json`, under the state root the GPU lease uses (no `state_dir` is needed), any process of the machine can read. Its
  identity is the sha256 of the task, the prompt and the parameters without the `waiter_token`: **any identical re-send attaches to
  the live ticket**, with or without a token (`TestIdenticalResendAttachesToTheLiveTicket`). `waiter_token=mq-...` collects the
  result; a token of another request is refused by name; an unknown one is a new arrival; an `mq-` token never reaches the lease line.
  The result a call takes stays on file, marked collected, for 2 minutes (chosen), so the other calls waiting for the same ticket
  (an MCP door and a CLI of another process, or a second caller naming the token) get it too and nothing renders twice; a request
  that only repeats the content after that is a new job. The ticket **fixes one seed** at creation when the caller gave none (1 to
  1e9), so a bounce, a re-placement, an adoption and a local run render one composition (`TestTicketFixesOneSeed`).
- **It keeps its seniority in the machine's own line.** New `core.PlaceKeeper` (the pipeline implements it, the MCP runner forwards
  it) wraps the lease manager's place-keeping tokens and adds no admission logic: a place is left at creation with the ticket's
  arrival time on the cards the real admission would queue the call on, touched on its own rhythm (a keeper that does not wait for
  the scan, so a scan that spends its time in slow lane questions cannot let the places behind it lapse), left again with the same
  arrival time if it lapsed, let go when the ticket ends, and spent by the final local run as its `waiter_token`. **The lane question for a call
  that resumes a place is now ordered by arrival on every plan** (`gpualloc.QueuedClaimsBefore`); before, two places on one idle card
  each read the other as in its way and neither would ever have been served. (`TestMediaLaneFreeOrdersByArrivalOnlyOnTheWholeNodePlan`
  became `...ForACallThatResumesItsPlace`, and the pinned-plan row of the differential no longer requires the prober to call that state
  busy; it still must never call busy a lane the real wait-0 admission grants.) A host that does not lease cards keeps no places; the
  ticket waits without local seniority and the answer says so.
- **One scanner per process, local first, then the fleet, late binding.** Every 10 s +/- 20 % (a chosen transport constant) the
  scanner decides for each waiting ticket, oldest first, and never runs a job: a job runs in a goroutine of its own, so a local run
  waiting in the lease line stalls neither another ticket's scan nor the touch that keeps each place alive. It starts the local run
  when the lane question says the ticket would be served now (one ticket per scan), else asks the fleet with ADR 0082's own filter and
  ranking, minus the nodes a job of the machine has a claim on (a ticket's, or a whole-node graph call's) and the nodes an older
  waiting ticket of the machine matches. Nothing
  is sent to a node before a lane is free there. **A run on this machine's lane that the lane refuses at once although it read free**
  (`gpu_wait_ms` 0, or a younger place on the same card that a call without a wait cannot pass) does not make the scanner ask again at
  once: the ticket pauses the lane for 5, 15, 60 and then 300 s (the node steps) while the fleet is looked at, so no loop of runs, each
  writing the pipeline's own deferred row (`TestALostRaceOnTheLocalLaneIsNotARetryLoopAndTheFleetIsTriedMeanwhile`; 71 runs in 0.6 s
  before). **A lane the pipeline could not read is not a free lane**: when the card table does not read inside its 4 s bound, or the
  lease directory does not open, `core.LaneVerdict.Unjudged` is set, the call becomes a ticket, the fleet is looked at in the same scan
  and the run on this machine's lane is the answer only when no node can take the call (`TestAnUnreadLaneStillLooksAtTheFleet`); a call
  the prober does not model at all (another task, a binding without a lane, an inherited lease) still runs here at once. One node is
  tried per scan; a refusal or bounce pauses the node (5 s for a full node that names no `Retry-After`, 60 s then 300 s for a bounce)
  and the ticket goes on; the S1 bound of three nodes is gone, and a ticket ends only after 8 h without a lane.
- **A node a ticket has a job on is in use for every scanner on the machine** (`TestPlacedTicketMarksNodeInUse`): a claim file per
  node is written with the ticket's `placing` state, and the id of the job about to be sent, under a directory lock before the job is
  sent, so two processes never send one single-job node two jobs (`TestTwoProcessesNeverDispatchTwoTicketsToOneSingleJobNode`, which
  turns the ordering rule off so that only the claim stands between them). This closes the S1 open issue 6. **A whole-node graph call
  that overflows by itself** (`route` auto on a machine with the graph lane) **takes the same claim**, under the same lock, before it
  sends (a record marked `graph`, with no ticket file behind it, live while its process is, and, when the process is gone, only for the
  first 2 minutes after the claim was taken; next section): a ticket is never sent a node such a graph is running on, and the reverse
  (`TestATicketNeverSendsANodeAGraphHasAJobOn`, `TestAGraphAndATicketNeverHoldOneNodeTogether`). A call forced to a node (`route`
  remote, or `auto` on a machine with no lane for it, image or graph) takes no claim and reads none, as before this release: it is
  ranked by the nodes' own lease and queue answers, and the node's grant is what stands between it and a ticket's job.
- **A dead owner's claim holds its node for 2 minutes** (chosen), not a moment less: the job it sent keeps running on the node, which
  shows its lease only once the job is admitted, and in those seconds another process would have sent the node a second job.
  **A node that does not answer is logged once per backoff step** (5, 15, 60, then 300 s), never per scan.
- **Owners die.** A `waiting` or `local` ticket of a dead owner (process gone, or no beat for 10 minutes) is swept by the next scan or
  call of any process and its place let go; a `placed` or `fetching` one is **adopted by the next identical call**, which polls the
  recorded node job and fetches its outputs and never sends the request again, so the job renders once (`TestDeadOwnerPlacedTicketIsAdopted`:
  one POST in all, one ledger row, no second card). A finished ticket nobody collected stays 6 hours when its job succeeded (chosen: the
  caller was told to call once and come back for it) and 10 minutes when it failed (a repeat should try again). A ticket whose owner
  ended while the job was being sent is asked about at the node: a node that denies the job was never given it, and the ticket goes back
  in line with its arrival; a job the node accepted and passed back goes back in line too, and the node is left alone for a while.
- **The directory lock is an operating-system file lock** (`flock` / `LockFileEx`), not a file whose age decides whether it is stale: every
  way of breaking a stale file lock has a gap in which a slow contender removes the fresh lock of the one that broke it: on the first
  design 24 of 300 contended rounds in the review, and 14 of 200 in `TestTheDirectoryLockNeverHasTwoHolders`, had two holders inside.
  The system frees the lock when its holder's process ends, however it ends (`TestACrashedHoldersLockIsFreedWhenItsProcessEnds`).
  The lock file stays in the queue directory for good. A cancel that lands while the scan is claiming a node ends the ticket before
  anything is sent: the claim is let go and nothing reaches the node (`TestACancelDuringTheClaimSendsNothingAndFinishesOnce`; before, the
  job was sent after the cancel and the ticket was finished twice).
- **A scan writes no PAIR card, no ledger row and no lease waiter** (`TestScanNeverWritesCardsOrRows`, the C01 regression); the
  call's one row and one card open at dispatch.
- **The answer.** A call whose window ends first answers `{queued, scope: "cluster", ticket, waiter_token, position, local, cluster[],
  next}` as a capacity deferral of class `gpu_queued`: the position among the machine's waiting tickets, this machine's own lane (state,
  holder, callers ahead), and EVERY roster node in config order with its state (`free`, `busy`, `draining`, `not-capable`,
  `unreachable`, `in-use`, `bounced`, `refused`, `skipped`), why, the key that differs, who holds it and since when; `next` says the
  harness holds the job, not to call again, that a repeat attaches, and that `waiter_token=mq-...` only collects.
- **The doors.** `offload_generate_image` takes `wait_s` (default `gpu_wait_ms`; 0 answers after the ticket's first look; at most
  3600) and the ticket outlives the call. The CLI `generate-image` blocks until the job is done, prints where the ticket stands on
  stderr, and the first Ctrl-C cancels a ticket that sent nothing (a job already on a node cannot be recalled; the next identical
  command adopts it). `offload_status` gains `media_queue[]` (absent when there are none). The tool texts say not to call again.
- **A node added to `delegate_remotes` joins a running session.** An MCP session never reloads its config; the ticket re-reads
  `delegate_remotes` from the config file when its mtime or size changes (the server and the CLI pass the file), and only that key
  moves (`TestRosterReReadWithoutRestart`).
- **Closes** the S1 open issues 5 (a call carrying a `waiter_token` never overflowed, so a looping caller stayed in the local line) and
  6 (two processes could stack two jobs on one idle node), and the parked finding that a call that bounced off the fleet rejoined the
  local line from when it came back.
- **Changed tests, said.** 13 of the 35 S1 overflow tests that asserted the fallback to the local queue (or the order of a single
  call's attempts) now assert the ticket's answer (same rows, same reasons); 20 are unchanged and green (one of them needed a code fix: a
  node that answers 503 with no `Retry-After` is now left alone for the first backoff step, or every scan would send it the job again); `TestACallIsSentToAtMostThreeNodes` and `TestConcurrentCallsDoNotStackOnOneNode`
  are replaced by `TestABouncedAttemptGoesBackToWaitingAndTheNextScanTriesTheNextNode` and `TestPlacedTicketMarksNodeInUse`.
- **Not changed, and said.** One line per delegating machine (its state dir); two machines meet only at a node's own grant. A CLI ticket
  dies with the CLI. A repeat of an identical request while its result is uncollected returns that result (a second picture of one
  prompt differs in a seed). Each waiting ticket costs one lane question and one roster read per scan. A refused run on the local lane
  writes the pipeline's own deferred row (one per refusal, spaced by the pause). A process that is suspended for longer than 10 minutes
  reads as dead to the others until it writes its beat again (its waiting tickets can be swept meanwhile; a placed one is adopted, not
  re-sent). Nothing here was run against a live lease, a live node or a render; the scan interval, jitter, pause steps, owner and
  keep timings and the 8 h ceiling are chosen constants, not measurements.
- **Only an image call is a ticket.** A whole-node `run_graph` keeps its own one-shot overflow (the next section) and is **not** held
  by the queue: **a graph does not wait in the cluster queue yet** (named limit, see that section). Video, animation and audio calls,
  and an image call under a lease its process inherited, run as before.

### Added — a whole-node graph runs on the fleet: a node checks the files it names, one card only, and a VRAM floor (F32, ADR 0082 Amendment 1)

- **A node refuses a run-graph that names a file it does not hold, before any job exists.** `/fleet/dispatch` reads the graph at
  the door (`mediacap.ScanGraph`): any string input, under any key but `filename_prefix`, whose value ends `.safetensors`,
  `.gguf`, `.pth`, `.pt`, `.ckpt` or `.sft` and does not contain `://` is a weight file (a `ModelPatchLoader`'s `name` and a wrapper
  loader's `model` count, not only `*_name` keys; those two, the key `filename_prefix` and a value containing `://`, are the
  exclusions the code keeps), and the file input of `LoadImage`, `LoadImageMask`, `LoadImageOutput` (`image`), `LoadVideo` (`file`) and
  `LoadAudio` (`audio`) is an input file. A weight file must exist by name under any class directory of any models root, a class
  directory that is a symlink or a junction included (the same class-directory enumeration the host-RAM estimate and doctor's model
  bindings use); an input file must be in the
  ComfyUI input, output or temp directory. A node that lacks one answers **503** with the existing re-placeable route-not-ready
  refusal naming it (up to six names of each kind; past six weight files it says `7 or more files ... and more` and gives no count,
  because the node stops looking at the seventh: the door is open to any peer and a graph can name thousands of files, so the work is
  bounded by what the refusal shows and not by the graph), before a temp file, a lease or ComfyUI exists, so a delegator moves on instead of a
  node cold-starting ComfyUI to defer. The class-directory-link fix lives in `classDirs` (`internal/mediacap/modelbindings.go`), which
  doctor's model bindings and the host-RAM estimate read too, so on a node whose class directory is a link they now find the files
  behind it as well, not only this check. **Input files do not travel with a graph**, and the refusal says so. A model the manifest
  tells the node to fetch (`source_url`) is not refused; a name that is absolute or climbs with `..` is never looked up. The check is
  by name, not content.
- **A busy lane places a whole-node graph on an idle node.** `Pipeline.MediaLaneFree` answers for a `run_graph` that names no
  devices with the whole-node reading (any lease, slot or place in line; host RAM against the media class default) under the image
  probe's contract (a relaxation of the real admission, held by a differential test; it writes nothing). The one-shot overflow of
  0.179.0 now covers it, **as a path of its own for graphs**: an image call is a ticket in this release (the section above) and a
  graph is not. A busy graph is offered once to the idle nodes that can take it (no family and no digest travel; a node's 503 at the
  door moves the call to the next node, which, like any 503 that names no `Retry-After`, leaves that node alone for about 5 s in
  this process; a ticket's scan reads that node as refused for those seconds, and the cause kept for a graph's 503 does not call the
  node full, because the same 503 is a missing file), and when none admits it the graph waits in this machine's own queue, joining it when the fleet attempt is over and not
  from when it arrived; if that wait ends in a deferral, the deferral carries `cluster[]` naming, per node, the file it lacks (a call
  that then runs here and succeeds carries none). A graph with `devices`, a `waiter_token`, an inherited lease or an input file stays
  here. The graph path keeps one limit of the old image overflow that the ticket removed for images: a graph is sent to at most
  three nodes. **It shares the nodes with the tickets by the tickets' own two rules.** It takes the machine-wide claim of the node it
  uses, under the directory lock, before it sends (an in-process hold stands first, and is the whole cover on a machine whose lease
  root is refused and so keeps no queue), so no ticket and no other overflowing graph call, of this process or of another, is sent a
  node it is running on (a call forced to a node takes no claim; see the ticket section), and it leaves a
  node any such claim holds (`in-use`, `a job of this machine is running there (graph call, since ...)`;
  `TestTwoProcessesNeverStackTwoGraphsOnOneNode`). And it **yields to the tickets**: every ticket that is waiting is older than a
  graph that arrives now, so a node an older waiting ticket matched at its last scan is left to that ticket, and so is every node while
  that ticket has not been scanned yet (its first scan comes within about one scan interval of its creation); the row reads `free`,
  `idle, but ticket mq-... is older and first in line for it`, and the graph goes on to its own lane
  (`TestAGraphLeavesANodeAnOlderTicketIsFirstInLineFor`). **A lane the pipeline could not read is not a free lane for a graph
  either** (the card table past its 4 s bound, the lease directory not opening: `core.LaneVerdict.Unjudged`, as for an image): the
  fleet is looked at first, the placement reads `this machine's graph lane could not be judged (...)`, and the graph runs on this
  machine's lane when no node takes it (`TestAGraphWithAnUnreadLaneStillLooksAtTheFleet`, `TestMediaLaneFreeGraphIsFreeWhenTheCardTableCannotBeRead`,
  `TestMediaLaneFreeGraphIsUnjudgedWhenTheLeaseDirectoryCannotBeOpened`). Two machines (two state dirs) still meet only at a node's own grant.
- **A graph does not wait in the cluster queue yet** (a named limit of this release). It is not a ticket: it keeps no place in the
  cluster queue, does not start on a lane that frees after its one look at the fleet, and is not attached by content (a repeat of the
  same graph is a second call, not the first one's ticket). A graph that finds every lane busy therefore waits in this machine's
  own queue, as it did in 0.179.0, and its caller may need to call again with the `waiter_token` the local lane returned. Making a
  graph a ticket is a follow-up: a ticket's identity is the hash of the request's parameters, and a graph's parameters name files on
  this machine (`graph_path`, `manifest_path`) rather than carry them, so what makes two graph calls the same call needs a decision
  of its own.
- **A graph that reads an input file is not auto-overflowed.** Input files are the caller's own and carry generic names
  (`image.png`, `ComfyUI_00001_.png [output]`) that two machines share with different bytes, and a node can match one only by name,
  so a node holding a same-named file would have run the graph on the wrong bytes and answered ok with a sha-verified output. The
  call waits for this machine's own lane instead, and a deferral it ends in says why the fleet was not asked (`fleet_note` in its
  data, and in its reason). A call that must leave this machine anyway (`route remote`, or `auto` on a machine with no graph lane of
  its own, which has no queue to wait in) is placed as before, with the node's check by name only: the same-named-file risk
  remains there, a named limit. Sending a size and a hash of each file to the node, which would restore the overflow for these
  graphs, is a named seam, not built.
- **A whole-node graph is placed only on a node that publishes exactly one card.** A graph that names no devices runs in the default
  ComfyUI instance, which sees every card, and on a multi-GPU node could land on its display card. A node with more than one card is
  a named miss, and so is a node that publishes no `gpu_devices`. This covers every remote placement of a graph, `route remote` and a
  thin client included: this client no longer sends a graph to a multi-GPU node. (A graph sent to such a node by hand, or run on its
  own machine, is unchanged; that local case stays parked.)
- **`min_vram_gb` / `--min-vram-gb`, and `model_family` / `--model-family`, on `offload_run_graph` / `run-graph`.** Only a node whose
  largest `gpu_devices[].vram_total_gb` is at least N GB takes the graph; a card counts as the size it rounds to (nvidia-smi reports
  a 16 GB card as 15.92 GiB). With no argument, a graph that names `model_family` `wan22` and carries a `height` input of 720 or more
  gets a 16 GB floor: a row **chosen** from the film owner's stated constraint that Wan 2.2 14B at 720p needs a 16 GB card, not
  measured, and labelled so in every answer that cites it. The caller's number wins, 0 included. It filters remote candidates only.
  A malformed value (negative, not a number) is refused by name on every route; a call that stayed on this machine used to drop it
  silently.
- **What the two placement rules place today, said plainly.** With the fleet as the film owner described it on 2026-10-09 (the 16 GB
  cards are the three of the multi-card workstation; the single-card nodes carry 8 GB cards), a graph that gets the default Wan
  floor has **no remote target**: the one-card rule excludes the node with the 16 GB cards and the floor excludes the nodes the
  rule admits. It waits for its own lane, and a thin client cannot reach the workstation for a graph at all. The durable fix is on
  the node (keep a whole-node graph off its display card there) and is the parked local item. A single card that drives a display
  passes the one-card rule: it is the allocator's own carve-out for a box whose only card is its display card.
- **The release that ships this must carry a new version number before any delegator is deployed.** A delegator takes an equal
  `harness_version` as proof that a node has the file check; this change adds no version, so a node still on the build before it
  would read as equal and take a graph it cannot check. The bump belongs to the merge (a deploy precondition, not a code switch).
- **A graph is placed only on a node that runs this machine's release, on every route.** The proof above was first asked only by the
  overflow; `route remote` and a thin client's `auto` pick their node by another function and did not compare `harness_version`, so
  they could send a graph to a node on another release. They now refuse such a node, and one that publishes no version, by name, as the
  overflow does. A fleet part-way through an upgrade therefore takes graphs only on the nodes already upgraded.
- Not changed, and said: this change adds no VERSION or buildinfo bump of its own. A hash of the input files, `devices` on a remote
  graph and a custom node's own file keys are named seams, not built. The node's host-RAM guard sizes a graph at the media class
  default (the largest family the box binds), not at the files it names, so a graph naming weights bigger than every bound family
  can be under-counted (a named limit; the numbers are the RAM guard's). Nothing here was run live: the live acceptance (A8) is the
  first.

### Fixed — a healthy long render's lease (a render helper, or a script that runs one) is no longer labelled "expired ... open to a takeover" (F8)

- **The defect.** A 1,394 s Qwen-Image still run as `gpu reserve --for ... -- node render/comfy-generate.mjs ...` had its
  lease labelled "expired ... still held and heartbeating, reading held-overdue and open to a takeover" at the end of its
  first term while it was rendering normally: the caller declared no progress contract, the lease recorded no owner it could
  tell apart, and the render helpers never wrote a progress file, so nothing vouched for the job (the term rule renews on
  progress for such an owner and on nothing else). A waiter reading `gpu status` was told a healthy job was abandoned. The
  first form of this fix recognised only the direct command line `node render/comfy-*.mjs ...`; the long renders of this
  workspace are launched from a recipe (`cmd /c render-both.cmd`, `bash render-card-v3.sh`: 8 of the 9 orphaned
  lease-wrapper cards in the C02 evidence), with the helper's path inside the script, and those leases were still labelled. The recipe form is
  covered below.
- **The wrapper declares the contract.** `gpu reserve` in its wrapper form, running a render helper that heartbeats
  (`comfy-generate`, `-render`, `-edit`, `-inpaint`, `-video`, `-animate`, `-music`, `-upscale`, `-run-graph`) with no
  `--progress-file` of the caller's, records a progress contract on the lease: a file under `<state root>/progress/` and a
  15 minute stall window (chosen, not measured; the figure and why are in `docs/systems/gpu-lease.md`). It prints one line
  saying so (and where it found the helper), creates the file just before the command starts, removes it when the command
  ends, and hands the path to the command in the new `GPU_LEASE_PROGRESS_FILE` (a caller's own `--progress-file` is kept as
  it is and handed down the same way, and never removed). `gpulease.SweepMintedProgress` removes minted files a killed wrapper
  left, after seven days, inline in the next wrapper that mints one.
- **Where it looks for the helper.** The helper is found as an argument behind an interpreter, in the inline command line of
  one (`cmd /c "node ..."`, `bash -c`, `pwsh -Command`), or **in a script the command runs**: `gpulease.HeartbeatHelperBehind`
  reads the script (`.cmd`, `.bat`, `.ps1`, `.sh`, `.bash`, `.py`, `.mjs`, `.js`, `.cjs`; the first 64 KiB, comment lines
  skipped) and the scripts it names (8 files, 2 deep) for a helper's path, which is how `set "R=...\comfy-generate.mjs"` and
  `GEN="...comfy-generate.mjs"` are found. It reads text and does not parse shell; a script it cannot find or open is "nothing
  found" and the lease behaves as before. **`--progress-file auto`** asks for the minted file for a command the scan cannot
  read (a compiled driver, a name built at run time), with your `--stall` or the 15 minute default. A bench, `sdcpp-generate`
  or a Go verb gets nothing declared unless asked. A `--detach` hold wraps no command and runs several over its life, so it
  declares nothing by itself and `--detach --progress-file auto` is refused: name its file with `--progress-file <path>
  --stall <window>`, which the helpers find through the lease record. A script that names a helper and also runs long steps
  that are not render helpers reads `held-stalled` during them (it would have been labelled expired at the end of its first
  term without a contract); pass your own `--progress-file` for such a recipe.
- **What `gpu status` says while the file is absent.** The wrapper's file exists only while its command runs, so a lease in
  its queue, its drain (`--drain-timeout 90m` is ordinary), the seat's unload or the warm-back declares a file that is not
  there. The status line told such a lease "has not appeared in 1h0m0s, past its 15m0s stall window: the job has not written
  to it or the path is wrong"; for the wrapper's own file it now says "does not exist: the wrapper creates it when its command
  starts and removes it when the command ends, so it is absent while the lease queues, drains, unloads the seat or warms it
  back". The verdict never changed (a missing file reads unknown, never stalled). The file is removed before the warm-back, not
  after the release: left in place it would go stale during a warm-back of minutes and read stalled.
- **The helper heartbeats it.** New `render/lease-progress.mjs`: `pollOutputs` (the poll loop every ComfyUI runner shares)
  appends one JSON line to the file, at most every 30 s, when ComfyUI answered a poll and still lists the prompt (in
  `/history`, or running or pending in `/queue`); `comfy-generate --batch` writes one before and after every picture,
  `withGpuSlot` one when the slot is ready, and `comfy-run-graph` one per manifest model while it provisions them. A helper under a lease whose record declares a progress file but that the wrapper
  did not start (a detached hold with `--progress-file`) finds it in the record. The lines are appended (the file may be a
  log the caller keeps), never throw, and do nothing when no file is declared.
- **A hung or lost render is still labelled.** A hung helper stops polling, a ComfyUI that stops answering is an unanswered
  poll, a prompt a restarted ComfyUI lost is listed nowhere, and a poisoned CUDA context ends the wait through the poll
  loop's own probe: each stops the lines, the file stops moving, the lease reads `held-stalled` after its window and its
  term is labelled expired at the end, as for any lease with a progress contract that stops moving. Nothing is killed or
  released. **Not caught:** a prompt that sits in ComfyUI's running queue with a wedged sampler while HTTP keeps answering
  and `/system_stats` stays healthy (ComfyUI has no polled step counter; its progress events are websocket-only and the
  render tree is dependency-free): that stays bounded by `COMFY_WAIT_SEC` and the lease's maximum total, not by a stall
  verdict.
- **Tests.** `render/lease-progress.test.mjs` (22; the end-to-end one runs the real `comfy-generate.mjs --batch` and the
  `comfy-render.mjs` child it spawns against a fake ComfyUI; two cover a `/history` that answers HTTP 500, which is no
  evidence of a live prompt unless `/queue` lists it), `gpu_progress_test.go` and `gpu_progress_script_test.go` (the decision,
  the helper list pinned to what `render/` contains, the real wrapper renewing a beating helper's lease, labelling a silent
  one, keeping a caller's own file, reading a batch file and declaring from it, `--progress-file auto`),
  `gpu_progress_drain_test.go` (the file does not exist through a drain and exists while the command runs),
  `internal/gpulease/helperscan_test.go` (the scan and its bounds) and `internal/gpuactivity/minted_unknown_test.go`.
  23 mutations of the first form were each caught; 22 more of this follow-up's code were each caught (one survived its first
  test, an unset variable expanding to a path with a hole in it, and was killed after the test was sharpened).
- **Docs.** `docs/systems/gpu-lease.md` (a new paragraph; the two passages that said an attended job without a progress file
  is labelled at the end of its first term now say which job that still is), `docs/systems/media-generation.md`,
  `render/README.md`, ADR 0070 (its amendment).

### Fixed — `gpu status` no longer calls a card the display card because it is busy, and a batch between pictures does not read idle (F38)

- **The defect.** `gpu status` printed `HELD-IDLE` on a live krea2 batch between two pictures and called card 0 "the display
  card". The note said "that is the display card" about the busiest card the verdict had excluded from the holder's work,
  whatever that card was: a lease on one card over a busy neighbour (a seat, another lease, another process) named the
  neighbour the display card although it drives no screen.
- **The naming.** A card is called the display card only when the card table says it is driving a screen right now
  (`display_active`; never by index). Any other busy card the lease does not hold is named as **outside this lease's cards**
  (a seat, another lease or another process is using it), and when both kinds are busy the note names each for what it is.
  This is narrower than the placement rule: the allocator and `gpu cards` also mark a card display when a monitor is attached
  to it with the screen asleep, so such a busy card outside the lease reads "outside this lease's cards" in `gpu status`
  while `gpu cards` marks it display (both true; neither calls the load the holder's), and a test pins it.
- **The batch.** With the progress contract a wrapped render helper now keeps (F8), a batch between two pictures reads
  `held-working` while its file moves, and its note says the cards are quiet between items. A batch that stops writing
  reads `held-stalled` as before. A lease running a render helper with no contract (an older wrapper, a caller that pinned
  an old binary) stays `held-idle` and says why it cannot tell idle from stalled.
- **Tests.** `internal/gpuactivity/f38_test.go` (6), `f38_display_rule_test.go` and `internal/gpulease/progressdir_test.go`; six
  mutations of the shipped code were each caught, and 1 more for the display rule. `docs/systems/gpu-lease.md` (the verdict table).

### Fixed — `gpu status` no longer flags the display card for desktop load as a possible device-trespass (F34)

- **The defect.** While a lease held other cards, `gpu status` and `offload_status.gpu_lease` printed `possible device-trespass`
  against the workstation's display card for ordinary desktop load (15 % or more). The flag exempted the display card by
  `display_active` alone, which reads Disabled on every card whenever no lit screen or game is initialised, while
  `display_attached` stays Yes on the monitor's card; so the card the card table marks `display` was never exempt. The docs
  already promised "not flagged: the display card".
- **The fix.** The flag now exempts a card the card table marks display (`display_active` Enabled **or** `display_attached` Yes,
  the placement rule the desktop-process fold of F18 already uses), together with the cards `display_active` alone marks, so
  neither reading's card loses its exemption. A card another live lease holds, a card a registered run is pinned to, a busy seat
  and a whole-node lease stay excluded as before, and a busy card that is not marked display is still flagged. The verdict's own
  load reading (`held-working`, `busy-outside`, the foreign-load sentence) is unchanged: it keeps `display_active` alone.
- **A named limit.** The exemption is the card's marking, not its load, so an unleased heavy job on the display card is not
  flagged either; from the activity view the desktop's use and a stray job on that card look the same.
- **Tests.** `internal/gpuactivity/f34_trespass_display_test.go` (3: the `display_attached`-only card at 23 % and at 95 % with the
  lease's card busy or quiet, the unleased non-display card still flagged, the union where the placement rule marks nothing).
  Five mutations of the shipped line were each caught. `docs/systems/gpu-lease.md` (the `device-trespass` paragraph).

### Fixed — `gpu --help` prints usage, and `gpu --config <path> <subcommand>` works (F35)

- `local-offload gpu --help`, `-h`, `-help` and `help` print the subcommands on stdout and exit 0 (they answered `unknown gpu
  subcommand "--help"` and exited 1); a bare `gpu` and an unknown subcommand are still errors, and the unknown-subcommand
  message now points at `gpu --help`. A `--config <path>` (or `--config=<path>`) in front of the subcommand is handed to it,
  as the top-level verb already did: `gpu --config x status` dispatched on `--config` and failed. `gpuVerbs` lists the
  dispatcher's cases and a test pins the list to the switch and the usage to the list.
- **Tests.** `gpu_usage_test.go` (5); six mutations were each caught. `docs/systems/gpu-lease.md` ("Using it").

### Fixed — a transcription of a file with no audio stream says so (F21), and every deferred ledger row carries a reason code (F22)

- **F21.** `offload_transcribe` on a file with no audio stream deferred with ffmpeg's raw text (`audio convert: audioio: ffmpeg
  failed: exit status 234 ([out#0/wav @ 0x...] Output file does not contain any stream ...)`). `audioio` now returns
  `ErrNoAudioStream` for it (matched on ffmpeg's own words, checked against a real ffmpeg on a video with no audio track), and the door
  defers with **"the input has no audio stream"**, defer class `contract` (the caller's file is what is wrong, so it is not read as a
  broken stack), `err_class` `bad_input`, and the ledger reason code `bad_input`. The remote lane no longer uploads such a file to a node
  (the failed Opus conversion fell back to sending the original, and the node then failed on it with ffmpeg's words).
- **F22.** The reason code was settled for `task == agent_delegate` alone, so a deferred row of any other door carried free text and,
  on the media doors, an `err_class`: on one node 23 of 23 deferred rows, on another 21 of 100 had none (the review lane's local
  "gpu busy" row among them). `Record`, the one path every writer takes, now settles the code of **every deferred row**: the
  writer's own code from the closed set wins, otherwise the row is classified from its `err_class` (`ledger.DeferCode`) and then from the
  stable prefix of its reason (`gpu busy:`, `seat busy:`, `no ... configured`, `audio convert:`, ...), and `other` when neither names it.
  The closed set gains **`bad_input`** (additive). A row of another task that did not defer still carries none.
- **F22 follow-up: the codes that were still `other`.** Replaying the real ledgers of a 48 hour window (221 deferred rows that are
  not `agent_delegate`), the first form coded 162 of them `other`: the agent door's 96, the 48 `imagegen_families[...] ... is
  required` rows of a config fault, the audio and video `generation failed:` rows (the identical image sentence was coded and
  these were not) and others. Now: **the agent door's deferred rows carry the node's defer class as their code** (the door
  knows it when it defers: `output failed schema`, `structured re-pack skipped` and `step budget exhausted` are prose, the
  class is `budget`, `abstention`, `infrastructure`...; `capacity` is `node_busy`), `ledger.CodeForDeferClass`, pinned to every
  `core.DeferClass*`; the audio and video `generation failed:`, `imagegen_families[`, `unknown image/edit family`, `refused:`
  and the media doors' own empty-prompt and unknown-kind sentences have prefixes; and `audio convert:` is no longer a blanket
  `bad_input`: ffmpeg that could not be run at all (`audioio.ErrFFmpegUnavailable`, an error that is not an `*exec.ExitError`)
  files as `ffmpeg_unavailable` (`config`), a temp directory that could not be made as `infrastructure`, and what is left is
  ffmpeg refusing the file. On the same 221 rows the words alone now code all but 96 of them, and those 96 are the agent door's,
  which carry their class from now on (rows already in a ledger are not backfilled, so that figure cannot be replayed).
  `TestEveryLiteralReasonAPipelineDoorDefersWithHasACode` walks the pipeline's writers (every literal reason passed to
  `recordDefer` or `deferGen`) and fails when a new sentence classifies as `other` and is not listed with the reason it cannot
  be classified from its words, which the file-name pin of `TestEveryWriterOfALedgerRowIsKnown` could not do. 14 mutations of this
  follow-up's code were each caught.
- **The writers are walked.** `TestEveryDeferredRowCarriesAReasonCodeFromTheClosedSet` records a row shaped like each writer's output
  (the pipeline doors, the review lane's local defer, the media doors' err_classes, `RecordAccel`, the remote lane's row, a fleet node's
  executor row); `TestEveryWriterOfALedgerRowIsKnown` parses the tree and fails when a file that builds a `ledger.Entry` or calls
  `Record` on a ledger is not on its list; `TestEveryCoreErrClassHasALedgerReasonCode` pins every `core.ErrClass*` to the table; and
  `TestADocumentedCodeIsAMemberAndTheFleetNodeDocListsEveryMember` keeps `docs/FLEET-NODE.md` listing every member.
- **Tests.** `internal/audioio/noaudio_test.go`, `internal/sttremote/noaudio_test.go`, `internal/pipeline/transcribe_noaudio_test.go`,
  `internal/ledger/defer_code_test.go`. `docs/FLEET-NODE.md`, `docs/OPERATOR-GUIDE.md`, `docs/systems/offload-pipeline.md`.

### Fixed — PAIR cards that outlived what they described, and orphaned cards that say what happened (C02, C03, C04)

- **C04, a seat-watch card that stays open.** Two paths left a seat-watch card "running" for as long as the watcher lived,
  until the next fleet-serve's first sweep closed it "harness process exited". `Poll` returned at once when llama-swap's own
  `/running` was unreadable, with no bound; and a harness load that could not be resolved held an open card untouched, with no
  bound. **Which path, if either, held the one observed card for 642 s was not established**: the stretch may have been real,
  with the fleet-serve that watched it stopped by a deploy swap; these are the two unbounded holes found in the code. After `seatUnreadableLimit` consecutive polls (a minute) the first now fails every open card once
  ("llama-swap stopped answering while this seat served direct traffic"), and the second completes the card at its last busy poll.
  Both counts reset on one good poll. And a seat card orphaned by the death of its **watcher** (a fleet-serve killed by a deploy
  swap) is closed `completed` with no error, as a clean stop closes it: it was never a harness job, and a red "harness process
  exited before the job finished" over a stretch whose seat may have been fine is a red card for no failure. Its `completedAt`
  is the moment the sweep found the watcher gone (the register holds the stretch's start, not its last busy poll), so after a
  long outage the stretch can read longer than the traffic it carried.
- **C02, a killed wrapper.** The cards of the lease wrappers that died in the same second (a console close, exit `0xC000013A`)
  and of the ones a session killed on purpose were correct to be red: the job died. The decision is to keep them red and have the
  card say what happened. The sweep's text now names the process, whether the job was still queued ("it never started") or
  running ("gone 7m into the run"), that no outcome was reported, and the causes it has seen (killed, console closed, reboot).
  "Queued" means "never started" only where the producer marks its card running when its work starts (a lease card, a
  delegation, a media call); a **transcription** never does (the wait is inside llama-swap), so its swept card says "gone 9m
  in, no outcome reported" and does not claim the work had not begun (the first form said "it never started" about a
  transcription killed mid-decode). The text
  fits the 140-character card whole, and still starts with the words consumers match. It cannot say who ended the process: the
  register holds a pid and a start time. `gpu reserve -- <command>` now also asks for `SIGTERM`: Go on Windows delivers a console
  close, a logoff and a shutdown as `SIGTERM` and ends the process only after its handler has had its grace, so the wrapper kills
  its command, closes its card ("terminated: its console was closed, or the session is ending or the machine is shutting down")
  and releases its lease instead of dying silent. With `--drain --unload-seat` it skips the seat's warm-back (a reload of
  minutes inside a grace of seconds, with a model loading while the machine goes down) and releases the lease right after
  stopping the ComfyUI instances kept under it (a step bounded at 60 s, usually seconds): the warm stays owed in the seat marker, and `gpu release --warm-seat` or the seat's next request pays it. A Ctrl-C has
  no such deadline and still warms the seat back first. A `taskkill /F` gives no signal; the sweep's text covers it. The cause of
  the 01:09:59 incident itself was never established.
- **C03, a recovered delegation.** A job a node finished and a recovery pass filed under `delegate-recovered/` had a card that
  said it never finished. A delegation's orphan card now reads the delegator's intent ledger: "the node may still finish it, see
  delegate-recovered/" while the dispatch is open, "the node reported the job's end; see delegate-recovered/" once a recovery
  pass has filed the answer (the pass files an errored job the same way as a finished one, so the card does not say "finished").
  It stays `failed`, because the call did fail (its caller got no answer), and a card keeps the text it was closed with: a
  recovery filed after the sweep (the observed case, one minute after) does not rewrite it. Whether PAIR would let a later
  `completed` frame replace a card that is already terminal cannot be established from this tree, so none is sent.
- **Tests.** `internal/pairworkloads/seatwatch_exit_test.go`, `orphans_text_test.go`, `orphans_callcard_test.go`,
  `gpu_signals_test.go`, `gpu_signals_warm_test.go` (the wrapper runs in a child process and the signal is delivered through a
  seam; a console close cannot be driven from a test); 16 mutations were each caught, and 9 more for the transcription
  text, the recovered-card wording and the warm-back. `docs/systems/pair-workloads.md`.

## [0.179.0] - 2026-10-10 - a busy image lane overflows to an idle node that renders the same recipe, and every GPU lease path has a host-RAM brake

### Added — a busy image lane overflows to an idle node that renders the same recipe (F52, ADR 0082)

- **The behaviour.** An `auto` image call (`offload_generate_image`, `generate-image`) on a machine that has the lane and a fleet
  configured used to wait for its own lane however many other nodes stood idle. It now asks its pipeline whether the lane is free
  right now and, when it is not (a lease holds the card, callers are in line, or the host-RAM guard is holding the lane back),
  sends the call to an idle node whose **image recipe** digests alike, under the node's own family name. The answer says where it
  ran and why it left (`meta.node`, `meta.placement`). A box with no `delegate_remotes` asks nothing and reads no node (the path is
  unchanged), `route local` never leaves, a call that carries a `waiter_token` stays in the local line, a call under an inherited
  lease stays, and only image jobs overflow (graphs arrive with their own file check). When no node admits, the call goes on to the local
  queue, which it joins when the fleet attempt is over and not from when it arrived (see "Not changed, and said"), and a deferral it
  ends in carries `cluster[]`: one row per `delegate_remotes` node (`busy`, `not-capable`,
  `unreachable`, `refused`, `bounced`, `skipped`) with the reason in the node's own terms (the lease and its reason, the config key
  that differs and the family of this machine that would match, the release or the refine it lacks).
- **The question is read-only and a relaxation.** `core.LaneProber` / `Pipeline.MediaLaneFree` is built from the helpers the
  admission already reads state with and edits nothing inside `acquireCards`: it says busy only when something that makes the real
  wait-0 grant refuse is present, and free whenever it cannot say. `TestMediaLaneFreeNeverRefusesAGrant` drives the real admission
  over 38 lane states and fails if a lane the grant would serve is called busy (and `TestMediaLaneFreeNeverRefusesAGrantOverRandomStates`
  over 150 fixed-seed random ones); `TestMediaLaneFreeWritesNothing` snapshots the lease root in each of them. It removes nothing
  either: it reads through `gpulease.Manager.ReadOnly`, a view whose readers skip the records the line's readers prune as
  housekeeping (an expired place in line, a waiter that stopped polling, a dead one) and leave them on disk, and five of the 38
  states hold exactly such a record. On the whole-node
  plan, where the lease queue orders by arrival, a call that resumes a place counts only the callers that arrived before that place
  (the randomized differential found the one state where the prober called such a lane busy; unreachable from the S1 trigger, live
  for the ticket scan of the next step).
- **Identity is the recipe, not the name, and the recipe is name plus size, not content.** `mediacap.ImageRecipe`: the checkpoint,
  text encoder, VAE and LoRA a family loads with their names and byte sizes, and its sampling, digested over RESOLVED values (an unset Qwen-Image-2.1 key takes the builder's default from a
  table `TestBuilderDefaultsMatchTheJSConstants` reads out of `render/wf-qwen-image-21.mjs` and `render/comfy-render.mjs`), so a node
  that writes the default scheduler out and one that leaves it unset match, and a bf16, an int8 and an NVFP4 build of one family
  name never do. Node-local keys are not in it, and a test fails when a key joins the image overlay's clear list unclassified.
  No recipe exists for an sd.cpp binding. The match is strict, needs the same release, and never substitutes. Two same-named files
  of one size whose bytes differ (a corrupted full-length copy, a same-named re-release with the same tensor layout, a file updated
  in place on one node) digest alike, and the node's 412 re-check recomputes the same size-based digest, so it cannot catch them
  either; a sha256 per weight file is the named upgrade and is not built.
- **Nodes publish and check recipes.** `/fleet/health` gains `image_recipes[]` and `refine_honoured` (additive; absent on a node
  without an image lane and on older nodes, which a delegator reads as "cannot be matched"), from a 60 s memo. An image-gen dispatch
  may carry `recipe_digest`; the node recomputes the digest of the family it names from the files on disk at admission and answers
  **412** with no job on a mismatch (not 409, which already means a job failed here before, and not 503, which means busy).
  The delegator also decodes the singular lease block's class, reason and remaining seconds (published, never read).
- **`refine=false` travels.** The node's image-gen payload decodes `refine` the way the MCP handler does, and the client sends it only
  to a node whose health says `refine_honoured`; any other node is a named miss in the defer instead of a contract refusal or a
  silent refinement (this applies to `route remote` and the no-lane `auto` path as well). `tts_voice`, `transformer` and a graph's
  `devices` still defer by name.
- **Refusals move on, an accepted job is final.** At most 3 nodes, each under a fresh job id; a refusal at the door (503, 429, 412,
  any status, a dial that never connected) or an accepted job answered `gpu_busy`, `gpu_queued` or `gpu_lease_unavailable` (nothing
  ran in any of them) is passed over; any other answer after acceptance is the call's result, a POST that was sent and got no answer
  is final too, and a job a node holds is never also run here (pinned by `TestAnAmbiguousPostIsFinal` and
  `TestACallIsSentToAtMostThreeNodes`). A node that does not answer is left alone for 5, 15, 60 and then 300 s, a `Retry-After`
  pauses it, and a node that took a call and passed it back is left alone for 60 s and then 300 s per consecutive bounce, because a
  node that reads idle in its health but whose own grant refuses every job would otherwise park each call for its whole
  `gpu_wait_ms`; the cluster row names the pause as `bounced`, `refused` or `unreachable` and for how long (chosen constants, not
  measurements). The memory is per process: the MCP server keeps it across the calls of a session, while a one-shot CLI call starts
  with none and pays a bounced node's `gpu_wait_ms` again. `RemoteAttribution.Bounce` closes
  the PAIR card of an attempt that ran nothing (completed, never started) whatever its `err_class`, `gpu_lease_unavailable` included,
  and writes no ledger row; an overflowing call opens its
  card only once a node has accepted the job, so the call has one row and one card per node that held it.
- **A roster that lists the machine itself.** An entry whose `node_id` equals this machine's (`fleet_node_id`, else the hostname)
  is never a candidate and is named in `cluster[]` as `skipped`: it would match the recipe perfectly and send the call back into the
  lane that is not free.
- **Cost.** Every `auto` image call on a lane-plus-fleet box now pays one `nvidia-smi` read (at most 4 s) for the lane question, and a
  roster read when the lane is not free, including for families no node can match; a roster-first short circuit and a short table
  copy for pinned plans are named follow-ups.
- **Fixed on the way.** A seed (and width, height, steps) the caller gave as an integer reached a fleet node through a float64, so a
  seed above 2^53 changed; the sender now reads integers exactly (`TestSeedTravelsExactly`).
- **Not changed, and said.** One line per delegating machine, not one global order; a call resumed with its token is not moved
  (late binding is the next step); a call that went to the fleet first queues locally from when it came back, not from when it arrived (each bounce off a node
  can cost up to that node's `gpu_wait_ms`, three nodes at most, and a call no node matches pays the lane question and the roster
  read; carrying
  its arrival time in needs both the lease queue and the in-process slot queue, which takes none; the ticket queue of the next step
  keeps a call's place across the attempt); every remote job cold-starts ComfyUI; the same recipe and seed on another GPU architecture is the
  same composition, not bit-identical pixels. Nothing here was run against a live lease or a real render: the recipe match was
  tested against fixtures that reproduce the image blocks of three live nodes as stored on 2026-10-09 and 2026-10-10 (config text),
  one of them cut inside the block that matters, so the first live action reads `image_recipes` on both nodes.

### Added — a host-RAM guard on every GPU lease path; a kept ComfyUI instance is freed when its family changes

- **A declared load is checked before the grant, not discovered after it.** On the reference 3-card Windows box (127.7 GiB physical),
  2026-10-09, two card-scoped media leases each streamed bf16 weights a 16 GiB card cannot hold: committed memory reached
  162.9 GiB against a 187.7 GiB limit, the page file grew 60 to 68 GiB and free RAM bottomed at 3.2 GiB. The allocator's host
  term read FREE RAM and applied only to `--cards`, so `--devices` (what every owner wrapper takes) bypassed it, and nothing
  read committed memory. Now a lease carries a declared host-RAM need (`host_ram_gib`, an additive field of the lease record)
  and every grant path (`--devices`, `--cards`, whole-node, the pipeline's media admission, the detached holder) admits it
  atomically with the grant, under the epoch lock. The rule has two terms and a grant needs both:
  `committed now + need + not-yet-loaded <= physical - gpu_host_ram_headroom_gib` and
  `available now - need - not-yet-loaded >= gpu_host_ram_headroom_gib`. Commit is primary (it counts what the OS has promised,
  which is what stops a second lane before it loads); on Windows a process's GPU allocations may be charged to its commit
  without occupying RAM, so a lane's commit can exceed what it declared (a declaration is the model files' size, raised to the
  largest measured resident peak once three runs exist). **This is a brake on declared loads, not a bound on what the box
  commits:** the rule applied to a commit reading of 81.6 GiB admits a 32.8 GiB Krea 2 declaration (114.4 GiB projected against
  119.7), and a ComfyUI lane holding 57.7 GiB private bytes (its family is not recorded) read 129.3 GiB committed on the 127.7 GiB
  box hours later (docs/systems/gpu-lease.md, "Known limits"; these readings were measured on the reference box on 2026-10-10 from
  session readings that are not recorded in this repository, and are pinned only as arithmetic, `TestTheQuotedCounterexampleIsAdmittedOnBothTerms`). The not-yet-loaded part of a granted lease is its declared need minus what the processes below its holder hold
  RESIDENT (not private: committed-but-unoccupied memory is not RAM that has loaded). The default headroom is **8 GiB** (was
  4): chosen, not measured, and not a number the operator typed; it is a floor until each node's interactive working-set swing
  is measured (docs/systems/gpu-lease.md, "How the numbers get measured"). A shortage queues in the same FIFO as a card
  shortage (`waiting for host RAM: needs N GiB, committed X of Y GiB physical (+P GiB still to load by leases already
  running), Z GiB headroom`, printed once per request; the waiter's record and `gpu status` say it waits for host RAM);
  `--wait 0` refuses with that text; a need no state of the box can admit is refused at once, naming `--ram` and
  `gpu_host_ram_headroom_gib`, and the pipeline classes it `gpu_lease_unavailable`, not `gpu_busy`. A need of 0 is never read
  against the host.
- **Where the need comes from.** `gpu reserve --ram <GiB>` (0 allowed), else the model files of a recognised render-helper call
  that do not fit the card, counted in FULL because ComfyUI's dynamic VRAM stages the whole file in host memory (a krea2 bf16
  call on a 16 GiB card is 24.48 + 8.27 GiB; a file that cannot be sized takes the documented per-family size), else the media
  class default (the largest ComfyUI render family the box binds; an sd.cpp binding counts nothing), and 0 for a text lease. The detached holder is handed the parent's
  resolved number as `--ram`. The pipeline's media admission asks the same estimate per route from the binding it is about to
  render with. A host with card-scoped leases off reads no card table, so its card is unknown and the files count in full.
- **Visibility.** `gpu status` (text and `--json`) and `offload_status`'s `gpu_lease` block carry `host_memory` (physical,
  available, commit used and limit, headroom, declared and still-to-load GiB, how much the next lease may declare) and one
  verdict: OK, NEAR (within the headroom of physical RAM) or OVER (committed memory above physical RAM). The brief verdict line
  leads with `HOST RAM OVER (committed memory X GiB exceeds the Y GiB of physical RAM)`. OVER is a statement about what the OS
  has promised; no surface claims the box is paging, because no reading here can know it (the page file growing, or the
  pages-output rate, would). Lease rows show what each lease declared; queue rows show what a waiter waits for.
- **A kept ComfyUI instance is freed when its family changes.** The launch marker remembers the family (and weights file)
  an instance may still hold. The runner's end-of-run `/free` is now awaited, retried once and loud (`COMFY-FREE-WARN`), sent
  even when the instance is kept, and clears the recorded family only when it succeeded; a runner that finds a different
  family on a kept instance frees it before its first job; `run-graph` frees an instance it ran on, including one it found
  already up; the lease-close proof of a kept instance retries 3 times at 8 s instead of once at 3 s (a ComfyUI in the middle of
  a 550-second prompt answers late); the pipeline's backstop `/free` waits 5 s instead of 1 s. The incident's 57 GiB instance is
  reconstructed from the code and the surviving logs, not observed, and its probable cause (a second family cached beside the
  first) may account for only part of the figure: a ComfyUI process launched fresh on 2026-10-10 held the same 57.7 GiB private
  about seven minutes after launch (a reading on the reference box, from a session, not recorded in this repository), so the footprint of one lane is unmeasured (the S1 acceptance runs record it).
- **Not changed, and said:** the guard checks the declared loads of the leases that pass through it. A pinned older binary (its leases declare
  nothing), a direct llama-swap request and a hand-started ComfyUI are outside it; a pipeline job under a parent's ambient
  `--devices` lease gets no admission of its own; a detached lease counts its whole need as still to load for its life; Linux
  `Committed_AS` reads high; run-graph, sd.cpp and iGPU calls take the class default, which is the largest ComfyUI family the box
  binds: on a box that binds none (an sd.cpp-only node) it is 0, so those lanes declare nothing and the guard does not see the
  weights `--offload-to-cpu` parks in RAM (`TestAnSdcppOnlyBoxDeclaresNothing`); sizing sd.cpp from its own model files is not built.
  A video family on the sd.cpp engine is left out of that default like the image, animate and music bindings (it was counted as the
  runner family its name maps to, `TestAnSdcppVideoFamilyIsNotCountedInTheClassDefault`); the edit route has no engine key.

### Added — the node's host-RAM verdict can be asked without taking anything (G1)

- `gpulease.Manager.HostRAMCheck(need)` puts a need to the host as it reads now: the same function the grant calls
  (`hostRAMCheckAgainst`), over the leases live now, with the numbers and the refusal sentence. It takes no lease, registers no
  waiter, spends no epoch and writes nothing; a need of 0 is admitted without reading the host. A cluster placement can ask the
  node before it sends a lane there and cannot disagree with the node's own grant (`TestHostRAMCheckIsTheRuleTheGrantApplies`).

### Fixed — a media call that cannot wait answers the guard's sentence on every plan (G2)

- With `gpu_wait_ms` 0 (one gated try) and the cards free but the host's memory short, a pinned or allocated plan fell through
  the queue step and answered "card(s) X promised to callers ahead of this one" (or "held, or promised..."): false, and naming
  nobody. It now answers the guard's own sentence, as a queued place for a door that can resume and as the busy defer
  carrying the same words for one that cannot. The whole-node path and the allocator's own pre-filter already did; their
  tests are the pins (`TestWaitZeroKeepsTheGuardsSentence...`).

### Fixed — a waiter that waits only on host RAM no longer stops a request that declares none (G6)

- A waiter the host refuses stays at the front of the FIFO for its whole `--wait` (eight hours by default), so a 0 GiB request
  for the same card queued behind an idle card, and a whole-node waiter stopped every request on the box ("which has not
  claimed the card"). A waiter whose record says it waits only on host RAM (`waiting_for: host-ram`) now does not hold back a
  request that declares no host RAM: that request cannot make the shortage worse. A request that declares host RAM stays behind
  it and is told what the waiter in front waits for. The delay the pass costs the waiter is bounded, not a stream: the moment
  it finds its card held it stops being a waiter on memory and nothing else passes it. Named limits: nothing reserves the
  waiter's memory against a declaring request on disjoint cards, and a token (a place held for a caller who left) still holds
  for its 30 s grace. `gpualloc.QueuedClaims` takes whether the asker declares host RAM, so the allocator does not steer a
  passing request off an idle card.

### Added — the warm-back of the agent seat is admitted like a load; the seat declares its footprint (G4)

- The warm-back after a lease (`--unload-seat`, `gpu release --warm-seat`) was the one load nothing sized. It now puts the
  seat's host footprint (`agent_seat_host_ram_gib`, a new additive config key; unset, `hostneed.DefaultSeatHostGiB` = 21 GiB, a
  chosen fail-closed figure and never 0: not a measurement, and below the 44 GB the 2026-09-10 incident recorded for a seat, so a
  node that runs a seat sets the key from its own measurement) to the grant's admission over every lease
  but the one being released (`Manager.HostRAMCheckWithout`), and refuses outright while another lane that declared host RAM is
  live. A host that reads NEAR or OVER never warms; a refused warm stays owed and the seat loads on its next request. Not done,
  on purpose: the owed warm is not counted as pending at another lease's grant (the seat was unloaded to make room for that
  lease; the reload happens after it), and a text lease still declares 0 unless `--ram` says.

### Added — what each render held on the host is measured; declarations are raised by it (G3)

- Every sampled GPU render records, beside its VRAM peak, the peak private and the peak resident memory of its process tree
  (`gpulease.TreeMemory`, sampled by gpugen on the same tick) into the footprint store as `host_peak_gb` and
  `host_resident_peak_gb` with the number of runs; both ride `/fleet/health` `model_footprints[]` additively. The media admission
  raises a declaration to the measured resident peak once at least three runs exist (image and video routes), never lowers one,
  and leaves a declaration of 0 alone. Whether a GPU process's WDDM allocations count toward its private bytes is partly
  measured and not settled (docs/systems/gpu-lease.md: one process steady-state fits, a second does not, the lane-start case
  is unverified); the two-term rule and the resident not-yet-loaded sum are the guard's choice under that uncertainty, not a
  proof that it holds whichever way it falls: an admitted lane can still read OVER (docs/systems/gpu-lease.md, "Known limits").
  The pipeline test suite now isolates the offload home, so a test can no longer write the operator's footprint store.

### Fixed — the host-RAM surfaces say what the reading shows (G5)

- "OVER" is commit above physical RAM, the guard's own conservative line chosen 2026-10-09; it was labelled "the operator's
  definition" and the status line said "the box is paging". Neither is true of what the code can know. The note, the brief line
  and the docs say "committed memory X GiB exceeds the Y GiB of physical RAM" and that the reading cannot say whether the box is
  paging now.

### Fixed — PickAuto's poll loop is bounded by the polls the wait allows

- `gpualloc.PickAuto` ended its poll loop only when the injected clock passed the deadline. A caller that injects a sleep that
  does not advance that clock (a no-op sleep with the real clock, a frozen clock) turned it into a busy-spin building one
  `AllocInput` per pass for the whole wait. A run of the tests of an interrupted patch (2026-10-10) held 52 GiB private on the
  reference box, and the patch contains exactly that shape (reconstructed from it: the 52 GiB was not re-observed; the committed
  tests do not grow). The loop is now also bounded by `wait/2s + 1` passes (the same bound in production);
  `TestPickAutoPollsAreBoundedWhateverTheClockDoes` is the bound.

### Fixed — merging the guard onto 0.178.0

- `gpu reserve`'s host-RAM estimate reuses the card table its plan already read instead of taking a read of its own (F24: a
  wedged `nvidia-smi` costs a command the reads it already makes), and the whole-node path of a media call whose admission read
  failed sizes its declaration against an unknown card instead of reading the table again.

## [0.178.0] - 2026-10-10 - the lease queue is first come first served, media outputs are atomic, and held media jobs close cleanly

### Fixed — a fresh `gpu reserve` queues behind registered waiters instead of winning a just-freed card

- **`acquireQueued` probed the card with a bare `TryAcquire` before it ever queued.** The place in line was taken
  only after that probe returned `ErrHeld`, so a card freed in the window between a holder's release and the front
  waiter's next poll tick (1 s) went to whichever fresh reserve was launched in it, ahead of everyone registered.
  A recipe that chains reserves back to back (its next `gpu reserve` starts the instant the previous one returns)
  won that race every batch: on the reference 3-card box, 2026-10-09, three chained media reserves took card 2 as
  epochs 1320, 1321 and 1322 while a text waiter for cards 0+2, registered for 1h26m and front of queue the whole
  time, never got its turn; the recipe's own log printed "2 lease(s) queued behind this one" at each release and
  never "queued behind". The same class sat in `gpulease.Acquire`, which returned a bare `TryAcquire` for
  `Wait <= 0`, and in the two other direct `TryAcquire` callers (`gpu reserve --cards` without a wait, the media
  admission's first claim). Now: `Acquire` with no wait runs the same registered, front-of-queue gated loop with a
  deadline of now — one attempt if this caller is front of the line, else the queue's answer (`ErrHeld` for a held
  card, `ErrStillQueued` naming the waiter ahead for a free one); `acquireQueued` never probes bare and prints its
  one entry line from a read-only look at the line (`queueLine`: the holder, else the conflicting waiter);
  `--wait 0` on a free card with a waiter ahead fails fast with the `--wait` hint; `gpu reserve --cards` and the
  media admission go through the gated attempt and treat "still queued" as a lost claim (allocate again, then
  queue), and both allocators read the line (`gpualloc.QueuedClaims`), so a card with a waiter ahead is not
  picked as free; the pipeline's no-wait media lease maps it to `gpu busy`. Disjoint backfill is unchanged: a
  waiter for other cards is no reason to wait, and a blocked text-load admission queues on its seat's cards.
  `gpulease.DevicesConflict` is exported for the entry line. Tests: a no-wait `Acquire` yields to a registered
  waiter on a free card and wins once it leaves; it still backfills a disjoint card; a fresh reserve (`--wait 0`
  and `--wait 300ms`) queues behind a `RegisterSeatWaiter` entry on a free card and wins once it leaves; the
  pipeline's no-wait whole-node lease reads a waiter ahead as `gpu busy` and is granted once it leaves — all four
  red on the unpatched base (mutants: the bare probe restored in the CLI, the shortcut restored in `Acquire`, the
  busy mapping removed, each red). The media admission already counted a registered waiter's place at allocation
  (`gpualloc.QueuedClaims`, moved out of the pipeline so the reserve verb reads the same rule), so its gated
  attempt only closes the gap between the allocation and the claim, and costs a call that resumed a place nothing;
  a test pins that an image call with a seat waiter in line answers `gpu_queued` and starts nothing.
  Not changed here: a binary older than this release keeps winning the gap (the
  pinned copy that chained the incident's reserves must be refreshed to carry the fix).

### Added — Qwen-Image-2.1-Turbo: a `turbo` schedule for the 2.1 image and edit graphs

- **`render/wf-qwen-image-21.mjs` learns the Turbo checkpoint's saved schedule.** `Qwen/Qwen-Image-2.1-Turbo` (8 steps,
  cfg 1, the same 7B architecture, Qwen Research License, generation and editing) ships its sampling schedule as
  `sample_sigmas` in `model_index.json` with dynamic shifting off, shift 1.0 and no terminal stretch — a list the base
  model's `official` schedule (dynamic mu, `shift_terminal` 0.02) does not reproduce at 8 steps, and ComfyUI's fixed
  shift does not either. `imagegen_schedule: "turbo"` feeds that list through `ManualSigmas` (terminal 0 appended) and
  refuses any step count but 8, naming the saved schedule. The 2.1 edit graph gains the same custom-sampler path behind
  the new `gen_edit_schedule` (`comfy`, the KSampler default, or `turbo`; the size-dependent `official` mode has no
  place on the encoder's latent); `comfy-edit.mjs --schedule` is a 2.1-only knob, refused on 2511. Bind the Turbo as
  named families beside the 40-step ones (the Comfy-Org repack: `qwen_image_2.1_turbo_bf16.safetensors` /
  `qwen_image_2.1_turbo_int8_convrot.safetensors`). Tests: the list pinned value for value, T2I and edit graphs at
  several sizes, wrong step counts refused, the edit default graph unchanged, the 2511 refusal.
- **doctor: the default `edit_image_generative` row names the named-families case.** A node whose only edit bindings
  are `gen_edit_families` overlays (no `gen_edit_unet`) read "gen_edit_script/gen_edit_unet is unset" beside a
  CONFIGURED family row, as if defective; it now says it edits through its named families.

### Fixed — a failed render leaves nothing at its output path, a full disk stops a batch, and a batch with failed jobs says so

- **Atomic outputs.** Every render helper delivers its final output through a staged sibling renamed into place
  (`render/atomic-out.mjs`; `render/atomic_out.py` for the Python workers; the ffmpeg ops in `internal/mediaops` the same way).
  A full data drive used to leave zero-byte pictures (21 of 36 in one batch, 2026-10-09) that skip-existing scripts read as
  finished; now no file is left at the path and a good file already there survives.
- **A full disk stops a batch.** A `--batch` stops at `ENOSPC`/`EDQUOT`/`EROFS` (recognised by its errno token, never by prose)
  and records the remaining jobs as not run; the ledger files it under its own class, `disk_full`.
- **Exit codes.** `comfy-generate`/`comfy-inpaint --batch` and `local-offload generate-image --batch` exit 4 when any job
  failed (they exited 0); a clean batch is 0, a stop or setup error 1. The Go caller reads 4 as a finished batch.

### Fixed — `gpu status` names what holds the card, and `offload_status` stops burying it

- The `gpu status` headline, the `gpu reserve` refusal, the media lanes' queued answer and the brief `gpu_lease_verdict` say
  `held by a media-class lease` / `a text-class lease` (was `held by media`, which read as a seat).
- The owner line shows the lease's `--origin` when no session or pid owner is recorded (display only; still never orphaned).
- `gpu_processes` folds a display card's unsizable processes (the Windows desktop: 31 rows on the reference 3-card box) into
  `display_card_processes_unknown`, one `{index, gpu_uuid, name, count}` per card; every other row is still listed.

### Fixed — a card-table read that runs out of time under load no longer hard-defers a media call

- The allocator's `nvidia-smi` read is retried once under 15 s after its 5 s; a call whose re-read still fails is placed from the
  newest table it holds and queued with a reason that says so, instead of a `gpu_lease_unavailable` defer. `gpu reserve`,
  `node-swap --cards` and the drain/unload scope read the table with the same retry; the render helper asks `nvidia-smi -L`
  once more and logs `COMFY-GPU-LIST-WARN` instead of silently leaving ComfyUI on card 0. The allocation reads only the
  per-device `--query-gpu` fields (no process listing).

### Fixed — a PAIR job is closed with its real outcome when the door answers a deferral

- A door killed right after its reply no longer leaves a red "harness process exited before the job finished" card: the close
  of a long call is on the wire before the door answers. A call that waited its window and found the card (`gpu_queued`,
  `gpu_busy`) or the compose slot held closes quiet (`completed`, no start, the reason in `error`; PAIR has no cancelled
  state); a render that broke still closes `failed`. A fleet node's job poll carries `err_class` (additive), so a remote call
  whose node's card was held closes quiet on its asker too; remote lanes close on a panic; the call deadline's abandoned
  subtasks have their cards closed.

### Fixed — a short call's PAIR card is closed once, whichever way the scheduler runs the emitter's sweep

- **An emitter's own sweep resent the terminal frame the emitter was posting.** The verdict is parked as a pending marker
  before it is posted (the entry above), and the sweep every emitter makes on its first `Emit` runs on a goroutine: a call
  short enough to close before that goroutine had read the register (a deferral, a refused dispatch, a lease card) was
  closed twice, the second frame byte for byte the first. PAIR merges it, but it is a duplicate on the wire, and it failed
  three tests of three packages (`TestLeaseCardLifecycle`, `TestPulledReclaimOfAFinishedJobEmitsNoSecondCard`,
  `TestEndClosesUnstartedCardWithoutRow`) on the 4-vCPU Linux CI runner of this release while they passed on a 16-core
  host: the sweep races the caller and wins as processors get scarcer (none of 50 runs on 16 CPUs, up to 8 of 50 on 2 to 4,
  nearly every run on one). An emitter now remembers the markers it has a post in flight for and its sweeps leave them alone; a
  post that fails parks the frame again and clears the mark, so the next sweep sends it as before. Another process's sweep,
  or fleet-serve's periodic one, that reads the marker in those milliseconds still resends an identical frame, which PAIR
  merges. Tests: a held terminal post and the emitter's own sweep (nothing sent), a refused post and the same emitter's
  next sweep (one sent), a short card closed once on one processor; the first and the last red without the guard, the
  second red when the mark is never cleared.

### Fixed — two tests passed only on a host with a live llama-swap and a pinned Go cache

- **`TestGPUReserveConfinementReadsTheCardTableWithTheRetry` counted card-table reads that depend on a llama-swap answering.** A
  card-scoped `gpu reserve` hands its command the unload list for the render lane, cut from the llama-swap roster, and the cut
  reads the card table only for a roster it could read. The test's scripted reads (`ok`, `ok`, `hang`) assumed that read
  succeeded, which it does on a host with a live llama-swap on the config's default `127.0.0.1:11436` and does not on a CI
  runner (connection refused): there the wrapper made one read fewer, its own confinement read took the second `ok`, and the
  test saw `reads = [40ms 40ms]`. The code is right either way (a roster it cannot read leaves the render lane on its own
  rule, and the log says so). The card-table tests now read a stand-in roster (`useRoster`), so the reads are the same
  everywhere and a test run no longer asks the host's llama-swap anything. Checked on one Linux host with one binary: a fresh
  loopback fails with the CI's message, the host's own loopback passes.
- **`TestGenerateImageBatchExitCodesThroughTheBuiltBinary` built the binary under its temp HOME.** The test moves HOME to a temp
  directory for the binary's runs and then ran `go build`. On Linux GOPATH, and with it GOMODCACHE and GOCACHE, follow HOME, so
  the compiler downloaded the whole module tree into that directory (45 s per run) and Go's module cache is read-only: the
  directory could not be removed ("TempDir RemoveAll cleanup: unlinkat .../go/pkg/mod/...: permission denied"). A Windows host
  pins GOMODCACHE in its own `go env`, so it never showed there. The binary is now built first, under the caller's own HOME and
  caches; what the test pins (exit 0, 4 and 1 and their stderr lines through the built binary) is unchanged.

### Fixed — `offload_review_diff` no longer publishes hollow reviews, stacked restatements or discarded cut answers

- **Root cause of the hollow findings:** the structured re-pack, told only `"findings" (array of strings)`, split
  `severity | file:line | claim | why` lines into bare strings on the fleet's 9B and 35B seats. The lane now also reads the
  seat's own answer (pipe lines, or JSON finding objects) and publishes whichever reading keeps more findings
  (`salvaged:"repack_flattened"`).
- Findings with no severity, file or why are dropped and counted (`dropped_hollow`); when nothing survives filtering and the
  seat did not say NONE the lane defers instead of returning an empty success. Findings on the same file and line fold into
  the most severe, the others' claims in `also`.
- A review the seat wrote but the clock kept from being structured is read line by line through the same filters and
  published with `salvaged:"output_truncated"|"wall"`.

### Added — a decode mode for the Wan 2.2 graph: `videogen_wan_decode` (`tiled` | `auto` | `plain`), default `tiled`

- `render/comfy-video.mjs --wan-decode tiled|auto|plain` (the pipeline passes `videogen_wan_decode`). `tiled` is today's
  `VAEDecodeTiled` graph and stays the default. `plain` is `VAEDecode`; `auto` picks plain when the render card's
  `GET /system_stats` `devices[0].vram_total` is 12 GiB or more, else tiled, and logs one `wan-decode:` line. Measured on an
  RTX 5060 Ti 16 GB (2026-10-03, ComfyUI dynamic VRAM on, clip shape unrecorded): plain 38 s at a 10.3 GB peak against
  412 s tiled, 45 dB PSNR apart.
- **Why the default stays tiled:** no plain decode has been run at the 16 GB tiers' own shape (1280x720x81), where ComfyUI's
  estimate is 12.0 GiB of a 15.9 GiB card; ComfyUI retries an out-of-memory plain decode tiled once, and a second
  out-of-memory fails the clip after sampling. The default moves to `auto` after a live acceptance render at that shape
  decodes in about 40 s with no `Ran out of memory when regular VAE decoding` line.

## [0.177.0] - 2026-10-09 - warm-back survives a llama-swap reload, the embedder can run from its own llama.cpp build, and two flaky tests stop reading the clock

### Fixed — a warm-back interrupted by a llama-swap reload or restart is re-sent, and the owed-warm marker clears when the debt is moot

- **Warm-back survives a reload.** `gpu release --warm-seat` and the reserve wrapper's warm-back used to report "warm-back failed"
  when the load landed on a llama-swap `-watch-config` reload or a restart (a transport error, a 502/503/504, or a 500
  "is shutting down"), although nothing was wrong. The load is now re-sent for up to 60 s with a 1/2/4/8 s back-off, the lease
  guards re-read before each re-send. A body saying the start died (`upstream command exited`, `unable to start process`) is never
  re-sent and never opens the window, whatever its status code (one classification, `internal/seatwait`, shared with the
  pipeline). A seat another client is loading is waited for; a 4xx is no longer "warmed back" unless `/running` shows the seat
  loaded; an unreadable `/running` during a seat last seen starting is waited out, also after a reload.
- **The owed marker clears when the debt is moot.** A failed warm whose seat is then observed loaded clears it (guards re-read
  first, compare-by-seat), and `gpu status` clears a marker it can prove stale: its stamp predates status's own readings, no
  lease is live right before the remove, and the record is unchanged. It says so, and `--json` carries `seat_warm_owed_cleared`
  on that run. Messages say whether the seat read cold or could not be read.

### Added — `install render --llama-bin-eg2`: the embeddinggemma2 entry runs from a second llama.cpp build (ADR 0081)

- `install render --llama-bin-eg2 <dir>` (`install.sh --llama-bin-eg2`, `OFFLOAD_EG2_LLAMA_BIN` on Windows) runs the
  embeddinggemma2 entry alone from a second llama.cpp build (b11452 or newer), recorded as `eg2_llama_bin` in the render stamp
  and carried by the `audit-yaml` replay (with `--llama-bin-cpu`, which the replay used to drop). Renders without the flag are
  byte-identical to 0.175.0, pinned by golden hashes of every template variant. A binary older than 0.177.0 reads a node rendered
  with the flag as HAND-EDITED.
- A write-time floor check refuses an entry build named below b11452 (the directory, or the one above a `bin`/`build` folder);
  a path that names no build gets an advisory `note:`, which `install.ps1` now relays. `audit-yaml` reports a stamped second
  build its tier no longer permits as STALE with the renderer's own refusal, not "no longer in the tier table".
- `install.ps1`'s Step 6 skip probe matches the executable path, and `render.tests.ps1` survives Windows PowerShell 5.1 (the
  shell CI's `installer-windows` job runs).

### Changed — the EG2 card budget states the 3-card tier's measured reason instead of a utilization model

- The vLLM-share arm (utilization arithmetic that said the text-only entry "fits with 186 to spare") is removed: it modelled
  neither the shipped util-sized seat nor the reference box's pinned pool. The sum arm and the text-only result are unchanged.
- The 3-card tier stays text-only on a measurement (2026-10-09, reference 3-card box, local pinned-pool seat): the agent seat's
  cold start failed with all three memory-stack residents on one seat card (2.66 GiB KV needed, 2.49 available) and passed with
  embeddinggemma2 on the seat's other card. Recorded in `docs/systems/setup-installer.md` and the tier's notes, with the known
  gap: the `win-triple-blackwell` template still pins the three residents to one seat card (device 0's fit beside a 501 MiB
  resident is unmeasured, and a model is never re-pinned from a guess).

### Fixed — two timing-flaky tests stop reading the runner's speed, and the delegate one can no longer pass a defect by skipping

- **Pipeline:** `TestRepackChatFallbackStreamsUnderLiveness` measures a 1 s liveness allowance against a 40 ms delta gap and gains
  a clock-free twin, `TestRepackChatFallbackReportsEveryDeltaToTheProgressHook`, that counts the progress hook's events.
- **Delegate:** the four-slot and six-slot "a wait holding a run slot keeps its TTL" tests run one body (`heldSlotWait`): reasons
  and the clock's lower bounds are asserted before any skip, and a call-deadline cut of the last subtask skips only on evidence
  from outside it, otherwise it fails. A wait that ignores its horizon, holders run to the horizon, and an unstarted count floored
  at one each fail both tests.

## [0.176.0] - 2026-10-09 - the render helper's drain sees a vLLM seat's requests

### Fixed — the render helper's drain reads a vLLM seat's requests, so a media lease no longer unloads it under a request in flight

- **`render/gpu-lock.mjs` `quiesceLlamaSwap` read only llama-server `/slots`.** A vLLM seat has no `/slots` route, so a loaded
  vLLM seat was named unknown, given the brief grace, and unloaded by `freeLlamaSwap` ("proceeding without a verified
  drain") under a request in flight: on the reference 3-card box, 2026-10-09 16:56 UTC, a card-scoped media lease cleared
  the two-card 27B vLLM seat 4 s after it started and llama-swap answered the running chat completion 500 "unspecific
  error: aborted". The drain now reads `/upstream/<id>/metrics` when `/slots` is absent (404 or 501) and sums the same
  gauges the Go-side drain reads (`internal/seatload.InflightGauges`: `vllm:num_requests_running` and `_waiting`,
  `llamacpp:requests_processing` and `_deferred`); a seat that answers neither, or whose exposition carries none of them,
  stays unknown and never reads as idle. Tests: `parseInflightMetrics` on labelled float samples, a vLLM seat drained
  through `/metrics`, a seat answering neither route named unknown, an exposition without the gauges named unknown.
  Not changed here: the helper still clears every non-keep-set model whatever card it sits on, and a media lease is not
  exclusive, so a text seat reloads under it between renders; both are design items for a later release. A raw vLLM batch
  client that wants never to be drained at all holds `gpu reserve --class text --cards ...` around its job.

## [0.175.0] - 2026-10-09 - a RAM-spill MoE is the 6 GB node's agent seat, and the memory stack gains EmbeddingGemma-2

### Added — ampere-6's agent seat is Qwen3.6-35B-A3B, a RAM-spill MoE, on a box with 28 GB or more of RAM (ADR 0080)

The operator's 2026-10-07 ruling on RAM spill ("acceptable as long as it truly adds capability and does not become a RAM hog or
make the system unstable") met its first seat. `qwen3.6-35b-a3b-agent` (unsloth `Qwen3.6-35B-A3B-UD-IQ3_XXS.gguf`, 12.30 GiB, 35B total
and about 3B active, Apache-2.0) keeps the routed experts in host RAM through `--n-cpu-moe 40` and attention, the dense layers and
the KV cache on the card. Measured in the 2026-10-07/08 lean bake on the reference 6 GB node (llama.cpp b10964), blind pack G4
(24 judgements): overall 8.11 against the incumbent Qwen3.5-4B's 7.26 (gap 0.85, bar 0.5), 17 of 24 head to head, hard-8 set in
1,399 s against 4,818 s (decode 16.5 against 7.9 tok/s, heat-soaked); about 11.6 GB host RSS while loaded, released on unload,
MemAvailable never under 5,446 MiB on a 32 GB box, and about 2.1 GB on the card at the 32768 window with q8_0 KV; no abort and
zero swap-in. gpt-oss-20b (void) and Gemma 4 26B-A4B (failed spill stability) were the other arms.

- **The tier flag and the gate.** `profiles.json` `include_qwen36_35b` (ampere-6 only) lets a tier carry the entry. It renders only
  where the box's `ram_tier` is `low`, `mid` or `high` (28 GB and up, which is what a 32 GB box reports), through
  `spillSeatIncluded`; a `min` box renders no spill seat and keeps the Qwen3.5-4B as the `agent-seat` (its roster differs from
  before only by the `embeddinggemma2` entry below, which has no RAM gate), and an UNSPECIFIED `--ram-tier` is below the floor
  too, so `install render` without the flag serves the 4B. The entry is a literal copy of the measured configuration in `llama-swap.linux-cuda.yaml` and
  `llama-swap.win-cuda.yaml` (`--n-cpu-moe 40`, `--cache-ram 0`, `--load-mode none`, a 32768 window, q8_0 KV, no `--reasoning`
  flag because it is a thinking model), one more heavy alternative in the interactive set, never a resident. It needs llama.cpp
  b10964 or newer.
- **A second RAM overlay, `config_seed_ram_low_up`.** The only seed overlay was `config_seed_ram_mid_high` (56 GB and up), and the
  measured node has 32 GB and classifies as `low`, so binding the seat there would have rendered it on the measured node and
  left its config on the 4B. The new layer applies on `low`, `mid` and `high` (before the mid/high layer, which wins key by key)
  and never on `min`; ampere-6 binds `agent_model` to the seat in it while its base seed stays `qwen3.5-4b-agent`. The render and
  the seed call one predicate, `tierseed.RAMLowUp` (case-blind and trimmed, as `install render` reads its flag, so `--ram-tier LOW`
  reaches both commands the same way; an empty tier is below the floor on both), so a config cannot name a seat the roster dropped.
  `install.ps1` applies the
  same layer (`Test-RamLowUp`), and `audit-config` now compares a `low` box against the base seed plus this layer instead of the
  base seed alone, so a correctly installed 32 GB node does not read as drifted.
- **The alias moves, the 4B stays.** The seat claims `agent-seat`; `qwen3.5-4b-agent` (and `qwen3.5-9b-agent`) hands the alias
  off and stays rendered as an un-aliased opt-in rollback. `include_qwen36_35b` beside `include_mimo_9b` is refused by name.
- **The spill is sanctioned by the tier's measured number.** ampere-6 declares `n_cpu_moe_max: 40`, the first tier to declare
  one; the write gate (INV-1) still refuses a render whose `--n-cpu-moe` exceeds it. `moe_26b` stays `drop`.
- **Windows installer.** `model-qwen36-35b` joins `$PINNED` (13,211,155,424 bytes, sha256 `9c964e65…88fbe`, read from the Hugging Face
  resolve redirect's `X-Linked-Size` / `X-Linked-ETag`; no model bytes were downloaded and the bake node's own copy was not
  re-hashed). It downloads, into its subdirectory, only when the tier carries the seat and `ram_tier` is `low` or above.
- **Docs.** [ADR 0080](docs/architecture/decisions/0080-a-ram-spill-moe-is-the-agent-seat-when-it-earns-it.md) (Proposed: the operator
  made the decision, an agent does not accept an ADR), ampere-6's notes (the "no expert may sit in RAM" paragraph is amended, the
  evidence is appended), `docs/tiers/ampere-6.md` (both `agent_model` rows), `docs/systems/setup-installer.md`.

Tests (each name says what it pins): `TestTheAgentSeatEachRAMTierBindsIsTheOneItsRenderServes` (ampere-6 at min/low/mid/high on
both OSes: the bound seat is the served one, one holder of `agent-seat`, a spill only where the seat is),
`TestNoOtherTierRendersTheSpillSeatOrAnNCPUMoESpill`, `TestTheSpillSeatIsSanctionedByTheTiersMeasuredSpillAndNothingElse`,
`TestTheSeedOverlayAndTheRenderShareOneRAMPredicate`, `TestRAMLowUpIsExactlyLowMidHighWhateverTheCaseOrPadding`,
`TestTheInstallCommandsSeatTheSpillSeatAndWarnForItsWeightOnlyWhereItIsRendered` (both commands, with the flag absent, upper-cased
and padded), `TestQwen3635BSpillSeatKeepsEveryMeasuredFlag`,
`TestQwen3635BSpillSeatTakesTheAgentSeatAliasFromTheSmallSeats`, `TestDroppingTheQwen3635BSpillSeatLeavesNoTraceOfItAnywhere`,
`TestDroppingTheQwen3827BAgentEntryKeepsTheFirstLineOfTheSpillSeatsComment`,
`TestResolveAppliesTheLowUpOverlayOnLowMidHighAndNeverOnMinOrUnknown`,
`TestAuditConfigDetectedLowRAMKeepsAnInstalledSpillSeatNodeClean`; the agent-window, fleet-cap and overlay-load gates now cover the
`low` tier and an overlay-bound seat; `setup/render.tests.ps1` and `setup/tests/install-config-seed.test.ps1` gain the per-band and
download-gate checks.

### Added — `embeddinggemma2`, the memory stack's second embedder (EmbeddingGemma-2 Q8_0 + its multimodal projector), and the llama.cpp build it needs

By the memory-stack session's order of 2026-10-08, EmbeddingGemma-2 replaces EmbeddingGemma-300m for the whole memory stack; the
harness ships the entry and the memory stack owns the cutover. Measured on the reference 6 GB node: 1,196 MiB loaded, 1,466 MiB peak
after image embeds, 1,536 MiB after a short video with the projector; 460 MiB loaded and 482 MiB peak without it, at
`--ubatch-size 2048`. The memory authority (ampere-6) runs the entry with its projector; the replicas run it text-only (below).
**The build requirement:** the GGUF's architecture (`gemma-embedding2`) needs llama.cpp **b11452 or newer** (upstream PR #30054,
merged 2026-10-06); b11490 is the build it was proven on.

- **The entry, on every template that renders the stack** (`linux-cuda`, `linux-vulkan`, `linux-cpu`, `win-cuda`,
  `win-cuda-resident`, `win-cpu`, `win-vulkan`, `win-dual-cuda`, `win-dual-blackwell`, `win-triple-blackwell`): `embeddinggemma2`,
  `--model embeddinggemma-2-Q8_0.gguf --mmproj mmproj-embeddinggemma-2-Q8_0.gguf --embeddings --pooling mean --ctx-size 4096
  --batch-size 4096 --ubatch-size 2048 --n-gpu-layers 99 --flash-attn on` (the authority's entry; a replica's has no `--mmproj`),
  `ttl: 300`, no aliases (mem0 selects the id). Like
  `embeddinggemma` it bypasses `${common}`; the CPU templates run it without `-ngl` and `--flash-attn`. It is a member of the
  stack's residency set (`emb & rer & eg2`; `emb & eg2` where a template has no reranker; the one `resident` set of the
  all-resident templates), so it is resident beside the swappable seats and never swapped by them, and it carries the stack's
  evict cost. `embeddinggemma` (300M) stays on every template, for the harness's own embed lane and the callers of its aliases
  `text-embedding` / `local-embed`; retiring it is a later release.
- **Gated by a tier field, default false.** `include_embeddinggemma2` is true on ampere-6, the card it was measured on, and on
  ampere-8 and blackwell-3x16 as text-only replicas, and on no other tier;
  `TestTheEmbeddingGemma2FlagAndProjectorAreSetOnExactlyTheirTiers` makes adding a tier a deliberate diff. No RAM gate: it is
  VRAM-resident. With the flag off the entry, its matrix var and its `evict_costs` row are all stripped (`dropModel` matches a var by
  its value and cannot see the evict row, so `dropEG2` removes it itself instead of relying on llama-swap to tolerate a cost for an
  undeclared var, which was not verified).
- **Replicas render the entry text-only: `embeddinggemma2_projector: false` on ampere-8 and blackwell-3x16, and the authority
  (ampere-6) keeps the projector.** A replica only embeds text (media adds go to the authority) and text vectors are identical with
  and without the projector (cosine 1.0, measured on the reference 6 GB node), so the projector buys a replica nothing and costs
  it VRAM: 1,196 MiB loaded and 1,536 MiB at peak with it, 460 MiB loaded and 482 MiB at peak without it, at
  `--ubatch-size 2048`. The residency matrix declares the stack's members valid beside whichever seat is loaded and llama-swap does
  not check VRAM, so a tier that carries the entry must have room on its card. On record, ampere-8 binds `mimo-9b-agent` at ctx 65536
  (6,707 MiB peak on the 3070 Laptop) beside `embeddinggemma` (about 460 MiB): with the projector the entry would add 1,196 to 1,536
  MiB, 8,363 to 8,703 MiB on a card of 8,192 MiB, so the projector would exceed the card; text-only it is 6,707 + 460 + 482 = 7,649
  MiB (8,027 with the reranker the Linux template adds). blackwell-3x16's utility card holds the vl-8b OCR seat (11,751 MiB),
  whisper (2.2 GiB declared), `embeddinggemma` (458) and the reranker (378), 14,839 MiB: with the projector 16,035 to 16,375 MiB on a
  card that reports 16,311, no headroom after an image embed and over after a video one; text-only 14,839 + 482 = 15,321 MiB. Neither
  box was measured with the entry loaded (the entry was measured on the reference 6 GB node only), so an on-box co-residency
  measurement can still turn the projector on: set the field true and record the figures in the tier's notes and in
  `eg2CardBudget`. **The replica entry is the authority's entry with exactly one argument removed**: id `embeddinggemma2` (the memory
  stack's restore check greps that id in `/v1/models`), the same ggml-org Q8_0 GGUF (sha256 `2188ac1d…`), `--embeddings --pooling mean
  --ctx-size 4096 --batch-size 4096 --ubatch-size 2048 --n-gpu-layers 99 --flash-attn on`, llama.cpp b11452 or newer, and no
  `--mmproj`. `--ubatch-size 2048` is load-bearing (the stack's hot budget is 1,900 tokens; a smaller ubatch returns HTTP 500 on long
  memories). The strip is `servingtmpl.dropEG2Projector`, a mirror of the other gated strips, rather than a template token, so the
  templates keep the authority's entry verbatim (the gated-weight contract test still pins the projector's filename in them): it
  removes the one `--mmproj <GGUF>` argument from the `embeddinggemma2` entry and from nothing else, and refuses the render by name
  if the entry does not carry that argument exactly once or any `--mmproj` survives in it. `Params.EG2TextOnly` (zero means with the
  projector, so every existing caller renders what it always did) comes from the profile field, which is optional: **absent means
  true**, only an explicit false makes a tier text-only. The projector GGUF is not downloaded for a text-only tier, the missing-weight
  warning names only the model for it, and `TestTheEmbeddingGemma2FlagIsOnTheTiersWhoseRecordedFootprintsFitTheCardAndOnlyThem` is
  exact in both directions: a tier that carries the projector needs the projector sum to fit its card, and a text-only tier needs the
  text-only sum to fit and the projector sum NOT to fit, so a new measurement forces the projector to be turned on (or the row
  corrected) on purpose. It is a necessary condition (a peak sum against the largest card figure on record), not a proof of fit.
  **On a text-only box the memory stack's `MEM0_MEDIA_EMBEDDER=off` (memory stack 1.35.1) makes offline media search a clean 400**
  (a figure from the memory stack's owner; not exercised by this repo's tests).
- **Spec hashes.** The provenance fields `include_qwen36_35b`, `include_embeddinggemma2` and `eg2_text_only` are omitted from the
  canonical basis when false, so they add nothing to the Params portion of any tier that does not carry them (the authority's
  entry, with its projector, hashes without `eg2_text_only`). The text-only field IS in the basis and in the `Params()` inverse the
  stale-render re-derive replays: without it a replica would be re-rendered with the projector and read stale forever. They keep no hash still: `template_sha256`
  is in the basis and ten of the eleven llama-swap templates changed (all but `linux-rk3588`), so every node rendered from one of
  them reads STALE until it is re-rendered, as for any template edit, and the tiers whose own `profiles.json` entry changed
  (ampere-6, ampere-8, blackwell-3x16) also move `profiles_entry_sha256`.
- **Config defaults.** `internal/config` `MemoryStack` and `render/gpu-lock.mjs` `DEFAULT_MEMORY_STACK` gain `embeddinggemma2`, appended
  (an unserved name is inert; `EmbedModel()` still falls back to `embeddinggemma`); `config.example.json` is regenerated. A config
  that NAMES a `memory_stack` replaces the default, and the installer's seeded `config.json` names two entries, so the tiers that
  render the entry (ampere-6, and ampere-8 and blackwell-3x16 as text-only replicas: a projector-less entry is as much a stack
  member) seed `config_seed.memory_stack` with
  `[embeddinggemma, bge-reranker-v2-m3, embeddinggemma2]`; without it a fresh install on those tiers would let `--unload-seat`, the
  render helper and fleet reclaim unload the entry. The shared template stays at two entries on purpose: `llamaswap bind check`
  reports a listed name the roster does not serve as dangling, which would flag every other tier. A node that carries its own
  hand-written list must still add `embeddinggemma2` itself.
- **Windows installer.** The llama.cpp pin moves from **b9934 to b11490**, with the six assets re-pinned from the GitHub release API
  digests (the vulkan, cpu and cuda-13.4 zips were also downloaded and hashed against them; the 12.4 cudart zip is byte-identical to
  the old pin). **The CUDA 13 asset family is `cuda-13.4` at this tag (it was `cuda-13.3`), so the `llama-cuda13` / `llama-cudart13`
  URLs changed, not only their hashes;** `Select-CudaBuild` keys on the driver's CUDA major (13.0 or newer), so the selection did not
  move. The two EmbeddingGemma-2 GGUFs join `$PINNED` (309,855,456 and 554,821,024 bytes; sha256 `2188ac1d…` and `c4a8a526…`, read from
  the Hugging Face resolve redirect and equal to the values the memory-stack session measured on) and download, together, for a tier
  that carries the projector; a text-only tier downloads the model alone (`Get-GatedModelKeys -IncludeEmbeddingGemma2Projector`, read
  from the profile's `embeddinggemma2_projector` with the same strict-boolean check, absent meaning true). That build also provides the `--load-mode` the Qwen3.6-35B-A3B spill seat above needs.
- **Docs.** `docs/systems/setup-installer.md`, `docs/systems/gpu-lease.md` (the keep-set), `docs/systems/fleet-node.md`, the model table in
  `CLAUDE.md`, the tier pages (generated), `setup/SETUP-AGENT.md`'s asset name.

Tests: `TestEmbeddingGemma2IsTheMeasuredStackEntryOnEveryTemplate`, `TestEmbeddingGemma2JoinsTheStacksResidencySetBesideEmbeddinggemma`,
`TestEmbeddingGemma2StackSetsAreExactlyTheMemoryStackPlusIt`, `TestDroppingEmbeddingGemma2LeavesTheRenderThatExistedBeforeIt`,
`TestIncludeEmbeddingGemma2OnAnEntrylessTemplateIsRefused`, `TestEmbeddingGemma2RendersOnExactlyTheTiersThatCarryItOnEveryOSTheyRenderOn`,
`TestTheEmbeddingGemma2FlagIsOnTheTiersWhoseRecordedFootprintsFitTheCardAndOnlyThem`,
`TestTheEmbeddingGemma2ProjectorIsRefusedOnTheTextOnlyTiers`, `TestTheEmbeddingGemma2FlagAndProjectorAreSetOnExactlyTheirTiers`,
`TestEmbeddingGemma2TextOnlyIsTheAuthoritysEntryMinusTheProjectorOnEveryTemplate`, `TestEmbeddingGemma2TextOnlyLeavesTheOtherProjectorsAlone`,
`TestEmbeddingGemma2TextOnlyWithoutTheStackMemberChangesNothing`, `TestTextOnlyOnAnEntrylessTemplateIsStillRefusedByName`,
`TestDropEG2ProjectorIsExactAndScopedToTheEntry`, `TestTheTextOnlyEntryOfEachReplicaTierIsTheAuthoritysEntryMinusTheProjector`,
`TestTheCanonicalBasisOmitsTheTextOnlyFieldWhenFalseAndCarriesItWhenTrue`, `TestATextOnlyTierStillSeedsTheKeepSet`,
`TestTheInstallRenderCommandWarnsForExactlyTheEmbeddingGemma2FilesTheTierDownloads`,
`TestWarnMissingGatedModelsNamesBothEmbeddingGemma2Files`, `TestWarnMissingGatedModelsNamesOnlyTheEmbeddingGemma2ModelForATextOnlyTier`,
`TestDefaultMemoryStack` (and the Go/Node agreement test), the PowerShell
download-gate, pin and llama.cpp-tag checks (`setup/tests/install-config-seed.test.ps1`, which also gains the text-only download set and
the per-tier projector and keep-set checks) and the per-tier render checks in
`setup/render.tests.ps1` (the replicas' command line compared word for word with the authority's); each new rule was mutation-checked once.

## [0.174.0] - 2026-10-08 - a seat that fills under the hint's fallback is a refusal, not the result

### Fixed — a remote hint's no-wait seat fallback files a seat that filled in between as a refusal, not as its result

- **`hintSeatWithoutAWait` (ADR 0078 decision 5) reads the seat free and runs it; when the seat's run-cap line filled between
  that read and the run, the seat's admission declined the subtask as capacity with nothing run (ADR 0063 decision 3), and the
  fallback published that defer as the seat's result.** A sheddable run then read as a capacity defer of the local seat, with
  the seat as the place it ran, instead of the shed it was; with the wait switched off, the defer named the seat as having run
  it. The declined attempt is now filed as the refusal the capacity wait files the same answer as (`refusalLine`, credited like
  it), and the caller's outcome stands with that refusal on it. From the R4e regression critic (2026-10-08); not seen live.
  Tests: `TestAGatedRemoteHintIsShedWhenTheSeatsLineFillsBetweenTheReadAndTheRun` (red before),
  `TestAGatedRemoteHintDefersAsCapacityWhenTheSeatsLineFillsWithTheWaitOff`.
- **A test pins that the bare remote route's `retry_note` names no pin on a seatless box**
  (`TestTheBareRemoteRoutesRetryNoteNamesNoPinOnASeatlessBox`). The two `remotePinned()` keys of the skipped-retry notes in
  `runOne` could be widened to the route (`r.route == "remote"`) with every test green, because the bare route's retry always
  had the local seat to go to and never reached those notes; a delegation client is the box where it has not.
- **`TestAWaitRunsToTheCallDeadlineNotTheConfiguredTTL` no longer measures the runner's speed.** The credited
  `capacity_wait_sec` is the wait's idle time: each re-ask of the full node is an attempt, charged to the budget and left out
  of the credit (`awaitCapacity`'s `spanStart`). At the test's 20 ms poll the wait re-asked the node some 75 times inside the
  1.5 s the node was full, and on a loaded CI runner those re-asks took 0.44 s of it, so the credit read 1.06 under the test's
  1.2 floor (2026-10-08, the 0.172.0 build job; passed on rerun). The test now polls every 250 ms, a handful of re-asks, and
  its `fullUntil` hook starts the node's window at the first dispatch instead of at fixture build, so setup never counts either.

## [0.173.0] - 2026-10-08 - Remote media routing, the media-job door, and iGPU media engines (sd.cpp video and animate, audio.cpp voice and music)

A caller on any machine can now send one render to a fleet node together with its input files, name the node or let the roster place it, and get the output back with its bytes verified (the media-job door, ADR 0077); a node advertises and admits a media task only while its route is actually CONFIGURED, so a missing weight drops the task out of the roster instead of failing jobs. A box whose only GPU is a Vulkan iGPU, with no CUDA, no ROCm and no model on the CPU, now serves video (I2V and T2V), character animation, voice with cloning and music through stable-diffusion.cpp and audio.cpp, advertises those lanes to the fleet, and ships the measured amd-gcn seed for them.

### Added — the amd-gcn tier seeds its measured iGPU media set (register CT-51, I5)

`setup/templates/profiles.json` `profiles.amd-gcn.config_seed` now carries the lanes measured on the Vega 7 reference box (no model on
CPU, every backend Vulkan): video family `fastwan` (engine sdcpp, FastWan2.2-TI2V-5B q8_0 + Wan2.2 VAE + umt5-xxl Q8_0 + the opt-in
`taew2_2` fast decode; 3 steps, cfg 1, flow_shift 5, euler, 832x480x49 at 24 fps; token cap 5200 / stride 16; Apache-2.0,
`commercial_use` true; `videogen_timeout_sec` 7200), animate on sd.cpp (Wan2.1 VACE 1.3B fp16 `.safetensors`, Apache-2.0, with the
depth-anything.cpp control video: 288x512, `animategen_frames` 33 (the default clip), 20 steps, cfg 6, token cap 5800 / stride 8;
`animategen_timeout_sec` 5400) and voice + music
on audio.cpp (Chatterbox q8_0, ACE-Step 1.5 turbo bf16; backend `vulkan`, device 0). Paths follow the seed convention
(`__OFFLOAD_HOME__/models/<subdir>/<file>`, engines under `__OFFLOAD_HOME__` with `__EXE__`). The ACE-Step licence is not verified, so
its `commercial_use` stays unset and the tier notes say it must be checked at the ACE-Step source before anything records it. The nine
weights are pinned (url, name, size, sha256, version) in `setup/install.ps1`'s `$PINNED` table, the one place the repo pins media
models (the Linux install has no media leg yet, so they are pin-only: no gate downloads them), and their sizes are mirrored in
`internal/mediacap/modelsizes.go` (`TestKnownModelSizesMatchInstaller`) except `wan_2.1_vae.safetensors`, which every ComfyUI Wan
route also binds. The seed needs 0.173.0: 0.172.0 and earlier do not know the keys. Tier pages: `animategen_*` and `audiocpp_*` keys are media
keys (they were filed under the non-media heading, so `ampere-16` listed its `animategen_script` there), a nested seed object renders as
compact JSON instead of a Go map dump, and the README media column names the iGPU engines. Tests (`internal/tierseed`):
`TestAmdGcnMediaSeedRoutesFollowTheFiles` (the resolved seed derives the four routes BOUND-BUT-MISSING on a fresh install and
CONFIGURED once the files are placed), `TestAmdGcnMediaSeedValues`, `TestAmdGcnMediaSeedWeightsArePinned`; each was broken once and seen red.

### Added — a node whose lanes run on the native engines advertises and admits them (register CT-51, I1)

`internal/fleetnode` bound a media task only through its ComfyUI/python script (`videogen_script`, `animategen_script`,
`voicegen_script`/`musicgen_script`/`tts_endpoint`), so an iGPU box whose video, animate, voice and music run on the
sd.cpp and audio.cpp engines (CT-49) set none of them and never advertised or admitted `video-gen`, `animate` or `audio-gen`,
even with every route CONFIGURED. `mediaTaskBound` now binds `video-gen` through `config.VideoGenBound()`, `animate` through
`AnimateGenBound()` and `audio-gen` through `VoiceGenBound() || MusicGenBound()`; `run-graph` is unchanged (ComfyUI only). The
route verdict still decides: a bound engine whose model file is missing is BOUND-BUT-MISSING and the task is refused at
admission with the `503` route-not-ready, and a box with neither a script nor an engine behaves exactly as before.
The advertised model family stays in the namespace the lane records its footprints under: an sdcpp default video family
advertises its own name (the wan22 sentinel keeps the store's `wan2.2`), the sd.cpp animate lane advertises
`config.AnimateSdcppFootprintFamily` (`wan-vace`, the constant both sides use), and audio-gen advertises the audio.cpp family
names the lanes record (`chatterbox` for voice, `ace_step` for music) instead of the ComfyUI `acestep`. Every one of these lanes
does record a footprint, so none is advertised without one. Tests: `TestEngineOnlyBoxAdvertisesAndAdmitsItsMediaLanes`,
`TestEngineOnlyBoxRefusesALaneWhoseBoundFileIsMissing`, `TestABoxWithNeitherScriptNorEngineKeepsItsBehaviour`,
`TestEngineLaneFamiliesAreTheRecordedOnes` (fleetnode) and `TestEngineLaneFamiliesMatchTheAdvertisedOnes` (pipeline, the pin
extended from `TestVideoFootprintFamilyMatchesTheAdvertisedFamily`); each guard was broken once and seen red.

### Added — media jobs run on a fleet node, with their input files, and come back verified (ADR 0077, register CT-50)

A machine with no render lane, or a caller who names a node, can now render an image, a clip, a character animation, a voice
or music clip or a ComfyUI graph on the fleet. Nothing in the repository dispatched the five media tasks before: the MCP doors
and CLI verbs called the local pipeline directly.

- **Node: the media-job door.** `POST /fleet/media-job` (task `media-job`) takes one media task with its input files (a
  `video-gen` still, an `animate` reference and driver, an `audio-gen` clone sample) from a holder of the fleet token. It is
  closed unless `fleet_media_inputs` is true, `fleet_auth_token` is set and a media task is bound; the bearer is checked
  before the body; the bundle is capped (`fleet_media_inputs_max_mb`, default 256 MiB), sha256-checked, extracted into
  `<media_dir>/fleet-inputs/in-*` and sniffed by magic bytes per field kind; the inner task is built by the builder
  `/fleet/dispatch` uses; the directory is removed with the job and swept at startup. `media-job` is token-gated, so its jobs
  are masked from tokenless polls.
- **Node: artifacts.** A finished media job's data gains `artifacts: [{name, bytes, sha256}]` for every output inside
  `media_dir`. Additive.
- **Node: honest advertisement.** `video-gen`, `animate`, `audio-gen` and `run-graph` are advertised and admitted only while
  `internal/mediacap` derives their route CONFIGURED (default configs ship every script bound, so a box listed tasks whose
  weights were missing and failed the first job). `/fleet/health` gains `media_routes`; its task and family lists are derived
  per request from the same reading, cached at most 60 s. An endpoint-only voice node now advertises `audio-gen`.
- **Client: `internal/mediaremote`** mirrors `composeremote`: `route` local, auto (here when this machine has the lane,
  read from the files; with no lane and no fleet it still runs here) or remote; node pick by task, `media-job`, route verdict,
  text lease, held lease, queue and config order, every miss named; outputs fetched by name and verified against the node's
  sha256 before anything lands. `delegate.NodeView` decodes `media_routes` (absent = unknown) and any held lease.
  On a machine that renders here the five media doors still go through the per-card admission (0.164.0): the job reaches
  the pipeline through the server's own `runTask`, so `waiter_token` is taken and a queued answer's place in line is kept
  exactly as on the doors that have no route. A route that sends the job to a node does not carry `waiter_token` (a place in
  line is on one machine) or run_graph's `devices`, which defers by name.
- **Doors.** `offload_generate_image`, `offload_generate_video`, `offload_animate_character`, `offload_generate_audio` and
  `offload_run_graph` gain `route` and `remotes`; `generate-image`, `generate-video`, `generate-audio`, `animate-character` and
  `run-graph` gain `--route` and a repeatable `--remote`. No tool was added. `refine=false`, `tts_voice`, `transformer` and (`run_graph`) `devices` defer on a remote
  route (the node's tasks cannot carry them).
- Closes the input half of register C-90 for media inputs (STT stays a separate door).

Tests: `TestMediaJobDoorChecksTheDoorAndTheBearerBeforeReadingTheBody`, `TestMediaJobRefusesBadPayloadsAndLeavesNothing`,
`TestMediaJobRewritesTheInputFieldsAndRemovesTheDirectory`, `TestSweepOrphanedInputDirs`,
`TestWithArtifactsListsEveryOutputInsideMediaDir`, `TestFinishedMediaJobDataCarriesArtifacts`,
`TestAMissingWeightDropsVideoGenFromTheAdvertisement`, `TestMediaRoutesAreCachedForAtMostSixtySeconds`,
`TestAStillTravelsAsAMediaJobBundleWhoseHashMatches`, `TestAShaMismatchDefersAndLeavesNoFile`,
`TestANodeWithoutTheRouteIsSkippedAndNamed`, `TestCallerRemotesMustBeAmongDelegateRemotes`,
`TestMediaToolsAdvertiseRouteAndRemotes`, `TestMediaVerbsRouteToAFleetNode`.

### Changed — review round for the media-job work (register CT-50)

Behaviour that differs from the first cut of the entry above (all of it unreleased):

- **`fleet_media_inputs_max_mb` defaults to 256, not 512.** A node holds the base64 request body and the decoded bundle
  together while it admits a job; the default keeps that peak under about 0.6 GiB (the door takes one body at a time). Raise it for a node with the RAM.
- **A running or queued media job no longer pins its request body.** The admission closure keeps only the job id and task
  type; the door decodes the head of the body without materialising the bundle and base64-decodes the bundle straight out of
  the body, so the extracted directory is the only copy the job holds (a 24 MiB bundle held 64 MiB of heap for the job's
  whole life before; the test bounds it).
- **`run_graph`'s `out_dir` is honoured on a remote route.** It never travels to the node; the fetched outputs are written into
  it (created if missing, refused before the network when it cannot be created). `out` still takes the primary output.
- **A fetched output never replaces an existing local file.** Each is staged under a unique temp name in its destination
  directory and its final name is claimed exclusively: the primary keeps the node's file name in `media_dir` when it is free,
  every other output is prefixed with the remote job id (an `out_dir` keeps the node's names and prefixes only a taken one).
  Only the caller's own `out` is replaced, and it is placed last; if a placement fails part-way every temp and claim is
  removed and the defer names the files that already landed.
- **A bound media task whose route is not CONFIGURED is a `503`, not a `400`.** The refusal names the route and its mediacap
  state and is re-placeable; a task that is not bound at all keeps `400 unsupported task_type`. The `media-job` door answers
  the same when every media task it carries is bound but not ready.
- **The pull claim loop** advertises the tasks the node can run now (re-derived every claim) instead of the startup list.
- **The `media-job` file-field guard** matches field names without regard to case (the builders do), so `{"Still":"/etc/passwd"}`
  is refused; a shipped file replaces every spelling of its field.
- **Defer classes.** A call whose own deadline passed is `budget` and names the node and remote job; a 401 or 403 on the poll or
  the fetch is `config` (and a 404 or a repeated poll failure still ends the wait); an input file this machine cannot read is
  `contract` while its own temp directory, disk or packer failing is `infrastructure`; an unencodable parameter is a `contract`
  defer naming the cause; graph and manifest are checked together against the 1 MiB dispatch body.
- **The node is chosen before the bundle is packed**, so a fleet with no eligible node costs one probe, not the bundle.
- **Route verdicts are read once per health request and per admission** (the node keys its cache once at construction) and a
  slow derivation no longer holds one lock for every config; `route: auto` caches this machine's lane verdict for at most
  60 s instead of walking the files on every call. The door's read and write deadline extensions report a writer that cannot
  carry them, once per route.
- **`animate-character` takes `--route` and `--remote`** like the other media verbs.

Tests: `TestAnInFlightMediaJobDoesNotPinItsRequestBody`, `TestMediaJobFileFieldsAreRefusedAsNodePathsInAnyCasing`,
`TestMediaJobCarriesOnlyItsFiveTasks`, `TestABoundTaskWithANotReadyRouteIs503NamingTheRouteAnUnboundOneStays400`,
`TestClaimAdvertisesTheMediaTasksTheNodeCanRunRightNow`, `TestPulledMediaJobIsAckedWithArtifacts`,
`TestMediaVerdictsAreReadOncePerHealthRequestAndPerAdmission`, `TestEachMediaTaskIsAdvertisedOnlyForItsOwnRoute`,
`TestSweepKeepsADirectoryYoungerThanTheLongestTimeoutPlusAnHour`, `TestAFetchedOutputNeverReplacesAFileThatIsAlreadyThere`,
`TestAFailedPlacementRemovesEveryTempAndNamesWhatLanded`, `TestRunGraphOutDirReceivesTheFetchedOutputs`,
`TestTheTaskBudgetEndsACallThatSetNoDeadlineAsABudgetDefer`, `TestACallersDeadlineIsHonouredInBothDirections`,
`TestAPollThatIs404EndsTheWaitAtOnce`, `TestConsecutivePollFailuresEndTheWaitAfterFiveAndAResetByASuccess`,
`TestEveryMCPParameterReachesTheNodeBuilderUnderItsFieldName`, `TestEveryMediaDoorParameterReachesTheWire`,
`TestLocalConfiguredMapsEachTaskToItsOwnRoute`, `TestAHostileResultPathCollapsesToAPlainNameInsideMediaDir`,
`TestAnimateCharacterVerbRoutesToAFleetNode`, `TestEveryMediaVerbPassesRouteAndRemotesToTheRouter`,
`TestCacheDoesNotHoldOneLockAcrossEveryConfigsDerivation`.

### Changed — second review round for the media-job work (register CT-50)

- **The media-job body is read into one buffer of exactly `Content-Length` bytes.** `bytes.Buffer.ReadFrom` regrew a buffer sized
  `Content-Length + 1` to about twice its size whenever the last reads left fewer than 512 bytes free (5 of 400 loopback
  uploads), which broke the 0.6 GiB peak the 256 MiB default promises. The door now uses `io.ReadFull` and probes one byte past
  the declared length; a body longer than it declared is refused `413 request body too large` like one over the cap, and one
  shorter than it declared is a `400`.
- **Fetched files are mode 0644 less the umask**, not the temp file's 0600, so a service reading `media_dir` (or the caller's
  `out`) on Linux can read them, as before the staging change.
- **An empty `media_dir` means the current directory** for a fetch (it failed after the render with `mkdir : ...`).
- **The caller's `out` is decided first and reserved.** An `out` inside `out_dir` that shares a base name with a secondary
  output no longer lets the secondary claim that name and be renamed over; the secondary takes the job-id-prefixed name.
- **A budget or deadline that ends a call says what it cannot undo.** A media job cannot be withdrawn: it is claimed to running
  as soon as it is admitted, and `DELETE /fleet/jobs/{id}` is for agent jobs only (ADR 0064, `TestWithdrawIsForAgentJobsOnly`),
  so the node answers 405 and the job keeps its card until it ends. The client therefore sends no withdraw. A deadline that
  passes while the job is sent or rendering defers as `budget`, names the node and the remote job, and says the node may still
  be running the job and it cannot be recalled; one that passes during the fetch says the render finished and the fetch ran out
  of time.
- **Leftovers of a fetch that never finished are swept** from the destination directory before names are claimed: stale
  `.media-fetch-*.part` temps and zero-byte `media-<16 hex>-*` claim files (bounded to 4096 entries scanned and 256 removed,
  logged). "Stale" is older than the longest call budget (1 to 6 hours, the same `Budgets` a call runs under) plus an hour, so
  no call still running can lose its files, and a call refreshes the modification time of its own claims and finished temps
  after each download, so a long multi-output fetch never ages toward it. Two limits are deliberate: a crash mid-fetch can leave
  an empty claim under the node's bare file name (the primary), which the sweep does not remove because it cannot tell it from
  an empty file the user made; and an empty `media-<16 hex>-*` file older than the threshold is indistinguishable from a claim.
- **Replacing an existing `out` keeps that file's permission bits** (a private 0600 `out` stays 0600); a new `out` is 0644 less
  the umask like every other fetched file.
- **`compose-project` reports a deadline it cannot extend** like the media-job door does (it discarded the errors).

Tests: `TestReadMediaJobBodyAllocatesExactlyContentLength`, `TestFetchedOutputsLandAs0644LessTheUmask`,
`TestAnEmptyMediaDirIsTheCurrentDirectory`, `TestAnOutThatSharesANameWithASecondaryNeverDestroysIt`,
`TestABudgetThatEndsTheCallSaysTheJobCannotBeRecalled`, `TestABudgetExpiryAgainstARealNodeSendsNoDeleteAndSaysTheJobCannotBeRecalled`,
`TestABudgetThatEndsWhileTheJobIsBeingSentSaysSoAndSendsNoDelete`, `TestABudgetThatEndsDuringTheFetchSaysTheRenderFinished`, `TestAStaleFetchLeftoverIsSweptBeforeTheNextClaim`,
`TestTheSweepThresholdIsTheLongestBudgetPlusAnHour`, `TestSweepStaleKeepsAFileInsideTheThresholdAndTakesOneBeyondIt`,
`TestTouchRefreshesTheClaimsAndFinishedTempsACallHolds`, `TestAMultiOutputFetchKeepsItsClaimsFresh`,
`TestReplacingAnExistingOutKeepsItsPermissionBits`, `TestPollFailuresThatAlternateWithAnswersNeverEndTheWait`,
`TestAGraphThatEscapesPastTheNodesBodyCapIsRefusedBeforeTheNetwork`, `TestComposeProjectReportsADeadlineItCannotExtend`.

### Changed — the media lane reads the roster the way every other single-shot lane does (ADR 0074, register CT-50)

Merging main (0.165.0 to 0.168.0) put the tailnet-zone guard and the shared roster reader (`internal/rosterprobe`) under
every single-shot lane except the media one, which was written before them and probed `delegate_remotes` one node at a time
through the dial gate alone.

- **A roster entry the tailnet guard refuses is a named miss, never a dial.** `mediaremote` now reads the candidate nodes
  through `rosterprobe.Probe`: each entry is judged by the same shape check the agent lane applies (against `tailnet_suffix`
  and `tailnet_suffixes`), the rest are probed at once in configured order through the shared memo and negative cache, and the
  "probed ..." line says `not dialled, refused by the tailnet guard` for the refused one while the others still serve. The entry
  prints redacted (a token pasted into it never reaches a defer), the dispatch errors are scrubbed the same way, and a node that
  accepts a job drops out of the negative cache.
- Merge bookkeeping, no behaviour change: the decision record is ADR 0077 (main took 0072 and 0076 meanwhile), and `media-job`
  sits in main's config-taking `tokenGated` beside `compose-project` and `stt-upload`.

Tests: `TestPickNodeNamesARosterEntryTheTailnetGuardRefusesAndDoesNotDialIt`,
`TestRunNamesARefusedRosterEntryInTheDeferAndRedactsItsToken`.

### Fixed — remote attribution, an in-flight cap and gated outputs for the media-job work (ADR 0077, register CT-50)

Review of the branch against main 0.169.0 found three behaviours it had not picked up from main's stt and compose work.

- **A remote media call is attributed like every other remote lane (0.165.0, D5-D11).** `internal/mediaremote` wrote no asker
  ledger row and no PAIR card for a call it sent to a node, and its dispatches named no asker. A remote or auto-spilled media
  call now opens a `core.BeginRemote` handle, reports the dispatch to the node it picked and the node's running state, and
  finishes the handle on every exit (a result, a refused POST, a node defer, a deadline); a call that reached no node has its row
  and no card. Both POSTs, the plain dispatch and the media-job, send `X-Offload-Asker` and, only when this machine's emitter is
  off, `X-Offload-Pair-Card: node`. The MCP doors hand the lane their `runTaskAs`, which now forwards `BeginRemote` to the
  server's pipeline (without it the doors attributed nothing); the CLI verbs pass the pipeline itself. The local route and an
  auto call that runs here are unchanged.
- **The media-job door caps its in-flight uploads.** The body (a bundle of up to
  `fleet_media_inputs_max_mb`, 256 MiB, in base64) is read before the admission gates, so every concurrent upload could hold one.
  The door now takes one of `mediaJobInFlightMax` slots (one, see the release review below) after the bearer check and before the
  first body byte, releases it on every exit of the handler, and answers `503` with `Retry-After` to a caller that waits past 30 s
  (`mediaremote` reads it as a capacity defer; see the release review). `takeSTTUploadSlot` and the new door share `takeUploadSlot`.
- **The outputs of a media-job are served only to the fleet token.** They are rendered from the caller's private files (a still,
  a driver video, a voice sample), but `GET /fleet/media/{name}` served them by bare name while main bearer-gates compose-project
  and stt-upload renders. The door now names the render itself (`mediajob-<16 hex>.<ext>` under `media_dir`, set after the inner
  builder) and `gatedMediaName` recognises the stem, so a tokenless fetch is `401` exactly as a gated project render's is.
  `mediaremote` sends the fleet bearer on its output fetches (a test pins it against a node that gates the name). run-graph and
  every output of the tokenless `/fleet/dispatch` door are unchanged.
- Docs: the lists of lanes that share `internal/rosterprobe` (fleet-node.md, FLEET-NODE.md, ADR 0074) now name the media lane, as
  do the attribution headers table in pair-workloads.md and "Which node" in media-generation.md; a stale comment in
  `admit` that said the run closure captures the envelope was reworded.

Tests: `TestRemoteMediaCallIsOneCardOnTheServingNodeAndOneRow`, `TestRemoteMediaCardResolvesThroughTheFleetNodeID`,
`TestAutoSpilledMediaCallIsOneCardAndOneRow`, `TestRemoteMediaCallThatNeverReachedANodeWritesARowAndNoCard`,
`TestRemoteMediaRefusedDispatchClosesTheCardFailed`, `TestRemoteMediaNodeDeferClosesTheCardFailed`,
`TestRemoteMediaCallThatRunsOutOfTimeStillClosesItsCard`, `TestRemoteMediaSendsTheAttributionHeaders`,
`TestRemoteMediaCardTurnsRunningWhenTheNodeSaysTheJobStarted`, `TestLocalMediaRoutesWriteNoRemoteAttribution`,
`TestMediaDoorThatRoutesRemoteWritesTheAskersLedgerRow`, `TestMediaJobSlotIsHeldWhileTheBodyIsReadAndReleasedAfter`,
`TestMediaJobsInFlightAreBoundedAndWaitForASlot`, `TestMediaJobOverTheCapGets503WithRetryAfter`,
`TestMediaJobSlotIsReleasedOnEveryExit`, `TestMediaJobRendersUnderAGatedStem`,
`TestMediaJobOutputNeedsTheBearerAPlainDispatchOutputDoesNot`, `TestAMediaJobOutputIsFetchedWithTheBearerAndRefusedWithout`,
`TestGatedMediaNames`.

### Fixed — release review of 0.173.0 (register CT-51)

The release reviewer and the round-4 reviewers of the integrated branch found the items below; each is fixed before the release, and
each guard was broken once at its real call site and seen red.

- **A remote output cannot be larger than the node said it is.** The client fetched each output with no bound but its 30-minute wall,
  so a node streaming past the file it announced could fill the caller's disk before the sha256 check refused the file. The fetch now
  stops one byte past the `bytes` the node published in `artifacts` and defers as infrastructure, naming the file and the size; nothing
  is kept. A graph or manifest file the caller names is sized before it is read, so a large file named by mistake is refused without
  being read into memory. (Found by a clean-context `offload_review_diff` pass; an older node that publishes no artifacts still yields
  an `unverified` result, unchanged.)
- **A default animate request fits the amd-gcn seed's token cap (REL1, high).** The seed pinned the animate geometry (288x512) and its
  latent-token cap (5800, stride 8) to the measured 33-frame run, but no key could carry a frame count and both the lane and the runner
  defaulted to 49: 288x512x49 plus the VACE reference frame is 8,064 tokens, so on a node seeded from the tier every animate call that
  named no `frames` (`offload_animate_character`, `animate-character`, a delegator's `animate` job) was refused `token_cap_exceeded`
  before the runner started. New key `animategen_frames` is the sd.cpp animate lane's default clip when a request names none (0 = the
  runner's own 49, as before; a request's `frames` always wins; the ComfyUI animate route ignores it; a negative value is refused at
  load). The lane applies it before the pre-lease token-cap check and passes it to the runner as `--frames`, and the amd-gcn seed sets
  `animategen_frames` 33 (288x512x33 plus the reference frame is 5,760 tokens against the cap of 5,800). `config.example.json` and
  `docs/tiers/amd-gcn.md` are regenerated and the animate `frames` help of the CLI verb and the MCP tool names the sd.cpp default. A
  box that does not set the key is byte for byte as before. Tests: `TestTheSeededAmdGcnDefaultAnimateRequestFitsItsTokenCap` (the real
  seed: a default request runs and the runner gets `--frames 33`), `TestAnAnimateRequestOverTheSeededCapIsStillRefusedBeforeTheLease`,
  `TestABoxWithoutAnimategenFramesKeepsTheRunnersDefault`, `TestAnimategenFramesDoesNotTouchTheComfyUIAnimateRoute`, the seeded values
  in `TestAmdGcnMediaSeedValues` and the negative-value case of `TestNegativeRecipeValuesAreRefused`. With the lane's default reverted
  to the runner's, the first two fail with the review's own refusal (`288x512x49 + reference needs 8064 latent tokens ... cap is 5800`).
- **`render/igpu-runners-main.test.mjs` passes on Linux (REL2).** The test-only stub engine replayed its log with
  `process.stderr.write` and then called `process.exit()`; on POSIX a pipe write is asynchronous, so the tail of the 21 KB real-engine
  fixtures was lost (about 8 KB reached the runner) and the runner's positive GPU-evidence guard read a healthy run as `CPU_PLACEMENT`
  ("no GPU evidence"). Windows makes its stdio pipes blocking, which is why the Windows gate was green. The stub now writes its log with
  a synchronous `writeSync(2, ...)` that retries a momentarily full pipe (EAGAIN) and stops quietly when the reader is gone (EPIPE).
  The production runners were never at fault (they read until the stream closes). Run under Linux (Node 22.23.1): 2 of 20 failed before
  (`--offload-to-cpu` sanctioned spill, device reset), 20 of 20 pass after, none skipped; Windows (Node 26.7.0) stays 20 of 20.
- **An engine-only node opens the media-job door (REL4).** `config.MediaInputsAdmissible` bound a task only through the ComfyUI script
  keys, the speech endpoint and `run_graph_script`, while `fleetnode.mediaTaskBound` had moved onto the engine-aware helpers (CT-51 I1),
  so the repository held two notions of "bound". A node whose renderers are the sd.cpp and audio.cpp engines, with every script key
  blank (the shape of the CT-49 tests and the rockchip seed), advertised `video-gen`, `animate` and `audio-gen` but not `media-job`, and
  a remote caller could not send it a still, a reference and driver pair or a clone sample. The predicate now binds through
  `VideoGenBound`, `AnimateGenBound`, `VoiceGenBound` (which includes `tts_endpoint`), `MusicGenBound` and `run_graph_script`; a node
  with nothing bound, no opt-in or no token keeps the door closed. Tests: `TestMediaInputsAdmissibleBindsThroughTheEngineAwareHelpers`
  and `TestAnEngineOnlyBoxOpensTheMediaJobDoor` (an engine-only box advertises `media-job` and takes a bearer'd still); with the old
  predicate restored both fail, the second with `[video-gen animate audio-gen]` advertised and no `media-job`.
- **The merge-time fix in `runIGPU` is pinned (REL3).** The iGPU lanes take their lease in one place, which takes the whole node with the
  request's `waiter_token` and its door's resumability, as the sdcpp image lane does (the merge fixed it; it had taken a bare whole-node
  lease). Nothing pinned it: `TestEveryMediaDoorThreadsTheRequestsResumability` scanned `pipeline.go` only, and replacing the need with
  `wholeNeed("")` and, separately, dropping `.resumableBy(req)` both left the pipeline, mcpserver and mediaremote suites green. The scan
  now covers `igpumedia.go` with a per-file minimum, and `TestAnIGPULaneKeepsAPlaceInLineAndResumesIt` runs the sd.cpp video, sd.cpp
  animate and audio.cpp voice lanes against a held node: a resumable caller is queued with a token, a caller that cannot resume gets the
  plain busy answer and leaves no place, and the token resumes the place once the node frees. Each mutation fails it on all three lanes.
- **The sd.cpp animate family pin reads what the lane records (REL5).** The pin compared the family `fleetnode` advertises with
  `config.AnimateSdcppFootprintFamily`, the constant both sides read, so a lane recording under another name kept it green (the video and
  audio legs read the writers' own helpers). The leg now runs the lane and reads back the family its footprint was recorded under;
  `fpFamily: "wan-vace2"` in the lane fails it.
- **The media-job door takes one body at a time (C5S1, low).** `mediaJobInFlightMax` was the stt upload door's 2, copied unscaled, but
  a media-job body is far larger than an stt upload (48 MiB): at the 256 MiB default cap one slot holds the base64 text (341 MiB) beside
  the decoded bundle (up to 256 MiB), about 0.58 GiB, so the door's node-wide peak was about 1.17 GiB beside a ComfyUI render while the
  config key, the operator guide and this file promised "under about 0.6 GiB". The door now has its own bound, 1: its peak is that one
  figure and uploads are served one at a time (a second token holder waits up to 30 s for the slot, then gets the 503). ADR 0077, the
  fleet-node and media-generation pages, the operator guide and the config key state the slots x 0.58 GiB bound. Tests:
  `TestMediaJobDoorHoldsOneBodyAtATimeSoItsPeakIsTheDocumentedFigure` (computes the peak from the cap and the constant; red at 2 with
  1.17 GiB) and `TestASecondMediaJobWaitsWhileTheFirstBodyIsStillArriving` (red at 2: a second upload is answered while the first
  body is still arriving).
- **The slot-wait 503 is documented as what it is (C5S4, low).** ADR 0077, the fleet-node page and this file said a delegator re-places
  the media-job door's slot-wait 503, but the door's only client is `internal/mediaremote`, which reads the status as a capacity
  defer, does not read `Retry-After` and makes one pass over the node it picked (`internal/delegate` never posts to the door). The call
  returns the capacity defer and a later call places the job again. Upload-slot occupancy is not in `/fleet/health` or in the queue
  depth the node pick ranks on, so a node whose slot a long upload holds can still rank first. The docs now say so; no behaviour changed.
- **The upload-slot wait's cancel arm is pinned (C5S2, low).** `takeUploadSlot`, shared by the stt upload door and the media-job door,
  returns without a slot and without an answer when the caller's request is cancelled while it waits. Mutating that arm to
  `return true` survived the whole package and would have let the deferred release free a slot the request never took, so the door
  admitted more bodies than its bound and the real owner's release then blocked forever.
  `TestAnUploadWaiterWhoseRequestIsCancelledLeavesWithoutASlotOrAnAnswer` cancels a waiter's context against full slots on both doors
  and asserts it leaves at once, writes no answer and leaves every slot as it found it.
- **`mediaJobOutputPath` creating a missing `media_dir` is pinned (C5C7, low).** The pipeline creates `media_dir` only where it names
  the output itself, and the door now always supplies `out`, so an input-less job from a client other than `mediaremote` on a node whose
  `media_dir` does not exist yet would be handed a path in a missing directory. Deleting the `MkdirAll`, or the guard that keeps an
  empty `media_dir` from reaching it, left the suite green. `TestMediaJobOutputPathCreatesAMissingMediaDirAndToleratesNone` pins both.
- **The asker row's route is pinned to the normalised one (C5C6, low).** `mediaremote.Run` and `composeremote.Run` open the remote call's
  attribution with the normalised route (`local|auto|remote`, the contract in `core/remoteattr.go`). Passing the raw string instead left
  every test green, and the MCP doors omit `route`, so the commonest remote spill would have written its asker ledger row with
  `Route: ""`. `TestAutoSpilledMediaCallIsOneCardAndOneRow` now runs the routes `auto`, `""` and `"  AUTO "`,
  `TestAForcedRemoteMediaCallIsRecordedUnderTheNormalisedRoute` the spellings of `remote`, and
  `TestARemoteComposeCallIsRecordedUnderTheNormalisedRoute` both for the compose lane (the identical gap); the raw route fails them.
- **The docs and comments say what the code does (C5C1 to C5C5, C5S3, REL6, REL7).** The Auth passages (`docs/FLEET-NODE.md`,
  `docs/systems/fleet-node.md`) list a media-job's renders (`mediajob-<16 hex>.*`) among the names `GET /fleet/media/{name}` gates and no
  longer say every other name stays tokenless (the outputs of the tokenless lanes do), and the handler comment at the gate lists all four
  gated families. ADR 0074's index row and Consequences count six lanes; its Context keeps main's five, because the media lane shipped on
  the shared reader and was never one of the lanes that dialled through the dial gate alone. The `rosterprobe`, `core`, `pipeline` and
  `pairworkloads` comments list the lanes that call them (media and stt included), and the attribution-header sender list adds
  `/fleet/stt`. The `docs/FLEET-NODE.md` task table binds `video-gen`, `animate` and `audio-gen` by script OR engine, gains the
  `animate` row and names the engine families (`wan-vace`, `ace_step`, `chatterbox`). `setup/SETUP-AGENT.md` says the amd-gcn tier seeds
  the iGPU media keys and what the installing agent must still stage at the seeded paths (the engine binaries and the nine model
  files; without them the four routes read BOUND-BUT-MISSING and `doctor` prints FAIL for each).

### Fixed — CT-49 review round: the dead-air gate cannot be fooled by a dying pass, low-key footage passes, the deadline is a typed timeout

- **The dead-air gate fails closed on a pass that did not finish.** `parseLoudness` took the LAST `I: <x> LUFS` anywhere in the measuring ffmpeg's output, and every per-tick ebur128 line (one per 100 ms) prints an `I:` too, while `measure()` looked only at whether ffmpeg failed to spawn. A measuring pass that printed one tick and was OOM-killed (exit 137) gave a loudness and an empty silence list, which reads "no silence", and a silent render was delivered (the reviewer's repro: `gpugen.Generate` returned nil with a 192 KB silent file). The loudness and the peak are now read from ebur128's `Summary:` block only (printed at the very end of a pass that ran to completion); `measure()` reports the facts of the pass (`exitStatus`, `exitSignal`) and decides nothing; `gateDeadAir` requires exit 0 AND the Summary, else `UNMEASURABLE` (`unmeasurable`, not delivered). The same fail-open exists in `comfy-music.mjs`, which feeds `assessDeadAir(measure(...))` and ships on a missing measurement by design (it never withholds an already-produced render); it reads only `duration` and `silences`, so it is unchanged, and the decision for that lane is the operator's.
- **The black-clip gate no longer rejects low-key footage.** `blackdetect` `pic_th` 0.98 called a dark frame with a small bright object (a candle flame, the moon: 0.75 % to 1.5 % of the frame) black, a full `BLACK_CLIP` that is never retried. It is 0.999 now (measured with ffmpeg 6.1 on 320x240 clips: both objects pass, a pure black clip still fails, a 4 % object passed at either value).
- **The deadline and the cancel are typed timeouts.** `Generate`'s deadline and cancel branches returned plain errors embedding the child's last 400 bytes, and `ClassifyErr` substring-matched them: "...timeout...(a living room, boom)" and "...canceled...(the zoom lens)" classified as `oom`. Both now return `*RunError` with class `timeout` (message and `%w` chain unchanged).
- **The audio runner's ffmpeg and ffprobe calls are bounded** like the video and animate runners' were documented to be: the trailing-silence trim, both duration probes, the master or re-encode and the dead-air measurement carry what is left of `--timeout-sec`, each step asks `deadline.enforce` first, and a call killed at its bound is a `timeout`, never `dead_air` or `unmeasurable`. `measure()` and `durationSec()` take an optional `timeoutMs` (default none).
- **Go and Node agree on the vertical tab.** `--threads=<U+000B>cpu` was refused by the runner and accepted by `config.ScreenExtraArgs` (Go's `\s` has no vertical tab), so doctor showed CONFIGURED and every call then failed after taking the lease. U+0085 stays the one known difference, in the safe direction (Go refuses first); the shared parity table now lists it (`go_stricter`) and each side asserts its half.
- **Test pins from the mutation round (each seen red against its inverse mutation).** The process 'exit' hook's engine stop (a real engine must be dead after an uncaught error; the wait is pinned by order, which needed the hook to run with the deps `installLifecycle` was given, no behaviour change); `enginePids` on real child objects; a non-zero engine exit that still wrote valid output is a failure (audio.cpp, depth-anything, sd-cli; Go chain rows); SIGFPE / SIGTRAP / SIGSYS as crash signals on both sides; unset, empty and non-numeric options never reach an argv; the ltx25 pool reads the bound binding and `ace` is a runner family (mediacap); the `failAndExit` slow-reader test no longer flakes under load (1 of 48 runs at 8-way before, 0 of 148 after). The exit hook's engine stop and the engine wait were the two guards the round 2 entry below called "seen red" without a test that failed when they were removed; that claim now says so, and both have one.

### Fixed — CT-49 round 3: the GPU-evidence guard reads sd.cpp's new log format (master-945)

- **A healthy sd.cpp master-945 run failed as `CPU_PLACEMENT`.** The committed guard, run on a real master-945 (`a1ded76`) log from the reference node, ended `no GPU evidence ... diffusion-stage "compute buffer size ... on Vulkan<N>" line (missing)`: a fail-closed false positive that would have blocked every iGPU video and animate job on a node the day it upgrades sd.cpp (a fleet-update sweep does exactly that). sd.cpp #2104 and #2106 changed the log record: one-letter tags (`[V]` for `[VERBOSE]`, `[I]`, `[W]`, `[E]`, `[D]`), the source moved from in front of the message (`ggml_runner.cpp:1019 - `) to a trailing ` --- ggml_runner.cpp:1019` (a record of several lines has its tag on the first line and its source on the last, so a parameter dump now closes with `} --- main.cpp:699`), and a prompt echo prints its newlines escaped. The guard's record-head patterns knew only the old shape.
- **Both shapes are read, for good** (nodes upgrade at different times). Every sd.cpp line goes through `normalizeSdLine` before a shape anchored at the start of a record reads it: one leading level tag in either spelling (and, after the long one, the `file.cpp:N - ` prefix) and one trailing source tail are cut, the raw line stays for the error message, and leading whitespace stays (a dump's indented `  }` is not its close). A dump block closes on a normalised `}`. The plain-words shapes (a CPU backend, a lost device, `-> compute CPU`) still read the whole raw line, so request text hides none of them. A build between the two upstream commits, which puts ` - ` in front of the source, is read as well (from the commits; no log of it was captured). An unterminated block still ends at the next long-tag record; in the new shape the one-letter tags are deliberately no valve, so a closing line the guard does not recognise ends the run `CPU_PLACEMENT` "no GPU evidence" loudly instead of passing it on a guess.
- **The auto-fit plan is evidence too.** Both releases print `DiT   params ... -> compute Vulkan0, params Vulkan0`: the DiT line on a Vulkan device shows the diffusion stage on the GPU (as the compute-buffer line does), `-> compute CPU` for any component is a placement, and `params RAM` / `params CPU` alone is the sanctioned spill.
- **Request text stays inert in the new shape.** A record that is itself a line of the request (`[V] SDCliParams {` as a prompt line) opens no dump block; a multi-line text also counts as an echo in the one-line spelling sd.cpp prints it in (`\n` for a newline), and the message alone is asked as well, so a 20-character tail does not dilute the share the text has of the line.
- **Setup and docs.** sd.cpp master-945 (its decoded frames are bit-identical to master-929's on the measured FastWan run, about 10% faster) and audio.cpp v0.9.1 (the Chatterbox S3 encoder fix, upstream #778), built natively with the same planner prefill patch (`setup/patches/audiocpp-v0.9.0-vulkan-planner-prefill.patch` applies to v0.9.1 unchanged; the file keeps its name), are named as the measured releases in `setup/SETUP-AGENT.md` and `docs/systems/media-generation.md`, which also describe the two log formats and the normalisation rule. `SETUP-AGENT.md` tells an installing agent what a `no GPU evidence` on a run whose log shows the GPU means.
- **Fixtures** (`render/testdata/`, scrubbed like the earlier ones): real master-945 logs (`sdcpp945-video-healthy.log`, `sdcpp945-video-tae.log`, `sdcpp945-vace-healthy.log`) and real audio.cpp v0.9.1 logs (`audiocpp091-voice-clone.log`, `audiocpp091-music.log`, a build with the planner patch: the planner on `Vulkan0`, no CPU line). The negatives for the new shape are derived from them (every params and compute line, only the diffusion compute buffer, only the VACE diffusion buffer, only the plan's DiT line, the TAE run) and the old shape gets its plan negative, each file headed "derived from ... no CPU run was captured because the node's operator forbids model compute on its CPU", with the re-derive test (`derive-cpu-fixtures.mjs`).
- **Tests.** Every real healthy log of both shapes passes (also with the real prompts as the echo list), every derived negative of both shapes fails `CPU_PLACEMENT` on its own line, `} --- main.cpp:699` closes a block (and `  } ---`, `}, ---` do not), prompt text equal to part of a placement line hides nothing in either shape, the plan lines (any component on the CPU kills the run, the spill passes, the DiT line is evidence, a forged one is not), escaped newlines, `GPU_RESET` in the new shape, the audio.cpp v0.9.1 logs, and the real video and animate runners each replaying a real master-945 log end to end (the video one also its CPU-derived twin: the engine dies at that line, nothing delivered). Against the unfixed guard the master-945 tests fail with `CPU_PLACEMENT: no GPU evidence` through the real runners. The block-end mutation (the close reads a bare `}` again) turns every master-945 healthy-log test red, and 18 other mutations of the guard (the tag and tail strips, the head forms, the plan evidence and its CPU shape, the escaped spelling, the echo of the message, the block-open guard, the valve, the compute-buffer shape reading the raw line, a quadratic tail pattern) are each caught by a test.

### Fixed — CT-49 fix round 2: typed errors survive gpugen, the engine dies with its runner, the guards cannot be switched off by request text

- **A device reset was reported as a timeout.** The runner's real `GPU_RESET` line is about 470 bytes with its token at the start and gpugen kept only a 400-byte tail, so `ClassifyErr` saw "lockup timeout". Every runner now ends a typed failure with one short `IGPU_CLASS=<class>` line; `gpugen.Generate` reads the class from the last such line anywhere in the captured output (a `gpugen.RunError`) and keeps the runner's labelled line in the reason. Pinned end to end with the REAL runners and a stub engine replaying the captured logs (`internal/gpugen/igpu_runner_chain_test.go`: every typed class, with the true message length) and in the pipeline against the real error builders.
- **Classifier.** An engine crash signal is `engine_crashed` (not a timeout through "killed"); ggml `insufficient memory` / sd.cpp `alloc compute buffer failed` are `oom` (exit 1 or SIGABRT); `dead_air` and `ffmpeg_unavailable` beat the `oom`/`timeout` words of a path; `unmeasurable` and `device_invalid` are new classes; a client cancel of an iGPU lane is `timeout` like every other lane.
- **The engine is in the runner's process group** (not detached), so gpugen's SIGKILL of the group takes it along; the runner waits (bounded) for the engine tree to be gone before it exits, so the lease is not released while the iGPU is still held; a SIGKILLed runner's temp dirs are swept by the next job. Proved on Linux with the real runner SIGSTOPped through `Generate`.
- **Request text cannot switch the CPU detector off.** Echo suppression is limited to sd.cpp's dump blocks and tokenizer lines on its own record shape; every placement and evidence shape is anchored at the start of a record; an echo of the request can only fail to count as positive evidence. audio.cpp's `[TIMING ts=..]` / `[TRACE ts=..]` heads are recognised. `SD_AUX_MODULE` matches sd.cpp's runner names whole (a diffusion model called `Wan2.1-Fun-14B-Control` no longer fails for lack of evidence).
- **Gates fail closed and delivery is atomic.** A dead-air measurement with no loudness summary is `UNMEASURABLE`, a clip whose duration could not be read is `UNMEASURABLE` (and no longer claims "no picture"); mp4 and audio results are written to a partial beside the result and renamed after the gate, so a failed or deadline-killed run never overwrites or deletes a good file.
- **Config and pipeline agree with the runners.** Empty per-module backend parts (`vulkan0,`) and Unicode-space cpu values are refused in Go as in Node (one shared parity table); a token cap with no usable VAE stride is refused before the lease; `audiocpp_backend` is `vulkan` only (the evidence guard recognises only Vulkan buffers); a non-numeric `audiocpp_device` is `device_invalid`; the animate frame extraction asks the deadline and reports its own kill as a timeout; a typed failure is flushed before the runner exits.
- **Tests (seen red against their fixes, except the exit hook's engine stop and the engine wait, which the review round above pinned afterwards).** `render/igpu-lifecycle.test.mjs` (the real runners: parent death, SIGTERM/SIGINT/SIGHUP, the process group, the exit hook), `render/igpu-deadline.test.mjs` (every `deadline.enforce` site and bound, through a test-only clock preload), `render/igpu-guard.test.mjs`, `render/igpu-delivery.test.mjs`, `render/igpu-classes.test.mjs`, the animate/video main-level guard and gate cases, `internal/pipeline` (the Spec every lane hands gpugen: `OwnProcessGroup`, footprint key, ledger row; the cap at the runner defaults; each unbound key) and the per-family doctor entries. The POSIX parts run on Windows hosts only through a Linux (WSL) run, which needs ffmpeg for the main-level suite.

### Changed — `--offload-to-cpu` is sanctioned spill on the iGPU media engines

- The operator rule of 2026-10-07 allows RAM as overflow (bounded, released on unload, never destabilizing the box) while all compute stays on the GPU, so the `*_extra_args` screen (`config.ScreenExtraArgs` / `config.ExtraArgsRefusal` and `render/igpu-engine.mjs` `screenExtraArgs`) now ACCEPTS sd.cpp's `--offload-to-cpu`: it parks the weights in RAM and stages them to the device, and every compute buffer stays on the GPU. Every compute-placing flag is still refused exactly as before (`--clip-on-cpu`, `--vae-on-cpu`, `--control-net-cpu`, `--backend`/`-b`, `--params-backend`, `--rpc`, audio.cpp `--device`, any other cpu-named flag and any cpu backend value), and the positive GPU-evidence guard still passes a run whose params sit on the host (`on CPU` / `Vulkan_Host`) with compute on Vulkan and fails a compute buffer on the CPU with `CPU_PLACEMENT`. The runners never add the flag themselves (the sd.cpp VACE docs command line shows it; the runner does not copy it); it reaches sd-cli only through a binding's extra args. On a UMA iGPU box, where "VRAM" is the same memory, it only adds copies, so the amd-gcn bindings leave it off: whether a seat uses it is a per-seat measurement, not a screen. Tests assert acceptance at the config, pipeline-lane, script and runner layers.


### Added / Fixed — CT-49 fix round, phase B: runner correctness, output gates, and the pipeline contract

- **Depth and VACE, verified on the node.** The depth step runs `da3-cli depth --model M --input <frame> --png <out> --no-invert` once per frame (never the multi-view `--input a --input b` form), and every depth PNG is converted to `rgb24` at exactly W x H and read back before sd-cli gets the directory (sd-cli refuses 1-channel PNGs; `DEPTH_FRAMES_INVALID`). The VACE model is a `.safetensors` checkpoint (the public GGUFs lack `vace_patch_embedding.weight`); sd-cli's `model metadata validation failed` is a typed `MODEL_INCOMPATIBLE` naming the model file; `--offload-to-cpu` is never passed; `-i` is the VACE reference image (sd.cpp `docs/wan.md`, G14). VACE decodes exactly N frames on this sd.cpp, so the N+4 reference-latent trim is a probe-and-only-if conditional (ffprobe count; N+4 drops the first 4).
- **One runner contract.** Flags first, then `--`, then the positionals (a prompt or lyrics starting with `--` stays positional; G11). The Go side resolves every engine binary with `mediaops.ResolveBinary` and passes the absolute path, the runners refuse a bare or relative one (`BINARY_NOT_ABSOLUTE`), and `doctor` agrees (G10, G23). The output directory is created before the lease, and an uncreatable one is reported (`OUT_DIR_UNWRITABLE`; G27). `audiocpp_backend` is validated against audio.cpp's own values (`vulkan|cuda|hip|rocm|metal`) with a separate numeric `audiocpp_device`; `vulkan0` is refused with the hint at config load, in mediacap, the pipeline and the runner (G13).
- **A14B high-noise recipe and the tiny autoencoder.** `high_noise_cfg`, `high_noise_steps` and `high_noise_sampler` on a video family map to sd-cli's `--high-noise-cfg-scale` / `--high-noise-steps` / `--high-noise-sampling-method` (G9). `--vae-tiling` now carries `--vae-tile-overlap 0.25` (the measured best). `sdcpp_tae` / `animategen_sdcpp_tae` bind an opt-in tiny autoencoder: `fast: true` adds `--taesd`, and without the key `fast` is a no-op on the sdcpp lanes, which the result's `notes` say; `taew2_2` is the verified file and `lighttaew2_2` is broken (never bind it); mediacap lists the TAE as optional and it never fails a route.
- **Finalize and output gates.** `finalizeAudio` fails on any ffmpeg error with its stderr tail instead of copying the raw engine file (G22). Music trims trailing silence below -45 dB, fades out, loudnorms to -14 LUFS / -1 dBTP at 48 kHz; voice and music both pass the repo's dead-air gate (`DEAD_AIR`, file removed; G26). A video or animate clip that is entirely black or entirely frozen fails `BLACK_CLIP` / `FROZEN_CLIP` (ffmpeg `blackdetect` + `freezedetect` over the whole clip). New `err_class` values: `black_clip`, `frozen_clip`, `depth_frames_invalid`, `model_incompatible`, `binary_not_absolute`, `out_dir_unwritable`.
- **mediacap and the pipeline agree.** An sdcpp family named like a ComfyUI family (`wan22`, `ltx25`, `hunyuan`, `h3`) gives the verdict the pipeline routes on, with one binding row per family and the ledger family named for it (G2, G24, G32). `doctor`'s ComfyUI video file check reads `ResolveVideoFamilyBinding` of the canonical render family, the binding the pipeline renders with, not the flat `videogen_*` keys (G41). ffprobe is a required binding of every iGPU route; the untested route guards are pinned (ffmpeg, runner scripts, voice/music independence, the byte-identical no-engine-key route list; G36, G40).
- **For the fleet seam (CT-51).** `config.VideoGenBound()`, `AnimateGenBound()`, `VoiceGenBound()` and `MusicGenBound()` say whether a lane has a ComfyUI/python renderer (or the TTS endpoint) OR the CT-49 engine bound; `fleet-measure` probes the video and music lanes through them instead of printing "skipped (no videogen_script configured)" on an sdcpp or audio.cpp box (G15). `internal/fleetnode` is untouched.
- **audio.cpp on a CPU without AVX-512, and the ACE-Step patch.** The official release binary needs AVX-512 (SIGILL, `ILLEGAL_INSTRUCTION`); build it on the node. Upstream runs the Vulkan ACE-Step planner prefill on the host (a CPU placement): `setup/patches/audiocpp-v0.9.0-vulkan-planner-prefill.patch` (v3, three changes) moves it onto the device; the bf16 ACE-Step package is the one to bind (q8_0 is graded No upstream).
- **Tests.** Each runner's `main()` is run against a stub engine binary (`render/igpu-runners-main.test.mjs`: argv reaches the engine, the GPU pin reaches the depth child, a CPU line / no evidence / device reset / timeout leaves the engine dead and writes nothing, the gates remove what they reject, temp dirs are gone; G30); `render/igpu-qa.test.mjs` runs against real ffmpeg detector output; the pipeline lanes' seed minting, voice clone fallback, COMFY_DIR strip, no ComfyUI `/free`, empty-prompt and missing-input defers and per-request steps are pinned (G35, G37).

### Added — iGPU media engines: video, animate, voice and music on a Vulkan-only box (CT-49)

A box whose only GPU is a Vulkan iGPU (no CUDA, no ROCm, and an operator rule that no model runs on CPU) now serves
`generate_video` (I2V and T2V), `animate_character` and `generate_audio` voice (with clone) and music through the same
MCP tools, CLI verbs and fleet task types as a ComfyUI box. Each engine is a spawn-per-job native CLI under the existing media
lease (nothing resident, no ComfyUI, no Python): a `videogen_families` entry with `"engine": "sdcpp"` renders through
`render/sdcpp-video.mjs` (stable-diffusion.cpp `vid_gen`, Wan2.2 TI2V-5B or an A14B pair; the family may carry any name and
`videogen_family` may name it as the box default); `animategen_engine: "sdcpp"` renders through
`render/sdcpp-animate.mjs` (ffmpeg frames, depth-anything.cpp, sd.cpp Wan2.1 VACE 1.3B with the depth directory as the
control video); `voicegen_engine` / `musicgen_engine: "audiocpp"` render through `render/audiocpp-generate.mjs` (audio.cpp
`chatterbox` and `ace_step`, music loudness-normalized to -14 LUFS / -1 dBTP when ffmpeg is present). New keys: the
`sdcpp_*`, `steps`, `cfg`, `flow_shift` and `sampler` fields of a video family binding, `videogen_sdcpp_script`,
`animategen_engine`, `animategen_sdcpp_*`, `animategen_depth_*`, `animategen_steps/_cfg/_flow_shift`, `voicegen_engine`,
`musicgen_engine` and `audiocpp_*` (`config.example.json` regenerated). A box that sets none of them behaves byte for byte
as before (`TestEveryRouteWithNoEngineKeyKeepsItsExactArgv`, green on the unmodified base too). `doctor`, `offload_status` and
`acceptance` derive a route per engine (CONFIGURED only when the runner, binaries and every bound model file exist; a non-default
sdcpp video family gets `generate_video:<name>`). Not in this change: run-graph (stays ComfyUI-only) and per-node seeds; the fleet
advertisement of these lanes is the CT-51 entry above.

### Added — the GPU-evidence guard is positive, with GPU_RESET, a token cap, real kill semantics and extra-args screening (CT-49 safety core)

- **A run passes only on positive evidence it ran on the GPU.** sd.cpp needs a non-software `ggml_vulkan` device line and a diffusion-stage `compute buffer size ... on Vulkan<N>` line and no compute buffer `on CPU`; da3-cli needs `da::Backend using device: Vulkan<N>`; audio.cpp needs `<component>.weights.buffer_name Vulkan<N>` and any `*.weights.buffer_name CPU` is `CPU_PLACEMENT` (the real captured ACE-Step host-prefill case). No evidence by process end is `CPU_PLACEMENT` ("no GPU evidence was seen"). The parameter dump blocks, tokenizer echoes, ggml's CPU-backend registration, `Initializing backend: CPU` and any line repeating the request's prompt / negative / text / lyrics are never scanned. The format strings behind the sd.cpp lines were read at commit 3f8527a (the CPU backend prints `CPU`; `Vulkan_Host` is host-pinned memory). The synthetic fixtures are replaced by the real engine logs from the reference node (`render/testdata/`); the sd.cpp CPU negatives are derived from the healthy logs and say so.
- **`GPU_RESET`** (`err_class` `gpu_reset`): `ErrorDeviceLost` / `device lost` / `context is lost` in an engine log kills the run with a typed error that names the amdgpu 2 s lockup timeout and the token cap; never retried.
- **Token cap**: `sdcpp_max_tokens` + `sdcpp_vae_stride` (video families) and `animategen_sdcpp_max_tokens` + `animategen_sdcpp_vae_stride` (animate; the VACE reference adds a latent frame). Computed in Go before the media lease and again in the runner; a typed non-retryable defer (`token_cap_exceeded`) names tokens, cap and how to fit. The Go and Node formulas are pinned to one shared table. No cap configured = no check.
- **Kill semantics**: the iGPU lanes start the runner as a process-group leader (`gpugen.Spec.OwnProcessGroup`, non-Windows) and a timeout or cancel SIGTERMs the group, then SIGKILLs it after 5 s; the runners kill the engine tree and remove temp dirs on SIGTERM/SIGINT/SIGHUP and when the parent disappears; a signal death names the signal (SIGKILL = OOM killer hint), exit 132 / SIGILL is `ILLEGAL_INSTRUCTION` (the audio.cpp release binary is AVX-512: build it on the node); the self-timeout is a deadline from runner start covering pre-spawn work and the ffmpeg encode.
- **`*_extra_args` screening**: an element that changes the backend or placement (`--backend`/`-b`, `--params-backend`, `--clip-on-cpu`, `--vae-on-cpu`, `--control-net-cpu`, `--rpc`, `--device` for audio.cpp, any cpu-named flag or cpu value) is refused at config load (doctor FAIL), as a typed defer (`extra_args_refused`) and in each runner. Backends are now an allowlist (`vulkan` / `vulkanN`).

### Added — no model ever runs on CPU on these engines, enforced at four layers

A `cpu` or unset backend (also `cpu0`, `best`, `auto` and any per-module assignment such as `diffusion=vulkan0,vae=cpu`) is refused at config
load (so `doctor` fails on it by name), reported BOUND-BUT-MISSING by the route derivation, a typed defer in the pipeline
(`err_class` `cpu_backend_refused`), and refused again by each runner. The runners also read their engine's own log while it
runs: the first line that places a compute module on the CPU (a software Vulkan device such as llvmpipe included) kills the process tree and fails the job with `CPU_PLACEMENT`
(`err_class` `cpu_placement`) instead of finishing a long render on the wrong silicon. Tests:
`TestCPUBackendRefusal`, `TestSdcppVideoFamilyRefusesACPUBackend`, `TestAnimateAndAudioEnginesRefuseACPUBackend`,
`TestACPUBackendIsATypedDeferOnEveryIGPULane`, `TestACPUOrUnsetBackendMakesEveryIGPURouteBoundButMissing` and the node tests in
`render/igpu-engine.test.mjs` (each guard was broken once and seen red). The first cut of the log guard was negative-only and
fixture-modelled; the entry above replaces it with the positive guard pinned to real captured logs.

## [0.172.0] - 2026-10-08 - a single-slot node publishes one worker, and the ranking eta stops at no wall

### Fixed — a node whose agent seat serves one request at a time now publishes one worker, and doctor shows the mismatch

- **Ten tiers seed `fleet_max_concurrent_jobs: 1`.** A node publishes `fleet_max_concurrent_jobs` workers (default 4) and the
  delegator deals it as many jobs as that leaves free. Over an agent seat that serves `--parallel 1` those are one worker and
  three jobs waiting inside the seat, charged to their wall, while the delegator counted them as parallel. Seen 2026-10-08
  during a 12-page research call: one single-slot node published 4 and held 3 running, another held 4 running plus 1 queued.
  The scope is a rule, not a list: every tier whose agent seat, resolved through its aliases, is a single-slot llama.cpp entry
  of the rendered serving template (`--parallel 1`, `--parallel=1`, `-np 1` or `LLAMA_ARG_N_PARALLEL=1`, on every OS the tier
  renders for) seeds 1. That selects `amd-gcn`, `amd-rdna3-dgpu`, `ampere-6`, `ampere-8`, `blackwell-8`, `blackwell-32`,
  `blackwell-48`, `blackwell-72`, `dual-gpu` and `volta-16`. Out of scope, with the reason in the test: the four tiers that
  declare a vLLM agent seat (`ampere-16`, `blackwell-16`, `blackwell-2x16`, `blackwell-3x16`: that seat serves many requests at
  once, so a one-worker cap there is the opposite defect), and the tiers that seed no agent seat, which are three cases and not
  one. `rockchip-rk3588` serves its NPU seat (`qwen3.5-2b-npu`, one generation at a time), not an agent seat, and already seeds 1
  for it. `amd-rdna3` and `cpu` seed no agent seat at all: their agent lane falls back to the workhorse (`offload-e4b`, which
  also serves `--parallel 1`) and their cap stays at the default 4, a known gap that is a separate decision and not fixed here.
  `fleet_max_queue_depth` stays unseeded (one running, one waiting, the rk3588 precedent). `docs/tiers/*.md` gain one row each.
- **The key is also the delegator's own run-cap line, on purpose.** On such a box the local seat takes one run at a time and the
  rest of a call goes to the fleet, because a single-slot seat serves one request at a time. It governs the text-endpoint lanes
  (agent and text jobs), not media, stt or pipeline routes. `OPERATOR-GUIDE.md`, `FLEET-NODE.md` and `systems/fleet-node.md` no
  longer state a default of 4 workers as if universal.
- **A seed reaches fresh configs only.** A live node keeps its own value until `fleet_max_concurrent_jobs` is set in its
  `config.json` and `fleet-serve` is restarted; `local-offload audit-config` lists the key as `SEED-ONLY` or `DIFFERENT` until then.
- **`doctor` prints a `capacity:` row on a box with an agent seat.** It compares the published cap with the slots of the agent
  seat in the LIVE serving config (`serving_config_path`; `--parallel N`, `--parallel=N`, `-np N`, `LLAMA_ARG_N_PARALLEL`; macros
  expanded, alias-aware, a command-line flag beats the env twin): `OK` when the cap equals the slots, `OK` (saying so) when it is
  below them, `WARN` with the value to set when it exceeds them or is unlimited, `UNKNOWN` naming why when the slots cannot be read
  (the path is unset or unreadable, no entry answers to the seat, it is not a llama.cpp entry, or it states no slot flag). It never
  changes the exit code and is silent on a config with no agent seat. `servingtmpl.SeatParallel` and `SeatParallelWhy` are the reader.
- **The doctor row follows the seat the box serves.** A box of a vLLM tier (`ampere-16`, `blackwell-16`, `blackwell-2x16`,
  `blackwell-3x16`) whose vLLM venv is not installed serves the tier's llama.cpp fallback at `--parallel 1`, so the row correctly
  warns that a cap of 4 exceeds one slot; an operator who followed it and installed the venv later would have ended at a cap of 1
  over a `max_num_seqs` of 32 (4 on `ampere-16`). The WARN now says the cap must match the seat served now and be changed again whenever the seat changes
  (to `max_num_seqs` on a vLLM seat), and a box whose tier (the installer's `installed.json`, else the config's `tier_profile`)
  declares a vLLM seat it is not serving says so: the seat, its `max_num_seqs`, the fallback it serves, and the cap to raise to when
  the seat takes over. With no provable tier the row names none. It stays informational; the exit code never changes.
- **The row reads what a config states.** A stated `--parallel -1` or `auto` is read as the 4 slots llama-server serves for it
  (it printed `states no --parallel` although one was stated), and an entry that is not llama.cpp reads as its llama-swap
  `concurrencyLimit`, which a vLLM seat's installer sets to `max_num_seqs` (it printed that the slots were not in the file). A cap
  above a limit is described as llama-swap answering 429, not as a wait. The new reader is `servingtmpl.SeatSlotsWhy`;
  `SeatParallel` and `SeatParallelWhy` stay llama.cpp-only and now read `-1` and `auto` as 4 too.
- Tests: `TestTheSingleSlotLlamaCppTiersSeedAOneWorkerFleetNode` (derives the tiers; pins `fleet_max_queue_depth` unseeded and the
  vLLM-tier exemption), `TestEverySeededCapFitsTheSlotsOfTheSeatItsTierServes`, `TestAmdGcnSeedsTheSeatItWasMeasuredOn` (the cap),
  `TestSeatParallelReadsTheSlotsOfTheEntryThatAnswersToTheAlias`, `TestACapOfOneGivesTheLocalSeatOneRunAtATime`,
  `TestDoctorWarnsWhenTheFleetCapExceedsTheAgentSeatsSlots`, `TestDoctorCapacityRowIsOKWhenTheCapEqualsTheSeatsSlots`,
  `TestDoctorCapacityRowIsOKWhenTheCapIsBelowTheSeatsSlots`, `TestDoctorCapacityRowWarnsOnAnUnlimitedCap`,
  `TestDoctorCapacityRowSaysUnknownWhenTheServingConfigIsUnsetOrHoldsNoSuchSeat`, `TestDoctorCapacityRowNeverChangesTheExitCode`,
  `TestDoctorStaysSilentAboutCapacityOnADefaultConfig`, `TestTierDocsAreCurrent` (after `go generate`),
  `TestDoctorCapacityRowReadsAStatedAutoParallelAsFourSlots`, `TestDoctorCapacityRowReadsAVLLMSeatsConcurrencyLimit`,
  `TestDoctorCapacityRowNamesTheVLLMSeatOfATierThatServesItsFallback`, `TestDoctorCapacityRowOnTheVLLMSeatItselfNamesMaxNumSeqs`,
  `TestDoctorCapacityRowNamesNoVLLMTierItCannotProve`, `TestSeatSlotsWhyReadsAVLLMSeatsConcurrencyLimitAndNamesAnAutoCount`. Tested
  against fixtures; no live node ran it before this release.

### Fixed — the delegator never follows a redirect with the fleet bearer (ADR 0074 decision 9)

- **The health read and the dispatch, poll and withdraw client refuse a 3xx instead of following it.** Every request the
  delegator sends a roster node carries `fleet_auth_token`. ADR 0074 gave the token probe and the single-shot lane clients
  that rule and left `delegate.healthClient` following redirects; `delegate.fleetClient`, which sends the contract itself,
  followed them too. A node, or a proxy in front of one, could have had the client replay the request and its
  `Authorization` header at a `Location` it chose. Both clients now hand the 3xx back (`refuseRedirect`), and the callers
  read it as the failed health read or the refusal any other non-2xx status already is. Test:
  `TestTheDelegatorNeverReplaysTheFleetBearerAtARedirectTarget`.

### Fixed — test runs no longer write the operator's ledger

- **The delegate, mcpserver and root test suites isolate the offload home as well as the state root.** On 2026-10-03,
  1,204 `agent_run` rows written by `go test` runs of a development branch landed in the real ledger and read as fleet
  traffic: 160 "no fleet node serves seat" deferrals and 46 "the result came from" verification failures, all against
  fixture nodes, which a placement diagnosis then took for a wrong-seat routing defect. `config.Default()` fixes the
  ledger and corpus paths from `DefaultBase()` when it is called, so a test that sets `Home` afterwards still wrote the
  real files, and each `TestMain` guarded only `LOCAL_OFFLOAD_STATE_DIR`. Each now also points `LOCAL_OFFLOAD_HOME`
  inside its throwaway directory (fail-closed, like the state root) and sets `LOCAL_OFFLOAD_ORIGIN=go-test`, so a row that
  still escapes names its writer. Test: `TestMainIsolatesTheHomeAndTheLedger` in each package (red when the home is not
  set).

### Fixed — the ranking eta stops at no wall (ADR 0079)

- **Seats that cannot finish inside the wall are ordered by speed again.** `etaFor` priced the final fitted to the contract's wall
  and clamped the sum at the wall, so every such seat read the wall. Modelled with fixture rates (not live `seat_rate` readings),
  a schema contract with an 8,192-token configured final, loaded seats, an explicit 300 s wall: seats at 43 / 40 / 22.5 / 20 / 12
  tok/s all read 270 s and 6.57 / 3.9 tok/s read 300 s; they now read 48 / 52 / 92 / 103 / 171 and 312 / 526 s. The cap came with
  0.128.1 (`d34a149a`), when the wall was a kill; ADR 0055 decision 2 made it an expectation. Placement ordered those seats by
  queue estimates and the near-tie draw instead of speed.
- **The new eta is `etaParts`: cold + the node's own wait + a reference final at the seat's rate.** The reference is
  `seatrate.FinalBudgetFloor` tokens, and the same again for a schema's re-pack, plus the seat's own tool steps and think block;
  no `FitFinalBudget`, no clamp, and the contract's wall is not an input (the same seat and contract at any `timeout_sec`, or
  under `timeout_auto`, give the same parts). `otherSecExcludingCold`, whose only caller was the fit, is gone.
  `chosenVerdictDetail` prints `eta N s (cold C + G gen)` from the same parts, and the bare total it printed for a contract with
  no wall is gone. Unchanged: `feasibleFinal` still judges the wall, patience and the gates still read the node's wait, the node
  still fits the final to the wall when it runs the job, and an idle slow seat still beats a busy fast one. The eta stays a
  ranking number and must not become a patience or gate input.
- **ADR 0079 supersedes the cap clause of ADR 0050 decision 3.** ADR 0050 carries only a link to it, and ADR 0073 a pointer.
  `docs/systems/fleet-node.md` and `docs/systems/coding-agent.md` say the new definition. Every route that ranks by eta re-ranks
  at once (auto, remote, the capacity wait, the retry, spread); there is no flag, because the old definition was the defect.
  Placement among near-tied seats changes with it: the near-tie draw moves an eta by at most 10 % either way, so seats more than
  about 22 % apart can no longer swap in it, and seats that used to tie at the wall (and were spread by the draw) now go to the
  fastest unless it is busier or not idle (`provablyStartsNow` still ranks first, so an idle slow seat beats a busy fast one).
- **Adequacy is a ranking key, not a number inside the score.** `fitInadequate = math.MinInt32` ranked a seat that cannot hold the
  contract below every seat that can, on the premise that no score could reach it. A rated, adequate seat's mechanical score is
  `-int(eta x 10) x 2^24 - window`, which passes that value once its eta exceeds about 12.8 s, on every build (a probe: -8,724,160,512
  under the uncapped eta, -4,362,084,352 under the capped one), so such a seat ranked BELOW one that cannot hold the contract. It
  was latent, because `placeSpreadWith` filters through `remoteEligible` before `fitPickWith` scores; the ranking no longer depends
  on that. `scoreFit` and `scoreFitWith` return a `fitKey` (`adequate`, then `score`), `fitKey.beats` compares adequacy first, and
  `fitPickWith` ranks adequacy ahead of the lease, saturation and free-card keys. An inadequate seat is still never excluded, only
  ranked last. Placement over an already-eligible roster is unchanged. ADR 0079 said this was only a 32-bit overflow; it now says
  what it is.
- Tests: `TestAnInadequateSeatRanksLastBesideARatedSlowAdequateSeat` (a rated adequate seat with an eta above 13 s beside an
  inadequate one; `TestScoreFitRanksAnInadequateSeatLast` only ever passed because its fixtures publish no rate),
  `TestAdequacyOutranksEveryDemotionKeyOfTheSpreadPick`, `TestALayerDecisionThatDefersOrWaitsIsAnInadequateKey`,
  `TestEtaForRanksSeatsByRateWhenNoneCanFinishInsideTheWall`, `TestEtaForSeparatesTwoSeatsTheOldCapTiedAtTheWall`,
  `TestEtaForDoesNotDependOnTheContractsWall`, `TestEtaForHasNoOpinionWithoutARate`, `TestEtaForIsTheSumOfItsParts`,
  `TestChosenVerdictDetailReportsColdWaitAndGenerationFromTheSameParts`, `TestPlaceRanksTwoSeatsThatCannotFinishInsideTheWallByRate`,
  `TestEtaRankingIsAStrictWeakOrderingAcrossSeatsPastTheWall`. Rewritten to state the new rule: `TestEtaForNeverExceedsColdPlusWall`
  is now `TestEtaForIsNotCappedAtTheWall`, `TestEtaForDoesNotDoubleCountAFittedRepack` is now
  `TestEtaForChargesTheRepackOnceAtTheReferenceSize`, and `TestEtaForUnchangedWhenTheFinalIsNotFloored` (false under the new
  rule) is now `TestEtaForPricesTheReferenceFinalWhateverTheSeatsConfiguredFinalIs`.

## [0.171.0] - 2026-10-08 - a research page that said something is no longer failed for lacking a verdict

### Fixed — a research page with populated lists is no longer failed for lacking a verdict

- **The default research digest now fails only when it said nothing, and its goal asks for the verdict.** Since
  2026-09-30, 36 default-digest pages failed `nonempty:verdict` (the guard 0.147.0 gave the harness's own digest). None
  was an empty page: every one had populated lists (`key_facts` median 10.5 items) and `verdict: ""`. All 36 came from
  the vLLM json_schema re-pack lane. The research goal never asks for a verdict, and the re-pack prompt says "Use empty
  values when a field is absent", so the extractor writes `""` when the loop's final text has none. An acceptance-only
  research failure is not retried, so each one discarded a whole run: 4.07 h of seat wall time in total. The check
  exists only to catch the silent-empty digest (every list empty and no verdict), which occurred 0 times in 687 pages;
  on that data, failing only when every list AND the verdict are empty turns 35 failures into passes and turns no pass
  into a failure. `Build` now derives `nonempty:key_facts|numbers|quotes|verdict` for the default digest and ends its
  goal with one sentence asking for the verdict ("Always end with a one-sentence verdict that answers the goal from
  this page, or says plainly that the page does not address it."); a caller-supplied schema is unchanged (no sentence,
  the same `nonEmptyChecks`). The anchor words still come from the caller's goal alone, and the default schema already
  requires all four fields, so it comes through byte for byte.
- **The acceptance DSL gains the any-of form `nonempty:<f1>|<f2>|…`.** It passes when at least one named field is
  present and non-empty, under the same emptiness rules as the one-field check (`null`, `""`, `[]`, `{}` are empty; a
  present `0` or `false` is a value), and an absent field counts as empty for its alternative. A failure is one reason
  that names the check and says why each field is empty or absent; a missing or unparseable structured object still
  fails with the one-field check's reason, word for word. An empty alternative (`a||b`, `|a`, `a|`) and a repeated name
  (`a|a`) are parse errors. A single name is the one-field check unchanged, failure reasons included. `AcceptedFields`
  and `RequireFields` declare each name an any-of check lists (presence only), through a new
  `AcceptanceCheck.Fields()`; `min_items` keeps a pipe in its field name literally. Only the delegator evaluates
  acceptance (`delegate.EvalAcceptance`); a node only parses it when it ACKs a contract, and the parser accepted any
  non-empty argument after `nonempty:`, so a node one release behind admits the new spelling unchanged (verified at
  3568a530 by running that parser on the default digest's check: `ParseAcceptanceCheck`, `Validate` and
  `DecodeAgentContract` all accepted it).
- Tests: `TestNonemptyAlternationPassesWhenAtLeastOneNamedFieldHasAValue`, `TestParseAcceptanceCheck` (the any-of
  rows), `TestASingleNameNonemptyCheckKeepsItsFailureReasonsByteForByte` (written and run green against the released
  parser first), `TestNonemptyAlternationReadsEveryNamedFieldAndOnlyNonemptyAlternates`,
  `TestAContractCarryingTheAnyOfCheckPassesTheNodesACKValidation`, `TestAcceptedFieldsSplitsTheNonemptyAlternation`,
  `TestRequireAcceptanceFieldsDeclaresEachAlternativeAndIsANoOpWhenAllAreRequired` in `internal/core`;
  `TestADigestWithPopulatedListsAndNoVerdictPassesAcceptance` (the live failure shape, on an anchored and a thin page),
  `TestAThinPageWithEmptyListsAndAVerdictPasses`, `TestAllEmptyDefaultDigestFailsButAFaithfulEmptyOneStillPasses`,
  `TestDefaultDigestCarriesTheAnyOfGuardOnEveryPage` (was `TestDefaultDigestCarriesAVerdictCheckOnEveryPage`),
  `TestTheDefaultDigestGoalAsksForAVerdictAndACallersSchemaGoalDoesNot` and
  `TestTheDefaultDigestContractPassesTheNodesACKAndKeepsItsSchemaByteForByte` in `internal/research`. Fifteen
  mutations at their call sites (any-of as all-of, an absent field passing or dropped from the reason, an empty or
  repeated alternative accepted, a single name through the any-of path, `Fields()` not splitting or splitting
  `min_items`, an array never empty, the shared structured-object reasons dropped, `Build` deriving the old check, the
  goal sentence dropped, doubled or added to a caller's schema, the check drifting from the schema) each turned a test
  red and were restored byte for byte.

## [0.170.0] - 2026-10-08 - a placement pin needs a reason; without one it is a hint

### A placement pin needs a reason; without one it is a hint (fleet-first, track R4e; ADR 0078)

- **`route:"local"` and `route:"remote"` are a pin only with a `pin_reason`; without one they are a hint placement may override ([ADR
  0078](docs/architecture/decisions/0078-a-placement-pin-needs-a-reason-without-one-it-is-a-hint.md); the 2026-10-07 incident).** A
  session passed `route:"local"` to a 78-page `offload_research` call: 46 ledger rows read `route=local forced`, and 32 pages ended
  `not started: the call ended first`, because the whole fan-out queued on one seat while four fleet nodes had free workers. The
  operator's directive: a caller may not narrow placement with no reason recorded. `agent_delegate`, `offload_research` and the
  `delegate` and `research` verbs (`--pin-reason`) now take `pin_reason`, a closed set: `privacy` and `locality` with `local` only,
  `measurement` and `operator` with `local` or `remote`. A value outside the set, a reason with the wrong route, and a reason with a
  route that is not a pin (`auto`, `spread`, `queue`, research's default `spread`) are refused at intake with the valid set listed,
  before a `context_paths` file is read, a page is fetched or the config is loaded (`delegate.CheckPinReason`, checked again in
  `runWith`). A reasoned pin places as it always did, and its retry now obeys it too: a remote pin retries on another fleet node or not at
  all, never on the local seat. That part is new: a `route=remote` retry used to be able to land on the local seat, so a node's wrong
  answer under a reasoned remote pin now ends `failed_verification` where the seat used to recover it. A reasonless `local` is applied as `auto` (ADR 0076): the idle seat takes the
  first `room` subtasks of its run-cap line and the overflow goes through the unchanged `remoteEligible` gate to the remotes with room,
  else to the capacity wait, so a call that fits the line stays local and reads no node's health, a reserved, fenced, full, loading or
  occupied seat sends the work to the fleet, and a failed verification can be retried on another node. A reasonless `remote` is placed
  fleet-first (the deal treats the seat as unavailable by preference, as `route=remote` always did), and where `remote` would have
  deferred the idle local seat may run the subtask: at once when no remote is eligible, and, when every eligible remote is at its
  headroom, in the deal itself up to the seat's run-cap room (counted like a remote's headroom, so it does not wait for a dealt
  subtask to finish and does not depend on the capacity wait being on), never over a lease or an occupant; a dispatch the process gate
  turned away reads the fleet once in the capacity wait before the subtask takes the idle seat (with the wait switched off, or for a
  sheddable run, it takes the idle seat at once under the same guards instead of deferring or being shed). Only the four surfaces that
  offer `pin_reason` opt in (`RunOptions.PinNeedsReason`): `fleet-smoke`, the review lane's remote fallthrough, `offload_ask` and
  `agent_run` hand the engine a bare route and keep their authoritative pins. A call that carries a browse grant (`allow_browse`,
  which drives this machine's own browser) is pinned under the implied reason `locality`, so ADR 0060's rule that such a contract
  never reaches another node survives a bare `local` being a hint. Tests: the acceptance, a reasonless `local` fan-out with a free
  remote dispatching remotely with the rows saying so, and a reasoned pin behaving as before
  (`TestAReasonlessLocalFanOutWithAFreeRemoteDispatchesRemotelyAndTheRowSaysSo`, `TestAReasonedLocalPinBehavesAsItAlwaysDid`,
  `TestACallThroughADoorWithNoReasonChannelKeepsItsAuthoritativePin`), the small call that stays local
  (`TestASmallReasonlessLocalCallStaysLocalAndSaysSo`), a reserved seat and a busy seat
  (`TestAReasonlessLocalCallDoesNotRunOnASeatAnotherSessionReserved`, `TestAReasonlessLocalCallLeavesABusySeatForTheFleet`), the
  research-sized batch (`TestAHintedResearchSizedBatchIsOneDealAcrossTheFleet`), the retry
  (`TestAHintedLocalCallMayRetryOnAFleetNodeWhereAPinMayNot`), the remote hint at each place it can fall back
  (`TestAReasonlessRemoteRouteIsPlacedFleetFirst`, `...FallsBackToTheLocalSeatWhereRemoteWouldDefer`,
  `...GivesTheIdleSeatWhatNoRemoteHasRoomFor`, `...CountsTheIdleSeatsLineLikeARemotesHeadroom`,
  `...DoesNotGiveTheSeatAContractItCannotRun`, `...FallsBackFromTheCapacityWaitToo`, `...NeverRunsOnASeatAnotherSessionReserved`,
  `TestThePerSubtaskPlacementOfARemoteHintAlsoPrefersTheFleet`, `TestARemoteHintTurnedAwayByTheProcessGateReadsTheFleetBeforeItTakesTheIdleSeat`),
  the browse grant
  (`TestABrowseGrantPinsALocalRouteUnderTheImpliedReasonLocality`), and intake at every door
  (`TestCheckPinReason*`, `TestARefusedPinReasonSpendsNothing`, `TestAgentDelegateRefusesABadPinReasonBeforeReadingAnyContext`,
  `TestOffloadResearchRefusesABadPinReasonBeforeFetchingAnyPage`, `TestDelegateAndResearchVerbsRefuseABadPinReasonBeforeLoadingAnything`);
  each new branch mutated red at its real call site: 97 mutations across the closed set, the intake checks of all four doors, the hint and its remote-first forcing, the deal's fallback and each of its conditions, the stamp, the tally and the ledger columns, and the lane door's seat reading, every one red and every file restored byte-identical.
- **Every result and ledger row says what happened to a hint.** Each result's placement reason opens with a plain-words clause:
  `route=local was a hint (no pin_reason), honoured: placed on the local seat`, `... overridden: placed on <node>`, `route=remote was
  a hint (no pin_reason), ...`, or `... and no node took it`. The clause opens the note because the ledger keeps 120 bytes of a
  placement. A ledger row of a call through a door that offers `pin_reason` with route `local` or `remote` gains `route_asked` and
  `pin_reason` (empty for a hint), as does its dispatch marker; `route` stays the route the engine applied, so a hint's row says
  `auto`; every other row is byte-identical. A reasoned pin's reason is also on every result as `results[].pin_reason`. Tests:
  `TestTheHintClauseSaysHonouredOverriddenOrThatNoNodeTookIt`, `TestTheLedgerKeepsTheHintClauseInItsShortFormOfThePlacement`,
  `TestADispatchMarkerCarriesThePinReasonOfAReasonedPin`, `TestAHintedCallRefusedBeforePlacementKeepsItsReasonCode` (the reason
  code is read from the result as the engine made it), `TestAHintWhoseOnlyNodeRefusedItSaysNoNodeTookItAndCountsNoOverride` (a node that
  refused a subtask has not taken it, alone or as the end of a refusal chain), `TestAPinnedResultCarriesItsReasonOnTheWire`.
- **`offload_status` counts the pins.** The fleet block has a `pins` object: this server's counts since it started (`started_at`,
  and the block says so), in subtasks, `reasoned` by reason, `hints`, `hints_overridden` and `unreasoned` (pins through the callers
  above that run inside the server). It is one `delegate.PinTally` per MCP server that each door hands the engine, counted once per
  published result (a retried subtask is one), and nothing scans the ledger. The default `offload_status` golden moved by that block
  alone (856 bytes); the block is absent when delegation is off. Tests: `TestOffloadStatusPublishesThePinAccountingOfThisServer`,
  `TestOffloadStatusHasNoPinsBlockWhenDelegationIsOff`, `TestAPinTallyListsEveryReasonOfTheClosedSetAndCountsOnlyPublishedResults`,
  `TestTheFleetDispatchOfAskAndReviewStaysAPinAndIsCounted`.
- **The tools say it.** The `route` text of `agent_delegate` and `offload_research` no longer calls `local` "force in-process": it
  says a pin needs a `pin_reason`, what a hint is, and that each result says whether it was honoured or overridden; `pin_reason` is
  an `enum` of the closed set with each reason's meaning. The measurement scripts that must run on one seat
  (`scripts/parallel-sessions-gate.ps1`, `scripts/write-door-gate.ps1`) and the `contracts/` examples pass `--pin-reason
  measurement`. Tests: `TestBothDelegationToolsOfferPinReasonAsTheClosedSet`, `TestBothDelegationToolDescriptionsSayAPinNeedsAReason`.
- **The vision lane's `auto` route also reads the vision seat (ADR 0078 decision 9; amends ADR 0040 decision 6).** It sent a call to
  a fleet node only while the machine-wide GPU lease was held, so a seat serving other requests with no lease anywhere queued the
  next image behind them while a node with an idle vision seat sat free. It now also asks the delegator's own question of the seat the
  task would run on (`delegate.LocalSeatBusy`, through the one `probeSeatBusy` that the delegator reads its agent seat with): a
  request in flight, a load or unload in progress, or a load that would unload another loaded vLLM seat reads busy, an unreadable seat
  reads idle. The seat is the one the pipeline picks for the task (`ocr` has its own binding when the machine has one). The lease is
  read first and keeps its words (`remote: local gpu busy`); a busy seat places `remote: local vision seat busy (<what>)`. A box
  with no `delegate_remotes` has no node to choose and does not read the seat. The threshold is the seat's own, any request in flight, not the agent run cap, which counts registered agent loops and cannot see a
  single-shot vision call. STT keeps its own trigger (ADR 0072), compose chooses by capability, and the text lane is left alone: the
  cascade's entry rung is chosen per request inside the pipeline, so the door cannot name a seat to read. Tests:
  `TestAutoGoesRemoteWhenTheVisionSeatIsBusyWithNoLeaseAnywhere`, `TestTheProductionReadingSendsAnImageOffTheBoxWhileItsSeatServesAnother`,
  `TestAHeldLeaseKeepsItsWordsAndSkipsTheSeatRead`, `TestRouteLocalAndRemoteNeverReadTheSeat`, `TestVisionSeatForFollowsThePipelinesPick`,
  `TestAutoNeverReadsTheSeatOfABoxWithNoNodeToSendTheImageTo`, `TestLocalSeatBusyReadsTheNamedSeatsOwnLoad`,
  `TestTheLaneReadingIsTheReadingTheDelegatorMakesOfItsAgentSeat`; each new branch mutated red.
- **Review fixes before release (adversarial review of the four R4e commits).**
  - *A reasoned remote pin is authoritative on its retry too.* `alternativeNode` sent the retry of a first attempt that ran on a node (a
    failed verification, an abstention, an admission defer, a seat-down defer) to the local seat, so a call pinned to a node under
    `measurement` or `operator` could publish the local seat's answer with `route=remote` and the reason on its rows, and
    `offload_status` counted it as obeyed; with a second idle node the untried node was never asked either. The retry now goes to the
    best untried fleet node or is skipped, and the `retry_note` (and, when it ran, the retry's placement) name the pin. The guard is keyed on the
    reason (`runner.remotePinned`), not on the route: the bare remote route of `fleet-smoke`, the review lane, `offload_ask` and
    `agent_run`, and a reasonless remote hint, keep `remote -> local` (a route-keyed guard turns ten existing tests red). The tool
    text, the CLI help, the glossary and `fleet-node.md` now say where a pin runs, retry included. Tests: `pin_retry_test.go`.
  - *A remote hint the process gate turned away takes the idle seat where no wait can carry it.* With the capacity wait switched off
    (`agent_placement_wait_sec` negative) the subtask was deferred, and a sheddable run (priority -1) was shed, both while the idle
    seat had room. `hintSeatWithoutAWait` takes the seat at those two exits under the deal's own guards (the seat read free at the
    deal, serves the layer, and, read again, no lease reserves or fences it, it is not at its run cap, loading or occupied, and its
    run-cap line has a slot); a composite decision that asks the seat to wait leaves the exit's outcome alone. Tests: `pin_gate_test.go`.
  - *The remote hint's seat guard is pinned.* A full node with the wait on and a seat that another vLLM seat occupies, that has 4 in
    flight at a cap of 4, or that is loading, waits in line and ends as a capacity defer that says no node took it; an idle seat runs
    it. A refused dispatch falling back to the idle seat, which the docs list as tested, now is, with the remote pin that fails there
    as the contrast. Tests: `pin_fallback_test.go`.
  - *Three narrations.* A remote hint on a box with no agent seat (a delegation client) and no eligible remote ends as the fleet's
    defer a pin gets, not as a run on a seat that does not exist counted as an override. An abandoned hinted subtask (the call gave up
    while a seat was still running it) says where it was running is not known, not that no node took it. With no remote eligible at
    all, a full or loading seat takes the subtask at once, as route `auto` does, and only a lease, a fence or an occupant hold it
    back: the code was right and ADR 0078 decision 5 and `fleet-node.md` said otherwise, so the text is aligned to the code.
  - *Two surviving mutants killed.* `LocalSeatBusy`'s production wiring (the seat guard) and the stamped placement on the delegation-log
    corpus row each had no test that could fail.
  - *The opencode plugin's digest.* It counted a result as local when its placement contained "local", which every result of a local
    hint now does, so it told the model the pair did not land for a call whose subtasks all ran on nodes. It reads the hint clause
    first (`ranLocally`). Tests: `integrations/opencode/test/plugin.test.ts`.
  - *Texts made true.* The vision tools' `route` schema, the vision verbs' help and `docs/FLEET-NODE.md` name the busy-seat trigger of
    the `auto` route; `docs/FLEET-NODE.md`, `opencode-integration.md` and `gpu-lease.md` say a bare `route=local` or `route=remote` is
    a hint through the doors that offer `pin_reason`; `unreasoned` is documented as what it counts (the review lane's fallthrough and
    the remote route of `offload_ask` and `agent_run`; their explicit local route never reaches the engine and is not counted, and the
    status golden moved by that note alone). ADR 0078's Alternatives considered records why the closed set has no reason for "the
    local seat is the better model": placement chooses among adequate seats and does not rank them by a quality nothing measures.
  - Every new or changed branch was mutated at its call site: 63 distinct mutations (twelve for the pin guard, 24 for the gate fallback, 15
    for the guards and narrations, seven for the plugin, five for the tool and help texts), each red and each file restored byte-identical; one of them (the deal
    fallback's guard never true) is not reached by the new tests and is killed by three existing ones.
- Docs: ADR 0078; amendment notes in ADRs 0040, 0060, 0063 and 0076 where their text describes the old route or trigger;
  `fleet-node.md` ("A placement pin needs a reason"); `OPERATOR-GUIDE.md`; `contracts/README.md`.

## [0.169.0] - 2026-10-07 - a call is as wide as its deal, and a vision verb runs under its own lease

### A call is as wide as its deal, and a research call is one deal (fleet-first, track R3; ADR 0076)

- **A call is as wide as its deal, and a research call is one deal ([ADR
  0076](docs/architecture/decisions/0076-a-calls-width-is-sized-from-its-deal-a-batch-of-up-to-16-is-one-deal-and-auto-counts-the-local-run-cap-line.md);
  the diagnosis' F01).** One call never had more than four jobs in flight: the semaphore in `RunWith` was the constant
  `runConcurrency = 4`, so a joint deal that committed two subtasks to each of four nodes of four slots ran one at a
  time on each, and `RunBatched` cut a 12-page `offload_research` call into chunks of eight that ran strictly one after
  the other, so the second chunk waited for the slowest page of the first and the call used at most four of the fleet's
  sixteen slots. The semaphore is now `dealParallelism()`, the sum over the deal's dealt slots of what each place takes
  at once, never below four: a remote that publishes `max_concurrent_jobs` counts every subtask it was dealt (the deal
  already holds that to its headroom, and the process gate to its admission ceiling), a remote that publishes none
  counts at most four per node (an unpublished ceiling is unknown, never a limit, so two such nodes give eight and a
  call whose only remote publishes none stays at four on it), the local seat counts `min(dealt, run-cap room)`, and a
  subtask the deal gave no place (the capacity-wait overflow, a slot a lease reserves) counts nothing, so ADR 0073's
  rules hold at any width: a wait that holds a slot keeps its TTL while a subtask has not started, only the last waits
  to the horizon, and nothing starts inside the reserve. `route=local` has no deal and keeps four (`route=queue` hands
  its subtasks to the pull holders before any semaphore exists); the width is logged (`fan-out width N for M
  subtask(s)`) when it is not four. `RunBatched` deals up to 16 subtasks as ONE joint deal through an internal `runWith`
  that takes the subtask bound, so a research call's 12 pages are never chunked, a longer list is consecutive deals of
  16 and `batches` counts the deals; `agent_delegate` and the `delegate` CLI verb call `RunWith` and keep their bound of
  eight, and the tool's input schema (`maxItems` 8) is unchanged. Tests: 8 subtasks over 4 nodes of 4 slots now peak at
  8 open jobs and 12 pages over 3 nodes of 4 at 12 in one batch, where the constant gave 4 and 4
  (`TestACallIsAsWideAsItsDeal`, `TestRunBatchedDealsTwelvePagesAsOneBatchAcrossTheFleet`,
  `TestRunBatchedSpreadsAResearchCallOverTheLocalSeatAndTheFleet`; the nodes hold their jobs until the peak is reached,
  so the peak is the engine's width and not the runner's speed), the width table
  (`TestDealParallelismSumsWhatTheDealCommittedToEachPlace`), the local seat's room
  (`TestTheLocalSeatCountsOnlyItsRunCapRoomInTheWidth`), R1's rules at a width of six and eight
  (`TestAWaitHoldingARunSlotOfAWideCallKeepsItsTTLWhileASubtaskIsUnstarted`,
  `TestASubtaskBehindAWideCallIsNotStartedInsideTheReserve`, `TestAWideCallUnwindsAtTheDeadline`), and the
  chunk-boundary tests moved from 8 to 16; each new branch mutated red.
- **`route=auto` counts the idle local seat's run-cap line (ADR 0076; the diagnosis' F02).** The joint deal dealt every
  subtask to an idle local seat without counting what it had committed to it: 8 subtasks, a run cap of 4 and one remote
  with 4 free slots made 8 local and 0 remote (the spread deal on the same inputs made 4 and 4), and the overflow stood
  in the seat's own FIFO for up to the run's wall while an eligible remote idled; concurrent callers each read the seat
  idle at their own start. The deal now reads `localRunCapRoom` once per call and counts what it gives the idle seat
  against it, as the spread deal counts the local seat (ADR 0063 decision 6): the seat still wins the first `room`
  subtasks (ADR 0050 decision 1), and the rest are dealt through the unchanged `remoteEligible` gate, the backlog gate
  and the headroom count to the remotes with room, to the capacity wait when none has, and to the seat only while no
  remote could run the contract at all. A subtask moved off the seat says so in its placement reason (`the idle local
  seat's run-cap line is spent by this deal (4 of 4 free slot(s) dealt; ...)`), and a layer-naming subtask the seat
  could never take is not blamed on the line. An idle seat's call reads the fleet's health when it has more subtasks
  than the seat's line takes; one that fits it reads none, as before. Tests:
  `TestAutoDealCountsTheIdleLocalSeatAgainstItsRunCap` (the diagnosis' S2: 4 local, 4 remote) with its control,
  no-eligible-remote, registered-runs, overflow and layer variants, and end to end
  `TestAutoRunSendsTheOverflowOfAnIdleSeatToTheFleet` and
  `TestAutoRunDoesNotReadTheFleetWhileTheIdleSeatsLineTakesEverySubtask`; each new branch mutated red.
- Docs: ADR 0076; amendment notes in ADRs 0032, 0050, 0063 and 0073 where their text describes the old width or the idle
  seat; `fleet-node.md` ("How wide a call is, and how a list of pages is dealt"); `OPERATOR-GUIDE.md`; the
  `agent_delegate` tool text no longer says an idle local box always runs the work.
- **Review fixes before release.** **(1)** A node that publishes neither ceiling is held to four open dispatches by the
  process gate (`admissionCeiling`). The width counted it as four, but the semaphore is one pool, so once the call's
  other legs finished it could hold every subtask dealt to it: 8 of a 12-page call in
  `TestANodeThatPublishesNoCeilingIsHeldToFourWhenTheCallsOtherLegsFinish`. **(2)** A local seat with no run cap
  (`fleet_max_concurrent_jobs` < 0) counts at most four toward the width, instead of everything dealt to it. Each fix
  was mutated red. Stated in ADR 0076's consequences: an `auto` call on an idle seat with more subtasks than the seat's
  room reads the fleet before it starts; sheddable overflow past the seat's line is shed when no remote has an idle slot
  (it used to queue in the seat's FIFO); a node struck during a call keeps its share of pages 9 to 12 (they were a
  second deal).

### A vision verb run under its own GPU lease does not wait for that lease

- **A vision verb run under its own GPU lease no longer waits for that lease.** `gpu reserve ... -- local-offload
  vqa|ocr|assess-image` is the command the lease was taken for, and the vision gate waited on it anyway: it read the
  held lease as a generation job, polled `vision_gpu_wait_sec` and deferred `gpu_busy`, so a measured vision run (the
  2026-09-12 vision bake) could not queue like every other GPU job and had to stop the fleet node instead. The gate now
  drops the lease whose epoch is this process's `GPU_LEASE_EPOCH` before it decides, the rule `gpu-lease.md` already
  stated for it and the one the render runners (`ambientLeaseEnv`) and the delegator (`ForeignFence`) apply. The
  comparison moved into one helper, `gpulease.Inherited`, that the delegator now calls too. Per lease and by epoch: a
  child of another lease, or a stale variable from a lease since handed on, still waits. Tests:
  `TestVisionUnderItsOwnLeaseDoesNotWaitForIt` (mutated red: without the fix it waits the full window and defers),
  `TestVisionUnderAnotherLeasesEpochStillWaits`, `TestInheritedComparesTheEpoch`.
- Test: `TestVisionGPULockReleasedMidWaitProceeds` now releases its lock the way production's `removeClaim` does,
  retrying a Windows sharing violation while the gate's poll holds `meta.json` open. Under load it failed 4 times in 150
  (`-cpu 1`); it now passes 300 of 300.

## [0.168.0] - 2026-10-07 - the display layer opens under the presence guard and leaves when the operator returns

### The display layer opens under its presence guard and leaves when the operator returns (ADR 0075)

- **The card allocator keeps the desktop floor on a display card it opens.** `operator_presence` `auto` or `away` already
  opened the display card to `gpu reserve --cards N` and to an auto-placed media call, with no floor: with the pair seat
  loaded on the other two cards the display card sorted first as the card with nothing to evict, and a 10 GiB job could
  take it to about 2 GiB free. The allocator now holds an opened display card to the display layer's own arithmetic,
  `free − footprint ≥ display_floor_gib` (`config.DisplayFloorGiB()`: the largest floor of a layer guarded by
  `display_floor`, 0 when none is declared, which leaves a box without one exactly as it was). A job that declares no
  footprint (`--vram`) cannot be shown to leave the floor and is not given the card, as the layer's guard refuses a seat
  with no display footprint; `--devices` still names the card on the operator's word. The skip reads `vram` and names the
  floor, and a display card that could not clear the floor is not queued on while a lease holds it.
- **A loaded display twin is a resident seat of its card.** `gpualloc.ResidentFrom` counted a seat only by its `model`,
  and the display layer's router names its twins only in `model_map`, so a loaded twin left the display card reading as an
  empty one. Twins now count, each loaded twin costed at `footprint_gib`, else `display_footprint_gib`, so taking the card is
  priced as the eviction it is.
- Docs: `gpu-lease.md` ("The display card, once the operator is away"), `media-generation.md`. Tests: the allocator floor
  table (`TestAnOpenedDisplayCardIsHeldToTheDesktopFloor` and its siblings in `internal/gpulease`), `ResidentFrom` over a
  twin, `BuildInput` carrying the floor and the end-to-end picker (`internal/gpualloc`), `TestDisplayFloorGiB…`
  (`internal/config`); each new branch mutated red.
- **The `display_floor` guard checks the card the config names against the card the driver says drives the monitor.** The
  floor guarded `layers[display].display_device` and never asked the driver which card the screen is on, so a board that
  re-enumerated after a power loss, or a cable that moved, left the floor protecting a card that is not the desktop's. The
  guard now refuses, before any arithmetic, naming both cards (`display_device 1 is CUDA index 1, but the driver reports
  the monitor on index 0`) when the driver singles out cards that drive a monitor and the declared one is not among them.
  It contradicts only on positive evidence: a reading that cannot say (no card flagged, a transient failure that left
  `display_attached` unknown, no reader) trusts the declaration, and a second monitor on another card does not contradict a
  declared card that also drives one. Index and UUID pins are both resolved first, and the node publishes the same verdict
  in its `layers` rows (`gpuprobe.ScreenCardIndexes`, `placement.Live.ScreenCards`).
- **`CheckComposite` reads UUID pins.** The checked union read only digits after `CUDA_VISIBLE_DEVICES=`, so a render that
  pins a seat by GPU UUID (this box's rule, and what the twin template tells an operator to substitute) read as "pins no
  CUDA_VISIBLE_DEVICES" and was refused. A UUID pin is now a pin: two UUID lists are compared as the same cards (a declared
  prefix stands for the id that extends it), a UUID list must name as many cards as the declaration, and a UUID against an
  index declaration is compared through this machine's card table or refused (see the review fixes below).
- Docs: `composite-tier.md` (the guard, "The rendered config is checked"). Tests: `TestDisplayFloorCrossChecks…`,
  `TestLiveFromReadingsCarriesTheScreenCards…`, `TestANodePublishesAMismatchedDisplayCardAsInadmissible`,
  `TestSnapshotServesTheScreenCardsFromItsMemoisedProbe` (`internal/placement`), `TestScreenCardIndexes`
  (`internal/gpuprobe`), `TestCheckCompositeReadsUUIDPins` (`internal/servingtmpl`); each new branch mutated red.
- **A loaded display twin is unloaded when the operator returns or the desktop's memory goes.** The layer's guards decide
  once, at the placement; a twin then sat on the desktop's card until llama-swap's 300 s idle ttl however soon the operator
  was back, and a twin another llama-swap client loaded was never asked at all. `fleet-serve` now runs a watcher
  (`internal/displaywatch`, `fleet_displaywatch.go`): every `display_watch_sec` (0 = 10 s, negative = off, above 300
  refused at load) it reads llama-swap's `/running`, and only while a model of the display layer (its seats' own models
  and router `model_map` twins) is loaded does it ask the layer's own desktop guards again through
  `placement.ResidentVerdict`: presence reads present or unknown, or free VRAM on the display card is below the floor. It
  uses the same readers, card resolution and fail-closed reading as admission, but tests `free ≥ floor` and does not
  subtract the footprint again (a resident seat is already out of the free number), and leaves `host_ram` alone. On a
  refusal it unloads the layer's loaded models through llama-swap's per-model route (never the total one, no drain: the
  desktop needs the memory now), logs each one with the reason, and retries a failed unload at the next check. It
  unloads nothing else, and an unreadable `/running` unloads nothing. It checks once at start, so a twin loaded before
  fleet-serve came up is not exempt.
- **`offload_status` shows the guard.** `local.display_guard` (omitted on a box with no display layer, and on a dormant
  layer that never acted) carries `watching` (a heartbeat the watcher wrote recently, so a box whose fleet-serve is not
  running reads `false` with the reason), the check period and `last_action` (the models, the reasons, what came down and
  any error). `local.operator_presence` publishes the presence reading and mode on a composite box, with the caution
  that `away` is the operator's unconditional override and `auto` the supported mode. A plain box's payload is unchanged.
- Tests: `internal/displaywatch` (presence flips, the floor falls, fail-closed inputs, a failed unload
  retried, a stopping twin ignored, only the display layer's models, the per-model route and never the total one, the
  state file and the status view), `TestResidentVerdict…` and `TestPresenceAllowsIsTheGuardsRule` (`internal/placement`),
  `TestStartDisplayWatch…` (root), `TestDisplayWatch…` (`internal/config`), `TestStatusPublishes…` (`internal/mcpserver`);
  each new branch mutated red.
- **Docs: the real enable sequence for the display layer, and the `away` caution.** `composite-tier.md` gains "Opening the
  display layer" (back up; seed `tier_profile`/`tiers`/`layers` into the live config; UUID-pinned display twins in the
  llama-swap yaml; restart llama-swap with the vLLM seat stopped cleanly; `operator_presence` `auto`; `dormant` false;
  read it back), and "The guards keep holding after the load". `operator_presence` `away` is documented as an
  unconditional override that admits at the desk, and `auto` as the supported mode. A rendered twin is loadable by any
  llama-swap client whatever `dormant` and `operator_presence` say; the unload-on-return check also takes down a twin a
  non-harness client loaded. The G1b recipe in the composite-tier plan is refreshed: the presence step, the
  `summarize <file>` syntax, baselines, the assertions (`placed.seat` is the twin, `/v1/models` lists it, the floor holds
  in every sample, the pair is not evicted), the unload-on-return check, and the precondition that the three-card seat is
  not loaded.
- **ADR 0075, "The display layer opens under the presence guard and leaves when the operator returns".** Records the
  operator's 2026-10-07 "yes" to opening the display-card seat under its presence guard (it takes work only while the
  console is locked or idle and nothing is fullscreen, with a 4 GiB desktop floor), that this amends the earlier operator
  rule keeping single-card seats off the display card for the display layer only, and the terms: the gaps closed before it
  opens (allocator floor and twin residents, the screen cross-check, UUID pins), the post-admission check (per-model unload,
  no drain, fail loud, nothing else touched), `auto` as the supported presence mode with `away` documented as an override,
  and the rendered-twin-is-loadable-by-any-client caveat. Indexed in the ADR README; `composite-tier.md` links it.
- **Review fixes: the desktop rule holds when the card is unknown, at the grant, and when nothing is watching.**
  - **A display-unknown card is held to the floor.** After a transient display query failure every card is `DisplayUnknown`
    and none is `Display`, so the floor (`c.Display && floor > 0`) was skipped on exactly the reading where no card can be
    vouched for as not the monitor. The floor now applies to a card that is `Display` or `DisplayUnknown`.
  - **A queued request is asked again at the grant.** A `--cards` request or an auto-placed media call that queued on the
    display card while the operator was away was decided at the enqueue and never again; FIFO can take hours. The grant now
    vets the cards it just handed over (`Options.GrantCheck`, `Manager.VetGrant`, `gpulease.DesktopRefusals`, built by
    `gpualloc.GrantCheck`): a card that no longer qualifies (the operator is back, the floor is gone) is released and the
    request chooses again from fresh readings, taking another card it still fits or waiting for what qualifies, with its
    arrival time kept. The check runs after the claim because a check before it can be skipped by a lease that is
    mid-release. Wired into the reserve verb (including its immediate-grant path) and the media admission queue. A lease
    already RUNNING on the display card is not revoked when the operator returns (the watcher unloads llama-swap twins and
    nothing else); `gpu-lease.md` and ADR 0075 say so.
  - **A blind watcher is not watching.** An unreadable `/running` used to leave `watching: true` on a fresh heartbeat. The
    state file now records `read_err` and `blind_since` (written at once on the first failed read and on recovery) and
    `local.display_guard` shows `watching: false` with a note while it is blind. A state path that cannot be resolved is
    no longer dropped: it is logged at start and carried in the status note.
  - **The display layer does not open while nothing is watching it.** Its `presence` guard now also refuses when the
    watcher's heartbeat is absent or stale, the watcher is blind, or `display_watch_sec` is negative
    (`internal/displaystate.Alive`, read through `placement.Live.WatcherAlive`; a new leaf package so `placement` needs no
    import of `displaywatch`). The post-admission check itself never asks. A node publishes the same verdict in its health
    rows. Closing the card (`operator_presence` back to `present`) reaches the watcher only on a `fleet-serve` restart; the
    docs say so. The watcher covers only the layer named `display`; the docs say that too.
  - **A UUID pin the render cannot compare is refused, not passed.** `comparePins` let a UUID-rendered pin against an
    index-declared one through. The gate now resolves the UUIDs through this machine's card table (`nvidia-smi`, read once
    and only when a comparison needs it) and compares index sets; without a table, or with a UUID the table does not list, it
    refuses and says to declare UUID pins in `layers` or render on the box. The `display_floor` guard resolves a seat's UUID
    `device` through the probe too (one it cannot resolve counts as on the display card), so that advice does not turn the
    floor's arithmetic into `free - 0`.
  - **An unreadable `/running` reads the display card as occupied; every loaded twin is costed.** `ResidentSeats` returned
    nothing when llama-swap could not be read, so the display card sorted first as the card with nothing to evict. The
    display layer's models are now counted as loaded then (other layers contribute nothing, as before), and `ResidentFrom`
    costs each loaded twin at its footprint instead of once per seat.
  - **`display_watch_sec` that overflows a `Duration` is refused.** `seconds * time.Second` wraps negative above about 9.2e9,
    which read as off and passed validation. The value is compared as an integer first, and `DisplayWatchInterval` clamps an
    unvalidated one to 300 s.
  - Tests: `TestAnUnknownDisplayCardIsHeldToTheDesktopFloor`, `TestDesktopRefusals…`, `TestAcquireRefusesTheGrant…`,
    `TestVetGrant…` (`internal/gpulease`); `TestAQueuedLease…`, `TestARefusedGrantKeepsTheArrivalTime…`,
    `TestAnImmediateGrantIsVettedToo`, `TestRenderGate…UUIDPin…` (root); `TestAQueuedMediaCall…` (`internal/pipeline`);
    `TestABlindWatcher…`, `TestTheWatcherSeesAgain…`, `TestAStatePathThatCannotBeResolved…` (`internal/displaywatch`);
    `internal/displaystate`; `TestTheDisplayLayerDoesNotOpenWhileTheWatcherIsNotAlive` and siblings, `TestAUUIDPinnedSeat…`
    (`internal/placement`); `TestTheDisplayRowCarriesThisNodesWatcherLiveness` (`internal/fleetnode`);
    `TestCheckCompositeReadsUUIDPins` (`internal/servingtmpl`); `TestResidentSeatsTreatsTheDisplayCard…` (`internal/gpualloc`);
    `TestValidateLayersRefusesADisplayWatchThatOverflowsADuration` (`internal/config`); each new branch mutated red.

## [0.167.0] - 2026-10-07 - truth and guards outside the delegator: tailnet zones, redacted roster URLs, faster probes, doctor rows, OCR off Q4 seats

### Truth and guards outside the delegator (fleet-first, track B; ADR 0074)

- **The tailnet guard admits a configured list of zones.** A node shared in from another tailnet is named by Tailscale
  under the SHARER's zone, and `tailnet_suffix` held one zone, so the only form that passed every guard for such a node
  was its raw `100.x` address. The new key `tailnet_suffixes` lists further zones beside `tailnet_suffix` (which keeps its
  meaning and keeps working alone); load installs the union, the own zone first, before any endpoint is vetted, and a
  malformed entry refuses the load naming `tailnet_suffixes[i]` and changes nothing. Every consumer of the zone judges the
  whole list: `netguard.TailnetURL` (a new `TailnetURLIn` takes the list as an argument), the research lane's by-name
  refusal (a host under ANY listed zone is the tailnet, not web), and the cache-store host check. The default is
  unchanged and fail-closed, the dial gate is untouched (a name that resolves outside loopback and the tailnet CGNAT range is
  still refused at dial time), and a generic `.ts.net` rule is still rejected. `doctor` prints one `tailnet zones:` row
  naming each zone and its key (nothing when none is configured).
  Docs: ADR 0074 (new; amends 0023, whose decision text carries a pointer), `OPERATOR-GUIDE.md`, `mcp-server.md`,
  `config.example.json`. Tests: `tailnet_zones_test.go` (netguard), `tailnetzones_test.go` (config: every guarded key,
  the install, malformed entries, the cache-store host), `fetch_tailnet_test.go` (research), `doctor_tailnetzones_test.go`;
  each new branch mutated red.
- **Every single-shot lane admits a roster entry the way the agent lane does.** The vision, text, stt-upload, compose
  and accelerator lanes dialled `delegate_remotes` through the dial gate alone, so one entry was refused by the agent
  lane's shape check and used by five others, and a bad entry got no message naming it. They now share
  `internal/rosterprobe`: every entry is judged by `netguard.TailnetURL` before any dial, and a refused one is a named
  miss in the lane's defer reason (`<base>: not dialled, refused by the tailnet guard (<why>)`), never a failed call;
  the entries after it still serve. Blank slots are left out, and configured order and slot numbers are kept (compose
  breaks queue ties by slot; the accelerator lane takes the first listing node). The agent lane still fails a whole
  call on a refused entry: that is `internal/delegate`'s, not changed here (reported for the delegator track).
  Docs: ADR 0074 decision 4, `FLEET-NODE.md` (vision placement), `fleet-node.md`, `accelerators.md`. Tests:
  `rosterprobe_test.go` and one `roster_test.go` per lane; the accelerator dial-gate proof now goes through the lane's
  own request helper (the shape check refuses its old input first). Each guard, five lane sites and `Members`,
  mutated red.
- **The single-shot lanes probe the roster at once, through one shared cache.** Each lane probed `delegate_remotes`
  serially (k dead members cost k x 5 s before any node was chosen, k x 2 s per accelerator call), per call, sharing
  nothing with the other lanes (membership review F06). `rosterprobe.Probe` reads every admitted entry's health
  concurrently (the lane's own bound: 5 s, 2 s for the accelerator lane), returns the readings in configured order, memoises a
  good answer for 2 s and negative-caches a transport failure for 30 s, process-wide: the delegator's own windows, and
  one lane's discovery of a dead node saves every other lane the wait. A node that answered with a status is never
  cached as down, nothing is cached once the caller's context is done, and a timeout is not held against a caller willing
  to wait longer (a 2 s accelerator failure never turns a 5 s vision call away). A call over a roster with three hung
  members places in about one bound instead of three, and a second call dials none of them. `healthTimeout` is a var in
  each lane for tests. Timings are tabled in `fleet-node.md`.
- **The cascade lane's dead base costs one probe, and only for itself.** `cascade_remote_lanes` read a base's health, and
  when that got no answer asked its roster too (two bounds under one mutex shared by every lane base: membership review
  F08). A base that does not answer the health GET at all (no dial, no route, a timeout, a reset, a body that stalls) is
  no longer asked for a roster, and the probe runs outside the cache lock with one probe in flight per base, so a call
  that needs one base never waits on another. A base that ANSWERED but is not a node (a llama-swap 404s the route) is
  read through its roster as before. `laneProbeTimeout` is a var for tests; the stale "bounded by laneProbeTimeout"
  comments are corrected.
  Docs: ADR 0074 decisions 5 and 6, `fleet-node.md` (the timings table), `OPERATOR-GUIDE.md` (the lane section). Tests:
  `probe_test.go` (rosterprobe: concurrency, order, both caches and their guards), `roster_test.go` per lane (a black-holed
  roster placed once, in parallel, dial counts), `lanes_probe_test.go` (llamaclient: one probe not two, no head-of-line
  blocking, single flight, a dropped or stalled health reply); every new branch mutated red. The race detector is not
  available on the machine this was built on (no C toolchain), so the concurrency tests ran without `-race`.
- **`doctor` shows a wrong or missing fleet token, and names a refused roster entry by slot.** `/fleet/health` ignores
  the bearer, so a `fleet_auth_token` that was missing or wrong read healthy on every doctor row and first showed as a
  401 on dispatch, which the delegator does not re-place (membership review F11). `doctor` now reads the roster once,
  concurrently, through `rosterprobe` (it was serial at 5 s a remote), then asks each reachable node the one question
  health cannot: `rosterprobe.CheckToken` sends an agent dispatch with no `job_id`, which the node answers after its
  bearer check and before it creates anything (401 other token, 403 no token beyond loopback, 400 `job_id required`
  accepted; no job is queued). Rows `OK` / `MISMATCH` / `NO-TOKEN` / `UNKNOWN` / `UNCHECKED` name the entry by its
  `delegate_remotes[i]` slot, and a box with no token prints one `NOT SET` line. The token and a node's reply text are
  never printed, and the bearer is sent only to a base the tailnet guard admits. The fleet-versions rows name entries by
  slot too and show a guard-refused one as `REFUSED ... not dialled` instead of `UNREACHABLE`. At config load, a
  `delegate_remotes` entry the tailnet guard refuses is now a finding (`delegate_remotes[i] is refused by the tailnet
  guard`, judged against the zones the config itself names), so it is learned at load and not at the first call; it
  warns and does not refuse, like every finding there. Every message about a roster entry prints it redacted
  (userinfo, query and fragment removed), and a dial error's quotation of the raw base is scrubbed.
  Docs: ADR 0074 decisions 7 and 8, `OPERATOR-GUIDE.md` (troubleshooting rows). Tests: `doctor_fleettoken_test.go` (rows
  by slot, nothing asked of a refused or unreachable entry, no token printed, the real node handler end to end),
  `rosterprobe/token_test.go` (the three answers through the real handler, nothing queued, bearer only in the header,
  never sent to a refused base), `endpointshape_tailnet_test.go`; each new branch mutated red.
- **No message about a roster entry carries a credential it was pasted with, and the bearer never follows a redirect.**
  A roster entry is a base URL, and nothing stops one being pasted with `?token=` or `user:password@`. The tailnet
  guard's own refusal quoted it (a `url.Error` quotes the whole input), `Member.Miss`, `Reading.Miss`, the lanes'
  "probed ..." lines and dispatch errors printed the raw base, and the negative cache replayed a dial error that quotes
  the URL. `netguard.RedactBase` (moved below `internal/config`, which delegates to it) is the one redaction, now also
  covering the scheme-less `user:pw@host` form; `TailnetURLIn` quotes only that and words a parse failure without the
  input, `validateEndpointValue` and the loader inherit it, `rosterprobe.Scrub` (the doctor's scrub moved here) also masks
  every other URL left in a message, the cache scrubs a dial error before it is cached, and the doctor, the five lanes
  and the lane/loopback config findings use them. Separately, the token probe and the five single-shot lane clients
  followed redirects, so a node answering 3xx could have the client replay the request and its `Authorization` header at
  a `Location` it chose; they now return the 3xx as the answer (`rosterprobe.NoRedirect`); the health read in
  `internal/delegate` has the same gap and is that package's to change. ADR 0074 now says plainly that
  the one `fleet_auth_token` is sent to every admitted roster node, including nodes under a sharer's tailnet zone, whose
  administrator therefore receives a token that opens every node on the roster: list only nodes you trust with it (a
  per-remote token is future work).
  Docs: ADR 0074 (decisions 5, 7 and 9, consequences), `fleet-node.md`. Tests: `redact_test.go` and
  `tailnet_zones_test.go` (netguard), `endpointshape_tailnet_test.go` (config), `scrub_test.go` and
  `token_redirect_test.go` (rosterprobe), `doctor_fleettoken_test.go`, `redirect_test.go` per lane; each new branch
  mutated red.
- **A tailnet zone must be a zone.** `ParseZone` took any dotted string without a space, slash, colon or `@`, so
  `*.x.ts.net`, `x.ts.net?`, a trailing backslash and the bare generic `ts.net` all loaded as "a zone" that matched
  nothing, or (the bare domain) every tailnet's hostnames. A zone is now dot-separated `[a-z0-9-]` labels, and the bare
  `ts.net` is refused by name; the load refuses the entry naming `tailnet_suffix[es][i]`. `EndpointWarnings` now says
  what `TailnetZones` does on a malformed list (no zones, all-or-nothing) and prints the zone error as the first finding.
- **The negative cache holds a busy or restarting node out for seconds, not half a minute, and a cold burst costs one
  probe.** A node that timed out or refused the dial sat out the whole 30 s window, so a box that rebooted stayed
  invisible to the single-shot lanes long after it was back. A timeout, a refused or an unroutable dial (classified by
  error type, since Windows reports ECONNREFUSED as a different errno) is now held at most 5 s; a name that does not
  resolve keeps 30 s. `Cache.Forget(base)` drops the verdict, and the five lanes call it when a dispatch is accepted.
  Concurrent cold probes of one base under one bound share one request (a waiter's own context still ends its wait, a
  prober that leaves does not hand its cancellation to a waiter, and callers with different bounds do not share).
  `MemoTTL` and `NegativeTTL` are constants now: the windows and the clock are per-cache, so the timing tests run on a
  fake clock and on gates, assert counts, and no longer mutate a package variable; the lane roster tests got the same
  slack for a loaded CI runner.

- **A Q4 seat its tier page calls inadequate for OCR no longer answers remote OCR.** The fleet vision lane advertises
  `vqa`, `ocr` and `assess_image` on any node whose seed binds a vision seat without `tasks`, so the `ampere-8` node
  (Qwen3-VL-4B Q4_K_M) was handed remote `ocr` although its own page records the omitted `ocr` alias as a quality
  statement and Q4 as the measured OCR-fidelity cliff (fleet-first review R3-F14). Its vision seat now declares `tasks`
  `vqa` and `assess_image`, which the seed writes as `vision_tasks`: the node refuses `ocr` at ack time naming the
  allowed set, publishes the list in health, and a delegator places remote `ocr` on another node or defers. A local
  `ocr` call is unaffected (routing uses `vision_model`). The declaration is dropped when an ocrprobe measurement clears
  the seat. NOT changed, on purpose: `ampere-6`, `amd-gcn`, `amd-rdna3`, `amd-rdna3-dgpu`, `cpu` and `dual-gpu` also
  run a Q4 vision seat, but their pages make no statement about OCR quality (the review calls them unmeasured), so they
  keep all three tasks until a measurement or a page says otherwise. A node only picks this up when its config is
  re-seeded from the tier.
  Docs: `FLEET-NODE.md`, `fleet-node.md`, `docs/tiers/ampere-8.md` (regenerated). Tests:
  `vision_tasks_tier_test.go` (a row for every shipped tier that binds a vision seat, so a new tier must decide; the
  ampere-8 declaration stays tied to its page's statement); the declaration, the ocr-restored case and the rewritten
  statement each mutated red.

- **A `linux-amdgpu` node publishes `gpu_devices[]`.** ADR 0053 decision 4 and `FLEET-NODE.md` say a Linux AMD node
  reports its cards the way an nvidia-smi node does, and `AmdgpuSysfsDeviceProbe` existed to do it, but
  `chooseSamplerKind` routed only `nvidia-smi` to the device sampler, so the probe was never started and the amd-gcn
  node published no per-card rows. `fleet-serve` now starts its sampler
  through `startVRAMSampler`, which sends `linux-amdgpu` to the device sampler: one row per amdgpu card (index, name,
  memory composed as the node's memory probe composes it), an empty uuid and no utilisation, so the delegator still reads
  the cards as unknown rather than busy. If the per-device read fails at startup though the gate probe worked, the node
  keeps the single reading and logs why. `linux-meminfo` (the RK3588 SoC: one RAM pool, no cards) deliberately stays on
  the single-value sampler, and the docs now say so instead of naming windows-generic as the only source without rows.
  Docs: `FLEET-NODE.md`, `fleet-node.md`, ADR 0053 decision 4. Tests: `fleet_sampler_test.go` (a fake sysfs tree
  through `startVRAMSampler`: one device row, APU and discrete composition, the fallback, the two single sources; a
  pin that `runFleetServe` starts its sampler through the chooser), `TestChooseSamplerKind`; each new branch and the
  call site mutated red.
- **Composite-tier citations point at ADR 0052.** The composite tier (a box as the union of its tiers, the device
  layers, the display layer and the placement table) was drafted as "ADR 0039" and recorded as ADR 0052, because 0039 was
  already "A held card is a place in line". About fifty comments, the tier pages and their generators
  (`tierdocs`, `profiles.json`), the setup scripts and ADR 0054 kept the draft number, so a reader following "ADR 0039"
  from `internal/config/layers.go` landed on the lease queue. They now cite 0052; references that mean the real 0039
  (ADR 0041, the lease-queue line in `gpuactivity` and one older entry in this file) are untouched. Comment and prose
  changes only, plus the regenerated tier pages. Not changed, on purpose: the dated plan and spec under
  `docs/superpowers/` (they record the number the work was drafted under), ADR 0061's "ADR 0039 (placement)" (it can
  be read either way), and the citations inside `internal/delegate` and `internal/mcpserver/mcpserver.go`, which
  belong to the tracks editing those files.

## [0.166.0] - 2026-10-07 - the capacity wait runs to the call's deadline; the spread deal and the roster get their guards

- **The capacity wait runs to the call's deadline, not to a fixed 120 s (ADR 0073; the diagnosis' M1).** 85 of one day's 281 deferred delegate rows were `capacity wait: no node had room within 2m0s`, for subtasks a node freeing a minute later would have taken, inside a call that has 1,500 s. A call that has a whole-call deadline (`agent_delegate`, `offload_research`) now waits for capacity until that deadline less a 15 s reserve, whatever `agent_placement_wait_sec` says, so band-0 work no longer defers for capacity while the call still has time to place it; only the last subtask to start waits that long, because a wait holds one of the call's four run slots and four of them waiting to the horizon would keep the rest of the call (and every later chunk of a batched one) from starting until it was too late to place anything: the earlier ones keep the bound the wait had before (the larger of `agent_placement_wait_sec` and `agent_lease_wait_sec`, or the call's end if sooner) and their defer says why; no subtask is started inside the reserve either (it ends at once as a `capacity` defer, `not started: the call's deadline left no room`; a call that never had more than the reserve is exempt); nothing is dispatched inside the reserve (a job placed there could only be cut, and would run on in its node with nobody waiting for it), and a call with no more than the reserve left does not wait. `agent_placement_wait_sec` is now the wait of a call with no deadline (the `delegate` CLI verb, `agent_run` and `offload_ask` with a route), 0 = 120 s; a negative value is still the off switch for every call. A composite box's eviction wait, a verification retry's wait for a busy seat and a wait that holds a run slot keep their TTL, and an `agent_lease_wait_sec` longer than the call no longer extends a wait the call bounds. Sheddable work is still shed at once. The capacity defer keeps its class and prefix and now says what bounded it (`bounded by the call's deadline, not by agent_placement_wait_sec`) instead of telling the caller to raise a setting that never bounded the wait; the holder-naming deferral of a reserved seat words the same fact. To shorten the wait of the MCP doors, shorten `agent_call_deadline_sec`. Docs: `fleet-node.md` ("What bounds the wait"), `OPERATOR-GUIDE.md`, `gpu-lease.md`, `opencode-integration.md`, `glossary.md`, the `agent_delegate` schema, forward links in ADR 0063 and 0065. Tests: `callwait_slots_test.go` (five subtasks on a fleet that never frees: the first four defer at the TTL and say why, the fifth waits for the horizon; a subtask that gets a slot inside the reserve is not started, nor a later chunk; the reserve is taken only out of a call longer than it), `callwait_test.go` (a fleet full for 1.5 s lands the subtask inside a 4 s call although the configured TTL is 1 s; the same with the setting unset; the no-deadline control arm; a wait that never frees ends before the deadline as a capacity defer with nothing dispatched in the reserve; sheddable shed at once; the off switch stays off; a call with less left than the reserve; a lease wait; nothing placed at the wait's last look; the bound itself, row by row); the six tests that pin the CUT of a wait by the deadline set the reserve to zero; each new branch mutated red.

- **Patience is the time the call has left (ADR 0073; the diagnosis' F07).** How long a job may wait to START on a node was the contract's poll budget, 660-1,260 s for a `timeout_auto` contract, longer than the call can still honour late in it: a node with a 500 s backlog passed the backlog gate, the job was dealt, the call ended first, and a job the node had already started kept running there with nobody waiting for it (17 call-deadline cuts in one day, 11 of them with the job still on a node). The patience is now `min(poll budget, time left on the call - reserve)` at every site that reads it (the auto deal, the spread deal, re-placement, the capacity wait's tick and the verdict that narrates them, which takes the gate's own function so the two cannot diverge), and the reason prints the call's arithmetic behind the gate's (`... past the 30 s this contract will wait for a start; the call's deadline is 45 s away, less the 15 s reserved for a placed job to run in, and the contract's own poll budget is 21m0s`). A feasibility refusal like the backlog gate itself: it never ranks seats by speed, a node that publishes no ETA stays no opinion, and a call with no deadline is unchanged. Docs: `fleet-node.md` (the backlog gate), ADR 0073 decision 7. Tests: `patience_call_test.go` (the rule row by row; the auto deal, the spread deal, re-placement, the capacity wait end to end with a control arm each, and the narration); each site mutated red.

- **A health read after a refusal that proves a free worker lifts the node's Retry-After cooldown (ADR 0073).** A 503's `Retry-After` is the node's own estimate, at the moment it refused, of when a worker frees; the capacity wait re-read the node's health every tick and kept it out until the hint ended all the same, so a node that drained a minute into a 300 s hint stayed unused for the rest of the wait while the subtask deferred (and with the wait now running to the call's deadline the cost is minutes). A read taken after the refusal that shows a published `max_concurrent_jobs`, fewer jobs running than it, none queued and no saturation lifts the cooldown, in the capacity wait and in re-placement, and the placement reason says so. The proof is positive evidence only: a node that publishes no counters (an older node, a queue that reads empty because nothing is counted) proves nothing, and a saturated or backlogged one keeps its cooldown. The wait's read must postdate the latest refusal in force, so a probe-memo snapshot another waiter took just before the refusal is never reused for it. A node that refuses again after a lift is firm for the rest of the run, so a node whose counters disagree with its admission costs one early ask, not one per tick, and a dispatch that follows that second refusal, or that lands after the hint would have ended anyway, is not narrated as the lift's. Docs: `fleet-node.md`, `glossary.md`, `OPERATOR-GUIDE.md`, ADR 0063 decision 1 (link), ADR 0073 decision 8. Tests: `cooldown_lift_test.go` (the proof row by row, the lift row by row, the wait end to end with a control arm for health that proves nothing and one for a node that refuses after a lift, re-placement, the memo); each branch mutated red.

- **The spread deal ranks an unmeasured seat on the fleet's median rate (ADR 0057; the diagnosis' F04).** `scoreFit`, the int the spread deal picks seats by, rated a seat that publishes no `seat_rate` on its window alone while `rankFor` (Place, the joint auto deal, re-placement) priced it at the fleet's median, so the same roster was ordered two ways: an unmeasured seat's `-window` (about -3e4) sat five orders of magnitude above a measured seat's `-eta x 10 x 2^24`, took every first mechanical slot, and lost every reasoning slot to a measured seat with a smaller window. A node that has just joined (`samples == 0`) is exactly that seat. `scoreFitWith` takes the prior (`fleetTokSPrior` over the roster the run probed, once for the whole deal, so a score does not move as nodes at their headroom leave the rotation); `scoreFit` keeps its meaning as the prior-less rule. It orders and never admits: eligibility, adequacy and the feasibility refusals are untouched. Docs: `fleet-node.md` (Score). Tests: the 18-seat brute-force ordering test is extended to the fold (`TestScoreFitOrdersAMixedFleetAsThePairwiseRankingDoes`: it agrees with `betterRanked` for every pair the seeded draw cannot decide) and `TestSpreadDealRanksAnUnmeasuredSeatOnTheFleetMedian` deals the diagnosed shapes; both mutated red.

- **The spread deal reads saturation and lease demotion for a remote, as `betterRemote` does (the diagnosis' F09).** The deal picked among the remotes by free-card tier and fit score only, so a node reporting `saturation.high` (draining, or its admission queue at the ceiling) or sitting under a long or overdue text lease kept its slot of the cycle while a healthy node idled: the dispatch was refused, the node cooled, one of the subtask's re-placements was spent and the cycle's order was gone. The pick now ranks a lease-demoted node, then a saturated one, behind every node that is neither, before tier and fit. They are demotions and never exclusions (a demoted node still takes its slot when nothing better is left in the cycle), so a cycle, a one-per-seat deal and an all-equal roster deal exactly as before. Docs: `fleet-node.md`. Tests: `spread_demotion_test.go` (a saturated node, a node at its admission ceiling, a long and an overdue lease, the demote-not-exclude cycle, the unchanged rotation); each key mutated red.

- **One number prices a new job's wait for a worker: `new_job_wait_sec` (ADR 0050 amendment; the diagnosis' F10, both halves).** A node published `queue_wait_estimate_sec`, the wait of the deepest job ALREADY queued (`excess x wall / workers`), and the delegator preferred it outright although its own formula prices a new job one slot deeper (`running + queued - workers + 1`): 4 running and 3 queued behind a 300 s wall on 4 workers read 225 s while a new job waited 300 s, so the ranking, the backlog gate and the queue budget under-priced exactly the slow nodes with the longest queues. At `depth == max_concurrent_jobs` the estimate was 0 and omitted although a new job still waits for a worker to retire. The node now publishes `new_job_wait_sec` beside it (`(capped depth - workers + 1) x wall / workers`, over the same capped backlog and wall sample; additive, absent only when the node has no wall sample or an unlimited ceiling, and an explicit `0` when a worker is free, so a delegator never falls back to counters that include uncapped renders) and the old field keeps its meaning byte for byte. The delegator believes the new field as it stands, adds the slot itself to an older node's positive estimate (from that node's own wall and ceiling), and derives the same number from the counters when a node publishes neither, so every old/new pairing prices a new job at one number and a new delegator never adds a slot to one a new node already counted. The gate's arithmetic names which number it is. Docs: `fleet-node.md` (health fields), `FLEET-NODE.md`, `glossary.md`, ADR 0050 (amendment). Tests: `startwait_test.go` (as it stands, one slot deeper, the property over a grid of node states, the printed arithmetic, a negative figure, the decode), `newjobwait_test.go` and `newjobwait_wire_test.go` in `fleetnode` (the node's arithmetic row by row against the older field, the capped-only rule, and the shipped decoder reading the shipped handler); `TestQueueWaitForFormula` and `TestQueueBudgetIsDerivedFromTheNodesETA` now say which published number they mean; each branch mutated red.

- **An unlimited `fleet_max_concurrent_jobs` no longer makes `route=auto` read an idle local seat as busy (the diagnosis' F11).** `FleetConcurrencyLimit()` resolves a negative setting to 0, "unlimited", and the busy formula compared the seat's in-flight count against it with `>=` at four sites and guarded the comparison at one (`localStillBusy`): `inflight >= 0` is always true, so with the cap unlimited an IDLE seat read as permanently busy and the work left the box although the setting says it can take any number of runs. One helper, `runner.atRunCap` (`limit > 0 && inflight >= limit`), now serves the joint deal (both of its readings, which moved from a block in `RunWith` into `readAutoLocalSlot` so they can be pinned), the per-subtask placement (both of its readings) and the capacity wait's re-reading of a seat a deal kept it off, so the sites cannot drift. Latent on the reference box (it sets no cap); the default and an explicit cap behave exactly as before. Docs: `fleet-node.md` (the `auto` route). Tests: `unlimited_cap_test.go` (the helper's truth table, the per-subtask path and the "read busy" flag it records, the joint deal through `RunWith` and `readAutoLocalSlot`'s two flags, the wait's re-reading, a control for the built-in cap); the helper and each of the six call sites mutated red.

- **A call's `remotes` may only narrow `delegate_remotes` (ADR 0038 amendment; the diagnosis' R2-F08, the standing rule that a box never delegates to the standalone machine).** A model-named `remotes` list on `agent_delegate` REPLACED the configured roster after one check, the shape of each URL (loopback, the tailnet's range or zone, a dotless name), so any tailnet host passed and was dialled with the fleet's bearer on its health read: the standalone box was kept out by habit alone, and a caller could confine a whole fan-out to one node without anything saying it had. The door now refuses an entry that `delegate_remotes` does not list, as a `config`-class defer that names it and the roster and says what to do, before any contract is prepared and before anything is dialled; a box with no `delegate_remotes` accepts no list; entries compare as the roster spells them (a trailing slash and case are the same node), and what proceeds is the roster's own spelling, so the process in-flight gate keys one node once. The engine enforces it again for a call that says its list came from a model (`RunOptions.RosterOnly`, set only by that door), so `delegate.RunWith` is not a way around the door. The operator's own CLI verbs keep naming any tailnet node: `fleet-smoke --remote` tests a node that has not joined yet. The tool schema now says the list narrows, that a URL outside the roster is refused and never dialled, and that a list REPLACES the roster (a one-node list confines every subtask); `OPERATOR-GUIDE.md` no longer says remotes are named per call, not in config. Docs: `OPERATOR-GUIDE.md`, `fleet-node.md`, ADR 0038 (amendment). Tests: `roster_test.go` (the rule row by row, the engine refusing before dialling with a control arm for the CLI path) and `roster_remotes_test.go` (the door, a box with no roster, the schema text); the two existing door tests that named a node now configure it; each branch mutated red.

- **The text a caller reads says what the mechanical ranking does: the seat expected to finish first, not the smallest (ADR 0050 item 3; the diagnosis' F12).** The `agent_delegate` `route` description told a model that mechanical goals "take the smallest eligible seat so the roomier one stays free", and the CLI help, `fit.go`'s `scoreFit` comment and `fleet-node.md` said the same. That was the rule until W-11: since then mechanical work ranks by expected completion (cold load, the node's queue wait and generation at its measured rate), the window only breaking a tie and deciding alone when no seat of the roster publishes a rate, so a fast roomy seat takes mechanical work its slower, smaller neighbour would finish later. A caller who believed the old text phrased goals to steer work to a seat the engine no longer prefers. The tool schema, the `delegate --route` help, the `scoreFit` and `adequate` comments (and the two comments in `gate.go` and `run.go` that repeated the phrase) and `fleet-node.md` (the Score bullet, the shape bullet and the cycle measurement, now dated to 0.99.0) describe the real order; the behaviour is untouched. Tests: `route_schema_test.go` reads the registered tool's `route` description and fails on the stale phrases; the ranking itself was already pinned by `TestScoreFitMechanicalPrefersTheFasterSeatWhenRatesArePublished`; the description mutated back to the stale text goes red.

## [0.165.3] - 2026-10-06 - an occupied seat waits in line; a gate turn-away keeps the deal's busy reading

- **An occupied seat is never the fallback (operator 2026-10-06: "wait in line").** 0.165.2 still loaded the local
  agent seat over another vLLM seat when no remote could take the contract at all. Now route `auto` (the joint deal
  and the per-subtask placement) and `spread` hand that subtask to the capacity wait instead: it runs on a remote that
  frees, or on the local seat once the occupant has left (`local seat was occupied by the vLLM seat <seat>, which left
  after <t>`), and ends as a capacity defer at `agent_placement_wait_sec`. The wait checks occupancy whatever started
  it, with or without remotes configured. An explicit `route:"local"` and `agent_run` with no route are unchanged.
- **A process-gate turn-away keeps the deal's busy reading.** The capacity wait re-read the local seat's load only for
  the deal's overflow subtask. A subtask the deal had sent to a remote because the seat read busy, and that the
  process gate then turned away (this process already held the node's admission slot) or a remote refused, took the
  busy seat at the first tick. `dealReadLocalBusy` now carries the deal's reading of the seat's LOAD (in flight,
  loading, occupied; never a lease, which the wait judges itself, and one this process holds must not keep its own
  subtasks off the seat) into the wait for every subtask. The first draft carried the lease too and a review caught
  it: a held media lease kept a refused subtask off a seat whose run-cap line had freed. This was the CI-only flake of
  `TestOverflowStaysOffABusySeatWhoseLoadBecomesUnreadable` (red on main 2026-10-01 and on #535);
  `TestAGateTurnAwayKeepsTheDealsBusyReading` reproduces it deterministically (old rule: 3 of 3 runs red on both
  routes).
- Docs: `fleet-node.md` (the capacity wait, the occupied seat), `opencode-integration.md`. Tests:
  `TestRunOccupiedWithNoFleetWaitsInLine` (auto, spread), `TestRunOneOccupiedWithNoFleetTakesTheSeatOnceTheOccupantLeaves`,
  `TestAGateTurnAwayKeepsTheDealsBusyReading` (spread, auto); each new branch mutated red.

## [0.165.2] - 2026-10-06 - an agent seat whose load would evict another vLLM seat counts as busy

- **The defect, on the reference box.** The harness's agent seat (`agent-pool`, the two-card pair) and opencode's
  three-card seat share the same cards, one llama-swap port and a mutually exclusive set. The local-busy probe read
  only the agent seat's own in-flight count, so while opencode held the three-card seat the agent seat read as
  not loaded, which meant idle: `agent_delegate` (route `auto` and `spread`) dealt the contract to the local seat and
  llama-swap unloaded the operator's session to load the pair. The cascade already had a guard for exactly this
  (`internal/seatguard`); the agent doors never asked it.
- **The fix.** `probeLocalBusy` asks the seat guard when the agent seat is not loaded. When the load would unload a
  loaded vLLM seat, the reading is busy and names that seat (`occupiedBy`). Route `auto` (the joint deal and the
  per-subtask placement), `spread`, the capacity wait and the retry treat it as they treat a busy seat: a remote with
  room first, a place in line when every remote is full, the local seat only when no remote can take the contract.
  Reasons say `local seat occupied: loading it would evict the loaded vLLM seat <seat>`; an unoccupied seat's wording
  is unchanged. An unknown guard reading names no seat and deals as before.
- **Refusals and the wait.** A remote that refuses (a 503 from a full fleet) no longer drops the contract onto the
  occupied seat: re-placement treats it as a place in line, as it treats a lease fence, and the capacity wait takes
  the local seat only once the occupant has left (`local seat was occupied by the vLLM seat <seat>, which left after
  <t>`). The guard is asked before the probe's roster-ambiguity fallback, because it resolves names from the serving
  config: a failed roster read no longer hides the occupant. Found by review; the first draft fixed only the deal.
- **`offload_ask` with no route.** Its files ride inline, so when the local seat is occupied an ask with no route takes
  `auto` and the response carries a `route_note` naming the seat. An explicit `route:"local"` still runs here, and
  `agent_run` with no route stays local (its `read_root` does not travel); docs say to name `route:"auto"` on a
  self-contained `agent_run` while an opencode session holds the three-card seat.
- **Off switch.** `cascade_seat_guard: false` turns this off together with the cascade guard.
- Docs: `fleet-node.md` (the local slot), `mcp-server.md` (C-46 routes), `opencode-integration.md`, and the
  `offload_ask` / `agent_delegate` route descriptions. Tests: `occupied_seat_test.go` (the real probe and guard against a
  fake llama-swap: occupied, idle, guard off, undeclared occupant, unreadable roster; `auto`, `spread`, the per-subtask
  path and the no-fleet fallback through `Run`; a refusing remote with the wait off and on; the retry note) and
  `ask_occupied_test.go`.

## [0.165.1] - 2026-10-06 - blackwell-3x16 runs the harness on two cards again; the three-card seat is opencode's

- **The operator's order, in the table.** 2026-10-04: "modify the <node-b> offload harness tier so it runs on 2 cards again
  (3 card should remain exclusive as an opencode configuration)". It reverses the 2026-09-19 order that 0.132.6 wired
  ("the 3 card tier as the agent seat now and the 2 card tier to be the opt in one"). blackwell-3x16 seeds the tp2 PAIR
  as its `vllm_seat` again: `qwen3.8-27b-vllm` on devices `0,2` (the two 5060 Tis, never the display card), window
  **163,840** at util 0.90 (soak-verified there because the display card is outside the seat), batched 3135, fp8 KV,
  L1 staging 8 GB, the tp2 store; `agent_ctx_tokens` 262,144 -> 163,840. It keeps `agent-pool-2card` and
  `27b-vllm-2card` as aliases (what callers named it while it was opt-in) and carries the fold-system chat template and
  prompt-token details over from the three-card seat, so the agent seat's request handling does not change.
- **Layers.** The `pair` layer is the home again (not opt-in; its agent seat is filled from the `vllm_seat` at install,
  as before 0.132.6), and the `triple` layer is gone from the seed, so `placement.AgentHome` resolves to the pair and no
  contract can name a three-card layer. The table declares 8 seats (9 with the triple): a card-2 lease leaves 3
  placeable, a card-0 lease 3, a display-card lease 7. The placement code is unchanged; a box that declares a
  non-opt-in triple still homes its agent lane there.
- **The three-card seat stays, as opencode's.** Its measured layout (pipeline 3 on `2,1,0`, `28,13,23`, fp8 KV pinned
  at 3.75 GiB per card, 262,144, util 0.84, L1 16 GB, its own store) is kept in the tier notes; on the reference box it
  is hand-wired and named by id in `opencode.jsonc`. The pipeline schema (`pipeline_parallel`, `layer_partition`,
  `kv_cache_memory_bytes`) is unchanged. `docs/systems/opencode-integration.md` gains the seat note, including the
  consequence that the two vLLM seats are mutually exclusive in llama-swap, so a harness contract placed locally while
  an opencode session holds the three-card seat swaps the seats (a cold load each way).
- **Docs:** the generated tier page, `composite-tier.md` (why there is no triple), `cache-server.md` (L1 staging per
  seat), `gpu-lease.md` (the home layer and the 8-seat counts), `OPERATOR-GUIDE.md`, and the code comments that named
  the three-card seat as the current `agent-pool`. Tests: `TestDualBlackwellSeedsThePairSeatWithTheCacheServer` compares the two tiers' pair seat directly
  again, `layerSetTiers` drops the triple on this instruction, `TestSeatsThatStayPlaceableUnderACardLease` counts 8,
  and the installer self-test (`setup/render.tests.ps1`) expects three layers with the bare agent seat on the pair.
