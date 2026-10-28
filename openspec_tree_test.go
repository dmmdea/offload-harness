package main

// The openspec/ tree, its forked schema, and the two scripts around the pinned OpenSpec CLI.
//
// Two layers. The structural tests need no OpenSpec binary and run everywhere: they read openspec/config.yaml
// and openspec/schemas/ and pin the shape the workflow depends on (a project-local schema that exists, an `adr`
// artifact that nothing waits for, rules that name real artifacts, the standing invariants in the context). The
// script tests drive scripts/openspec-validate.sh and scripts/openspec-install.sh against stand-in `openspec` and
// `npm` programs, so the switches, the arguments, the hash refusal and the install's success path (what runs, where
// npm's cache and temp files go, what is left behind) are proved without the network or the CLI. Two tests run the
// real thing and only when asked: TestOpenSpecTreeValidates when OPENSPEC_BIN names the CLI, and
// TestOpenSpecInstallScriptInstallsTheRealPackage when OPENSPEC_INSTALL_TEST=1 (it uses the network).

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
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
	if strings.Join(adr.Requires, ",") != "design" {
		t.Errorf("the adr artifact requires %v, want exactly [design]: an ADR is drafted from a decision already made, so it follows design", adr.Requires)
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
		"defer", "cloud fallback", "adr 0001",
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

// openspecTierIDAsMachineName matches the ways a sentence can say a machine is named by its tier id: "by tier id", the
// paraphrases "by/using/through/with/via its/their/the tier id(s)", "as tier ids", and the pairings "tier id or node
// letter" either way round, each with "tier id" or "tier-id".
var openspecTierIDAsMachineName = regexp.MustCompile(`(?i)((by|using|through|with|via) (its |their |the )?tier[- ]ids?|as tier[- ]ids?|tier[- ]ids? or node letters?|node letters? or tier[- ]ids?)`)

// The pattern must catch the forms a rewrite of the sentence would take, and must leave alone the sentence the tree
// carries today, where a tier id appears only inside "the <tier> reference box".
func TestOpenSpecTierIDPatternCatchesParaphrases(t *testing.T) {
	for _, bad := range []string{
		"name a machine by tier id",
		"Machines are named by their tier id",
		"Machines are named by its tier ids",
		"a machine is identified by the tier id",
		"name it as tier ids",
		"a tier id or node letter",
		"a node letter or tier ids",
		"name a machine by tier-id",
		"machines are named using tier ids",
		"machines are named through their tier id",
		"refer to a box with its tier id",
		"address it via the tier id",
		"a tier-id or node letter",
	} {
		if !openspecTierIDAsMachineName.MatchString(bad) {
			t.Errorf("the pattern misses %q", bad)
		}
	}
	for _, ok := range []string{
		"Machines appear as node letters or role phrases, never by name, and a tier id only as \"the `<tier>` reference box\"",
		"a node letter (`<node-a>`..`<node-f>`) or a role phrase, and a tier id only as the <tier> reference box",
		"the tier is chosen by the config",
	} {
		if m := openspecTierIDAsMachineName.FindString(ok); m != "" {
			t.Errorf("the pattern flags %q in %q", m, ok)
		}
	}
}

// The tree's public text names a machine the way docs/STYLE.md V1 to V4 say: a node letter or a role phrase, and a tier
// id only inside "the <tier> reference box". A tier id names a hardware class, never a machine, so "name a machine by
// tier id" is the sentence the style guide forbids. The check reads the config, the schema, the templates and the docs
// page, with line wraps folded away.
func TestOpenSpecTextNamesMachinesByNodeLetter(t *testing.T) {
	files := []string{
		"openspec/config.yaml",
		"openspec/schemas/harness-change/schema.yaml",
		"openspec/schemas/harness-change/templates/adr.md",
		"docs/systems/openspec.md",
	}
	forbidden := openspecTierIDAsMachineName
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(raw)), " ")
		if m := forbidden.FindString(text); m != "" {
			t.Errorf("%s says %q: a machine is a node letter or a role phrase (docs/STYLE.md V1), and a tier id is only ever \"the <tier> reference box\" (V2, V4)", f, m)
		}
	}
	raw, err := os.ReadFile("openspec/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if text := strings.Join(strings.Fields(string(raw)), " "); !strings.Contains(text, "reference box") {
		t.Error("openspec/config.yaml must say how a tier id may appear: only as \"the <tier> reference box\"")
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

// withStandInsFirst puts dir, the folder of a test's stand-in tools, at the head of PATH in a script environment built
// by openspecScriptEnv. withBashOnPath puts bash's own directory first, and on Linux that is /usr/bin, where the real
// npm lives. A stand-in must come before it, or the script runs the tool it stands in for.
//
// The same environment also keeps a shadowed stand-in from reaching the network. npm's registry is a closed loopback
// port, so the real npm that runs by mistake is refused at once; but npm retries a refused connection (fetch-retries
// defaults to 2, with a backoff of 10 to 60 s), which stalled each install test about 78 s before it failed. With
// fetch-retries at 0 and npm offline the real npm gives up in a moment instead: offline answers from the cache or fails
// without opening a socket. The stand-in npm is a script that reads none of these, so they cost a correct run nothing.
func withStandInsFirst(env []string, dir string) []string {
	kept := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok {
			switch strings.ToLower(k) {
			case "npm_config_registry", "npm_config_fetch_retries", "npm_config_offline":
				continue
			}
		}
		kept = append(kept, kv)
	}
	return append(prependPath(kept, dir),
		"npm_config_registry=http://127.0.0.1:9/",
		"npm_config_fetch_retries=0",
		"npm_config_offline=true")
}

// An inherited npm_config_* key is replaced whatever its case: Windows spells environment names in any case, and npm
// reads npm_config_* case-insensitively, so an inherited NPM_CONFIG_REGISTRY left beside the stand-in's lower-case one
// could win. Each key leaves exactly one entry, the stand-in's.
func TestOpenSpecStandInEnvironmentReplacesInheritedNpmKeysInAnyCase(t *testing.T) {
	env := withStandInsFirst([]string{
		"KEEP=1",
		"NPM_CONFIG_REGISTRY=https://registry.example.invalid/",
		"Npm_Config_Fetch_Retries=5",
		"NPM_CONFIG_OFFLINE=false",
		"npm_config_cache=/kept/cache",
	}, "standins")
	count := map[string]int{}
	val := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		count[strings.ToLower(k)]++
		val[strings.ToLower(k)] = v
	}
	for key, want := range map[string]string{
		"npm_config_registry":      "http://127.0.0.1:9/",
		"npm_config_fetch_retries": "0",
		"npm_config_offline":       "true",
	} {
		if count[key] != 1 || val[key] != want {
			t.Errorf("%s: %d entries, value %q; want exactly one, %q (an inherited upper-case spelling must be dropped)", key, count[key], val[key], want)
		}
	}
	if val["keep"] != "1" || val["npm_config_cache"] != "/kept/cache" {
		t.Errorf("unrelated entries were not kept: %v", val)
	}
}

// On Linux bash lives in /usr/bin beside the real npm. If bash's directory came ahead of the stand-ins, the install
// script would run the real npm against the registry instead of the stand-in. This test builds that layout on any
// host: a decoy "bash directory" ahead of everything else, holding an npm that records it ran and fails.
func TestOpenSpecStandInsAreNotShadowedByBashsDirectory(t *testing.T) {
	bash := guardBash(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	decoy := filepath.Join(dir, "decoy")
	for _, d := range []string{bin, decoy} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	shadow := filepath.ToSlash(filepath.Join(dir, "shadow-called.txt"))
	writeOpenSpecStandIn(t, decoy, "npm", "echo shadowed >> \"$SHADOW\"\nexit 1\n")
	writeOpenSpecStandIn(t, bin, "npm", `case "$1" in
  pack)
    printf 'not the admitted tarball' > fission-ai-openspec-1.14.1.tgz
    echo fission-ai-openspec-1.14.1.tgz ;;
esac
`)
	env := withBashOnPath(openspecScriptEnv(t, bash, "SHADOW="+shadow), filepath.Join(decoy, "bash"))
	cmd := winexec.Command(bash, "scripts/openspec-install.sh", filepath.ToSlash(filepath.Join(dir, "prefix")))
	cmd.Env = withStandInsFirst(env, bin)
	out, _ := cmd.CombinedOutput()
	if _, err := os.Stat(shadow); err == nil {
		t.Fatalf("the npm in bash's directory ran instead of the stand-in:\n%s", out)
	}
	if !strings.Contains(string(out), "REFUSED: sha256 mismatch") {
		t.Fatalf("the stand-in npm did not reach the hash check:\n%s", out)
	}
	envOf := func(key string) string {
		v := ""
		for _, kv := range cmd.Env {
			if k, val, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, key) {
				v = val
			}
		}
		return v
	}
	if registry := envOf("npm_config_registry"); !strings.HasPrefix(registry, "http://127.0.0.1:") {
		t.Errorf("npm_config_registry = %q: a stand-in run must point npm at a closed loopback port, so a shadowed stand-in fails without the network", registry)
	}
	// npm retries a refused connection (fetch-retries defaults to 2, with a backoff of 10 to 60 s): measured, each
	// shadowed install test stalled about 78 s before it failed. No retries, and offline, make it fail in a moment.
	if got := envOf("npm_config_fetch_retries"); got != "0" {
		t.Errorf("npm_config_fetch_retries = %q, want 0: npm would retry the closed registry port with a long backoff", got)
	}
	if got := envOf("npm_config_offline"); got != "true" {
		t.Errorf("npm_config_offline = %q, want true: a stand-in run never needs the network", got)
	}
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

// The validate script moves to the repository root before it runs the CLI, so a relative OPENSPEC_BIN has to be made
// absolute against the caller's directory first. The stand-in lives in a folder of its own, the script runs with that
// folder as its working directory, and OPENSPEC_BIN names the stand-in relative to it.
func TestOpenSpecValidateScriptResolvesARelativeBinaryFromTheCallersDirectory(t *testing.T) {
	bash := guardBash(t)
	script, err := filepath.Abs(filepath.Join("scripts", "openspec-validate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"./openspec", "tools/openspec"} {
		t.Run(rel, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "tools"), 0o755); err != nil {
				t.Fatal(err)
			}
			out := filepath.ToSlash(filepath.Join(dir, "calls.txt"))
			writeOpenSpecStandIn(t, filepath.Join(dir, filepath.Dir(filepath.FromSlash(rel))), "openspec", `echo "args=$*" >> "$OUT"
`)
			cmd := winexec.Command(bash, filepath.ToSlash(script))
			cmd.Dir = dir
			cmd.Env = openspecScriptEnv(t, bash, "OPENSPEC_BIN="+rel, "OUT="+out)
			combined, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("OPENSPEC_BIN=%s, relative to the caller's directory, was not found: %v\n%s", rel, err, combined)
			}
			calls, _ := os.ReadFile(filepath.FromSlash(out))
			if !strings.Contains(string(calls), "args=validate --all --strict --no-interactive") {
				t.Errorf("the stand-in at %s did not run the validation, calls were:\n%s", rel, calls)
			}
		})
	}
}

// A binary that is not there exits 2, which is how a missing tool differs from a red validation (any other non-zero
// code): a caller can tell "install the CLI" from "fix the spec". It holds for an absolute and a relative name.
func TestOpenSpecValidateScriptExitsTwoWhenTheBinaryIsMissing(t *testing.T) {
	bash := guardBash(t)
	script, err := filepath.Abs(filepath.Join("scripts", "openspec-validate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// named is what the message must carry: the path as the caller gave it, so a relative name whose folder does not exist
	// is reported as given (the script keeps it unchanged when there is no folder to make it absolute against).
	for _, c := range []struct{ bin, named string }{
		{filepath.ToSlash(filepath.Join(dir, "no-such-openspec")), filepath.ToSlash(filepath.Join(dir, "no-such-openspec"))},
		{"./no-such-openspec", "no-such-openspec"},
		{"tools/no-such-openspec", "tools/no-such-openspec"},
	} {
		bin := c.bin
		cmd := winexec.Command(bash, filepath.ToSlash(script))
		cmd.Dir = dir
		cmd.Env = openspecScriptEnv(t, bash, "OPENSPEC_BIN="+bin)
		combined, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Errorf("OPENSPEC_BIN=%s: want an exit error, got %v:\n%s", bin, err, combined)
			continue
		}
		if ee.ExitCode() != 2 {
			t.Errorf("OPENSPEC_BIN=%s: exit code %d, want 2:\n%s", bin, ee.ExitCode(), combined)
		}
		if !strings.Contains(string(combined), "not found") {
			t.Errorf("OPENSPEC_BIN=%s: the message does not say the binary was not found:\n%s", bin, combined)
		}
		if !strings.Contains(string(combined), c.named) {
			t.Errorf("OPENSPEC_BIN=%s: the message does not name %q:\n%s", bin, c.named, combined)
		}
	}
}

// bash's cd consults CDPATH for a relative directory and then prints the directory it chose, so an exported CDPATH
// that names a folder holding a same-named subfolder would send the validate script's `cd "$bin_dir"` somewhere else
// and put a second line into its command substitution. The script clears CDPATH for the whole run. The folder `other`
// beside the caller's directory holds a decoy `tools`, and CDPATH names `other` (relative, so the test is the same on
// every host, where a Windows path would break on its drive colon).
func TestOpenSpecValidateScriptIgnoresAnInheritedCDPATH(t *testing.T) {
	bash := guardBash(t)
	script, err := filepath.Abs(filepath.Join("scripts", "openspec-validate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, d := range []string{"tools", filepath.Join("other", "tools")} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.ToSlash(filepath.Join(dir, "calls.txt"))
	writeOpenSpecStandIn(t, filepath.Join(dir, "tools"), "openspec", `echo "args=$*" >> "$OUT"
`)
	cmd := winexec.Command(bash, filepath.ToSlash(script))
	cmd.Dir = dir
	cmd.Env = openspecScriptEnv(t, bash, "OPENSPEC_BIN=tools/openspec", "OUT="+out, "CDPATH=other")
	combined, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("an inherited CDPATH broke the relative OPENSPEC_BIN: %v\n%s", err, combined)
	}
	calls, _ := os.ReadFile(filepath.FromSlash(out))
	if !strings.Contains(string(calls), "args=validate --all --strict --no-interactive") {
		t.Errorf("the stand-in in the caller's tools folder did not run the validation, calls were:\n%s", calls)
	}
}

// The install script's `cd "$prefix"` has the same exposure: a relative prefix is resolved through CDPATH unless the
// script clears it. The decoy `other/prefix` exists, the real prefix is the caller's own `prefix`, and the run must
// reach the hash check with its scratch folder under the real one.
func TestOpenSpecInstallScriptIgnoresAnInheritedCDPATH(t *testing.T) {
	bash := guardBash(t)
	script, err := filepath.Abs(filepath.Join("scripts", "openspec-install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{bin, filepath.Join(dir, "prefix"), filepath.Join(dir, "other", "prefix")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mark := filepath.ToSlash(filepath.Join(dir, "pack-cwd.txt"))
	writeOpenSpecStandIn(t, bin, "npm", `case "$1" in
  pack)
    pwd > "$MARK"
    printf 'not the admitted tarball' > fission-ai-openspec-1.14.1.tgz ;;
esac
`)
	cmd := winexec.Command(bash, filepath.ToSlash(script), "prefix")
	cmd.Dir = dir
	cmd.Env = withStandInsFirst(openspecScriptEnv(t, bash, "MARK="+mark, "CDPATH=other"), bin)
	combined, _ := cmd.CombinedOutput()
	if !strings.Contains(string(combined), "REFUSED: sha256 mismatch") {
		t.Fatalf("an inherited CDPATH kept the install script from resolving its relative prefix:\n%s", combined)
	}
	packed, err := os.ReadFile(filepath.FromSlash(mark))
	if err != nil {
		t.Fatalf("npm pack never ran: %v\n%s", err, combined)
	}
	if got := strings.ReplaceAll(strings.TrimSpace(string(packed)), `\`, "/"); strings.Contains(got, "/other/") || !strings.Contains(got, "/prefix/") {
		t.Errorf("npm pack ran in %q, want a scratch folder under the caller's prefix, not the CDPATH decoy", got)
	}
}

var (
	openspecAdmittedRowVersion = regexp.MustCompile("\\| (\\d+\\.\\d+\\.\\d+), tarball sha256 `([0-9a-f]{64})`")
	openspecScriptVersion      = regexp.MustCompile(`(?m)^readonly OPENSPEC_VERSION="([^"]+)"$`)
	openspecScriptSHA          = regexp.MustCompile(`(?m)^readonly OPENSPEC_SHA256="([0-9a-f]{64})"$`)
)

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
	cmd.Env = withStandInsFirst(openspecScriptEnv(t, bash, "MARK="+mark), bin)
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

// What the stand-in npm "packs". The install tests rewrite the script's pinned sha256 on a copy to this content's hash
// (the constants are readonly, so a copy is the way a test gets a hash it can match).
const openspecStandInTarball = "stand-in for the openspec tarball, not a real package"

type openspecInstallRun struct {
	out    string
	err    error
	prefix string            // the install prefix as the script sees it (symlinks and short names resolved)
	calls  []string          // the npm subcommands the stand-in saw, in order
	log    map[string]string // what the stand-in recorded: "pack argv", "ci cwd", "ci cache", ...
}

// copyOpenSpecInstallScript puts a copy of the install script, with its pinned sha256 replaced by sha, under
// dir/scripts, together with the lockfile project the script reads from beside itself (scripts/openspec/). It returns
// the copy's path.
func copyOpenSpecInstallScript(t *testing.T, dir, sha string) string {
	t.Helper()
	script, err := os.ReadFile("scripts/openspec-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	pin := regexp.MustCompile(`(?m)^readonly OPENSPEC_SHA256="[0-9a-f]{64}"$`)
	if !pin.Match(script) {
		t.Fatal(`scripts/openspec-install.sh has no line readonly OPENSPEC_SHA256="<64 hex>"`)
	}
	scripts := filepath.Join(dir, "scripts")
	if err := os.MkdirAll(filepath.Join(scripts, "openspec"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(scripts, "openspec-install.sh")
	if err := os.WriteFile(copyPath, pin.ReplaceAll(script, []byte(`readonly OPENSPEC_SHA256="`+sha+`"`)), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"package.json", "package-lock.json"} {
		b, err := os.ReadFile(filepath.Join("scripts", "openspec", f))
		if err != nil {
			t.Fatalf("the install script installs from the committed lockfile project, scripts/openspec/%s: %v", f, err)
		}
		if err := os.WriteFile(filepath.Join(scripts, "openspec", f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.ToSlash(copyPath)
}

// runOpenSpecInstallWithStandInNpm runs a copy of the install script, whose pin matches the stand-in tarball, against a
// stand-in npm. The stand-in records its subcommands, the arguments, the directory it ran in, the cache and temp
// settings it saw and whether the lockfile project and the tarball it was handed are the committed ones; its `ci`
// "installs" an openspec binary that prints fakeVersion and records the config home it ran with. The caller's environment is hostile on purpose (npm's cache
// and the temp folders point outside the prefix), so only the script itself can move them under the prefix.
func runOpenSpecInstallWithStandInNpm(t *testing.T, fakeVersion string, prepare func(prefix string)) openspecInstallRun {
	t.Helper()
	return runOpenSpecInstallAt(t, fakeVersion, false, prepare)
}

// runOpenSpecInstallAt is runOpenSpecInstallWithStandInNpm with a choice of how the prefix is spelled to the script:
// when viaSymlink is set, the script is handed a symlink to the prefix, as a macOS temp folder is (/var is a link to
// /private/var). The run's prefix is always the physical folder.
func runOpenSpecInstallAt(t *testing.T, fakeVersion string, viaSymlink bool, prepare func(prefix string)) openspecInstallRun {
	t.Helper()
	bash := guardBash(t)
	dir := t.TempDir()
	sum := sha256.Sum256([]byte(openspecStandInTarball))
	script := copyOpenSpecInstallScript(t, dir, hex.EncodeToString(sum[:]))
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "npm-calls.log")
	writeOpenSpecStandIn(t, bin, "npm", `log() { printf '%s\n' "$*" >> "$LOG"; }
# What npm would use for its cache and its temp files in this step, as the script left the environment.
log_env() {
  log "$1 cache=${npm_config_cache-unset}"
  log "$1 tmpdir=${TMPDIR-unset}"
  log "$1 tmp=${TMP-unset}"
  log "$1 temp=${TEMP-unset}"
}
log "call=$1"
case "$1" in
  pack)
    log "pack argv=$*"
    log_env pack
    printf '%s' "$STANDIN_TARBALL" > fission-ai-openspec-1.14.1.tgz
    echo fission-ai-openspec-1.14.1.tgz ;;
  ci)
    log "ci argv=$*"
    log "ci cwd=$(pwd -W 2>/dev/null || pwd)"
    log_env ci
    cmp -s package.json "$SRC/package.json" && log "ci package.json=same" || log "ci package.json=different"
    cmp -s package-lock.json "$SRC/package-lock.json" && log "ci lockfile=same" || log "ci lockfile=different"
    [ "$(cat fission-ai-openspec-1.14.1.tgz)" = "$STANDIN_TARBALL" ] && log "ci tarball=same" || log "ci tarball=different"
    mkdir -p node_modules/.bin
    printf '#!/usr/bin/env bash\necho "bin xdg=${XDG_CONFIG_HOME-unset}" >> "%s"\necho %s\n' "$LOG" "$FAKE_VERSION" > node_modules/.bin/openspec
    chmod +x node_modules/.bin/openspec ;;
  *)
    exit 1 ;;
esac
`)
	repoProject, err := filepath.Abs(filepath.Join("scripts", "openspec"))
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(dir, "prefix")
	if err := os.MkdirAll(prefix, 0o755); err != nil {
		t.Fatal(err)
	}
	if prepare != nil {
		prepare(prefix)
	}
	hostile := func(name string) string {
		p := filepath.Join(dir, "hostile-"+name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return filepath.ToSlash(p)
	}
	realPrefix, err := filepath.EvalSymlinks(prefix)
	if err != nil {
		t.Fatal(err)
	}
	arg := prefix
	if viaSymlink {
		arg = filepath.Join(dir, "prefix-link")
		if err := os.Symlink(prefix, arg); err != nil {
			t.Skipf("this host cannot create a symlink: %v", err)
		}
	}
	cmd := winexec.Command(bash, script, filepath.ToSlash(arg))
	cmd.Env = openspecScriptEnv(t, bash,
		"LOG="+filepath.ToSlash(logPath), "SRC="+filepath.ToSlash(repoProject),
		"STANDIN_TARBALL="+openspecStandInTarball, "FAKE_VERSION="+fakeVersion,
		"npm_config_cache="+hostile("cache"), "TMPDIR="+hostile("tmpdir"), "TMP="+hostile("tmp"), "TEMP="+hostile("temp"),
		"XDG_CONFIG_HOME="+hostile("xdg"))
	cmd.Env = withStandInsFirst(cmd.Env, bin)
	out, runErr := cmd.CombinedOutput()
	r := openspecInstallRun{out: string(out), err: runErr, prefix: realPrefix, log: map[string]string{}}
	raw, _ := os.ReadFile(logPath)
	for _, l := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(l, "\r"), "=")
		if !ok {
			continue
		}
		if k == "call" {
			r.calls = append(r.calls, v)
			continue
		}
		r.log[k] = v
	}
	return r
}

// openspecPathUnder reports whether path is strictly inside root, comparing as the script and the OS may spell them
// (slashes, and a drive letter's case on Windows).
func openspecPathUnder(path, root string) bool {
	norm := func(p string) string { return strings.ToLower(strings.TrimRight(filepath.ToSlash(p), "/")) }
	return strings.HasPrefix(norm(path), norm(root)+"/")
}

func openspecSamePath(a, b string) bool {
	norm := func(p string) string { return strings.ToLower(strings.TrimRight(filepath.ToSlash(p), "/")) }
	return norm(a) == norm(b)
}

// The data-location cell of the admitted-software row says where the install lands. The script takes a prefix
// directory from the caller and puts node_modules, the npm cache and the config home under it; an npm `--prefix` is
// not how it works, and this row is what a reader trusts.
func TestOpenSpecAdmittedRowDescribesTheInstallLayout(t *testing.T) {
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
	if strings.Contains(row, "--prefix") {
		t.Error("the OpenSpec row still says `--prefix`: the install script takes a prefix directory and does not pass npm's --prefix")
	}
	for _, want := range []string{"prefix directory", "node_modules", ".npm-cache", "XDG_CONFIG_HOME"} {
		if !strings.Contains(row, want) {
			t.Errorf("the OpenSpec row's data-location cell does not mention %q", want)
		}
	}
}

// The success path, with a stand-in npm: pack by the scoped, pinned name; install with `npm ci --ignore-scripts` in a
// staged copy of the committed lockfile project, next to the verified tarball; npm's cache and temp files under the
// prefix whatever the caller exported; the binary in the prefix; no scratch directory left; the four exports printed.
func TestOpenSpecInstallScriptInstallsFromTheLockfileUnderThePrefix(t *testing.T) {
	r := runOpenSpecInstallWithStandInNpm(t, "1.14.1", nil)
	if r.err != nil {
		t.Fatalf("the script failed on a matching tarball and a matching binary: %v\n%s", r.err, r.out)
	}
	if got := strings.Join(r.calls, ","); got != "pack,ci" {
		t.Errorf("npm was called with subcommands %q, want exactly pack then ci (no resolving install)", got)
	}
	if a := r.log["pack argv"]; !strings.HasPrefix(a, "pack @fission-ai/openspec@1.14.1 ") || !strings.Contains(a, "--ignore-scripts") {
		t.Errorf("npm pack ran as %q, want the scoped, pinned name with --ignore-scripts", a)
	}
	if a := r.log["ci argv"]; !strings.HasPrefix(a, "ci ") || !strings.Contains(a, "--ignore-scripts") {
		t.Errorf("npm ci ran as %q, want ci with --ignore-scripts", a)
	}
	if !openspecPathUnder(r.log["ci cwd"], r.prefix) {
		t.Errorf("npm ci ran in %q, want a staged copy under the prefix %q", r.log["ci cwd"], r.prefix)
	}
	for _, k := range []string{"ci package.json", "ci lockfile", "ci tarball"} {
		if r.log[k] != "same" {
			t.Errorf("%s = %q: npm ci must run in a copy that holds the committed package.json, the committed package-lock.json and the verified tarball", k, r.log[k])
		}
	}
	// Both steps that touch npm's cache and temp files, the fetch and the install, run with them under the prefix:
	// the caller exported folders elsewhere (the user profile, the system drive), and only the script can move them.
	for _, step := range []string{"pack", "ci"} {
		if !openspecSamePath(r.log[step+" cache"], filepath.Join(r.prefix, ".npm-cache")) {
			t.Errorf("npm %s used the cache %q, want <prefix>/.npm-cache whatever the caller exported", step, r.log[step+" cache"])
		}
		for _, v := range []string{"tmpdir", "tmp", "temp"} {
			k := step + " " + v
			if !openspecPathUnder(r.log[k], r.prefix) {
				t.Errorf("%s was %q, want a folder under the prefix %q whatever the caller exported", k, r.log[k], r.prefix)
			}
		}
	}
	for _, want := range []string{"export OPENSPEC_TELEMETRY=0", "export DO_NOT_TRACK=1", "export OPENSPEC_NO_UPDATE_CHECK=1"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("the output does not print %q:\n%s", want, r.out)
		}
	}
	if !openspecPathUnder(r.log["bin xdg"], r.prefix) {
		t.Errorf("the CLI's version check ran with XDG_CONFIG_HOME=%q, want a folder under the prefix %q whatever the caller exported", r.log["bin xdg"], r.prefix)
	}
	xdg := regexp.MustCompile(`(?m)^export XDG_CONFIG_HOME='([^']+)'\r?$`).FindStringSubmatch(r.out)
	if xdg == nil || !openspecPathUnder(xdg[1], r.prefix) {
		t.Errorf("the output must print an XDG_CONFIG_HOME export under the prefix, got %v in:\n%s", xdg, r.out)
	}
	if _, err := os.Stat(filepath.Join(r.prefix, "node_modules", ".bin", "openspec")); err != nil {
		t.Errorf("the installed binary is not in the prefix: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(r.prefix, ".openspec-pack.*")); len(left) != 0 {
		t.Errorf("the scratch directory was left behind: %v", left)
	}
	entries, _ := os.ReadDir(r.prefix)
	for _, e := range entries {
		switch e.Name() {
		case "node_modules", ".npm-cache", "config":
		default:
			t.Errorf("unexpected entry %q in the prefix after the install", e.Name())
		}
	}
}

// A prefix handed over as a symlink (a macOS temp folder: /var is a link to /private/var) is installed into, and every
// path the script logs or prints is the physical one, which is how the other tests, and a caller who resolves the
// prefix, spell it. The script settles on the physical path after the Windows attempt (`pwd -W`), which has no
// symlinks to resolve.
func TestOpenSpecInstallScriptSpellsAPrefixSymlinkPhysically(t *testing.T) {
	r := runOpenSpecInstallAt(t, "1.14.1", true, nil)
	if r.err != nil {
		t.Fatalf("the script failed on a symlinked prefix: %v\n%s", r.err, r.out)
	}
	if !openspecPathUnder(r.log["ci cwd"], r.prefix) {
		t.Errorf("npm ci ran in %q, want a folder under the physical prefix %q", r.log["ci cwd"], r.prefix)
	}
	if !openspecSamePath(r.log["ci cache"], filepath.Join(r.prefix, ".npm-cache")) {
		t.Errorf("npm ci used the cache %q, want <physical prefix>/.npm-cache", r.log["ci cache"])
	}
	xdg := regexp.MustCompile(`(?m)^export XDG_CONFIG_HOME='([^']+)'\r?$`).FindStringSubmatch(r.out)
	if xdg == nil || !openspecPathUnder(xdg[1], r.prefix) {
		t.Errorf("the printed XDG_CONFIG_HOME must be under the physical prefix %q, got %v in:\n%s", r.prefix, xdg, r.out)
	}
}

// A binary that reports another version than the pin fails the install, and nothing of it reaches the prefix.
func TestOpenSpecInstallScriptRejectsAnInstalledVersionThatIsNotThePin(t *testing.T) {
	r := runOpenSpecInstallWithStandInNpm(t, "9.9.9", nil)
	if r.err == nil {
		t.Fatalf("the script accepted an installed CLI that reports 9.9.9:\n%s", r.out)
	}
	if !strings.Contains(r.out, "9.9.9") {
		t.Errorf("the failure must name the version it found, got:\n%s", r.out)
	}
	if left, _ := filepath.Glob(filepath.Join(r.prefix, ".openspec-pack.*")); len(left) != 0 {
		t.Errorf("the scratch directory was left behind: %v", left)
	}
	if _, err := os.Stat(filepath.Join(r.prefix, "node_modules")); err == nil {
		t.Error("a node_modules directory reached the prefix although the installed version was wrong")
	}
}

// npm ci replaces a node_modules wholesale, so the script replaces an earlier install of this CLI but never a
// node_modules that is something else's.
func TestOpenSpecInstallScriptReplacesAnEarlierInstallOnly(t *testing.T) {
	earlier := runOpenSpecInstallWithStandInNpm(t, "1.14.1", func(prefix string) {
		d := filepath.Join(prefix, "node_modules", "@fission-ai", "openspec")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"package.json", "stale-file-of-the-earlier-install.txt"} {
			if err := os.WriteFile(filepath.Join(d, f), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	})
	if earlier.err != nil {
		t.Fatalf("the script failed over an earlier install of the CLI: %v\n%s", earlier.err, earlier.out)
	}
	if _, err := os.Stat(filepath.Join(earlier.prefix, "node_modules", "@fission-ai", "openspec", "stale-file-of-the-earlier-install.txt")); err == nil {
		t.Error("the earlier install's files are still in node_modules: the new install must replace it")
	}
	if _, err := os.Stat(filepath.Join(earlier.prefix, "node_modules", ".bin", "openspec")); err != nil {
		t.Errorf("the new binary is not in the prefix: %v", err)
	}

	var foreign string
	other := runOpenSpecInstallWithStandInNpm(t, "1.14.1", func(prefix string) {
		d := filepath.Join(prefix, "node_modules", "some-other-tool")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		foreign = filepath.Join(d, "package.json")
		if err := os.WriteFile(foreign, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if other.err == nil {
		t.Fatalf("the script replaced a node_modules that holds no OpenSpec install:\n%s", other.out)
	}
	if !strings.Contains(other.out, "node_modules") {
		t.Errorf("the refusal must name node_modules, got:\n%s", other.out)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("the foreign node_modules lost a file: %v", err)
	}
	if len(other.calls) != 0 {
		t.Errorf("npm ran (%v) although the prefix was refused", other.calls)
	}
}

type openspecLockPackage struct {
	Version   string `json:"version"`
	Resolved  string `json:"resolved"`
	Integrity string `json:"integrity"`
	Link      bool   `json:"link"`
}

type openspecLockfile struct {
	LockfileVersion int `json:"lockfileVersion"`
	Packages        map[string]struct {
		openspecLockPackage
		Dependencies map[string]string `json:"dependencies"`
	} `json:"packages"`
}

func readOpenSpecLockfile(t *testing.T) openspecLockfile {
	t.Helper()
	raw, err := os.ReadFile("scripts/openspec/package-lock.json")
	if err != nil {
		t.Fatalf("the install script installs from a committed lockfile: %v", err)
	}
	var l openspecLockfile
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatalf("scripts/openspec/package-lock.json is not valid JSON: %v", err)
	}
	return l
}

func openspecSRI(t *testing.T, where, integrity string) {
	t.Helper()
	b64, ok := strings.CutPrefix(integrity, "sha512-")
	if !ok {
		t.Errorf("%s: integrity %q is not a sha512 value", where, integrity)
		return
	}
	if raw, err := base64.StdEncoding.DecodeString(b64); err != nil || len(raw) != 64 {
		t.Errorf("%s: integrity %q is not the base64 of a 64-byte sha512", where, integrity)
	}
}

// The lockfile pins the dependency tree: the install project names the one admitted tarball by file, and every other
// package in the lock is an exact version from the registry under a sha512 value. A caret range, a missing integrity or
// a link would let the tree drift between installs.
func TestOpenSpecLockfilePinsTheWholeTree(t *testing.T) {
	script, err := os.ReadFile("scripts/openspec-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	v := openspecScriptVersion.FindStringSubmatch(string(script))
	if v == nil {
		t.Fatal("scripts/openspec-install.sh declares no readonly OPENSPEC_VERSION")
	}
	tarball := "fission-ai-openspec-" + v[1] + ".tgz"
	if !strings.Contains(string(script), `fission-ai-openspec-${OPENSPEC_VERSION}.tgz`) {
		t.Error("scripts/openspec-install.sh must stage the verified tarball under the name the lockfile project refers to (fission-ai-openspec-${OPENSPEC_VERSION}.tgz)")
	}

	rawProject, err := os.ReadFile("scripts/openspec/package.json")
	if err != nil {
		t.Fatalf("the install script installs from a committed project: %v", err)
	}
	var project struct {
		Private      bool              `json:"private"`
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(rawProject, &project); err != nil {
		t.Fatalf("scripts/openspec/package.json is not valid JSON: %v", err)
	}
	if !project.Private {
		t.Error("scripts/openspec/package.json must be private: it is an install recipe, never a package to publish")
	}
	want := map[string]string{"@fission-ai/openspec": "file:" + tarball}
	if len(project.Dependencies) != 1 || project.Dependencies["@fission-ai/openspec"] != want["@fission-ai/openspec"] {
		t.Errorf("scripts/openspec/package.json dependencies are %v, want exactly %v (the admitted tarball by file, never a range)", project.Dependencies, want)
	}

	l := readOpenSpecLockfile(t)
	if l.LockfileVersion != 3 {
		t.Errorf("lockfileVersion is %d, want 3", l.LockfileVersion)
	}
	root, ok := l.Packages[""]
	if !ok || len(root.Dependencies) != 1 || root.Dependencies["@fission-ai/openspec"] != want["@fission-ai/openspec"] {
		t.Errorf("the lockfile's root entry depends on %v, want exactly %v (npm ci refuses a lock that disagrees with package.json)", root.Dependencies, want)
	}
	cli, ok := l.Packages["node_modules/@fission-ai/openspec"]
	if !ok {
		t.Fatal("the lockfile has no node_modules/@fission-ai/openspec entry")
	}
	if cli.Version != v[1] || cli.Resolved != "file:"+tarball {
		t.Errorf("the lockfile's CLI entry is version %q resolved %q, want %q and %q", cli.Version, cli.Resolved, v[1], "file:"+tarball)
	}
	openspecSRI(t, "node_modules/@fission-ai/openspec", cli.Integrity)

	exact := regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
	deps := 0
	for name, p := range l.Packages {
		if name == "" || name == "node_modules/@fission-ai/openspec" {
			continue
		}
		deps++
		if !strings.HasPrefix(name, "node_modules/") {
			t.Errorf("lock entry %q is not under node_modules/: a workspace or a link would escape the pin", name)
		}
		if p.Link {
			t.Errorf("%s is a link", name)
		}
		if !exact.MatchString(p.Version) {
			t.Errorf("%s has version %q, want an exact version", name, p.Version)
		}
		if !strings.HasPrefix(p.Resolved, "https://registry.npmjs.org/") {
			t.Errorf("%s resolves to %q, want the npm registry", name, p.Resolved)
		}
		openspecSRI(t, name, p.Integrity)
	}
	if deps < 2 {
		t.Errorf("the lockfile pins %d dependencies besides the CLI: the tree is not pinned", deps)
	}
}

// TestOpenSpecInstallScriptInstallsTheRealPackage runs the install script for real: real npm, the network, about 70
// packages, into a scratch prefix. It runs only with OPENSPEC_INSTALL_TEST=1 and skips with the reason otherwise. Home,
// application-data, XDG and temp folders are all pointed at empty folders beside the prefix, so a write outside the
// prefix shows up as a file there; the installed package set is compared with the committed lockfile.
func TestOpenSpecInstallScriptInstallsTheRealPackage(t *testing.T) {
	if os.Getenv("OPENSPEC_INSTALL_TEST") != "1" {
		t.Skip("OPENSPEC_INSTALL_TEST=1 is not set: this test runs scripts/openspec-install.sh for real (npm, the network, about 70 packages) into a scratch prefix; the stand-in install tests above still ran")
	}
	bash := guardBash(t)
	root := t.TempDir()
	sandbox := map[string]string{}
	var env []string
	for _, k := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "TMP", "TEMP", "TMPDIR"} {
		d := filepath.Join(root, "outside-"+strings.ToLower(k))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		sandbox[k] = d
		env = append(env, k+"="+filepath.ToSlash(d))
	}
	prefix := filepath.Join(root, "prefix")

	cmd := winexec.Command(bash, "scripts/openspec-install.sh", filepath.ToSlash(prefix))
	cmd.Env = openspecScriptEnv(t, bash, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scripts/openspec-install.sh failed: %v\n%s", err, out)
	}
	t.Logf("install output:\n%s", out)

	script, err := os.ReadFile("scripts/openspec-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	pin := openspecScriptVersion.FindStringSubmatch(string(script))[1]
	run := winexec.Command(bash, filepath.ToSlash(filepath.Join(prefix, "node_modules", ".bin", "openspec")), "--version")
	run.Env = openspecScriptEnv(t, bash, append(env, "OPENSPEC_TELEMETRY=0", "DO_NOT_TRACK=1", "OPENSPEC_NO_UPDATE_CHECK=1")...)
	ver, err := run.Output()
	if err != nil {
		t.Fatalf("the installed binary does not run: %v", err)
	}
	if got := strings.TrimSpace(string(ver)); got != pin {
		t.Errorf("the installed binary reports %q, want the pin %q", got, pin)
	}

	for k, d := range sandbox {
		entries, _ := os.ReadDir(d)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if len(names) != 0 {
			t.Errorf("the install wrote outside the prefix, into the stand-in %s folder: %v", k, names)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(prefix, ".openspec-pack.*")); len(left) != 0 {
		t.Errorf("the scratch directory was left behind: %v", left)
	}

	// The installed tree is the committed lock's tree: same packages, same versions, same integrity values.
	committed := readOpenSpecLockfile(t)
	hiddenRaw, err := os.ReadFile(filepath.Join(prefix, "node_modules", ".package-lock.json"))
	if err != nil {
		t.Fatalf("npm left no node_modules/.package-lock.json to compare: %v", err)
	}
	var hidden openspecLockfile
	if err := json.Unmarshal(hiddenRaw, &hidden); err != nil {
		t.Fatal(err)
	}
	for name, p := range committed.Packages {
		if name == "" {
			continue
		}
		h, ok := hidden.Packages[name]
		if !ok {
			t.Errorf("%s is in the committed lockfile but was not installed", name)
			continue
		}
		if h.Version != p.Version || h.Integrity != p.Integrity {
			t.Errorf("%s installed as %s %s, the lockfile pins %s %s", name, h.Version, h.Integrity, p.Version, p.Integrity)
		}
	}
	for name := range hidden.Packages {
		if _, ok := committed.Packages[name]; !ok {
			t.Errorf("%s was installed but is not in the committed lockfile", name)
		}
	}
}

// The OpenSpec validator runs in no automatic gate (CI does not set OPENSPEC_BIN), so the pre-PR list a contributor
// reads must name it for a change under openspec/.
func TestOpenSpecValidateGateIsInTheContributorChecklist(t *testing.T) {
	b, err := os.ReadFile("CONTRIBUTING.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	i := strings.Index(text, "## Opening a pull request")
	if i < 0 {
		t.Fatal("CONTRIBUTING.md has no `## Opening a pull request` section")
	}
	section := text[i:]
	for _, want := range []string{"openspec/", "scripts/openspec-validate.sh", "OPENSPEC_BIN", "scripts/openspec-install.sh"} {
		if !strings.Contains(section, want) {
			t.Errorf("the pre-PR list in CONTRIBUTING.md does not mention %q", want)
		}
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
