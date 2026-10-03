package datahome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/volumes"
)

const gib = volumes.GiB

// winVolumes is a two-drive Windows box: the OS drive and a roomy data drive.
func winVolumes() []volumes.Volume {
	return []volumes.Volume{
		{Root: `C:\`, FS: "NTFS", TotalBytes: 500 * gib, FreeBytes: 200 * gib, IsOS: true},
		{Root: `D:\`, FS: "NTFS", TotalBytes: 2000 * gib, FreeBytes: 900 * gib},
	}
}

// loaded is a config the way a node gets one: written to a file and read back through
// config.Load, under a user profile on the OS drive. It is the only way to see what a
// node REALLY resolves, because `home` rebases the derived paths at load time.
func loaded(t *testing.T, json string) config.Config {
	t.Helper()
	profile := `C:\Users\user`
	t.Setenv("USERPROFILE", profile)
	t.Setenv("HOME", profile)
	t.Setenv("LOCAL_OFFLOAD_HOME", "")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func keys(ls []Location) string {
	var ks []string
	for _, l := range ls {
		ks = append(ks, l.Key)
	}
	return strings.Join(ks, ",")
}

// TestAuditFlagsAnUnconfiguredWindowsNode is the defect itself: a node with no `home`
// keeps media, svg, the cache, the ledger, the delegation log and the footprints under
// the user profile, on the OS drive, while a 900 GiB data drive sits idle.
func TestAuditFlagsAnUnconfiguredWindowsNode(t *testing.T) {
	cfg := loaded(t, `{}`)
	r := Audit(cfg, `C:\`, winVolumes())
	if !r.Failing() {
		t.Fatalf("a node whose data sits on C: while D: qualifies must FAIL; report %+v", r)
	}
	got := keys(r.OnOS)
	for _, want := range []string{"home", "media_dir", "svg_dir", "cache_path", "ledger_path"} {
		if !strings.Contains(got, want) {
			t.Errorf("the report must name %s, got %s", want, got)
		}
	}
	if r.Target == nil || r.Target.Volume.Root != `D:\` {
		t.Fatalf("the rule must point at the roomy data drive, got %+v", r.Target)
	}
}

// TestAuditAcceptsAHomeOnADataDrive: one `home` key moves every derived path, so a
// node that sets it is clean without listing a dozen paths.
func TestAuditAcceptsAHomeOnADataDrive(t *testing.T) {
	cfg := loaded(t, `{"home": "D:/local-offload"}`)
	r := Audit(cfg, `C:\`, winVolumes())
	if r.Failing() || len(r.OnOS) != 0 {
		t.Fatalf("home on D: moves every derived path off C:, got %s", keys(r.OnOS))
	}
}

// TestAuditKeepsAnExplicitHomeOnTheOSDrive: an operator who wrote `home` on C: keeps
// it (nothing here rewrites a config); the audit still names it, because the rule
// is about where the data is, not who put it there.
func TestAuditKeepsAnExplicitHomeOnTheOSDrive(t *testing.T) {
	cfg := loaded(t, `{"home": "C:/stuff/offload"}`)
	if !strings.EqualFold(filepath.ToSlash(cfg.BaseDir()), "C:/stuff/offload") {
		t.Fatalf("the explicit home must survive the load untouched, BaseDir=%q", cfg.BaseDir())
	}
	r := Audit(cfg, `C:\`, winVolumes())
	if !r.Failing() || !strings.Contains(keys(r.OnOS), "home") {
		t.Fatalf("an explicit home on the OS drive is still data on the OS drive: %+v", r)
	}
}

// TestAuditNamesAnExplicitKeyLeftBehind: `home` rebases only values still at their
// default, so one hand-written path survives on C: and must be named on its own.
func TestAuditNamesAnExplicitKeyLeftBehind(t *testing.T) {
	cfg := loaded(t, `{"home": "D:/local-offload", "media_dir": "C:/media"}`)
	r := Audit(cfg, `C:\`, winVolumes())
	if !r.Failing() || keys(r.OnOS) != "media_dir" {
		t.Fatalf("only media_dir was left on C:, got %q", keys(r.OnOS))
	}
}

// TestAuditStillSeesTheDerivedRootWhenOnlySomeKeysMoved is the shape of a node that
// hand-wrote media_dir and svg_dir onto a data drive but never set `home`: the
// delegation log, pipeline jobs and footprints still hang off the default root on C:,
// which is exactly what that node must hear about.
func TestAuditStillSeesTheDerivedRootWhenOnlySomeKeysMoved(t *testing.T) {
	cfg := loaded(t, `{"media_dir": "D:/media", "svg_dir": "D:/svg"}`)
	r := Audit(cfg, `C:\`, winVolumes())
	if !r.Failing() || !strings.Contains(keys(r.OnOS), "home") {
		t.Fatalf("delegation-log/pipeline-jobs/footprints still follow the C: root, got %q", keys(r.OnOS))
	}
	if strings.Contains(keys(r.OnOS), "media_dir") {
		t.Errorf("media_dir is on D: and must not be named, got %q", keys(r.OnOS))
	}
}

// TestAuditDoesNotFailWhenNothingElseQualifies: a one-disk laptop has nowhere to move
// to. Failing it would be an alarm nobody can silence, so it is reported, not failed.
func TestAuditDoesNotFailWhenNothingElseQualifies(t *testing.T) {
	cfg := loaded(t, `{}`)
	only := []volumes.Volume{{Root: `C:\`, TotalBytes: 500 * gib, FreeBytes: 200 * gib, IsOS: true}}
	r := Audit(cfg, `C:\`, only)
	if r.Failing() {
		t.Fatalf("no data volume exists: this is a note, not a FAIL")
	}
	if len(r.OnOS) == 0 || r.Target != nil || r.Why == "" {
		t.Fatalf("the data is still on C:, and the report must say nothing else qualifies: %+v", r)
	}
}

// TestAuditAppliesToWindowsOnly: no OS drive, no rule.
func TestAuditAppliesToWindowsOnly(t *testing.T) {
	cfg := loaded(t, `{}`)
	r := Audit(cfg, "", winVolumes())
	if r.Failing() || len(r.OnOS) != 0 || r.OSDrive != "" {
		t.Fatalf("a host with no OS drive letter has no rule to break: %+v", r)
	}
}

// TestStateDirIsNotData: the GPU lease root is machine-wide by design and tiny; it
// must not turn every node's doctor red.
func TestStateDirIsNotData(t *testing.T) {
	cfg := loaded(t, `{"home": "D:/local-offload", "state_dir": "C:/ProgramData/local-offload", "gpu_lock_path": "C:/ProgramData/local-offload/gpu"}`)
	r := Audit(cfg, `C:\`, winVolumes())
	if r.Failing() {
		t.Fatalf("state_dir and gpu_lock_path are the machine-wide lease root, got %s", keys(r.OnOS))
	}
}

func TestProposedHomeAndValue(t *testing.T) {
	if got := ProposedHome(`D:\`); got != `D:\local-offload` {
		t.Errorf("ProposedHome(D:\\) = %q", got)
	}
	if got := ProposedHome(`/srv/data/`); got != `/srv/data/local-offload` {
		t.Errorf("ProposedHome(/srv/data/) = %q", got)
	}
	if got := HomeValue(`D:\local-offload`); got != `D:/local-offload` {
		t.Errorf("HomeValue = %q, want forward slashes so the JSON needs no escaping", got)
	}
}

// TestGroupedCollapsesWhatFollowsHome: on the defaults every path hangs off the
// install root, so one row names the problem; a key written somewhere else on the OS
// drive stays on its own row because moving `home` does not move it.
func TestGroupedCollapsesWhatFollowsHome(t *testing.T) {
	r := Audit(loaded(t, `{}`), `C:\`, winVolumes())
	root, under, others := r.Grouped()
	if root == nil || root.Key != "home" || len(others) != 0 || len(under) < 10 {
		t.Fatalf("the defaults are one root with its paths under it: root=%v under=%d others=%v", root, len(under), others)
	}

	r = Audit(loaded(t, `{"media_dir": "C:/elsewhere/media"}`), `C:\`, winVolumes())
	root, under, others = r.Grouped()
	if root == nil || len(others) != 1 || others[0].Key != "media_dir" {
		t.Fatalf("a path outside the root needs its own row: root=%v others=%v", root, others)
	}
	for _, l := range under {
		if l.Key == "media_dir" {
			t.Fatalf("media_dir is not under home and must not be folded into it")
		}
	}

	r = Audit(loaded(t, `{"home": "D:/local-offload", "media_dir": "C:/media"}`), `C:\`, winVolumes())
	root, under, others = r.Grouped()
	if root != nil || len(under) != 0 || len(others) != 1 {
		t.Fatalf("with home on D: only the stray key remains: root=%v under=%v others=%v", root, under, others)
	}
}

// TestAuditNeverTargetsTheOSDriveEvenIfTheListDoesNotMarkIt: the volume list is an
// enumeration, and a stale or hand-built one may not flag the OS drive. The audit
// itself knows which drive that is, and it is never the answer.
func TestAuditNeverTargetsTheOSDriveEvenIfTheListDoesNotMarkIt(t *testing.T) {
	vols := []volumes.Volume{
		{Root: `C:\`, TotalBytes: 2000 * gib, FreeBytes: 900 * gib}, // roomiest, but it is the OS drive
		{Root: `D:\`, TotalBytes: 500 * gib, FreeBytes: 100 * gib},
	}
	r := Audit(loaded(t, `{}`), `C:\`, vols)
	if r.Target == nil || r.Target.Volume.Root != `D:\` {
		t.Fatalf("the target must be D:, never the OS drive, got %+v", r.Target)
	}
}

// cloudDrive is the virtual drive a sync client mounts: FAT32, neither removable nor a
// network share, and a cloud quota's worth of free space, so it outranks every real disk.
func cloudDrive(root string, free uint64) volumes.Volume {
	return volumes.Volume{Root: root, FS: "FAT32", Label: "Google Drive", TotalBytes: 2000 * gib, FreeBytes: free}
}

// TestAuditNeverTargetsACloudSyncedVolume: the FAIL tells the operator to copy a bbolt
// store onto the volume it names. The install-volume rule alone would name the cloud
// drive whenever it reports the most free space; a node's cache must not live there.
func TestAuditNeverTargetsACloudSyncedVolume(t *testing.T) {
	vols := []volumes.Volume{
		{Root: `C:\`, FS: "NTFS", TotalBytes: 500 * gib, FreeBytes: 200 * gib, IsOS: true},
		{Root: `D:\`, FS: "NTFS", TotalBytes: 1000 * gib, FreeBytes: 100 * gib},
		cloudDrive(`E:\`, 900*gib),
	}
	if plain, err := volumes.Pick(vols, volumes.PickOptions{}); err != nil || plain.Volume.Root != `E:\` {
		t.Fatalf("the fixture must make the plain rule prefer the cloud drive: %+v %v", plain, err)
	}
	r := Audit(loaded(t, `{}`), `C:\`, vols)
	if r.Target == nil || r.Target.Volume.Root != `D:\` {
		t.Fatalf("the target must be the local data drive D:, got %+v", r.Target)
	}
	if !r.Failing() {
		t.Errorf("data on C: with D: free is still a FAIL")
	}
}

// TestAuditSaysWhyWhenOnlyACloudDriveRemains: no real data volume is not a FAIL (there is
// nowhere to move to), and the note must name the drive that was passed over.
func TestAuditSaysWhyWhenOnlyACloudDriveRemains(t *testing.T) {
	vols := []volumes.Volume{
		{Root: `C:\`, FS: "NTFS", TotalBytes: 500 * gib, FreeBytes: 200 * gib, IsOS: true},
		cloudDrive(`E:\`, 900*gib),
	}
	r := Audit(loaded(t, `{}`), `C:\`, vols)
	if r.Target != nil || r.Failing() {
		t.Fatalf("a cloud drive is no target and no FAIL, got %+v", r)
	}
	if !strings.Contains(r.Why, "Google Drive") || !strings.Contains(r.Why, `E:\`) {
		t.Errorf("Why must name the passed-over drive: %q", r.Why)
	}
}

// TestDataTargetIsTheOneFilterForEveryCaller: doctor and `data migrate` both ask here, so
// the OS drive, a cloud drive and a FAT volume are skipped in one place.
func TestDataTargetIsTheOneFilterForEveryCaller(t *testing.T) {
	vols := []volumes.Volume{
		{Root: `C:\`, TotalBytes: 2000 * gib, FreeBytes: 1500 * gib}, // not flagged IsOS, still the OS drive
		cloudDrive(`E:\`, 900*gib),
		{Root: `F:\`, FS: "exFAT", TotalBytes: 2000 * gib, FreeBytes: 800 * gib},
		{Root: `D:\`, FS: "NTFS", TotalBytes: 500 * gib, FreeBytes: 100 * gib},
	}
	got, err := DataTarget(vols, `C:\`)
	if err != nil || got.Volume.Root != `D:\` {
		t.Fatalf("DataTarget = %+v, %v; want D:", got, err)
	}
	if _, err := DataTarget(vols[:3], `C:\`); err == nil {
		t.Errorf("with only the OS drive, a cloud drive and an exFAT stick there is no data target")
	}
}
