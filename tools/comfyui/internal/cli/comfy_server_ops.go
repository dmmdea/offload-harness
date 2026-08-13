// Server-state operations: `free` (POST /free) and `features` (GET /features).
//
// NOT generated — markerless on purpose, so `printing-press generate --force`
// preserves it. Do not add the generated-file marker.
//
// Both endpoints are declared in the ComfyUI comms-routes docs and were absent
// from this CLI. They are grouped here because they are the two commands that
// describe and change the SERVER's own state rather than a render's.
//
// Contracts below were read from the ComfyUI server source (server.py
// post_free / get_features), not inferred from documentation, because both
// have behaviour the docs do not state: /free returns an EMPTY 200 body and
// its effect is asynchronous, and /features returns a flat capability map with
// one nested key.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"comfyui-pp-cli/internal/cliutil"
	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// free
// ---------------------------------------------------------------------------

// comfyFreeRequest is the POST /free body. Both fields default to false
// server-side; sending false for one is how you free memory without evicting
// the models, or vice versa.
type comfyFreeRequest struct {
	UnloadModels bool `json:"unload_models"`
	FreeMemory   bool `json:"free_memory"`
}

// comfyFreeResult is this command's own envelope. ComfyUI answers POST /free
// with a bare 200 and NO body, so there is nothing to echo — a result document
// has to be synthesised or a machine caller gets an empty stdout on success.
type comfyFreeResult struct {
	Action       string `json:"action"`
	Executed     bool   `json:"executed"`
	UnloadModels bool   `json:"unload_models"`
	FreeMemory   bool   `json:"free_memory"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	Note         string `json:"note"`
}

const comfyFreeAsyncNote = "ComfyUI answers /free with an empty 200 and acts on the request asynchronously: " +
	"it sets a queue flag that the execution loop honors, so VRAM is not guaranteed to be released the " +
	"instant this returns. Confirm with 'comfyui-pp-cli system stats' (per-device vram_free) rather than " +
	"assuming the 200 meant the memory is back."

func newComfyFreeCmd(flags *rootFlags) *cobra.Command {
	var (
		unloadModels bool
		freeMemory   bool
		execute      bool
	)

	cmd := &cobra.Command{
		Use:   "free",
		Short: "Ask ComfyUI to release VRAM — unload models and/or free cached memory (prints by default; --execute sends it)",
		Long: `Ask the running ComfyUI to give VRAM back: POST /free with
{"unload_models": ..., "free_memory": ...}.

WHY THIS EXISTS. On a box where ComfyUI and a model-serving proxy share the
same cards, this is the handoff primitive: it is how you make room for the
other tenant without restarting ComfyUI and losing its RAM-only history. It is
the counterpart to llamaswap-pp-cli's unload/keep-set on the other side of the
same two GPUs.

PRINTS BY DEFAULT. Releasing VRAM is disruptive to whatever else is using the
card, and a wrong call can evict a model another process is mid-render with.
So the default is to print exactly what would be sent and change nothing;
--execute is what actually posts it.

--unload-models  evict loaded models from VRAM.
--free-memory    free cached/reserved memory.
Both default to true, which is the "give me everything back" call. Set either
to false to be narrower.

ASYNCHRONOUS. The server replies with an empty 200 and sets a queue flag that
its execution loop honors; it does not free memory inline. Verify with
'comfyui-pp-cli system stats' rather than trusting the 200.

Exit codes:
  0   the request was printed (default) or accepted (--execute)
  2   usage error
  4   ComfyUI is unreachable
  5   the server refused the request`,
		Example: `  comfyui-pp-cli free
  comfyui-pp-cli free --execute
  comfyui-pp-cli free --unload-models --free-memory=false --execute
  comfyui-pp-cli free --execute --json`,
		Annotations: map[string]string{
			// Deliberately NO mcp:read-only: this evicts models another
			// process may be using. It must stay classified as a write so the
			// MCP surface annotates it destructive rather than safe.
			"pp:data-source":      "live",
			"pp:typed-exit-codes": "0,2,4,5",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			req := comfyFreeRequest{UnloadModels: unloadModels, FreeMemory: freeMemory}

			if !unloadModels && !freeMemory {
				return usageErr(fmt.Errorf(
					"nothing to do: --unload-models and --free-memory are both false, which posts a no-op the server ignores"))
			}

			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags,
					fmt.Sprintf("POST /free with unload_models=%t free_memory=%t", req.UnloadModels, req.FreeMemory))
			}

			// Print-by-default. Nothing is sent without --execute.
			if !execute {
				return comfyFreeEmit(cmd, flags, comfyFreeResult{
					Action:       "would-free",
					Executed:     false,
					UnloadModels: req.UnloadModels,
					FreeMemory:   req.FreeMemory,
					Note: "nothing was sent — re-run with --execute to post it. " +
						comfyFreeAsyncNote,
				})
			}

			// Verify mode short-circuits before the POST so a verification run
			// never evicts a real model from a real card.
			if cliutil.IsVerifyEnv() && !cliutil.IsVerifyLiveHTTPEnv() {
				return writeNoop(flags, "verify_short_circuit", "verify mode: no POST /free was issued")
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}
			_, statusCode, err := c.Post(cmd.Context(), "/free", req)
			if err != nil {
				return classifyAPIError(err, flags)
			}

			return comfyFreeEmit(cmd, flags, comfyFreeResult{
				Action:       "free",
				Executed:     true,
				UnloadModels: req.UnloadModels,
				FreeMemory:   req.FreeMemory,
				HTTPStatus:   statusCode,
				Note:         comfyFreeAsyncNote,
			})
		},
	}

	cmd.Flags().BoolVar(&unloadModels, "unload-models", true, "Evict loaded models from VRAM")
	cmd.Flags().BoolVar(&freeMemory, "free-memory", true, "Free cached/reserved memory")
	cmd.Flags().BoolVar(&execute, "execute", false, "Actually POST the request. Without this the call is printed and nothing is sent")

	return cmd
}

func comfyFreeEmit(cmd *cobra.Command, flags *rootFlags, result comfyFreeResult) error {
	if !wantsHumanTable(cmd.OutOrStdout(), flags) {
		return flags.printJSON(cmd, result)
	}
	w := cmd.OutOrStdout()
	if !result.Executed {
		fmt.Fprintf(w, "would POST /free  unload_models=%t  free_memory=%t\n", result.UnloadModels, result.FreeMemory)
		fmt.Fprintln(w, "  nothing was sent. Re-run with --execute.")
	} else {
		fmt.Fprintf(w, "POST /free accepted (HTTP %d)  unload_models=%t  free_memory=%t\n",
			result.HTTPStatus, result.UnloadModels, result.FreeMemory)
	}
	fmt.Fprintf(w, "  note: %s\n", comfyFreeAsyncNote)
	return nil
}

// ---------------------------------------------------------------------------
// features
// ---------------------------------------------------------------------------

// comfyPinnedAPIVersion is the ComfyUI version this CLI was generated and
// verified against. `features` compares the live capability map to the
// expectations below and reports drift instead of failing, because a newer
// server adding a flag is normal and must not break a pinned client.
const comfyPinnedAPIVersion = "0.32.0"

// comfyExpectedFeatures are the capability keys ComfyUI 0.32.0 serves from
// GET /features. Read from the server source (comfy_api/feature_flags.py,
// _CORE_FEATURE_FLAGS) rather than guessed, so "missing" really means the
// server dropped it.
//
// Deliberately a key list, not a key->value map: the VALUES are deployment
// state (max_upload_size follows a CLI arg, assets follows --enable-assets),
// so asserting them would report a local configuration choice as API drift.
// Only the SHAPE is a contract.
var comfyExpectedFeatures = []string{
	"assets",
	"extension",
	"max_upload_size",
	"node_replacements",
	"supports_model_type_tags",
	"supports_preview_metadata",
}

type comfyFeaturesResult struct {
	PinnedAPIVersion string         `json:"pinned_api_version"`
	Features         map[string]any `json:"features"`
	Known            []string       `json:"known"`
	Added            []string       `json:"added"`
	Missing          []string       `json:"missing"`
	Drift            bool           `json:"drift"`
	Verdict          string         `json:"verdict"`
	Note             string         `json:"note,omitempty"`
}

func newComfyFeaturesCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "features",
		Short: "Report the server's capability flags and any drift from the pinned ComfyUI version",
		Long: `Read GET /features — ComfyUI's capability negotiation map — and compare it to
the version this CLI was built against (` + comfyPinnedAPIVersion + `).

WHY THIS EXISTS. "This CLI is pinned to ComfyUI ` + comfyPinnedAPIVersion + `" is
otherwise an invisible assumption. This turns it into an asserted, reported
fact: a capability the server no longer serves is exactly the shape of a
breaking upgrade, and it is far cheaper to learn that here than from a
mysterious failure three commands later.

WHAT IS AND IS NOT COMPARED. Only the SHAPE — which capability keys exist — is
treated as a contract. The VALUES are deployment state: max_upload_size follows
the server's --max-upload-size, and assets follows --enable-assets. Asserting
those would report a local configuration choice as API drift.

added   keys this server serves that ` + comfyPinnedAPIVersion + ` did not.
        Informational: a newer server is allowed to grow.
missing keys ` + comfyPinnedAPIVersion + ` served that this server does not.
        This is the one that matters — something this CLI may rely on is gone.

Drift is reported, never treated as a failure: exit stays 0 so this is safe in
a pipeline. 'comfyui-pp-cli doctor' surfaces the same finding alongside the
rest of the environment check.

Exit codes:
  0   features were read (with or without drift)
  2   usage error
  4   ComfyUI is unreachable`,
		Example: `  comfyui-pp-cli features
  comfyui-pp-cli features --json
  comfyui-pp-cli features --agent`,
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:data-source":      "live",
			"pp:typed-exit-codes": "0,2,4",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "GET /features")
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			data, err := c.Get(cmd.Context(), "/features", map[string]string{})
			if err != nil {
				return classifyAPIError(err, flags)
			}
			result, err := comfyBuildFeaturesResult(data)
			if err != nil {
				return err
			}
			return comfyFeaturesEmit(cmd, flags, result)
		},
	}

	return cmd
}

// comfyBuildFeaturesResult is split out from RunE so the drift comparison is
// testable without a server.
func comfyBuildFeaturesResult(data json.RawMessage) (comfyFeaturesResult, error) {
	result := comfyFeaturesResult{
		PinnedAPIVersion: comfyPinnedAPIVersion,
		Features:         map[string]any{},
		Known:            append([]string(nil), comfyExpectedFeatures...),
		Added:            []string{},
		Missing:          []string{},
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &result.Features); err != nil {
			return result, apiErr(fmt.Errorf(
				"GET /features did not return a JSON object: %s", strings.TrimSpace(truncate(string(data), 200))))
		}
	}

	expected := make(map[string]bool, len(comfyExpectedFeatures))
	for _, k := range comfyExpectedFeatures {
		expected[k] = true
	}
	for k := range result.Features {
		if !expected[k] {
			result.Added = append(result.Added, k)
		}
	}
	for _, k := range comfyExpectedFeatures {
		if _, ok := result.Features[k]; !ok {
			result.Missing = append(result.Missing, k)
		}
	}
	sort.Strings(result.Added)
	sort.Strings(result.Missing)

	result.Drift = len(result.Added) > 0 || len(result.Missing) > 0
	switch {
	case len(result.Missing) > 0:
		result.Verdict = "DRIFT"
		result.Note = fmt.Sprintf(
			"this server does not serve %d capability key(s) that ComfyUI %s did. Anything in this CLI relying on them may fail.",
			len(result.Missing), comfyPinnedAPIVersion)
	case len(result.Added) > 0:
		result.Verdict = "NEWER"
		result.Note = fmt.Sprintf(
			"this server serves %d capability key(s) ComfyUI %s did not. Additive; nothing to do.",
			len(result.Added), comfyPinnedAPIVersion)
	default:
		result.Verdict = "MATCH"
	}
	return result, nil
}

func comfyFeaturesEmit(cmd *cobra.Command, flags *rootFlags, result comfyFeaturesResult) error {
	if !wantsHumanTable(cmd.OutOrStdout(), flags) {
		return flags.printJSON(cmd, result)
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "%s  (pinned to ComfyUI %s)\n", result.Verdict, result.PinnedAPIVersion)
	keys := make([]string, 0, len(result.Features))
	for k := range result.Features {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "  %-28s %v\n", k, result.Features[k])
	}
	if len(result.Added) > 0 {
		fmt.Fprintf(w, "  added:   %s\n", strings.Join(result.Added, ", "))
	}
	if len(result.Missing) > 0 {
		fmt.Fprintf(w, "  MISSING: %s\n", strings.Join(result.Missing, ", "))
	}
	if result.Note != "" {
		fmt.Fprintf(w, "  note: %s\n", result.Note)
	}
	return nil
}

// comfyFeatureDriftFinding is the doctor-facing summary of the same check.
// Returns an empty string when features could not be read, so doctor reports
// the read failure it already has rather than inventing a drift verdict.
func comfyFeatureDriftFinding(ctx context.Context, flags *rootFlags) string {
	c, err := flags.newClient()
	if err != nil {
		return ""
	}
	data, err := c.Get(ctx, "/features", map[string]string{})
	if err != nil {
		return ""
	}
	result, err := comfyBuildFeaturesResult(data)
	if err != nil {
		return ""
	}
	switch result.Verdict {
	case "DRIFT":
		return fmt.Sprintf("error: server does not serve %s — expected by the pinned ComfyUI %s",
			strings.Join(result.Missing, ", "), comfyPinnedAPIVersion)
	case "NEWER":
		return fmt.Sprintf("ok (newer than the pinned %s; adds %s)",
			comfyPinnedAPIVersion, strings.Join(result.Added, ", "))
	default:
		return fmt.Sprintf("ok (matches the pinned ComfyUI %s)", comfyPinnedAPIVersion)
	}
}
