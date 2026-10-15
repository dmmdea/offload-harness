package main

// The host RAM a `gpu reserve` declares (the paging incident of 2026-10-09; the rule is
// internal/gpulease/hostram.go, the estimate internal/hostneed). The verb resolves ONE number for the
// lease, before any card is chosen or claimed, so every path agrees on it: the named cards, the
// allocator's pre-filter (`--cards`), the grant itself, and the detached holder, which receives it as
// `--ram` and never resolves again.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/hostneed"
)

// resolveReserveHostRAM is the order of internal/hostneed: the operator's --ram when given (0 is a
// value), else the estimate for a recognised render helper call, else the class default, else
// nothing. The card table is read only when an estimate needs a card's VRAM (a recognised render
// call, or a media lease with no --ram), so a plain text reservation pays nothing; a table that cannot
// be read leaves the card unknown, and then nothing is assumed to fit. readCards is the host's
// card-scoped switch: a host that has not enabled card-scoped leases reads no card table at all (its
// leases are whole-node, exactly as before, and it may have no NVIDIA card to ask), so its card is
// unknown by construction and the estimate is the conservative one. It says what it declared and
// why on out, but only when the number is not zero by default: the operator needs the line when a
// lease will wait on it. When the figure is the media class default and the operator did not choose
// it, one more line says why and that --ram states the real figure (classDefaultHint, F71).
func resolveReserveHostRAM(ramGiven bool, ramGiB float64, class gpulease.Class, cmdArgs []string, ids []string, cfg config.Config, readCards bool, out io.Writer) hostneed.Need {
	return resolveReserveHostRAMWith(ramGiven, ramGiB, class, cmdArgs, ids, cfg, readCards, nil, out)
}

// resolveReserveHostRAMWith is resolveReserveHostRAM for a verb whose plan already read the card table: known
// is that table (nil = none was read, and the estimate reads one when it needs a card's VRAM). Reusing it
// keeps a command's nvidia-smi reads to its deciding stages (the plan's, the unload scope's, the wrapper's),
// each with its retry, instead of adding a fourth for the estimate: a wedged driver costs a command the
// reads it already makes (F24), and the estimate sees the very table the plan chose its cards from.
func resolveReserveHostRAMWith(ramGiven bool, ramGiB float64, class gpulease.Class, cmdArgs []string, ids []string, cfg config.Config, readCards bool, known []gpuprobe.Card, out io.Writer) hostneed.Need {
	req := hostneed.Request{Class: class, Args: cmdArgs}
	if ramGiven {
		req.Explicit = &ramGiB
	}
	facts := hostneed.Facts{Cfg: cfg, Env: os.Getenv}
	call, recognised := hostneed.ParseRenderCall(cmdArgs)
	if readCards && !ramGiven && (recognised || class == gpulease.ClassMedia) {
		// CardsGiB is each leased card's size: the card-only Wan placement (comfy-video.mjs --wan-placement multicard) runs on
		// exactly two of them and is sized against the pair, where every other call is sized against the largest one.
		if known != nil {
			facts.VRAMGiB = hostneed.LargestCardGiB(known, ids)
			facts.CardsGiB = hostneed.LeasedCardsGiB(known, ids)
		} else if cards, _, err := cardTable(context.Background(), cfg); err == nil {
			facts.VRAMGiB = hostneed.LargestCardGiB(cards, ids)
			facts.CardsGiB = hostneed.LeasedCardsGiB(cards, ids)
		}
	}
	need := hostneed.Resolve(req, facts)
	if need.GiB > 0 || need.Source == hostneed.SourceExplicit || need.CardOnly { // a measured card-only 0 is said, with why: nothing corrects it afterwards
		fmt.Fprintf(out, "gpu reserve: declaring %s\n", need)
		if hint := classDefaultHint(need, cmdArgs, call, recognised); hint != "" {
			fmt.Fprintln(out, hint)
		}
	}
	return need
}

// classDefaultHint is the line after the declaring one when the declared figure is the media class default (the largest
// ComfyUI family this box binds, sized as if it streamed from RAM) and the operator gave no --ram (F71). The default is a
// figure for a call the lease could not read, so the caller whose weights sit on the cards cannot tell from "class default"
// that it is theirs to correct: a driver script that wrapped card-only renders declared 46.8 GiB for weights that sat on
// two cards, and waited a queue cycle on it. Resolve reaches the class default for exactly four reasons, and the
// line names the one that applies: nothing is wrapped (a hold), the wrapped command is not a render helper it knows (named
// from the same table that recognises them), a recognised helper runs its own --graph whose weight files the lease could
// not read and size in full (a graph it can is sized from its own loaders, hostneed.ForGraphFile, and is not here), or a
// recognised helper's weight files could not be sized. It is one line, and it never contains the word the declaring line is grepped by, so a script
// that greps for the declaration still sees one. It is a WARNING since the 2026-10-14 incident: the figure is a guess about a
// command the lease could not read, and a lease that unloads the seat took the cards on it and held them idle for about 50
// minutes while the host could not meet it; the line says so (the hold bound is what ends that now) and that --ram is the fix.
// It changes no figure.
func classDefaultHint(need hostneed.Need, cmdArgs []string, call hostneed.RenderCall, recognised bool) string {
	if need.Source != hostneed.SourceClassDefault {
		return ""
	}
	var why string
	switch {
	case len(cmdArgs) == 0:
		why = "no command is wrapped, so there is nothing to size"
	case !recognised:
		why = "the wrapped command is not a recognised render helper (" + strings.Join(hostneed.RenderHelpers(), ", ") + "), so its weights are not sized"
	case call.RunsOwnGraph():
		why = call.Script + " runs its own --graph, and the lease could not size the weight files that graph names (it is not a readable ComfyUI API-format graph, it names no weight file, or a file it names is not in the ComfyUI model tree)"
	default:
		why = "the weight files " + call.Script + " names could not be sized (a named file is not in the ComfyUI model tree and its family has no documented size)"
	}
	return "gpu reserve: WARNING: that is the class default, not a measure of this command: " + why + ". Declared as it stands, the figure may hold the cards while the host cannot meet it (a lease that unloads the seat takes the cards first and holds them up to gpu_hostram_hold_max_sec, then releases them). Pass --ram <GiB> to state the real figure (--ram 0 when the weights sit on the cards)"
}
