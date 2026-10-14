package pipeline

// A media call's lease window is the call's own timeout (register C-33). acquireMediaLease takes the duration the render
// runs under and stamps it on the lease as its TTL, so a render cannot outlive the lease the pipeline took for it: the
// window and the kill are one number. gpulease.DefaultTTL (3,600 s) is only the fallback for a lease acquired with no
// window, and no media call is one.
//
// These are guards, green on the code as it stands: they fail the day a media route is moved onto the fallback window, or
// clamps its TTL below the render's timeout (a 5,400 s video render under a 3,600 s lease), or hard-codes one. Each runs on
// both lease paths, the card-scoped writer (mediaadmit.go) and the whole-node one (pipeline.go), because each stamps the
// TTL in its own place. These two pin the lease half for a ComfyUI image and video call on both paths. The other half, that the
// render is killed at the same value, and the same pin over every other route that takes the lease (and a batch's per-image
// timeout times its jobs), are in mediaadmit_lease_routes_test.go. The operator guide's description of all this is pinned in
// operator_guide_lease_row_test.go.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/hostneed"
)

// leaseWindowHosts are the two lease paths a media call can take.
var leaseWindowHosts = []struct {
	name    string
	noAudit bool
}{
	{"card-scoped host", false},
	{"whole-node host", true},
}

// theOneLeaseWindow is the declared window of the single live lease: its end minus its acquisition, read from the record.
// The two readings are taken a moment apart, so the result is the stamped TTL less a few milliseconds.
func theOneLeaseWindow(t *testing.T, f *admitFixture) time.Duration {
	t.Helper()
	leases := f.m.Leases()
	if len(leases) != 1 {
		t.Fatalf("want one live lease while the render runs, got %+v", leases)
	}
	return time.Until(leases[0].ExpiresAt) + leases[0].Age
}

// wantWindow fails unless got is want, to the second (the stamp is in milliseconds, the reads are a moment apart).
func wantWindow(t *testing.T, what string, got, want time.Duration) {
	t.Helper()
	if d := got - want; d < -2*time.Second || d > 2*time.Second {
		t.Errorf("%s: the lease window is %s; the call's own timeout is %s (gpulease.DefaultTTL, the fallback for a lease with no window, is %s)",
			what, got.Round(time.Second), want, gpulease.DefaultTTL)
	}
}

// A video call under a 5,400 s timeout holds a 5,400 s lease: longer than the 3,600 s fallback, so neither the fallback nor
// a clamp to it can pass. (That the render is killed at that same value is pinned in mediaadmit_lease_routes_test.go, which
// has to use a short timeout to wait for a kill.)
func TestAVideoCallsLeaseWindowIsItsOwnTimeout(t *testing.T) {
	const timeout = 5400 * time.Second
	if timeout <= gpulease.DefaultTTL {
		t.Fatalf("this pin needs a timeout longer than the fallback window (%s), got %s", gpulease.DefaultTTL, timeout)
	}
	const (
		int8T = "ltx-2.5-22b-distilled-transformer-comfy-int8-convrot.safetensors"
		gemma = "gemma4-12b-with-proj-ltx-2.5-comfy-int8-convrot.safetensors"
	)
	sizes := map[string]float64{int8T: 20.03, gemma: 14.32}
	for _, host := range leaseWindowHosts {
		t.Run(host.name, func(t *testing.T) {
			restore := hostneed.UseStat(func(path string) (int64, bool) {
				g, ok := sizes[filepath.Base(path)]
				return int64(g * (1 << 30)), ok
			})
			t.Cleanup(restore)

			f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, noAudit: host.noAudit, mutate: func(c *config.Config) {
				c.VideoGenScript, c.VideoGenFamily = c.ImageGenScript, "ltx25" // the fixture's fake runner
				c.VideoGenTimeoutSec = int(timeout / time.Second)
			}})
			for _, class := range []string{"diffusion_models", "text_encoders"} {
				if err := os.MkdirAll(filepath.Join(f.cfg.ComfyDir, "models", class), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			call := f.start(core.TaskGenerateVideo, "a calm ocean at dawn", map[string]any{
				"model": "ltx25",
				"out":   filepath.Join(f.dir, "clip.mp4"),
			})
			f.waitStarted(1)
			wantWindow(t, "video", theOneLeaseWindow(t, f), timeout)
			f.letRunnersGo()
			if r := f.await(call); !r.OK {
				t.Fatalf("the video call: class=%q %s", r.Meta.ErrClass, r.Reason)
			}
		})
	}
}

// An image call under a 100 s timeout holds a 100 s lease, not the 3,600 s fallback: a wedged render can deny the card for
// the timeout it was given and no longer. (The deployed 3,600 s imagegen timeout the roadmap's T6 cut to 600 was a 3,600 s
// lease for exactly this reason.)
func TestAnImageCallsLeaseWindowIsItsOwnTimeout(t *testing.T) {
	const timeout = 100 * time.Second
	if timeout >= gpulease.DefaultTTL {
		t.Fatalf("this pin needs a timeout shorter than the fallback window (%s), got %s", gpulease.DefaultTTL, timeout)
	}
	for _, host := range leaseWindowHosts {
		t.Run(host.name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, noAudit: host.noAudit, mutate: func(c *config.Config) {
				c.ImageGenTimeoutSec = int(timeout / time.Second)
			}})
			call := f.image(nil)
			f.waitStarted(1)
			wantWindow(t, "image", theOneLeaseWindow(t, f), timeout)
			f.letRunnersGo()
			if r := f.await(call); !r.OK {
				t.Fatalf("the image call: class=%q %s", r.Meta.ErrClass, r.Reason)
			}
		})
	}
}
