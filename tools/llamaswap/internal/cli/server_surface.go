// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored novel command + shared server-surface intelligence.
// pp:data-source live
//
// Two questions this CLI could previously only answer by assumption:
//
//  1. Is the server actually the version this CLI was written against? The
//     pinned surface (v249) was an invisible assumption baked into the
//     capability map, the User-Agent, and the config schema. `version drift`
//     turns it into an asserted, reported fact.
//  2. Is the thing on the other end llama-swap at all? llama.cpp's own
//     llama-server has shipped a native model-swapping ROUTER mode since
//     Dec 2025, and it serves a /models endpoint of a similar shape. Pointing
//     these admin commands at one and reading the resulting 404s as faults is
//     a misdiagnosis; the detector below names it instead.

package cli

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/lsconfig"
)

func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		// Attach as a child of the generated `version` command rather than
		// editing it, so a reprint of version.go keeps the wiring.
		for _, c := range root.Commands() {
			if c.Name() == "version" {
				addNovelCommandIfAbsent(c, newVersionDriftCmd(flags))
				return
			}
		}
		addNovelCommandIfAbsent(root, newVersionDriftCmd(flags))
	})
}

// Backend kinds the detector can report.
const (
	backendLlamaSwap  = "llama-swap"
	backendRouterMode = "llama.cpp-router-mode"
	backendUnknown    = "unknown"
)

// routerOnlyStatuses are model states that ONLY llama.cpp router mode emits.
// llama-swap reports loaded/unloaded; the router adds a download lifecycle and
// an idle-sleep state it uses in place of a TTL.
var routerOnlyStatuses = map[string]bool{
	"downloading": true,
	"downloaded":  true,
	"sleeping":    true,
	"loading":     true,
}

// backendProbe is the read-only verdict on what is listening.
type backendProbe struct {
	Kind string `json:"kind"`
	// Confidence is "confirmed" when a positive discriminator matched, and
	// "inconclusive" when the shape fit neither.
	Confidence string   `json:"confidence"`
	Evidence   []string `json:"evidence"`
	Version    string   `json:"version,omitempty"`
	Commit     string   `json:"commit,omitempty"`
	Warning    string   `json:"warning,omitempty"`
}

// rosterProbeEntry is /v1/models (or /models) with the fields both backends
// might carry.
type rosterProbeEntry struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
	Status  struct {
		Value string `json:"value"`
	} `json:"status"`
	Meta struct {
		Llamaswap *struct {
			Aliases []string `json:"aliases"`
		} `json:"llamaswap"`
		NCtx int `json:"n_ctx"`
	} `json:"meta"`
}

type rosterProbeEnvelope struct {
	Data   []rosterProbeEntry `json:"data"`
	Models []rosterProbeEntry `json:"models"`
}

// entries normalizes the two envelope spellings.
func (e rosterProbeEnvelope) entries() []rosterProbeEntry {
	if len(e.Data) > 0 {
		return e.Data
	}
	return e.Models
}

// probeBackend decides what is listening, READ-ONLY. It never loads a model:
// /v1/models and /api/version are both roster/metadata reads on either
// backend. In particular it does NOT touch the router's
// GET /metrics?model=, which upstream confirmed triggers an autoload
// (llama.cpp issue #23096, closed not-planned).
func probeBackend(ctx context.Context, flags *rootFlags, timeout time.Duration) backendProbe {
	p := backendProbe{Kind: backendUnknown, Confidence: "inconclusive"}

	var ver struct {
		Version   string `json:"version"`
		Commit    string `json:"commit"`
		BuildDate string `json:"build_date"`
	}
	versionErr := mcGetJSON(ctx, flags, "/api/version", timeout, &ver)
	if versionErr == nil && ver.Version != "" {
		p.Version, p.Commit = ver.Version, ver.Commit
		p.Evidence = append(p.Evidence, fmt.Sprintf("/api/version answered %s (commit %s) - a llama-swap endpoint; llama.cpp's server has no /api/version", ver.Version, ver.Commit))
	} else if versionErr != nil {
		p.Evidence = append(p.Evidence, "/api/version did not answer ("+versionErr.Error()+")")
	}

	var roster rosterProbeEnvelope
	rosterErr := mcGetJSON(ctx, flags, "/v1/models", timeout, &roster)
	if rosterErr != nil {
		p.Evidence = append(p.Evidence, "/v1/models did not answer ("+rosterErr.Error()+")")
		if versionErr == nil && ver.Version != "" {
			p.Kind, p.Confidence = backendLlamaSwap, "confirmed"
		}
		return p
	}

	entries := roster.entries()
	swapMeta, routerStatus := 0, map[string]bool{}
	for _, e := range entries {
		if e.Meta.Llamaswap != nil || e.OwnedBy == "llama-swap" {
			swapMeta++
		}
		if s := strings.ToLower(e.Status.Value); routerOnlyStatuses[s] {
			routerStatus[s] = true
		}
	}
	p.Evidence = append(p.Evidence, fmt.Sprintf("/v1/models returned %d entries, %d carrying llama-swap ownership metadata", len(entries), swapMeta))

	switch {
	case swapMeta > 0:
		p.Kind, p.Confidence = backendLlamaSwap, "confirmed"
	case len(routerStatus) > 0:
		states := make([]string, 0, len(routerStatus))
		for s := range routerStatus {
			states = append(states, s)
		}
		p.Kind, p.Confidence = backendRouterMode, "confirmed"
		p.Evidence = append(p.Evidence, "model status values "+strings.Join(states, "/")+" are router-mode lifecycle states; llama-swap reports only loaded/unloaded")
		p.Warning = routerModeWarning()
	case versionErr == nil && ver.Version != "":
		p.Kind, p.Confidence = backendLlamaSwap, "confirmed"
	case len(entries) > 0:
		p.Kind, p.Confidence = backendUnknown, "inconclusive"
		p.Evidence = append(p.Evidence, "an OpenAI-shaped /v1/models answered but carried neither llama-swap metadata nor a router lifecycle state; this may be a plain OpenAI-compatible server")
		p.Warning = "the backend could not be identified. This CLI's admin surface (unload, profiles, activity, config intelligence) is llama-swap-specific and will 404 against anything else."
	}
	return p
}

func routerModeWarning() string {
	return "llama.cpp ROUTER-MODE server detected, not llama-swap - this CLI's admin commands target llama-swap. " +
		"The read paths that overlap (/v1/models, /v1/chat/completions) work; the llama-swap-specific surface " +
		"(unload, profiles, activity, captures, config lint/drift, keep-set audit) does not exist here and will 404. " +
		"Router mode also has no TTL semantics (LRU plus idle-sleep only), and GET /metrics?model= triggers an AUTOLOAD " +
		"upstream, so a naive monitoring loop against it loads models - pass &autoload=false if you scrape it by hand."
}

// ---------------------------------------------------------------------------
// version drift
// ---------------------------------------------------------------------------

// pinnedSurfaceVersion is the llama-swap build this CLI's surface knowledge was
// written and verified against. It is read from the config-schema constant so
// there is ONE pin in the tree rather than a copy that can rot separately.
func pinnedSurfaceVersion() string { return lsconfig.SchemaLlamaSwapVersion }

// surfaceVersionPattern extracts the numeric part of a version like "v249".
var surfaceVersionPattern = regexp.MustCompile(`v?(\d+)`)

// surfaceDriftAffected maps a version distance to the commands most likely to be
// wrong. Only claims that follow from a KNOWN change are listed; the honest
// answer for an unknown newer build is "unverified surfaces", not a guess.
var surfaceDriftAffected = []doctorCapability{
	{212, "upstream metrics"},
	{226, "load"},
	{229, "captures, captures export"},
	{236, "config lint (store.path)"},
	{241, "profiles list, profiles activate"},
	{247, "server hardware"},
}

type surfaceDriftReport struct {
	SchemaVersion int          `json:"schema_version"`
	Pinned        string       `json:"cli_pinned_surface"`
	Live          string       `json:"live_server_version"`
	Commit        string       `json:"live_server_commit,omitempty"`
	Relation      string       `json:"relation"`
	Backend       backendProbe `json:"backend"`
	Affected      []string     `json:"possibly_affected_commands,omitempty"`
	Verdict       string       `json:"verdict"`
	Notes         []string     `json:"notes,omitempty"`
}

func newVersionDriftCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "drift",
		Aliases: []string{"check-drift"},
		Short:   "Compare the llama-swap surface this CLI was built against with the live server",
		Long: `Reports whether the server this CLI is pointed at is the build its surface
knowledge was written against.

The pin is otherwise invisible: the capability map, the config schema
validator, and the User-Agent all assume ` + lsconfig.SchemaLlamaSwapVersion + `. This command turns
that assumption into an asserted fact, and names the commands whose behaviour
depends on the difference when there is one.

It also reports WHICH BACKEND answered. llama.cpp's own llama-server has
shipped a native model-swapping router mode since Dec 2025 that serves a
similarly-shaped /models endpoint; pointing this CLI's admin commands at one
produces 404s that look like faults. The detector names it instead.

Read-only: /api/version and /v1/models only. No model is loaded.`,
		Example: `  llamaswap-pp-cli version drift
  llamaswap-pp-cli version drift --json`,
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": "0=in sync or newer, 4=proxy unreachable, 25=drift found (a finding, not an error)",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "version drift")
			}
			if err := validateDataSourceStrategy(flags, "live"); err != nil {
				return usageErr(err)
			}
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			timeout := mcTimeout(cmd, flags, 15*time.Second)

			rep := &surfaceDriftReport{SchemaVersion: 1, Pinned: pinnedSurfaceVersion()}
			rep.Backend = probeBackend(ctx, flags, timeout)
			rep.Live, rep.Commit = rep.Backend.Version, rep.Backend.Commit

			if rep.Backend.Kind == backendRouterMode {
				rep.Relation = "not-applicable"
				rep.Verdict = "the server is llama.cpp router mode, not llama-swap: there is no llama-swap version to compare against"
				rep.Notes = append(rep.Notes, rep.Backend.Warning)
				return mcEmit(cmd, flags, rep, func(w io.Writer) { surfaceDriftPrint(w, rep) })
			}
			if rep.Live == "" {
				return spineExitErr(ExitServerUnreachable, fmt.Errorf(
					"could not read /api/version, so surface drift cannot be assessed: %s", strings.Join(rep.Backend.Evidence, "; ")))
			}

			pinned, okP := surfaceVersionNumber(rep.Pinned)
			live, okL := surfaceVersionNumber(rep.Live)
			switch {
			case !okP || !okL:
				rep.Relation = "unparseable"
				rep.Verdict = fmt.Sprintf("could not compare %q with %q; both must look like vNNN", rep.Pinned, rep.Live)
			case live == pinned:
				rep.Relation = "in-sync"
				rep.Verdict = fmt.Sprintf("live server %s matches the surface this CLI was verified against", rep.Live)
			case live > pinned:
				rep.Relation = "server-ahead"
				rep.Verdict = fmt.Sprintf(
					"live server %s is NEWER than the %s surface this CLI was verified against by %d releases. Commands still work; endpoints or fields ADDED since %s are simply not used, and any field REMOVED since then would surface as a missing value rather than as an error",
					rep.Live, rep.Pinned, live-pinned, rep.Pinned)
				rep.Notes = append(rep.Notes, "unverified surfaces: anything added after "+rep.Pinned+" (for example meta.n_ctx on /v1/models, which this CLI uses opportunistically with a fallback)")
			default:
				rep.Relation = "server-behind"
				for _, feat := range surfaceDriftAffected {
					if live < feat.MinVersion {
						rep.Affected = append(rep.Affected, fmt.Sprintf("%s (needs v%d)", feat.Name, feat.MinVersion))
					}
				}
				rep.Verdict = fmt.Sprintf(
					"live server %s is OLDER than the %s surface this CLI was verified against by %d releases",
					rep.Live, rep.Pinned, pinned-live)
			}

			if err := mcEmit(cmd, flags, rep, func(w io.Writer) { surfaceDriftPrint(w, rep) }); err != nil {
				return err
			}
			if rep.Relation == "server-behind" {
				return &cliError{code: ExitDrift, err: fmt.Errorf("%s; affected: %s", rep.Verdict, mcJoinOrNone(rep.Affected))}
			}
			return nil
		},
	}
	return cmd
}

func surfaceVersionNumber(v string) (int, bool) {
	m := surfaceVersionPattern.FindStringSubmatch(v)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

func surfaceDriftPrint(w io.Writer, r *surfaceDriftReport) {
	fmt.Fprintf(w, "%s\n", bold("version drift"))
	fmt.Fprintf(w, "  backend         %s (%s)\n", r.Backend.Kind, r.Backend.Confidence)
	fmt.Fprintf(w, "  CLI pinned      %s\n", r.Pinned)
	fmt.Fprintf(w, "  live server     %s %s\n", r.Live, r.Commit)
	fmt.Fprintf(w, "  relation        %s\n", r.Relation)
	fmt.Fprintf(w, "  verdict         %s\n", r.Verdict)
	for _, a := range r.Affected {
		fmt.Fprintf(w, "  affected        %s\n", a)
	}
	for _, e := range r.Backend.Evidence {
		fmt.Fprintf(w, "  evidence        %s\n", e)
	}
	if r.Backend.Warning != "" {
		fmt.Fprintf(w, "  %s %s\n", yellow("warning:"), r.Backend.Warning)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "  note            %s\n", n)
	}
}

// ---------------------------------------------------------------------------
// meta.n_ctx fast path
// ---------------------------------------------------------------------------

// rosterNCtx reads meta.n_ctx from the roster listing. llama-swap exposes it
// on /v1/models when the seat declares capabilities.context (added after
// v249), which answers "what context is this model served at" WITHOUT the
// /upstream/{model}/props round trip - and, more importantly, without the
// auto-start that probing an unloaded model would trigger.
//
// Returns ok=false when the field is absent, which is the case on v249 and
// earlier. Callers MUST keep their existing derivation as the fallback: a
// fast path that silently returns zero would replace a measured context with
// a fabricated one.
func rosterNCtx(ctx context.Context, flags *rootFlags, model string, timeout time.Duration) (int, bool) {
	var roster rosterProbeEnvelope
	if err := mcGetJSON(ctx, flags, "/v1/models", timeout, &roster); err != nil {
		return 0, false
	}
	for _, e := range roster.entries() {
		if strings.EqualFold(e.ID, model) && e.Meta.NCtx > 0 {
			return e.Meta.NCtx, true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// doctor integration
// ---------------------------------------------------------------------------

// doctorCheckBackendKind reports what is actually listening. Registered in
// doctor_extras.go's check registry.
func doctorCheckBackendKind(ctx context.Context, flags *rootFlags) doctorFinding {
	f := doctorFinding{Check: "backend-kind", Severity: doctorOK}
	if cliutil.IsVerifyEnv() {
		f.Severity = doctorInfo
		f.Detail = "skipped under PRINTING_PRESS_VERIFY=1 (no live reads)"
		return f
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p := probeBackend(probeCtx, flags, 10*time.Second)
	switch p.Kind {
	case backendLlamaSwap:
		f.Detail = fmt.Sprintf("llama-swap %s confirmed (%s)", p.Version, p.Confidence)
	case backendRouterMode:
		f.Severity = doctorWarn
		f.Detail = routerModeWarning()
		f.Fix = "point this CLI at a llama-swap instance (--host), or use only the OpenAI-compatible read commands against router mode."
	default:
		f.Severity = doctorInfo
		f.Detail = "backend could not be identified: " + strings.Join(p.Evidence, "; ")
		if p.Warning != "" {
			f.Fix = p.Warning
		}
	}
	return f
}

// doctorCheckSurfaceDrift reports the pin-vs-live comparison as a finding, so
// `doctor` surfaces it without the operator having to know `version drift`
// exists.
func doctorCheckSurfaceDrift(ctx context.Context, flags *rootFlags) doctorFinding {
	f := doctorFinding{Check: "surface-drift", Severity: doctorOK}
	if cliutil.IsVerifyEnv() {
		f.Severity = doctorInfo
		f.Detail = "skipped under PRINTING_PRESS_VERIFY=1 (no live reads)"
		return f
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var ver struct {
		Version string `json:"version"`
	}
	if err := mcGetJSON(probeCtx, flags, "/api/version", 10*time.Second, &ver); err != nil {
		f.Severity = doctorInfo
		f.Detail = "could not read /api/version (" + err.Error() + "); surface drift unassessed"
		return f
	}
	pinned, okP := surfaceVersionNumber(pinnedSurfaceVersion())
	live, okL := surfaceVersionNumber(ver.Version)
	switch {
	case !okP || !okL:
		f.Severity = doctorInfo
		f.Detail = fmt.Sprintf("could not compare pinned %s with live %q", pinnedSurfaceVersion(), ver.Version)
	case live == pinned:
		f.Detail = fmt.Sprintf("live server %s matches the %s surface this CLI was verified against", ver.Version, pinnedSurfaceVersion())
	case live > pinned:
		f.Severity = doctorInfo
		f.Detail = fmt.Sprintf("live server %s is newer than the verified %s surface by %d releases; post-%s additions are unused, not broken",
			ver.Version, pinnedSurfaceVersion(), live-pinned, pinnedSurfaceVersion())
		f.Fix = "run `llamaswap-pp-cli version drift` for the detail."
	default:
		f.Severity = doctorWarn
		f.Detail = fmt.Sprintf("live server %s is OLDER than the verified %s surface by %d releases", ver.Version, pinnedSurfaceVersion(), pinned-live)
		f.Fix = "run `llamaswap-pp-cli version drift` to see which commands are affected."
	}
	return f
}
