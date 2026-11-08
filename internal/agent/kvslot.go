package agent

// KV-slot retention inside the loop (ADR 0056 Layer 2, ADR 0100). llama-server can write a slot's KV cache to a file
// and read it back; PR #26004 made that buy prefill on hybrid models, so a run whose first request the node has already
// paid for can skip that prefill across the five-minute idle unload. The loop is the only place that knows the
// byte-exact prefix (recall is queried at run time and sits in the preamble), so the retention lives here and the
// keys are computed here.
//
// internal/agent imports neither the pipeline nor the fleet node, so the loop talks to a KVSlotStore the pipeline
// builds over the endpoint, the seat and the shared save/restore core (internal/kvslot). The store and the policy are
// immutable and shared by every run of the Loop; every per-run fact lives in kvRun on the run's budgetState, because
// --serve shares one *Loop across concurrent handlers.
//
// Nothing here may fail or slow a run: every error is a recorded skip, and a restore is never trusted - it is judged
// on the completion that follows it (cache_n against the prompt), never from n_restored.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// KVSlotResult is the answer of one save or restore. OK means tokens moved; otherwise Reason says why not
// ("miss", "seat-cold", "unsupported", "shape-mismatch", an error text), and the loop records it as a skip.
type KVSlotResult struct {
	OK     bool
	Tokens int
	Bytes  int64
	Ms     int64
	Reason string
}

// KVSlotStore is what the loop needs from the node: slot 0 of the run's seat saved to and restored from files keyed by
// the loop's own hashes.
type KVSlotStore interface {
	// Base is the identity every key is hashed over: the seat's config pin (taken once, at attach time) and the model id.
	Base() string
	// Save writes slot 0 under key; Restore reads it back. Each bounds itself.
	Save(ctx context.Context, key string) KVSlotResult
	Restore(ctx context.Context, key string) KVSlotResult
	// FileExists is whether a slot file for key is on disk; Delete removes it.
	FileExists(key string) bool
	Delete(key string)
	// Rewarm loads the seat through the normal warm path when it is no longer loaded, and reports whether it had to.
	Rewarm(ctx context.Context) (rewarmed bool)
	// Seen is the presence tracker (process-level, bounded): it reports whether key was already seen on this node
	// and marks it seen. A key is saved on its SECOND sighting and restored from the third.
	Seen(key string) bool
}

// KVSlotPolicy is the loop's side of the retention: how large a prompt is worth a file (a file is ~80 KB per token)
// and how long a tool call must be allowed to run before a save is taken around it.
type KVSlotPolicy struct {
	MinTokens       int
	ToolWaitSaveSec int
}

// DefaultKVSlotPolicy is the policy a node runs unless told otherwise: 1024 tokens and a 240 s tool cap (the browse
// tool's 360 s cap is past the 300 s idle unload, run_shell's 120 s is not).
func DefaultKVSlotPolicy() KVSlotPolicy { return KVSlotPolicy{MinTokens: 1024, ToolWaitSaveSec: 240} }

// WithKVSlots attaches the retention. A nil store leaves the loop exactly as it was.
func (l *Loop) WithKVSlots(store KVSlotStore, pol KVSlotPolicy) *Loop {
	if store == nil {
		return l
	}
	if pol.MinTokens <= 0 {
		pol.MinTokens = DefaultKVSlotPolicy().MinTokens
	}
	if pol.ToolWaitSaveSec <= 0 {
		pol.ToolWaitSaveSec = DefaultKVSlotPolicy().ToolWaitSaveSec
	}
	l.kvStore, l.kvPolicy = store, pol
	return l
}

// KVSlotReport is what the retention did for one run. Present on the Result only when a store was attached.
type KVSlotReport struct {
	// Restore is the run-start restore, judged on the next completion: "hit", "miss" or "skip:<reason>". Empty when
	// none was attempted (no file for the key yet, or the prompt was under the policy's floor).
	Restore        string `json:"restore,omitempty"`
	RestoredTokens int    `json:"restored_tokens,omitempty"`
	RestoreMS      int64  `json:"restore_ms,omitempty"`
	// Admit is the presence-admission verdict for the run-start key when nothing was restored: "below-min-tokens",
	// "first-sighting" (marked seen), "repeat-saved" or "repeat-save-skip:<reason>".
	Admit            string `json:"admit,omitempty"`
	Saves            int    `json:"saves,omitempty"`
	SavedBytes       int64  `json:"saved_bytes,omitempty"`
	ToolWaitSaves    int    `json:"toolwait_saves,omitempty"`
	ToolWaitRestores int    `json:"toolwait_restores,omitempty"`
	ToolWaitHits     int    `json:"toolwait_hits,omitempty"`
}

// kvToolWaitTimeout bounds the save taken around one tool call, on top of the store's own bounds.
const kvToolWaitTimeout = 2 * time.Minute

// kvHitShare is the share of a completion's prompt the cache must have served for a restore to count as a hit.
const kvHitShare = 0.8

// kvRun is one run's retention state. It lives on the run's budgetState and nowhere else.
type kvRun struct {
	rep KVSlotReport
	// saveKey is the run-start key to save after the first model step (its second sighting); "" = nothing to save.
	saveKey string
	// pending is a restore waiting for the next completion to judge it: "start" or "toolwait"; pendingTokens is how many
	// tokens that restore moved.
	pending       string
	pendingTokens int
	// startKey is the run-start key a pending "start" restore was made from.
	startKey string
	// runKey is the run-scoped key of tool-wait saves ("" until the first one); runSaved says a file exists for it.
	runKey   string
	runSaved bool
}

func (k *kvRun) report(attached bool) *KVSlotReport {
	if !attached {
		return nil
	}
	r := k.rep
	if k.pending == "start" && r.Restore == "" {
		r.Restore = "skip:unjudged" // the run ended before a completion could judge the restore
	}
	return &r
}

// kvKey is "k1-" + sha256 over the store's identity, the canonical JSON of the tool specs and of the messages. The
// JSON encoders emit struct fields in declaration order and map keys sorted, so equal prefixes hash equal.
func kvKey(base string, specs []ToolSpec, msgs []Msg) string {
	sj, err := json.Marshal(specs)
	if err != nil {
		return ""
	}
	mj, err := json.Marshal(msgs)
	if err != nil {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(base))
	h.Write([]byte{0})
	h.Write(sj)
	h.Write([]byte{0})
	h.Write(mj)
	return "k1-" + hex.EncodeToString(h.Sum(nil))
}

// kvRunScopedKey is the key of this run's tool-wait file: the seat identity plus a per-run random id, so no other run
// can ever ask for it.
func kvRunScopedKey(base string) string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(base))
	h.Write([]byte{0})
	h.Write(id[:])
	return "k1-" + hex.EncodeToString(h.Sum(nil))
}

func skipReason(r KVSlotResult) string {
	if r.Reason == "" {
		return "skip:unknown"
	}
	if r.Reason == "miss" {
		return "miss"
	}
	return "skip:" + r.Reason
}

// kvRunStart is the run-start decision, after the preamble AND the setup replay are in msgs and before the first model
// call: restore the key's file when there is one, else apply presence admission. It never returns an error.
func (l *Loop) kvRunStart(ctx context.Context, bs *budgetState, msgs []Msg) {
	if l.kvStore == nil {
		return
	}
	k := &bs.kv
	if estimateTokens(msgs)+int(l.specReserve.Load()) < l.kvPolicy.MinTokens {
		k.rep.Admit = "below-min-tokens"
		return
	}
	key := kvKey(l.kvStore.Base(), l.specs, msgs)
	if key == "" {
		k.rep.Admit = "skip:unkeyable"
		return
	}
	if l.kvStore.FileExists(key) {
		r := l.kvStore.Restore(ctx, key)
		if !r.OK {
			k.rep.Restore = skipReason(r)
			return
		}
		k.rep.RestoredTokens, k.rep.RestoreMS = r.Tokens, r.Ms
		k.pending, k.pendingTokens, k.startKey = "start", r.Tokens, key
		return
	}
	if l.kvStore.Seen(key) {
		k.saveKey = key
		return
	}
	k.rep.Admit = "first-sighting"
}

// kvAfterStep runs after every completion that arrives: it judges a pending restore against what the server says it
// served from cache, then takes the deferred save of the run-start key once the first model step has returned.
func (l *Loop) kvAfterStep(ctx context.Context, bs *budgetState, comp Completion) {
	if l.kvStore == nil {
		return
	}
	k := &bs.kv
	if k.pending != "" {
		// The share is of what the restore COULD serve: the restored tokens, or the whole prompt when that is shorter.
		// After a tool wait the prompt also carries the tool's result, which no restore can hold; judging against the
		// whole prompt would call a working restore before a large page a miss.
		hit := false
		if s := comp.Serve; s != nil && s.CacheN > 0 {
			could := s.CacheN + s.PromptN
			if k.pendingTokens > 0 && k.pendingTokens < could {
				could = k.pendingTokens
			}
			hit = float64(s.CacheN) >= kvHitShare*float64(could)
		}
		switch k.pending {
		case "start":
			if hit {
				k.rep.Restore = "hit"
			} else {
				k.rep.Restore = "miss"
				// A file that restores into a drifted prefix would be restored again by every run with this key; drop
				// it so the next sighting saves a fresh one.
				l.kvStore.Delete(k.startKey)
			}
		case "toolwait":
			if hit {
				k.rep.ToolWaitHits++
			}
		}
		k.pending, k.pendingTokens = "", 0
	}
	if k.saveKey != "" {
		key := k.saveKey
		k.saveKey = ""
		r := l.kvStore.Save(ctx, key)
		if r.OK {
			k.rep.Saves++
			k.rep.SavedBytes += r.Bytes
			k.rep.Admit = "repeat-saved"
		} else {
			k.rep.Admit = "repeat-save-skip:" + r.Reason
		}
	}
}

// kvToolWait is a save running beside one tool call.
type kvToolWait struct {
	done   chan KVSlotResult
	cancel context.CancelFunc
}

// kvToolWaitBegin starts the save around a tool call whose cap reaches the policy's floor, when the last prompt was big
// enough to be worth a file. The save runs concurrently with the tool and is joined by kvToolWaitEnd.
func (l *Loop) kvToolWaitBegin(ctx context.Context, bs *budgetState, call ToolCall, lastPrompt int) *kvToolWait {
	if l.kvStore == nil || lastPrompt < l.kvPolicy.MinTokens {
		return nil
	}
	t, ok := l.tools[call.Name]
	if !ok {
		return nil
	}
	cap := t.Timeout
	if cap <= 0 {
		cap = l.toolTimeout
	}
	// A non-positive cap is an uncapped tool: it can outlast any unload.
	if cap > 0 && cap < time.Duration(l.kvPolicy.ToolWaitSaveSec)*time.Second {
		return nil
	}
	k := &bs.kv
	if k.runKey == "" {
		k.runKey = kvRunScopedKey(l.kvStore.Base())
		if k.runKey == "" {
			return nil
		}
	}
	sctx, cancel := context.WithTimeout(ctx, kvToolWaitTimeout)
	w := &kvToolWait{done: make(chan KVSlotResult, 1), cancel: cancel}
	key, store := k.runKey, l.kvStore
	go func() { w.done <- store.Save(sctx, key) }()
	return w
}

// kvToolWaitEnd joins the save and, when the seat went cold during the wait, brings the session back: it re-warms the
// seat through the normal path and restores the run-scoped file. The restore is judged on the next completion.
func (l *Loop) kvToolWaitEnd(ctx context.Context, bs *budgetState, w *kvToolWait) {
	if w == nil {
		return
	}
	r := <-w.done // bounded by the save's own timeout
	w.cancel()
	k := &bs.kv
	if !r.OK {
		return
	}
	k.runSaved = true
	k.rep.ToolWaitSaves++
	k.rep.SavedBytes += r.Bytes
	if ctx.Err() != nil || !l.kvStore.Rewarm(ctx) {
		return // the seat is still loaded: its slot is intact, nothing to restore
	}
	rr := l.kvStore.Restore(ctx, k.runKey)
	if !rr.OK {
		return
	}
	k.rep.ToolWaitRestores++
	k.pending, k.pendingTokens = "toolwait", rr.Tokens
}

// kvRunEnd removes the run-scoped file whatever happened to the run.
func (l *Loop) kvRunEnd(bs *budgetState) {
	if l.kvStore != nil && bs.kv.runKey != "" {
		l.kvStore.Delete(bs.kv.runKey)
	}
}
