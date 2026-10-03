package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/datahome"
	"github.com/dmmdea/offload-harness/internal/volumes"
)

// doctorWinVolumes is a two-drive Windows box: the OS drive and a roomy data drive.
func doctorWinVolumes() []volumes.Volume {
	return []volumes.Volume{
		{Root: `C:\`, FS: "NTFS", TotalBytes: 500 * volumes.GiB, FreeBytes: 200 * volumes.GiB, IsOS: true},
		{Root: `D:\`, FS: "NTFS", TotalBytes: 2000 * volumes.GiB, FreeBytes: 900 * volumes.GiB},
	}
}

// nodeAt is the built-in default config with every derived path rooted at base, so a
// test says the same thing on a box whose real user profile is on C:, on D: or on a
// Linux CI runner. LOCAL_OFFLOAD_HOME is how the harness itself relocates DefaultBase.
func nodeAt(t *testing.T, base, endpoint string) config.Config {
	t.Helper()
	t.Setenv("LOCAL_OFFLOAD_HOME", base)
	cfg := config.Default()
	cfg.Endpoint = endpoint
	cfg.Home = base
	return cfg
}

// dataOnC is a node whose harness data sits under the user profile on the OS drive:
// the shape of every Windows node installed before the data-drive rule (register C-92).
func dataOnC(t *testing.T, endpoint string) config.Config {
	return nodeAt(t, `C:\Users\<user>\.local-offload`, endpoint)
}

// TestDoctorFailsWhenDataSitsOnTheOSDrive: doctor was silent about where the harness
// keeps its data. The operator rule is that C: holds Windows and program installs,
// never data, so a node with data there and a data drive free to take it must exit
// non-zero, name every offending key, and say how to move it.
func TestDoctorFailsWhenDataSitsOnTheOSDrive(t *testing.T) {
	srv := fakeSwap(t, defaultAliasIDs())
	cfg := dataOnC(t, srv.URL)
	rep := datahome.Audit(cfg, `C:\`, doctorWinVolumes())

	var out strings.Builder
	err := doctorRunChecked(cfg, nil, &out, &rep)
	got := out.String()
	if err == nil {
		t.Fatalf("data on the OS drive with a data drive free must fail doctor:\n%s", got)
	}
	if !strings.Contains(err.Error(), "OS drive") {
		t.Errorf("the exit error must say why, got %v", err)
	}
	for _, want := range []string{
		"FAIL", "home", "media_dir", "cache_path", // every key that sits on C:
		`D:\`, "900.0 GiB", // where the install-volume rule would put it
		"local-offload data migrate", `"home": "D:/local-offload"`, // the way there
		"stop fleet-serve", // the doors hold bbolt stores open
	} {
		if !strings.Contains(got, want) {
			t.Errorf("doctor output must name %q:\n%s", want, got)
		}
	}
	// Everything here hangs off one install root, so the root is the one FAIL row and
	// the paths under it are folded into a single line, not eighteen rows to scroll.
	if !strings.Contains(got, "FAIL  home") || !strings.Contains(got, "paths under it that follow it") || strings.Contains(got, "FAIL  media_dir") {
		t.Errorf("paths under the install root must fold into the root's row:\n%s", got)
	}
}

// TestDoctorListsAKeyOutsideTheRootOnItsOwnRow: moving `home` does not move a path
// written somewhere else on the OS drive, so it keeps a row of its own.
func TestDoctorListsAKeyOutsideTheRootOnItsOwnRow(t *testing.T) {
	srv := fakeSwap(t, defaultAliasIDs())
	cfg := nodeAt(t, `D:\local-offload`, srv.URL)
	cfg.MediaDir = `C:\scratch\media`
	rep := datahome.Audit(cfg, `C:\`, doctorWinVolumes())
	var out strings.Builder
	if err := doctorRunChecked(cfg, nil, &out, &rep); err == nil {
		t.Fatalf("a stray media_dir on C: must fail doctor:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "FAIL  media_dir") || strings.Contains(out.String(), "FAIL  home") {
		t.Errorf("only media_dir sits on C:, and it needs its own row:\n%s", out.String())
	}
}

// TestDoctorDataSectionIsSilentWhenClean: a node on a data drive prints nothing about
// it, so a green doctor stays as short as it was.
func TestDoctorDataSectionIsSilentWhenClean(t *testing.T) {
	srv := fakeSwap(t, defaultAliasIDs())
	cfg := nodeAt(t, `D:\local-offload`, srv.URL)
	rep := datahome.Audit(cfg, `C:\`, doctorWinVolumes())
	var out strings.Builder
	if err := doctorRunChecked(cfg, nil, &out, &rep); err != nil {
		t.Fatalf("a node whose home is on D: must pass doctor: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "data volume") {
		t.Errorf("a clean node must not print the section:\n%s", out.String())
	}
}

// TestDoctorDataSectionNotesAOneDiskBox: nothing else qualifies, so there is nothing
// to do about it. It is said once, and it does not fail the verb.
func TestDoctorDataSectionNotesAOneDiskBox(t *testing.T) {
	srv := fakeSwap(t, defaultAliasIDs())
	cfg := dataOnC(t, srv.URL)
	rep := datahome.Audit(cfg, `C:\`, doctorWinVolumes()[:1])
	var out strings.Builder
	if err := doctorRunChecked(cfg, nil, &out, &rep); err != nil {
		t.Fatalf("a one-disk box has nowhere to move to and must not fail: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "no other volume qualifies") || strings.Contains(out.String(), "FAIL") {
		t.Errorf("expected a note and no FAIL row:\n%s", out.String())
	}
}

// TestDoctorDataSectionPrintsBeforeTheHealthProbe: the verdict is pure config plus a
// volume list, so a serving layer that is down must not hide it.
func TestDoctorDataSectionPrintsBeforeTheHealthProbe(t *testing.T) {
	cfg := dataOnC(t, "http://127.0.0.1:1")
	rep := datahome.Audit(cfg, `C:\`, doctorWinVolumes())
	var out strings.Builder
	if err := doctorRunChecked(cfg, nil, &out, &rep); err == nil {
		t.Fatalf("an unreachable endpoint must still fail doctor")
	}
	if !strings.Contains(out.String(), "media_dir") || !strings.Contains(out.String(), "health:     DOWN") {
		t.Errorf("both the data rows and the health verdict belong in the output:\n%s", out.String())
	}
}

// TestRunDoctorAuditsTheDataVolumes pins the wiring: the verb an operator actually
// runs builds the report from THIS machine's volumes and OS drive. The seams stand in
// for the machine, so the test says the same thing on every platform.
func TestRunDoctorAuditsTheDataVolumes(t *testing.T) {
	srv := fakeSwap(t, defaultAliasIDs())
	oldDrive, oldList := doctorOSDrive, doctorVolumes
	doctorOSDrive = func() string { return `C:\` }
	doctorVolumes = func() ([]volumes.Volume, error) { return doctorWinVolumes(), nil }
	t.Cleanup(func() { doctorOSDrive, doctorVolumes = oldDrive, oldList })

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	body := `{"endpoint": "` + srv.URL + `", "home": "C:/node-data"}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := captureStdout(t, func() { _ = runDoctor([]string{"--config", cfgPath}) })
	if !strings.Contains(got, "data volume") || !strings.Contains(got, "C:/node-data") {
		t.Errorf("runDoctor must audit the data volumes and name the offending home:\n%s", got)
	}

	doctorOSDrive = func() string { return "" }
	got = captureStdout(t, func() { _ = runDoctor([]string{"--config", cfgPath}) })
	if strings.Contains(got, "data volume") {
		t.Errorf("a host with no OS drive letter has no rule to audit:\n%s", got)
	}
}

// TestDoctorDoesNotScanDrivesForACleanNode: enumerating volumes can stall on a dead
// mapped share, so a node with nothing on the OS drive must not pay for it.
func TestDoctorDoesNotScanDrivesForACleanNode(t *testing.T) {
	scans := 0
	oldDrive, oldList := doctorOSDrive, doctorVolumes
	doctorOSDrive = func() string { return `C:\` }
	doctorVolumes = func() ([]volumes.Volume, error) { scans++; return doctorWinVolumes(), nil }
	t.Cleanup(func() { doctorOSDrive, doctorVolumes = oldDrive, oldList })

	if rep := liveDataReport(nodeAt(t, `D:\local-offload`, "http://127.0.0.1:1")); rep == nil || len(rep.OnOS) != 0 || scans != 0 {
		t.Fatalf("a clean node: report %+v, drive scans %d (want none)", rep, scans)
	}
	if rep := liveDataReport(dataOnC(t, "http://127.0.0.1:1")); rep == nil || !rep.Failing() || scans != 1 {
		t.Fatalf("a node on C: needs one scan to name the target: report %+v, scans %d", rep, scans)
	}
}
