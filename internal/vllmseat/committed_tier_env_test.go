package vllmseat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The tests in this file render the COMMITTED tier table, not a fixture. Every other
// windows-wsl render test in this package goes through flagship()/pinned(), a hand-written
// copy of the seat, so the tier a fresh install actually gets (its devices, its utilization,
// its argument tail) was asserted nowhere as a rendered env file. A box's seat env is
// maintained by hand today; these goldens are what makes "does it match the tier" a question
// with a mechanical answer: render the tier, diff the box's file against the render.

// committedWSLSeats is every committed tier whose lane seat starts through WSL, read from
// setup/templates/profiles.json. It refuses to go blind: the two Blackwell workstation tiers
// are the ones this gate exists for.
func committedWSLSeats(t *testing.T) map[string]Spec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			VLLMSeat *Spec `json:"vllm_seat"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	out := map[string]Spec{}
	for tier, p := range doc.Profiles {
		if p.VLLMSeat != nil && p.VLLMSeat.launch() == LaunchWindowsWSL {
			out[tier] = *p.VLLMSeat
		}
	}
	for _, tier := range []string{"blackwell-2x16", "blackwell-3x16"} {
		if _, ok := out[tier]; !ok {
			t.Fatalf("tier %s declares no windows-wsl vllm_seat — this gate went blind", tier)
		}
	}
	return out
}

// renderCommittedSeat renders one committed tier's lane seat through windows-wsl with the
// synthetic runtime. The snapshot is pinned (as pinned() does) so the render exercises the
// template, not snapshot resolution, which has its own tests.
func renderCommittedSeat(t *testing.T, tier string) (Spec, map[string]string) {
	t.Helper()
	spec, ok := committedWSLSeats(t)[tier]
	if !ok {
		t.Fatalf("no committed windows-wsl seat for tier %s", tier)
	}
	spec.ModelPath = "/hf/" + spec.ModelRepo + "/snapshots/synthetic"
	files, err := spec.Artifacts(wslTemplatesDir(), wslRT())
	if err != nil {
		t.Fatalf("tier %s: the committed seat does not render through windows-wsl: %v", tier, err)
	}
	return spec, files
}

// envValues parses the assignments of a rendered seat env (comments and blank lines skipped,
// one pair of surrounding quotes removed, a leading `export ` ignored).
func envValues(env string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(env, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if n := len(v); n >= 2 && (v[0] == '"' || v[0] == '\'') && v[n-1] == v[0] {
			v = v[1 : n-1]
		}
		out[k] = v
	}
	return out
}

// The operating point each committed tier renders, line by line. The values are the tier's
// own (profiles.json); what this pins is that the TEMPLATE carries them to the file the
// engine reads, which is the join no other test covers.
func TestCommittedTiersRenderTheirMeasuredSeatEnv(t *testing.T) {
	common := map[string]string{
		"SEAT_NAME":               "qwen3.8-27b-vllm",
		"SEAT_ALIAS":              "agent-pool",
		"SEAT_PORT":               "18797",
		"SEAT_MP_PORT":            "18796",
		"SEAT_TP":                 "2",
		"SEAT_PP":                 "",
		"SEAT_PARTITION":          "",
		"SEAT_MAX_LEN":            "163840",
		"SEAT_SEQS":               "32",
		"SEAT_BATCHED":            "3135",
		"SEAT_L1_GB":              "8",
		"SEAT_CHUNK":              "1568",
		"SEAT_LMCACHE_PYTHONPATH": "/opt/seat/lmcache-overlay",
		"SEAT_HF_HOME":            "/hf",
		"SEAT_L2_STATUS_FILE":     "/opt/seat/seat-l2-qwen3.8-27b-vllm.status",
	}
	const fourFlags = "--enable-auto-tool-choice --tool-call-parser qwen3_xml --reasoning-parser qwen3 --kv-cache-dtype fp8"
	cases := map[string]struct {
		devices, util, extraArgs string
	}{
		"blackwell-3x16": {
			devices: "0,2", util: "0.9",
			extraArgs: fourFlags + " --chat-template /opt/seat/templates/qwen3-fold-system.jinja --enable-prompt-tokens-details",
		},
		"blackwell-2x16": {devices: "0,1", util: "0.85", extraArgs: fourFlags},
	}
	for tier, want := range cases {
		t.Run(tier, func(t *testing.T) {
			spec, files := renderCommittedSeat(t, tier)
			env, ok := files[spec.ID+".env"]
			if !ok {
				t.Fatalf("no %s.env rendered; got %v", spec.ID, keys(files))
			}
			got := envValues(env)
			for k, v := range common {
				if got[k] != v {
					t.Errorf("%s = %q, want %q", k, got[k], v)
				}
			}
			for k, v := range map[string]string{
				"SEAT_DEVICES":    want.devices,
				"SEAT_UTIL":       want.util,
				"SEAT_EXTRA_ARGS": want.extraArgs,
			} {
				if got[k] != v {
					t.Errorf("%s = %q, want %q", k, got[k], v)
				}
			}
			// SEAT_TP is the other statement of SEAT_DEVICES' width; the tier schema refuses a
			// pair that disagrees, and the rendered file must not be where they come apart.
			if n := len(strings.Split(got["SEAT_DEVICES"], ",")); strconv.Itoa(n) != got["SEAT_TP"] {
				t.Errorf("SEAT_DEVICES %q names %d card(s) but SEAT_TP is %s", got["SEAT_DEVICES"], n, got["SEAT_TP"])
			}
		})
	}
}

// The agent loop sends tool_choice=auto, and vLLM answers 400 unless the parsers are named,
// so the tail is asserted token by token as well as whole: a duplicated --kv-cache-dtype or a
// value with a trailing comma ("fp8,") is the hand-edit shape a live seat env has carried,
// and the renderer must never be the thing that produces it.
func TestSeatExtraArgsNameTheAgentContractOnceAndCleanly(t *testing.T) {
	for tier := range committedWSLSeats(t) {
		t.Run(tier, func(t *testing.T) {
			spec, files := renderCommittedSeat(t, tier)
			args := strings.Fields(envValues(files[spec.ID+".env"])["SEAT_EXTRA_ARGS"])
			valueOf := func(flag string) string {
				n, v := 0, ""
				for i, a := range args {
					if a == flag {
						n++
						if i+1 < len(args) {
							v = args[i+1]
						}
					}
				}
				if n != 1 {
					t.Errorf("%s appears %d times in %q, want exactly once", flag, n, strings.Join(args, " "))
				}
				return v
			}
			for flag, want := range map[string]string{
				"--tool-call-parser": "qwen3_xml",
				"--reasoning-parser": "qwen3",
				"--kv-cache-dtype":   "fp8",
			} {
				if got := valueOf(flag); got != want {
					t.Errorf("%s = %q, want %q", flag, got, want)
				}
			}
			valueOf("--enable-auto-tool-choice")
			for _, a := range args {
				if strings.HasSuffix(a, ",") {
					t.Errorf("argument %q ends with a comma — a hand-edit shape the render must never produce", a)
				}
			}
		})
	}
}

// A shipped chat template travels with the render and the argument tail names its installed
// path, or the seat env points the engine at a file that is not there (seat_fg.sh refuses to
// start, naming it). The pair tier names no template and must not grow one.
func TestCommittedTierShipsTheChatTemplateItsTailNames(t *testing.T) {
	spec, files := renderCommittedSeat(t, "blackwell-3x16")
	if spec.ChatTemplate != "qwen3-fold-system.jinja" {
		t.Fatalf("the 3-card tier's chat_template is %q; update this gate with the evidence that moved it", spec.ChatTemplate)
	}
	if body, ok := files["templates/"+spec.ChatTemplate]; !ok || body == "" {
		t.Errorf("the render lacks templates/%s; got %v", spec.ChatTemplate, keys(files))
	}
	_, pair := renderCommittedSeat(t, "blackwell-2x16")
	for name := range pair {
		if strings.HasPrefix(name, "templates/") {
			t.Errorf("the pair tier names no chat template, yet the render carries %s", name)
		}
	}
}

// concurrencyLimit is not a template FILE in the windows-wsl set (git grep of the hand
// templates finds it only under linux-systemd/), so the premise "the reference box's entry has it" is
// carried by Entry alone. llama-swap's per-model default (10) answers 429 to the streams a
// 32-slot seat was provisioned for, so the entry's limit must equal the engine's --max-num-seqs
// for every committed tier that launches through WSL.
func TestWindowsEntryConcurrencyLimitEqualsMaxNumSeqsForEveryWSLTier(t *testing.T) {
	limit := regexp.MustCompile(`(?m)^\s+concurrencyLimit: (\d+)\s*$`)
	for tier, spec := range committedWSLSeats(t) {
		t.Run(tier, func(t *testing.T) {
			entry := spec.Entry(wslRT())
			m := limit.FindStringSubmatch(entry)
			if m == nil {
				t.Fatalf("the windows-wsl entry has no concurrencyLimit:\n%s", entry)
			}
			if got, _ := strconv.Atoi(m[1]); got != spec.MaxNumSeqs {
				t.Errorf("concurrencyLimit %d != max_num_seqs %d", got, spec.MaxNumSeqs)
			}
			// The measured stream count of the workstation seat; a change has to justify itself.
			if m[1] != "32" {
				t.Errorf("concurrencyLimit %s, the workstation seat's measured fan-out is 32 streams", m[1])
			}
			for _, want := range []string{"ttl: 300", `useModelName: "` + spec.ID + `"`} {
				if !strings.Contains(entry, want) {
					t.Errorf("entry lacks %q:\n%s", want, entry)
				}
			}
		})
	}
}

// setup/templates/vllm-seat/llama-swap-entry.yaml is the hand-install reference for a WSL
// seat. Nothing renders it and nothing read it, and it drifted: ttl 1800 against the house
// idle window of 300 (the operator's rule: no model stays loaded past five idle minutes),
// and no concurrencyLimit, so a hand-installed 32-slot seat answered 429 from the eleventh
// stream. It must declare what Spec.Entry renders, value for value where the value is not a
// placeholder.
func TestVLLMSeatReferenceEntryDeclaresWhatEntryRenders(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "vllm-seat", "llama-swap-entry.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	parse := func(name, body string) (string, map[string]any) {
		var doc map[string]map[string]any
		if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatalf("%s is not a llama-swap models fragment: %v", name, err)
		}
		if len(doc) != 1 {
			t.Fatalf("%s declares %d models, want exactly one", name, len(doc))
		}
		for id, m := range doc {
			return id, m
		}
		return "", nil
	}
	_, ref := parse("the reference", string(raw))
	spec, _ := renderCommittedSeat(t, "blackwell-3x16")
	_, want := parse("Spec.Entry", spec.Entry(wslRT()))

	var missing []string
	for k := range want {
		if _, ok := ref[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the reference entry does not declare %s, which Spec.Entry renders", strings.Join(missing, ", "))
	}
	for _, k := range []string{"checkEndpoint", "ttl", "unloadTimeout", "concurrencyLimit", "proxy"} {
		if got, w := ref[k], want[k]; got != w {
			t.Errorf("reference %s = %v, but Spec.Entry renders %v", k, got, w)
		}
	}
	// cmd and cmdStop differ by path placeholder, so the comparison is the program and the stub
	// they run: a hand entry that runs bash through wsl.exe cannot start the distro from SYSTEM.
	for k, stub := range map[string]string{"cmd": "seat-cmd.ps1", "cmdStop": "seat-cmdstop.ps1"} {
		for name, m := range map[string]map[string]any{"the reference": ref, "Spec.Entry": want} {
			if got, _ := m[k].(string); !strings.Contains(got, "powershell.exe") || !strings.Contains(got, stub) {
				t.Errorf("%s %s = %q, want the PowerShell stub %s", name, k, got, stub)
			}
		}
	}
	refID, _ := parse("the reference", string(raw))
	if got := ref["useModelName"]; got != refID {
		t.Errorf("reference useModelName = %v, want its own model id %q (vLLM 404s every name it does not serve)", got, refID)
	}
	if aliases, _ := ref["aliases"].([]any); len(aliases) == 0 || aliases[0] != "agent-pool" {
		t.Errorf("reference aliases = %v, want the harness's agent-pool alias first", ref["aliases"])
	}
}
