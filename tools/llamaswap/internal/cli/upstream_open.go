// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave D final glue): print (or open) a seat's passthrough URL.
// Attaches to the GENERATED `upstream` command as a subcommand; that file is
// not modified.
// pp:data-source live

package cli

import (
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/cliutil"
)

// glueUpstreamOpenReport is the command envelope.
type glueUpstreamOpenReport struct {
	SchemaVersion int    `json:"schema_version"`
	Action        string `json:"action"`
	Requested     string `json:"requested"`
	Model         string `json:"model"`
	URL           string `json:"url"`
	// Running reports whether the seat currently holds VRAM. Opening the URL
	// for a stopped seat STARTS it, so this is the fact the caller needs
	// before clicking.
	Running  bool     `json:"running"`
	Launched bool     `json:"launched"`
	Notes    []string `json:"notes,omitempty"`
}

func newGlueUpstreamOpenCmd(flags *rootFlags) *cobra.Command {
	var flagLaunch bool

	cmd := &cobra.Command{
		Use:   "open <model|alias>",
		Short: "Print a model's llama.cpp passthrough URL — or open it in a browser with --launch.",
		Long: strings.Trim(`
Every seat's own llama-server web UI and API live behind
http://<proxy>/upstream/<model>/. This resolves the alias, checks whether the
seat is loaded, and prints that URL.

PRINTING is the default, and deliberately so. Opening a browser is a visible
side effect on someone's desktop; a command that does it unasked is a command
you stop trusting in a script. --launch opts in.

The warning that matters: requesting ANY /upstream path for a stopped model
makes llama-swap START it — a multi-GB load triggered by what looks like
browsing. The report always says whether the seat is currently running, so
clicking is an informed decision.

Exit codes: 2 usage, 3 model not found, 4 server unreachable.`, "\n"),
		Example: strings.Trim(`
  # Print the URL (safe; nothing is started)
  llamaswap-pp-cli upstream open gemma-4-e2b

  # Aliases resolve
  llamaswap-pp-cli upstream open local-embed --json

  # Actually open a browser
  llamaswap-pp-cli upstream open gemma-4-e2b --launch
`, "\n"),
		Annotations: map[string]string{
			"pp:data-source":      "live",
			"pp:typed-exit-codes": "0,2,3,4",
			// The default path only reads and prints. --launch is the opt-in
			// side effect and short-circuits under verify.
			"mcp:read-only": "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) > 0 {
				target = strings.TrimSpace(args[0])
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "upstream open "+target)
			}
			if target == "" {
				return glueUsageErrf("%s requires a model id or alias; run %q for usage", cmd.CommandPath(), cmd.CommandPath()+" --help")
			}
			return glueRunUpstreamOpen(cmd, flags, target, flagLaunch)
		},
	}
	cmd.Flags().BoolVar(&flagLaunch, "launch", false, "Actually open the URL in the default browser instead of only printing it.")
	return cmd
}

func glueRunUpstreamOpen(cmd *cobra.Command, flags *rootFlags, target string, launch bool) error {
	ctx := cmd.Context()
	base, _ := spineBaseURL(flags)
	rep := &glueUpstreamOpenReport{SchemaVersion: glueSchemaVersion, Action: "print_url", Requested: target}

	c, err := glueClient(flags)
	if err != nil {
		return err
	}
	entry, err := glueResolve(ctx, c, target)
	if err != nil {
		return err
	}
	rep.Model = entry.ID
	rep.URL = strings.TrimRight(base, "/") + "/upstream/" + entry.ID + "/"

	running, rerr := c.Running(ctx)
	if rerr != nil {
		rep.Notes = append(rep.Notes, "could not read /running ("+rerr.Error()+"); load state unknown")
	} else {
		rep.Running = glueIsRunning(running, entry.ID)
	}
	if !rep.Running {
		rep.Notes = append(rep.Notes,
			"this seat is NOT loaded: opening the URL will make llama-swap start it (a multi-GB load). Use 'load "+entry.ID+" --wait' if that is the intent.")
	}

	if launch {
		// The side-effect convention: a verify pass reports the intent and
		// stops, so a verifier never spawns a browser on someone's desktop.
		if cliutil.IsVerifyEnv() {
			rep.Action = "would_launch"
			rep.Notes = append(rep.Notes, "PRINTING_PRESS_VERIFY=1: no browser was launched")
		} else if lerr := glueOpenBrowser(rep.URL); lerr != nil {
			rep.Notes = append(rep.Notes, "could not launch a browser: "+lerr.Error())
		} else {
			rep.Action = "launched"
			rep.Launched = true
		}
	}

	return mcEmit(cmd, flags, rep, func(w io.Writer) {
		fmt.Fprintln(w, rep.URL)
		fmt.Fprintf(w, "model:   %s (running: %v)\n", rep.Model, rep.Running)
		for _, n := range rep.Notes {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	})
}

// glueOpenBrowser opens a URL with the platform's default handler, without a
// visible console window on Windows (house rule: no flashing consoles).
func glueOpenBrowser(target string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case "darwin":
		c = exec.Command("open", target)
	default:
		c = exec.Command("xdg-open", target)
	}
	hideSpawnedWindow(c)
	return c.Start()
}
