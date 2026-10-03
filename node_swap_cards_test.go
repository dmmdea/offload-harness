package main

// `node-swap --cards` names the cards a standalone deploy touches (GPU routing P7). The default
// of the wait is to HOLD for every lease; naming a card narrows it, so a card the operator meant
// that places on nothing must never read as "no lease there": a card index, a truncated UUID or a
// typo that silently matched no lease would let the deploy go ahead past the very render the
// operator meant to wait for. Every entry is resolved against the box's card table, the way
// `gpu reserve --devices` resolves it, and an entry that places on no card refuses the deploy.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/nodeswap"
)

// stubCardTable replaces the card-table read for one test (no nvidia-smi).
func stubCardTable(t *testing.T, cards []gpuprobe.Card, err error) {
	t.Helper()
	old := cardTableFn
	cardTableFn = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { return cards, "", err }
	t.Cleanup(func() { cardTableFn = old })
}

// An nvidia-smi index, a full UUID and an unambiguous UUID prefix (with or without the "GPU-"
// head, in any case) each resolve to the lease id a lease records.
func TestResolveNodeSwapCardsAcceptsIndexUUIDAndPrefix(t *testing.T) {
	stubCardTable(t, statusCards(), nil)
	var logged []string
	plan, err := resolveNodeSwapCards(context.Background(), nodeswap.Plan{Cards: []string{"2", "GPU-AAAA0000", "bbbb0000-x"}}, config.Config{},
		func(s string) { logged = append(logged, s) })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gpu-cccc0000-x", "gpu-aaaa0000-x", "gpu-bbbb0000-x"}
	if strings.Join(plan.Cards, ",") != strings.Join(want, ",") {
		t.Fatalf("Cards = %q, want the lease ids %q", plan.Cards, want)
	}
	if len(logged) == 0 {
		t.Error("the resolution was not logged: a detached run's log must say which cards the deploy named")
	}
}

// An entry that places on no card refuses the deploy: an unknown index, a prefix no card has, a
// prefix that is too short to be one, a prefix that fits two cards, a card named twice, a blank.
func TestResolveNodeSwapCardsRefusesAnEntryThatPlacesOnNoCard(t *testing.T) {
	two := []gpuprobe.Card{
		{UUID: "GPU-dddd0000-x", NvidiaIndex: 0},
		{UUID: "GPU-dddd1111-x", NvidiaIndex: 1},
	}
	for _, c := range []struct {
		name  string
		cards []gpuprobe.Card
		keys  []string
	}{
		{"unknown index", statusCards(), []string{"7"}},
		{"prefix no card has", statusCards(), []string{"GPU-ffff9999"}},
		{"too short to be a prefix", statusCards(), []string{"gpu-"}},
		{"the right card and a typo", statusCards(), []string{"2", "gpu-cccd0000"}},
		{"ambiguous prefix", two, []string{"dddd"}},
		{"a card named twice", statusCards(), []string{"2", "gpu-cccc0000"}},
		{"a blank entry", statusCards(), []string{"2", " "}},
	} {
		t.Run(c.name, func(t *testing.T) {
			stubCardTable(t, c.cards, nil)
			plan, err := resolveNodeSwapCards(context.Background(), nodeswap.Plan{Cards: c.keys}, config.Config{}, nil)
			if err == nil {
				t.Fatalf("Cards %q resolved to %q: an entry that places on no card must refuse the deploy, not read as no lease there", c.keys, plan.Cards)
			}
			if !strings.Contains(err.Error(), "--cards") {
				t.Errorf("error = %q, want it to name --cards", err)
			}
		})
	}
}

// With no card table the entries cannot be checked, so the deploy is refused (the fail-closed
// answer; dropping --cards gives the whole-node wait).
func TestResolveNodeSwapCardsRefusesWithoutACardTable(t *testing.T) {
	stubCardTable(t, nil, errors.New("nvidia-smi not found"))
	_, err := resolveNodeSwapCards(context.Background(), nodeswap.Plan{Cards: []string{"2"}}, config.Config{}, nil)
	if err == nil || !strings.Contains(err.Error(), "card table") || !strings.Contains(err.Error(), "--cards") {
		t.Fatalf("error = %v, want the missing card table named", err)
	}
}

// No cards named is the whole node and reads no table; with a health URL the engine refuses the
// flag, so the table is not read for it either.
func TestResolveNodeSwapCardsLeavesTheWholeNodeAndHealthURLPlansAlone(t *testing.T) {
	old := cardTableFn
	cardTableFn = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		t.Error("the card table was read for a plan that names no resolvable cards")
		return nil, "", nil
	}
	t.Cleanup(func() { cardTableFn = old })
	for _, p := range []nodeswap.Plan{{}, {Cards: []string{"2"}, HealthURL: "http://node.invalid/fleet/health"}} {
		got, err := resolveNodeSwapCards(context.Background(), p, config.Config{}, nil)
		if err != nil || strings.Join(got.Cards, ",") != strings.Join(p.Cards, ",") {
			t.Fatalf("plan %+v became %+v (err %v), want it untouched", p, got, err)
		}
	}
}

// The command refuses before anything is touched, and a detached poller reads why from the
// result file.
func TestRunNodeSwapRefusesACardThatPlacesOnNoCard(t *testing.T) {
	stubCardTable(t, statusCards(), nil)
	t.Setenv("GPU_LOCK", "")
	dir := t.TempDir()
	staged, target := filepath.Join(dir, "staged.exe"), filepath.Join(dir, "target.exe")
	if err := os.WriteFile(staged, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	leaseDir := filepath.Join(dir, "lease-state")
	if err := os.MkdirAll(leaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"fleet_listen": "127.0.0.1:18811", "state_dir": "`+filepath.ToSlash(leaseDir)+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(dir, "result.json")
	err := runNodeSwap([]string{
		"--staged", staged, "--target", target, "--sha256", "h", "--skip-hash-check",
		"--config", cfgPath, "--cards", "9", "--result", resultPath,
	})
	if err == nil {
		t.Fatal("--cards 9 names no card on this box and the deploy went ahead")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "old" {
		t.Fatalf("target = %q: the binary was swapped past a card that does not exist", got)
	}
	b, rerr := os.ReadFile(resultPath)
	if rerr != nil {
		t.Fatalf("no result file for the poller: %v", rerr)
	}
	if !strings.Contains(string(b), `"ok": false`) || !strings.Contains(string(b), "--cards") {
		t.Fatalf("result = %s, want ok:false naming --cards", b)
	}
}

// The cards the command resolved are the ones the engine waits on: a lease on the card index 0
// names holds a deploy that says --cards 0, and the deploy does not touch the binary meanwhile.
// Left as the string "0" the entry would match no lease and the deploy would go ahead.
func TestRunNodeSwapHoldsForALeaseOnTheCardTheIndexNames(t *testing.T) {
	stubCardTable(t, statusCards(), nil)
	t.Setenv("GPU_LOCK", "")
	dir := t.TempDir()
	staged, target := filepath.Join(dir, "staged.exe"), filepath.Join(dir, "target.exe")
	if err := os.WriteFile(staged, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	leaseState := filepath.Join(dir, "lease-state")
	if err := os.MkdirAll(leaseState, 0o755); err != nil {
		t.Fatal(err)
	}
	m, err := gpulease.OpenAt("", leaseState)
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render on card 0", Devices: []string{"gpu-aaaa0000-x"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"fleet_listen": "127.0.0.1:18811", "state_dir": "`+filepath.ToSlash(leaseState)+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	err = runNodeSwap([]string{
		"--staged", staged, "--target", target, "--sha256", "h", "--skip-hash-check",
		"--config", cfgPath, "--cards", "0", "--wait-idle-timeout", "1ms", "--result", filepath.Join(dir, "result.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "GPU lease never cleared") {
		t.Fatalf("err = %v, want the lease on card 0 to hold a deploy that names card 0", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "old" {
		t.Fatalf("target = %q: the binary was swapped under a render on the card the deploy named", got)
	}
}
