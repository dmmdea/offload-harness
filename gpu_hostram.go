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

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/hostneed"
)

// resolveReserveHostRAM is the order of internal/hostneed: the operator's --ram when given (0 is a
// value), else the estimate for a recognised render helper call, else the class default, else
// nothing. The card table is read only when an estimate needs a card's VRAM (a recognised render
// call, or a media lease with no --ram), so a plain text reservation pays nothing; a table that cannot
// be read leaves the card unknown, and then nothing is assumed to fit. It says what it declared and
// why on out, but only when the number is not zero by default: the operator needs the line when a
// lease will wait on it.
func resolveReserveHostRAM(ramGiven bool, ramGiB float64, class gpulease.Class, cmdArgs []string, ids []string, cfg config.Config, out io.Writer) hostneed.Need {
	req := hostneed.Request{Class: class, Args: cmdArgs}
	if ramGiven {
		req.Explicit = &ramGiB
	}
	facts := hostneed.Facts{Cfg: cfg, Env: os.Getenv}
	_, recognised := hostneed.ParseRenderCall(cmdArgs)
	if !ramGiven && (recognised || class == gpulease.ClassMedia) {
		if cards, _, err := cardTable(context.Background(), cfg); err == nil {
			facts.VRAMGiB = hostneed.LargestCardGiB(cards, ids)
		}
	}
	need := hostneed.Resolve(req, facts)
	if need.GiB > 0 || need.Source == hostneed.SourceExplicit {
		fmt.Fprintf(out, "gpu reserve: declaring %s\n", need)
	}
	return need
}
