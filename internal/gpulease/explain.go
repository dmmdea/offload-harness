package gpulease

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// explain.go — the sentence a WAITER reads when the lease it is queued behind is not a
// healthy one: which lease, what is wrong with it, for how long, what is running, and the
// exact command that frees it. A refusal that says only "held by pid N" sends the reader
// away; one that says "held by a lease whose owner has been gone for 40 minutes, take it
// over with ..." lets a human decide in one read. It is information: nothing here, or
// anywhere in the lease code, releases or kills a lease on its own.

// defaultOrphanGrace is the process-wide grace (config gpu_orphan_grace_min, armed from
// config.Load next to the lease directory); zero means DefaultOrphanGrace.
var defaultOrphanGrace atomic.Int64

// SetDefaultOrphanGrace installs the operator's orphan grace for every Standing computed
// with a zero grace. Zero or negative restores DefaultOrphanGrace.
func SetDefaultOrphanGrace(d time.Duration) {
	if d <= 0 {
		d = 0
	}
	defaultOrphanGrace.Store(int64(d))
}

// OrphanGrace is the grace an attended lease's owner is given before the lease reads as
// orphaned: the installed one, else DefaultOrphanGrace.
func OrphanGrace() time.Duration { return orphanGraceOrDefault(0) }

// orphanGraceOrDefault resolves a grace argument: explicit, else the installed one, else
// DefaultOrphanGrace.
func orphanGraceOrDefault(grace time.Duration) time.Duration {
	if grace > 0 {
		return grace
	}
	if d := time.Duration(defaultOrphanGrace.Load()); d > 0 {
		return d
	}
	return DefaultOrphanGrace
}

// Escalation is the standing verdict word of a lease that is not healthy, in precedence
// order (stalled, then orphaned, then overdue), or "" for a lease that is. The words are
// the verdicts `gpu status` prints (held-stalled and so on).
func (st Standing) Escalation() string {
	switch {
	case st.Stalled:
		return "held-stalled"
	case st.Orphaned:
		return "held-orphaned"
	case st.Overdue:
		return "held-overdue"
	}
	return ""
}

// TakeoverCommand is the command that frees a lease whose holder will not, for epoch.
func TakeoverCommand(epoch uint64) string {
	return fmt.Sprintf("local-offload gpu takeover --epoch %d", epoch)
}

// ExplainHeld describes, for a waiter, why the lease info is not a healthy one, or returns
// "" when it is (or when nothing can be said). grace <= 0 means the installed default.
// It is READ-ONLY: it reads the orphan marker a status surface recorded and never writes
// one or takes the epoch lock, so it is safe wherever a refusal is formatted (the text
// gate, an error's Error()). A lease whose owner is gone but that no status surface has
// looked at yet reads as inside its grace, so it is explained only once something recorded
// the moment; a stalled or overdue lease needs no marker and is explained at once.
func ExplainHeld(leaseDir string, info Info, grace time.Duration) string {
	if !info.Held || info.Epoch == 0 {
		return ""
	}
	return ObserverAt(leaseDir).ExplainHeld(info, grace)
}

// ExplainHeld is ExplainHeld on a Manager (its clock and process table).
func (m *Manager) ExplainHeld(info Info, grace time.Duration) string {
	if !info.Held || info.Epoch == 0 {
		return ""
	}
	grace = orphanGraceOrDefault(grace)
	st := m.StandingReadOnly(info, grace)
	word := st.Escalation()
	if word == "" {
		return ""
	}
	now := m.now()
	var why string
	switch word {
	case "held-stalled":
		why = fmt.Sprintf("its progress file %s showed no activity for %s, past its %s stall window",
			filepath.Base(st.Progress.File), roundDur(st.Progress.Age), roundDur(st.Progress.Stall))
		if st.Progress.Detail != "" {
			why += " (last line: " + st.Progress.Detail + ")"
		}
	case "held-orphaned":
		who := "the process that asked for it"
		switch {
		case st.OwnerSession != "":
			who = "session " + st.OwnerSession
		case info.Owner != nil && info.Owner.PID > 0:
			who = fmt.Sprintf("process %d", info.Owner.PID)
		}
		why = fmt.Sprintf("its owner (%s) has been gone for %s, past the %s grace", who, roundDur(now.Sub(st.OrphanedSince)), roundDur(grace))
	case "held-overdue":
		why = fmt.Sprintf("it is past its declared window by %s and its holder is still renewing", roundDur(st.OverdueBy))
		if info.Expired {
			// The holder's own tick found the term ended and not renewable (plan P9) and said why.
			reason := info.ExpiredWhy
			if reason == "" {
				reason = "nothing vouched for it"
			}
			why = fmt.Sprintf("it has expired: its term ended %s ago and was not renewed because %s, and its holder is still heartbeating", roundDur(st.OverdueBy), reason)
		}
	}
	s := fmt.Sprintf("lease epoch %d is %s: %s; held %s", info.Epoch, word, why, roundDur(info.Age))
	if c := strings.Join(strings.Fields(info.Command), " "); c != "" {
		if r := []rune(c); len(r) > 100 {
			c = string(r[:99]) + "…"
		}
		s += "; running: " + c
	}
	return s + fmt.Sprintf("; nothing reclaims or kills it: a human-authorised session frees it with `%s` (not in this build yet: until it ships, ask whoever owns it or the operator)", TakeoverCommand(info.Epoch))
}

func roundDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}
