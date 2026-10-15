// recipe_audit_cmd.go — the `recipe-audit` verb: run a contract set with and without a recipe on
// one seat, under the GPU lease, judged by the acceptance DSL, and publish the recipe only on a
// measurement-gate ADOPT with steps no worse than the incumbent's. Anything else demotes it.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/eval"
)

// The arms of the A/B.
const (
	recipeArmIncumbent = "incumbent" // the seat's own agent_env_rules, untouched
	recipeArmCandidate = "candidate" // the recipe's rule set in their place
)

// The kinds of refusal. Every refusal ends the process with code 2, before the seat is asked.
const (
	refusalUsage        = "usage"
	refusalNoLease      = "no_lease"
	refusalLeaseNotHeld = "lease_not_held"
	refusalLeaseLost    = "lease_lost"
	refusalSeatTag      = "seat_tag_mismatch"
	refusalInput        = "bad_input"
)

// recipeRefusal is a typed refusal of the verb. It unwraps to the exitCodeError that ends the
// process with code 2, so main prints its text and exits 2 like every other refusal.
type recipeRefusal struct{ Kind, Detail string }

func (e *recipeRefusal) Error() string { return "recipe-audit: refused (" + e.Kind + "): " + e.Detail }

func (e *recipeRefusal) Unwrap() error { return &exitCodeError{code: 2, msg: e.Error()} }

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

var recipeAuditDeps = recipeAuditSeams{
	config: loadCfg,
	lease:  func(config.Config) error { return nil },
	seat: func(config.Config, string, core.AgentEnvRules) (recipeSeat, func(), error) {
		return nil, nil, errors.New("recipe-audit: no seat")
	},
}

// recipeAuditReport is what the verb prints.
type recipeAuditReport struct {
	Recipe       string           `json:"recipe"`
	Seat         string           `json:"seat"`
	SeatTag      string           `json:"seat_tag"`
	StatusBefore string           `json:"status_before"`
	Status       string           `json:"status"`
	Decision     string           `json:"decision"`
	Steps        recipeAuditSteps `json:"steps"`
	LiveABKeys   []string         `json:"live_ab_keys"`
	Gate         eval.GateResult  `json:"gate"`
}

// recipeAuditSteps is the verb's own check beside the gate: mean steps of the two arms over the
// tasks both finished.
type recipeAuditSteps struct {
	Incumbent float64 `json:"incumbent"`
	Candidate float64 `json:"candidate"`
	Pairs     int     `json:"pairs"`
	NonWorse  bool    `json:"non_worse"`
}

// recipeJudge turns one run into a gate outcome.
func recipeJudge(c core.AgentContract, wire core.AgentWireResult, runErr error) (eval.AttemptOutcome, *float64) {
	return "", nil
}

// recipeMetrics reads the declared efficiency metrics off one run.
func recipeMetrics(effs []eval.EfficiencyMetric, wire core.AgentWireResult) map[string]*float64 {
	return nil
}

// realRecipeAuditLease is the lease check the verb runs with.
func realRecipeAuditLease(config.Config) error { return nil }

const recipeAuditUsage = "usage: local-offload recipe-audit\n"

func runRecipeAudit(args []string) error { return recipeAuditMain(args, os.Stdout, os.Stderr) }

// recipeAuditMain is runRecipeAudit with its streams passed in, so a test drives the whole verb in-process.
func recipeAuditMain(args []string, stdout, stderr io.Writer) error {
	return errors.New("recipe-audit: not implemented")
}
