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
	// Shards is populated when the file is one member of a split set and the
	// siblings could be located on disk.
	Shards *gguf.ShardSet `json:"shards,omitempty"`
	Notes  []string       `json:"notes,omitempty"`
	Raw    bool           `json:"raw_metadata_included"`
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
				if wantsAgentErrorEnvelope(flags) {
					return usageEnvelopeErr(flags, fmt.Errorf("%q requires a path or a loaded model name", cmd.CommandPath()))
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
			report.Notes = append(report.Notes, ggufHeaderNotes(header)...)
			if header.Split.IsShard() {
				set, serr := gguf.ResolveShards(path, header.Split.Count)
				switch {
				case serr != nil:
					report.Notes = append(report.Notes, "shard siblings not resolved: "+serr.Error())
				default:
					report.Shards = set
					if set.Complete {
						report.Notes = append(report.Notes, fmt.Sprintf(
							"all %d shards present: %s across the set (this file alone is %s)",
							set.Count, mcFmtMiB(int(set.TotalBytes/(1024*1024))), mcFmtMiB(int(header.FileSizeBytes/(1024*1024)))))
					} else {
						report.Notes = append(report.Notes, fmt.Sprintf(
							"INCOMPLETE shard set: %d of %d present, missing %v - any whole-model total would under-report",
							set.Count-len(set.Missing), set.Count, set.Missing))
					}
				}
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

// ggufHeaderNotes turns the header facts that CHANGE AN ANSWER into operator
// sentences. Shared by gguf, fit and ctx so a shard, a non-model file or a
// YaRN-extended context reads the same way wherever it surfaces.
func ggufHeaderNotes(h *gguf.Result) []string {
	if h == nil || !h.IsGGUF {
		return nil
	}
	var out []string
	if h.Split != nil {
		out = append(out, "split metadata: "+h.Split.Summary)
		if h.Split.Disagreement != "" {
			out = append(out, "shard numbering: "+h.Split.Disagreement)
		}
	}
	if !h.IsModel {
		out = append(out, "NOT A MODEL: "+h.NotAModelReason)
	}
	if h.FileTypeGuessed {
		out = append(out, fmt.Sprintf(
			"general.file_type carries LLAMA_FTYPE_GUESSED (%d): the quantization was INFERRED by the writer, not declared by the quantizer",
			gguf.FileTypeGuessedBit))
	}
	if h.PoolingRole != "" {
		out = append(out, "pooling: "+h.PoolingRole)
	}
	if native, declared, note := h.NativeContext(); note != "" {
		out = append(out, fmt.Sprintf("context: native %d, declared %d - %s", native, declared, note))
	}
	if h.UnsupportedKVArch != "" {
		out = append(out, fmt.Sprintf(
			"architecture-specific KV/state math: %s (keys: %s). fit and ctx REFUSE to size this rather than apply the standard formula",
			h.UnsupportedKVArch, strings.Join(h.UnsupportedKVKeys, ", ")))
	}
	if h.Quant != nil && h.Quant.LabelMismatch {
		out = append(out, "quantization label: "+h.Quant.MismatchNote)
	}
	if h.TensorInfoError != "" {
		out = append(out, "tensor table not walked ("+h.TensorInfoError+"): bits-per-weight and MoE parameter counts are unavailable")
	}
	return out
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
	if h.GeneralType != "" {
		fmt.Fprintf(w, "  general.type    %s\n", h.GeneralType)
	}
	if r.Shards != nil {
		fmt.Fprintf(w, "  shard set       %d shards, %s total, complete=%v  [%s]\n",
			r.Shards.Count, mcFmtMiB(int(r.Shards.TotalBytes/(1024*1024))), r.Shards.Complete, r.Shards.Convention)
	}
	fmt.Fprintf(w, "  quantization    %s (general.file_type=%d)\n", h.Quantization, h.FileType)
	if q := h.Quant; q != nil {
		fmt.Fprintf(w, "  bits per weight %.3f measured over %s params in %d tensors\n",
			q.BitsPerWeight, mcHumanCount(q.Elements), q.Tensors)
		for _, ts := range q.Types {
			fmt.Fprintf(w, "                  %-9s %5d tensors  %7s params  %6.2f%% of bytes  %.4g bpw\n",
				ts.Type, ts.Tensors, mcHumanCount(ts.Elements), ts.ShareBytes*100, ts.BitsPerElem)
		}
		if len(q.UnknownTypes) > 0 {
			fmt.Fprintf(w, "                  %s %d tensors use types this reader cannot size (%s)\n",
				yellow("unsized:"), q.UnsizedTensors, strings.Join(q.UnknownTypes, ", "))
		}
	}
	if m := h.MoE; m != nil {
		fmt.Fprintf(w, "  experts         %d total, %d active per token", m.ExpertCount, m.ExpertUsedCount)
		if m.SharedCount > 0 {
			fmt.Fprintf(w, " (+%d always-on shared)", m.SharedCount)
		}
		fmt.Fprintln(w)
		if m.ParamsTotal > 0 {
			fmt.Fprintf(w, "  params total    %s\n", mcHumanCount(m.ParamsTotal))
		}
		if m.ParamsActive > 0 {
			fmt.Fprintf(w, "  params active   %s per token\n", mcHumanCount(m.ParamsActive))
		}
		fmt.Fprintf(w, "                  [%s]\n", m.ActiveSource)
	}
	fmt.Fprintf(w, "  layers          %d\n", h.BlockCount)
	fmt.Fprintf(w, "  heads           %d attention / %d KV  [%s]\n", h.HeadCount, h.HeadCountKV, h.HeadCountKVSource)
	if h.HeadCount > 0 && h.HeadCountKV > 0 && h.HeadCount != h.HeadCountKV {
		fmt.Fprintf(w, "                  GQA %dx - using head_count for KV math would overestimate by that factor\n",
			h.HeadCount/h.HeadCountKV)
	}
	fmt.Fprintf(w, "  embedding       %d\n", h.EmbeddingLength)
	fmt.Fprintf(w, "  K/V per head    %d/%d [%s]\n", h.KeyLength, h.ValueLength, h.LengthSource)
	native, declared, ctxNote := h.NativeContext()
	if ctxNote != "" && native != declared {
		fmt.Fprintf(w, "  context         %d native / %d declared  [%s]\n", native, declared, ctxNote)
	} else {
		fmt.Fprintf(w, "  native ctx      %d tokens\n", declared)
	}
	if h.RoPE != nil && h.RoPE.Type != "" {
		fmt.Fprintf(w, "  rope scaling    %s factor %.4g", h.RoPE.Type, h.RoPE.Factor)
		if h.RoPE.YarnBetaFast > 0 || h.RoPE.YarnBetaSlow > 0 {
			fmt.Fprintf(w, " (yarn beta %.4g/%.4g)", h.RoPE.YarnBetaFast, h.RoPE.YarnBetaSlow)
		}
		fmt.Fprintln(w)
	}
	if h.PoolingTypeName != "" {
		fmt.Fprintf(w, "  pooling         %s\n", h.PoolingTypeName)
	}
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
