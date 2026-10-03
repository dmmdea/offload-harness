package pipeline

import (
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// errClassGPUBusy is the err_class of a vision call that was skipped because a
// generation job holds the GPU lock (LO-1). runVisionGen stamps it; video_watch
// reads it to stop a sweep and to name the call's own class.
const errClassGPUBusy = "gpu_busy"

// mintJobID returns a fresh id for a call that writes several ledger rows: the
// call's own row carries it as JobID and every other row of the call names it as
// ParentJobID (an inner row, register C-62). prefix says which kind of call.
func mintJobID(prefix string) string {
	var b [6]byte
	_, _ = crand.Read(b[:]) // cannot fail on this toolchain (Go >= 1.24)
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}

// visionRetryKey carries a *visionRetry from runVideoDescribe's halve-and-retry
// loop to runVisionGen, so the one place that records an overflow defer knows
// whether the caller will retry it (an inner row) or not (the call's row).
type visionRetryKey struct{}

type visionRetry struct {
	canRetry bool   // the attempt in flight will be retried if it overflows
	id       string // the call's job id, minted on the first retried attempt
}
