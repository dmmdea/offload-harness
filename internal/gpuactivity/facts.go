package gpuactivity

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// facts.go — what can honestly be said about a LEGACY lease: one that names no owner and
// carries no progress contract, so neither "is its owner there" nor "is it progressing"
// can be asked of it. Two facts are shown, as information and never as a verdict input:
// how long ago the newest ComfyUI output was written, and how long ago the ComfyUI log
// was. A reader decides for itself whether a render that has not written in an hour is
// working; the harness refuses to guess for it, because a guess is how a stale lease was
// ever labelled green.
//
// The ComfyUI launch marker is evidence about a lease only when it can be tied to it. A
// marker left by an earlier lease's job proves nothing about this one, so it counts only
// when it was written at or after this lease began and this is the only live lease.
// (Tying it by process ancestry, the other safe rule, needs the process-tree work and is
// not attempted here.)

// factsOutputWalkLimit bounds how many output entries (files and directories) are looked
// at: a media output directory can hold tens of thousands and a status call must stay
// cheap. A var so a test can lower it.
var factsOutputWalkLimit = 5000

// legacyFacts returns the activity facts for one lease, or nil when the lease is not a
// legacy lease, there is no ComfyUI directory, or the marker cannot be tied to the lease.
func legacyFacts(comfyDir string, info gpulease.Info, onlyLease bool, now time.Time) []string {
	if comfyDir == "" || info.Owner != nil || info.Progress != nil || !onlyLease {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(comfyDir, ".offload-launch.json"))
	if err != nil {
		return nil
	}
	var marker struct {
		StartedAt int64 `json:"startedAt"`
	}
	if json.Unmarshal(raw, &marker) != nil || marker.StartedAt <= 0 {
		return nil
	}
	if info.AcquiredAt.IsZero() || marker.StartedAt < info.AcquiredAt.UnixMilli() {
		return nil
	}
	var out []string
	switch newest, found, capped := newestOutput(filepath.Join(comfyDir, "output")); {
	case capped:
		// The newest file the scan saw is not necessarily the newest there is, and stating
		// an old time as "the newest output" would read a live job as stale.
		out = append(out, fmt.Sprintf("newest ComfyUI output unknown (output directory scan capped at %d entries)", factsOutputWalkLimit))
	case found:
		out = append(out, "newest ComfyUI output "+ago(now.Sub(newest)))
	default:
		out = append(out, "no ComfyUI output written yet")
	}
	if fi, err := os.Stat(filepath.Join(comfyDir, "offload-comfyui.log")); err == nil {
		out = append(out, "ComfyUI log last written "+ago(now.Sub(fi.ModTime())))
	}
	return out
}

// newestOutput is the modification time of the newest file under dir. capped reports that
// the scan stopped at the walk limit (directories count toward it: a large output tree is
// mostly directories), in which case the answer is not trustworthy and is not given.
func newestOutput(dir string) (newest time.Time, found, capped bool) {
	seen := 0
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if seen++; seen > factsOutputWalkLimit {
			capped = true
			return fs.SkipAll
		}
		if d.IsDir() {
			return nil
		}
		if fi, ierr := d.Info(); ierr == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		return nil
	})
	return newest, !newest.IsZero(), capped
}

// ago renders an elapsed time as "4m ago" / "2h10m ago".
func ago(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm ago", int(d.Hours()), int(d.Minutes())%60)
	}
}
