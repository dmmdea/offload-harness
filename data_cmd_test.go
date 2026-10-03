package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/volumes"
)

// dataSeams stands in for the machine: the OS drive and the mounted volumes.
func dataSeams(t *testing.T, osDrive string, vols []volumes.Volume) {
	t.Helper()
	oldDrive, oldList := doctorOSDrive, doctorVolumes
	doctorOSDrive = func() string { return osDrive }
	doctorVolumes = func() ([]volumes.Volume, error) { return vols, nil }
	t.Cleanup(func() { doctorOSDrive, doctorVolumes = oldDrive, oldList })
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestDataMigratePlansThenCopies is the operator's path end to end: a dry run that
// writes nothing, then an apply that copies, and the line to put in config.json.
func TestDataMigratePlansThenCopies(t *testing.T) {
	dataSeams(t, "", nil)
	from, to := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	writeTree(t, from, map[string]string{"ledger.jsonl": "row\n", "media/a.srt": "srt", "cache.db": "BOLT"})
	cfg := writeCfg(t, `{}`)

	got := captureStdout(t, func() {
		if err := runData([]string{"migrate", "--config", cfg, "--from", from, "--to", to}); err != nil {
			t.Errorf("a dry run must succeed even with a held store: %v", err)
		}
	})
	if !strings.Contains(got, "dry run") || !strings.Contains(got, "held") {
		t.Errorf("the dry run must say it wrote nothing and name the held store:\n%s", got)
	}
	if _, err := os.Stat(to); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a dry run created the destination (%v)", err)
	}

	got = captureStdout(t, func() {
		if err := runData([]string{"migrate", "--config", cfg, "--from", from, "--to", to, "--apply", "--stopped"}); err != nil {
			t.Errorf("apply --stopped: %v", err)
		}
	})
	if !strings.Contains(got, `"home": "`+datahomeValue(to)+`"`) {
		t.Errorf("the output must carry the exact config line to set:\n%s", got)
	}
	for _, rel := range []string{"ledger.jsonl", "media/a.srt", "cache.db"} {
		if _, err := os.Stat(filepath.Join(to, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s was not copied: %v", rel, err)
		}
		if _, err := os.Stat(filepath.Join(from, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s vanished from the source: %v", rel, err)
		}
	}
}

func datahomeValue(p string) string { return strings.ReplaceAll(p, `\`, "/") }

// TestDataMigrateFailsWhileAStoreIsHeld: an apply that could not carry everything must
// not exit zero, or a wrapper would go on to switch home over a half-copied tree.
func TestDataMigrateFailsWhileAStoreIsHeld(t *testing.T) {
	dataSeams(t, "", nil)
	from, to := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	writeTree(t, from, map[string]string{"ledger.jsonl": "row\n", "cache.db": "BOLT"})
	cfg := writeCfg(t, `{}`)
	var err error
	got := captureStdout(t, func() {
		err = runData([]string{"migrate", "--config", cfg, "--from", from, "--to", to, "--apply"})
	})
	if err == nil || !strings.Contains(got, "do not switch home yet") {
		t.Fatalf("a held store must fail the apply and say not to switch: err=%v\n%s", err, got)
	}
}

// TestDataMigrateRefusesTheOSDrive: the verb exists to get data OFF that drive.
func TestDataMigrateRefusesTheOSDrive(t *testing.T) {
	dataSeams(t, `C:\`, nil)
	from := filepath.Join(t.TempDir(), "old")
	writeTree(t, from, map[string]string{"ledger.jsonl": "row\n"})
	cfg := writeCfg(t, `{}`)
	err := runData([]string{"migrate", "--config", cfg, "--from", from, "--to", `C:\local-offload`, "--apply"})
	if err == nil || !strings.Contains(err.Error(), "OS drive") {
		t.Fatalf("a target on the OS drive must be refused, got %v", err)
	}
}

// TestDataMigrateDefaultsToTheRulesVolume: with no --to the target is the install-volume
// rule's pick, never the OS drive, and the plan names it.
func TestDataMigrateDefaultsToTheRulesVolume(t *testing.T) {
	dataSeams(t, `C:\`, doctorWinVolumes())
	from := filepath.Join(t.TempDir(), "old")
	writeTree(t, from, map[string]string{"ledger.jsonl": "row\n"})
	cfg := writeCfg(t, `{}`)
	got := captureStdout(t, func() {
		if err := runData([]string{"migrate", "--config", cfg, "--from", from}); err != nil {
			t.Errorf("dry run: %v", err)
		}
	})
	if !strings.Contains(got, `D:\local-offload`) {
		t.Errorf("the default target must be the roomy data drive:\n%s", got)
	}

	// A volume list that does not flag the OS drive must not make it the default
	// target: the verb knows which drive Windows boots from.
	dataSeams(t, `C:\`, []volumes.Volume{
		{Root: `C:\`, TotalBytes: 2000 * volumes.GiB, FreeBytes: 900 * volumes.GiB},
		{Root: `D:\`, TotalBytes: 500 * volumes.GiB, FreeBytes: 100 * volumes.GiB},
	})
	got = captureStdout(t, func() {
		if err := runData([]string{"migrate", "--config", cfg, "--from", from}); err != nil {
			t.Errorf("dry run: %v", err)
		}
	})
	if !strings.Contains(got, `D:\local-offload`) || strings.Contains(got, `C:\local-offload`) {
		t.Errorf("the OS drive must never be the default target:\n%s", got)
	}
}

// TestDataMigrateDefaultsFromToTheLoadedHome: with no --from the source is the install
// root the loaded config resolves, which is the tree the node actually runs from.
func TestDataMigrateDefaultsFromToTheLoadedHome(t *testing.T) {
	dataSeams(t, "", nil)
	from, to := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	writeTree(t, from, map[string]string{"ledger.jsonl": "row\n"})
	cfg := writeCfg(t, `{"home": "`+datahomeValue(from)+`"}`)
	got := captureStdout(t, func() {
		if err := runData([]string{"migrate", "--config", cfg, "--to", to, "--apply"}); err != nil {
			t.Errorf("apply: %v", err)
		}
	})
	if _, err := os.Stat(filepath.Join(to, "ledger.jsonl")); err != nil {
		t.Fatalf("the loaded home was not the default source (%v):\n%s", err, got)
	}
}

// TestDataMigrateHoistedConfigFlag: `local-offload --config X data migrate` reaches the
// verb with the flag in front of the subcommand.
func TestDataMigrateHoistedConfigFlag(t *testing.T) {
	dataSeams(t, "", nil)
	from, to := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	writeTree(t, from, map[string]string{"ledger.jsonl": "row\n"})
	cfg := writeCfg(t, `{}`)
	got := captureStdout(t, func() {
		if err := runData([]string{"--config", cfg, "migrate", "--from", from, "--to", to}); err != nil {
			t.Errorf("hoisted config: %v", err)
		}
	})
	if !strings.Contains(got, "dry run") {
		t.Errorf("the verb did not run:\n%s", got)
	}
}

// TestDataStatusReportsAndExits: the audit a human reads, with the same verdict doctor
// uses; non-zero exactly when doctor would FAIL.
func TestDataStatusReportsAndExits(t *testing.T) {
	dataSeams(t, `C:\`, doctorWinVolumes())
	t.Setenv("LOCAL_OFFLOAD_HOME", `C:\Users\<user>\.local-offload`)
	onC := writeCfg(t, `{}`)
	var err error
	got := captureStdout(t, func() { err = runDataStatus([]string{"--config", onC}, os.Stdout) })
	if err == nil || !strings.Contains(got, "<== OS drive") || !strings.Contains(got, "local-offload data migrate") {
		t.Fatalf("data on C: with D: free must fail and say how to move:\nerr=%v\n%s", err, got)
	}

	t.Setenv("LOCAL_OFFLOAD_HOME", "")
	onD := writeCfg(t, `{"home": "D:/local-offload"}`)
	got = captureStdout(t, func() { err = runDataStatus([]string{"--config", onD}, os.Stdout) })
	if err != nil || !strings.Contains(got, "no data location is on the OS drive") {
		t.Fatalf("a node on D: is clean:\nerr=%v\n%s", err, got)
	}

	dataSeams(t, "", nil)
	got = captureStdout(t, func() { err = runDataStatus([]string{"--config", onC}, os.Stdout) })
	if err != nil || !strings.Contains(got, "does not apply") {
		t.Fatalf("a host with no OS drive letter has no rule:\nerr=%v\n%s", err, got)
	}
}

func TestDataUnknownSubcommand(t *testing.T) {
	if err := runData([]string{"shred"}); err == nil {
		t.Fatal("an unknown subcommand must be an error")
	}
	if err := runData(nil); err == nil {
		t.Fatal("no subcommand must be an error")
	}
}

// TestDataMigratePrintsAnAbsoluteHome: a relative --to is resolved before it is printed,
// because the `home` line goes into config.json and a relative value there means "under
// whatever directory the node starts in".
func TestDataMigratePrintsAnAbsoluteHome(t *testing.T) {
	dataSeams(t, "", nil)
	base := t.TempDir()
	from := filepath.Join(base, "old")
	writeTree(t, from, map[string]string{"ledger.jsonl": "row\n"})
	cfg := writeCfg(t, `{}`)
	t.Chdir(base)
	got := captureStdout(t, func() {
		if err := runData([]string{"migrate", "--config", cfg, "--from", from, "--to", "newhome", "--json"}); err != nil {
			t.Errorf("dry run: %v", err)
		}
	})
	var out struct {
		Home   string `json:"home"`
		Result struct {
			To string `json:"to"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("the JSON output did not parse: %v\n%s", err, got)
	}
	for name, p := range map[string]string{"home": out.Home, "result.to": out.Result.To} {
		if !filepath.IsAbs(filepath.FromSlash(p)) || !strings.HasSuffix(p, "newhome") {
			t.Errorf("%s = %q, want the absolute path of newhome", name, p)
		}
	}
}

// TestDataMigrateDefaultTargetSkipsACloudDrive: with no --to the target is the data-volume
// rule's pick; a Google Drive virtual drive with the most free space on the box is not it.
func TestDataMigrateDefaultTargetSkipsACloudDrive(t *testing.T) {
	vols := doctorWinVolumes()
	vols = append(vols, volumes.Volume{Root: `E:\`, FS: "FAT32", Label: "Google Drive", TotalBytes: 2000 * volumes.GiB, FreeBytes: 1500 * volumes.GiB})
	dataSeams(t, `C:\`, vols)
	from := filepath.Join(t.TempDir(), "old")
	writeTree(t, from, map[string]string{"ledger.jsonl": "row\n"})
	cfg := writeCfg(t, `{}`)
	got := captureStdout(t, func() {
		if err := runData([]string{"migrate", "--config", cfg, "--from", from}); err != nil {
			t.Errorf("dry run: %v", err)
		}
	})
	if !strings.Contains(got, `D:\local-offload`) || strings.Contains(got, `E:\local-offload`) {
		t.Errorf("the default target must be the local data drive, not the cloud drive:\n%s", got)
	}
	// Only the cloud drive and the OS drive: a loud error that names the drive, never a pick.
	dataSeams(t, `C:\`, []volumes.Volume{vols[0], vols[2]})
	err := runData([]string{"migrate", "--config", cfg, "--from", from})
	if err == nil || !strings.Contains(err.Error(), "Google Drive") {
		t.Errorf("with no local data drive the verb must refuse and name why, got %v", err)
	}
}
