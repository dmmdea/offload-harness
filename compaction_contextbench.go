// compaction_contextbench.go - `compaction-eval contextbench`: drive generated
// ContextBench tasks against one agent seat and grade them from the context the
// harness kept, so the compaction ladder and the context pager can be measured on
// long-horizon work. See internal/compeval/contextbench_run.go for the run shape and
// docs/systems/coding-agent.md for what each family measures.
//
//	compaction-eval contextbench [--family all|kv_store|log_triage|needle_retention|sudoku_sketchpad]
//	    [--level NAME] [--tasks N] [--seed S] [--endpoint URL] [--model ID] [--out results.json]
//	    [--budget N] [--reserve N] [--delivery tool_result|user] [--gcf] [--skeleton]
//	    [--max-turns N] [--max-out N] [--pass-at X] [--request-timeout S] [--config FILE]
//
// It runs a model; the tests never do, they inject a fake seat. With no seat
// reachable every task is infra_error, never task_fail.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/compeval"
)

// contextBenchSeat builds the seat the run talks to. A variable so a test can
// replace the real client with a fake; the verb never opens a connection itself.
var contextBenchSeat = func(endpoint, model string, timeout time.Duration) compeval.CBSeat {
	if endpoint == "" {
		return func(context.Context, []agent.Msg, int) (string, error) {
			return "", fmt.Errorf("no serving endpoint configured (set --endpoint or the config's endpoint)")
		}
	}
	client := agent.NewLLMClient(endpoint, model, "", timeout)
	return func(ctx context.Context, msgs []agent.Msg, maxOut int) (string, error) {
		comp, err := client.Chat(ctx, msgs, nil, maxOut)
		if err != nil {
			return "", err
		}
		return comp.Msg.Content, nil
	}
}

// checkContextBenchFlags refuses a value the run would silently replace with a default (the
// library reads a zero as "unset", so --reserve 0 would run, and be reported, as 2048) or that
// cannot mean anything (a pass line above 1.0, a negative cap).
func checkContextBenchFlags(budget, reserve, maxOut, maxTurns, requestTimeout int, passAt float64) error {
	const verb = "compaction-eval contextbench: "
	switch {
	case budget < 1:
		return fmt.Errorf(verb+"--budget must be at least 1 token, got %d", budget)
	case reserve < 1:
		return fmt.Errorf(verb+"--reserve must be at least 1 token (0 would be read as the default %d), got %d", compeval.CBDefaultReserve, reserve)
	case !(passAt > 0 && passAt <= 1):
		return fmt.Errorf(verb+"--pass-at must be above 0 and at most 1 (a reward never exceeds 1, and 0 would be read as the default 1.0), got %v", passAt)
	case maxOut < 1:
		return fmt.Errorf(verb+"--max-out must be at least 1 token (0 would be read as the default 4096), got %d", maxOut)
	case maxTurns < 0:
		return fmt.Errorf(verb+"--max-turns must be 0 (the default, 3 x ops + 10) or more, got %d", maxTurns)
	case requestTimeout < 1:
		return fmt.Errorf(verb+"--request-timeout must be at least 1 second, got %d", requestTimeout)
	}
	return nil
}

func runCompactionContextBench(args []string) error {
	fs := flag.NewFlagSet("compaction-eval contextbench", flag.ExitOnError)
	fs.String("config", "", "config file path (supplies the default endpoint and model)")
	family := fs.String("family", "all", "task family: all | kv_store | log_triage | needle_retention | sudoku_sketchpad")
	level := fs.String("level", "", "a published level of the one chosen family (default: the smallest whose uncompacted transcript exceeds the usable budget)")
	tasks := fs.Int("tasks", 1, "tasks per family; task i uses seed --seed+i")
	seed := fs.Int64("seed", 42, "first task seed (the published seeds are 42 to 49)")
	endpoint := fs.String("endpoint", "", "serving endpoint (default: the configured endpoint)")
	model := fs.String("model", "", "model id of the seat to drive (default: the configured workhorse)")
	seat := fs.String("seat", "", "alias of --model: the seat's model id")
	route := fs.String("route", "local", "only 'local': this verb drives the one endpoint it is given and does not place work on the fleet")
	out := fs.String("out", "", "write the results JSON here (default: stdout)")
	budget := fs.Int("budget", compeval.CBDefaultBudget, "context budget in estimated tokens (must fit the served window)")
	reserve := fs.Int("reserve", compeval.CBDefaultReserve, "tokens held back from the budget for the reply")
	maxOut := fs.Int("max-out", 4096, "reply cap in tokens")
	maxTurns := fs.Int("max-turns", 0, "seat replies allowed per task, nudges included; reaching it ends the task as timeout (0 = 3 x ops + 10)")
	delivery := fs.String("delivery", compeval.CBDeliveryToolResult, "how an operation reaches the seat: tool_result (every ladder rung can act) | user (the upstream shape; only the drop rung can act)")
	passAt := fs.Float64("pass-at", 1.0, "reward at or above which a graded task is 'pass' (the reward is always reported too)")
	gcf := fs.Bool("gcf", false, "enable the lossless GCF rung in the ladder")
	skeleton := fs.Bool("skeleton", false, "enable the skeleton rung in the ladder")
	reqTimeout := fs.Int("request-timeout", 600, "transport deadline per seat request, in seconds; a request that hits it ends the task as infra_error")
	_ = fs.Parse(args)

	if *route != "local" {
		return fmt.Errorf("compaction-eval contextbench: --route %q is not supported; the verb drives the one endpoint it is given (use --endpoint)", *route)
	}
	if err := checkContextBenchFlags(*budget, *reserve, *maxOut, *maxTurns, *reqTimeout, *passAt); err != nil {
		return err
	}
	if *delivery != compeval.CBDeliveryToolResult && *delivery != compeval.CBDeliveryUser {
		return fmt.Errorf("compaction-eval contextbench: --delivery must be %s or %s, got %q", compeval.CBDeliveryToolResult, compeval.CBDeliveryUser, *delivery)
	}
	if *reserve >= *budget {
		return fmt.Errorf("compaction-eval contextbench: --reserve (%d) must be below --budget (%d)", *reserve, *budget)
	}
	if *seat != "" {
		if *model != "" && *model != *seat {
			return fmt.Errorf("compaction-eval contextbench: --seat and --model name different seats (%q, %q)", *seat, *model)
		}
		*model = *seat
	}
	instances, err := compeval.BuildCBTasks(*family, *level, *seed, *tasks)
	if err != nil {
		return err
	}

	opts := compeval.CBRunOpts{
		Budget: *budget, Reserve: *reserve, MaxOut: *maxOut, MaxTurns: *maxTurns,
		Ladder: compeval.LadderOpts{GCF: *gcf, Skeleton: *skeleton}, Delivery: *delivery, PassAt: *passAt,
	}
	for _, in := range instances {
		if err := compeval.CBCheckBudget(in, opts); err != nil {
			return fmt.Errorf("compaction-eval contextbench: --budget %d --reserve %d is too small: %w", *budget, *reserve, err)
		}
	}

	cfg := loadCfg(fs)
	if *endpoint == "" {
		*endpoint = cfg.Endpoint
	}
	if *model == "" {
		*model = cfg.Model
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fmt.Fprintf(os.Stderr, "[compaction-eval] contextbench: %d task(s) seat=%s delivery=%s ladder=%s budget=%d reserve=%d\n",
		len(instances), *model, *delivery, opts.Ladder.Label(), *budget, *reserve)
	rep := compeval.RunCB(ctx, instances, contextBenchSeat(*endpoint, *model, time.Duration(*reqTimeout)*time.Second), opts, *model)

	if *out != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "[compaction-eval] contextbench report -> %s\n", *out)
	} else if err := emitJSON(rep); err != nil {
		return err
	}
	for _, w := range compeval.CBOutcomes {
		if n := rep.Counts[w]; n > 0 {
			fmt.Fprintf(os.Stderr, "[compaction-eval] contextbench %s: %d\n", w, n)
		}
	}
	if graded := rep.Counts[compeval.CBOutcomePass] + rep.Counts[compeval.CBOutcomeTaskFail] + rep.Counts[compeval.CBOutcomeCheat]; graded == 0 {
		return fmt.Errorf("contextbench: no task reached a verdict (%d infra_error, %d timeout, %d judge_error, %d not_run): nothing was measured",
			rep.Counts[compeval.CBOutcomeInfraError], rep.Counts[compeval.CBOutcomeTimeout],
			rep.Counts[compeval.CBOutcomeJudgeError], rep.Counts[compeval.CBOutcomeNotRun])
	}
	return nil
}
