// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored novel command family: the config surface.
// pp:data-source local
// Supported strategies: auto, local, live, or computed. `config` reads the
// YAML on disk; the two subcommands that also read the live proxy (drift,
// and backup's orphan check) declare their own source.

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/client"
	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/lsconfig"
)

// init wires the config and bind families onto the root command through the
// generated novel-command hook, so a `printing-press generate --force` regen
// refreshes root.go without dropping this registration and without this file
// needing to edit generated code.
func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		addNovelCommandIfAbsent(root, newConfigCmd(flags))
		addNovelCommandIfAbsent(root, newBindCmd(flags))
	})
}

// theTrustContract is printed by `config --help` and by every command that
// could be mistaken for a writer. It is not decoration: the whole reason this
// family exists as read-only tooling is that programs writing this file have
// caused real outages on this class of deployment.
const theTrustContract = `The live llama-swap YAML is READ-ONLY to this CLI, permanently.

Its comments are the operator's decision journal — which flags are load-bearing,
what was tried and reverted, and why. A program that re-marshals the file to
"just change one flag" destroys that journal and, historically, silently drops
load-bearing flags along with it.

So: no command here writes the config. Not with a backup first, not atomically,
not behind --force. 'config apply' prints the change and the exact restart
command for a human to run. The only files this family ever creates are NEW
content-addressed backups plus their sidecar index.`

func newConfigCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Read-only intelligence about the llama-swap YAML: validate, lint, explain, diff, drift, backup, test, apply-plan.",
		Long:  "Read-only intelligence about the llama-swap YAML.\n\n" + theTrustContract,
		Example: "  llamaswap-pp-cli config lint --json\n" +
			"  llamaswap-pp-cli config explain gemma-4-e4b\n" +
			"  llamaswap-pp-cli config drift",
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:parent-group":     "true",
			"pp:typed-exit-codes": "0,2",
		},
		RunE: parentNoSubcommandRunE(flags),
	}
	cmd.AddCommand(newConfigValidateCmd(flags))
	cmd.AddCommand(newConfigLintCmd(flags))
	cmd.AddCommand(newConfigExplainCmd(flags))
	cmd.AddCommand(newConfigDiffCmd(flags))
	cmd.AddCommand(newConfigDriftCmd(flags))
	cmd.AddCommand(newConfigBackupCmd(flags))
	cmd.AddCommand(newConfigTestInstanceCmd(flags))
	cmd.AddCommand(newConfigApplyCmd(flags))
	return cmd
}

// ---------------------------------------------------------------- shared

// resolveConfigPath returns args[idx] when present, otherwise the live config
// discovered from LLAMASWAP_YAML / the default candidates.
func resolveConfigPath(args []string, idx int) (string, error) {
	if len(args) > idx && strings.TrimSpace(args[idx]) != "" {
		return args[idx], nil
	}
	p, err := lsconfig.DefaultConfigPath()
	if err != nil {
		return "", usageErr(err)
	}
	return p, nil
}

// loadConfigFile parses a config, mapping a parse failure onto the typed
// config-invalid exit code rather than a generic error.
func loadConfigFile(path string) (*lsconfig.File, error) {
	f, err := lsconfig.Load(path, lsconfig.LoadOptions{})
	if err != nil {
		return nil, &cliError{code: ExitConfigInvalid, err: err}
	}
	return f, nil
}

// loopbackBaseURL returns the proxy base URL with any `localhost` host
// rewritten to the 127.0.0.1 literal.
//
// This is not pedantry. On a dual-stack Windows host `localhost` resolves ::1
// first; when the proxy binds IPv4 only, every request eats the full IPv6
// connect timeout before falling back — a ~21s stall per call that has been
// misread as "the server is down" more than once. Every live read in this
// family goes through here.
func loopbackBaseURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := u.Hostname()
	if !strings.EqualFold(host, "localhost") {
		return raw
	}
	if p := u.Port(); p != "" {
		u.Host = "127.0.0.1:" + p
	} else {
		u.Host = "127.0.0.1"
	}
	return u.String()
}

// newLoopbackClient builds the generated client with the loopback discipline
// applied. Read-only GETs only; nothing in this family mutates server state.
func newLoopbackClient(flags *rootFlags) (*client.Client, error) {
	c, err := flags.newClient()
	if err != nil {
		return nil, err
	}
	c.BaseURL = loopbackBaseURL(c.BaseURL)
	return c, nil
}

// runningSeat is one entry of GET /running. The `cmd` string is the ground
// truth for what a seat is ACTUALLY running: it is the process's own argv as
// llama-swap spawned it, after macro expansion and port assignment.
type runningSeat struct {
	Model string `json:"model"`
	State string `json:"state"`
	Cmd   string `json:"cmd"`
	Proxy string `json:"proxy"`
	TTL   int    `json:"ttl"`
	Name  string `json:"name"`
}

type runningEnvelope struct {
	Running []runningSeat `json:"running"`
}

// fetchRunning reads GET /running with caching disabled. A cached answer would
// make `config drift` report a stale process state as current, which is the
// one failure this command must never have.
func fetchRunning(ctx context.Context, flags *rootFlags) ([]runningSeat, error) {
	c, err := newLoopbackClient(flags)
	if err != nil {
		return nil, err
	}
	data, err := c.GetWithHeadersNoCache(ctx, "/running", nil, nil)
	if err != nil {
		return nil, &cliError{code: ExitServerUnreachable, err: fmt.Errorf("read /running from %s: %w", c.BaseURL, err)}
	}
	var env runningEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		// Older builds answered with a bare array.
		var bare []runningSeat
		if err2 := json.Unmarshal(data, &bare); err2 != nil {
			return nil, fmt.Errorf("decode /running: %w", err)
		}
		return bare, nil
	}
	return env.Running, nil
}

// modelEntry is one entry of GET /v1/models, with the alias metadata
// llama-swap attaches under meta.llamaswap.
type modelEntry struct {
	ID   string `json:"id"`
	Meta struct {
		LlamaSwap struct {
			Aliases []string `json:"aliases"`
		} `json:"llamaswap"`
	} `json:"meta"`
}

// fetchRosterAliases returns id -> aliases for every listed model, plus a flat
// resolution map (id and every alias -> canonical id).
func fetchRosterAliases(ctx context.Context, flags *rootFlags) ([]modelEntry, map[string]string, error) {
	c, err := newLoopbackClient(flags)
	if err != nil {
		return nil, nil, err
	}
	data, err := c.GetWithHeadersNoCache(ctx, "/v1/models", nil, nil)
	if err != nil {
		return nil, nil, &cliError{code: ExitServerUnreachable, err: fmt.Errorf("read /v1/models from %s: %w", c.BaseURL, err)}
	}
	var env struct {
		Data []modelEntry `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, nil, fmt.Errorf("decode /v1/models: %w", err)
	}
	resolve := map[string]string{}
	for _, m := range env.Data {
		resolve[m.ID] = m.ID
		for _, a := range m.Meta.LlamaSwap.Aliases {
			resolve[a] = m.ID
		}
	}
	return env.Data, resolve, nil
}

// wantsJSON reports whether the caller asked for machine output.
func wantsJSON(w io.Writer, flags *rootFlags) bool {
	if flags == nil {
		return false
	}
	if flags.asJSON || flags.agent {
		return true
	}
	return !isTerminal(w) && !flags.csv && !flags.quiet && !flags.plain
}

// severityMark renders a lint severity for the human table.
func severityMark(s lsconfig.Severity) string {
	switch s {
	case lsconfig.SevError:
		return red("ERROR")
	case lsconfig.SevWarn:
		return yellow("WARN ")
	case lsconfig.SevInfo:
		return "INFO "
	case lsconfig.SevSkipped:
		return "SKIP "
	}
	return string(s)
}

// fprintBlock writes a titled, indented text block for human output.
func fprintBlock(w io.Writer, title, body string) {
	if strings.TrimSpace(body) == "" {
		return
	}
	fmt.Fprintf(w, "\n%s\n", bold(title))
	for _, ln := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		fmt.Fprintf(w, "  %s\n", ln)
	}
}

// configFileLines returns a file's source lines for unified-diff rendering.
func configFileLines(f *lsconfig.File) []string {
	out := make([]string, len(f.Lines))
	copy(out, f.Lines)
	return out
}

// errConfigInvalid wraps err with the typed config-invalid exit code.
func errConfigInvalid(err error) error { return &cliError{code: ExitConfigInvalid, err: err} }

// errDrift wraps err with the typed drift exit code. Drift is a FINDING, not a
// failure: the command completed and answered correctly.
func errDrift(err error) error { return &cliError{code: ExitDrift, err: err} }

// errPortConflict wraps err with the typed port-conflict exit code.
func errPortConflict(err error) error { return &cliError{code: ExitPortConflict, err: err} }

// errModelNotFound wraps err with the typed model-not-found exit code.
func errModelNotFound(err error) error { return &cliError{code: ExitModelNotFound, err: err} }

// verifyPlan prints what a side-effecting command WOULD do and returns true
// when the caller should stop. Two gates, deliberately separate:
//   - the printing-press verifier's env var, so a verify pass never spawns a
//     process or writes a backup file;
//   - --dry-run, the operator-facing equivalent.
func verifyPlan(w io.Writer, flags *rootFlags, action string, plan []string) bool {
	if !cliutilIsVerifyEnv() && !dryRunOK(flags) {
		return false
	}
	reason := "dry-run"
	if cliutilIsVerifyEnv() {
		reason = "verify-mode"
	}
	if wantsJSON(w, flags) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"dry_run": true,
			"reason":  reason,
			"action":  action,
			"plan":    plan,
		})
		return true
	}
	fmt.Fprintf(w, "%s: would %s\n", reason, action)
	for _, p := range plan {
		fmt.Fprintf(w, "  %s\n", p)
	}
	return true
}

// cliutilIsVerifyEnv is a tiny indirection so tests can exercise the gate
// without setting a process-wide env var.
var cliutilIsVerifyEnv = func() bool { return cliutil.IsVerifyEnv() }

// shortSha returns a 16-hex-character sha256 prefix of s. Used to give a
// command line a stable identity that can be joined against a benchmark row
// without storing the whole string twice.
func shortSha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}
