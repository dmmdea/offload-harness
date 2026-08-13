// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored novel command.
// pp:data-source auto

package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"llamaswap-pp-cli/internal/gguf"
)

// ggufReport is the JSON contract of `gguf`.
type ggufReport struct {
	SchemaVersion int          `json:"schema_version"`
	Target        string       `json:"target"`
	ResolvedFrom  string       `json:"resolved_from"`
	Header        *gguf.Result `json:"header"`
	Notes         []string     `json:"notes,omitempty"`
	Raw           bool         `json:"raw_metadata_included"`
}

func newMeasureGgufCmd(flags *rootFlags) *cobra.Command {
	var flagRaw bool

	cmd := &cobra.Command{
		Use:   "gguf <path|loaded-model>",
		Short: "Read a GGUF file's header: architecture, layers, GQA heads, native context, quantization",
		Long: `Reads the header and metadata of a GGUF file - never the tensor body, so a 4 GB
model costs a fraction of a second.

Accepts a filesystem path, or the name of a model that is LOADED right now (its
weights path is taken from the live /running command line). An unloaded model
name is refused rather than guessed at: resolving it would mean parsing the
llama-swap YAML, which is 'config explain's job, or starting the seat, which is
a multi-GB side effect.

Non-GGUF weights (whisper .bin ggml containers) are reported as a typed
non-GGUF result, not an error.`,
		Example: `  llamaswap-pp-cli gguf V:/models/gemma-4-E4B-it-qat-GGUF/gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf
  llamaswap-pp-cli gguf embeddinggemma --json
  llamaswap-pp-cli gguf V:/models/whisper/ggml-large-v3-turbo.bin --json`,
		Annotations: map[string]string{
			"mcp:read-only":        "true",
			"pp:typed-exit-codes":  "3=model not loaded / file not found, 4=proxy unreachable",
			"pp:measurement-owner": "wave-c",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				if flags.asJSON {
					if err := printJSONFiltered(cmd.OutOrStdout(), map[string]any{
						"error": "requires a GGUF path or a loaded model name",
						"usage": cmd.CommandPath() + " --help",
					}, flags); err != nil {
						return err
					}
					return usageErr(fmt.Errorf("%q requires a path or a loaded model name", cmd.CommandPath()))
				}
				return cmd.Help()
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "gguf "+args[0])
			}
			// auto: a path is a purely local read; a model name needs the live
			// /running list to resolve its weights path.
			if flags != nil && flags.dataSource == "local" && !mcLooksLikePath(args[0]) {
				return usageErr(fmt.Errorf("--data-source local requires a .gguf path; resolving the model name %q needs the live /running list", args[0]))
			}

			target := args[0]
			report := &ggufReport{SchemaVersion: 1, Target: target, Raw: flagRaw}

			path := target
			if !mcLooksLikePath(target) {
				ctx, cancel := boundCtx(cmd.Context(), flags)
				defer cancel()
				timeout := mcTimeout(cmd, flags, 15*time.Second)
				seats, err := mcRunning(ctx, flags, timeout)
				if err != nil {
					return mcClassify(err)
				}
				roster, rosterErr := mcRoster(ctx, flags, timeout)
				name := target
				if rosterErr == nil {
					if resolved, ok := mcResolveAlias(roster, target); ok {
						name = resolved
						if resolved != target {
							report.Notes = append(report.Notes, fmt.Sprintf("alias %q resolves to %q", target, resolved))
						}
					}
				}
				seat, ok := mcFindSeat(seats, name)
				if !ok {
					return &cliError{code: ExitModelNotFound, err: fmt.Errorf(
						"%q is not a file and is not loaded right now (loaded: %s)\n"+
							"give me a path to the .gguf, or a model that is currently loaded - "+
							"resolving an unloaded seat's weights path would mean parsing the llama-swap YAML "+
							"(see `config explain`) or starting the model (multi-GB side effect)",
						target, mcJoinOrNone(mcLoadedNames(seats)))}
				}
				if !mcIsLlamaServer(seat.Cmd) {
					report.Notes = append(report.Notes, "seat is not a llama-server process (non-llama-server seat); its weights may not be GGUF")
				}
				p, ok := mcSeatModelPath(seat.Cmd)
				if !ok {
					return notFoundErr(fmt.Errorf("loaded seat %q has no -m/--model argument in its command line: %s", name, seat.Cmd))
				}
				path = p
				report.ResolvedFrom = fmt.Sprintf("live /running seat %q (-m %s)", name, p)
			} else {
				abs, err := filepath.Abs(target)
				if err == nil {
					path = abs
				}
				report.ResolvedFrom = "filesystem path"
			}

			if _, err := os.Stat(path); err != nil {
				return notFoundErr(fmt.Errorf("cannot read %s: %w", path, err))
			}
			header, err := gguf.Read(path)
			if err != nil {
				return err
			}
			if !header.IsGGUF {
				if label := gguf.KnownNonGGUF(header.MagicSeen); label != "" {
					report.Notes = append(report.Notes, "recognized container: "+label)
				}
				report.Notes = append(report.Notes, "classified non-llama-server/non-GGUF: GGUF, ctx and fit checks skip this file rather than false-positive on it")
			}
			if !flagRaw {
				header.KV = nil
			}
			report.Header = header

			return mcEmit(cmd, flags, report, func(w io.Writer) { mcPrintGguf(w, report) })
		},
	}
	cmd.Flags().BoolVar(&flagRaw, "raw", false, "Include every metadata key/value in the output (large)")
	return cmd
}

// mcLooksLikePath decides path-vs-model-name without touching the network.
func mcLooksLikePath(s string) bool {
	if strings.ContainsAny(s, `/\`) {
		return true
	}
	lower := strings.ToLower(s)
	if strings.HasSuffix(lower, ".gguf") || strings.HasSuffix(lower, ".bin") {
		return true
	}
	if _, err := os.Stat(s); err == nil {
		return true
	}
	return false
}

func mcJoinOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

func mcPrintGguf(w io.Writer, r *ggufReport) {
	h := r.Header
	fmt.Fprintf(w, "%s\n", bold(h.Path))
	fmt.Fprintf(w, "  resolved from   %s\n", r.ResolvedFrom)
	fmt.Fprintf(w, "  file size       %s (%d bytes)\n", mcFmtMiB(int(h.FileSizeMiB)), h.FileSizeBytes)
	if !h.IsGGUF {
		fmt.Fprintf(w, "  %s %s\n", yellow("NOT GGUF:"), h.NotGGUFReason)
		for _, n := range r.Notes {
			fmt.Fprintf(w, "  note            %s\n", n)
		}
		return
	}
	fmt.Fprintf(w, "  gguf version    v%d (%d tensors, %d metadata keys, header+metadata = %s)\n",
		h.Version, h.TensorCount, h.KVCount, mcFmtMiB(int(h.MetadataBytes/(1024*1024))))
	fmt.Fprintf(w, "  architecture    %s\n", h.Architecture)
	if h.Name != "" {
		fmt.Fprintf(w, "  name            %s\n", h.Name)
	}
	if h.SizeLabel != "" {
		fmt.Fprintf(w, "  size label      %s\n", h.SizeLabel)
	}
	fmt.Fprintf(w, "  quantization    %s (general.file_type=%d)\n", h.Quantization, h.FileType)
	fmt.Fprintf(w, "  layers          %d\n", h.BlockCount)
	fmt.Fprintf(w, "  heads           %d attention / %d KV  [%s]\n", h.HeadCount, h.HeadCountKV, h.HeadCountKVSource)
	if h.HeadCount > 0 && h.HeadCountKV > 0 && h.HeadCount != h.HeadCountKV {
		fmt.Fprintf(w, "                  GQA %dx - using head_count for KV math would overestimate by that factor\n",
			h.HeadCount/h.HeadCountKV)
	}
	fmt.Fprintf(w, "  embedding       %d\n", h.EmbeddingLength)
	fmt.Fprintf(w, "  K/V per head    %d/%d [%s]\n", h.KeyLength, h.ValueLength, h.LengthSource)
	fmt.Fprintf(w, "  native ctx      %d tokens\n", h.ContextLength)
	if h.SlidingWindow > 0 {
		swa := 0
		for _, b := range h.SlidingWindowPattern {
			if b {
				swa++
			}
		}
		fmt.Fprintf(w, "  sliding window  %d tokens (%d of %d layers)\n", h.SlidingWindow, swa, h.BlockCount)
	}
	if h.SharedKVLayers > 0 {
		fmt.Fprintf(w, "  shared KV       %d trailing layers reuse an earlier layer's cache\n", h.SharedKVLayers)
	}
	if h.ChatTemplateChars > 0 {
		fmt.Fprintf(w, "  chat template   %d chars (sha256 %s)\n", h.ChatTemplateChars, h.ChatTemplateSHA256[:12])
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "  note            %s\n", n)
	}
}
