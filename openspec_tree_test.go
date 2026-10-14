package main

// ENH-1b: the openspec/ tree, its forked schema, and the two scripts around the pinned OpenSpec CLI.
//
// Two layers. The structural tests need no OpenSpec binary and run everywhere: they read openspec/config.yaml
// and openspec/schemas/ and pin the shape the workflow depends on (a project-local schema that exists, an `adr`
// artifact that nothing waits for, rules that name real artifacts, the standing invariants in the context). The
// script tests drive scripts/openspec-validate.sh and scripts/openspec-install.sh against stand-in `openspec` and
// `npm` programs, so the switches, the arguments and the hash refusal are proved without the network or the CLI.
// TestOpenSpecTreeValidates is the one test that runs the real CLI, and only when OPENSPEC_BIN names it.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/winexec"
	"gopkg.in/yaml.v3"
)

type openspecConfig struct {
	Schema  string              `yaml:"schema"`
	Context string              `yaml:"context"`
	Rules   map[string][]string `yaml:"rules"`
}

type openspecArtifact struct {
	ID          string   `yaml:"id"`
	Generates   string   `yaml:"generates"`
	Description string   `yaml:"description"`
	Template    string   `yaml:"template"`
	Requires    []string `yaml:"requires"`
}

type openspecSchema struct {
	Name      string             `yaml:"name"`
	Artifacts []openspecArtifact `yaml:"artifacts"`
	Apply     struct {
		Requires []string `yaml:"requires"`
		Tracks   string   `yaml:"tracks"`
	} `yaml:"apply"`
}

func loadOpenSpecConfig(t *testing.T) openspecConfig {
	t.Helper()
	raw, err := os.ReadFile("openspec/config.yaml")
	if err != nil {
		t.Fatalf("openspec/config.yaml: %v", err)
	}
	var c openspecConfig
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatalf("openspec/config.yaml is not valid YAML: %v", err)
	}
	return c
}

func loadOpenSpecSchema(t *testing.T, name string) openspecSchema {
	t.Helper()
	p := filepath.Join("openspec", "schemas", name, "schema.yaml")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("openspec/config.yaml names schema %q, which must exist under openspec/schemas/: %v", name, err)
	}
	var s openspecSchema
	if err := yaml.Unmarshal(raw, &s); err != nil {
		t.Fatalf("%s is not valid YAML: %v", p, err)
	}
	return s
}

// The config's schema is a project-local one: a built-in name would leave the optional `adr` artifact and the
// invariants' rules without a home, and a typo would make every OpenSpec command fall back silently.
func TestOpenSpecConfigNamesASchemaThatExists(t *testing.T) {
	c := loadOpenSpecConfig(t)
	if strings.TrimSpace(c.Schema) == "" {
		t.Fatal("openspec/config.yaml has no schema:")
	}
	s := loadOpenSpecSchema(t, c.Schema)
	if s.Name != c.Schema {
		t.Errorf("openspec/schemas/%s/schema.yaml is named %q; the directory, the schema name and config.yaml must agree", c.Schema, s.Name)
	}
	if len(s.Artifacts) == 0 {
		t.Fatal("the schema declares no artifacts")
	}
	ids := map[string]bool{}
	for _, a := range s.Artifacts {
		if ids[a.ID] {
			t.Errorf("artifact %q is declared twice", a.ID)
		}
		ids[a.ID] = true
		if _, err := os.Stat(filepath.Join("openspec", "schemas", c.Schema, "templates", a.Template)); err != nil {
			t.Errorf("artifact %q names template %q, which is not under openspec/schemas/%s/templates/", a.ID, a.Template, c.Schema)
		}
	}
	for _, a := range s.Artifacts {
		for _, r := range a.Requires {
			if !ids[r] {
				t.Errorf("artifact %q requires %q, which the schema does not declare", a.ID, r)
			}
		}
	}
}

// The forked workflow keeps proposal -> specs -> design -> tasks and adds an ADR draft that nothing waits for.
// OpenSpec 1.14.1 has no `optional` key in its artifact schema (an unknown key is dropped without a word), so
// "optional" is a property of the graph: no other artifact requires `adr` and apply does not.
func TestOpenSpecAdrArtifactIsOptional(t *testing.T) {
	c := loadOpenSpecConfig(t)
	s := loadOpenSpecSchema(t, c.Schema)
	byID := map[string]openspecArtifact{}
	for _, a := range s.Artifacts {
		byID[a.ID] = a
	}
	spine := map[string][]string{
		"proposal": nil,
		"specs":    {"proposal"},
		"design":   {"proposal"},
		"tasks":    {"design", "specs"},
	}
	for id, want := range spine {
		a, ok := byID[id]
		if !ok {
			t.Errorf("the spine artifact %q is missing: the fork must keep proposal -> specs -> design -> tasks", id)
			continue
		}
		got := append([]string(nil), a.Requires...)
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("artifact %q requires %v, want %v: the adr artifact must not change the spine", id, a.Requires, want)
		}
	}
	adr, ok := byID["adr"]
	if !ok {
		t.Fatal("the schema has no `adr` artifact")
	}
	if adr.Generates != "adr.md" {
		t.Errorf("the adr artifact generates %q, want adr.md", adr.Generates)
	}
	if len(adr.Requires) == 0 {
		t.Error("the adr artifact requires nothing: an ADR is drafted from a decision already made, so it follows design")
	}
	if !strings.Contains(strings.ToLower(adr.Description), "optional") {
		t.Errorf("the adr artifact's description must say it is optional, got %q", adr.Description)
	}
	for _, a := range s.Artifacts {
		for _, r := range a.Requires {
			if r == "adr" {
				t.Errorf("artifact %q requires adr: an optional artifact must block nothing", a.ID)
			}
		}
	}
	for _, r := range s.Apply.Requires {
		if r == "adr" {
			t.Error("apply.requires lists adr: the ADR draft must never block tasks or apply")
		}
	}
	if len(s.Apply.Requires) != 1 || s.Apply.Requires[0] != "tasks" || s.Apply.Tracks != "tasks.md" {
		t.Errorf("apply must require tasks and track tasks.md, got requires=%v tracks=%q", s.Apply.Requires, s.Apply.Tracks)
	}
}

// Rules are keyed by artifact id; OpenSpec only warns on an unknown key and drops it, so a typo would silently lose a rule.
func TestOpenSpecConfigRulesNameRealArtifacts(t *testing.T) {
	c := loadOpenSpecConfig(t)
	s := loadOpenSpecSchema(t, c.Schema)
	ids := map[string]bool{}
	for _, a := range s.Artifacts {
		ids[a.ID] = true
	}
	for _, id := range []string{"proposal", "design", "tasks"} {
		if len(c.Rules[id]) == 0 {
			t.Errorf("openspec/config.yaml has no rules for %q", id)
		}
	}
	for id, rules := range c.Rules {
		if !ids[id] {
			t.Errorf("rules are keyed %q, which is not an artifact of schema %q", id, c.Schema)
		}
		for _, r := range rules {
			if strings.TrimSpace(r) == "" {
				t.Errorf("rules.%s holds an empty rule", id)
			}
		}
	}
}

// The context is injected into every artifact's instructions, so it is where the standing invariants live. Each
// phrase below is one invariant; the test pins that it stays in the context (a hollowed-out context would still
// validate) and that the context stays inside OpenSpec's own 50 KiB limit.
func TestOpenSpecConfigCarriesTheStandingInvariants(t *testing.T) {
	c := loadOpenSpecConfig(t)
	if len(c.Context) > 50*1024 {
		t.Errorf("context is %d bytes; OpenSpec refuses a context over 50 KiB", len(c.Context))
	}
	ctx := strings.ToLower(c.Context)
	for _, want := range []string{
		"inv-5", "wall-time",
		"inv-1", "cpu",
		"inv-2", "300 s",
		"daemon", "scheduler",
		"defer",
		"leak gate", "node letter",
		"changes/unreleased", "changelog.md",
		"adr",
		"red-first",
		"docs/systems/security.md",
	} {
		if !strings.Contains(ctx, want) {
			t.Errorf("the context in openspec/config.yaml no longer mentions %q", want)
		}
	}
}

// The scripts run under Git's bash (not the WSL launcher), with the tools beside it on PATH.
func openspecScriptEnv(t *testing.T, bash string, extra ...string) []string {
	t.Helper()
	drop := map[string]bool{"OPENSPEC_TELEMETRY": true, "DO_NOT_TRACK": true, "OPENSPEC_NO_UPDATE_CHECK": true}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if drop[strings.ToUpper(k)] {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, extra...)
	return withBashOnPath(env, bash)
}

func writeOpenSpecStandIn(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/usr/bin/env bash\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(p)
}

// A stand-in `openspec` records its arguments and the three switches; the wrapper must set the switches whatever the
// caller exported, run the strict non-interactive validation over the whole tree, validate every local schema, and
// carry the validator's exit code out.
func TestOpenSpecValidateScriptSetsTheSwitchesAndRunsStrict(t *testing.T) {
	bash := guardBash(t)
	dir := t.TempDir()
	out := filepath.ToSlash(filepath.Join(dir, "calls.txt"))
	fake := writeOpenSpecStandIn(t, dir, "openspec", `printf 'args=%s\n' "$*" >> "$OUT"
printf 'switches=%s,%s,%s\n' "${OPENSPEC_TELEMETRY-unset}" "${DO_NOT_TRACK-unset}" "${OPENSPEC_NO_UPDATE_CHECK-unset}" >> "$OUT"
exit "${FAKE_EXIT:-0}"
`)
	run := func(exit string) (string, error) {
		_ = os.Remove(filepath.FromSlash(out))
		cmd := winexec.Command(bash, "scripts/openspec-validate.sh")
		cmd.Env = openspecScriptEnv(t, bash, "OPENSPEC_BIN="+fake, "OUT="+out, "FAKE_EXIT="+exit,
			"OPENSPEC_TELEMETRY=1", "DO_NOT_TRACK=0", "OPENSPEC_NO_UPDATE_CHECK=0")
		combined, err := cmd.CombinedOutput()
		calls, _ := os.ReadFile(filepath.FromSlash(out))
		if err != nil {
			t.Logf("script output:\n%s", combined)
		}
		return string(calls), err
	}

	calls, err := run("0")
	if err != nil {
		t.Fatalf("the script failed although the validator passed: %v", err)
	}
	if !strings.Contains(calls, "args=validate --all --strict --no-interactive\n") {
		t.Errorf("the script must run `validate --all --strict --no-interactive`, calls were:\n%s", calls)
	}
	if !strings.Contains(calls, "args=schema validate harness-change\n") {
		t.Errorf("the script must validate the local schema (validate --all does not read schemas), calls were:\n%s", calls)
	}
	if strings.Count(calls, "switches=0,1,1\n") != strings.Count(calls, "args=") || !strings.Contains(calls, "switches=0,1,1\n") {
		t.Errorf("every call must see OPENSPEC_TELEMETRY=0, DO_NOT_TRACK=1, OPENSPEC_NO_UPDATE_CHECK=1 whatever the caller exported, calls were:\n%s", calls)
	}

	if _, err := run("3"); err == nil {
		t.Error("the script exited 0 although the validator failed: a red validation must fail the gate")
	}
}

var (
	openspecAdmittedRowVersion = regexp.MustCompile("\\| (\\d+\\.\\d+\\.\\d+), tarball sha256 `([0-9a-f]{64})`")
	openspecScriptVersion      = regexp.MustCompile(`(?m)^readonly OPENSPEC_VERSION="([^"]+)"$`)
	openspecScriptSHA          = regexp.MustCompile(`(?m)^readonly OPENSPEC_SHA256="([0-9a-f]{64})"$`)
)

// The version and the tarball hash are admitted in the public table of docs/systems/security.md; the install script
// must pin exactly those, as constants a caller cannot override.
func TestOpenSpecInstallScriptPinsWhatSecurityMdAdmitted(t *testing.T) {
	doc, err := os.ReadFile("docs/systems/security.md")
	if err != nil {
		t.Fatal(err)
	}
	var row string
	for _, l := range strings.Split(string(doc), "\n") {
		if strings.HasPrefix(l, "| OpenSpec CLI") {
			row = l
		}
	}
	if row == "" {
		t.Fatal("docs/systems/security.md has no OpenSpec CLI row in the admitted software table")
	}
	m := openspecAdmittedRowVersion.FindStringSubmatch(row)
	if m == nil {
		t.Fatalf("the OpenSpec row names no `<version>, tarball sha256 `<hex>`` pin: %s", row)
	}
	script, err := os.ReadFile("scripts/openspec-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	if v := openspecScriptVersion.FindStringSubmatch(text); v == nil || v[1] != m[1] {
		t.Errorf("scripts/openspec-install.sh must declare `readonly OPENSPEC_VERSION=%q` (the admitted version)", m[1])
	}
	if h := openspecScriptSHA.FindStringSubmatch(text); h == nil || h[1] != m[2] {
		t.Errorf("scripts/openspec-install.sh must declare `readonly OPENSPEC_SHA256=%q` (the admitted tarball hash)", m[2])
	}
	for _, want := range []string{
		"npm pack",
		"npm install --prefix",
		"--ignore-scripts",
		"OPENSPEC_TELEMETRY=0",
		"DO_NOT_TRACK=1",
		"OPENSPEC_NO_UPDATE_CHECK=1",
		"XDG_CONFIG_HOME",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("scripts/openspec-install.sh no longer contains %q", want)
		}
	}
	// The two npm calls that matter, spelled out: pack by the scoped, pinned name without running scripts; install from the
	// verified file (never from the registry by name) without running scripts.
	for _, re := range []string{
		`(?m)^\( cd "\$work" && npm pack "\$\{OPENSPEC_PACKAGE\}@\$\{OPENSPEC_VERSION\}" --ignore-scripts`,
		`(?m)^npm install --prefix "\$abs" "\$tarball" --ignore-scripts`,
	} {
		if !regexp.MustCompile(re).MatchString(text) {
			t.Errorf("scripts/openspec-install.sh has no line matching %s", re)
		}
	}
	// Install from the verified file, never from the registry by name, and never globally.
	for _, bad := range []string{"npm install -g", "npm i -g", "--global", "curl ", "wget ", "npx "} {
		if strings.Contains(text, bad) {
			t.Errorf("scripts/openspec-install.sh contains %q", bad)
		}
	}
	// No local path: the prefix is an argument or OPENSPEC_PREFIX.
	if p := regexp.MustCompile(`[A-Za-z]:[\\/]|/home/|/Users/|/mnt/[a-z]/`).FindString(text); p != "" {
		t.Errorf("scripts/openspec-install.sh hard-codes a local path (%q): the prefix must come from the caller", p)
	}
}

// A tarball whose hash is not the pinned one is refused before anything is installed, and the scratch directory
// the script made is gone. The stand-in npm "packs" a file of the wrong content and records any install call.
func TestOpenSpecInstallScriptRefusesAHashMismatch(t *testing.T) {
	bash := guardBash(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	mark := filepath.ToSlash(filepath.Join(dir, "install-called.txt"))
	writeOpenSpecStandIn(t, bin, "npm", `case "$1" in
  pack)
    printf 'not the admitted tarball' > fission-ai-openspec-1.14.1.tgz
    echo fission-ai-openspec-1.14.1.tgz ;;
  *)
    echo "$*" >> "$MARK" ;;
esac
`)
	prefix := filepath.ToSlash(filepath.Join(dir, "prefix"))
	cmd := winexec.Command(bash, "scripts/openspec-install.sh", prefix)
	sep := string(os.PathListSeparator)
	cmd.Env = openspecScriptEnv(t, bash, "MARK="+mark, "PATH="+bin+sep+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the script accepted a tarball with the wrong hash:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(string(out)), "sha256") {
		t.Errorf("the refusal must say it is a sha256 mismatch, got:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.FromSlash(mark)); statErr == nil {
		b, _ := os.ReadFile(filepath.FromSlash(mark))
		t.Errorf("npm install ran after a hash mismatch: %s", b)
	}
	left, _ := filepath.Glob(filepath.Join(filepath.FromSlash(prefix), ".openspec-pack.*"))
	if len(left) != 0 {
		t.Errorf("the scratch download directory was left behind: %v", left)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.FromSlash(prefix), "node_modules")); statErr == nil {
		t.Error("a node_modules directory exists after a refused install")
	}
}

// Without a prefix the script says so and exits non-zero (no default install location exists to fall back on).
func TestOpenSpecInstallScriptNeedsAPrefix(t *testing.T) {
	bash := guardBash(t)
	cmd := winexec.Command(bash, "scripts/openspec-install.sh")
	cmd.Env = openspecScriptEnv(t, bash, "OPENSPEC_PREFIX=")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the script ran with no prefix:\n%s", out)
	}
	if !strings.Contains(string(out), "prefix") {
		t.Errorf("the usage message must name the prefix, got:\n%s", out)
	}
}

// TestOpenSpecTreeValidates runs the real CLI over the tree, through the wrapper the gate list uses. The CLI is a
// separately installed, hash-pinned program (docs/systems/openspec.md), so the test runs only where OPENSPEC_BIN
// names it; everywhere else it skips and says why.
func TestOpenSpecTreeValidates(t *testing.T) {
	bin := strings.TrimSpace(os.Getenv("OPENSPEC_BIN"))
	if bin == "" {
		t.Skip("OPENSPEC_BIN is not set: the pinned OpenSpec CLI is not installed here (scripts/openspec-install.sh installs it); the structural OpenSpec tests above still ran")
	}
	bash := guardBash(t)
	cmd := winexec.Command(bash, "scripts/openspec-validate.sh")
	cmd.Env = openspecScriptEnv(t, bash, "OPENSPEC_BIN="+filepath.ToSlash(bin))
	out, err := cmd.CombinedOutput()
	t.Logf("openspec validate output:\n%s", out)
	if err != nil {
		t.Fatalf("scripts/openspec-validate.sh failed: %v", err)
	}
}
