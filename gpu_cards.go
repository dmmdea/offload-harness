package main

// The per-card view of the GPU lease (plan P3): `gpu cards`, and the card table and lease
// list inside `gpu status`. The lease record names its cards by UUID (invariant I2), so
// the view is keyed the same way and shows which index each space gives the card.
//
// Status text is read by people and asserted by contains-checks, not frozen goldens
// (later phases change it); the JSON keys are the stable surface and are only ever
// added to.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpualloc"
	"github.com/dmmdea/offload-harness/internal/gpucards"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// cardTableFn reads the card table, bounded only by ctx: the callers set the deadline (cardTable
// one attempt, cardTablePatient the attempt and its retry), because a reader that added a
// deadline of its own would cap the retry's longer one too. It IS the allocator's production
// reader (gpualloc.ReadCards), so the rule has one definition and one run against a stand-in
// nvidia-smi. A variable so tests do not need nvidia-smi.
var cardTableFn = gpualloc.ReadCards

// cardTable returns the cards, a warning about the declared ComfyUI order (when it was
// rejected), and an error when nvidia-smi gave no table. One attempt, five seconds: it feeds the
// views (`gpu cards`, `gpu status`), which show "no table" and move on.
func cardTable(ctx context.Context, cfg config.Config) ([]gpuprobe.Card, string, error) {
	first := cardReadFirst
	if first <= 0 {
		first = gpualloc.DefaultCardRead
	}
	cctx, cancel := context.WithTimeout(ctx, first)
	defer cancel()
	return cardTableFn(cctx, cfg)
}

// cardReadFirst and cardReadRetry are the deadlines of the card-table read: the one attempt of
// cardTable, and the attempt and its retry in cardTablePatient (zero = gpualloc's defaults, five
// seconds and fifteen). Variables so a test does not wait them out.
var cardReadFirst, cardReadRetry time.Duration

// cardTableDeps is the allocator's card-table reader and its display-card idle reader through this package's seams.
func cardTableDeps() gpualloc.Deps {
	return gpualloc.Deps{Cards: cardTableFn, ReadDeadline: cardReadFirst, RetryDeadline: cardReadRetry, DisplayBusy: displayBusyFn}
}

// displayBusyFn is the display card's idle reader (gpualloc.DisplayBusy: "" = idle, the operator away and the card
// not busy), the one every allocator input and status surface of this package reads. A variable so a test does not
// depend on whether someone is at the keyboard of the machine running it.
var displayBusyFn = gpualloc.DisplayBusy

// cardTablePatient reads the table for the callers that DECIDE something from it: a reserve or a
// node swap that names cards (it refuses without a table), the allocator's input, and the scope
// of a drain or an unload (without a table every seat counts as on the leased cards). A read
// that ran out of time is made once more under a longer deadline (gpualloc.Deps.CardTable)
// before it counts as "no table", so one slow nvidia-smi under load does not refuse an
// operator's command or widen a fence.
func cardTablePatient(ctx context.Context, cfg config.Config) ([]gpuprobe.Card, string, error) {
	return cardTableDeps().CardTable(ctx, cfg)
}

// statusLeaseSection is what `gpu status --json` adds for cards: the table (empty, with a
// note, when there is no card table), the per-lease list and the queue. Only keys are
// added; nothing existing changes type.
func statusLeaseSection(m *gpulease.Manager, cfg config.Config, cards []gpuprobe.Card, note string, waiters []gpulease.Waiter) map[string]any {
	capPct, floorGiB := displayCapNow(cfg) // gpu status --json
	sec := gpucards.Section(cards, note, modelaffinity.ScopeLeases(m.Dir(), m.Leases()), waiters, capPct, floorGiB)
	if rows, ok := sec["cards"].([]gpucards.Row); ok {
		gpucards.MarkIdleGate(rows, displayBusyNow(cfg, cards, capPct))
	}
	return sec
}

// displayBusyNow is the display card's idle gate as the allocator reads it now (gpualloc.DisplayBusy: "" = idle),
// for the surfaces that print the cap, so none calls the card open to jobs while the allocator keeps them off it.
// It reads the desk only when there is a display card and the cap is open (the gate is moot otherwise).
func displayBusyNow(cfg config.Config, cards []gpuprobe.Card, capPct int) string {
	if capPct <= 0 {
		return ""
	}
	for _, c := range cards {
		if c.Display {
			return displayBusyFn(cfg, cards)
		}
	}
	return ""
}

// displayCapNow is the display card's cap and declared floor as the allocator reads them: the cap from the
// config file as it is NOW (LiveGPUDisplayCap, so the operator's "I need the card" switch is seen by a
// process that started before he wrote it) and the display layer's declared floor. Every surface of this
// package that prints the card table asks this one function, so none shows the table without the verdict.
func displayCapNow(cfg config.Config) (capPct int, floorGiB float64) {
	return cfg.LiveGPUDisplayCap(), cfg.DisplayFloorGiB()
}

// printCardTable prints the card table under a "card table:" heading, or says why there is none.
func printCardTable(cfg config.Config, cards []gpuprobe.Card, note string, cerr error, leases []gpulease.Info) {
	if cerr != nil {
		fmt.Printf("  card table: none (%v)\n", cerr)
		return
	}
	fmt.Println("  card table:")
	capPct, floorGiB := displayCapNow(cfg) // the table under gpu status
	for _, ln := range gpucards.Table(gpucards.MarkIdleGate(gpucards.RowsWithCap(cards, leases, capPct, floorGiB), displayBusyNow(cfg, cards, capPct))) {
		fmt.Println("    " + ln)
	}
	if note != "" {
		fmt.Printf("    note: %s\n", note)
	}
	for _, c := range cards {
		if c.ComfyOrder < 0 && len(cards) > 1 {
			fmt.Println("    comfy ?: ComfyUI's device order is not declared (config gpu_comfy_order); a command naming `--cuda-device N` is reserved as the whole node until it is")
			break
		}
	}
}

// displayCheckRefused is `gpu display-check`'s exit status when the display card would not take the job now. It is
// not 1, so a launcher tells "no, not now" from a check that could not run (no card table, a bad flag).
const displayCheckRefused = 3

// runGPUDisplayCheck is `gpu display-check`: the allocator's display-card rule (gpulease.DisplayAdmits over the
// input every lease build reads, gpualloc.BuildInput) asked once for a footprint, for a launcher that puts work on
// the display card OUTSIDE a lease: the opencode-only three-card vLLM seat, whose middle stage lives on that card
// (setup/templates/vllm-seat/windows-wsl/seat-cmd.ps1 asks it when the seat's row declares DisplayVramGiB). Exit 0
// admits; exit 3 refuses and prints why; any other failure is exit 1. It judges admission only, as the allocator
// does: a job it admitted is not watched afterwards.
func runGPUDisplayCheck(args []string) error {
	fs := flag.NewFlagSet("gpu display-check", flag.ExitOnError)
	fs.String("config", "", "config file path")
	vram := fs.Float64("vram", 0, "GiB of VRAM the job will use on the display card (required: below a cap of 100 a job that declares none is refused, as the allocator refuses it)")
	asJSON := fs.Bool("json", false, "emit JSON")
	_ = fs.Parse(args)
	if *vram < 0 {
		return fmt.Errorf("gpu display-check: --vram must be 0 or more, got %g", *vram)
	}
	m, err := openLease(fs)
	if err != nil {
		return err
	}
	cfg := loadCfg(fs)
	in, err := gpualloc.BuildInput(context.Background(), m, cfg, gpualloc.Need{VRAMGiB: *vram}, cardTableDeps())
	if err != nil {
		return fmt.Errorf("gpu display-check: %w", err)
	}
	ok, why := gpulease.DisplayAdmits(in)
	if *asJSON {
		b, _ := json.MarshalIndent(map[string]any{
			"admits": ok, "why": why, "vram_gib": *vram,
			"display_cap_pct": in.DisplayCapPct, "display_busy": in.DisplayBusy,
		}, "", "  ")
		fmt.Println(string(b))
	} else if ok {
		fmt.Printf("display card admits a %.2f GiB job now (cap %d %%, idle)\n", *vram, in.DisplayCapPct)
	} else {
		fmt.Printf("display card refuses a %.2f GiB job now: %s\n", *vram, why)
	}
	if !ok {
		return &exitCodeError{code: displayCheckRefused, msg: "gpu display-check: refused: " + why}
	}
	return nil
}

// runGPUCards is `gpu cards`: the card table with each card's holder.
func runGPUCards(args []string) error {
	fs := flag.NewFlagSet("gpu cards", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "emit JSON")
	_ = fs.Parse(args)
	m, err := openLease(fs)
	if err != nil {
		return err
	}
	cfg := loadCfg(fs)
	cards, note, cerr := cardTable(context.Background(), cfg)
	if cerr != nil {
		return fmt.Errorf("no card table: %w", cerr)
	}
	leases := m.Leases()
	capPct, floorGiB := displayCapNow(cfg) // gpu cards
	rows := gpucards.MarkIdleGate(gpucards.RowsWithCap(cards, leases, capPct, floorGiB), displayBusyNow(cfg, cards, capPct))
	if *asJSON {
		b, _ := json.MarshalIndent(map[string]any{
			"cards": rows, "leases": gpucards.LeaseRows(leases), "cards_note": note,
			"card_scoped_leases": m.CardScoped(), "state_root": m.Root(),
		}, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	for _, ln := range gpucards.Table(rows) {
		fmt.Println(ln)
	}
	if note != "" {
		fmt.Fprintf(os.Stdout, "note: %s\n", note)
	}
	if !m.CardScoped() {
		fmt.Println("card-scoped leases: off on this host (every reservation is whole-node); see `gpu doctor`")
	}
	return nil
}
