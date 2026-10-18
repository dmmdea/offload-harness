package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Register B-57. A Windows box that runs more than one seat from one seat directory runs the same seat-cmd.ps1 and
// seat-cmdstop.ps1 for all of them (each seat's llama-swap entry passes its own seat id). `install vllm-seat` renders the
// tier's lane seat, so the stubs it writes must also serve the tier's other Windows seats: before, they baked the lane
// seat's id and port in, and a second seat waited for the lane seat's id for its whole load budget.

// stubRowPattern matches one seat row of the table the two stubs look their seat argument up in.
var stubRowPattern = regexp.MustCompile(`(?m)^\s*'([^']+)' = @\{ Id = '([^']+)'; Port = (\d+); HealthTimeout = (\d+) \}[ \t\r]*$`)

// stubRows reads the seat rows of a rendered stub as id -> port.
func stubRows(t *testing.T, stub string) map[string]string {
	t.Helper()
	rows := map[string]string{}
	for _, m := range stubRowPattern.FindAllStringSubmatch(stub, -1) {
		if m[1] != m[2] {
			t.Errorf("the row keyed %q serves the id %q: the seat argument is the seat id", m[1], m[2])
		}
		rows[m[1]] = m[3]
	}
	return rows
}

// copyVLLMSeatTemplates puts the repo's setup/templates/vllm-seat under a scratch repo root, so a command run with --root
// there renders from a tier table the test authored and the real templates.
func copyVLLMSeatTemplates(t *testing.T, root string) {
	t.Helper()
	src := filepath.Join("setup", "templates", "vllm-seat")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		dst := filepath.Join(root, src, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// twoSeatTier is blackwell-3x16 with extra vLLM seats added: a second Windows seat on its own port, and a seat that starts
// through systemd (the default launch), which does not run the PowerShell stubs.
func twoSeatTier(t *testing.T, mutate func(windowsExtra map[string]any)) string {
	t.Helper()
	doc := tableDoc(t)
	tier := tierEntry(doc, "blackwell-3x16")
	lane := tier["vllm_seat"].(map[string]any)
	clone := func(id, unit string, port int) map[string]any {
		e := map[string]any{}
		for k, v := range lane {
			e[k] = v
		}
		// The fields that exist only for the agent lane are not an extra seat's.
		for _, k := range []string{"fallback_agent_model", "fallback_agent_ctx_tokens", "fallback_agent_device", "agent_ctx_tokens",
			"agent_max_tokens", "agent_thinking", "agent_sampling", "agent_timeout_sec", "agent_seat_tok_s", "cache_server", "mp_port", "mp_http_port"} {
			delete(e, k)
		}
		e["id"], e["aliases"], e["unit"], e["port"] = id, []any{id + "-pool"}, unit, port
		return e
	}
	windows := clone("beta-vllm", "vllm-beta-seat", 18807)
	systemd := clone("gamma-vllm", "vllm-gamma-seat", 18817)
	delete(systemd, "launch")
	if mutate != nil {
		mutate(windows)
	}
	tier["extra_vllm_seats"] = []any{windows, systemd}
	root := tableRoot(t, doc)
	copyVLLMSeatTemplates(t, root)
	return root
}

// The stubs the lane seat's render writes hold the tier's other Windows seat (its id and its own port) and not the seat
// that starts through systemd; both stubs hold the same table, and the output says so.
func TestInstallVLLMSeatStubsServeEveryWindowsSeatOfTheTier(t *testing.T) {
	root := twoSeatTier(t, nil)
	lane := committedLaneSeat(t, "blackwell-3x16")
	r := newWSLBox(t, "blackwell-3x16").run(t, map[string]string{"-root": root})
	if r.err != nil {
		t.Fatalf("install vllm-seat: %v", r.err)
	}
	want := map[string]string{lane.ID: "18797", "beta-vllm": "18807"}
	for _, f := range []string{"seat-cmd.ps1", "seat-cmdstop.ps1"} {
		got := stubRows(t, readRendered(t, r.out, f))
		if len(got) != len(want) {
			t.Errorf("%s serves %v, want exactly %v (the lane seat and the tier's other Windows seat, not the systemd one)", f, got, want)
		}
		for id, port := range want {
			if got[id] != port {
				t.Errorf("%s: seat %s has port %q, want %s (rows %v)", f, id, got[id], port, got)
			}
		}
	}
	if strings.Contains(readRendered(t, r.out, "seat-cmd.ps1"), "gamma-vllm") {
		t.Errorf("a seat that starts through systemd runs no PowerShell stub and must not be in its table")
	}
	if !strings.Contains(r.stdout, "serve every Windows seat of the tier ("+lane.ID+", beta-vllm)") {
		t.Errorf("the next steps must say the stubs serve both seats:\n%s", r.stdout)
	}
	// This command writes the lane seat's env and tasks script; the other seat's are not its output.
	if got := renderedFiles(t, r.out); strings.Contains(strings.Join(got, ","), "beta-vllm") {
		t.Errorf("the other Windows seat's own files are not rendered by this run, yet it wrote %v", got)
	}
}

// A tier that declares one Windows seat renders a table of one: the committed tiers are unchanged apart from that table.
func TestInstallVLLMSeatStubsOfAOneSeatTierHoldOnlyThatSeat(t *testing.T) {
	for _, tier := range []string{"blackwell-2x16", "blackwell-3x16"} {
		t.Run(tier, func(t *testing.T) {
			r := newWSLBox(t, tier).run(t, nil)
			if r.err != nil {
				t.Fatalf("install vllm-seat --profile %s: %v", tier, r.err)
			}
			id := committedLaneSeat(t, tier).ID
			for _, f := range []string{"seat-cmd.ps1", "seat-cmdstop.ps1"} {
				if got := stubRows(t, readRendered(t, r.out, f)); len(got) != 1 || got[id] != "18797" {
					t.Errorf("%s of %s serves %v, want only %s on 18797", f, tier, got, id)
				}
			}
			if strings.Contains(r.stdout, "serve every Windows seat of the tier") {
				t.Errorf("a one-seat tier has nothing to say about other seats:\n%s", r.stdout)
			}
		})
	}
}

// A seat the table cannot hold is refused before anything is written, naming the problem, like every other render refusal.
func TestInstallVLLMSeatRefusesAnUnusableWindowsSiblingBeforeWriting(t *testing.T) {
	root := twoSeatTier(t, func(windowsExtra map[string]any) { windowsExtra["id"] = "beta vllm" })
	mustRefuse(t, newWSLBox(t, "blackwell-3x16").run(t, map[string]string{"-root": root}),
		"a Windows sibling whose id is not a seat argument", "beta vllm", "must match")
}
