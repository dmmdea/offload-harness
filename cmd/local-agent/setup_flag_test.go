package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

func TestResolveSetupActionsFlag(t *testing.T) {
	got, err := resolveSetupActions("")
	if err != nil || got != nil {
		t.Fatalf("empty flag must mean no replay: %+v %v", got, err)
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "setup.json")
	if err := os.WriteFile(good, []byte(`[{"tool":"read_file","args":{"path":"notes.md"}},{"tool":"list_dir"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = resolveSetupActions(good)
	if err != nil || len(got) != 2 || got[0].Tool != "read_file" || got[0].ArgsJSON() != `{"path":"notes.md"}` || got[1].ArgsJSON() != "{}" {
		t.Fatalf("file must load in order: %+v %v", got, err)
	}

	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte(`[{"tool":"read file"}]`), 0o644)
	_, err = resolveSetupActions(bad)
	if err == nil || !errors.Is(err, core.ErrAgentSetupActions) || !strings.Contains(err.Error(), "bad.json") {
		t.Fatalf("invalid file must fail by class and name: %v", err)
	}
	typo := filepath.Join(dir, "typo.json")
	_ = os.WriteFile(typo, []byte(`[{"tool":"read_file","arg":{"path":"x"}}]`), 0o644)
	if _, err = resolveSetupActions(typo); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("an unknown key must fail (strict decoder), got %v", err)
	}
	if _, err = resolveSetupActions(filepath.Join(dir, "missing.json")); err == nil || !strings.Contains(err.Error(), "--setup") {
		t.Fatalf("a missing file must fail naming the flag: %v", err)
	}
}
