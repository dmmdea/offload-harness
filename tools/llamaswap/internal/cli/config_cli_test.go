// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/cliutil/testenv"
	"llamaswap-pp-cli/internal/lsconfig"
)

const testConfigBody = `healthCheckTimeout: 300
startPort: 9200

macros:
  server: "/opt/llama.cpp/llama-server --port ${PORT} --host 127.0.0.1"
  mdir:   "/models"

models:
  # ===== the workhorse =====
  "worker":
    cmd: "${server} -m ${mdir}/worker.gguf -ngl 99 -c 65536"   # ctx raised
    aliases: ["work"]
    ttl: 300

  "resident":
    cmd: "${server} -m ${mdir}/resident.gguf --embeddings --pooling mean"
    ttl: -1

  "stt":
    cmd: "/opt/whisper.cpp/whisper-server -m ${mdir}/ggml-large.bin --vad -nfa --host 127.0.0.1 --port ${PORT}"
    ttl: 300
    aliases: ["whisper"]
`

// runCLI executes the root command with args and returns combined output.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Setenv("LLAMASWAP_NO_LEARN", "true")
	cmd := RootCmd()
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	return out.String(), err
}

func writeTestConfig(t *testing.T) string {
	t.Helper()
	return writeTestConfigIn(t, t.TempDir(), testConfigBody)
}

// writeTestConfigIn materializes a config plus the binaries and weights it
// references, so lint's file-existence checks have real files to find. Without
// this, every fixture config reports binary.missing/file.missing and the
// interesting assertions drown in noise that says nothing about the code.
func writeTestConfigIn(t *testing.T, dir, body string) string {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	models := filepath.Join(dir, "models")
	for _, d := range []string{bin, models} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{
		filepath.Join(bin, "llama-server"),
		filepath.Join(bin, "whisper-server"),
		filepath.Join(models, "worker.gguf"),
		filepath.Join(models, "resident.gguf"),
		filepath.Join(models, "ggml-large.bin"),
	} {
		if err := os.WriteFile(f, []byte("stub"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	body = strings.NewReplacer(
		"/opt/llama.cpp/llama-server", filepath.ToSlash(filepath.Join(bin, "llama-server")),
		"/opt/whisper.cpp/whisper-server", filepath.ToSlash(filepath.Join(bin, "whisper-server")),
		`mdir:   "/models"`, `mdir:   "`+filepath.ToSlash(models)+`"`,
	).Replace(body)
	path := filepath.Join(dir, "llama-swap.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestConfigFamilyWires proves every command in the family resolves and
// renders help. A novel command that is written but never registered is the
// single most common way this work silently fails to ship.
func TestConfigFamilyWires(t *testing.T) {
	cases := [][]string{
		{"config", "--help"},
		{"config", "validate", "--help"},
		{"config", "lint", "--help"},
		{"config", "explain", "--help"},
		{"config", "diff", "--help"},
		{"config", "drift", "--help"},
		{"config", "backup", "--help"},
		{"config", "testinstance", "--help"},
		{"config", "apply", "--help"},
		{"seat", "--help"},
		{"seat", "log", "--help"},
		{"seat", "show", "--help"},
		{"seat", "try", "--help"},
		{"bind", "--help"},
		{"bind", "check", "--help"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args[:len(args)-1], "_"), func(t *testing.T) {
			out, err := runCLI(t, args...)
			if err != nil {
				t.Fatalf("%v: %v", args, err)
			}
			if !strings.Contains(out, "Usage:") {
				t.Fatalf("%v help missing Usage:\n%s", args, out)
			}
		})
	}
}

// TestConfigTestAliasResolves covers the `config test` spelling the operator
// ritual uses. The command is named testinstance only because a Go file named
// config_test.go would be a test file.
func TestConfigTestAliasResolves(t *testing.T) {
	out, err := runCLI(t, "config", "test", "--help")
	if err != nil {
		t.Fatalf("config test --help: %v", err)
	}
	if !strings.Contains(out, "throwaway") {
		t.Fatalf("alias resolved to the wrong command:\n%s", out)
	}
}

// TestDryRunShortCircuits is the verify-friendliness contract: every command
// must reach its own --dry-run guard. A command that puts validation in
// cobra's Args or MarkFlagRequired never gets there, and the printing-press
// verifier fails it.
func TestDryRunShortCircuits(t *testing.T) {
	cases := [][]string{
		{"config", "validate", "--dry-run"},
		{"config", "lint", "--dry-run"},
		{"config", "explain", "--dry-run"},
		{"config", "diff", "--dry-run"},
		{"config", "drift", "--dry-run"},
		{"config", "backup", "--dry-run"},
		{"config", "apply", "--dry-run"},
		{"seat", "log", "--dry-run"},
		{"seat", "show", "--dry-run"},
		{"seat", "try", "--dry-run"},
		{"bind", "check", "--dry-run"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			testenv.Isolate(t, cliutil.StateDir)
			t.Setenv(lsconfig.EnvVarConfigPath, writeTestConfig(t))
			if _, err := runCLI(t, args...); err != nil {
				t.Fatalf("%v under --dry-run returned %v; dry-run must never do real work or fail", args, err)
			}
		})
	}
}

func TestConfigValidateAndLintOnCleanConfig(t *testing.T) {
	path := writeTestConfig(t)
	out, err := runCLI(t, "config", "validate", path, "--json")
	if err != nil {
		t.Fatalf("validate: %v\n%s", err, out)
	}
	var res lsconfig.ValidationResult
	if err := json.Unmarshal([]byte(firstJSONLine(out)), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if !res.Valid || res.ModelCount != 3 {
		t.Fatalf("validate result = %+v", res)
	}

	out, err = runCLI(t, "config", "lint", path, "--json", "--no-listener-check")
	if err != nil {
		t.Fatalf("lint: %v\n%s", err, out)
	}
	var rep lsconfig.LintReport
	if err := json.Unmarshal([]byte(firstJSONLine(out)), &rep); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if rep.Errors != 0 {
		t.Fatalf("clean config produced %d error(s): %+v", rep.Errors, rep.Findings)
	}
	if len(rep.NonLlamaServerSeats) != 1 || rep.NonLlamaServerSeats[0] != "stt" {
		t.Fatalf("non-llama-server seats = %v, want [stt]", rep.NonLlamaServerSeats)
	}
	// The whisper seat must not carry a single error or warning.
	for _, f := range rep.Findings {
		if f.Model == "stt" && (f.Severity == lsconfig.SevError || f.Severity == lsconfig.SevWarn) {
			t.Fatalf("false positive on the whisper seat: %+v", f)
		}
	}
}

func TestConfigValidateExitsConfigInvalid(t *testing.T) {
	body := strings.Replace(testConfigBody, "startPort: 9200", "startport: 9200", 1)
	path := writeTestConfigIn(t, t.TempDir(), body)
	_, err := runCLI(t, "config", "validate", path, "--json")
	if err == nil {
		t.Fatal("a misspelled top-level key must fail validation")
	}
	if code := ExitCode(err); code != ExitConfigInvalid {
		t.Fatalf("exit code = %d, want %d", code, ExitConfigInvalid)
	}
}

func TestConfigExplainResolvesAlias(t *testing.T) {
	path := writeTestConfig(t)
	out, err := runCLI(t, "config", "explain", "whisper", "--config-file", path, "--json")
	if err != nil {
		t.Fatalf("explain: %v\n%s", err, out)
	}
	var res explainResult
	if err := json.Unmarshal([]byte(firstJSONLine(out)), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if res.Model != "stt" || res.MatchedBy != "alias:whisper" {
		t.Fatalf("alias resolution = %+v", res)
	}
	if res.SeatKind != lsconfig.SeatNonLlamaServer {
		t.Fatalf("seat kind = %q", res.SeatKind)
	}
	if !strings.Contains(res.CmdExpanded, "/models/ggml-large.bin") {
		t.Fatalf("macro not expanded: %s", res.CmdExpanded)
	}
	if !strings.Contains(res.CmdExpanded, "${PORT}") {
		t.Fatalf("${PORT} must stay symbolic: %s", res.CmdExpanded)
	}
	if len(res.SkippedNotes) == 0 {
		t.Error("a non-llama-server seat must carry an explicit skipped-checks note")
	}
}

func TestConfigExplainUnknownModelExitCode(t *testing.T) {
	path := writeTestConfig(t)
	_, err := runCLI(t, "config", "explain", "nope", "--config-file", path)
	if err == nil {
		t.Fatal("unknown model must fail")
	}
	if code := ExitCode(err); code != ExitModelNotFound {
		t.Fatalf("exit code = %d, want %d", code, ExitModelNotFound)
	}
}

func TestConfigApplyWriteIsRefused(t *testing.T) {
	path := writeTestConfig(t)
	out, err := runCLI(t, "config", "apply", path, "--write")
	if err == nil {
		t.Fatal("--write must be refused")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("refusal should explain itself: %v", err)
	}
	if !strings.Contains(err.Error()+out, "READ-ONLY") {
		t.Fatalf("refusal should state the trust contract: %v\n%s", err, out)
	}
}

// TestConfigApplyNeverMutates is the guard that matters most in this family:
// after a full apply plan against a real file, the file's bytes must be
// unchanged.
func TestConfigApplyNeverMutates(t *testing.T) {
	dir := t.TempDir()
	live := writeTestConfigIn(t, dir, testConfigBody)
	candidatePath := writeTestConfigIn(t, filepath.Join(dir, "candidate"),
		strings.Replace(testConfigBody, "-c 65536", "-c 131072", 1))
	before, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's binaries and weights do not exist on the test machine, so
	// lint raises blockers and apply exits ExitConfigInvalid. That is the
	// correct verdict; what this test guards is that the plan still ran and
	// the live file was still not touched.
	out, err := runCLI(t, "config", "apply", candidatePath, "--live", live, "--json")
	if err != nil && ExitCode(err) != ExitConfigInvalid {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	after, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("config apply MUTATED the live file; this command must never write")
	}
	var plan applyPlan
	if err := json.Unmarshal([]byte(firstJSONLine(out)), &plan); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(plan.RestartCommand) == 0 {
		t.Error("apply must print a restart command")
	}
	if len(plan.VerifyPlan) == 0 {
		t.Error("apply must print a post-restart verify plan")
	}
	if !strings.Contains(plan.UnifiedDiff, "-c 131072") {
		t.Errorf("unified diff missing the change:\n%s", plan.UnifiedDiff)
	}
	foundCtx := false
	for _, md := range plan.Semantic.Models {
		for _, d := range md.FlagDeltas {
			if d.Flag == "-c" && d.To == "131072" {
				foundCtx = true
			}
		}
	}
	if !foundCtx {
		t.Error("semantic diff missed the -c change")
	}
}

func TestBackupWritesContentAddressedFileAndIndex(t *testing.T) {
	src := writeTestConfig(t)
	dir := t.TempDir()
	res, err := runBackup(src, dir, "pre-ctx-raise", true)
	if err != nil {
		t.Fatalf("runBackup: %v", err)
	}
	if !res.Written {
		t.Fatalf("backup not written: %+v", res)
	}
	base := filepath.Base(res.File)
	if !strings.HasPrefix(base, res.SourceSha[:10]) || !strings.HasSuffix(base, "-pre-ctx-raise.yaml") {
		t.Fatalf("backup name %q is not <sha10>-<label>.yaml", base)
	}
	copied, err := os.ReadFile(res.File)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(copied, original) {
		t.Fatal("backup is not a byte-for-byte copy")
	}

	idxRaw, err := os.ReadFile(res.IndexFile)
	if err != nil {
		t.Fatalf("index not written: %v", err)
	}
	var entries []backupIndexEntry
	if err := json.Unmarshal(idxRaw, &entries); err != nil {
		t.Fatalf("index is not valid JSON: %v", err)
	}
	if len(entries) != 1 || entries[0].Sha256 != res.SourceSha || entries[0].Label != "pre-ctx-raise" {
		t.Fatalf("index entry = %+v", entries)
	}
	if entries[0].SourceMtime == "" {
		t.Error("index must record the SOURCE mtime: the filename is a label, the mtime and hash are the truth")
	}

	// Second run: same content, must dedup rather than write a second copy.
	res2, err := runBackup(src, dir, "another-label", true)
	if err != nil {
		t.Fatalf("second runBackup: %v", err)
	}
	if res2.Written {
		t.Error("identical content must not be archived twice")
	}
	if !res2.Dedup {
		t.Error("dedup must be reported")
	}
}

func TestBackupDetectsOrphanBackup(t *testing.T) {
	dir := t.TempDir()
	live := writeTestConfigIn(t, dir, testConfigBody)
	body, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	// A backup byte-identical to live: the change it was named for never
	// landed.
	orphan := filepath.Join(dir, "backup-2026-08-12-pre-something.yaml")
	if err := os.WriteFile(orphan, body, 0o644); err != nil {
		t.Fatal(err)
	}
	res, rerr := runBackup(live, dir, "x", false)
	if rerr != nil {
		t.Fatalf("runBackup: %v", rerr)
	}
	if len(res.OrphanBackups) != 1 || !strings.Contains(res.OrphanBackups[0], "pre-something") {
		t.Fatalf("orphan backup not detected: %+v", res.OrphanBackups)
	}
	joined := strings.Join(res.Notes, " ")
	if !strings.Contains(joined, "ORPHAN BACKUP") {
		t.Fatalf("orphan finding must be explained: %v", res.Notes)
	}
}

func TestBackupDryRunWritesNothing(t *testing.T) {
	src := writeTestConfig(t)
	dir := t.TempDir()
	res, err := runBackup(src, dir, "x", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Written {
		t.Fatal("dry-run must not write")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("dry-run created files: %v", entries)
	}
}

func TestBackupRefusesToClobberUnparsableIndex(t *testing.T) {
	dir := t.TempDir()
	idx := filepath.Join(dir, BackupIndexName)
	if err := os.WriteFile(idx, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := appendBackupIndex(idx, backupIndexEntry{Sha256: "abc", File: "abc-x.yaml"})
	if err == nil {
		t.Fatal("an unparsable index must not be silently overwritten — it may be hand-edited history")
	}
	raw, _ := os.ReadFile(idx)
	if string(raw) != "{ not json" {
		t.Fatal("the unparsable index was destroyed")
	}
}

func TestScratchPortValidation(t *testing.T) {
	f, err := lsconfig.ParseBytes([]byte(testConfigBody), "t.yaml", lsconfig.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateScratchPort(9205, f); err == nil {
		t.Error("a port inside the startPort span must be refused")
	} else if ExitCode(err) != ExitPortConflict {
		t.Errorf("exit code = %d, want %d", ExitCode(err), ExitPortConflict)
	}
	if err := validateScratchPort(443, f); err == nil {
		t.Error("a reserved well-known port must be refused")
	} else if ExitCode(err) != ExitPortConflict {
		t.Errorf("exit code = %d, want %d", ExitCode(err), ExitPortConflict)
	}
	if err := validateScratchPort(DefaultTestInstancePort, f); err != nil {
		t.Errorf("the default scratch port should be acceptable: %v", err)
	}
}

func TestLoopbackBaseURLRewrite(t *testing.T) {
	cases := map[string]string{
		"http://localhost:11436":   "http://127.0.0.1:11436",
		"http://LOCALHOST:11436/x": "http://127.0.0.1:11436/x",
		"http://127.0.0.1:11436":   "http://127.0.0.1:11436",
		"http://node-a:11436":        "http://node-a:11436",
	}
	for in, want := range cases {
		if got := loopbackBaseURL(in); got != want {
			t.Errorf("loopbackBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestApplyFlagEdits(t *testing.T) {
	base := "${server} -m /models/a.gguf --parallel 4 --n-gpu-layers 0"
	got, err := applyFlagEdits(base, []string{"--n-gpu-layers 99"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "${server} -m /models/a.gguf --parallel 4 --n-gpu-layers 99" {
		t.Fatalf("in-place replacement failed: %q", got)
	}

	got, err = applyFlagEdits(base, []string{"-c 65536"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "-c 65536") {
		t.Fatalf("a new flag should append: %q", got)
	}

	got, err = applyFlagEdits(base, nil, []string{"--parallel"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "--parallel") || strings.Contains(got, " 4 ") {
		t.Fatalf("unset must drop the flag AND its value: %q", got)
	}

	if _, err := applyFlagEdits(base, []string{"ctx-size 4096"}, nil); err == nil {
		t.Error("a --set without a leading dash must be a usage error")
	}
}

func TestSeatTryNeverWrites(t *testing.T) {
	path := writeTestConfig(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, "seat", "try", "worker", "--config-file", path, "--set", "-c 131072", "--json")
	if err != nil {
		t.Fatalf("seat try: %v\n%s", err, out)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("seat try MUTATED the config; it is plan-only")
	}
	var plan seatTryPlan
	if err := json.Unmarshal([]byte(firstJSONLine(out)), &plan); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(plan.Deltas) != 1 || plan.Deltas[0].Flag != "-c" || plan.Deltas[0].To != "131072" {
		t.Fatalf("flag delta = %+v", plan.Deltas)
	}
	if len(plan.AcceptanceProbe) == 0 {
		t.Error("a flag experiment without an acceptance probe cannot be kept or reverted on evidence")
	}
	if len(plan.RestartCommand) == 0 {
		t.Error("plan must include the restart command")
	}
}

func TestSeatTryKeepResidentWarning(t *testing.T) {
	path := writeTestConfig(t)
	out, err := runCLI(t, "seat", "try", "resident", "--config-file", path, "--set", "--pooling cls", "--json")
	if err != nil {
		t.Fatalf("seat try: %v\n%s", err, out)
	}
	var plan seatTryPlan
	if err := json.Unmarshal([]byte(firstJSONLine(out)), &plan); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	joined := strings.Join(plan.Warnings, " ")
	if !strings.Contains(joined, "keep-resident") {
		t.Errorf("a keep-resident seat must warn that a restart drops it: %v", plan.Warnings)
	}
	if !strings.Contains(strings.Join(plan.AcceptanceProbe, " "), "embed") {
		t.Errorf("an embedding seat should get an embedding probe: %v", plan.AcceptanceProbe)
	}
}

func TestSeatTryNonLlamaServerWarning(t *testing.T) {
	path := writeTestConfig(t)
	out, err := runCLI(t, "seat", "try", "stt", "--config-file", path, "--set", "-t 8", "--json")
	if err != nil {
		t.Fatalf("seat try: %v\n%s", err, out)
	}
	var plan seatTryPlan
	if err := json.Unmarshal([]byte(firstJSONLine(out)), &plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan.Warnings, " "), "not llama-server") {
		t.Errorf("a non-llama-server seat must say so rather than imply llama-server semantics: %v", plan.Warnings)
	}
}

func TestRestartCommandReadsRegistrationScript(t *testing.T) {
	dir := t.TempDir()
	cfg := writeTestConfigIn(t, dir, testConfigBody)
	script := `Register-ScheduledTask -TaskName "my-swap" -Action $a
Get-Process llama-swap | Stop-Process -Force
Start-ScheduledTask -TaskName my-swap
`
	if err := os.WriteFile(filepath.Join(dir, "register-task.ps1"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmds, source := restartCommand(cfg)
	joined := strings.Join(cmds, "\n")
	if !strings.Contains(joined, "my-swap") {
		t.Errorf("task name not read from the registration script:\n%s", joined)
	}
	if !strings.Contains(source, "read from") {
		t.Errorf("source must say the command was READ, not assumed: %s", source)
	}
	if !strings.Contains(joined, "Stop-Process") {
		t.Errorf("the script's own restart form should be carried through:\n%s", joined)
	}

	// With no script, the fallback must SAY it is assumed.
	bare := writeTestConfigIn(t, t.TempDir(), testConfigBody)
	_, source2 := restartCommand(bare)
	if !strings.Contains(source2, "ASSUMED") {
		t.Errorf("a guessed restart command must be labeled as such: %s", source2)
	}
}

func TestPostRestartVerifyPlanNamesKeepSet(t *testing.T) {
	f, err := lsconfig.ParseBytes([]byte(testConfigBody), "t.yaml", lsconfig.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(postRestartVerifyPlan(f), "\n")
	if !strings.Contains(plan, "--expect-models 3") {
		t.Errorf("verify plan should assert the roster count:\n%s", plan)
	}
	if !strings.Contains(plan, "--keepset resident") {
		t.Errorf("verify plan should name the keep-resident seats read from the CONFIG:\n%s", plan)
	}
}

func TestBuildDriftReport(t *testing.T) {
	f, err := lsconfig.ParseBytes([]byte(testConfigBody), "t.yaml", lsconfig.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	running := []runningSeat{
		// Port substituted by the proxy: must NOT read as drift.
		{Model: "worker", State: "ready", Cmd: "/opt/llama.cpp/llama-server --port 9201 --host 127.0.0.1 -m /models/worker.gguf -ngl 99 -c 65536"},
		// A real divergence.
		{Model: "resident", State: "ready", Cmd: "/opt/llama.cpp/llama-server --port 9202 --host 127.0.0.1 -m /models/resident.gguf --embeddings"},
		// A seat the file does not define at all.
		{Model: "ghost", State: "ready", Cmd: "/opt/llama.cpp/llama-server --port 9203"},
	}
	rep := buildDriftReport(f, running, "http://127.0.0.1:11436")
	byModel := map[string]seatDrift{}
	for _, s := range rep.Seats {
		byModel[s.Model] = s
	}
	if byModel["worker"].Status != "match" {
		t.Errorf("port substitution reported as drift: %+v", byModel["worker"])
	}
	if byModel["resident"].Status != "drift" {
		t.Errorf("a dropped --pooling flag must be drift: %+v", byModel["resident"])
	}
	if byModel["ghost"].Status != "not-in-config" {
		t.Errorf("a running seat absent from the file must be flagged: %+v", byModel["ghost"])
	}
	if rep.Drifted != 1 || rep.Compared != 2 {
		t.Errorf("drifted=%d compared=%d, want 1/2", rep.Drifted, rep.Compared)
	}
	// The unloaded seat must be reported as NOT EVALUATED, never as clean.
	if len(rep.NotEvaluated) != 1 || rep.NotEvaluated[0] != "stt" {
		t.Errorf("not-evaluated = %v, want [stt]", rep.NotEvaluated)
	}
}

func TestResolveBinding(t *testing.T) {
	resolve := map[string]string{"gemma-4-e4b": "gemma-4-e4b", "offload-e4b": "gemma-4-e4b"}
	if b := resolveBinding("model", 0, "gemma-4-e4b", resolve); !b.OK || b.Via != "id" {
		t.Errorf("id binding = %+v", b)
	}
	if b := resolveBinding("model", 0, "offload-e4b", resolve); !b.OK || b.Via != "alias" || b.Resolved != "gemma-4-e4b" {
		t.Errorf("alias binding = %+v", b)
	}
	if b := resolveBinding("stt_model_hq", 0, "", resolve); b.OK || !strings.Contains(b.Note, "empty") {
		t.Errorf("an empty value must be reported as dangling with a reason: %+v", b)
	}
	if b := resolveBinding("model", 0, "retired-seat", resolve); b.OK {
		t.Errorf("a retired seat must not resolve: %+v", b)
	}
}

func TestBackupFileName(t *testing.T) {
	name := backupFileName("deadbeefcafebabe0000", "pre ctx/raise")
	if name != "deadbeefca-pre-ctx-raise.yaml" {
		t.Errorf("backupFileName = %q", name)
	}
	if got := backupFileName("deadbeefcafebabe0000", ""); got != "deadbeefca-backup.yaml" {
		t.Errorf("empty label = %q", got)
	}
}

// firstJSONLine extracts the first complete JSON object from command output.
// Output may be a single compact line, a pretty-printed multi-line object, or
// either of those followed by cobra's "Error: ..." line when the command
// exited non-zero after printing a valid report — which several commands here
// do on purpose, since a finding is not a failure to report.
func firstJSONLine(out string) string {
	start := strings.Index(out, "{")
	if start < 0 {
		return out
	}
	depth, inString, escaped := 0, false, false
	for i := start; i < len(out); i++ {
		c := out[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// literal character inside a string
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				candidate := out[start : i+1]
				var probe map[string]any
				if json.Unmarshal([]byte(candidate), &probe) == nil {
					return candidate
				}
				return out
			}
		}
	}
	return out
}

var _ = cobra.Command{}
