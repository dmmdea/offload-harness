package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadFloorMatchesSecretMaterial (register SF-07): the built-in read floor covers
// the secret-material names and nothing else; the folding mirrors the write floor
// (case, trailing dots), and the example/sample env files stay readable.
func TestReadFloorMatchesSecretMaterial(t *testing.T) {
	for _, p := range []string{".env", "sub/.env.local", ".envrc", ".ENV", "server.pem", "server.pem.", "a/b/tls.key",
		"id_rsa", "id_ed25519", "deep/id_rsa.old", ".npmrc", "x/.npmrc", ".git-credentials", ".claude.json",
		".ssh/config", "home/.ssh/known_hosts", ".SSH/id_x"} {
		if _, hit := readFloorHit(p); !hit {
			t.Errorf("readFloorHit(%q) = miss, want a floor hit", p)
		}
	}
	for _, p := range []string{".env.example", ".env.sample", "sub/.env.example", "main.go", "keys.go", "README.md",
		"environment.txt", "sshconfig", "docs/ssh.md", "pem.go", "key.json"} {
		if g, hit := readFloorHit(p); hit {
			t.Errorf("readFloorHit(%q) = hit (%s), want a miss", p, g)
		}
	}
}

// floorWorktree is a worktree holding one secret file and one ordinary file, both
// containing the word TOKEN.
func floorWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{".env": "TOKEN=abc123\n", "notes.txt": "the TOKEN lives in the env file\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func floorTool(t *testing.T, tools []Tool, name string) Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("tool %q not built", name)
	return Tool{}
}

func readRows(t *testing.T, path string) []auditEntry {
	t.Helper()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rows []auditEntry
	for sc := bufio.NewScanner(f); sc.Scan(); {
		var e auditEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, e)
	}
	return rows
}

// buildForFloor builds an unattended read-only run over dir with the given read-floor
// mode and an audit trail outside the worktree.
func buildForFloor(t *testing.T, dir, mode, rules string) (*BuildResult, string) {
	t.Helper()
	audit := filepath.Join(t.TempDir(), "agent-audit.jsonl")
	res, err := Build(BuildConfig{
		PlannerBase: "http://127.0.0.1:11436", Model: "m", ReadRoot: dir,
		Unattended: true, AuditPath: audit, ReadFloor: mode, RulesPath: rules,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res, audit
}

// TestReadFileHonoursTheReadFloorPerMode: enforce refuses the secret file and
// records a deny row; warn returns it unchanged and records a warn row; off neither
// refuses nor records; an ordinary file is never touched by the floor.
func TestReadFileHonoursTheReadFloorPerMode(t *testing.T) {
	dir := floorWorktree(t)
	for _, tc := range []struct {
		mode    string
		refuse  bool
		wantRow string
	}{{"enforce", true, "deny"}, {"warn", false, "warn"}, {"", false, "warn"}, {"off", false, ""}} {
		res, audit := buildForFloor(t, dir, tc.mode, "")
		read := floorTool(t, res.Tools, "read_file")
		out, err := read.Exec(context.Background(), `{"path":".env"}`)
		if tc.refuse != (err != nil) {
			t.Fatalf("mode %q: read .env err=%v out=%q, want refused=%v", tc.mode, err, out, tc.refuse)
		}
		if tc.refuse && !strings.Contains(err.Error(), "secret-material") {
			t.Errorf("mode %q: refusal %q does not say why", tc.mode, err)
		}
		if !tc.refuse && !strings.Contains(out, "TOKEN=abc123") {
			t.Errorf("mode %q: the file came back changed: %q", tc.mode, out)
		}
		if _, err := read.Exec(context.Background(), `{"path":"notes.txt"}`); err != nil {
			t.Errorf("mode %q: an ordinary file was refused: %v", tc.mode, err)
		}
		rows := readRows(t, audit)
		switch {
		case tc.wantRow == "" && len(rows) != 0:
			t.Errorf("mode %q: %d audit rows, want none", tc.mode, len(rows))
		case tc.wantRow != "" && (len(rows) != 1 || rows[0].Decision != tc.wantRow || rows[0].Kind != string(ActRead) || rows[0].Path != ".env"):
			t.Errorf("mode %q: audit rows %+v, want exactly one %q read row for .env", tc.mode, rows, tc.wantRow)
		}
	}
}

// TestSearchWithholdsFloorFilesUnderEnforce: search_files is a read tool too; under
// enforce a secret file is never searched and nothing says it was skipped (a count is
// a probe signal), under warn its lines are shown as before.
func TestSearchWithholdsFloorFilesUnderEnforce(t *testing.T) {
	dir := floorWorktree(t)
	for _, tc := range []struct {
		mode     string
		withheld bool
	}{{"enforce", true}, {"warn", false}} {
		res, _ := buildForFloor(t, dir, tc.mode, "")
		out, err := floorTool(t, res.Tools, "search_files").Exec(context.Background(), `{"pattern":"TOKEN"}`)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "abc123") == tc.withheld {
			t.Errorf("mode %q: secret line shown=%v, want shown=%v:\n%s", tc.mode, strings.Contains(out, "abc123"), !tc.withheld, out)
		}
		if !strings.Contains(out, "notes.txt") {
			t.Errorf("mode %q: the ordinary file's match is missing:\n%s", tc.mode, out)
		}
		if tc.withheld && (strings.Contains(out, "withheld") || strings.Contains(out, ".env")) {
			t.Errorf("mode %q: output reveals the skipped file (a count or a name is a probe signal):\n%s", tc.mode, out)
		}
	}
}

// TestRulesOffSwitchesTheReadFloorOff: --rules off is the documented off switch for
// the whole tighten-only table, the read floor included.
func TestRulesOffSwitchesTheReadFloorOff(t *testing.T) {
	dir := floorWorktree(t)
	res, audit := buildForFloor(t, dir, "enforce", RulesOff)
	if _, err := floorTool(t, res.Tools, "read_file").Exec(context.Background(), `{"path":".env"}`); err != nil {
		t.Errorf("--rules off must switch the read floor off, got %v", err)
	}
	if rows := readRows(t, audit); len(rows) != 0 {
		t.Errorf("--rules off recorded %d read rows, want none", len(rows))
	}
}

// TestRuleTableAcceptsReadRules: an operator table may add read rules (tighten-only);
// one fires under enforce exactly like the built-in floor.
func TestRuleTableAcceptsReadRules(t *testing.T) {
	dir := floorWorktree(t)
	if err := os.WriteFile(filepath.Join(dir, "customers.sqlite"), []byte("rows"), 0o600); err != nil {
		t.Fatal(err)
	}
	table := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(table, []byte(`[{"kind":"read","glob":"*.sqlite","decision":"deny","severity":"high","reason":"customer data is never read by an agent run"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _ := buildForFloor(t, dir, "enforce", table)
	_, err := floorTool(t, res.Tools, "read_file").Exec(context.Background(), `{"path":"customers.sqlite"}`)
	if err == nil || !strings.Contains(err.Error(), "customer data") {
		t.Errorf("a table read rule did not fire under enforce: %v", err)
	}
}
