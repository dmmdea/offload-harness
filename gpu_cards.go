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
	"github.com/dmmdea/offload-harness/internal/gpucards"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// cardTableFn reads the card table. A variable so tests do not need nvidia-smi.
var cardTableFn = func(ctx context.Context, cfg config.Config) ([]gpuprobe.Card, string, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return gpuprobe.ReadCards(cctx, cfg.GPUComfyOrder)
}

// cardTable returns the cards, a warning about the declared ComfyUI order (when it was
// rejected), and an error when nvidia-smi gave no table.
func cardTable(ctx context.Context, cfg config.Config) ([]gpuprobe.Card, string, error) {
	return cardTableFn(ctx, cfg)
}

// statusLeaseSection is what `gpu status --json` adds for cards: the table (empty, with a
// note, when there is no card table), the per-lease list and the queue. Only keys are
// added; nothing existing changes type.
func statusLeaseSection(m *gpulease.Manager, cards []gpuprobe.Card, note string, waiters []gpulease.Waiter) map[string]any {
	return gpucards.Section(cards, note, modelaffinity.ScopeLeases(m.Dir(), m.Leases()), waiters)
}

// printCardTable prints the card table under a "card table:" heading, or says why there is none.
func printCardTable(cards []gpuprobe.Card, note string, cerr error, leases []gpulease.Info) {
	if cerr != nil {
		fmt.Printf("  card table: none (%v)\n", cerr)
		return
	}
	fmt.Println("  card table:")
	for _, ln := range gpucards.Table(gpucards.Rows(cards, leases)) {
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
	if *asJSON {
		b, _ := json.MarshalIndent(map[string]any{
			"cards": gpucards.Rows(cards, leases), "leases": gpucards.LeaseRows(leases), "cards_note": note,
			"card_scoped_leases": m.CardScoped(), "state_root": m.Root(),
		}, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	for _, ln := range gpucards.Table(gpucards.Rows(cards, leases)) {
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
