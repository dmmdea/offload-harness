// Package datahome answers the question the operator rule "C: holds Windows and
// program installs, never data" leaves open on a Windows node: where does this
// harness keep its data, is any of it on the OS drive, and how does it move?
//
// The harness resolves one install root (config `home`, else $LOCAL_OFFLOAD_HOME,
// else ~/.local-offload) and hangs the cache, ledger, media, delegation log, pipeline
// jobs and footprints off it. With no `home` that root is on the OS drive. The
// decision to move it is deliberately NOT made here at load time: a runtime default
// that picked a drive would re-point every existing node at an empty tree the moment
// the binary was upgraded, which is moving live data silently. The installer records
// the choice as an explicit `home`; this package audits that nobody left data behind
// and plans the copy that gets an existing node there.
package datahome

import (
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/volumes"
)

// Location is one place the harness writes data: the config key that decides it and
// the resolved path.
type Location struct {
	Key  string `json:"key"`
	Path string `json:"path"`
}

// Report is the audit of one config against one volume list.
type Report struct {
	// OSDrive is the drive Windows boots from (`C:\`); "" when the rule does not
	// apply (a non-Windows host), which makes every other field empty.
	OSDrive string `json:"os_drive,omitempty"`
	// OnOS lists every data location that sits on the OS drive.
	OnOS []Location `json:"on_os_drive,omitempty"`
	// Target is the volume the install-volume rule would put the data on; nil when no
	// non-OS volume qualifies.
	Target *volumes.Choice `json:"target,omitempty"`
	// Why says why Target is nil.
	Why string `json:"why,omitempty"`
}

// Locations lists the data locations cfg resolves, one row per distinct path, `home`
// first. `home` is the install root: the delegation log, the pipeline job trees, the
// footprint store and the agent audit trail all hang off it without a config key of
// their own, so it is listed as a location in its own right, which is also what makes
// a node that hand-wrote media_dir onto a data drive but never set `home` still show
// up. The machine-wide lease root (state_dir, gpu_lock_path) is deliberately absent:
// it is shared by every user of the card and holds a few bytes, not data.
func Locations(cfg config.Config) []Location {
	rows := []Location{
		{"home", cfg.BaseDir()},
		{"media_dir", cfg.MediaDir},
		{"svg_dir", cfg.SVGDir},
		{"cache_path", cfg.CachePath},
		{"embed_memo_path", cfg.EmbedMemoPath},
		{"ledger_path", cfg.LedgerPath},
		{"thresholds_path", cfg.ThresholdsPath},
		{"tier_overrides_path", cfg.TierOverridesPath},
		{"router_weights_path", cfg.RouterWeightsPath},
		{"router_labels_path", cfg.RouterLabelsPath},
		{"confhead_path", cfg.ConfHeadPath},
		{"confhead_labels_path", cfg.ConfHeadLabelsPath},
		{"confhead_thresholds_path", cfg.ConfHeadThresholdsPath},
		{"exemplars_dir", cfg.ExemplarsDir},
		{"shadow_queue_path", cfg.ShadowQueuePath},
		{"agent_trajectory_queue_path", cfg.AgentTrajectoryQueuePath},
		{"agent_trajectory_labels_path", cfg.AgentTrajectoryLabelsPath},
		{"knn_index_path", cfg.KNNIndexPath},
		// Both default to a place that is NOT a data path of their own (under media_dir,
		// under the machine-wide state root), so they count only when the file names one.
		{"compose_cache_dir", cfg.ComposeCacheDir},
		{"browse_capture_dir", cfg.BrowseCaptureDir},
	}
	out := make([]Location, 0, len(rows))
	seen := map[string]bool{}
	for _, r := range rows {
		p := strings.TrimSpace(r.Path)
		if p == "" || seen[strings.ToLower(p)] {
			continue
		}
		seen[strings.ToLower(p)] = true
		out = append(out, Location{Key: r.Key, Path: p})
	}
	return out
}

// Audit checks every data location of cfg against osDrive (`C:\`; "" on a host with
// no OS drive letter, where the rule does not apply and the Report is empty) and asks
// the install-volume rule where the data should live instead.
func Audit(cfg config.Config, osDrive string, vols []volumes.Volume) Report {
	if volumes.DriveRootOf(osDrive) == "" {
		return Report{}
	}
	r := Report{OSDrive: osDrive}
	for _, l := range Locations(cfg) {
		if volumes.OnDrive(l.Path, osDrive) {
			r.OnOS = append(r.OnOS, l)
		}
	}
	if len(r.OnOS) == 0 {
		return r
	}
	choice, err := DataTarget(vols, osDrive)
	if err != nil {
		r.Why = err.Error()
		return r
	}
	r.Target = &choice
	return r
}

// DataTarget is the volume the harness's data should live on: the install-volume rule over
// the volumes that may hold it. It is the one place doctor, `data status` and `data
// migrate` ask, so the answer cannot differ between them. The OS drive is never the answer,
// whatever the list says about it (the point of the rule is that it is not allowed to be),
// and neither is a cloud-synced virtual drive or a FAT-family volume: Pick alone would name
// Google Drive for desktop (FAT32, not removable, a cloud quota's free space) whenever it
// shows the most room, and the operator would be told to copy a bbolt store onto it.
func DataTarget(vols []volumes.Volume, osDrive string) (volumes.Choice, error) {
	return volumes.PickData(withoutDrive(vols, osDrive), volumes.PickOptions{})
}

// withoutDrive drops the OS drive from the candidates, so a volume list that did not
// mark it IsOS (a stale enumeration, a test) cannot make it the target.
func withoutDrive(vols []volumes.Volume, osDrive string) []volumes.Volume {
	out := make([]volumes.Volume, 0, len(vols))
	for _, v := range vols {
		if volumes.OnDrive(v.Root, osDrive) {
			continue
		}
		out = append(out, v)
	}
	return out
}

// Failing reports whether the node should FAIL doctor: data on the OS drive AND a
// data volume that qualifies to take it. A box with nowhere to move to (one disk) is
// reported, not failed, because a FAIL nobody can clear teaches everyone to ignore the
// verb; the explicit decision to keep data on the OS drive is the install-volume
// rule's allow-os-volume, and it belongs to the operator.
func (r Report) Failing() bool { return len(r.OnOS) > 0 && r.Target != nil }

// Grouped splits OnOS for display: the install root (nil when it is not on the OS
// drive), the locations sitting inside that root, which follow it when it moves, and
// the rest, each of which needs its own fix. A node on the defaults has one root and
// seventeen paths under it; naming the root once says as much as eighteen rows.
func (r Report) Grouped() (root *Location, under, others []Location) {
	for i := range r.OnOS {
		if r.OnOS[i].Key == "home" {
			root = &r.OnOS[i]
		}
	}
	for _, l := range r.OnOS {
		switch {
		case l.Key == "home":
		case root != nil && inside(l.Path, root.Path):
			under = append(under, l)
		default:
			others = append(others, l)
		}
	}
	return root, under, others
}

// inside reports whether path is dir or sits under it, comparing as Windows does:
// either slash, any case.
func inside(path, dir string) bool {
	norm := func(p string) string { return strings.TrimRight(strings.ToLower(strings.ReplaceAll(p, `\`, "/")), "/") }
	p, d := norm(path), norm(dir)
	return p == d || strings.HasPrefix(p, d+"/")
}

// ProposedHome is the directory an install on volume root would use as `home`: a
// fixed name directly under the volume root, in the root's own separator style.
func ProposedHome(root string) string {
	sep := "/"
	if strings.Contains(root, `\`) || volumes.DriveOf(root) != "" {
		sep = `\`
	}
	return strings.TrimRight(root, `\/`) + sep + "local-offload"
}

// HomeValue is home spelled as the JSON value for the `home` key: forward slashes,
// which every Windows API accepts and which need no escaping inside the string.
func HomeValue(home string) string { return strings.ReplaceAll(home, `\`, "/") }
