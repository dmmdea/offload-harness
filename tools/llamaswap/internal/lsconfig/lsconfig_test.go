// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package lsconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureConfig mirrors the shapes that matter on a real deployment: a macro
// referencing a reserved token, a whisper seat that must take the escape
// hatch, a keep-resident seat, a matrix, and comment blocks separated by blank
// lines so the header-attribution rule is exercised.
const fixtureConfig = `# top of file banner
healthCheckTimeout: 300
startPort: 9200

macros:
  server: "/opt/llama.cpp/llama-server --port ${PORT} --host 127.0.0.1"
  mdir:   "/models"
  unused: "nothing references me"

models:
  # A note about a model that was DELETED. It belongs to nobody below.

  # ===== the workhorse =====
  # -c 65536 raised from 8192 on evidence.
  "worker":
    cmd: "${server} -m ${mdir}/worker.gguf -ngl 99 -c 65536"   # ctx raised 2026-08-03
    aliases: ["work", "w"]
    ttl: 300
    name: "worker"

  "resident":
    cmd: "${server} -m ${mdir}/resident.gguf --embeddings --pooling mean"
    ttl: -1
    aliases: ["embed"]

  # ===== STT — own binary, not llama-server =====
  "stt":
    cmd: "/opt/whisper.cpp/whisper-server -m ${mdir}/ggml-large.bin -vm ${mdir}/vad.bin --vad -nfa --host 127.0.0.1 --port ${PORT}"
    checkEndpoint: /health
    ttl: 300
    aliases: ["whisper"]

matrix:
  vars:
    w: worker
    r: resident
    s: stt
  evict_costs:
    r: 1000
  sets:
    residents: "r"
    interactive: "+residents & (w | s)"
`

func parseFixture(t *testing.T, body string) *File {
	t.Helper()
	f, err := ParseBytes([]byte(body), "fixture.yaml", LoadOptions{
		LookupEnv: func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	return f
}

func TestParseModelsAndProvenance(t *testing.T) {
	f := parseFixture(t, fixtureConfig)
	if len(f.Models) != 3 {
		t.Fatalf("models = %d, want 3", len(f.Models))
	}
	if f.StartPort != 9200 {
		t.Errorf("startPort = %d, want 9200", f.StartPort)
	}
	w := f.ModelIndex["worker"]
	if w == nil {
		t.Fatal("worker not parsed")
	}
	if got := w.CmdExpanded; !strings.HasPrefix(got, "/opt/llama.cpp/llama-server --port ${PORT}") {
		t.Errorf("worker expanded cmd = %q", got)
	}
	if !strings.Contains(w.CmdExpanded, "/models/worker.gguf") {
		t.Errorf("mdir macro not expanded: %q", w.CmdExpanded)
	}

	// The header block must stop at the blank line: the "DELETED" note above
	// belongs to nobody and must NOT be attributed to worker.
	if strings.Contains(w.HeaderComment, "DELETED") {
		t.Errorf("worker header swallowed an unrelated comment block:\n%s", w.HeaderComment)
	}
	if !strings.Contains(w.HeaderComment, "the workhorse") {
		t.Errorf("worker header missing its own block:\n%s", w.HeaderComment)
	}
	if !strings.Contains(w.RawBlock(), `"worker":`) || !strings.Contains(w.RawBlock(), "ctx raised 2026-08-03") {
		t.Errorf("worker raw block incomplete:\n%s", w.RawBlock())
	}
	foundInline := false
	for _, ic := range w.InlineComments {
		if ic.Key == "cmd" && strings.Contains(ic.Text, "ctx raised") {
			foundInline = true
		}
	}
	if !foundInline {
		t.Errorf("inline comment on cmd not captured: %+v", w.InlineComments)
	}
}

func TestResolveByAlias(t *testing.T) {
	f := parseFixture(t, fixtureConfig)
	for _, name := range []string{"worker", "work", "w"} {
		m, ok := f.Resolve(name)
		if !ok || m.ID != "worker" {
			t.Errorf("Resolve(%q) = %v, %v", name, m, ok)
		}
	}
	if _, ok := f.Resolve("nope"); ok {
		t.Error("Resolve(nope) should fail")
	}
}

// TestWhisperEscapeHatch is the load-bearing classification test. A seat whose
// binary is not llama-server must be SeatNonLlamaServer, must NOT be offered
// llama-server context flags, and must NOT collect a single error or warning
// from lint on an otherwise-healthy config.
func TestWhisperEscapeHatch(t *testing.T) {
	f := parseFixture(t, fixtureConfig)
	stt := f.ModelIndex["stt"]
	if stt.Seat != SeatNonLlamaServer {
		t.Fatalf("stt seat kind = %q, want %q", stt.Seat, SeatNonLlamaServer)
	}
	if f.ModelIndex["worker"].Seat != SeatLlamaServer {
		t.Fatalf("worker seat kind = %q, want llama-server", f.ModelIndex["worker"].Seat)
	}
	if ContextFlagsFor(stt.Seat) != nil {
		t.Error("context flags must not apply to a non-llama-server seat")
	}
	if !containsString(FileFlagsFor(stt.Seat, stt.Binary), "-vm") {
		t.Error("whisper seat should have its VAD-model flag checked")
	}

	rep := Lint(f, LintOptions{
		Stat:      func(string) (os.FileInfo, error) { return nil, nil },
		LookupEnv: func(string) (string, bool) { return "", false },
	})
	for _, fd := range rep.Findings {
		if fd.Model != "stt" {
			continue
		}
		if fd.Severity == SevError || fd.Severity == SevWarn {
			t.Errorf("FALSE POSITIVE on the non-llama-server seat: [%s] %s: %s", fd.Severity, fd.Check, fd.Message)
		}
	}
	skips := 0
	for _, fd := range rep.Findings {
		if fd.Model == "stt" && fd.Severity == SevSkipped {
			skips++
		}
	}
	if skips == 0 {
		t.Error("the escape hatch must emit explicit skipped notes; silence reads as a pass")
	}
}

func TestLintCleanConfigHasNoErrors(t *testing.T) {
	f := parseFixture(t, fixtureConfig)
	rep := Lint(f, LintOptions{
		Stat:      func(string) (os.FileInfo, error) { return nil, nil },
		LookupEnv: func(string) (string, bool) { return "", false },
	})
	if rep.Errors != 0 {
		for _, fd := range rep.Findings {
			if fd.Severity == SevError {
				t.Errorf("unexpected error: %s: %s", fd.Check, fd.Message)
			}
		}
	}
	if !hasCheck(rep, "macro.unused") {
		t.Error("unused macro not reported")
	}
	if !hasCheck(rep, "ttl.keep-resident") {
		t.Error("ttl:-1 keep-resident note not reported")
	}
	if !hasCheck(rep, "store.path-unset") {
		t.Error("store.path INFO not reported")
	}
	if !hasCheckSeverity(rep, "model.dead", SevSkipped) {
		t.Error("dead-model detection must report SKIPPED, not silently pass")
	}
}

func TestLintEvictCostsKeyedByModelID(t *testing.T) {
	body := strings.Replace(fixtureConfig, "  evict_costs:\n    r: 1000", "  evict_costs:\n    resident: 1000", 1)
	f := parseFixture(t, body)
	rep := Lint(f, LintOptions{Stat: func(string) (os.FileInfo, error) { return nil, nil }})
	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == "matrix.evict-cost-key" {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatal("evict_costs keyed by a model id must be an error")
	}
	if found.Severity != SevError {
		t.Errorf("severity = %q, want error", found.Severity)
	}
	// The message must cite llama-swap's own rejection so an operator can grep
	// their boot log for it.
	if !strings.Contains(found.Message, "unknown var ID") {
		t.Errorf("message should quote llama-swap's rejection: %s", found.Message)
	}
	if !strings.Contains(found.Message, `Use "r"`) {
		t.Errorf("message should suggest the right var id: %s", found.Message)
	}
}

func TestLintDuplicateAliasAndShadow(t *testing.T) {
	body := strings.Replace(fixtureConfig, `aliases: ["embed"]`, `aliases: ["embed", "w"]`, 1)
	f := parseFixture(t, body)
	rep := Lint(f, LintOptions{Stat: func(string) (os.FileInfo, error) { return nil, nil }})
	if !hasCheckSeverity(rep, "alias.duplicate", SevError) {
		t.Error("duplicate alias must be an error")
	}

	body2 := strings.Replace(fixtureConfig, `aliases: ["embed"]`, `aliases: ["embed", "worker"]`, 1)
	f2 := parseFixture(t, body2)
	rep2 := Lint(f2, LintOptions{Stat: func(string) (os.FileInfo, error) { return nil, nil }})
	if !hasCheckSeverity(rep2, "alias.shadows-id", SevError) {
		t.Error("an alias equal to another model's id must be an error")
	}
}

func TestLintHardcodedPortInSpan(t *testing.T) {
	body := strings.Replace(fixtureConfig,
		`cmd: "${server} -m ${mdir}/resident.gguf --embeddings --pooling mean"`,
		`cmd: "/opt/llama.cpp/llama-server --port 9205 -m ${mdir}/resident.gguf --embeddings"`, 1)
	f := parseFixture(t, body)
	rep := Lint(f, LintOptions{
		Stat:           func(string) (os.FileInfo, error) { return nil, nil },
		CheckListeners: true,
		ListenerProbe:  func(int) (bool, error) { return true, nil },
	})
	if !hasCheckSeverity(rep, "port.span-collision", SevError) {
		t.Error("a hardcoded port inside the startPort span must be an error")
	}
	if !hasCheckSeverity(rep, "port.listener-busy", SevError) {
		t.Error("a live listener on a hardcoded port must be an error")
	}
	// ${PORT} is the CORRECT spelling and must never be flagged.
	for _, fd := range rep.Findings {
		if fd.Check == "port.hardcoded" && fd.Model == "worker" {
			t.Error("${PORT} must not be reported as a hardcoded port")
		}
	}
}

func TestLintUndefinedAndEnvMacros(t *testing.T) {
	body := strings.Replace(fixtureConfig,
		`cmd: "${server} -m ${mdir}/worker.gguf -ngl 99 -c 65536"`,
		`cmd: "${server} -m ${missing}/worker.gguf -ngl 99 -c ${env.CTX_SIZE}"`, 1)
	f := parseFixture(t, body)
	rep := Lint(f, LintOptions{
		Stat:      func(string) (os.FileInfo, error) { return nil, nil },
		LookupEnv: func(string) (string, bool) { return "", false },
	})
	if !hasCheckSeverity(rep, "macro.undefined", SevError) {
		t.Error("an undefined macro must be an error")
	}
	if !hasCheckSeverity(rep, "macro.env-unset", SevWarn) {
		t.Error("an unset ${env.VAR} must be a WARNING (the service env may differ), not an error")
	}
}

func TestExpanderCycleDetection(t *testing.T) {
	e := NewExpander(map[string]string{
		"a": "${b}",
		"b": "${c}",
		"c": "${a}",
	}, func(string) (string, bool) { return "", false })
	if cycles := e.MacroCycles(); len(cycles) == 0 {
		t.Fatal("cycle not detected")
	}
	_, _, errs := e.Expand("run ${a}")
	if len(errs) == 0 {
		t.Fatal("expanding through a cycle must report an error rather than loop")
	}
}

func TestExpanderReservedAndEnv(t *testing.T) {
	e := NewExpander(map[string]string{"bin": "/opt/llama-server --port ${PORT}"},
		func(k string) (string, bool) {
			if k == "MODELS" {
				return "/mnt/models", true
			}
			return "", false
		})
	got, subs, errs := e.Expand("${bin} -m ${env.MODELS}/x.gguf --id ${MODEL_ID} --miss ${env.NOPE}")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if !strings.Contains(got, "/mnt/models/x.gguf") {
		t.Errorf("env macro not expanded: %q", got)
	}
	if !strings.Contains(got, "${PORT}") || !strings.Contains(got, "${MODEL_ID}") {
		t.Errorf("reserved macros must stay symbolic: %q", got)
	}
	if !strings.Contains(got, "${env.NOPE}") {
		t.Errorf("an unset env macro must stay symbolic rather than silently become empty: %q", got)
	}
	kinds := map[string]string{}
	for _, s := range subs {
		kinds[s.Name] = s.Kind
	}
	for name, want := range map[string]string{
		"bin": "macro", "env.MODELS": "env", "PORT": "reserved",
		"MODEL_ID": "reserved", "env.NOPE": "env-unset",
	} {
		if kinds[name] != want {
			t.Errorf("expansion kind for %s = %q, want %q", name, kinds[name], want)
		}
	}
}

func TestParseCmdAndPortNormalization(t *testing.T) {
	spec := ParseCmd(`/opt/llama-server --port 9201 --host 127.0.0.1 -m "/models/a b.gguf" -ngl 99 --temp -1 --flag`)
	if spec.Binary != "/opt/llama-server" {
		t.Errorf("binary = %q", spec.Binary)
	}
	if f, ok := spec.Get("-m"); !ok || f.Values[0] != "/models/a b.gguf" {
		t.Errorf("quoted path not tokenized: %+v", f)
	}
	// A negative number is a VALUE, not a flag; otherwise a diff invents a
	// phantom flag change.
	if f, ok := spec.Get("--temp"); !ok || len(f.Values) != 1 || f.Values[0] != "-1" {
		t.Errorf("negative value mis-parsed: %+v", f)
	}
	if f, ok := spec.Get("--flag"); !ok || len(f.Values) != 0 {
		t.Errorf("valueless flag mis-parsed: %+v", f)
	}

	// The runtime-assigned port must never read as drift.
	deltas := DiffCmds(
		"/opt/llama-server --port ${PORT} -m /models/a.gguf -ngl 99",
		"/opt/llama-server --port 9201 -m /models/a.gguf -ngl 99")
	if len(deltas) != 0 {
		t.Errorf("port substitution reported as drift: %+v", deltas)
	}
}

func TestDiffCmdsDetectsRealChanges(t *testing.T) {
	deltas := DiffCmds(
		"/opt/llama-server --port ${PORT} -m /models/a.gguf -c 8192 --jinja",
		"/opt/llama-server --port 9201 -m /models/a.gguf -c 65536 -ngl 99")
	got := map[string]string{}
	for _, d := range deltas {
		got[d.Flag] = d.Kind
	}
	if got["-c"] != "changed" {
		t.Errorf("-c change missed: %+v", deltas)
	}
	if got["-ngl"] != "added" {
		t.Errorf("-ngl addition missed: %+v", deltas)
	}
	if got["--jinja"] != "removed" {
		t.Errorf("--jinja removal missed: %+v", deltas)
	}
}

func TestDiffConfigsSemanticAndComments(t *testing.T) {
	a := parseFixture(t, fixtureConfig)
	b := parseFixture(t, strings.Replace(fixtureConfig,
		"-ngl 99 -c 65536\"   # ctx raised 2026-08-03",
		"-ngl 99 -c 131072\"   # ctx MAXED 2026-08-05", 1))
	d := DiffConfigs(a, b)
	var worker *ModelDiff
	for i := range d.Models {
		if d.Models[i].Model == "worker" {
			worker = &d.Models[i]
		}
	}
	if worker == nil || worker.Status != "changed" {
		t.Fatalf("worker diff = %+v", worker)
	}
	if len(worker.FlagDeltas) != 1 || worker.FlagDeltas[0].Flag != "-c" {
		t.Errorf("flag delta = %+v", worker.FlagDeltas)
	}
	for i := range d.Models {
		if d.Models[i].Model != "worker" && d.Models[i].Changed() {
			t.Errorf("unrelated model reported as changed: %+v", d.Models[i])
		}
	}
}

func TestValidateEmbeddedSchema(t *testing.T) {
	f := parseFixture(t, fixtureConfig)
	res, err := Validate(f)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !res.Valid {
		t.Fatalf("clean fixture failed validation: %+v", res.Issues)
	}
	if res.SchemaForVersion == "" || res.SchemaRetrieved == "" {
		t.Error("validation result must carry the schema's provenance")
	}
	if len(KnownTopLevelKeys()) < 10 {
		t.Errorf("embedded schema yielded %d top-level keys", len(KnownTopLevelKeys()))
	}
}

func TestValidateUnknownTopLevelKeyWithSuggestion(t *testing.T) {
	f := parseFixture(t, strings.Replace(fixtureConfig, "startPort: 9200", "startport: 9200", 1))
	res, err := Validate(f)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res.Valid {
		t.Fatal("a misspelled top-level key must be reported (upstream's schema alone lets it pass)")
	}
	found := false
	for _, is := range res.Issues {
		if is.Pointer == "/startport" {
			found = true
			if is.Suggestion != "startPort" {
				t.Errorf("suggestion = %q, want startPort", is.Suggestion)
			}
			if is.Line == 0 {
				t.Error("issue should carry a source line")
			}
		}
	}
	if !found {
		t.Errorf("unknown key not reported: %+v", res.Issues)
	}
}

func TestValidateRejectsBadSchemaShape(t *testing.T) {
	// models must be a mapping of objects with a required cmd.
	body := strings.Replace(fixtureConfig,
		`    cmd: "${server} -m ${mdir}/resident.gguf --embeddings --pooling mean"`+"\n"+`    ttl: -1`,
		`    ttl: -1`, 1)
	f, err := ParseBytes([]byte(body), "bad.yaml", LoadOptions{})
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	res, verr := Validate(f)
	if verr != nil {
		t.Fatalf("Validate: %v", verr)
	}
	if res.Valid {
		t.Fatal("a model with no cmd must fail schema validation")
	}
}

func TestNearestKey(t *testing.T) {
	known := KnownTopLevelKeys()
	if got, ok := NearestKey("macro", known); !ok || got != "macros" {
		t.Errorf("NearestKey(macro) = %q,%v", got, ok)
	}
	if got, ok := NearestKey("globalttl", known); !ok || got != "globalTTL" {
		t.Errorf("NearestKey(globalttl) = %q,%v", got, ok)
	}
	if _, ok := NearestKey("completely_unrelated_nonsense", known); ok {
		t.Error("a far-away key should get no suggestion")
	}
}

// ---------------------------------------------------------------- corpus

func writeCorpus(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	live := filepath.Join(dir, "llama-swap.yaml")
	mustWrite(t, live, fixtureConfig, time.Date(2026, 8, 11, 3, 17, 0, 0, time.Local))

	v1 := strings.Replace(fixtureConfig, "-c 65536", "-c 8192", 1)
	v2 := strings.Replace(fixtureConfig, "-c 65536", "-c 32768", 1)

	// Two byte-identical copies with different labels.
	mustWrite(t, filepath.Join(dir, "backup-2026-08-01-a.yaml"), v1, time.Date(2026, 8, 1, 10, 0, 0, 0, time.Local))
	mustWrite(t, filepath.Join(dir, "backup-2026-08-01-b.yaml"), v1, time.Date(2026, 8, 1, 11, 0, 0, 0, time.Local))
	// A label whose date disagrees with its mtime.
	mustWrite(t, filepath.Join(dir, "backup-2026-08-09-mislabeled.yaml"), v2, time.Date(2026, 8, 5, 9, 0, 0, 0, time.Local))
	// A non-.yaml-suffixed copy a glob would miss.
	mustWrite(t, filepath.Join(dir, "llama-swap.yaml.pre-matrix"), v2+"\n# variant\n", time.Date(2026, 8, 6, 9, 0, 0, 0, time.Local))
	// A copy in a dated SUBDIRECTORY: the directory's date must NOT be read as
	// the file's label.
	sub := filepath.Join(dir, "backup-2026-07-25")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(sub, "llama-swap.yaml"), v1+"\n# older\n", time.Date(2026, 7, 22, 21, 38, 0, 0, time.Local))
	// An orphan backup: byte-identical to LIVE.
	mustWrite(t, filepath.Join(dir, "backup-2026-08-12-never-applied.yaml"), fixtureConfig, time.Date(2026, 8, 12, 22, 20, 0, 0, time.Local))
	// Decoys that must be rejected.
	mustWrite(t, filepath.Join(dir, "docker-compose.yaml"), "services:\n  web:\n    image: nginx\n", time.Now())
	mustWrite(t, filepath.Join(dir, "notes.txt"), "models: not yaml\n", time.Now())
	return live
}

func mustWrite(t *testing.T, path, body string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverCorpus(t *testing.T) {
	live := writeCorpus(t)
	c, err := DiscoverCorpus(live, DiscoverOptions{})
	if err != nil {
		t.Fatalf("DiscoverCorpus: %v", err)
	}
	if len(c.Historical) != 6 {
		var names []string
		for _, s := range c.Historical {
			names = append(names, s.Rel)
		}
		t.Fatalf("historical = %d (%v), want 6", len(c.Historical), names)
	}
	if len(c.FlatHistorical) != 5 {
		t.Errorf("flat historical = %d, want 5", len(c.FlatHistorical))
	}
	// 5 flat files, one byte-identical pair -> 4 distinct content states.
	if c.DistinctFlatStates != 4 {
		t.Errorf("distinct flat states = %d, want 4", c.DistinctFlatStates)
	}
	if len(c.IdenticalPairs) != 1 || len(c.IdenticalPairs[0].Paths) != 2 {
		t.Errorf("identical pairs = %+v", c.IdenticalPairs)
	}
	if len(c.OrphanBackups) != 1 || !strings.Contains(c.OrphanBackups[0], "never-applied") {
		t.Errorf("orphan backups = %v", c.OrphanBackups)
	}

	// Exactly one label/mtime mismatch — and the SUBDIRECTORY's date must not
	// manufacture a second one.
	if len(c.LabelMismatches) != 1 {
		var got []string
		for _, s := range c.LabelMismatches {
			got = append(got, s.Rel+" label="+s.LabelDate+" mtime="+s.MTimeDate)
		}
		t.Fatalf("label mismatches = %v, want exactly 1 (the dated backup DIRECTORY must not be read as a file label)", got)
	}
	if !strings.Contains(c.LabelMismatches[0].Rel, "mislabeled") {
		t.Errorf("wrong mismatch: %s", c.LabelMismatches[0].Rel)
	}

	// Decoys rejected.
	for _, s := range c.Historical {
		if strings.Contains(s.Rel, "docker-compose") || strings.Contains(s.Rel, "notes.txt") {
			t.Errorf("decoy admitted into the corpus: %s", s.Rel)
		}
	}
	// The non-.yaml-suffixed and the subdirectory copies must both be found.
	var rels []string
	for _, s := range c.Historical {
		rels = append(rels, filepath.ToSlash(s.Rel))
	}
	joined := strings.Join(rels, " ")
	for _, want := range []string{"llama-swap.yaml.pre-matrix", "backup-2026-07-25/llama-swap.yaml"} {
		if !strings.Contains(joined, want) {
			t.Errorf("extension-tolerant/recursive discovery missed %s (found: %v)", want, rels)
		}
	}
}

func TestCorpusChronologyDedupesByContent(t *testing.T) {
	live := writeCorpus(t)
	c, err := DiscoverCorpus(live, DiscoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	chron := c.Chronology()
	seen := map[string]bool{}
	for _, s := range chron {
		if seen[s.Sha256] {
			t.Errorf("chronology repeated a content state: %s", s.Rel)
		}
		seen[s.Sha256] = true
	}
	for i := 1; i < len(chron); i++ {
		if chron[i].ModTime.Before(chron[i-1].ModTime) {
			t.Errorf("chronology not mtime-ascending at %d", i)
		}
	}
	// Five distinct contents exist across the corpus; the live file is
	// byte-identical to the orphan backup, so it must NOT add a sixth.
	if len(chron) != 5 {
		var got []string
		for _, s := range chron {
			got = append(got, s.Rel)
		}
		t.Errorf("chronology = %d states (%v), want 5", len(chron), got)
	}
}

func TestUnifiedDiff(t *testing.T) {
	a := []string{"one", "two", "three", "four"}
	b := []string{"one", "TWO", "three", "four"}
	out := UnifiedDiff("a", "b", a, b, 1)
	if !strings.Contains(out, "-two") || !strings.Contains(out, "+TWO") {
		t.Errorf("unified diff missing the change:\n%s", out)
	}
	if UnifiedDiff("a", "b", a, a, 1) != "" {
		t.Error("identical inputs must produce an empty diff")
	}
}

func hasCheck(rep *LintReport, check string) bool {
	for _, f := range rep.Findings {
		if f.Check == check {
			return true
		}
	}
	return false
}

func hasCheckSeverity(rep *LintReport, check string, sev Severity) bool {
	for _, f := range rep.Findings {
		if f.Check == check && f.Severity == sev {
			return true
		}
	}
	return false
}
