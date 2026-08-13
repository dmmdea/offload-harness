// ComfyUI slots and patching (absorbed section B).
//
// NOT generated — hand-written and preserved across regeneration.
// Do not add the generated-file marker to this file.
//
// Three commands replace the bespoke per-model Python patcher:
//
//	slots     — list a graph's tweakable inputs as stable <node_id>.<input> addresses
//	set       — apply overrides, DRY-RUN BY DEFAULT, guarded by a class assertion
//	validate  — offline preflight against the cached /object_info schema
//
// The class assertion is the guard no competing tool has. comfy-cli type-checks
// the VALUE and will happily write a correctly-typed value into a renamed or
// re-purposed node; that is exactly how a template revision silently patches the
// wrong node. An address may therefore carry the class it expects
// (`<node_id>@<ClassType>.<input>`), and a mismatch is a typed refusal.
//
// All logic lives in internal/comfy/slots as pure functions; this file is I/O,
// flags, and rendering only.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"comfyui-pp-cli/internal/comfy/slots"
	"comfyui-pp-cli/internal/store"
	"github.com/spf13/cobra"
)

// Exit codes for the patching surface. 2/3/4/5/6/7/10 are already claimed by
// helpers.go; these two are distinct so a script can tell "you addressed the
// wrong node" from "the graph itself will not run".
func slotsClassAssertionErr(err error) error { return &cliError{code: 12, err: err} }
func slotsGraphInvalidErr(err error) error   { return &cliError{code: 13, err: err} }

const slotsStdinArg = "-"

// slotsReadGraphSource reads an API-format graph from a path, or from stdin for "-".
func slotsReadGraphSource(cmd *cobra.Command, path string) ([]byte, error) {
	if path == slotsStdinArg {
		raw, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, fmt.Errorf("reading graph from stdin: %w", err)
		}
		return raw, nil
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied graph path is the whole point.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, notFoundErr(fmt.Errorf("graph file not found: %s", path))
		}
		return nil, fmt.Errorf("reading graph %s: %w", path, err)
	}
	return raw, nil
}

// slotsLoadGraphArg reads and parses the graph named by the positional argument.
func slotsLoadGraphArg(cmd *cobra.Command, path string) ([]byte, store.APIGraph, error) {
	raw, err := slotsReadGraphSource(cmd, path)
	if err != nil {
		return nil, nil, err
	}
	graph, err := slots.ParseGraph(raw)
	if err != nil {
		return nil, nil, usageErr(fmt.Errorf("%s: %w", path, err))
	}
	return raw, graph, nil
}

// slotsBareInvocation renders the "called with no arguments" outcome. Machine
// callers get a usage error and exit 2 rather than silent exit-0 help, so an
// incomplete invocation is never mistaken for success.
func slotsBareInvocation(cmd *cobra.Command, flags *rootFlags) error {
	if flags != nil && flags.asJSON {
		if err := printJSONFiltered(cmd.OutOrStdout(), map[string]any{
			"error": "requires input",
			"usage": cmd.CommandPath() + " --help",
		}, flags); err != nil {
			return err
		}
		return usageErr(fmt.Errorf("%q requires input; run %q for usage", cmd.CommandPath(), cmd.CommandPath()+" --help"))
	}
	return cmd.Help()
}

// ---------------------------------------------------------------------------
// slots
// ---------------------------------------------------------------------------

type slotsReport struct {
	Graph     string       `json:"graph"`
	NodeCount int          `json:"node_count"`
	SlotCount int          `json:"slot_count"`
	Slots     []slots.Slot `json:"slots"`
}

// newSlotsCmd returns `comfyui-pp-cli slots <graph.json>`.
//
// The whole family (slots / set / validate) is offline by construction: the graph
// comes from a file on disk and the schema from the local sync cache or an explicit
// --object-info dump. Never the network — a preflight that needs the server running
// is not a preflight.
//
// pp:data-source local
func newSlotsCmd(flags *rootFlags) *cobra.Command {
	var (
		roleFilter   string
		classFilter  string
		inputFilter  string
		includeLinks bool
	)

	cmd := &cobra.Command{
		Use:   "slots <graph.json>",
		Short: "List a graph's tweakable inputs as stable <node_id>.<input> addresses",
		Long: `List every tweakable input of an API-format graph as a stable address.

Each row is one slot: its address, the node's class_type, the input name, its
type, and its current value. Where the role is recognisable it is tagged
(positive/negative prompt, seed, steps, cfg, sampler, width, height,
checkpoint/unet, lora, denoise, batch, input image). Prompt polarity is read
from the graph WIRING — which text node reaches the sampler's positive vs
negative input — not guessed from the node title, so it survives the
ConditioningCombine and ControlNet chains that sit in between.

The typed_address column carries the guarded form ` + "`<node_id>@<ClassType>.<input>`" + `.
Paste THAT into ` + "`set`" + ` and a template revision that renumbers or re-purposes
the node becomes a refusal instead of a silently mis-applied patch.

Wired inputs (links to another node's output) are plumbing, not knobs, and are
hidden unless --include-links is passed. Pass - as the path to read stdin.`,
		Example: `  comfyui-pp-cli slots workflow_api.json
  comfyui-pp-cli slots workflow_api.json --role seed
  comfyui-pp-cli slots workflow_api.json --class KSampler --json
  comfyui-pp-cli slots workflow_api.json --include-links`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "list the slots of a graph file")
			}
			if len(args) == 0 {
				return slotsBareInvocation(cmd, flags)
			}
			_, graph, err := slotsLoadGraphArg(cmd, args[0])
			if err != nil {
				return err
			}

			all := slots.ExtractSlots(graph)
			filtered := make([]slots.Slot, 0, len(all))
			for _, s := range all {
				if s.Link && !includeLinks {
					continue
				}
				if roleFilter != "" && !strings.EqualFold(string(s.Role), roleFilter) {
					continue
				}
				if classFilter != "" && !strings.EqualFold(s.ClassType, classFilter) {
					continue
				}
				if inputFilter != "" && !strings.EqualFold(s.Input, inputFilter) {
					continue
				}
				filtered = append(filtered, s)
			}

			report := slotsReport{
				Graph:     args[0],
				NodeCount: len(graph),
				SlotCount: len(filtered),
				Slots:     filtered,
			}
			if flags.asJSON || (!isTerminal(cmd.OutOrStdout()) && !flags.csv && !flags.quiet && !flags.plain) {
				return printJSONFiltered(cmd.OutOrStdout(), report, flags)
			}
			if len(filtered) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "no slots matched (graph has %d nodes, %d total inputs)\n", len(graph), len(all))
				return nil
			}
			rows := make([][]string, 0, len(filtered))
			for _, s := range filtered {
				rows = append(rows, []string{
					s.TypedAddress,
					s.ClassType,
					s.Input,
					s.Type,
					string(s.Role),
					truncate(slots.ValueString(s.Value), 48),
				})
			}
			return flags.printTable(cmd, []string{"ADDRESS", "CLASS", "INPUT", "TYPE", "ROLE", "VALUE"}, rows)
		},
	}
	cmd.Flags().StringVar(&roleFilter, "role", "", "Only slots carrying this semantic role. One of: "+strings.Join(slotsRoleNames(), ", "))
	_ = cmd.RegisterFlagCompletionFunc("role", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return slotsRoleNames(), cobra.ShellCompDirectiveNoFileComp
	})
	cmd.Flags().StringVar(&classFilter, "class", "", "Only slots on nodes of this class_type")
	cmd.Flags().StringVar(&inputFilter, "input", "", "Only slots with this input name")
	cmd.Flags().BoolVar(&includeLinks, "include-links", false, "Include wired inputs (links to another node's output); hidden by default")
	return cmd
}

// ---------------------------------------------------------------------------
// set
// ---------------------------------------------------------------------------

type slotsSetReport struct {
	Graph       string          `json:"graph"`
	DryRun      bool            `json:"dry_run"`
	Applied     bool            `json:"applied"`
	Out         string          `json:"out,omitempty"`
	ChangeCount int             `json:"change_count"`
	NoOpCount   int             `json:"noop_count"`
	GraphSHAIn  string          `json:"graph_sha_in,omitempty"`
	GraphSHAOut string          `json:"graph_sha_out,omitempty"`
	ShapeSHAIn  string          `json:"shape_sha_in,omitempty"`
	ShapeSHAOut string          `json:"shape_sha_out,omitempty"`
	Changes     []slots.Change  `json:"changes"`
	PatchedJSON json.RawMessage `json:"graph_json,omitempty"`
}

// newSetCmd returns `comfyui-pp-cli set <graph.json> <addr>=<value> [...]`.
func newSetCmd(flags *rootFlags) *cobra.Command {
	var (
		apply         bool
		out           string
		allowNewInput bool
		allowRelink   bool
		allowHostPath bool
	)

	cmd := &cobra.Command{
		Use:   "set <graph.json> <addr>=<value> [<addr>=<value> ...]",
		Short: "Apply guarded overrides to a graph's slots (dry-run by default)",
		Long: `Apply overrides to an API-format graph. DRY-RUN BY DEFAULT: without --apply
the resolved patch set is printed (old -> new, per node) and nothing changes.

THE GUARD. An address may carry the class it expects:

    <node_id>@<ClassType>.<input>=<value>

If the graph's node no longer has that class_type, set REFUSES with exit code
12 and prints the asserted class against the class actually found. This is the
failure comfy-cli cannot catch: it type-checks the VALUE, so a correctly-typed
value lands happily in a node that a template revision renamed or re-purposed,
and the render is silently wrong. Addresses without @Class still work; they are
just unguarded.

VALUES parse as JSON first and fall back to a literal string, so ` + "`steps=30`" + `,
` + "`denoise=0.55`" + `, ` + "`enabled=true`" + ` and ` + "`x=[1,2]`" + ` land as the types ComfyUI expects
while ` + "`text=a cat on a roof`" + ` needs no quoting. Numbers keep full 64-bit
precision, so a large noise_seed round-trips exactly.

Three further refusals, each with an explicit escape hatch: writing an input the
node does not declare (--allow-new-input), overwriting a wired link with a
literal (--allow-relink), and putting an absolute host path into an input
ComfyUI resolves against its OWN input directory (--allow-host-path).

OUTPUT. With --apply and --out, the patched graph is written to that file. With
--apply and no --out, the patched graph goes to stdout (pipe it onward) and the
summary to stderr; under --json the patched graph is embedded in the report.`,
		Example: `  comfyui-pp-cli set workflow_api.json 3.steps=30
  comfyui-pp-cli set workflow_api.json '6@CLIPTextEncode.text=a lighthouse at dusk'
  comfyui-pp-cli set workflow_api.json 3@KSampler.seed=1234567890123456 --apply --out patched.json
  comfyui-pp-cli set workflow_api.json 3.steps=30 --apply | comfyui-pp-cli validate -`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "resolve overrides against a graph file")
			}
			if len(args) < 2 {
				return slotsBareInvocation(cmd, flags)
			}
			raw, graph, err := slotsLoadGraphArg(cmd, args[0])
			if err != nil {
				return err
			}

			assignments := make([]slots.Assignment, 0, len(args)-1)
			var parseProblems []error
			for _, spec := range args[1:] {
				a, perr := slots.ParseAssignment(spec)
				if perr != nil {
					parseProblems = append(parseProblems, perr)
					continue
				}
				assignments = append(assignments, a)
			}
			if len(parseProblems) > 0 {
				return usageErr(errors.Join(parseProblems...))
			}

			changes, resolveErr := slots.Resolve(graph, assignments, slots.ResolveOptions{
				AllowNewInput: allowNewInput,
				AllowRelink:   allowRelink,
				AllowHostPath: allowHostPath,
			})
			if resolveErr != nil {
				return classifySlotsResolveError(cmd, flags, resolveErr)
			}

			report := slotsSetReport{
				Graph:       args[0],
				DryRun:      !apply,
				ChangeCount: len(changes),
				Changes:     changes,
			}
			for _, ch := range changes {
				if ch.NoOp {
					report.NoOpCount++
				}
			}
			if sha, shaErr := store.GraphSHA(graph); shaErr == nil {
				report.GraphSHAIn = sha
			}
			if sha, shaErr := store.ShapeSHA(graph); shaErr == nil {
				report.ShapeSHAIn = sha
			}

			patched, err := slots.ApplyChanges(raw, changes)
			if err != nil {
				return err
			}
			if patchedGraph, perr := slots.ParseGraph(patched); perr == nil {
				if sha, shaErr := store.GraphSHA(patchedGraph); shaErr == nil {
					report.GraphSHAOut = sha
				}
				if sha, shaErr := store.ShapeSHA(patchedGraph); shaErr == nil {
					report.ShapeSHAOut = sha
				}
			}

			if apply {
				report.Applied = true
				switch {
				case out != "":
					if werr := slotsWritePatchedGraph(out, patched); werr != nil {
						return werr
					}
					report.Out = out
				case flags.asJSON:
					report.PatchedJSON = json.RawMessage(patched)
				default:
					// No --out: the patched graph IS the stdout payload so it
					// can be piped; the summary goes to stderr.
					if _, werr := cmd.OutOrStdout().Write(patched); werr != nil {
						return werr
					}
					slotsWriteSetSummary(cmd.ErrOrStderr(), report)
					return nil
				}
			}

			if flags.asJSON || (!isTerminal(cmd.OutOrStdout()) && !flags.csv && !flags.quiet && !flags.plain) {
				return printJSONFiltered(cmd.OutOrStdout(), report, flags)
			}
			slotsWriteSetSummary(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "Write the patch. Without this the resolved patch set is printed and nothing changes")
	cmd.Flags().StringVar(&out, "out", "", "With --apply, write the patched graph here (default: stdout)")
	cmd.Flags().BoolVar(&allowNewInput, "allow-new-input", false, "Permit writing an input the node does not currently declare")
	cmd.Flags().BoolVar(&allowRelink, "allow-relink", false, "Permit replacing a wired link with a literal value")
	cmd.Flags().BoolVar(&allowHostPath, "allow-host-path", false, "Permit an absolute host path in an image/video/audio input")
	return cmd
}

// classifySlotsResolveError maps a resolve failure to its exit code. A class
// assertion failure gets its own code (12) so a caller can distinguish "your
// address is stale, re-derive it" from an ordinary usage mistake.
func classifySlotsResolveError(cmd *cobra.Command, flags *rootFlags, err error) error {
	var mismatch *slots.ClassMismatchError
	if errors.As(err, &mismatch) {
		if flags != nil && flags.asJSON {
			_ = printJSONFiltered(cmd.OutOrStdout(), map[string]any{
				"error":          "class assertion failed",
				"address":        mismatch.Address,
				"node_id":        mismatch.NodeID,
				"expected_class": mismatch.Expected,
				"actual_class":   mismatch.Actual,
				"detail":         err.Error(),
				"hint":           "the node was renamed or re-purposed; re-derive the address with 'slots'",
			}, flags)
		}
		return slotsClassAssertionErr(err)
	}
	var notFound *slots.NodeNotFoundError
	if errors.As(err, &notFound) {
		return notFoundErr(err)
	}
	return usageErr(err)
}

func slotsWritePatchedGraph(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating output directory: %w", err)
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing patched graph: %w", err)
	}
	return nil
}

func slotsWriteSetSummary(w io.Writer, report slotsSetReport) {
	if len(report.Changes) == 0 {
		fmt.Fprintln(w, "no changes resolved")
		return
	}
	for _, ch := range report.Changes {
		old := "(absent)"
		if ch.OldExists {
			old = truncate(slots.ValueString(ch.OldValue), 60)
		}
		suffix := ""
		if ch.NoOp {
			suffix = "  (no-op: already this value)"
		}
		fmt.Fprintf(w, "  %s: %s -> %s%s\n", ch.TypedAddress, old, truncate(slots.ValueString(ch.NewValue), 60), suffix)
	}
	switch {
	case report.Applied && report.Out != "":
		fmt.Fprintf(w, "applied %d change(s) -> %s\n", report.ChangeCount, report.Out)
	case report.Applied:
		fmt.Fprintf(w, "applied %d change(s)\n", report.ChangeCount)
	default:
		fmt.Fprintf(w, "dry-run: %d change(s) resolved, nothing written. Pass --apply to write.\n", report.ChangeCount)
	}
	if report.GraphSHAOut != "" && report.GraphSHAIn != report.GraphSHAOut {
		fmt.Fprintf(w, "graph_sha %s -> %s\n", slotsShortSHA(report.GraphSHAIn), slotsShortSHA(report.GraphSHAOut))
	}
}

func slotsShortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// ---------------------------------------------------------------------------
// validate
// ---------------------------------------------------------------------------

// Verdicts for `validate`. Kept as constants so the human summary, the JSON
// field, and any test assert against the same three strings.
const (
	slotsVerdictPass       = "pass"
	slotsVerdictFail       = "fail"
	slotsVerdictUnverified = "unverified"
)

type slotsValidateReport struct {
	Graph string `json:"graph"`
	// OK is deliberately a POINTER so it can be null.
	//
	// With no cached schema every schema-dependent check is skipped, so
	// error_count is 0 for a graph nobody has actually validated — reporting
	// ok:true there is a false green, and the false green lands on the FIRST
	// run, before anyone has synced objectinfo. null means "not answered";
	// only a real check sets true or false. Read `verdict` for the word form.
	OK           *bool  `json:"ok"`
	Verdict      string `json:"verdict"`
	SchemaSource string `json:"schema_source"`
	// SchemaSyncedAt and SchemaAge are populated only for a schema read from
	// the local cache. A cached schema can be WRONG rather than merely old —
	// a checkpoint added after the last sync is absent from it, so a value the
	// server would accept still reports as out-of-range — and an agent that
	// cannot see the age has no way to tell that verdict apart from a real one.
	SchemaSyncedAt *time.Time      `json:"schema_synced_at,omitempty"`
	SchemaAge      string          `json:"schema_age,omitempty"`
	ClassesKnown   int             `json:"classes_known"`
	NodeCount      int             `json:"node_count"`
	ErrorCount     int             `json:"error_count"`
	WarningCount   int             `json:"warning_count"`
	Findings       []slots.Finding `json:"findings"`
	Hint           string          `json:"hint,omitempty"`
}

// newValidateCmd returns `comfyui-pp-cli validate <graph.json>`.
func newValidateCmd(flags *rootFlags) *cobra.Command {
	var (
		objectInfoPath string
		strict         bool
	)

	cmd := &cobra.Command{
		Use:   "validate <graph.json>",
		Short: "Offline preflight of a graph against the cached node schema",
		Long: `Preflight an API-format graph offline, before it costs a GPU minute.

There is NO validate-only endpoint on the ComfyUI server: POST /prompt validates
and immediately queues, so a rejected graph and an accepted one both cost a
round trip and the accepted one starts rendering. Client-side is the only dry
run that exists.

Checks: unknown class types, missing required inputs, COMBO values that are not
members of the server's option list, inputs the class does not declare, dangling
links, and absolute host paths in inputs ComfyUI resolves against its own input
directory.

COMBO options are read through the shared parser that understands BOTH shapes
ComfyUI ships simultaneously (v3 keeps options at index 1, legacy at index 0) —
a reader that assumes one shape mis-reads a large fraction of all inputs. An
EMPTY option list is reported as an unregistered model CLASS (a missing
extra_model_paths.yaml key for that loader), never as a missing file, because
dropping the file in place does not fix it.

The schema comes from the local cache (` + "`comfyui-pp-cli sync --resources objectinfo`" + `)
or from an explicit --object-info dump. With no schema available the graph-local
checks still run and the report says so rather than reporting a clean bill.

A CACHED schema goes stale the moment a model file is dropped in or a custom
node pack is installed: the value is on the server, absent from the cache, and
reported as out-of-range. Pass --data-source live to check against the running
server instead, which is the authority. When a cached run does report an
out-of-range value, the report carries the cache's age so the verdict can be
told apart from a real one.`,
		Example: `  comfyui-pp-cli validate workflow_api.json
  comfyui-pp-cli validate workflow_api.json --json
  comfyui-pp-cli validate workflow_api.json --data-source live
  comfyui-pp-cli validate workflow_api.json --object-info object_info.json
  comfyui-pp-cli validate workflow_api.json --strict`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "validate a graph file against the cached node schema")
			}
			if len(args) == 0 {
				return slotsBareInvocation(cmd, flags)
			}
			_, graph, err := slotsLoadGraphArg(cmd, args[0])
			if err != nil {
				return err
			}

			resolved, schemaErr := slotsResolveObjectInfo(cmd.Context(), flags, objectInfoPath)
			if schemaErr != nil {
				return schemaErr
			}
			schema := resolved.schema

			findings := slots.ValidateGraph(graph, schema)
			errorCount, warningCount := slots.CountFindings(findings)
			report := slotsValidateReport{
				Graph:          args[0],
				SchemaSource:   resolved.source,
				SchemaSyncedAt: resolved.syncedAt,
				ClassesKnown:   len(schema),
				NodeCount:      len(graph),
				ErrorCount:     errorCount,
				WarningCount:   warningCount,
				Findings:       findings,
			}
			if resolved.fromCache {
				report.SchemaAge = slotsCacheAge(resolved.syncedAt)
			}
			passed := errorCount == 0 && (!strict || warningCount == 0)
			switch {
			case !passed:
				report.Verdict = slotsVerdictFail
				report.OK = &passed
			case len(schema) == 0:
				// Nothing schema-dependent could run, so "no errors" is not a pass.
				report.Hint = "no cached node schema: only graph-local checks ran, so unknown classes, missing required inputs, and out-of-range COMBO values were NOT checked. Run 'comfyui-pp-cli sync --resources objectinfo' or pass --object-info <dump.json> for a real verdict."
				report.Verdict = slotsVerdictUnverified
			default:
				report.Verdict = slotsVerdictPass
				report.OK = &passed
			}
			// A membership finding read from a CACHED schema is the one verdict
			// staleness can fabricate: the value may be on the server already and
			// simply postdate the last sync. Say how old the cache is and name the
			// two ways out, rather than letting a correct graph read as broken.
			if report.Hint == "" && resolved.fromCache && slotsHasStaleSensitiveFinding(findings) {
				report.Hint = slotsStaleSchemaHint(resolved.syncedAt)
			}

			if flags.asJSON || (!isTerminal(cmd.OutOrStdout()) && !flags.csv && !flags.quiet && !flags.plain) {
				if perr := printJSONFiltered(cmd.OutOrStdout(), report, flags); perr != nil {
					return perr
				}
			} else {
				slotsWriteValidateSummary(cmd.OutOrStdout(), report)
			}
			// Only a real failure is an error exit. `unverified` stays 0: the
			// missing cache is a first-run condition, not a bad graph, and
			// failing here would break the documented first command a user runs.
			// The null `ok`, the `unverified` verdict, and the hint carry the
			// signal instead.
			if report.Verdict == slotsVerdictFail {
				return slotsGraphInvalidErr(fmt.Errorf("graph validation failed: %d error(s), %d warning(s)", errorCount, warningCount))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&objectInfoPath, "object-info", "", "Validate against this /object_info JSON dump instead of the local cache")
	cmd.Flags().BoolVar(&strict, "strict", false, "Treat warnings as failures")
	return cmd
}

func slotsWriteValidateSummary(w io.Writer, report slotsValidateReport) {
	if report.SchemaAge != "" {
		fmt.Fprintf(w, "  schema: %s, %s (%d classes)\n", report.SchemaSource, report.SchemaAge, report.ClassesKnown)
	} else {
		fmt.Fprintf(w, "  schema: %s (%d classes)\n", report.SchemaSource, report.ClassesKnown)
	}
	if report.Hint != "" {
		fmt.Fprintf(w, "  %s %s\n", yellow("INFO"), report.Hint)
	}
	for _, f := range report.Findings {
		indicator := yellow("WARN")
		if f.Severity == slots.SeverityError {
			indicator = red("FAIL")
		}
		where := f.NodeID
		if f.ClassType != "" {
			where = fmt.Sprintf("%s (%s)", f.NodeID, f.ClassType)
		}
		fmt.Fprintf(w, "  %s %s node %s: %s\n", indicator, f.Kind, where, f.Message)
		if len(f.Options) > 0 {
			fmt.Fprintf(w, "        options: %s\n", truncate(strings.Join(f.Options, ", "), 200))
		}
	}
	if report.ErrorCount == 0 && report.WarningCount == 0 {
		if report.Verdict == slotsVerdictUnverified {
			// Never print OK here: with no schema the checks that matter never ran.
			fmt.Fprintf(w, "  %s %d node(s) checked, no graph-local findings — schema checks NOT run\n",
				yellow("UNVERIFIED"), report.NodeCount)
			return
		}
		fmt.Fprintf(w, "  %s %d node(s) checked, no findings\n", green("OK"), report.NodeCount)
		return
	}
	fmt.Fprintf(w, "  %d node(s) checked: %d error(s), %d warning(s)\n", report.NodeCount, report.ErrorCount, report.WarningCount)
}

// slotsObjectInfo is a resolved node schema plus enough provenance to judge it.
//
// fromCache is what the caller branches on and is deliberately NOT inferable
// from source alone: a cached schema is the only one that can be silently wrong
// about membership, so "where did this come from" has to survive as a fact
// rather than as a string a future edit might reword.
type slotsObjectInfo struct {
	schema    slots.Schema
	source    string
	syncedAt  *time.Time
	fromCache bool
}

// slotsResolveObjectInfo resolves the node schema, honoring --data-source.
//
// Order, matching how every other read command dispatches (see data_source.go):
//
//   - an explicit --object-info dump wins — it is a LOCAL source, so combining
//     it with --data-source live is refused rather than silently resolved one
//     way, the same refusal validateDataSourceStrategy issues elsewhere;
//   - --data-source live reads /object_info off the running server. This is the
//     escape hatch from a stale cache and it must actually leave the machine:
//     answering it from cache is how a valid graph gets rejected for a
//     checkpoint the server has had for hours;
//   - otherwise the local sync cache (`sync --resources objectinfo`), which
//     lands /object_info in the generic resources table — as ONE row holding the
//     whole class map, or as one row per class depending on how the response was
//     enumerated. Both shapes are read.
//
// `auto` stays on the cache on purpose. This is the OFFLINE preflight: it is
// documented to need no server, agents run it before every submit, and turning
// the default into a network round trip would change what the command IS. The
// cost of that choice is paid by reporting the cache's age on any finding
// staleness could have invented.
//
// A missing cache is not an error: the caller reports what could not be checked.
func slotsResolveObjectInfo(ctx context.Context, flags *rootFlags, explicitPath string) (slotsObjectInfo, error) {
	if explicitPath != "" {
		if flags != nil {
			if err := validateDataSourceStrategy(flags, "local"); err != nil {
				return slotsObjectInfo{}, usageErr(err)
			}
		}
		raw, err := os.ReadFile(explicitPath) // #nosec G304 -- operator-supplied schema dump.
		if err != nil {
			return slotsObjectInfo{}, notFoundErr(fmt.Errorf("reading --object-info %s: %w", explicitPath, err))
		}
		schema, perr := slots.ParseObjectInfo(raw)
		if perr != nil {
			return slotsObjectInfo{}, usageErr(fmt.Errorf("%s: %w", explicitPath, perr))
		}
		return slotsObjectInfo{schema: schema, source: "file:" + explicitPath}, nil
	}

	if flags != nil && flags.dataSource == "live" {
		schema, err := slotsFetchLiveObjectInfo(ctx, flags)
		if err != nil {
			return slotsObjectInfo{}, err
		}
		return slotsObjectInfo{schema: schema, source: "live server"}, nil
	}

	return slotsLoadCachedObjectInfo(ctx)
}

// slotsFetchLiveObjectInfo pulls the whole schema off the running server.
//
// Unlike the cached path a failure here is fatal rather than degraded: the
// operator asked for live specifically, and quietly falling back to the cache
// would hand back exactly the answer they were trying to get away from.
func slotsFetchLiveObjectInfo(ctx context.Context, flags *rootFlags) (slots.Schema, error) {
	c, err := flags.newClient()
	if err != nil {
		return nil, err
	}
	data, err := c.Get(ctx, "/object_info", nil)
	if err != nil {
		return nil, classifyAPIError(err, flags)
	}
	schema, perr := slots.ParseObjectInfo(data)
	if perr != nil {
		return nil, apiErr(fmt.Errorf("/object_info did not return a class map: %w", perr))
	}
	if len(schema) == 0 {
		return nil, apiErr(fmt.Errorf("/object_info returned an empty schema; is the ComfyUI server still starting up?"))
	}
	return schema, nil
}

// slotsLoadCachedObjectInfo reads the node schema from the local sync cache
// without touching the network, and reports when that cache was last filled.
func slotsLoadCachedObjectInfo(ctx context.Context) (slotsObjectInfo, error) {
	dbPath := defaultDBPath("comfyui-pp-cli")
	if _, err := os.Stat(dbPath); err != nil {
		return slotsObjectInfo{source: "none (no local store)"}, nil
	}
	s, err := store.OpenReadOnlyContext(ctx, dbPath)
	if err != nil {
		return slotsObjectInfo{source: "none (local store unreadable)"}, nil
	}
	defer s.Close()

	syncedAt := slotsObjectInfoSyncedAt(s)

	rows, err := s.DB().QueryContext(ctx, `SELECT id, data FROM resources WHERE resource_type = 'objectinfo'`)
	if err != nil {
		return slotsObjectInfo{source: "none (objectinfo not synced)"}, nil
	}
	defer rows.Close()

	schema := slots.Schema{}
	for rows.Next() {
		var id string
		var data []byte
		if scanErr := rows.Scan(&id, &data); scanErr != nil {
			continue
		}
		slotsMergeObjectInfoRow(schema, id, data)
	}
	if err := rows.Err(); err != nil {
		return slotsObjectInfo{schema: schema, source: "local store (partial read)", syncedAt: syncedAt, fromCache: true}, nil
	}
	if len(schema) == 0 {
		return slotsObjectInfo{source: "none (objectinfo not synced)"}, nil
	}
	return slotsObjectInfo{schema: schema, source: "local store", syncedAt: syncedAt, fromCache: true}, nil
}

// slotsObjectInfoSyncedAt reads the objectinfo row's sync timestamp, reusing the
// same sync_state lookup the freshness hints run on. nil means the schema is in
// the resources table with no recorded sync — a write-through from a live read
// rather than a `sync` — which is unknown age, NOT fresh.
func slotsObjectInfoSyncedAt(s *store.Store) *time.Time {
	state, err := readSyncHintState(s, "objectinfo")
	if err != nil || !state.hasState {
		return nil
	}
	at := state.lastSynced
	return &at
}

// slotsCacheAge renders a cached schema's age for the report.
func slotsCacheAge(syncedAt *time.Time) string {
	if syncedAt == nil {
		return "age unknown (never synced through 'sync --resources objectinfo')"
	}
	return syncHintRoundAge(time.Since(*syncedAt)).String() + " old"
}

// slotsStaleSensitiveKinds are the finding kinds a stale cache can invent out of
// nothing. Each is a MEMBERSHIP verdict — this value/class is not in the set the
// server offers — and the set only ever grows between syncs, as model files are
// dropped in and custom node packs installed. A dangling link or a host path,
// by contrast, is wrong in the graph itself and no resync will change it.
var slotsStaleSensitiveKinds = map[string]bool{
	slots.KindComboNotInOptions: true,
	slots.KindUnknownClass:      true,
	slots.KindClassUnregistered: true,
}

func slotsHasStaleSensitiveFinding(findings []slots.Finding) bool {
	for _, f := range findings {
		if slotsStaleSensitiveKinds[f.Kind] {
			return true
		}
	}
	return false
}

func slotsStaleSchemaHint(syncedAt *time.Time) string {
	return fmt.Sprintf(
		"this verdict came from the LOCAL CACHED schema (%s): a model file or node pack added since then is absent from it, so a value the server would accept still reports as out of range. Refresh with 'comfyui-pp-cli sync --resources objectinfo', or re-run with --data-source live to check against the running server, which is the authority.",
		slotsCacheAge(syncedAt),
	)
}

// slotsMergeObjectInfoRow folds one cached row into the schema, tolerating either
// storage shape: the whole /object_info map, or a single class entry keyed by
// the row id.
func slotsMergeObjectInfoRow(schema slots.Schema, id string, data []byte) {
	if parsed, err := slots.ParseObjectInfo(data); err == nil && len(parsed) > 0 {
		for classType, spec := range parsed {
			schema[classType] = spec
		}
		return
	}
	var single interface{}
	if err := json.Unmarshal(data, &single); err != nil {
		return
	}
	if !slots.LooksLikeClassEntry(single) {
		return
	}
	if spec, ok := slots.ParseClassEntry(id, single); ok {
		schema[id] = spec
	}
}

// slotsRoleNames lists the recognised role tags, for help text and completion.
func slotsRoleNames() []string {
	names := []string{
		string(slots.RolePositivePrompt), string(slots.RoleNegativePrompt),
		string(slots.RoleSeed), string(slots.RoleSteps), string(slots.RoleCFG),
		string(slots.RoleSampler), string(slots.RoleScheduler), string(slots.RoleDenoise),
		string(slots.RoleWidth), string(slots.RoleHeight), string(slots.RoleBatch),
		string(slots.RoleCheckpoint), string(slots.RoleUNet), string(slots.RoleLoRA),
		string(slots.RoleVAE), string(slots.RoleInputImage),
	}
	sort.Strings(names)
	return names
}
