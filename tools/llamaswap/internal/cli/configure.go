// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave D final glue): emit ready-to-paste client configuration.
// pp:data-source live

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/lsconfig"
)

// glueConfigureModel is one roster entry as a client needs to see it.
type glueConfigureModel struct {
	// ID is the string a client must put in its "model" field.
	ID string `json:"id"`
	// Aliases are the other spellings that reach the same seat.
	Aliases []string `json:"aliases,omitempty"`
	// ContextLength is the seat's -c / --ctx-size from the llama-swap config,
	// which is the number a client's own context budget must respect. Zero
	// means it could not be established from the config — reported as unknown
	// rather than guessed, because a wrong context length here silently
	// truncates real prompts.
	ContextLength int `json:"context_length,omitempty"`
	// Role is chat, embedding, rerank, transcribe, or unknown, derived from
	// the seat's own flags. A chat client pointed at an embedding seat gets
	// nothing but errors, so the role decides which seat may be the default.
	Role string `json:"role"`
	// Loaded reports whether the seat currently holds VRAM.
	Loaded bool `json:"loaded"`
}

// Seat roles. Derived from the seat's llama-server flags, not from its name:
// a naming convention is a hint, --embeddings is a fact.
const (
	glueRoleChat       = "chat"
	glueRoleEmbedding  = "embedding"
	glueRoleRerank     = "rerank"
	glueRoleTranscribe = "transcribe"
	glueRoleUnknown    = "unknown"
)

// glueConfigureReport is the command envelope.
type glueConfigureReport struct {
	SchemaVersion int    `json:"schema_version"`
	Action        string `json:"action"`
	Target        string `json:"target"`
	BaseURL       string `json:"base_url"`
	// APIBase is the OpenAI-compatible base a client should be pointed at.
	APIBase string               `json:"api_base"`
	Models  []glueConfigureModel `json:"models"`
	// Snippet is the ready-to-paste configuration text.
	Snippet string   `json:"snippet"`
	Notes   []string `json:"notes,omitempty"`
}

func newGlueConfigureCmd(flags *rootFlags) *cobra.Command {
	var flagModel string

	cmd := &cobra.Command{
		Use:   "configure <claude-code|generic>",
		Short: "Emit ready-to-paste client configuration pointing at this llama-swap.",
		Long: strings.Trim(`
Print the configuration a client needs to talk to this proxy: the
OpenAI-compatible base URL, the model ids that are actually served, and each
seat's context length.

PRINT ONLY. It never edits a client's settings file. Which file, which profile,
and whether to merge or replace are decisions with real blast radius on a
working setup, and they belong to the person who owns that setup.

Two targets:

  claude-code   an ANTHROPIC_BASE_URL / model-id environment block
  generic       an OpenAI-compatible base_url + api_key + model block

Context lengths come from the llama-swap YAML (-c / --ctx-size), not from a
default: a seat with no explicit -c is reported as unknown rather than as 4096,
because a fabricated context length silently truncates prompts.

Exit codes: 2 usage, 4 server unreachable.`, "\n"),
		Example: strings.Trim(`
  # An environment block for Claude Code
  llamaswap-pp-cli configure claude-code

  # A generic OpenAI-compatible client, pinned to one seat
  llamaswap-pp-cli configure generic --model gemma-4-31b-agent

  # Machine-readable, including per-model context lengths
  llamaswap-pp-cli configure generic --json
`, "\n"),
		Annotations: map[string]string{
			"pp:data-source":      "live",
			"pp:typed-exit-codes": "0,2,4",
			"mcp:read-only":       "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "generic"
			if len(args) > 0 {
				target = strings.ToLower(strings.TrimSpace(args[0]))
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "configure "+target)
			}
			switch target {
			case "claude-code", "generic":
			default:
				return glueUsageErrf("unknown target %q; valid targets are claude-code and generic", target)
			}
			return glueRunConfigure(cmd, flags, target, strings.TrimSpace(flagModel))
		},
	}
	cmd.Flags().StringVar(&flagModel, "model", "", "Pin the emitted snippet to one model id or alias instead of listing the roster.")
	return cmd
}

func glueRunConfigure(cmd *cobra.Command, flags *rootFlags, target, pin string) error {
	ctx := cmd.Context()
	base, _ := spineBaseURL(flags)
	rep := &glueConfigureReport{
		SchemaVersion: glueSchemaVersion,
		Action:        "configure",
		Target:        target,
		BaseURL:       base,
		APIBase:       strings.TrimRight(base, "/") + "/v1",
	}

	if cliutil.IsVerifyEnv() {
		rep.Notes = append(rep.Notes, "PRINTING_PRESS_VERIFY=1: the roster was not read; snippet reflects the configured base URL only")
		rep.Snippet = glueConfigureSnippet(target, rep.APIBase, nil, pin)
		return printJSONFiltered(cmd.OutOrStdout(), rep, flags)
	}

	c, err := glueClient(flags)
	if err != nil {
		return err
	}
	roster, err := c.Models(ctx)
	if err != nil {
		return spineExitErr(ExitServerUnreachable, fmt.Errorf("read /v1/models from %s: %w", base, err))
	}
	running, _ := c.Running(ctx)
	ctxLens, roles, ctxNote := glueSeatFacts()
	if ctxNote != "" {
		rep.Notes = append(rep.Notes, ctxNote)
	}

	pinned := ""
	if pin != "" {
		entry, rerr := glueResolve(ctx, c, pin)
		if rerr != nil {
			return rerr
		}
		pinned = entry.ID
	}

	for _, m := range roster {
		if pinned != "" && !strings.EqualFold(m.ID, pinned) {
			continue
		}
		role := roles[strings.ToLower(m.ID)]
		if role == "" {
			role = glueRoleUnknown
		}
		rep.Models = append(rep.Models, glueConfigureModel{
			ID:            m.ID,
			Aliases:       m.Aliases,
			ContextLength: ctxLens[strings.ToLower(m.ID)],
			Role:          role,
			Loaded:        glueIsRunning(running, m.ID),
		})
	}
	sort.Slice(rep.Models, func(i, j int) bool { return rep.Models[i].ID < rep.Models[j].ID })
	rep.Snippet = glueConfigureSnippet(target, rep.APIBase, rep.Models, pinned)
	if pinned == "" {
		rep.Notes = append(rep.Notes,
			"the default model is the first CHAT seat, not the first seat alphabetically: pointing a chat client at an embedding or rerank seat produces nothing but errors. Override with --model.")
	}
	rep.Notes = append(rep.Notes, "print-only: nothing was written to any client's configuration")

	return mcEmit(cmd, flags, rep, func(w io.Writer) {
		fmt.Fprintln(w, rep.Snippet)
		fmt.Fprintln(w)
		tw := newTabWriter(w)
		fmt.Fprintln(tw, "MODEL\tROLE\tCTX\tLOADED\tALIASES")
		for _, m := range rep.Models {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\n", m.ID, m.Role, glueCtxText(m.ContextLength), m.Loaded, strings.Join(m.Aliases, ", "))
		}
		_ = tw.Flush()
		for _, n := range rep.Notes {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	})
}

func glueCtxText(n int) string {
	if n <= 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d", n)
}

// glueSeatFacts reads each seat's context length and role from the llama-swap
// YAML.
//
// Context length comes from -c / --ctx-size. A seat with no explicit flag is
// left at zero and reported as unknown: llama.cpp's own default depends on the
// GGUF, so a constant here would be a fabricated number — the exact class of
// error that has produced retracted context verdicts on this box.
//
// Role comes from the seat's own flags, never from its name. --embeddings makes
// an embedding seat whatever it is called; a seat named "embed-something" that
// lacks the flag is not one.
func glueSeatFacts() (map[string]int, map[string]string, string) {
	ctxLens := map[string]int{}
	roles := map[string]string{}
	path, err := lsconfig.DefaultConfigPath()
	if err != nil {
		return ctxLens, roles, "llama-swap YAML not found; context lengths and roles are reported as unknown"
	}
	cf, err := lsconfig.Load(path, lsconfig.LoadOptions{})
	if err != nil {
		return ctxLens, roles, "llama-swap YAML unreadable (" + err.Error() + "); context lengths and roles are reported as unknown"
	}
	for _, m := range cf.Models {
		key := strings.ToLower(m.ID)
		if m.Seat == lsconfig.SeatNonLlamaServer {
			// whisper-server and friends: no -c, no chat route. Classified so
			// they are visibly excluded rather than silently false-positived.
			roles[key] = glueRoleTranscribe
			continue
		}
		spec := lsconfig.ParseCmd(m.CmdExpanded)
		_, isEmbed := spec.GetAny("--embeddings", "--embedding")
		_, isRerank := spec.GetAny("--reranking", "--rerank")
		switch {
		case isEmbed:
			roles[key] = glueRoleEmbedding
		case isRerank:
			roles[key] = glueRoleRerank
		default:
			roles[key] = glueRoleChat
		}
		if f, ok := spec.GetAny("-c", "--ctx-size"); ok && len(f.Values) > 0 {
			if n := glueAtoi(f.Values[0]); n > 0 {
				ctxLens[key] = n
			}
		}
	}
	return ctxLens, roles, ""
}

func glueAtoi(s string) int {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// glueChatDefault picks the model a client should be pointed at by default.
//
// Alphabetical order is the wrong answer: on this roster it selects
// bge-reranker-v2-m3, and a chat client aimed at a rerank seat fails on every
// request. Chat seats only, preferring one that is already loaded so the first
// request does not pay a cold start.
func glueChatDefault(models []glueConfigureModel) string {
	for _, m := range models {
		if m.Role == glueRoleChat && m.Loaded {
			return m.ID
		}
	}
	for _, m := range models {
		if m.Role == glueRoleChat {
			return m.ID
		}
	}
	if len(models) > 0 {
		return models[0].ID
	}
	return ""
}

// glueConfigureSnippet renders the paste-ready block for a target.
func glueConfigureSnippet(target, apiBase string, models []glueConfigureModel, pinned string) string {
	primary := pinned
	if primary == "" {
		primary = glueChatDefault(models)
	}
	if primary == "" {
		primary = "<model-id>"
	}
	var b strings.Builder
	switch target {
	case "claude-code":
		b.WriteString("# llama-swap via its Anthropic-compatible endpoint.\n")
		b.WriteString("# llama-swap accepts any non-empty key; it does not authenticate by default.\n")
		fmt.Fprintf(&b, "export ANTHROPIC_BASE_URL=%q\n", strings.TrimSuffix(apiBase, "/v1"))
		b.WriteString("export ANTHROPIC_API_KEY=\"local\"\n")
		fmt.Fprintf(&b, "export ANTHROPIC_MODEL=%q\n", primary)
		if len(models) > 1 {
			b.WriteString("# other seats on this proxy:\n")
			for _, m := range models {
				if m.ID == primary {
					continue
				}
				fmt.Fprintf(&b, "#   %-20s %-10s%s\n", m.ID, m.Role, glueCtxSuffix(m.ContextLength))
			}
		}
	default:
		b.WriteString("# OpenAI-compatible client configuration for this llama-swap.\n")
		b.WriteString("# llama-swap accepts any non-empty key; it does not authenticate by default.\n")
		cfg := map[string]any{
			"base_url": apiBase,
			"api_key":  "local",
			"model":    primary,
		}
		raw, _ := json.MarshalIndent(cfg, "", "  ")
		b.Write(raw)
		b.WriteString("\n")
		if len(models) > 1 {
			b.WriteString("# other seats on this proxy:\n")
			for _, m := range models {
				if m.ID == primary {
					continue
				}
				fmt.Fprintf(&b, "#   %-20s %-10s%s\n", m.ID, m.Role, glueCtxSuffix(m.ContextLength))
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func glueCtxSuffix(ctxLen int) string {
	if ctxLen <= 0 {
		return "  (ctx unknown)"
	}
	return fmt.Sprintf("  (ctx %d)", ctxLen)
}
