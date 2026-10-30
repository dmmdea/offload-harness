// CLI plumbing shared by the subcommands: config loading, argument splitting,
// input reading and result printing.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/cache"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/embedmemo"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

func loadCfg(fs *flag.FlagSet) config.Config {
	cfg, _ := loadCfgWithSource(fs)
	return cfg
}

// loadCfgWithSource loads the config and DISCLOSES its true source, warning on stderr
// whenever the effective config is built-in defaults — including the two silent-trap
// shapes a review surfaced: an explicit --config/$LOCAL_OFFLOAD_CONFIG path that does not
// exist (Load maps IsNotExist to defaults with a NIL error by design), and a file that
// exists but fails to parse. Silent degradation is the trap (live incident 2026-07-20):
// a box whose real config lived at a non-conventional path served every bare CLI call
// from built-in defaults — empty VisionModel and all — and the only symptom was a
// misleading "no vision model configured" defer in a consumer's pipeline hours later.
func loadCfgWithSource(fs *flag.FlagSet) (config.Config, config.Source) {
	cfg, src := config.LoadWithSource(fs.Lookup("config").Value.String())
	config.WarnOnDefaults(src, os.Stderr)
	// C-95: every verb that builds a delegator loads its config here (the MCP
	// server behind agent_delegate/agent_run, delegate, acceptance, report), so
	// the note that a config's own layers shadow its agent_model lives on this
	// shared path, once per process, on stderr (MCP stdout is JSON-RPC).
	config.WarnOnShadowedAgentModelOnce(src, cfg, os.Stderr)
	return cfg, src
}

// resolveCfgPath picks the config file by precedence: explicit --config flag >
// $LOCAL_OFFLOAD_CONFIG > ./config.json if it exists (LO-4: the README
// quickstart says `cp config.example.json config.json`, which was never read) >
// the conventional ~/.local-offload/config.json if it exists > "" (built-in
// defaults). The conventional-path fallback is the fix for a bare
// `local-offload mcp` spawned by an MCP host that passes neither flag nor
// env (as the registration did): without it the server silently ran on defaults
// with ShadowEnabled=false, so capture never fired and the flywheel starved.
// exists is injected so the precedence is unit-testable without touching disk.
func resolveCfgPath(flagPath, envPath, home string, exists func(string) bool) string {
	return config.ResolvePath(flagPath, envPath, home, exists)
}

func openPipeline(cfg config.Config) (*pipeline.Pipeline, func(), error) {
	if err := cfg.EnsureDirs(); err != nil {
		return nil, nil, err
	}
	// WithSeatEndpoints threads the Phase-A per-model overrides (seat_endpoints):
	// an overridden seat's completions leave for its remote tailnet base through
	// the dial-guarded client; with the key absent this is a no-op and the client
	// is byte-identical to a pre-seat build.
	client := llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, time.Duration(cfg.RequestTimeoutSec)*time.Second).
		WithSeatEndpoints(cfg.SeatEndpoints)
	// WithRemoteLanes threads the busy-aware cascade lanes (cascade_remote_lanes,
	// roast delta 7): while the machine-wide GPU lease is held, a cascade call
	// whose model a lane roster-serves rides that lane instead of queueing behind
	// the local card. Guarded so the absent key stays byte-identical (pinned).
	if len(cfg.CascadeRemoteLanes) > 0 {
		gpuLockPath, stateDir := cfg.GPULockPath, cfg.StateDir
		// FleetLaneGates, not RosterResident alone: a lane base may be a
		// plain llama-swap OR a fleet node whose own llama-swap binds
		// loopback (C-41b). The pair shares one probe, so residency and the
		// route can never disagree about which shape a base is.
		laneResident, laneRoute := llamaclient.FleetLaneGates(cfg.FleetAuthToken)
		client = client.WithRemoteLanes(cfg.CascadeRemoteLanes,
			func() bool { return delegate.LocalBusy(gpuLockPath, stateDir) },
			llamaclient.LocalSwapBusy(cfg.Endpoint),
			laneResident).WithLaneRoute(laneRoute)
	}
	// Cache + ledger are bbolt (single-writer, exclusive file lock). When the
	// long-running MCP server holds the lock, a CLI invocation degrades to
	// cache-less rather than aborting — they speed things up / report savings,
	// they are not required for correctness.
	//
	// 0.121.1 (register D-05): the cache handle is LAZY. openPipeline runs for
	// every command and every MCP server start, and the eager open made each of
	// them take (or lose) the bbolt lock before knowing whether a cacheable task
	// would ever run — which is what produced 49 empty per-process siblings on
	// this box. Nothing below opens a file; the first Get/Put does, and prints
	// the operator note then, when it is true rather than merely predicted.
	var ca *cache.Cache
	if cfg.CachePath != "" { // "" = caller opted out of caching (e.g. the confhead A/B, where a shared cache would cross-contaminate arms)
		ca = cache.New(cfg.CachePath).WithNotify(func(mode cache.Mode, used string, err error) {
			switch mode {
			case cache.ModeSibling:
				// 0.113.21: the shared file is held by another harness process (the
				// MCP server, or a sibling session's); this process keeps its own
				// in-loop hits in a per-process sibling instead of running cache-less.
				fmt.Fprintln(os.Stderr, "note: result cache is held by another local-offload process; using the per-process cache", used)
			case cache.ModeUnavailable:
				fmt.Fprintln(os.Stderr, "note: cache unavailable; continuing without cache:", err)
			}
		})
	}
	led, err := ledger.Open(cfg.LedgerPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "note: ledger unavailable (held by the MCP server?); continuing without ledger")
		led = nil
	}
	// Every non-delegation tool row becomes one card in NVIDIA PAIR's Jobs
	// list (docs/systems/pair-workloads.md); inert unless
	// pair_workloads_enabled and PAIR is installed on this box.
	pair := pairworkloads.New(pairworkloads.FromConfig(cfg))
	pair.AttachLedger(led)
	p := pipeline.New(cfg, client, ca, led)
	// A long media call opens its card when it starts (calls.go).
	p.SetCallTracker(pair)
	// A call routed to a fleet node opens its card and writes its asker row through here
	// (D5/D6, internal/pairworkloads/remote.go).
	p.SetPairEmitter(pair)
	return p, func() {
		if ca != nil {
			ca.Close()
		}
		if led != nil {
			led.Close()
		}
		// A one-shot CLI task exits right after this: deliver its PAIR card
		// first (each send is bounded at 2 s), or the frame dies in flight.
		pair.Wait()
		// The embed memo's persisted hit/miss totals are written ONLY here. A
		// process that exits without this leaves them at zero, and every reporting
		// surface then states — in the one number the Phase 0.4 gate reads — that
		// a memo which served thousands of hits was "never consulted". The error
		// is surfaced rather than swallowed: losing the counters silently is the
		// same fabricated-measurement defect in a quieter form.
		if err := embedmemo.CloseShared(); err != nil {
			fmt.Fprintln(os.Stderr, "note: embed memo counters may not have been persisted:", err)
		}
	}, nil
}

// splitPositionals separates the first n positionals from flags, treating the named value-flags as consuming
// the following token. A positional past the n slots is dropped onto flags (see overflowArg). A bare "--" ends
// the flags. The result always has n slots; missing positionals are "", and an explicit empty token fills none (so
// `summarize "" file.txt` still reads the file). splitArgs, splitTwoArgs, splitThreeArgs
// and splitFourArgs are this loop with a fixed n, kept so each subcommand's shape reads at its call site.
func splitPositionals(args []string, valueFlags map[string]bool, n int) (pos []string, flags []string) {
	ended, fenced := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if endsFlags(a, ended) {
			ended = true
			continue
		}
		if isFlagArg(a, ended) { // a lone "-" (the stdin indicator) is not a flag: it falls through to the positional rule
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if strings.ContainsRune(name, '=') {
				continue
			}
			if valueFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		if len(pos) < n {
			if a != "" { // an empty token never fills a slot, as the named splitters always behaved
				pos = append(pos, a)
			}
		} else {
			flags = overflowArg(flags, a, ended, &fenced)
		}
	}
	for len(pos) < n {
		pos = append(pos, "")
	}
	return pos, flags
}

// splitFourArgs separates the FIRST FOUR positionals (out path + ref image + driver video + prompt) from flags.
// Used by animate-character, whose CLI shape is `<out> <ref> <driver> "<prompt>" [flags]`.
func splitFourArgs(args []string, valueFlags map[string]bool) (first, second, third, fourth string, flags []string) {
	pos, flags := splitPositionals(args, valueFlags, 4)
	return pos[0], pos[1], pos[2], pos[3], flags
}

// splitThreeArgs separates the FIRST THREE positionals (e.g. out path + still + prompt) from flags. Used by
// generate-video, whose CLI shape is `<out> <still> "<prompt>" [flags]`.
func splitThreeArgs(args []string, valueFlags map[string]bool) (first, second, third string, flags []string) {
	pos, flags := splitPositionals(args, valueFlags, 3)
	return pos[0], pos[1], pos[2], flags
}

// splitTwoArgs separates the FIRST TWO positionals (e.g. out path + text) from flags. Used by generate-audio,
// whose CLI shape is `<out> "<text>" [flags]`.
func splitTwoArgs(args []string, valueFlags map[string]bool) (first, second string, flags []string) {
	pos, flags := splitPositionals(args, valueFlags, 2)
	return pos[0], pos[1], flags
}

// endsFlags reports whether tok is the bare "--" that ends flag handling. Only the first one does: after it, "--" is a positional.
func endsFlags(tok string, ended bool) bool { return !ended && tok == "--" }

// isFlagArg reports whether tok is read as a flag by the positional splitters: it starts with "-", it is not the lone "-" (the
// stdin positional) and no bare "--" has ended the flags.
func isFlagArg(tok string, ended bool) bool {
	return !ended && strings.HasPrefix(tok, "-") && tok != "-"
}

// overflowArg hands a positional the splitter has no slot for on to the flags the caller gives fs.Parse
// (as the splitters always did: Parse stops at the first non-flag, so the extra is harmless). Once a bare
// "--" has ended the flags, the extra token may start with a dash, and Parse would read it as a flag, so
// ONE "--" of the splitter's own goes in front of the first such token: Parse stops there and nothing
// after the user's terminator is ever a flag. The user's own "--" is never passed on. fenced is the
// splitter's "my -- is already in" state.
func overflowArg(flags []string, tok string, ended bool, fenced *bool) []string {
	if ended && !*fenced {
		flags = append(flags, "--")
		*fenced = true
	}
	return append(flags, tok)
}

func printHuman(res core.Result) {
	if res.Deferred {
		fmt.Printf("DEFERRED (%s) — handle this yourself.\n", res.Reason)
		if res.Partial != "" {
			fmt.Println("partial:", truncate(res.Partial, 400))
		}
		return
	}
	var pretty any
	_ = json.Unmarshal(res.Data, &pretty)
	b, _ := json.MarshalIndent(pretty, "", "  ")
	fmt.Println(string(b))
	fmt.Printf("[%d in / %d out tok, %.0f tok/s, %dms%s]\n",
		res.Meta.TokensIn, res.Meta.TokensOut, res.Meta.TokPerSec, res.Meta.LatencyMs,
		map[bool]string{true: ", cache hit", false: ""}[res.Meta.CacheHit])
}

// emitResult prints a Result, optionally projecting its Data to the --select
// fields and/or minifying with --compact. This is the harness's fastcontext
// citation pattern at the output layer: the caller keeps only the fields it
// needs (e.g. `transcribe --select gist,srt_path` drops the verbose segments[]).
// selectCSV "" / compact false reproduces the prior plain behavior exactly.
func emitResult(res core.Result, asJSON bool, selectCSV string, compact bool) {
	if keys := core.SelectKeys(selectCSV); len(keys) > 0 {
		res.Data = core.ProjectFields(res.Data, keys)
	}
	if asJSON {
		var b []byte
		if compact {
			b, _ = json.Marshal(res)
		} else {
			b, _ = json.MarshalIndent(res, "", "  ")
		}
		fmt.Println(string(b))
		return
	}
	printHuman(res)
}

func readInput(arg string) (string, error) {
	if arg == "" || arg == "-" {
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	}
	b, err := os.ReadFile(arg)
	return string(b), err
}

// splitArgs separates the first positional (input path) from flags, treating
// the named value-flags as consuming the following token. This lets the input
// path appear before flags on the command line.
// leftoverArgErr rejects arguments left over after flag parsing. The flag
// package stops at the first non-flag token, so a boolean flag given in space
// form ("--refine false") silently drops every later flag and can promote its
// value into a positional. cmd names the command for the error text.
func leftoverArgErr(fs *flag.FlagSet, cmd string) error {
	if fs.NArg() == 0 {
		return nil
	}
	return fmt.Errorf("%s: unexpected argument %q — boolean flags need the =form (e.g. --refine=false), and the prompt must be one quoted argument", cmd, fs.Arg(0))
}

// applyBatchRefineDefault propagates the CLI-level --refine=false onto batch
// jobs that carry no per-job "refine" value (job-level wins). --refine=true is
// the inert default, so only an explicit false writes anything.
func applyBatchRefineDefault(jobs []pipeline.ImageBatchJob, refine bool) {
	if refine {
		return
	}
	off := false
	for i := range jobs {
		if jobs[i].Refine == nil {
			jobs[i].Refine = &off
		}
	}
}

// splitArgs separates the FIRST positional from flags, treating the named value-flags as consuming the
// following token. A lone "-" is the stdin positional. A bare "--" ends the flags: every later token is a
// positional, even one that starts with "-" (see overflowArg for what happens to an extra one).
func splitArgs(args []string, valueFlags map[string]bool) (positional string, flags []string) {
	pos, flags := splitPositionals(args, valueFlags, 1)
	return pos[0], flags
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
