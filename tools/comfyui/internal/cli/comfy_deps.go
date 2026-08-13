// deps — what would this box need INSTALLED to run this graph at all.
//
// NOT generated — markerless on purpose, so `printing-press generate --force`
// preserves it. Do not add the generated-file marker.
//
// WHY THIS IS NOT `validate`. The two questions look adjacent and are not:
//
//	validate  is this graph well-formed against the CURRENT schema —
//	          right inputs, in-range COMBO values, no unknown classes.
//	deps      which PACKS provide the classes this graph names, and which
//	          classes nothing installed provides.
//
// validate answers "will this run here"; deps answers "what is missing, and
// what is it called so I can go install it". A graph can fail validate for
// reasons that have nothing to do with packs, and a graph whose packs are all
// missing fails validate with a pile of unknown-class findings that never name
// a single installable thing.
//
// HOW PROVENANCE IS RESOLVED. /object_info's python_module is the only field
// that says where a class came from: "nodes" for core, "comfy_extras.<name>"
// for bundled extras, "custom_nodes.<PackName>" for an installed pack. For a
// class nothing installed provides there is no python_module to read, so the
// pack NAME is recovered — when present — from the hints ComfyUI Manager
// writes into a UI-format workflow's node properties (cnr_id / aux_id). That
// is the difference between "you are missing something called
// ImpactWildcardProcessor" and "you are missing comfyui-impact-pack".

package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"comfyui-pp-cli/internal/comfy/slots"
	"comfyui-pp-cli/internal/store"
	"github.com/spf13/cobra"
)

// comfyDepPack is one resolved provider of classes used by the graph.
type comfyDepPack struct {
	// Pack is the installable unit: "core" for built-in nodes, the
	// comfy_extras module name for a bundled extra, or the custom_nodes
	// directory name for an installed pack.
	Pack string `json:"pack"`
	// PythonModule is the raw python_module value, kept verbatim so a caller
	// can distinguish core from extras from custom without re-deriving.
	PythonModule string `json:"python_module"`
	// Kind is core | extra | custom | unknown.
	Kind    string   `json:"kind"`
	Classes []string `json:"classes"`
	Count   int      `json:"count"`
}

// comfyMissingClass is a class the graph names that nothing installed provides.
type comfyMissingClass struct {
	ClassType string   `json:"class_type"`
	NodeIDs   []string `json:"node_ids"`
	// PackHint is the pack name recovered from the workflow's own metadata,
	// when the file carried any. Empty means the graph gave no clue and the
	// class name is all there is to search for.
	PackHint string `json:"pack_hint,omitempty"`
	// HintSource names where PackHint came from, so a caller can weigh it.
	HintSource string `json:"hint_source,omitempty"`
}

type comfyDepsReport struct {
	Graph          string              `json:"graph"`
	SchemaSource   string              `json:"schema_source"`
	SchemaAge      string              `json:"schema_age,omitempty"`
	ClassesKnown   int                 `json:"classes_known"`
	NodeCount      int                 `json:"node_count"`
	DistinctClass  int                 `json:"distinct_classes"`
	Packs          []comfyDepPack      `json:"packs"`
	Missing        []comfyMissingClass `json:"missing"`
	CustomPackages []string            `json:"custom_packages"`
	Portable       bool                `json:"portable"`
	Verdict        string              `json:"verdict"`
	Hint           string              `json:"hint,omitempty"`
}

// Verdicts. Deliberately the same vocabulary `validate` uses, so an agent
// reading either report branches the same way.
const (
	comfyDepsVerdictCore       = "CORE_ONLY"
	comfyDepsVerdictCustom     = "NEEDS_CUSTOM_NODES"
	comfyDepsVerdictMissing    = "MISSING"
	comfyDepsVerdictUnverified = "UNVERIFIED"
)

// Pack kinds.
const (
	comfyDepKindCore    = "core"
	comfyDepKindExtra   = "extra"
	comfyDepKindCustom  = "custom"
	comfyDepKindUnknown = "unknown"
)

func newComfyDepsCmd(flags *rootFlags) *cobra.Command {
	var (
		objectInfoPath string
		missingOnly    bool
	)

	cmd := &cobra.Command{
		Use:   "deps <graph.json>",
		Short: "Report which custom-node packs a workflow needs, and which of its classes nothing installed provides",
		Long: `Extract a workflow's node-pack dependencies: which installed pack provides each
class the graph uses, and which classes nothing on this box provides at all.

This is the "what would this box need installed to run it" question, which is
NOT the question 'validate' answers. validate checks a graph against the
CURRENT schema — inputs, COMBO membership, unknown classes. deps names the
installable units, so a graph that arrived from another machine tells you what
to go get rather than just listing classes it has never heard of.

Provenance comes from /object_info's python_module — the only field that says
where a class came from:
  nodes                     core
  comfy_extras.<name>       bundled with ComfyUI
  custom_nodes.<PackName>   an installed custom pack

For a class nothing installed provides there is no python_module to read. The
pack NAME is then recovered, when the file carries it, from the hints ComfyUI
Manager writes into a UI-format workflow's node properties (cnr_id, aux_id).
An API-format graph carries no such hints, so a missing class there is reported
with the class name alone — that is a limitation of the format, not of this
command.

Reads the cached schema by default and reports the cache's age, exactly like
'validate'. --data-source live reads the running server instead;
--object-info <dump.json> reads a schema dump and needs no server.

Exit codes:
  0   every class resolved (with or without custom packs), or the schema was
      unavailable so nothing could be checked (verdict UNVERIFIED)
  2   usage error
  3   the graph file could not be read
  13  at least one class is provided by nothing installed`,
		Example: `  comfyui-pp-cli deps workflow_api.json
  comfyui-pp-cli deps workflow_api.json --json
  comfyui-pp-cli deps workflow_api.json --missing-only
  comfyui-pp-cli deps workflow_api.json --data-source live
  cat workflow_api.json | comfyui-pp-cli deps -`,
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:data-source":      "auto",
			"pp:typed-exit-codes": "0,2,3,13",
		},
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "report a graph's node-pack dependencies")
			}
			if len(args) == 0 {
				return usageErr(fmt.Errorf("no graph given: comfyui-pp-cli deps <graph.json> (or '-' for stdin)"))
			}

			raw, err := slotsReadGraphSource(cmd, args[0])
			if err != nil {
				return err
			}
			graph, perr := slots.ParseGraph(raw)
			if perr != nil {
				return usageErr(fmt.Errorf("%s: %w", args[0], perr))
			}

			resolved, err := slotsResolveObjectInfo(cmd.Context(), flags, objectInfoPath)
			if err != nil {
				return err
			}

			report := comfyBuildDepsReport(args[0], graph, resolved, comfyExtractPackHints(raw))
			if missingOnly {
				report.Packs = nil
			}

			if flags.asJSON || (!isTerminal(cmd.OutOrStdout()) && !flags.csv && !flags.quiet && !flags.plain) {
				if perr := printJSONFiltered(cmd.OutOrStdout(), report, flags); perr != nil {
					return perr
				}
			} else {
				comfyWriteDepsSummary(cmd.OutOrStdout(), report, missingOnly)
			}

			// UNVERIFIED stays 0 for the same reason validate's does: an empty
			// schema is a first-run condition, not a bad graph.
			if report.Verdict == comfyDepsVerdictMissing {
				return slotsGraphInvalidErr(fmt.Errorf(
					"%d class(es) are provided by nothing installed on this box", len(report.Missing)))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&objectInfoPath, "object-info", "", "Read the node schema from a dump instead of the cache or server")
	cmd.Flags().BoolVar(&missingOnly, "missing-only", false, "Report only the classes nothing provides, omitting the resolved packs")

	return cmd
}

// comfyBuildDepsReport is split out of RunE so the whole resolution is
// testable against a fixture schema with no server and no filesystem.
func comfyBuildDepsReport(graphPath string, graph store.APIGraph, resolved slotsObjectInfo, hints map[string]comfyPackHint) comfyDepsReport {
	report := comfyDepsReport{
		Graph:        graphPath,
		SchemaSource: resolved.source,
		ClassesKnown: len(resolved.schema),
		NodeCount:    len(graph),
		Packs:        []comfyDepPack{},
		Missing:      []comfyMissingClass{},
	}
	if resolved.fromCache {
		report.SchemaAge = slotsCacheAge(resolved.syncedAt)
	}

	// class -> the node ids that use it, so a missing class points at the
	// nodes to look at rather than just naming itself.
	classNodes := map[string][]string{}
	for nodeID, node := range graph {
		if strings.TrimSpace(node.ClassType) == "" {
			continue
		}
		classNodes[node.ClassType] = append(classNodes[node.ClassType], nodeID)
	}
	for _, ids := range classNodes {
		sort.Strings(ids)
	}
	report.DistinctClass = len(classNodes)

	// With no schema nothing can be resolved. Say so instead of reporting
	// every class as missing, which would be a fabricated verdict.
	if len(resolved.schema) == 0 {
		report.Verdict = comfyDepsVerdictUnverified
		report.Hint = "no node schema available, so no class could be attributed to a pack. " +
			"Run 'comfyui-pp-cli sync --resources objectinfo', pass --object-info <dump.json>, or use --data-source live."
		return report
	}

	byModule := map[string][]string{}
	for classType, nodeIDs := range classNodes {
		spec, known := resolved.schema[classType]
		if !known {
			missing := comfyMissingClass{ClassType: classType, NodeIDs: nodeIDs}
			if hint, ok := hints[classType]; ok {
				missing.PackHint, missing.HintSource = hint.Pack, hint.Source
			}
			report.Missing = append(report.Missing, missing)
			continue
		}
		module := spec.PythonModule
		if strings.TrimSpace(module) == "" {
			module = ""
		}
		byModule[module] = append(byModule[module], classType)
	}

	modules := make([]string, 0, len(byModule))
	for m := range byModule {
		modules = append(modules, m)
	}
	sort.Strings(modules)
	customSeen := map[string]bool{}
	for _, module := range modules {
		classes := byModule[module]
		sort.Strings(classes)
		pack, kind := comfyClassifyModule(module)
		report.Packs = append(report.Packs, comfyDepPack{
			Pack:         pack,
			PythonModule: module,
			Kind:         kind,
			Classes:      classes,
			Count:        len(classes),
		})
		if kind == comfyDepKindCustom && !customSeen[pack] {
			customSeen[pack] = true
			report.CustomPackages = append(report.CustomPackages, pack)
		}
	}
	sort.Strings(report.CustomPackages)
	sort.Slice(report.Missing, func(i, j int) bool {
		return report.Missing[i].ClassType < report.Missing[j].ClassType
	})

	report.Portable = len(report.CustomPackages) == 0 && len(report.Missing) == 0
	switch {
	case len(report.Missing) > 0:
		report.Verdict = comfyDepsVerdictMissing
		if report.SchemaAge != "" {
			report.Hint = slotsStaleSchemaHint(resolved.syncedAt)
		}
	case len(report.CustomPackages) > 0:
		report.Verdict = comfyDepsVerdictCustom
	default:
		report.Verdict = comfyDepsVerdictCore
	}
	return report
}

// comfyClassifyModule turns a python_module value into an installable name and
// a kind. Unrecognised shapes are reported as unknown rather than forced into
// one of the three known buckets.
func comfyClassifyModule(module string) (pack string, kind string) {
	switch {
	case module == "":
		return "unknown", comfyDepKindUnknown
	case module == "nodes":
		return "core", comfyDepKindCore
	case strings.HasPrefix(module, "comfy_extras."):
		return strings.TrimPrefix(module, "comfy_extras."), comfyDepKindExtra
	case module == "comfy_extras":
		return "comfy_extras", comfyDepKindExtra
	case strings.HasPrefix(module, "custom_nodes."):
		return strings.TrimPrefix(module, "custom_nodes."), comfyDepKindCustom
	default:
		// A bare module name that is not one of the known roots is most
		// likely a custom pack registered outside custom_nodes.
		return module, comfyDepKindCustom
	}
}

// comfyPackHint is a pack name recovered from the workflow file itself.
type comfyPackHint struct {
	Pack   string
	Source string
}

// comfyExtractPackHints mines a UI-format workflow for the pack hints ComfyUI
// Manager writes into node properties, returning class_type -> hint.
//
// API-format graphs (the format this CLI submits) carry no such properties, so
// this returns empty for them — which is correct and is reported as "no hint"
// rather than treated as an error. Every lookup is defensive: this is foreign
// data from an arbitrary machine and any field may be missing or the wrong
// type.
func comfyExtractPackHints(raw []byte) map[string]comfyPackHint {
	hints := map[string]comfyPackHint{}
	var doc struct {
		Nodes []struct {
			Type       string                 `json:"type"`
			Properties map[string]interface{} `json:"properties"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return hints
	}
	for _, node := range doc.Nodes {
		classType := strings.TrimSpace(node.Type)
		if classType == "" || node.Properties == nil {
			continue
		}
		// cnr_id is the ComfyUI Registry id and is the better answer;
		// aux_id (owner/repo) is the fallback for packs installed from git.
		for _, key := range []string{"cnr_id", "aux_id"} {
			v, ok := node.Properties[key].(string)
			if !ok || strings.TrimSpace(v) == "" {
				continue
			}
			if _, exists := hints[classType]; !exists {
				hints[classType] = comfyPackHint{Pack: strings.TrimSpace(v), Source: "workflow." + key}
			}
			break
		}
	}
	return hints
}

func comfyWriteDepsSummary(w interface{ Write([]byte) (int, error) }, report comfyDepsReport, missingOnly bool) {
	fmt.Fprintf(w, "%s  %s\n", report.Verdict, report.Graph)
	fmt.Fprintf(w, "  %d node(s), %d distinct class(es); schema: %s", report.NodeCount, report.DistinctClass, report.SchemaSource)
	if report.SchemaAge != "" {
		fmt.Fprintf(w, " (%s)", report.SchemaAge)
	}
	fmt.Fprintln(w)

	if !missingOnly {
		for _, pack := range report.Packs {
			fmt.Fprintf(w, "  %-9s %-32s %d class(es)\n", pack.Kind, pack.Pack, pack.Count)
		}
	}
	if len(report.CustomPackages) > 0 {
		fmt.Fprintf(w, "  needs installed: %s\n", strings.Join(report.CustomPackages, ", "))
	}
	for _, missing := range report.Missing {
		fmt.Fprintf(w, "  MISSING   %s (nodes %s)", missing.ClassType, strings.Join(missing.NodeIDs, ", "))
		if missing.PackHint != "" {
			fmt.Fprintf(w, "  <- likely pack %q per %s", missing.PackHint, missing.HintSource)
		}
		fmt.Fprintln(w)
	}
	if report.Hint != "" {
		fmt.Fprintf(w, "  hint: %s\n", report.Hint)
	}
}
