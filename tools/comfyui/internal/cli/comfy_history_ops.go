// history clear / history delete — the write half of POST /history.
//
// NOT generated — markerless on purpose, so `printing-press generate --force`
// preserves it. Do not add the generated-file marker.
//
// The generated `history` group shipped list/get only, so the queue family had
// clear/get/interrupt while history had no way to drop anything. These two
// leaves close that gap.
//
// THE STANDING WARNING. ComfyUI's history is a RAM dict. It is the ONLY source
// of honest per-prompt timing, it is destroyed on every restart, and these two
// commands destroy it on purpose. Both therefore refuse to run without
// --execute, and both point at `sync-history` first — a cleared history that
// was never synced is gone, and no amount of re-rendering reconstructs the
// original timings.
//
// Contract read from the ComfyUI server source (server.py post_history):
// {"clear": true} wipes everything; {"delete": [id, ...]} drops named entries;
// the reply is a bare 200 with NO body.

package cli

import (
	"fmt"
	"strings"

	"comfyui-pp-cli/internal/cliutil"
	"github.com/spf13/cobra"
)

// comfyHistoryMutateResult is this command's synthesised envelope — POST
// /history answers with an empty 200, so there is nothing to echo back.
type comfyHistoryMutateResult struct {
	Action     string   `json:"action"`
	Executed   bool     `json:"executed"`
	PromptIDs  []string `json:"prompt_ids,omitempty"`
	Count      int      `json:"count"`
	HTTPStatus int      `json:"http_status,omitempty"`
	Note       string   `json:"note"`
}

const comfyHistoryLossNote = "ComfyUI's history is RAM-only and is the only source of honest per-prompt " +
	"timings. Run 'comfyui-pp-cli sync-history' first if those runs are not in the local store yet — once " +
	"dropped they cannot be reconstructed."

func newComfyHistoryClearCmd(flags *rootFlags) *cobra.Command {
	var execute bool

	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Wipe the server's entire prompt history (destructive; --execute required)",
		Long: `POST /history {"clear": true} — wipe every completed prompt record the server
is holding.

DESTRUCTIVE AND UNRECOVERABLE. ComfyUI's history lives in RAM and is the only
place per-prompt execution timings exist. This command destroys all of it.
` + comfyHistoryLossNote + `

Prints what it would do and changes nothing unless --execute is passed.

Exit codes:
  0   the request was printed (default) or accepted (--execute)
  2   usage error
  4   ComfyUI is unreachable
  5   the server refused the request`,
		Example: `  comfyui-pp-cli history clear
  comfyui-pp-cli sync-history && comfyui-pp-cli history clear --execute
  comfyui-pp-cli history clear --execute --json`,
		Annotations: map[string]string{
			// No mcp:read-only — this destroys server state.
			"pp:data-source":      "live",
			"pp:typed-exit-codes": "0,2,4,5",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, `POST /history {"clear": true}`)
			}
			if !execute {
				return comfyHistoryMutateEmit(cmd, flags, comfyHistoryMutateResult{
					Action:   "would-clear",
					Executed: false,
					Note:     "nothing was sent — re-run with --execute to wipe the server's history. " + comfyHistoryLossNote,
				})
			}
			if cliutil.IsVerifyEnv() && !cliutil.IsVerifyLiveHTTPEnv() {
				return writeNoop(cmd.OutOrStdout(), flags, "verify_short_circuit", "verify mode: no POST /history was issued")
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			_, statusCode, err := c.Post(cmd.Context(), "/history", map[string]any{"clear": true})
			if err != nil {
				return classifyAPIError(cmd.OutOrStdout(), err, flags)
			}
			return comfyHistoryMutateEmit(cmd, flags, comfyHistoryMutateResult{
				Action:     "clear",
				Executed:   true,
				HTTPStatus: statusCode,
				Note:       "the server's prompt history was wiped. " + comfyHistoryLossNote,
			})
		},
	}

	cmd.Flags().BoolVar(&execute, "execute", false, "Actually wipe the history. Without this the call is printed and nothing is sent")
	return cmd
}

func newComfyHistoryDeleteCmd(flags *rootFlags) *cobra.Command {
	var execute bool

	cmd := &cobra.Command{
		Use:   "delete <prompt_id> [<prompt_id> ...]",
		Short: "Drop specific prompts from the server's history (destructive; --execute required)",
		Long: `POST /history {"delete": [...]} — drop named prompt records from the server's
in-memory history, leaving the rest intact.

DESTRUCTIVE AND UNRECOVERABLE for the named ids.
` + comfyHistoryLossNote + `

The server does not report which ids it actually found: it answers 200 whether
an id existed or not. So a 200 here means "the request was accepted", never
"every id you named was present". Confirm with 'comfyui-pp-cli history get
<prompt_id>' if that distinction matters.

Prints what it would do and changes nothing unless --execute is passed.

Exit codes:
  0   the request was printed (default) or accepted (--execute)
  2   usage error, including no prompt_id given
  4   ComfyUI is unreachable
  5   the server refused the request`,
		Example: `  comfyui-pp-cli history delete 6f0e5c4a-1d2b-4a5e-9c88-2f1b7a0d3e44
  comfyui-pp-cli history delete 6f0e5c4a-1d2b-4a5e-9c88-2f1b7a0d3e44 --execute
  comfyui-pp-cli history delete id-one id-two --execute --json`,
		Annotations: map[string]string{
			"pp:data-source":      "live",
			"pp:typed-exit-codes": "0,2,4,5",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ids := make([]string, 0, len(args))
			for _, a := range args {
				if trimmed := strings.TrimSpace(a); trimmed != "" {
					ids = append(ids, trimmed)
				}
			}
			if len(ids) == 0 {
				return usageErr(fmt.Errorf(
					"no prompt_id given. Name at least one, or use 'history clear' to wipe everything"))
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags,
					fmt.Sprintf(`POST /history {"delete": [%s]}`, strings.Join(ids, ", ")))
			}
			if !execute {
				return comfyHistoryMutateEmit(cmd, flags, comfyHistoryMutateResult{
					Action:    "would-delete",
					Executed:  false,
					PromptIDs: ids,
					Count:     len(ids),
					Note:      "nothing was sent — re-run with --execute to drop these records. " + comfyHistoryLossNote,
				})
			}
			if cliutil.IsVerifyEnv() && !cliutil.IsVerifyLiveHTTPEnv() {
				return writeNoop(cmd.OutOrStdout(), flags, "verify_short_circuit", "verify mode: no POST /history was issued")
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			_, statusCode, err := c.Post(cmd.Context(), "/history", map[string]any{"delete": ids})
			if err != nil {
				return classifyAPIError(cmd.OutOrStdout(), err, flags)
			}
			return comfyHistoryMutateEmit(cmd, flags, comfyHistoryMutateResult{
				Action:     "delete",
				Executed:   true,
				PromptIDs:  ids,
				Count:      len(ids),
				HTTPStatus: statusCode,
				Note: "the request was accepted. The server answers 200 whether or not an id existed, " +
					"so this does not confirm every id was present.",
			})
		},
	}

	cmd.Flags().BoolVar(&execute, "execute", false, "Actually delete the records. Without this the call is printed and nothing is sent")
	return cmd
}

func comfyHistoryMutateEmit(cmd *cobra.Command, flags *rootFlags, result comfyHistoryMutateResult) error {
	if !wantsHumanTable(cmd.OutOrStdout(), flags) {
		return flags.printJSON(cmd, result)
	}
	w := cmd.OutOrStdout()
	switch result.Action {
	case "would-clear":
		fmt.Fprintln(w, "would POST /history {\"clear\": true} — the ENTIRE server history")
		fmt.Fprintln(w, "  nothing was sent. Re-run with --execute.")
	case "clear":
		fmt.Fprintf(w, "history wiped (HTTP %d)\n", result.HTTPStatus)
	case "would-delete":
		fmt.Fprintf(w, "would POST /history delete for %d prompt(s):\n", result.Count)
		for _, id := range result.PromptIDs {
			fmt.Fprintf(w, "  %s\n", id)
		}
		fmt.Fprintln(w, "  nothing was sent. Re-run with --execute.")
	case "delete":
		fmt.Fprintf(w, "delete accepted for %d prompt(s) (HTTP %d)\n", result.Count, result.HTTPStatus)
	}
	fmt.Fprintf(w, "  note: %s\n", result.Note)
	return nil
}
