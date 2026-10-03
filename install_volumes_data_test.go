package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/volumes"
)

// installVolumes stands in for the machine's volume list.
func installVolumes(t *testing.T, vols []volumes.Volume) {
	t.Helper()
	old := installVolumesList
	installVolumesList = func() ([]volumes.Volume, error) { return vols, nil }
	t.Cleanup(func() { installVolumesList = old })
}

// cloudBox is a Windows box whose roomiest volume is a Google Drive virtual drive.
func cloudBox() []volumes.Volume {
	return []volumes.Volume{
		{Root: `C:\`, FS: "NTFS", TotalBytes: 500 * volumes.GiB, FreeBytes: 200 * volumes.GiB, IsOS: true},
		{Root: `D:\`, FS: "NTFS", TotalBytes: 1000 * volumes.GiB, FreeBytes: 100 * volumes.GiB},
		{Root: `E:\`, FS: "FAT32", Label: "Google Drive", TotalBytes: 2000 * volumes.GiB, FreeBytes: 900 * volumes.GiB},
	}
}

type volumesOut struct {
	Choice *struct {
		Volume struct {
			Root string `json:"root"`
		} `json:"volume"`
		Because string `json:"because"`
	} `json:"choice"`
	Error      string `json:"error"`
	DataTarget bool   `json:"data_target"`
}

func runVolumesJSON(t *testing.T, args ...string) (volumesOut, error) {
	t.Helper()
	var err error
	got := captureStdout(t, func() { err = runInstallVolumes(append([]string{"--json"}, args...)) })
	var out volumesOut
	if jerr := json.Unmarshal([]byte(got), &out); jerr != nil {
		t.Fatalf("the JSON output did not parse: %v\n%s", jerr, got)
	}
	return out, err
}

// TestInstallVolumesDataSkipsACloudDrive: install.ps1 writes the choice as the node's
// `home`, so the data-volume rule must run in the verb it asks. Without --data the answer
// is the install rule's and does not change (install.sh and the model placement use it).
func TestInstallVolumesDataSkipsACloudDrive(t *testing.T) {
	installVolumes(t, cloudBox())

	plain, err := runVolumesJSON(t)
	if err != nil || plain.Choice == nil || plain.Choice.Volume.Root != `E:\` {
		t.Fatalf("without --data the install rule is unchanged (and prefers the roomiest volume): %+v, %v", plain, err)
	}
	if plain.DataTarget {
		t.Errorf("an answer from the plain install rule must not claim to be a data target")
	}

	got, err := runVolumesJSON(t, "--data")
	if err != nil || got.Choice == nil || got.Choice.Volume.Root != `D:\` {
		t.Fatalf("--data must choose the local data drive, got %+v, %v", got, err)
	}
	if !got.DataTarget {
		t.Errorf("the installer reads data_target to know the answer came from the data-volume rule")
	}
	if !strings.Contains(got.Choice.Because, "Google Drive") {
		t.Errorf("the recorded reason must say what was passed over: %q", got.Choice.Because)
	}
}

// TestInstallVolumesDataWithOnlyACloudDrive: the failure names the drive and why, still
// carries the marker (so a reader knows which rule failed), and exits non-zero.
func TestInstallVolumesDataWithOnlyACloudDrive(t *testing.T) {
	installVolumes(t, []volumes.Volume{cloudBox()[0], cloudBox()[2]})
	got, err := runVolumesJSON(t, "--data")
	if err == nil || got.Choice != nil {
		t.Fatalf("a cloud drive must never be chosen as the data volume, got %+v, %v", got, err)
	}
	if !strings.Contains(got.Error, "Google Drive") || !strings.Contains(got.Error, "cloud") {
		t.Errorf("the error must name the passed-over drive and why: %q", got.Error)
	}
	if !got.DataTarget {
		t.Errorf("the marker belongs to the rule that ran, not to its outcome")
	}
}

// TestInstallVolumesDataTableMarksTheDrivesItWillNotUse: the human view says which volumes
// are not data targets, so the operator is not left to guess why the roomiest one lost.
func TestInstallVolumesDataTableMarksTheDrivesItWillNotUse(t *testing.T) {
	installVolumes(t, cloudBox())
	var err error
	got := captureStdout(t, func() { err = runInstallVolumes([]string{"--data"}) })
	if err != nil {
		t.Fatal(err)
	}
	var eRow string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, `E:\`) {
			eRow = line
		}
	}
	if !strings.Contains(eRow, "not a data target") || !strings.Contains(eRow, "cloud") {
		t.Errorf("the cloud drive's row must say it is not a data target:\n%s", got)
	}
	if !strings.Contains(got, `install target: D:\`) {
		t.Errorf("the target must be D:\n%s", got)
	}
}
