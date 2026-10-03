package agent

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestReadFloorCoversTheWiderSecretSet (SF-07 review): more key, certificate and
// credential names, and whole credential directories (a directory named .env too).
func TestReadFloorCoversTheWiderSecretSet(t *testing.T) {
	for _, p := range []string{".netrc", ".pypirc", "cert.p12", "cert.pfx", "id_ecdsa", "id_dsa.old",
		".aws/credentials", "home/.kube/config", ".gnupg/pubring.kbx", ".env/config.json", ".ssh"} {
		if _, hit := readFloorHit(p); !hit {
			t.Errorf("readFloorHit(%q) = miss, want a hit", p)
		}
	}
	for _, p := range []string{"env/config.json", "awsome.go", "kube.yaml"} {
		if g, hit := readFloorHit(p); hit {
			t.Errorf("readFloorHit(%q) = hit (%s), want a miss", p, g)
		}
	}
}

// TestSearchUnderEnforceIsNoOracle (SF-07 review B2): a pattern that matches a
// secret file's content and one that does not give byte-identical output, so a
// search cannot bisect a secret.
func TestSearchUnderEnforceIsNoOracle(t *testing.T) {
	dir := floorWorktree(t) // .env holds TOKEN=abc123
	res, _ := buildForFloor(t, dir, "enforce", "")
	search := floorTool(t, res.Tools, "search_files")
	hit, err := search.Exec(context.Background(), `{"pattern":"^TOKEN=abc[0-4]"}`)
	if err != nil {
		t.Fatal(err)
	}
	miss, err := search.Exec(context.Background(), `{"pattern":"^TOKEN=abc[5-9]"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(hit, "[0-4]", "[5-9]") != miss {
		t.Errorf("the output depends on the secret's content:\n%s\n---\n%s", hit, miss)
	}
}

// TestSearchRootedInAFloorDirectoryIsRefused: a search whose path is (or lies under)
// a credential directory is refused, and a search over its parent never reads it.
func TestSearchRootedInAFloorDirectoryIsRefused(t *testing.T) {
	dir := floorWorktree(t)
	if err := os.MkdirAll(filepath.Join(dir, "sub", ".ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", ".ssh", "config"), []byte("TOKEN=sshcfg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _ := buildForFloor(t, dir, "enforce", "")
	search := floorTool(t, res.Tools, "search_files")
	if out, err := search.Exec(context.Background(), `{"pattern":"TOKEN","path":"sub/.ssh"}`); err == nil || !IsNotPerformed(err) {
		t.Errorf("a search rooted in .ssh: out=%q err=%v, want a NotPerformed refusal", out, err)
	}
	out, err := search.Exec(context.Background(), `{"pattern":"TOKEN","path":"sub"}`)
	if err != nil || strings.Contains(out, "sshcfg") {
		t.Errorf("a search over the parent read .ssh: out=%q err=%v", out, err)
	}
}

// TestRefusalsAreNotPerformed (SF-07 review S4): a refused read is the broker's
// declined-action sentinel, so the effect ledger records it as not performed.
func TestRefusalsAreNotPerformed(t *testing.T) {
	dir := floorWorktree(t)
	res, _ := buildForFloor(t, dir, "enforce", "")
	_, err := floorTool(t, res.Tools, "read_file").Exec(context.Background(), `{"path":".env"}`)
	if err == nil || !IsNotPerformed(err) {
		t.Errorf("refused read err = %v, want NotPerformed", err)
	}
}

// TestEditFileIsNoOracleUnderEnforce (SF-07 review S1): edit_file reads the old
// content, so a present and an absent old_string must get the same refusal.
func TestEditFileIsNoOracleUnderEnforce(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".npmrc"), []byte("//registry/:_authToken=npm_S3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Build(BuildConfig{PlannerBase: "http://127.0.0.1:11436", Model: "m", ReadRoot: dir, Worktree: dir,
		Unattended: true, AllowWrite: true, AllowOverwrite: true, ReadFloor: "enforce",
		AuditPath: filepath.Join(t.TempDir(), "agent-audit.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	edit := floorTool(t, res.Tools, "edit_file")
	present, perr := edit.Exec(context.Background(), `{"path":".npmrc","old_string":"_authToken=npm_S","new_string":"x"}`)
	absent, aerr := edit.Exec(context.Background(), `{"path":".npmrc","old_string":"_authToken=npm_Z","new_string":"x"}`)
	if perr == nil || aerr == nil || perr.Error() != aerr.Error() || present != absent {
		t.Errorf("edit_file answers differ by content: present=(%q, %v) absent=(%q, %v)", present, perr, absent, aerr)
	}
}

// TestGitHubUploadNeverSendsSecretMaterial (SF-07 review S1): the upload tool reads a
// worktree file and sends it off-box; a floored file is refused before it is read
// (the fake token and an unreachable API mean nothing could leave anyway).
func TestGitHubUploadNeverSendsSecretMaterial(t *testing.T) {
	dir := floorWorktree(t)
	res, err := Build(BuildConfig{PlannerBase: "http://127.0.0.1:11436", Model: "m", ReadRoot: dir, Worktree: dir,
		Unattended: true, AllowGitHub: true, GitHubToken: "x", GitHubRepo: "o/r", ReadFloor: "enforce",
		AuditPath: filepath.Join(t.TempDir(), "agent-audit.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	_, uerr := floorTool(t, res.Tools, "github_upload_file").Exec(context.Background(), `{"path":".env"}`)
	if uerr == nil || !strings.Contains(uerr.Error(), "read refused") {
		t.Errorf("upload of .env: %v, want a read-floor refusal", uerr)
	}
}

// TestSummarizeFileHonoursTheFloor (SF-07 review S8): summarize_file reads through
// readBounded; under enforce a secret file never reaches the offload.
func TestSummarizeFileHonoursTheFloor(t *testing.T) {
	dir := floorWorktree(t)
	called := false
	res, err := Build(BuildConfig{PlannerBase: "http://127.0.0.1:11436", Model: "m", ReadRoot: dir, Unattended: true,
		ReadFloor: "enforce", AuditPath: filepath.Join(t.TempDir(), "agent-audit.jsonl"),
		Offload: func(context.Context, string, string, map[string]any) (string, error) {
			called = true
			return "summary", nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	_, serr := floorTool(t, res.Tools, "summarize_file").Exec(context.Background(), `{"path":".env"}`)
	if serr == nil || called {
		t.Errorf("summarize .env under enforce: err=%v offload called=%v, want refused and not called", serr, called)
	}
}

// TestAStrictTrailRefusesAReadItCannotRecord (SF-07 review S3): under warn, a read of
// secret material that an enforcing trail cannot record is refused, as Decide
// refuses an unrecordable allow; an advisory trail lets it through.
func TestAStrictTrailRefusesAReadItCannotRecord(t *testing.T) {
	dir := floorWorktree(t)
	for _, tc := range []struct {
		advisory bool
		refused  bool
	}{{false, true}, {true, false}} {
		res, err := Build(BuildConfig{PlannerBase: "http://127.0.0.1:11436", Model: "m", ReadRoot: dir, Unattended: true,
			ReadFloor: "warn", AuditPath: t.TempDir(), AuditAdvisory: tc.advisory}) // the trail is a directory: writes fail
		if err != nil {
			t.Fatal(err)
		}
		_, rerr := floorTool(t, res.Tools, "read_file").Exec(context.Background(), `{"path":".env"}`)
		if (rerr != nil) != tc.refused {
			t.Errorf("advisory=%v: err=%v, want refused=%v", tc.advisory, rerr, tc.refused)
		}
	}
}

// TestBuildNotesTellTheFloorsLimits (SF-07 review S5, S6): --rules off overriding a
// requested floor, read rules recorded but not enforced under warn, and the run/shell
// cage outside an enforced floor are each said.
func TestBuildNotesTellTheFloorsLimits(t *testing.T) {
	dir := t.TempDir()
	notes := func(cfg BuildConfig) string {
		cfg.PlannerBase, cfg.Model, cfg.ReadRoot, cfg.Unattended = "http://127.0.0.1:11436", "m", dir, true
		res, err := Build(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(res.Notes, "\n")
	}
	if n := notes(BuildConfig{ReadFloor: "enforce", RulesPath: RulesOff}); !strings.Contains(n, "read floor OFF") {
		t.Errorf("--rules off over enforce says nothing: %q", n)
	}
	table := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(table, []byte(`[{"kind":"read","glob":"*.db","decision":"deny","severity":"high","reason":"data"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := notes(BuildConfig{RulesPath: table}); !strings.Contains(n, "recorded, not refused") {
		t.Errorf("read rules under warn are not flagged as record-only: %q", n)
	}
}

// TestRipgrepSearchesDotfilesLikeTheGoWalk (SF-07 review S2): both search backends
// must see the same files, dotfiles included, with .git excluded.
func TestRipgrepSearchesDotfilesLikeTheGoWalk(t *testing.T) {
	args := strings.Join(ripgrepArgs("/w", regexp.MustCompile("x"), ""), " ")
	if !strings.Contains(args, "--hidden") || !strings.Contains(args, "--glob !.git") {
		t.Errorf("rg args %q: want --hidden with .git excluded", args)
	}
}

// TestASymlinkToASecretIsTheSecret (SF-07 review S7): a worktree-internal link to a
// floored file is refused by its target, not passed by its own name.
func TestASymlinkToASecretIsTheSecret(t *testing.T) {
	dir := floorWorktree(t)
	if err := os.Symlink(filepath.Join(dir, ".env"), filepath.Join(dir, "innocent.txt")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	res, _ := buildForFloor(t, dir, "enforce", "")
	if out, err := floorTool(t, res.Tools, "read_file").Exec(context.Background(), `{"path":"innocent.txt"}`); err == nil || strings.Contains(out, "abc123") {
		t.Errorf("read through a symlink to .env returned %q, %v; want a refusal", out, err)
	}
}
