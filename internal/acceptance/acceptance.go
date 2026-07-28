// Package acceptance answers one question a node must answer BEFORE a job is
// dispatched to it: can THIS process, running as THIS identity, actually do the
// work its config advertises?
//
// It exists because of two live fleet failures on 2026-07-27 that were identical
// in shape and invisible until job time:
//
//   - a node whose fleet-serve ran as a different Windows account than the one
//     that owns the harness's uv virtualenv. The venv's python.exe is a trampoline
//     that re-execs a base interpreter under the OWNER's roaming profile, so the
//     job died with "failed to spawn Python child process — entity not found";
//   - a node whose dispatcher ran as a different Unix user than the one owning the
//     GPU lease directory, so every job failed with "state dir ... is not writable".
//
// Neither is detectable by stat()ing files, which is what `doctor` does — the
// paths existed and were readable. Both are only detectable by ATTEMPTING the
// thing as the running identity: execute the interpreter, write to the lease dir.
// That distinction is the whole point of this package.
package acceptance

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

// Status is a check's outcome.
type Status string

const (
	Pass Status = "PASS"
	Fail Status = "FAIL"
	Skip Status = "SKIP" // not applicable on this machine — never a failure
)

// Check is one verified capability.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
}

// Report is the whole verdict. Ready is false if ANY check failed — a node that
// is not ready must not be handed work.
type Report struct {
	Identity string  `json:"identity"`
	Ready    bool    `json:"ready"`
	Checks   []Check `json:"checks"`
}

// Failures returns just the failed checks (the actionable subset).
func (r Report) Failures() []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Status == Fail {
			out = append(out, c)
		}
	}
	return out
}

// Identity reports the user this process runs as. It is printed on every report
// because both live failures were identity-dependent: the same binary, same
// config and same files, run by the wrong account.
func Identity() string {
	u, err := user.Current()
	if err != nil {
		return "unknown"
	}
	if u.Username != "" {
		return u.Username
	}
	return u.Uid
}

// WritableDir verifies a directory can be WRITTEN by this identity, by writing.
// Permission bits and ownership are not consulted: on Windows they are close to
// meaningless, and on Unix a group/ACL can grant or deny access that a bit-level
// reading of the mode would get wrong. The probe file is removed immediately.
func WritableDir(dir string) Check {
	const name = "gpu lease writable"
	if strings.TrimSpace(dir) == "" {
		return Check{name, Fail, "no lease directory resolved"}
	}
	if err := os.MkdirAll(dir, 0o775); err != nil {
		return Check{name, Fail, fmt.Sprintf("%s: cannot create (%v) — a job would fail the moment it took the GPU", dir, err)}
	}
	probe := filepath.Join(dir, fmt.Sprintf(".acceptance-%d.tmp", os.Getpid()))
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return Check{name, Fail, fmt.Sprintf("%s: not writable as %s (%v) — this is the failure that made every dispatched job die", dir, Identity(), err)}
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return Check{name, Pass, dir + " is writable as " + Identity()}
}

// Runnable verifies an interpreter or binary can be EXECUTED by this identity,
// by executing it. A uv virtualenv's python.exe is a small trampoline that
// re-execs a base interpreter elsewhere on disk; it stats fine for everyone and
// runs only for the account whose profile holds that base. Nothing short of
// running it distinguishes the two.
func Runnable(ctx context.Context, label, bin string, args ...string) Check {
	name := "run " + label
	if strings.TrimSpace(bin) == "" {
		return Check{name, Skip, "not bound on this machine"}
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, args...).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if len(detail) > 200 {
			detail = detail[:200] + "…"
		}
		if detail != "" {
			detail = ": " + detail
		}
		return Check{name, Fail, fmt.Sprintf("%s failed to execute as %s (%v)%s", bin, Identity(), err, detail)}
	}
	first := strings.TrimSpace(string(out))
	if i := strings.IndexAny(first, "\r\n"); i >= 0 {
		first = first[:i]
	}
	if first == "" {
		first = "ran"
	}
	return Check{name, Pass, first}
}

// Add appends checks to a report, keeping Ready in step.
func (r *Report) Add(checks ...Check) {
	for _, c := range checks {
		r.Checks = append(r.Checks, c)
		if c.Status == Fail {
			r.Ready = false
		}
	}
}

// New starts an empty, ready report for the current identity.
func New() *Report { return &Report{Identity: Identity(), Ready: true} }
