// recipe_audit_cmd.go — the `recipe-audit` verb: run a contract set with and without a recipe on
// one seat, under the GPU lease, judged by the acceptance DSL, and publish the recipe only on a
// measurement-gate ADOPT with steps no worse than the incumbent's. Anything else demotes it.
//
//	recipe-audit --recipe ID --seat ALIAS --seat-tag TAG --contracts TASKS.json --baseline BASELINE.json
//	             --engine-build STRING [--store DIR] [--attempts N] [--input-out FILE] [--config PATH]
//
// The A/B. The incumbent arm is the seat as it is: the box's own agent_env_rules, untouched. The
// candidate arm is the same seat with the recipe's rule set in their place (the semantics of
// `local-agent --env-rules`, which replaces the table). Each task of the contracts file runs on both
// arms, attempt after attempt, in an order that alternates by task so neither arm always goes first.
//
// The judge is the acceptance DSL: a finished run passes (score 1) when every acceptance check of its
// contract passes and fails (score 0) otherwise. A run that did not reach a verdict about the task is
// not scored: a runner error or an infrastructure, config, capacity or contract defer is
// infra_error, and a wall timeout is timeout; the gate counts them and puts the candidate's at the
// worst score. A spent step budget IS a verdict about the task (the seat used its steps and did not
// finish), so it is task_fail on both arms.
//
// The decision is the gate's, built exactly as `gate verdict` builds it: the baseline is the frozen
// pre-registration (eval.CheckBaseline), the input is an eval.GateInput, the judgement is eval.Gate,
// and the result carries the build that ran it. --input-out writes the input, so `gate verdict
// --baseline B --input FILE` gives the same result. The recipe is published only when the verdict is
// ADOPT AND the mean steps of the candidate, over the tasks both arms finished, are no worse than the
// incumbent's. Steps are the verb's own check beside the gate: the gate's efficiency classes are
// tokens, money and memory, never a count of steps. Everything else demotes. A demoted recipe stays
// in the store with the audit that demoted it; nothing is deleted.
//
// The lease. The verb refuses to run unless this process runs under a live GPU lease: no
// GPU_LEASE_EPOCH in the environment, or an epoch no live lease carries, is a typed refusal with
// exit 2, before the seat is asked. The lease is asked again before every seat call, and one that
// moved on mid-audit ends it with the same exit and records nothing. The verb never acquires a
// lease: run it as `local-offload gpu reserve ... -- local-offload recipe-audit ...`.
//
// Exit codes: 0 the audit ran to the end and the store records its result (published or demoted);
// 1 the audit ran but its result could not be recorded or the seat could not be opened; 2 a refusal
// (usage, no lease, a lease not held or lost, a seat tag that is not the recipe's, input the gate
// or the store will not take).
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/eval"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/recipe"
)

// The arms of the A/B.
const (
	recipeArmIncumbent = "incumbent" // the seat's own agent_env_rules, untouched
	recipeArmCandidate = "candidate" // the recipe's rule set in their place
)

// The kinds of refusal. Every refusal ends the process with code 2.
const (
	refusalUsage        = "usage"
	refusalNoLease      = "no_lease"
	refusalLeaseNotHeld = "lease_not_held"
	refusalLeaseLost    = "lease_lost"
	refusalSeatTag      = "seat_tag_mismatch"
	refusalInput        = "bad_input"
)

// recipeAuditJudgeIdentity is the judge a baseline must freeze for this verb: the acceptance DSL.
const recipeAuditJudgeIdentity = "acceptance-dsl"

// recipeAuditMaxAttempts bounds --attempts: every attempt is a run on a seat under a lease.
const recipeAuditMaxAttempts = 10

// recipeRefusal is a typed refusal of the verb. It unwraps to the exitCodeError that ends the
// process with code 2, so main prints its text and exits 2 like every other refusal.
type recipeRefusal struct{ Kind, Detail string }

func (e *recipeRefusal) Error() string { return "recipe-audit: refused (" + e.Kind + "): " + e.Detail }

func (e *recipeRefusal) Unwrap() error { return &exitCodeError{code: 2, msg: e.Error()} }

func recipeRefuse(kind, format string, a ...any) error {
	return &recipeRefusal{Kind: kind, Detail: fmt.Sprintf(format, a...)}
}

// recipeSeat runs one contract on one arm of the A/B.
type recipeSeat interface {
	Run(ctx context.Context, arm string, contract core.AgentContract) (core.AgentWireResult, error)
}

// recipeAuditSeams are the three things the verb reaches outside itself for, so a test puts a fake
// where the config file, the lease directory and the seat would be.
type recipeAuditSeams struct {
	config func(fs *flag.FlagSet) config.Config
	// lease returns nil when this process runs under a live GPU lease, else a *recipeRefusal.
	lease func(cfg config.Config) error
	seat  func(cfg config.Config, seat string, candidate core.AgentEnvRules) (recipeSeat, func(), error)
}

var recipeAuditDeps = recipeAuditSeams{config: loadCfg, lease: realRecipeAuditLease, seat: realRecipeSeat}

const recipeAuditUsage = `usage: local-offload recipe-audit --recipe ID --seat ALIAS --seat-tag TAG --contracts TASKS.json --baseline BASELINE.json --engine-build STRING [flags]

Run the contracts with and without the recipe on one seat, under the GPU lease, judged by the
acceptance DSL. Publish the recipe only when the measurement gate says ADOPT and the candidate's
steps are no worse than the incumbent's; otherwise demote it. Run it under
'local-offload gpu reserve ... -- local-offload recipe-audit ...': it never takes a lease and
refuses (exit 2) to run without one.

  --recipe ID           the recipe to audit, by id, from the store (required)
  --seat ALIAS          the seat both arms run on (required)
  --seat-tag TAG        the capability class of that seat; it must be the recipe's seat_tag (required)
  --contracts FILE      a JSON array of {"id": <task id of the baseline>, "contract": <agent contract>}, one per
                        task the baseline declares (held-out ones included); every contract carries at least one
                        acceptance check (required)
  --baseline FILE       the frozen pre-registration (gate freeze), judge {deterministic, acceptance-dsl}, primary
                        class unit_interval, only tokens efficiency metrics and no frozen incumbent: the incumbent
                        is measured here (required)
  --engine-build STR    the engine build the seat runs on; it pins the era of the comparison (required)
  --store DIR           the recipe store (default: <data home>/recipes)
  --attempts N          attempts per task and arm, 1 to 10 (default 1)
  --input-out FILE      also write the gate input, so 'gate verdict --baseline B --input FILE' reproduces the result
  --config PATH         the harness config

Exit codes:
  0  the audit ran to the end and the store records the result (published or demoted)
  1  the audit ran but the store could not record it, or the seat could not be opened
  2  a refusal: usage, no lease, a lease not held or lost, a seat tag that is not the recipe's, or input
     the store or the gate will not take
`

func runRecipeAudit(args []string) error { return recipeAuditMain(args, os.Stdout, os.Stderr) }

// recipeAuditReport is what the verb prints: one JSON document.
type recipeAuditReport struct {
	Recipe       string           `json:"recipe"`
	Seat         string           `json:"seat"`
	SeatTag      string           `json:"seat_tag"`
	StatusBefore string           `json:"status_before"`
	Status       string           `json:"status"`
	Decision     string           `json:"decision"`
	Steps        recipeAuditSteps `json:"steps"`
	// LiveABKeys names the keys of the recipe that only a live A/B can score (the audit is one).
	LiveABKeys []string        `json:"live_ab_keys"`
	Gate       eval.GateResult `json:"gate"`
}

// recipeAuditSteps is the verb's own check beside the gate: mean steps of the two arms over the
// tasks both finished (a pass or a task_fail on each arm).
type recipeAuditSteps struct {
	Incumbent float64 `json:"incumbent"`
	Candidate float64 `json:"candidate"`
	Pairs     int     `json:"pairs"`
	NonWorse  bool    `json:"non_worse"`
}

// recipeTask is one task of the contracts file after it was checked.
type recipeTask struct {
	id       string
	contract core.AgentContract
}

// recipeAttemptRec is one run of one task on one arm.
type recipeAttemptRec struct {
	outcome eval.AttemptOutcome
	score   *float64
	steps   int
	metrics map[string]*float64
}

// recipeAuditMain is runRecipeAudit with its streams passed in, so a test drives the whole verb in-process.
func recipeAuditMain(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("recipe-audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.String("config", "", "the harness config")
	recipeID := fs.String("recipe", "", "the recipe to audit, by id")
	storeDir := fs.String("store", "", "the recipe store")
	seat := fs.String("seat", "", "the seat both arms run on")
	seatTag := fs.String("seat-tag", "", "the capability class of the seat")
	contractsPath := fs.String("contracts", "", "the contracts file")
	baselinePath := fs.String("baseline", "", "the frozen baseline")
	engineBuild := fs.String("engine-build", "", "the engine build the seat runs on")
	attempts := fs.Int("attempts", 1, "attempts per task and arm")
	inputOut := fs.String("input-out", "", "also write the gate input to this file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, recipeAuditUsage)
			return nil
		}
		return recipeRefuse(refusalUsage, "%v", err)
	}
	if fs.NArg() > 0 {
		return recipeRefuse(refusalUsage, "unexpected argument %q", fs.Arg(0))
	}
	for _, f := range []struct{ name, val string }{
		{"--recipe", *recipeID}, {"--seat", *seat}, {"--seat-tag", *seatTag}, {"--contracts", *contractsPath},
		{"--baseline", *baselinePath}, {"--engine-build", strings.TrimSpace(*engineBuild)},
	} {
		if f.val == "" {
			return recipeRefuse(refusalUsage, "%s is required", f.name)
		}
	}
	if *attempts < 1 || *attempts > recipeAuditMaxAttempts {
		return recipeRefuse(refusalUsage, "--attempts %d: want 1 to %d", *attempts, recipeAuditMaxAttempts)
	}

	cfg := recipeAuditDeps.config(fs)
	if err := recipeLeaseCheck(cfg, false); err != nil {
		return err
	}

	// Inputs, all of them, before the seat is asked.
	dir := *storeDir
	if dir == "" {
		dir = filepath.Join(cfg.BaseDir(), "recipes")
	}
	store := recipe.Store{Dir: dir}
	rec, err := store.Load(*recipeID)
	if err != nil {
		return recipeRefuse(refusalInput, "%v", err)
	}
	if err := recipe.CheckSeat(*seat); err != nil {
		return recipeRefuse(refusalInput, "%v", err)
	}
	if rec.SeatTag != *seatTag {
		return recipeRefuse(refusalSeatTag, "recipe %s was distilled for seat class %q and this audit's seat is class %q: a recipe is audited on the class it was made for", rec.ID, rec.SeatTag, *seatTag)
	}
	rawBaseline, err := os.ReadFile(*baselinePath)
	if err != nil {
		return recipeRefuse(refusalInput, "baseline: %v", err)
	}
	baseline, baselineHash, err := eval.CheckBaseline(rawBaseline)
	if err != nil {
		return recipeRefuse(refusalInput, "%v", err)
	}
	if baseline.Incumbent != nil {
		return recipeRefuse(refusalInput, "the baseline froze an incumbent: recipe-audit measures the incumbent live (it needs the steps of both arms), so freeze the pre-registration without --incumbent")
	}
	if baseline.Judge.Kind != "deterministic" || baseline.Judge.Identity != recipeAuditJudgeIdentity {
		return recipeRefuse(refusalInput, "the acceptance DSL is the judge: the baseline must freeze the judge {deterministic, %s}, not {%s, %s}", recipeAuditJudgeIdentity, baseline.Judge.Kind, baseline.Judge.Identity)
	}
	// The verb scores a finished run 1 (every acceptance check passed) or 0 and reads only the tokens
	// class off a run. A baseline on another scale would judge the same runs wrongly (a candidate that
	// passes 16 of 16 against an incumbent that fails 16 of 16 is INCONCLUSIVE on the 0 to 10 scale)
	// or be refused by the gate after the whole A/B; a declared money or memory metric can never be
	// resolved, so a non-inferiority run on it is INCONCLUSIVE whatever the recipe did. All of that is
	// refused here, before the seat is asked.
	if baseline.Primary.Class != eval.ClassUnitInterval {
		return recipeRefuse(refusalInput, "the verb scores every run 1 or 0, so the baseline's primary class must be %q, not %q: freeze the pre-registration on the unit interval", eval.ClassUnitInterval, baseline.Primary.Class)
	}
	for _, e := range baseline.Efficiency {
		if e.Class != eval.EffTokens {
			return recipeRefuse(refusalInput, "efficiency metric %q is class %q and the verb can only read %q off a run (the forwarded prefill tokens): a metric it cannot resolve could never be traded away, so freeze the pre-registration without it", e.Name, e.Class, eval.EffTokens)
		}
	}
	// The gate stamps the build that ran it and refuses a stamp that is not a 64-character lower-case
	// hash (the check `gate selftest` makes before it does any work), so an unhashable build ends the
	// audit here and not after the A/B.
	build := gateBuildSHA()
	if !isLowerHex64(build) {
		return recipeRefuse(refusalInput, "the running build cannot be hashed (got %q): the gate stamps every arm with it and refuses a build_sha256 that is not 64 lower-case hex characters", build)
	}
	tasks, err := readRecipeTasks(*contractsPath, baseline.Tasks, cfg.AgentContextCapBytes())
	if err != nil {
		return err
	}

	runner, cleanup, err := recipeAuditDeps.seat(cfg, *seat, rec.Rules)
	if err != nil {
		return fmt.Errorf("recipe-audit: opening the seat: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	// The A/B.
	recs := map[string]map[string][]recipeAttemptRec{recipeArmIncumbent: {}, recipeArmCandidate: {}}
	ctx := context.Background()
	for ti, task := range tasks {
		for a := 0; a < *attempts; a++ {
			arms := []string{recipeArmIncumbent, recipeArmCandidate}
			if (ti+a)%2 == 1 {
				arms[0], arms[1] = arms[1], arms[0]
			}
			for _, arm := range arms {
				if err := recipeLeaseCheck(cfg, true); err != nil {
					return err
				}
				wire, rerr := runner.Run(ctx, arm, task.contract)
				outcome, score := recipeJudge(task.contract, wire, rerr)
				recs[arm][task.id] = append(recs[arm][task.id], recipeAttemptRec{
					outcome: outcome, score: score, steps: wire.Steps, metrics: recipeMetrics(baseline.Efficiency, wire),
				})
			}
		}
	}

	// The gate, built the way `gate verdict` builds it.
	incArm := recipeArmOf(recipeStamps(build, *seat, cfg.Endpoint, cfg.AgentEnvRules, *engineBuild), tasks, recs[recipeArmIncumbent])
	ruleSet := rec.Rules
	candArm := recipeArmOf(recipeStamps(build, *seat, cfg.Endpoint, &ruleSet, *engineBuild), tasks, recs[recipeArmCandidate])
	in := eval.GateInput{BaselineHash: baselineHash, Incumbent: &incArm, Candidate: candArm, Judge: baseline.Judge}
	if *inputOut != "" {
		raw, merr := json.MarshalIndent(in, "", "  ")
		if merr == nil {
			merr = os.WriteFile(*inputOut, append(raw, '\n'), 0o644)
		}
		if merr != nil {
			return fmt.Errorf("recipe-audit: writing --input-out: %w", merr)
		}
	}
	res, err := eval.Gate(baseline, baselineHash, in)
	if err != nil {
		return recipeRefuse(refusalInput, "the gate refused the input the runs produced (the recipe is unchanged): %v", err)
	}
	res.GateBuildSHA256 = build

	steps := recipeStepsOf(tasks, recs)
	updated := rec
	rep := recipeAuditReport{
		Recipe: rec.ID, Seat: *seat, SeatTag: rec.SeatTag, StatusBefore: rec.Status,
		Steps: steps, LiveABKeys: recipe.LiveABKeys(&rec.Rules), Gate: res,
	}
	switch {
	case res.Verdict == eval.VerdictAdopt && steps.NonWorse:
		updated.Status = recipe.StatusPublished
		rep.Decision = fmt.Sprintf("published: the gate says ADOPT and the steps are no worse (%g against %g over %d tasks)", steps.Candidate, steps.Incumbent, steps.Pairs)
	case res.Verdict == eval.VerdictAdopt && steps.Pairs == 0:
		updated.Status = recipe.StatusDemoted
		rep.Decision = "demoted: the gate says ADOPT but no task finished on both arms, so the steps were not measured"
	case res.Verdict == eval.VerdictAdopt:
		updated.Status = recipe.StatusDemoted
		rep.Decision = fmt.Sprintf("demoted: the gate says ADOPT but the steps got worse (%g against %g over %d tasks)", steps.Candidate, steps.Incumbent, steps.Pairs)
	default:
		updated.Status = recipe.StatusDemoted
		rep.Decision = fmt.Sprintf("demoted: the gate says %s: %s", res.Verdict, res.Reason)
	}
	rep.Status = updated.Status
	updated.Audit = &recipe.Audit{
		Seat: *seat, BaselineHash: baselineHash, EraMarker: res.EraMarker, Verdict: res.Verdict,
		StepsIncumbent: steps.Incumbent, StepsCandidate: steps.Candidate,
	}
	if err := store.Save(updated); err != nil {
		return fmt.Errorf("recipe-audit: the audit finished (%s) but the store could not record it: %w", rep.Decision, err)
	}
	j, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("recipe-audit: %w", err)
	}
	if _, err := stdout.Write(append(j, '\n')); err != nil {
		return fmt.Errorf("recipe-audit: write result: %w", err)
	}
	return nil
}

// recipeLeaseCheck asks whether this process still runs under a live GPU lease. mid is true for the
// checks between seat calls: a lease that was held at the start and is not now was lost.
func recipeLeaseCheck(cfg config.Config, mid bool) error {
	err := recipeAuditDeps.lease(cfg)
	if err == nil {
		return nil
	}
	var ref *recipeRefusal
	if !errors.As(err, &ref) {
		ref = &recipeRefusal{Kind: refusalNoLease, Detail: err.Error()}
	}
	if mid {
		return recipeRefuse(refusalLeaseLost, "the GPU lease this audit started under is gone (%s): the A/B stopped, nothing is recorded", ref.Detail)
	}
	return ref
}

// realRecipeAuditLease is the lease check the verb runs with: GPU_LEASE_EPOCH (threaded to the
// command by `gpu reserve`) must name a lease that is live in the lease directory. It reads; it
// never acquires.
func realRecipeAuditLease(cfg config.Config) error {
	raw := strings.TrimSpace(os.Getenv("GPU_LEASE_EPOCH"))
	if raw == "" {
		return recipeRefuse(refusalNoLease, "GPU_LEASE_EPOCH is not set: the A/B runs on a seat only under a GPU lease, so run it as 'local-offload gpu reserve ... -- local-offload recipe-audit ...'")
	}
	epoch, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || epoch == 0 {
		return recipeRefuse(refusalNoLease, "GPU_LEASE_EPOCH=%q is not a lease epoch", raw)
	}
	dir := strings.TrimSpace(os.Getenv("GPU_LEASE_DIR"))
	if dir == "" {
		d, derr := gpulease.LeaseDir(cfg.GPULockPath, cfg.StateDir)
		if derr != nil {
			return recipeRefuse(refusalLeaseNotHeld, "the lease directory cannot be resolved: %v", derr)
		}
		dir = d
	}
	if !gpulease.EpochIsCurrent(dir, epoch) {
		return recipeRefuse(refusalLeaseNotHeld, "lease epoch %d is not a live lease at %s: this process does not hold one", epoch, dir)
	}
	return nil
}

// readRecipeTasks reads the contracts file: a JSON array of {id, contract}, strict. Every id must be
// a task the baseline declared, once; every contract must decode and carry at least one acceptance
// check (the acceptance DSL is the judge, so a contract without one has none), and every declared
// task must have its contract.
func readRecipeTasks(path string, declared []string, capBytes int) ([]recipeTask, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, recipeRefuse(refusalInput, "contracts: %v", err)
	}
	var entries []struct {
		ID       string          `json:"id"`
		Contract json.RawMessage `json:"contract"`
	}
	if err := decodeJSONStrict(raw, &entries); err != nil {
		return nil, recipeRefuse(refusalInput, "contracts %s: %v", path, err)
	}
	if len(entries) == 0 {
		return nil, recipeRefuse(refusalInput, "contracts %s: no contracts", path)
	}
	known := make(map[string]bool, len(declared))
	for _, id := range declared {
		known[id] = true
	}
	seen := map[string]bool{}
	out := make([]recipeTask, 0, len(entries))
	for _, e := range entries {
		switch {
		case e.ID == "":
			return nil, recipeRefuse(refusalInput, "contracts %s: an entry has no id", path)
		case !known[e.ID]:
			return nil, recipeRefuse(refusalInput, "contracts %s: task %q is not in the baseline's declared tasks", path, e.ID)
		case seen[e.ID]:
			return nil, recipeRefuse(refusalInput, "contracts %s: task %q appears twice", path, e.ID)
		}
		seen[e.ID] = true
		c, err := core.DecodeAgentContractWithCap(bytes.NewReader(e.Contract), capBytes)
		if err != nil {
			return nil, recipeRefuse(refusalInput, "task %q: %v", e.ID, err)
		}
		if len(c.Acceptance) == 0 {
			return nil, recipeRefuse(refusalInput, "task %q: the contract has no acceptance check, so the acceptance DSL has nothing to judge", e.ID)
		}
		out = append(out, recipeTask{id: e.ID, contract: c})
	}
	// One contract per declared task, held-out ones included: a task with none is a pair the A/B never
	// measures, and a short set only shows as "n below min_pairs" after the seat has run for nothing.
	var missing []string
	for _, id := range declared {
		if !seen[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return nil, recipeRefuse(refusalInput, "contracts %s: the baseline declares %d task(s) with no contract: %s", path, len(missing), strings.Join(missing, ", "))
	}
	return out, nil
}

// recipeJudge turns one run into a gate outcome and score (see the file comment for the mapping).
func recipeJudge(c core.AgentContract, wire core.AgentWireResult, runErr error) (eval.AttemptOutcome, *float64) {
	score := func(v float64) *float64 { return &v }
	switch {
	case runErr != nil:
		return eval.OutcomeInfraError, nil
	case wire.Deferred:
		switch wire.DeferClass {
		case core.DeferClassInfrastructure, core.DeferClassConfig, core.DeferClassContract, core.DeferClassCapacity, core.DeferClassWrite:
			return eval.OutcomeInfraError, nil
		case core.DeferClassBudget:
			if wire.StopReason == "budget" || strings.Contains(wire.Reason, "step budget exhausted") {
				return eval.OutcomeTaskFail, score(0)
			}
			return eval.OutcomeTimeout, nil
		}
		return eval.OutcomeTaskFail, score(0)
	}
	if len(delegate.EvalAcceptance(c, wire)) == 0 {
		return eval.OutcomePass, score(1)
	}
	return eval.OutcomeTaskFail, score(0)
}

// recipeMetrics reads the declared efficiency metrics off one run. Only the tokens class can be read:
// the forwarded prefill tokens. A run with no prefill accounting, and every metric of another class,
// is unresolved (null), never 0.
func recipeMetrics(effs []eval.EfficiencyMetric, wire core.AgentWireResult) map[string]*float64 {
	out := make(map[string]*float64, len(effs))
	for _, e := range effs {
		var v *float64
		if e.Class == eval.EffTokens && wire.PrefillTokens > 0 {
			f := float64(wire.PrefillTokens)
			v = &f
		}
		out[e.Name] = v
	}
	return out
}

// recipeStamps pins the era of one arm. The seat config hash covers the seat, its endpoint and the
// env-rule table the arm ran under, so the two arms differ exactly by what the recipe changes.
func recipeStamps(build, seat, endpoint string, rules *core.AgentEnvRules, engineBuild string) eval.Stamps {
	doc := struct {
		Seat     string              `json:"seat"`
		Endpoint string              `json:"endpoint"`
		Rules    *core.AgentEnvRules `json:"agent_env_rules"`
	}{seat, endpoint, rules}
	j, _ := json.Marshal(doc) // strings and a struct of plain fields cannot fail to marshal
	sum := sha256.Sum256(j)
	return eval.Stamps{HarnessBuildSHA256: build, SeatConfigSHA256: hex.EncodeToString(sum[:]), EngineBuild: strings.TrimSpace(engineBuild)}
}

// recipeArmOf builds one arm of the gate input from the runs, in the order of the contracts file.
func recipeArmOf(stamps eval.Stamps, tasks []recipeTask, recs map[string][]recipeAttemptRec) eval.Arm {
	arm := eval.Arm{Stamps: stamps, Tasks: make([]eval.Task, 0, len(tasks))}
	for _, t := range tasks {
		et := eval.Task{ID: t.id, Attempts: make([]eval.Attempt, 0, len(recs[t.id]))}
		for _, r := range recs[t.id] {
			et.Attempts = append(et.Attempts, eval.Attempt{Outcome: r.outcome, Score: r.score, Metrics: r.metrics})
		}
		arm.Tasks = append(arm.Tasks, et)
	}
	return arm
}

// recipeStepsOf is the verb's steps check: for every task that finished on both arms (an attempt that
// is a pass or a task_fail), the mean steps of its finished attempts on each arm; then the mean of
// those over the tasks. NonWorse needs at least one such task and a candidate that used no more.
func recipeStepsOf(tasks []recipeTask, recs map[string]map[string][]recipeAttemptRec) recipeAuditSteps {
	mean := func(rs []recipeAttemptRec) (float64, bool) {
		sum, n := 0.0, 0
		for _, r := range rs {
			if r.outcome == eval.OutcomePass || r.outcome == eval.OutcomeTaskFail {
				sum += float64(r.steps)
				n++
			}
		}
		if n == 0 {
			return 0, false
		}
		return sum / float64(n), true
	}
	var st recipeAuditSteps
	var sumInc, sumCand float64
	for _, t := range tasks {
		mi, iok := mean(recs[recipeArmIncumbent][t.id])
		mc, cok := mean(recs[recipeArmCandidate][t.id])
		if iok && cok {
			sumInc += mi
			sumCand += mc
			st.Pairs++
		}
	}
	if st.Pairs > 0 {
		st.Incumbent = sumInc / float64(st.Pairs)
		st.Candidate = sumCand / float64(st.Pairs)
		st.NonWorse = st.Candidate <= st.Incumbent
	}
	return st
}

// recipePipelineSeat is the real seat: one pipeline per arm, opened when the arm first runs. The
// incumbent's pipeline is the box's config as it is; the candidate's differs by agent_env_rules alone.
type recipePipelineSeat struct {
	cfg       config.Config
	seat      string
	candidate core.AgentEnvRules
	pipes     map[string]*pipeline.Pipeline
	cleanups  []func()
}

func realRecipeSeat(cfg config.Config, seat string, candidate core.AgentEnvRules) (recipeSeat, func(), error) {
	s := &recipePipelineSeat{cfg: cfg, seat: seat, candidate: candidate, pipes: map[string]*pipeline.Pipeline{}}
	return s, s.close, nil
}

func (s *recipePipelineSeat) close() {
	for _, c := range s.cleanups {
		c()
	}
	s.cleanups = nil
}

func (s *recipePipelineSeat) pipelineFor(arm string) (*pipeline.Pipeline, error) {
	if p := s.pipes[arm]; p != nil {
		return p, nil
	}
	p, cleanup, err := openPipeline(recipeArmConfig(s.cfg, arm, s.candidate))
	if err != nil {
		return nil, err
	}
	if cleanup != nil {
		s.cleanups = append(s.cleanups, cleanup)
	}
	s.pipes[arm] = p
	return p, nil
}

// recipeArmConfig is the config one arm runs under: the box's own for the incumbent, and for the
// candidate the same config with the recipe's rule set in place of agent_env_rules (the semantics of
// local-agent --env-rules, which replaces the table). cfg is a copy; the box's table is not touched.
func recipeArmConfig(cfg config.Config, arm string, candidate core.AgentEnvRules) config.Config {
	if arm == recipeArmCandidate {
		rules := candidate
		cfg.AgentEnvRules = &rules
	}
	return cfg
}

func (s *recipePipelineSeat) Run(ctx context.Context, arm string, contract core.AgentContract) (core.AgentWireResult, error) {
	p, err := s.pipelineFor(arm)
	if err != nil {
		return core.AgentWireResult{}, err
	}
	return p.RunAgentContract(ctx, contract, delegate.LocalOptions{Seat: s.seat})
}
