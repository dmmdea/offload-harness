package mediaremote

// The delegator's reads of its own weight files must not outlive the call or hold up the ticket scan, and a call that
// cannot overflow because of them says so in the log (F59 fix round).

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// heldFingerprinter is a fingerprinter that has hashed nothing and whose reads wait for release (registered for the end of
// the test too).
func heldFingerprinter(t *testing.T) *mediacap.Fingerprinter {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(gate) }) })
	return mediacap.NewFingerprinterWith(func(path string, size int64) (string, error) {
		<-gate
		return mediacap.FingerprintFile(path, size)
	})
}

// swapLocalReaders points this machine's two ways of reading a weight file at await and peek, for one test.
func swapLocalReaders(t *testing.T, await func(context.Context, string, int64) string, peek mediacap.FingerprintFunc) {
	t.Helper()
	pa, pp := localFingerprints, localPeek
	localFingerprints, localPeek = await, peek
	t.Cleanup(func() { localFingerprints, localPeek = pa, pp })
}

func inTime[T any](t *testing.T, d time.Duration, what string, fn func() T) T {
	t.Helper()
	ch := make(chan T, 1)
	go func() { ch <- fn() }()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatalf("%s did not return in %s", what, d)
		var zero T
		return zero
	}
}

// THE IDENTITY ENDS WITH THE CALL. With every read held, localIdentity of a call whose context is cancelled gives up at once:
// no identity, and the reason names the files as pending. The call then runs on this machine's own line.
func TestLocalIdentityEndsWhenItsCallerIsGone(t *testing.T) {
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")})
	f := heldFingerprinter(t)
	swapLocalReaders(t, f.AwaitContext, f.Peek)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	type answer struct {
		ok  bool
		why string
	}
	got := inTime(t, 5*time.Second, "localIdentity of a cancelled caller", func() answer {
		_, ok, why := localIdentity(ctx, cfg, "qwen-image-2.1")
		return answer{ok, why}
	})
	if got.ok {
		t.Fatal("a family whose files are not fingerprinted has no identity")
	}
	if !strings.Contains(got.why, "qwen3vl_8b_bf16.safetensors (pending)") {
		t.Errorf("the reason must name the file that is still pending: %s", got.why)
	}
}

// THE HINT NEVER WAITS. "send family=X to use its Y" names another family of this machine; building it must not read a
// file on the scan path: a family still being hashed is left out of the hint (and its hash is started for the next call).
func TestTheAlternativeFamiliesHintNeverWaitsForAHash(t *testing.T) {
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1": block(int8Ckpt, ""), "qwen-image-2.1-fast": block(int8Ckpt, "")})
	l, ok, why := localIdentity(context.Background(), cfg, "qwen-image-2.1")
	if !ok {
		t.Fatal(why)
	}
	if warm := l.others(); len(warm) == 0 {
		t.Fatal("premise: with the files fingerprinted the hint knows this machine's other families")
	}

	l, _, _ = localIdentity(context.Background(), cfg, "qwen-image-2.1") // a fresh memo
	f := heldFingerprinter(t)
	swapLocalReaders(t, func(context.Context, string, int64) string {
		t.Error("the hint waited for a fingerprint (Await)")
		return mediacap.FingerprintPending
	}, f.Peek)
	others := inTime(t, 5*time.Second, "the hint of a machine whose other families are not hashed", l.others)
	if len(others) != 0 {
		t.Errorf("families whose files are not fingerprinted are left out of the hint: %v", others)
	}
}

// THE DOOR SAYS WHY. A call that cannot overflow because this machine could not fingerprint a file of its own family runs on
// its own line (as for a missing file), and the log says which file and why, so a transient failure is not invisible.
func TestTheDoorLogsWhyAnImageCallCannotOverflow(t *testing.T) {
	n := startImageNode(t, "node-b", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, n)
	prev := localFingerprints
	swapLocalReaders(t, func(ctx context.Context, path string, size int64) string {
		if strings.Contains(filepath.ToSlash(path), "/text_encoders/") {
			return mediacap.FingerprintUnreadable
		}
		return prev(ctx, path, size)
	}, localPeek)
	var buf bytes.Buffer
	pw := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(pw) })

	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
	runDoor(cfg, lr, overflowReq(t, "qwen-image-2.1", nil), AtOnce)
	noPosts(t, "a machine that could not fingerprint its own family", n)
	if _, runs := lr.counts(); runs != 1 {
		t.Errorf("the call runs on this machine's line: runs %d", runs)
	}
	if out := buf.String(); !strings.Contains(out, "qwen3vl_8b_bf16.safetensors (unreadable)") || !strings.Contains(out, "qwen-image-2.1") {
		t.Errorf("the log must say which family and which file: %q", out)
	}
}

// THE ADOPTER SAYS WHY TOO. A dead owner's placed job is taken over by the call that names its ticket; the adopter builds
// this machine's identity for the ticket's scan, and when a file reads back unreadable it goes on without one (the job is
// already placed) and logs the reason, as the door does.
func TestTheAdopterLogsWhyItHasNoIdentity(t *testing.T) {
	rig := newPairRig(t, true, "node-a")
	e := newQEnv(t, 1, false)
	e.cfg.PairWorkloadsEnabled = true
	e.lr.rig = rig
	n := e.nodes[0]

	local, ok, why := localIdentity(context.Background(), e.cfg, "qwen-image-2.1-fast")
	if !ok {
		t.Fatal(why)
	}
	req := e.req("a red door", nil)
	jobID := "media-deadbeef00000002"
	body, _ := json.Marshal(map[string]any{"job_id": jobID, "task_type": taskImage, "payload": map[string]any{
		"prompt": req.Input, "family": "qwen-image-2.1", "seed": 424242, "width": 2048, "height": 2048, "recipe_digest": local.recipe.Digest()}})
	if err := post(context.Background(), e.cfg, n.srv.URL, "/fleet/dispatch", body); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the node finishes the job", func() bool {
		j, err := getJSON[jobWire](context.Background(), e.cfg, n.srv.URL+"/fleet/jobs/"+jobID)
		return err == nil && j.State == "done"
	})
	st, _ := storeFor(e.cfg)
	dead := ticketFile{ID: "mq-deadbeef0002", ReqKey: reqKey(req), Created: nowMs() - 60_000, Owner: deadOwner, Beat: nowMs() - 5000,
		Task: "generate_image", Out: req.Params["out"].(string), Seed: 424242, State: stPlaced, Node: "node-a", Base: n.srv.URL, Job: jobID,
		LaneWhy: "card(s) c2 held by a media-class lease"}
	if err := st.write(dead); err != nil {
		t.Fatal(err)
	}

	prev := localFingerprints
	swapLocalReaders(t, func(ctx context.Context, path string, size int64) string {
		if strings.Contains(filepath.ToSlash(path), "/text_encoders/") {
			return mediacap.FingerprintUnreadable
		}
		return prev(ctx, path, size)
	}, localPeek)
	var buf bytes.Buffer
	pw := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(pw) })

	named := req // the same request, which now names the ticket
	named.Params = map[string]any{"waiter_token": dead.ID}
	for k, v := range req.Params {
		named.Params[k] = v
	}
	res := runDoor(e.cfg, e.lr, named, callWindow)
	if !res.OK || res.Meta.Node != "node-a" {
		t.Fatalf("the call that names the ticket adopts the job without an identity: %+v", res)
	}
	if out := buf.String(); !strings.Contains(out, "qwen3vl_8b_bf16.safetensors (unreadable)") {
		t.Errorf("the adopter must log why it has no identity: %q", out)
	}
}
