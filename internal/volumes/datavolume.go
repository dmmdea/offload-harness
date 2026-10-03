package volumes

import (
	"fmt"
	"strings"
)

// Pick answers "where should an install go", and for the models and tools that is the
// roomiest disk. Harness DATA is a stricter question: a bbolt store written continuously,
// an append-only ledger and media that grows by gigabytes a day need a real local disk.
// A virtual drive that a sync client mounts is a view of a cloud account (Google Drive for
// desktop reports FAT32, is neither removable nor a network share, and shows a quota's
// worth of free space, so it often outranks every real disk on the box), and a FAT-family
// filesystem has no journal and, for FAT32, a 4 GiB file ceiling. Pick is shared with
// installs that have no such constraint, so the data paths use PickData instead.

// The names a sync client gives its virtual drive. They mirror the GPU lease's refusal of
// a sync root as a state directory (its unexported lists in the gpulease package); a test
// reads that source and fails when a name is added there and not here. Exact names are the
// short common words that must not prefix-match ("Mega Games" is a game disk); prefixes
// are the vendor names, which count when the vendor name ends the label or is followed by
// a qualifier: "OneDrive - Contoso", "Dropbox (Personal)", "pCloud Drive", "iCloudDrive".
var (
	syncLabelExact    = []string{"my drive", "shared drives", "mega", "syncthing"}
	syncLabelPrefixes = []string{
		"onedrive", "dropbox", "icloud", "pcloud", "google drive", "googledrive",
		"nextcloud", "owncloud", "box sync",
	}
)

// syncMarker names the sync client s belongs to, "" when it belongs to none. s is one
// volume label or one segment of a mount path.
func syncMarker(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	for _, m := range syncLabelExact {
		if s == m {
			return m
		}
	}
	for _, m := range syncLabelPrefixes {
		if !strings.HasPrefix(s, m) {
			continue
		}
		if rest := s[len(m):]; rest == "" || rest[0] == ' ' || rest[0] == '(' || rest == "drive" {
			return m
		}
	}
	return ""
}

// isFAT reports whether fs is a FAT-family filesystem, in the spellings Windows and the
// Linux kernel report.
func isFAT(fs string) bool {
	switch strings.ToLower(strings.TrimSpace(fs)) {
	case "fat", "fat12", "fat16", "fat32", "exfat", "vfat", "msdos":
		return true
	}
	return false
}

// DataVolumeReason says why v must not hold the harness's data, "" when it may. The
// answer reads as a noun phrase ("a cloud-synced virtual drive (...)") so a caller can
// write "<root> is <reason>".
func DataVolumeReason(v Volume) string {
	if m := syncMarker(v.Label); m != "" {
		return fmt.Sprintf("a cloud-synced virtual drive (label %q): a view of an account, not a disk a database can live on", v.Label)
	}
	for _, seg := range strings.FieldsFunc(v.Root, func(r rune) bool { return r == '/' || r == '\\' }) {
		if m := syncMarker(seg); m != "" {
			return fmt.Sprintf("a cloud-synced virtual drive (mount path segment %q): a view of an account, not a disk a database can live on", seg)
		}
	}
	if isFAT(v.FS) {
		return fmt.Sprintf("a FAT-family filesystem (%s): no journal for a store written continuously, and FAT32 caps a file at 4 GiB", v.FS)
	}
	return ""
}

// DataCandidates splits vols into the volumes data may live on and one line for each that
// may not ("E:\ is a cloud-synced virtual drive (...)"), in the order given.
func DataCandidates(vols []Volume) (kept []Volume, skipped []string) {
	for _, v := range vols {
		if why := DataVolumeReason(v); why != "" {
			skipped = append(skipped, v.Root+" is "+why)
			continue
		}
		kept = append(kept, v)
	}
	return kept, skipped
}

// PickData is Pick for the volume that will hold harness data: the same policy over the
// volumes DataCandidates keeps. What it passed over is recorded in the choice's reason,
// and when nothing is left the error names the skipped volumes instead of claiming none
// were enumerated. With nothing to skip it is Pick, answer for answer.
func PickData(vols []Volume, opt PickOptions) (Choice, error) {
	kept, skipped := DataCandidates(vols)
	if len(skipped) == 0 {
		return Pick(vols, opt)
	}
	note := strings.Join(skipped, "; ")
	if len(kept) == 0 {
		return Choice{}, fmt.Errorf("no eligible install volume: not usable for data: %s", note)
	}
	choice, err := Pick(kept, opt)
	if err != nil {
		return Choice{}, fmt.Errorf("%w; not usable for data: %s", err, note)
	}
	choice.Because += "; passed over for data: " + note
	return choice, nil
}
