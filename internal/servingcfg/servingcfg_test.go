package servingcfg

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The reader is the F15 parser, moved here from internal/gpualloc with its tests (register C-86a) so
// that the load fence and the unload list read the serving config through one parser. These ids are
// synthetic (the repeated-nibble heads of the card fixtures).

const (
	card0 = "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee"
	card2 = "GPU-cccc3333-dddd-eeee-ffff-000000000000"
)

const servingYAML = `
models:
  yaml-card0:
    aliases: [yaml-card0-alias]
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=` + card0 + `"]
    cmd: llama-server --port 1
  yaml-card2:
    env: ["CUDA_VISIBLE_DEVICES=` + card2 + `"]
  yaml-idx0:
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=0"]
  yaml-idx-noorder:
    env: ["CUDA_VISIBLE_DEVICES=0"]
  yaml-pair:
    env: ["CUDA_VISIBLE_DEVICES=` + card0 + `,` + card2 + `"]
  yaml-unpinned:
    env: ["FOO=bar"]
    cmd: llama-server --device CUDA0 -sm layer
  yaml-cmd-only:
    cmd: CUDA_VISIBLE_DEVICES=` + card0 + ` llama-server
  yaml-proxy:
    proxy: http://127.0.0.1:1234
    env: ["CUDA_VISIBLE_DEVICES=` + card0 + `"]
  yaml-macro:
    env: ["CUDA_VISIBLE_DEVICES=${gpu_pin}"]
  on-card-0:
    env: ["CUDA_VISIBLE_DEVICES=` + card2 + `"]
`

func writeServing(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "llama-swap.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// oneModel is a serving config of one model, "m", pinned to card. Every card id has the same length,
// so two of these differ in content and not in size.
func oneModel(card string) string {
	return "models:\n  m:\n    env: [\"CUDA_VISIBLE_DEVICES=" + card + "\"]\n"
}

// settle gives the file a modification time long past the racy window, as a config llama-swap has
// been serving for a while has, so that a reading of it is trusted by (time, size).
func settle(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func rewrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func pinOfM(t *testing.T, path string) string {
	t.Helper()
	pins, ok := PinsOf(path, "m")
	if !ok {
		return ""
	}
	return strings.Join(pins, ",")
}

func TestServingPinsReadsOnlyTheEnvPin(t *testing.T) {
	pins, err := Pins(writeServing(t, servingYAML))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		pins string // comma-joined, "" = no usable pin
		why  string // a fragment of the reason when there is no pin
	}{
		{"yaml-card0", card0, ""},
		{"yaml-card0-alias", card0, ""},
		{"yaml-card2", card2, ""},
		{"yaml-idx0", "0", ""},
		{"yaml-idx-noorder", "", "PCI_BUS_ID"},
		{"yaml-pair", card0 + "," + card2, ""},
		{"yaml-unpinned", "", "pins no CUDA_VISIBLE_DEVICES in env"},
		{"yaml-cmd-only", "", "pins no CUDA_VISIBLE_DEVICES in env"},
		{"yaml-proxy", "", "proxy: seat"},
		{"yaml-macro", "", "not a literal device list"},
	} {
		p, ok := pins[tc.name]
		if !ok {
			t.Errorf("%s: not read", tc.name)
			continue
		}
		if got := strings.Join(p.Pins, ","); got != tc.pins {
			t.Errorf("%s: pins = %q, want %q (why %q)", tc.name, got, tc.pins, p.Why)
		}
		if tc.why != "" && !strings.Contains(p.Why, tc.why) {
			t.Errorf("%s: why = %q, want it to mention %q", tc.name, p.Why, tc.why)
		}
	}
}

func TestServingPinsRefusesAnAliasTwoEntriesClaimDifferently(t *testing.T) {
	pins, err := Pins(writeServing(t, `
models:
  a:
    aliases: [shared]
    env: ["CUDA_VISIBLE_DEVICES=`+card0+`"]
  b:
    aliases: [shared]
    env: ["CUDA_VISIBLE_DEVICES=`+card2+`"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if p := pins["shared"]; len(p.Pins) != 0 || !strings.Contains(p.Why, "two entries") {
		t.Errorf("an alias two entries claim with different pins is unknown, got %+v", p)
	}
	if len(pins["a"].Pins) != 1 || len(pins["b"].Pins) != 1 {
		t.Errorf("the ids themselves stay placed: a=%+v b=%+v", pins["a"], pins["b"])
	}
}

func TestServingPinsErrors(t *testing.T) {
	if _, err := Pins(""); err == nil {
		t.Error("an unset path is an error")
	}
	if _, err := Pins(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("a missing file is an error")
	}
	if _, err := Pins(writeServing(t, "models: [unclosed")); err == nil {
		t.Error("a file that is not YAML is an error")
	}
}

func TestIDsListsTheDeclaredModelsAndNotTheirAliases(t *testing.T) {
	got := IDs(writeServing(t, servingYAML))
	want := []string{"on-card-0", "yaml-card0", "yaml-card2", "yaml-cmd-only", "yaml-idx-noorder", "yaml-idx0", "yaml-macro", "yaml-pair", "yaml-proxy", "yaml-unpinned"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("IDs = %v, want %v (sorted, no alias)", got, want)
	}
	if got := IDs(filepath.Join(t.TempDir(), "gone.yaml")); got != nil {
		t.Errorf("IDs of a file that cannot be read = %v, want none", got)
	}
}

// PinsOf is the fence's question. Its answer is the caller's own copy: the run registry keeps what
// the fence hands it, and an edit there must not reach the next answer.
func TestPinsOfPlacesOnlyWhatTheEnvPinsAndHandsBackACopy(t *testing.T) {
	p := writeServing(t, servingYAML)
	settle(t, p) // a file edited moments ago is never memoised, and the copy is the memo's to protect
	if got, ok := PinsOf(p, "  YAML-CARD0-ALIAS "); !ok || !reflect.DeepEqual(got, []string{card0}) {
		t.Errorf("PinsOf(alias) = %v, %v; names compare case-insensitively and trimmed", got, ok)
	}
	for _, m := range []string{"yaml-idx-noorder", "yaml-proxy", "yaml-unpinned", "yaml-macro", "yaml-cmd-only", "not-listed", ""} {
		if got, ok := PinsOf(p, m); ok || got != nil {
			t.Errorf("PinsOf(%q) = %v, %v; want unknown", m, got, ok)
		}
	}
	if got, ok := PinsOf("", "yaml-card0"); ok || got != nil {
		t.Errorf("PinsOf with no path = %v, %v; want unknown", got, ok)
	}
	first, _ := PinsOf(p, "yaml-pair")
	first[0] = "GPU-scribbled"
	if again, _ := PinsOf(p, "yaml-pair"); strings.Contains(strings.Join(again, ","), "scribbled") {
		t.Errorf("a caller's edit reached the next answer: %v", again)
	}
}

// ---- the memo: re-read when the yaml changes, not before ------------------------------------------

func TestLoadParsesOnceWhileTheFileIsUnchanged(t *testing.T) {
	p := writeServing(t, servingYAML)
	settle(t, p)
	before := parsed.Load()
	for i := 0; i < 5; i++ {
		if _, err := Load(p); err != nil {
			t.Fatal(err)
		}
		if _, ok := PinsOf(p, "yaml-card0"); !ok {
			t.Fatal("yaml-card0 is placed")
		}
	}
	if n := parsed.Load() - before; n != 1 {
		t.Fatalf("ten asks of an unchanged file parsed it %d times, want 1", n)
	}
}

// Editing the yaml changes the answer on the next call, without a restart. The size differs here and
// the modification time is put back, so only the size can tell.
func TestTheServingPinsAreReReadWhenTheYamlChangesSize(t *testing.T) {
	p := writeServing(t, oneModel(card0))
	settle(t, p)
	if got := pinOfM(t, p); got != card0 {
		t.Fatalf("first read = %q, want %q", got, card0)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	rewrite(t, p, oneModel(card2)+"# moved to the other card\n")
	if err := os.Chtimes(p, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got := pinOfM(t, p); got != card2 {
		t.Fatalf("after an edit that changed only the size, the answer is %q, want %q", got, card2)
	}
}

// And the other way round: the size is the same (every card id has the same length) and only the
// modification time moves, as a reload that rewrites the file in place does.
func TestTheServingPinsAreReReadWhenTheYamlChangesModTime(t *testing.T) {
	p := writeServing(t, oneModel(card0))
	settle(t, p)
	if got := pinOfM(t, p); got != card0 {
		t.Fatalf("first read = %q, want %q", got, card0)
	}
	rewrite(t, p, oneModel(card2))
	later := time.Now().Add(-30 * time.Minute)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
	if got := pinOfM(t, p); got != card2 {
		t.Fatalf("after an edit that changed only the modification time, the answer is %q, want %q", got, card2)
	}
}

// A file changed less than racyWindow before the read cannot be told from a rewrite in the same clock
// tick (same size, same modification time), so it is not trusted until it has been quiet.
func TestLoadDoesNotTrustAReadOfAFileChangedInsideTheRacyWindow(t *testing.T) {
	p := writeServing(t, oneModel(card0))
	now := time.Now()
	prev := clock
	clock = func() time.Time { return now }
	t.Cleanup(func() { clock = prev })
	justBefore := now.Add(-time.Second)
	if err := os.Chtimes(p, justBefore, justBefore); err != nil {
		t.Fatal(err)
	}
	if got := pinOfM(t, p); got != card0 {
		t.Fatalf("first read = %q, want %q", got, card0)
	}
	// Rewritten to the same size and put back to the same modification time: nothing but the content differs.
	rewrite(t, p, oneModel(card2))
	if err := os.Chtimes(p, justBefore, justBefore); err != nil {
		t.Fatal(err)
	}
	if got := pinOfM(t, p); got != card2 {
		t.Fatalf("a rewrite inside the racy window was served from the memo: %q, want %q", got, card2)
	}
	// Once the file has been quiet for the window the reading is trusted again, and parsed once.
	clock = func() time.Time { return now.Add(10 * time.Second) }
	before := parsed.Load()
	for i := 0; i < 3; i++ {
		if got := pinOfM(t, p); got != card2 {
			t.Fatalf("settled read = %q, want %q", got, card2)
		}
	}
	if n := parsed.Load() - before; n != 1 {
		t.Fatalf("a settled file parsed %d times over three asks, want 1", n)
	}
}

// The direction of every doubt is "no placement": a memo is never served for a file that is gone.
func TestLoadServesNoMemoForAFileThatIsGone(t *testing.T) {
	p := writeServing(t, oneModel(card0))
	settle(t, p)
	if got := pinOfM(t, p); got != card0 {
		t.Fatalf("first read = %q, want %q", got, card0)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("Load served a memo for a file that is gone")
	}
	if got := pinOfM(t, p); got != "" {
		t.Fatalf("PinsOf placed a model by a file that is gone: %q", got)
	}
}

// The sentence an operator reads for an unreadable serving config is the one it always was: the
// failed read, "open <path>: ...", not the Stat that stands in for it.
func TestLoadWordsAMissingFileAsAFailedOpen(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "gone.yaml"))
	if err == nil || !strings.HasPrefix(err.Error(), "open ") {
		t.Fatalf("Load of a missing file = %v, want an error that starts with %q", err, "open ")
	}
	if _, err := Load("   "); err == nil || !strings.Contains(err.Error(), "serving_config_path is unset") {
		t.Fatalf("Load of a blank path = %v, want the unset error", err)
	}
}

// A file that is not YAML is reported as often as it is asked about but parsed once; the fixed file is read.
func TestLoadReportsAParseErrorWithoutParsingAgainAndRecoversWhenTheFileIsFixed(t *testing.T) {
	p := writeServing(t, "models: [unclosed")
	settle(t, p)
	before := parsed.Load()
	for i := 0; i < 3; i++ {
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "not parseable YAML") {
			t.Fatalf("Load of a broken file = %v, want the parse error", err)
		}
	}
	if n := parsed.Load() - before; n != 1 {
		t.Fatalf("a broken file was parsed %d times over three asks, want 1", n)
	}
	rewrite(t, p, oneModel(card2))
	settle(t, p)
	if got := pinOfM(t, p); got != card2 {
		t.Fatalf("after the fix the answer is %q, want %q", got, card2)
	}
}

func TestLoadMemoIsBounded(t *testing.T) {
	for i := 0; i < maxCached+5; i++ {
		p := writeServing(t, oneModel(card0))
		settle(t, p)
		if _, err := Load(p); err != nil {
			t.Fatal(err)
		}
	}
	cacheMu.Lock()
	n := len(cache)
	cacheMu.Unlock()
	if n > maxCached {
		t.Fatalf("the memo holds %d files, want at most %d", n, maxCached)
	}
}
