### Added - `recipe-audit` and the recipe store: ADR 0036's rules for what a proposer may build, and the audit that publishes or demotes a candidate rule set

- **What it does.** A recipe is a candidate edit to a seat's `agent_env_rules`, kept as a typed record (`internal/recipe`):
  `id`, `seat_tag` (the capability class of the seat it was distilled for), `rules` (the closed vocabulary), `provenance`
  (the acceptance-passing runs it came from) and `status` (`proposed | published | demoted`), plus the `audit` that set the
  status. `local-offload recipe-audit` (one `case` line and one usage line in `main.go`) runs a set of contracts on one seat
  twice, with and without the recipe, under the GPU lease, judged by the acceptance DSL, and publishes the recipe only on a
  measurement-gate ADOPT with the candidate's mean steps no worse than the incumbent's. Anything else demotes it; a demoted
  recipe stays in the store with its audit. No proposer is built.
- **The amendment (ADR 0036, "Amendment 2026-10-15").** Candidates are closed-vocabulary data, never code. The proposer
  proposes and never applies. Only the branch, the parallelism and the stop class are replay-scorable (`deny_tools`,
  `allow_tools`, `max_calls_per_tool`); rules that rewrite what the seat reads (`max_observation_tokens`,
  `observation_strip`, `rewrite_error`, and `arg_limits`, which changes what a call returns) need a live A/B. Recipes are
  distilled only from acceptance-passing runs and carry a seat-capability tag. No proposer is built until trace-bearing rows
  number in the hundreds; the amendment says so and builds none. The ADR index row is unchanged: its text is the ADR's title.
- **Strict decoding.** The record has six keys and refuses any other, in the rule set and the audit too, so `rules_code` is an
  unknown key. A rule set that changes nothing is refused, so is a recipe with no provenance, a duplicate run id, an id that
  is not a slug (the id is the file name, so it cannot name a path) and a `published` record that does not carry an ADOPT
  audit with steps no worse. Every string must be shaped like rule data: lower-case identifiers for tool and argument
  names, one line of plain ASCII (letters, digits, spaces and `. , : ' _ / -`, at most 160 bytes) for a `rewrite_error`
  text, and a printable-ASCII pattern of at most 200 bytes that does not match the empty string (such a pattern fires on
  every result). The shapes keep the store from holding anything that is not data; what keeps a recipe harmless is that an
  env rule never grants or denies an effect.
- **The store.** One `<id>.json` per recipe (default `<data home>/recipes`, or `--store`), canonical bytes, written to a
  temporary name and renamed over the old file. A file whose record names another id than its name is refused, and a listing
  fails by file name on a bad record. `internal/recipe/scoring.go` classifies every key of the vocabulary as replay-scorable
  or live-A/B; a reflection test fails when `core.AgentEnvRules` gains a key nobody classified.
- **The verb.** `recipe-audit --recipe ID --seat ALIAS --seat-tag TAG --contracts TASKS.json --baseline BASELINE.json
  --engine-build STR [--store DIR] [--attempts N] [--input-out FILE]`. It refuses with a typed refusal and exit 2, before any
  seat call, when there is no `GPU_LEASE_EPOCH`, when the epoch is not a live lease in the lease directory
  (`gpulease.EpochIsCurrent`), when `--seat-tag` is not the recipe's, and on input the store or the gate will not take (an
  undeclared or repeated task, a contract with no acceptance check, a baseline that froze an incumbent or a judge other than
  `{deterministic, acceptance-dsl}`, a baseline that is not the frozen bytes). It never takes a lease and asks again before
  every seat call: a lease that moves on mid-audit ends it with exit 2 and records nothing. The gate input is built as
  `gate verdict` builds it (`eval.CheckBaseline`, `eval.Gate`, the build hash on the result); `--input-out` writes it, and a
  test pins that `gate verdict` on that file gives the printed result byte for byte. gate_cmd.go is not edited.
- **Decisions taken, with the default.**
  - The candidate arm replaces the box's table with the recipe's rules (the semantics of `local-agent --env-rules`); the
    incumbent arm is the box's own table, untouched. Each task runs on both arms, attempt after attempt, and which arm goes
    first alternates by task.
  - A finished run is judged by the acceptance DSL (pass scores 1, fail scores 0). A runner error and an infrastructure,
    config, contract, capacity or write defer are `infra_error`; a wall timeout is `timeout`; a spent step budget is
    `task_fail`, because the seat used its steps and did not finish. The gate imputes the candidate's unfinished attempts at
    the worst score and skips the incumbent's.
  - Steps are the verb's own check beside the gate: the mean over the tasks that finished on both arms, candidate no greater
    than incumbent, at least one such task. The gate's efficiency classes are tokens, money and memory, never a count of
    steps. A declared `tokens` metric is read from the run's forwarded prefill tokens and is unresolved when the run has no
    prefill accounting; the other classes are always unresolved.
  - Both arms' stamps carry the harness build, the engine build and a hash of the seat, its endpoint and the env-rule table
    the arm ran under. `--engine-build` is required because nothing in the tree reads an engine version from a seat.
  - `--seat-tag` is required and must equal the recipe's: a recipe is evidence about one class of seat. The tag is a slug the
    operator chooses; the harness checks its shape and the match, not a vocabulary.
- **Tests.** Red first, each committed failing before the code that fixed it: the unknown top-level key, `rules_code` inside
  the rules, free text and code outside the vocabulary (18 cases), the empty rule set and missing provenance, the published
  record without the audit its status needs (seven cases), the store and the scoring partition; then the verb's refusals (no lease, a lease
  this process does not hold, a lease lost between runs, a seat-tag mismatch, 13 kinds of bad input), publish on an ADOPT
  with fewer steps, demote when the steps got worse, demote on a block and on a tie, the gate-input parity, the outcome
  mapping and the metric mapping. A fake seat stands in for the model; one test runs the verb against a real lease taken in a
  temporary lease directory and again after it is released. Mutations run and killed: the up-front and the per-run lease
  checks removed, the seat-tag check removed, the steps check removed from the publish rule, publish on any verdict, the steps
  comparison inverted, an unmeasured run read as zero tokens, a spent step budget read as a timeout, a frozen incumbent
  allowed, an undeclared task allowed, a contract without acceptance allowed, the candidate arm running the box's table,
  unknown fields allowed, the text charset widened to code characters, the empty-match pattern allowed, a record published
  without an audit or with worse steps, a proposed record carrying an audit, tool names unchecked, duplicate provenance
  allowed, a record accepted from the wrong file, and a replay key reclassified.
- **Not verified here.**
  - A real audit. Nothing in this change ran a model, took a lease or loaded a seat: the seat is a fake, and the real seat
    (`recipePipelineSeat`, one pipeline per arm through `RunAgentContract`) is covered only by the unit test of the config
    each arm runs under. The first real audit has to run under `gpu reserve` on a live seat and read the printed report.
  - That the 16-task floor of the gate is reachable with contracts a recipe actually changes the behaviour of, and what
    `--attempts` above 1 does to the cost of an audit on a real seat.
  - That the forwarded prefill tokens of a real run are present on the wire result of the seat in question; the metric is
    tested on scripted results.
  - That a provenance run id names an acceptance-passing run. The store checks the shape of the ids; the delegation log is
    the evidence and nothing here reads it.
  - That a published recipe helps in use: nothing applies one. An operator copies its rule set into the seat's table.
