package mediaremote

// A node's 412 says WHY, and the delegator tells a wait from a change (F59 polish round; ADR 0082 decision 7). Since F59 a node
// also refuses a recipe_digest when it has only not finished reading its weight files, which is a wait for an answer and not a
// finding that they changed, yet every 412 was read as "its files changed since it published them" and paused the node for a
// minute and then for the cap. The node
// names the cause in the body of its refusal (mediacap.RecipeRefusalCauseKey); these tests pin how the delegator reads it:
//
//	pending     a wait: the node is left alone for the first bounce step, no more, and it is not counted as a bounce;
//	unreadable  the node cannot vouch for the family: bounced, a minute and then the cap;
//	changed     its files or binding are not what it published: bounced, a minute and then the cap;
//	no cause    (a proxy, a node of another build, a word this build does not know): read as a change.
//
// The node's own half is pinned in internal/fleetnode/image_recipe_cause_test.go. The refusals below are served by the fixture
// with the body a node writes, from the same constants, except the tests that drive a REAL node to a real 412.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// The node's own sentence for each refusal (the shape of fleetnode's recipeMismatchError), which the row quotes after the
// delegator's own words.
const (
	whyPending    = "recipe_digest 0123456789ab… does not match image family qwen-image-2.1 on this node: this node cannot fingerprint qwen3vl_8b_bf16.safetensors (pending); read /fleet/health image_recipes again and place the job from that"
	whyUnreadable = "recipe_digest 0123456789ab… does not match image family qwen-image-2.1 on this node: this node cannot fingerprint qwen3vl_8b_bf16.safetensors (unreadable); read /fleet/health image_recipes again and place the job from that"
	whyChanged    = "recipe_digest 0123456789ab… does not match image family qwen-image-2.1 on this node: this node's family digests to fedcba987654…; read /fleet/health image_recipes again and place the job from that"
)

// What the delegator says of each cause, in the node's pause row ("not offered a call: <this>; looking again in <time>").
const (
	saysPending    = "it refused the recipe (412): it had not finished reading its weight files, so it could not say yet whether they changed"
	saysUnreadable = "it refused the recipe (412): it could not read one of its weight files, so it cannot vouch for the family"
	saysChanged    = "it refused the recipe (412): its files or its binding changed since it published them"
	saysNoCause    = "it refused the recipe (412) without saying why"
)

// refusalBody is the body a fleet node writes for a 412: the shape of every refusal (status, error) and, when cause is not empty,
// the cause.
func refusalBody(cause, why string) string {
	m := map[string]string{"status": "error", "error": why}
	if cause != "" {
		m[mediacap.RecipeRefusalCauseKey] = cause
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// rowSays accepts an answer whose cluster row for the node says phrase.
func rowSays(node, phrase string) func(core.Result) bool {
	return func(res core.Result) bool {
		var d struct {
			Cluster []ClusterRow `json:"cluster"`
		}
		if json.Unmarshal(res.Data, &d) != nil {
			return false
		}
		for _, r := range d.Cluster {
			if strings.Contains(r.Node, node) && strings.Contains(r.Why, phrase) {
				return true
			}
		}
		return false
	}
}

// refusalRun is one delegator with one node that refuses every dispatch with a 412, and the placer's clock under the test's hand.
type refusalRun struct {
	t   *testing.T
	n   *node
	cfg config.Config
	lr  *laneRunner
	req core.Request
	now *time.Time
}

func newRefusalRun(t *testing.T, body string) *refusalRun {
	t.Helper()
	n := startImageNode(t, "node-a", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{})
	n.refusePost, n.refuseBody = http.StatusPreconditionFailed, body
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, n)
	return &refusalRun{t: t, n: n, cfg: cfg, lr: &laneRunner{verdict: heldLane()}, req: overflowReq(t, "qwen-image-2.1-fast", nil), now: pinPlacerClock(t)}
}

// refused sends the call and returns the node's row once the node has refused it and is being left alone, and says why in
// the words of says.
func (r *refusalRun) refused(says string) ClusterRow {
	r.t.Helper()
	res := settle(r.t, r.cfg, r.lr, r.req, rowSays("node-a", "not offered a call: "+says+"; looking again in "))
	row, _ := rowFor(clusterOf(r.t, res), "node-a")
	return row
}

// pause is how the placer holds the node once it has been refused, waiting for the pause to be set.
func (r *refusalRun) pause() pause {
	r.t.Helper()
	var pz pause
	waitFor(r.t, 5*time.Second, "the node is left alone after its refusal", func() bool {
		var backed bool
		pz, backed = placer.backedOff(r.n.srv.URL)
		return backed
	})
	return pz
}

// again moves the clock past the pause the node is under and waits until the node has been sent the call for the posts-th time.
func (r *refusalRun) again(posts int) {
	r.t.Helper()
	*r.now = r.now.Add(r.pause().left + time.Second)
	waitFor(r.t, 5*time.Second, fmt.Sprintf("the node is offered the call again (POST %d)", posts), func() bool { return len(r.n.posts()) == posts })
}

// answers makes the node's next refusals carry body.
func (r *refusalRun) answers(body string) {
	r.n.mu.Lock()
	defer r.n.mu.Unlock()
	r.n.refuseBody = body
}

// A node that has not finished reading its weight files has made no finding that they changed. The row says it could not say
// yet, the node is left alone for the first step and no longer, however many times it answers that way, and the waits are not
// bounces: a real change after them still starts at a minute and not at the cap.
func TestAPendingRefusalIsAWaitAndIsNeverCountedAsABounce(t *testing.T) {
	r := newRefusalRun(t, refusalBody(mediacap.RecipeRefusalPending, whyPending))

	row := r.refused(saysPending)
	if row.State != "refused" || !strings.Contains(row.Why, "looking again in 1m0s") || strings.Contains(row.Why, "changed since it published") {
		t.Errorf("a node still reading its files is not told to have changed them, and is told for how long it is left alone: %+v", row)
	}
	for refusal := 1; refusal <= 3; refusal++ {
		if pz := r.pause(); pz.left != bounceSteps[0] || pz.state != "refused" {
			t.Fatalf("refusal %d (pending): the node is left alone for %s as refused, got %+v: a wait never reaches the cap", refusal, bounceSteps[0], pz)
		}
		if refusal < 3 {
			r.again(refusal + 1)
		}
	}
	if got := len(r.n.posts()); got != 3 {
		t.Fatalf("inside its pause the node is sent nothing: %d POSTs", got)
	}

	// The node's files change after all: the first change is a bounce of a minute, the second the cap. The waits left no count.
	r.answers(refusalBody(mediacap.RecipeRefusalChanged, whyChanged))
	r.again(4)
	if pz := r.pause(); pz.left != bounceSteps[0] {
		t.Errorf("the first real change after three waits is left alone for %s, got %s: a wait must not be counted as a bounce", bounceSteps[0], pz.left)
	}
	r.again(5)
	if pz := r.pause(); pz.left != maxCooldown {
		t.Errorf("the second real change is left alone for the cap %s, got %s", maxCooldown, pz.left)
	}
}

// A node that cannot read a weight file, and one whose files or binding changed, are bounced as every refusal was: a minute, then
// the cap, which holds. Each row says which it was, and says nothing of the other cause.
func TestAnUnreadableOrChangedRefusalIsBouncedAMinuteThenTheCap(t *testing.T) {
	for _, tc := range []struct {
		name, cause, why, says, never string
	}{
		{"unreadable", mediacap.RecipeRefusalUnreadable, whyUnreadable, saysUnreadable, "could not say yet"},
		{"changed", mediacap.RecipeRefusalChanged, whyChanged, saysChanged, "not finished reading"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusalRun(t, refusalBody(tc.cause, tc.why))
			row := r.refused(tc.says)
			if row.State != "refused" || strings.Contains(row.Why, tc.never) {
				t.Errorf("the row says what it was and nothing of the other cause: %+v", row)
			}
			for i, want := range []time.Duration{bounceSteps[0], maxCooldown, maxCooldown} {
				if pz := r.pause(); pz.left != want || pz.state != "refused" {
					t.Fatalf("refusal %d: the node is left alone for %s as refused, got %+v", i+1, want, pz)
				}
				if i < 2 {
					r.again(i + 2)
				}
			}
		})
	}
}

// A refusal that names no cause this build knows (a proxy's plain text, a node of another build, a word it has never heard) is
// read as a change, the conservative reading: bounced as always, and the row does not claim to know what happened.
func TestARefusalWithNoKnownCauseIsBouncedAsAChange(t *testing.T) {
	for name, body := range map[string]string{
		"plain text":    "",
		"no member":     refusalBody("", whyPending),
		"a stranger":    refusalBody("hungry", whyPending),
		"a sentence":    `{"status":"error","error":"x","cause":"it is pending, ask again"}`,
		"not a string":  `{"status":"error","error":"x","cause":["pending"]}`,
		"a JSON string": `"pending"`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newRefusalRun(t, body)
			row := r.refused(saysNoCause)
			if row.State != "refused" || strings.Contains(row.Why, "could not say yet") {
				t.Errorf("a node that does not say why is not taken for one that is only waiting: %+v", row)
			}
			if pz := r.pause(); pz.left != bounceSteps[0] {
				t.Fatalf("the first such refusal is a minute, got %+v", pz)
			}
			r.again(2)
			if pz := r.pause(); pz.left != maxCooldown {
				t.Errorf("the second is the cap, got %s", pz.left)
			}
		})
	}
}

// The row of the attempt itself (the one the ticket shows until its next scan) says what the refusal was in the delegator's words
// and then quotes the node's own, which names the file; and the three causes and the one that is not named are the four things
// it can be.
func TestTheRowOfARefusalSaysWhatItWasAndQuotesTheNode(t *testing.T) {
	resetPlacer()
	t.Cleanup(resetPlacer)
	pinPlacerClock(t)
	c := candidate{base: "http://node-a.invalid", node: "node-a", shown: "node-a (node-a)"}
	for _, tc := range []struct {
		name, cause, why, says string
	}{
		{"pending", mediacap.RecipeRefusalPending, whyPending, saysPending},
		{"unreadable", mediacap.RecipeRefusalUnreadable, whyUnreadable, saysUnreadable},
		{"changed", mediacap.RecipeRefusalChanged, whyChanged, saysChanged},
		{"no cause", "", whyPending, saysNoCause},
	} {
		body := refusalBody(tc.cause, tc.why)
		pe := &postError{
			placementError: &placementError{core.DeferClassInfrastructure, "dispatch http://node-a.invalid/fleet/dispatch: status 412: " + truncate([]byte(body))},
			status:         http.StatusPreconditionFailed, cause: tc.cause,
		}
		a := refusedRecipe(c, pe)
		if a.kind != attemptOut || a.row.State != "refused" || a.row.Node != c.shown {
			t.Errorf("%s: a refused recipe is a node out for this call, nothing ran there: %+v", tc.name, a)
		}
		// The delegator's words first (the same sentence as the pause's, without its "it"), then the node's own refusal.
		if want := strings.TrimPrefix(tc.says, "it ") + "; " + pe.Error(); a.row.Why != want {
			t.Errorf("%s: the row is the delegator's words and then the node's own refusal:\n got %s\nwant %s", tc.name, a.row.Why, want)
		}
		pz, backed := placer.backedOff(c.base)
		if !backed || !strings.Contains(pz.why(), tc.says) {
			t.Errorf("%s: the node is left alone with the cause in the pause's own words: %+v %v", tc.name, pz, backed)
		}
		resetPlacer()
	}
}

// The cause is read as a member of the body and only the three words of the wire count: a sentence that happens to contain one is
// not a cause, and nothing else the node writes is trusted to choose what the delegator does.
func TestTheCauseOfARefusalIsReadAsAMemberAndOnlyKnownWordsCount(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"pending", refusalBody(mediacap.RecipeRefusalPending, "x"), mediacap.RecipeRefusalPending},
		{"unreadable", refusalBody(mediacap.RecipeRefusalUnreadable, "x"), mediacap.RecipeRefusalUnreadable},
		{"changed", refusalBody(mediacap.RecipeRefusalChanged, "x"), mediacap.RecipeRefusalChanged},
		{"the word in the sentence only", refusalBody("", "this node cannot fingerprint a.safetensors (pending)"), ""},
		{"an unknown word", refusalBody("PENDING", "x"), ""},
		{"a word with padding", refusalBody(" pending", "x"), ""},
		{"empty", "", ""},
		{"text", "refused by the test", ""},
		{"an array", `["pending"]`, ""},
		{"a number", `{"cause":1}`, ""},
	} {
		if got := refusalCause([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: cause = %q, want %q (body %s)", tc.name, got, tc.want, tc.body)
		}
	}
}

// A REAL NODE, end to end. The node's weight file is replaced after the delegator read its health, so the node itself
// recomputes the digest and refuses with its own 412: post() carries the body the node really writes up to the delegator, which
// reads the cause from it.
func TestARealNodeWhoseFileChangedAnswersWithTheCauseChanged(t *testing.T) {
	var tree string
	a := startImageNode(t, "node-a", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")},
		nodeOpts{cfg: func(c *config.Config) { tree = c.ComfyDir }})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, a)
	local, ok, why := localIdentity(context.Background(), cfg, "qwen-image-2.1-fast")
	if !ok {
		t.Fatal(why)
	}
	replaceInPlace(t, tree, "models/diffusion_models/"+int8Ckpt, 0xEE)
	body, _ := json.Marshal(map[string]any{"job_id": "media-deadbeef00000009", "task_type": taskImage, "payload": map[string]any{
		"prompt": "a red door", "family": "qwen-image-2.1", "seed": 424242, "width": 2048, "height": 2048, "recipe_digest": local.recipe.Digest()}})

	err := post(context.Background(), cfg, a.srv.URL, "/fleet/dispatch", body)
	var pe *postError
	if !errors.As(err, &pe) || pe.status != http.StatusPreconditionFailed {
		t.Fatalf("the node whose file changed refuses the recipe with 412: %v", err)
	}
	if pe.cause != mediacap.RecipeRefusalChanged {
		t.Errorf("cause = %q, want %q: %v", pe.cause, mediacap.RecipeRefusalChanged, err)
	}
	if !strings.Contains(pe.Error(), "digests to") {
		t.Errorf("the node's own sentence still comes with it: %v", err)
	}
	a.runner.mu.Lock()
	ran := len(a.runner.reqs)
	a.runner.mu.Unlock()
	if ran != 0 {
		t.Errorf("a refused dispatch ran nothing: %d", ran)
	}
}

// ...and through the ticket: the replaced file is found by the node at the dispatch the ticket sends, the node is left alone as
// one whose files changed, and the call waits in its ticket (it is not run here, and not sent again).
func TestATicketLeavesARealNodeWhoseFileChangedAloneForAMinute(t *testing.T) {
	var tree string
	a := startImageNode(t, "node-a", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")},
		nodeOpts{cfg: func(c *config.Config) { tree = c.ComfyDir }})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, a)
	pinPlacerClock(t)
	a.beforePost = func([]byte) { replaceInPlace(t, tree, "models/diffusion_models/"+int8Ckpt, 0xEE) }
	lr := &laneRunner{verdict: heldLane()}

	res := settle(t, cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), rowSays("node-a", "not offered a call: "+saysChanged+"; looking again in 1m0s"))
	if row, _ := rowFor(clusterOf(t, res), "node-a"); row.State != "refused" {
		t.Errorf("the node answered the dispatch with a refusal: %+v", row)
	}
	if got := len(a.posts()); got != 1 {
		t.Fatalf("the node was sent the job once: %d", got)
	}
	if _, runs := lr.counts(); runs != 0 {
		t.Errorf("the call waits in its ticket, it is not run here: runs %d", runs)
	}
}
