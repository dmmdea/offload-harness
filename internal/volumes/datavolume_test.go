package volumes

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// cloudDrive is the virtual drive a sync client mounts. Google Drive for desktop reports
// FAT32, is neither removable nor a network share, and shows the free space of a cloud
// quota, so on a box with a modest local disk it is the volume Pick likes best.
func cloudDrive(root string, free uint64) Volume {
	return Volume{Root: root, FS: "FAT32", Label: "Google Drive", TotalBytes: 2000 * GiB, FreeBytes: free}
}

// TestDataVolumeReason: what may hold a harness home (a bbolt cache, an append-only
// ledger, media that grows by gigabytes a day). A volume that is a view of a cloud
// account, or a FAT-family filesystem, may not; an ordinary local one may.
func TestDataVolumeReason(t *testing.T) {
	cases := []struct {
		label, fs, root string
		refused         bool
	}{
		{"Google Drive", "FAT32", `E:\`, true},
		{"Google Drive", "NTFS", `E:\`, true}, // the label alone is enough
		{"google drive", "", `E:\`, true},
		{"GoogleDrive", "", `E:\`, true},
		{"My Drive", "", `E:\`, true},
		{"Shared drives", "", `E:\`, true},
		{"OneDrive", "", `E:\`, true},
		{"OneDrive - Contoso", "NTFS", `E:\`, true},
		{"Dropbox (Personal)", "NTFS", `E:\`, true},
		{"Dropbox(Personal)", "NTFS", `E:\`, true}, // a qualifier with no space before it
		{"iCloudDrive", "", `E:\`, true},
		{"pCloud Drive", "", `E:\`, true},
		{"Nextcloud", "", `E:\`, true},
		{"Data", "FAT32", `D:\`, true}, // a FAT volume with an innocent label
		{"Data", "exFAT", `D:\`, true},
		{"Data", "FAT", `D:\`, true},
		{"", "vfat", "/mnt/stick", true},
		{"", "msdos", "/mnt/stick", true},
		{"", "ext4", "/mnt/dropbox", true}, // a mount point named for the vendor
		{"Data", "NTFS", `D:\`, false},
		{"Games", "ReFS", `D:\`, false},
		{"Backup Drive", "NTFS", `D:\`, false}, // "drive" alone is not a vendor
		{"", "ext4", "/srv/data", false},
		{"", "zfs", "/tank", false},
		{"", "", `D:\`, false},
		{"Dropboxes", "NTFS", `D:\`, false},          // the vendor name must end where the label does or qualifies
		{"", "ext4", "/srv/dropbox-exporter", false}, // gpulease's own counter-example
		{"Mega Games", "NTFS", `D:\`, false},         // "mega" is exact-only: too common a word to prefix-match
	}
	for _, c := range cases {
		v := Volume{Root: c.root, FS: c.fs, Label: c.label, TotalBytes: GiB, FreeBytes: GiB}
		got := DataVolumeReason(v)
		if (got != "") != c.refused {
			t.Errorf("DataVolumeReason(label=%q fs=%q root=%q) = %q, refused want %v", c.label, c.fs, c.root, got, c.refused)
		}
	}
}

// TestDataVolumeReasonSaysWhy: the reason is what the operator reads in doctor and in the
// installer's error, so it names the cause rather than just refusing.
func TestDataVolumeReasonSaysWhy(t *testing.T) {
	if got := DataVolumeReason(cloudDrive(`E:\`, GiB)); !strings.Contains(got, "cloud") || !strings.Contains(got, "Google Drive") {
		t.Errorf("a cloud drive's reason must say so and quote the label: %q", got)
	}
	got := DataVolumeReason(Volume{Root: `D:\`, FS: "exFAT"})
	if !strings.Contains(got, "exFAT") || !strings.Contains(got, "FAT") {
		t.Errorf("a FAT-family reason must name the filesystem: %q", got)
	}
}

// TestPickDataSkipsWhatCannotHoldData: the cloud drive reports the most free space on the
// box and still loses to the smaller local disk, and the record says it was passed over.
func TestPickDataSkipsWhatCannotHoldData(t *testing.T) {
	vols := []Volume{
		{Root: `C:\`, FS: "NTFS", TotalBytes: 500 * GiB, FreeBytes: 200 * GiB, IsOS: true},
		{Root: `D:\`, FS: "NTFS", TotalBytes: 1000 * GiB, FreeBytes: 100 * GiB},
		cloudDrive(`E:\`, 900*GiB),
	}
	plain, err := Pick(vols, PickOptions{})
	if err != nil || plain.Volume.Root != `E:\` {
		t.Fatalf("the fixture must make Pick prefer the cloud drive, or this test proves nothing: %+v %v", plain, err)
	}
	got, err := PickData(vols, PickOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Volume.Root != `D:\` {
		t.Fatalf("PickData chose %s, want the local data drive D:", got.Volume.Root)
	}
	if !strings.Contains(got.Because, `E:\`) || !strings.Contains(got.Because, "cloud") {
		t.Errorf("the recorded reason must say the cloud drive was passed over: %q", got.Because)
	}
}

// TestPickDataIsPickWhenNothingIsSkipped: on a box with no cloud or FAT volume the two
// answer identically, so nothing changes for a node the new rule does not concern.
func TestPickDataIsPickWhenNothingIsSkipped(t *testing.T) {
	vols := []Volume{
		{Root: `C:\`, FS: "NTFS", TotalBytes: 500 * GiB, FreeBytes: 200 * GiB, IsOS: true},
		{Root: `D:\`, FS: "NTFS", TotalBytes: 2000 * GiB, FreeBytes: 900 * GiB},
		{Root: `F:\`, FS: "NTFS", TotalBytes: 1000 * GiB, FreeBytes: 100 * GiB},
	}
	want, werr := Pick(vols, PickOptions{})
	got, gerr := PickData(vols, PickOptions{})
	if got != want || (gerr == nil) != (werr == nil) {
		t.Errorf("PickData = %+v, %v; Pick = %+v, %v", got, gerr, want, werr)
	}
	_, perr := Pick(nil, PickOptions{})
	_, derr := PickData(nil, PickOptions{})
	if perr == nil || derr == nil || perr.Error() != derr.Error() {
		t.Errorf("an empty list must fail the same way: %v vs %v", perr, derr)
	}
}

// TestPickDataNamesTheSkippedVolumeWhenNothingElseQualifies: with only a cloud drive left
// the error must not say "no volumes were enumerated"; it names the drive and why.
func TestPickDataNamesTheSkippedVolumeWhenNothingElseQualifies(t *testing.T) {
	vols := []Volume{
		{Root: `C:\`, FS: "NTFS", TotalBytes: 500 * GiB, FreeBytes: 10 * GiB, IsOS: true},
		cloudDrive(`E:\`, 900*GiB),
	}
	_, err := PickData(vols, PickOptions{})
	if err == nil {
		t.Fatal("a cloud drive must never be the data volume")
	}
	for _, want := range []string{"no eligible", `E:\`, "Google Drive", "cloud"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must contain %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "no volumes were enumerated") {
		t.Errorf("a list with a skipped volume was enumerated: %v", err)
	}
	// The cloud drive is the ONLY volume: the list was enumerated, so say what was skipped.
	_, err = PickData([]Volume{cloudDrive(`E:\`, 900*GiB)}, PickOptions{})
	if err == nil || !strings.Contains(err.Error(), "Google Drive") || strings.Contains(err.Error(), "no volumes were enumerated") {
		t.Errorf("a list holding only a cloud drive must fail naming it: %v", err)
	}
	// Some other volume qualifies for nothing (too small): both reasons survive.
	vols = append(vols, Volume{Root: `D:\`, FS: "NTFS", TotalBytes: 50 * GiB, FreeBytes: 5 * GiB})
	_, err = PickData(vols, PickOptions{})
	if err == nil || !strings.Contains(err.Error(), "D:") || !strings.Contains(err.Error(), "Google Drive") {
		t.Errorf("the error must carry Pick's own rejection and the skipped drive: %v", err)
	}
}

// TestPickDataStillHonoursAnExplicitOSVolume: the operator's explicit decision to keep
// data on the OS volume is untouched by the cloud filter.
func TestPickDataStillHonoursAnExplicitOSVolume(t *testing.T) {
	vols := []Volume{
		{Root: `C:\`, FS: "NTFS", TotalBytes: 500 * GiB, FreeBytes: 200 * GiB, IsOS: true},
		cloudDrive(`E:\`, 900*GiB),
	}
	got, err := PickData(vols, PickOptions{AllowOSVolume: true})
	if err != nil || got.Volume.Root != `C:\` {
		t.Fatalf("PickData with the OS volume allowed = %+v, %v", got, err)
	}
}

// TestSyncLabelsMirrorGpulease: the sync-client names are the same set the GPU lease
// refuses as a state root. That list is unexported in a package this one does not import,
// so it is read from source here; a name added there must be refused here too, and a
// move of the list fails this test instead of letting the two drift apart.
func TestSyncLabelsMirrorGpulease(t *testing.T) {
	src, err := os.ReadFile("../gpulease/gpulease.go")
	if err != nil {
		t.Skipf("gpulease source not readable from here: %v", err)
	}
	list := func(name string) []string {
		m := regexp.MustCompile(`(?s)var ` + name + ` = \[\]string\{(.*?)\}`).FindSubmatch(src)
		if m == nil {
			t.Fatalf("gpulease no longer declares %s: point this test at the sync-root list's new home", name)
		}
		var out []string
		for _, q := range regexp.MustCompile(`"([^"]+)"`).FindAllString(string(m[1]), -1) {
			s, uerr := strconv.Unquote(q)
			if uerr != nil {
				t.Fatalf("%s: %v", q, uerr)
			}
			out = append(out, s)
		}
		if len(out) == 0 {
			t.Fatalf("%s parsed empty", name)
		}
		return out
	}
	for _, name := range list("syncRootExact") {
		if DataVolumeReason(Volume{Root: `E:\`, Label: name}) == "" {
			t.Errorf("gpulease refuses %q as a sync root but a volume labelled that is accepted for data", name)
		}
	}
	for _, name := range list("syncRootPrefixes") {
		for _, label := range []string{name, name + " (Personal)", name + " - Contoso"} {
			if DataVolumeReason(Volume{Root: `E:\`, Label: label}) == "" {
				t.Errorf("gpulease refuses %q as a sync root but a volume labelled %q is accepted for data", name, label)
			}
		}
	}
}
