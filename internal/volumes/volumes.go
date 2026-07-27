// Package volumes answers one question the installer has never asked: which disk
// should this machine's harness, models and media live on?
//
// It has always defaulted to $HOME (the OS drive), which is how a laptop ends up
// with a multi-GB model tree on the same volume as Windows, and how a services box
// ends up with its root filling while a 250 GB pool sits idle beside it. The rule
// is boring and should have been there from the start: pick the volume with the
// most free space that clears the minimum, prefer anything over the OS volume, and
// RECORD the choice with its reason so a later operator can see why the tree is
// where it is.
//
// Selection (Pick) is pure and unit-tested; only enumeration (List) is
// platform-specific, so the policy is identical on every OS by construction.
package volumes

import (
	"fmt"
	"sort"
	"strings"
)

// GiB is the unit every operator-facing number here speaks.
const GiB = uint64(1) << 30

// Volume is one mounted filesystem the harness could be installed on.
type Volume struct {
	// Root is the mount point: a drive root on Windows, a mount path on Unix.
	Root string `json:"root"`
	// FS is the filesystem type, best effort ("NTFS", "ext4", "zfs").
	FS string `json:"fs,omitempty"`
	// Label is a human name when the platform supplies one.
	Label      string `json:"label,omitempty"`
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
	// IsOS marks the volume the running OS boots from — usable, but the last
	// resort: filling it takes the machine down, not just the harness.
	IsOS bool `json:"is_os"`
	// Removable and Network volumes are never selected: an install that vanishes
	// with a USB stick or a dropped share is worse than no install.
	Removable bool `json:"removable,omitempty"`
	Network   bool `json:"network,omitempty"`
}

// Choice is a selected volume and the reason it won, which the installer writes
// into installed.json so the decision survives the session that made it.
type Choice struct {
	Volume  Volume `json:"volume"`
	Because string `json:"because"`
}

// PickOptions tunes selection. The zero value means "20 GiB minimum, never the OS
// volume" — the policy, not an accident of defaults.
type PickOptions struct {
	// MinFreeBytes is the floor a volume must clear. 0 => DefaultMinFree.
	MinFreeBytes uint64
	// AllowOSVolume lets the OS volume be selected when nothing else qualifies.
	// It is an explicit operator decision, never a silent fallback.
	AllowOSVolume bool
}

// DefaultMinFree is the floor for a usable install: the harness itself is small,
// but a tier's model set is not — the measured ampere-6 media set alone is >12 GiB
// before any video weights.
const DefaultMinFree = 20 * GiB

// Pick applies the policy to an enumerated list. It is pure: same volumes, same
// choice, on every platform.
func Pick(vols []Volume, opt PickOptions) (Choice, error) {
	min := opt.MinFreeBytes
	if min == 0 {
		min = DefaultMinFree
	}

	var eligible, osOnly []Volume
	var rejected []string
	for _, v := range vols {
		switch {
		case v.Removable:
			rejected = append(rejected, v.Root+" is removable")
		case v.Network:
			rejected = append(rejected, v.Root+" is a network share")
		case v.TotalBytes == 0:
			rejected = append(rejected, v.Root+" reports no capacity")
		case v.FreeBytes < min:
			rejected = append(rejected, fmt.Sprintf("%s has %s free (need %s)", v.Root, human(v.FreeBytes), human(min)))
		case v.IsOS:
			osOnly = append(osOnly, v)
		default:
			eligible = append(eligible, v)
		}
	}

	// Most free space wins. Ties break on DEPTH first, then lexicographically:
	// every dataset of a ZFS pool reports the same pool free space, so a
	// name-only tie-break would install the harness under whatever came first
	// alphabetically (`apps/adventurelog` on the measured box) instead of the
	// pool root a human would choose. Depth is also stable across machines.
	byFree := func(s []Volume) {
		sort.SliceStable(s, func(i, j int) bool {
			if s[i].FreeBytes != s[j].FreeBytes {
				return s[i].FreeBytes > s[j].FreeBytes
			}
			di, dj := depth(s[i].Root), depth(s[j].Root)
			if di != dj {
				return di < dj
			}
			return s[i].Root < s[j].Root
		})
	}
	byFree(eligible)
	byFree(osOnly)

	if len(eligible) > 0 {
		v := eligible[0]
		because := fmt.Sprintf("most free space of the non-OS volumes (%s free of %s)", human(v.FreeBytes), human(v.TotalBytes))
		if len(eligible) > 1 {
			because += fmt.Sprintf("; next best was %s with %s", eligible[1].Root, human(eligible[1].FreeBytes))
		}
		return Choice{Volume: v, Because: because}, nil
	}

	if len(osOnly) > 0 && opt.AllowOSVolume {
		v := osOnly[0]
		return Choice{Volume: v, Because: fmt.Sprintf(
			"OS volume, selected only because it was explicitly allowed and no other volume qualified (%s free of %s)",
			human(v.FreeBytes), human(v.TotalBytes))}, nil
	}

	// Failure must say what to do next, not just that nothing matched.
	var why strings.Builder
	why.WriteString("no eligible install volume")
	if len(osOnly) > 0 {
		fmt.Fprintf(&why, ": only the OS volume %s qualifies (%s free) — pass the allow-os-volume option to use it anyway",
			osOnly[0].Root, human(osOnly[0].FreeBytes))
	} else if len(rejected) > 0 {
		fmt.Fprintf(&why, ": %s", strings.Join(rejected, "; "))
	} else {
		why.WriteString(": no volumes were enumerated")
	}
	return Choice{}, fmt.Errorf("%s", why.String())
}

// depth counts path segments so a pool root outranks its datasets on a tie.
func depth(root string) int {
	trimmed := strings.Trim(strings.ReplaceAll(root, `\`, "/"), "/")
	if trimmed == "" {
		return 0
	}
	return strings.Count(trimmed, "/") + 1
}

// human renders bytes the way an operator reads them.
func human(b uint64) string {
	switch {
	case b >= GiB:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(GiB))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(1<<20))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// Human is human for callers rendering the same numbers (CLI tables, reports).
func Human(b uint64) string { return human(b) }
