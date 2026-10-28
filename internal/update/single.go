package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"llamaswap-pp-cli/pkg/llamaswap"

	"github.com/dmmdea/offload-harness/internal/nodeswap"
)

// A single-file component (llama-swap: the live path is the executable itself) is swapped through nodeswap: hash
// verify, idle wait, stop of the daemon, swap, restart of the consumer, verify, automatic rollback. layoutOf
// (layout.go) decides how the swap is made, once:
//
//	layoutFile  a hand layout: the file is backed up by rename (<live>.bak-<id>, the previous generation) and replaced.
//	layoutGens  the installer's layout: <dir> is a link to <dir>-versions/<id>/; the new build is staged into its own
//	            write-once directory and the link is flipped, the previous generation stays where it was.
//
// Everything around the swap (preflight, staging, self-check, gate, drain, stop, restart, /running and -version
// verification, rollback on failure) is the same code for both.

// restartSpec says how the llama-swap consumer is restarted: config first, else the box's own mechanism (the
// scheduled task on Windows, the systemd unit elsewhere).
func (e *Env) restartSpec(def *compDef) RestartSpec {
	if s, ok := e.restart[def.Name]; ok {
		return s // the mechanism prepareRestart settled on
	}
	if s, ok := e.Config.Restart[def.Name]; ok {
		return s
	}
	if runtime.GOOS == "windows" {
		return RestartSpec{Task: defaultWindowsTask}
	}
	return RestartSpec{Command: "systemctl restart " + defaultSystemdUnit}
}

var idSafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func (e *Env) backupSuffix(live, id string) string {
	s := idSafe.ReplaceAllString(id, "_")
	if s == "" {
		s = "previous"
	}
	if exists(live + ".bak-" + s) {
		s += "-" + e.Now().Format("20060102-150405")
	}
	return s
}

func (e *Env) swapPlan(def *compDef, live, staged, sha, suffix string) nodeswap.Plan {
	spec := e.restartSpec(def)
	p := nodeswap.Plan{Staged: staged, Target: live, ExpectedSHA256: sha, BackupSuffix: suffix,
		ProcessMatch: def.Binary, WaitIdleTimeout: 3 * time.Minute, VerifyTimeout: 90 * time.Second}
	if !spec.None {
		p.RestartTaskName, p.RestartCommand = spec.Task, spec.Command
		if p.RestartCommand != "" {
			p.RestartTaskName = ""
		}
	}
	return p
}

func (e *Env) swapLogger() *nodeswap.Logger {
	return nodeswap.NewLogger(func(line string) { e.logf("  %s", line) })
}

// drainAll unloads every running model (with drain) before the daemon restarts. busy is true when a model stays
// busy past the wait: the swap then does not start.
func (e *Env) drainAll(ctx context.Context, def *compDef) (note string, busy bool, err error) {
	if e.Endpoint == "" || e.restartSpec(def).None {
		return "", false, nil
	}
	c := Consumer{Endpoint: e.Endpoint}
	// A seat that is still starting cannot be drained yet (its slots do not answer): wait for it, a few times.
	var un []string
	for attempt := 0; attempt < 8; attempt++ {
		un, err = c.UnloadAll(ctx, 5*time.Minute)
		if err == nil || !(errors.Is(err, llamaswap.ErrDrainUnobservable) || errors.Is(err, llamaswap.ErrDrainTimeout)) {
			break
		}
		e.logf("  a seat is not drainable yet (%v); waiting", truncate(err.Error(), 120))
		select {
		case <-ctx.Done():
		case <-time.After(15 * time.Second):
		}
	}
	if err != nil {
		if errors.Is(err, llamaswap.ErrUnreachable) {
			return "llama-swap not answering; nothing to drain", false, nil
		}
		if errors.Is(err, llamaswap.ErrDrainTimeout) || errors.Is(err, llamaswap.ErrDrainUnobservable) {
			return "", true, err
		}
		return "", false, err
	}
	if len(un) > 0 {
		return "drained and unloaded before the restart: " + strings.Join(un, ", "), false, nil
	}
	return "", false, nil
}

// firstFailure is the first step of an outcome that did not succeed (the cause; later ones are the rollback's).
func firstFailure(o nodeswap.Outcome) string {
	for _, s := range o.Steps {
		if !s.OK {
			return s.Name
		}
	}
	return ""
}

func (e *Env) applySingle(ctx context.Context, def *compDef, st *State, cand *Candidate, live string, pre *preGen) result {
	// The daemon is only drained and stopped when it can be started again: nothing is touched, not even a folder,
	// on a node whose restart mechanism is not there.
	if missing := e.prepareRestart(ctx, def, live); missing != "" {
		return skipped(def.Name, missing)
	}
	lay := e.layoutOf(def, live)
	vroot := lay.Vroot
	if err := os.MkdirAll(vroot, 0o755); err != nil {
		return result{"error", err.Error(), ExitError, nil}
	}
	curID := "unknown-" + e.Now().Format("20060102")
	if st.Current != nil {
		curID = st.Current.ID
	}
	// Refuse before downloading what the swap would refuse before touching anything (an elevated holder it cannot read).
	if err := nodeswap.Preflight(e.swapPlan(def, live, "preflight", "preflight", "preflight"), e.Deps); err != nil {
		return result{"error", err.Error(), ExitError, nil}
	}
	e.cleanStaging(vroot)
	stage := filepath.Join(vroot, ".staging", cand.ID+"."+randHex())
	gen := filepath.Join(stage, "gen")
	defer func() { _ = os.RemoveAll(stage) }()

	var fe *fetched
	var staged string
	if pre != nil {
		e.logf("%s: promoting %s, staged by the installer (no download)", def.Name, cand.ID)
		fe = &fetched{size: pre.Size, attestation: "the generation's meta file carries the publisher's digests (verified by the installer)"}
		staged = findFile(pre.Dir, def.exeName())
	} else {
		e.logf("%s: staging %s", def.Name, cand.ID)
		var err error
		if fe, err = e.fetch(ctx, cand, filepath.Join(stage, "dl")); err != nil {
			return result{"error", "download or digest check failed, nothing installed: " + err.Error(), ExitError, nil}
		}
		if err = e.extractAll(fe.files, gen, stage); err != nil {
			return e.rejected(st, cand, "extract: "+err.Error())
		}
		staged = findFile(gen, def.exeName())
	}
	if rej, inc := e.attestationRatchet(st, vroot, fe); rej != "" {
		return e.rejected(st, cand, rej)
	} else if inc != "" {
		return result{"inconclusive", "nothing changed, will retry: " + inc, ExitOK, nil}
	}
	if staged == "" {
		return e.rejected(st, cand, def.exeName()+" is not in the archive")
	}
	notes, err := SelfCheck(ctx, def, filepath.Dir(staged), "", live)
	if err != nil {
		return e.rejected(st, cand, err.Error())
	}
	if n := attestationNote(fe); n != "" {
		notes = append(notes, n)
	}
	g := e.Gate.Run(ctx, GateInput{Component: def.Name, Candidate: cand, StagedDir: filepath.Dir(staged), PreviousDir: filepath.Dir(live), Live: live})
	switch g.Verdict {
	case GateReject:
		return e.rejected(st, cand, "gate: "+g.Reason)
	case GateInconclusive:
		return result{"inconclusive", "gate could not decide, nothing changed; will retry: " + g.Reason, ExitOK, notes}
	}
	sha, err := sha256File(staged)
	if err != nil {
		return result{"error", err.Error(), ExitError, notes}
	}
	if note, busy, derr := e.drainAll(ctx, def); busy {
		return result{"inconclusive", "a seat is still busy; nothing changed, will retry: " + derr.Error(), ExitOK, notes}
	} else if derr != nil {
		return result{"error", "draining llama-swap: " + derr.Error(), ExitError, notes}
	} else if note != "" {
		notes = append(notes, note)
	}
	if lay.Kind == layoutGens {
		return e.applySingleGens(ctx, def, st, cand, live, lay, gen, staged, pre, fe, curID, notes, g, sha)
	}

	plan := e.swapPlan(def, live, staged, sha, e.backupSuffix(live, curID))
	out := nodeswap.Run(ctx, plan, e.Deps, e.swapLogger())
	if !out.OK {
		return e.singleFailure(st, cand, out, notes)
	}
	note, verr := e.verifyLive(ctx, def, live, cand.ID)
	if verr != nil {
		return e.restoreSingle(ctx, def, st, cand, live, out.BackupPath, out.OldSHA256, "after the restart "+verr.Error(), notes)
	}
	if note != "" {
		notes = append(notes, note)
	}
	e.rotateSingle(def, st, cand, live, out, fe, curID, lay.Vroot, notes, g)
	return result{"updated", fmt.Sprintf("%s swapped %s -> %s (previous kept as %s)", filepath.Base(live), curID, cand.ID, filepath.Base(out.BackupPath)), ExitOK, notes}
}

// verifyLive is the check after a swap or a flip, the same for both layouts: llama-swap answers /running again
// (when a consumer is configured) and the live executable reports the expected build through its own -version.
func (e *Env) verifyLive(ctx context.Context, def *compDef, live, wantID string) (note string, err error) {
	if e.Endpoint != "" && !e.restartSpec(def).None {
		if rerr := (Consumer{Endpoint: e.Endpoint}).Ready(ctx, 60*time.Second); rerr != nil {
			return "", rerr
		}
		note = "llama-swap answers /running on the new build"
	}
	id, ok, detail := Probe(ctx, def, filepath.Dir(live))
	switch {
	case !ok:
		return note, fmt.Errorf("the live executable does not answer -version: %s", detail)
	case id != "" && def.IDProbe != nil && wantID != "" && !strings.HasPrefix(wantID, "unknown") && cmpID(id, wantID) != 0:
		return note, fmt.Errorf("the live executable reports %s, expected %s", id, wantID)
	}
	return note, nil
}

func (e *Env) rotateSingle(def *compDef, st *State, cand *Candidate, live string, out nodeswap.Outcome, fe *fetched, curID, vroot string, notes []string, g GateResult) {
	now := nowStamp(e.Now())
	oldPrev := st.Previous
	prev := &Generation{ID: curID, Path: out.BackupPath, FileSHA256: out.OldSHA256, InstalledAt: now}
	if st.Current != nil {
		prev.Digest, prev.Source, prev.Size, prev.Asset = st.Current.Digest, st.Current.Source, st.Current.Size, st.Current.Asset
	}
	st.Previous = prev
	st.Current = &Generation{ID: cand.ID, Path: live, Digest: cand.Assets[0].Digest, Size: fe.size, Source: cand.Source,
		InstalledAt: now, Asset: cand.Assets[0].Name, FileSHA256: out.NewSHA256}
	// The generation before the previous one is no longer kept: only the file this package made, beside the live file.
	if oldPrev != nil && oldPrev.Path != "" && oldPrev.Path != out.BackupPath && filepath.Dir(oldPrev.Path) == filepath.Dir(live) &&
		strings.HasPrefix(filepath.Base(oldPrev.Path), filepath.Base(live)+".bak-") {
		if err := os.Remove(oldPrev.Path); err != nil && !os.IsNotExist(err) {
			e.logf("the older backup %s stays (%v)", oldPrev.Path, err)
		}
	}
	writeMeta(vroot, meta{ID: cand.ID, Source: cand.Source, Assets: cand.Assets, Attestation: fe.attestation, Size: fe.size,
		InstalledAt: now, Checks: notes, Gate: e.Gate.Name(), GateNumbers: g.Numbers})
	_ = os.RemoveAll(filepath.Join(vroot, ".staging")) // single-file generations live beside the file; only the meta files stay here
}

// singleFailure reads a failed nodeswap outcome: a wait that timed out is "try again later", a refusal before
// anything moved is an error, a candidate that did not come up (and was rolled back) is rejected.
func (e *Env) singleFailure(st *State, cand *Candidate, out nodeswap.Outcome, notes []string) result {
	step := firstFailure(out)
	switch {
	case step == "wait-idle":
		return result{"inconclusive", "the node did not go idle in time, nothing changed; will retry: " + out.Error, ExitOK, notes}
	case out.RolledBack && out.RollbackOK && (step == "restart-node" || step == "verify-running"):
		e.reject(st, cand.ID, out.Error)
		return result{"rolled-back", "the new build did not come up; the previous one is running again: " + out.Error, ExitRejected, notes}
	case out.RolledBack && !out.RollbackOK:
		return result{"error", "swap failed (" + out.Error + ") and the rollback did not verify: CHECK THE NODE", ExitError, notes}
	}
	return result{"error", fmt.Sprintf("swap stopped at %s, previous build in place: %s", step, out.Error), ExitError, notes}
}

// restoreSingle puts the backup back after a swap that nodeswap called good but the consumer then failed to serve.
func (e *Env) restoreSingle(ctx context.Context, def *compDef, st *State, cand *Candidate, live, backup, backupSHA, why string, notes []string) result {
	plan := e.swapPlan(def, live, backup, backupSHA, e.backupSuffix(live, "failed-"+cand.ID))
	out := nodeswap.Run(ctx, plan, e.Deps, e.swapLogger())
	if !out.OK {
		return result{"error", "the new build failed (" + why + ") and putting the previous one back also failed: " + out.Error + ": CHECK THE NODE", ExitError, notes}
	}
	if out.BackupPath != "" {
		_ = os.Remove(out.BackupPath) // the rejected build is deleted at once
	}
	e.reject(st, cand.ID, why)
	return result{"rolled-back", "the new build was rolled back: " + why, ExitRejected, notes}
}

func (e *Env) rollbackSingle(ctx context.Context, def *compDef, st *State, live string) result {
	if missing := e.prepareRestart(ctx, def, live); missing != "" {
		return skipped(def.Name, missing)
	}
	if lay := e.layoutOf(def, live); lay.Kind == layoutGens {
		return e.rollbackSingleGens(ctx, def, st, live, lay)
	}
	prev, cur := st.Previous, st.Current
	if prev.Path == "" || !exists(prev.Path) {
		return result{"error", "the previous build " + prev.Path + " is gone", ExitError, nil}
	}
	sha := prev.FileSHA256
	if sha == "" {
		var err error
		if sha, err = sha256File(prev.Path); err != nil {
			return result{"error", err.Error(), ExitError, nil}
		}
	}
	curID := "unknown-" + e.Now().Format("20060102")
	if cur != nil {
		curID = cur.ID
	}
	if err := nodeswap.Preflight(e.swapPlan(def, live, "preflight", "preflight", "preflight"), e.Deps); err != nil {
		return result{"error", err.Error(), ExitError, nil}
	}
	var notes []string
	if note, busy, derr := e.drainAll(ctx, def); busy {
		return result{"inconclusive", "a seat is still busy; nothing changed: " + derr.Error(), ExitOK, nil}
	} else if derr != nil {
		return result{"error", "draining llama-swap: " + derr.Error(), ExitError, nil}
	} else if note != "" {
		notes = append(notes, note)
	}
	out := nodeswap.Run(ctx, e.swapPlan(def, live, prev.Path, sha, e.backupSuffix(live, curID)), e.Deps, e.swapLogger())
	if !out.OK {
		return result{"error", "rollback stopped at " + firstFailure(out) + ": " + out.Error, ExitError, notes}
	}
	if note, verr := e.verifyLive(ctx, def, live, prev.ID); verr != nil {
		return result{"error", "rolled back, but " + verr.Error(), ExitError, notes}
	} else if note != "" {
		notes = append(notes, note)
	}
	now := nowStamp(e.Now())
	newPrev := &Generation{ID: curID, Path: out.BackupPath, FileSHA256: out.OldSHA256, InstalledAt: now}
	if cur != nil {
		newPrev.Digest, newPrev.Source, newPrev.Size, newPrev.Asset = cur.Digest, cur.Source, cur.Size, cur.Asset
	}
	restored := *prev
	restored.Path, restored.FileSHA256 = live, out.NewSHA256
	st.Current, st.Previous, st.ETag = &restored, newPrev, ""
	return result{"rollback", fmt.Sprintf("%s is %s again (the build it replaced is kept as %s)", filepath.Base(live), restored.ID, filepath.Base(out.BackupPath)), ExitOK, notes}
}

// applySingleGens promotes a staged llama-swap build as a generation: the build moves into <vroot>/<id> (write-once),
// the link at <dir> is flipped by nodeswap.Run (which stops the daemon, restarts it and verifies the image), and the
// previous generation stays where it was (a plain directory is migrated into <vroot>/<previous id> at the flip).
func (e *Env) applySingleGens(ctx context.Context, def *compDef, st *State, cand *Candidate, live string, lay layout,
	gen, staged string, pre *preGen, fe *fetched, curID string, notes []string, g GateResult, sha string) result {
	vroot, exeName := lay.Vroot, def.exeName()
	oldTarget, _ := resolveLink(lay.Dir)
	finalDir := filepath.Join(vroot, cand.ID)
	if pre != nil {
		finalDir = pre.Dir
	} else {
		if filepath.Clean(filepath.Dir(staged)) != filepath.Clean(gen) {
			return e.rejected(st, cand, exeName+" is not at the root of the archive")
		}
		if exists(finalDir) {
			if filepath.Clean(finalDir) == oldTarget {
				return result{"error", finalDir + " already is the live generation", ExitError, notes}
			}
			_ = os.RemoveAll(finalDir)
		}
		if err := os.Rename(gen, finalDir); err != nil {
			return result{"error", "promoting the staged build: " + err.Error(), ExitError, notes}
		}
		writeMeta(vroot, meta{ID: cand.ID, Source: cand.Source, Assets: cand.Assets, Attestation: fe.attestation, Size: fe.size,
			InstalledAt: nowStamp(e.Now()), Checks: notes, Gate: e.Gate.Name(), GateNumbers: g.Numbers})
	}
	newExe := filepath.Join(finalDir, exeName)
	if !exists(newExe) {
		return result{"error", newExe + " is not there: the staged generation does not hold " + exeName + " at its root", ExitError, notes}
	}
	// The generation is dropped again when the link ends up elsewhere: the rejected build is deleted at once.
	drop := func() {
		cur, isLink := resolveLink(lay.Dir)
		if (isLink && cur != filepath.Clean(finalDir)) || (!isLink && exists(live)) {
			_ = os.RemoveAll(finalDir)
			_ = os.Remove(filepath.Join(vroot, filepath.Base(finalDir)+".meta.json"))
		}
	}

	l := newGenLink(lay.Dir, exeName, vroot, curID, oldTarget, finalDir)
	plan := e.swapPlan(def, live, newExe, sha, "")
	plan.Generation = l.g
	out := nodeswap.Run(ctx, plan, e.Deps, e.swapLogger())
	if !out.OK {
		res := e.singleFailure(st, cand, out, notes)
		drop()
		return res
	}
	note, verr := e.verifyLive(ctx, def, live, cand.ID)
	if verr != nil {
		return e.restoreGens(ctx, def, st, cand, live, lay, l, finalDir, "after the restart "+verr.Error(), notes, drop)
	}
	if note != "" {
		notes = append(notes, note)
	}

	now := nowStamp(e.Now())
	prior := l.PriorDir()
	if l.migrated != "" && !exists(l.migrated+".meta.json") {
		writeMeta(vroot, meta{ID: filepath.Base(l.migrated), Tag: curID, Source: "migrated from the live directory", InstalledAt: now})
	}
	prev := &Generation{ID: curID, Path: prior, FileSHA256: out.OldSHA256, InstalledAt: now}
	if st.Current != nil {
		prev.Digest, prev.Source, prev.Size, prev.Asset = st.Current.Digest, st.Current.Source, st.Current.Size, st.Current.Asset
	}
	st.Previous = prev
	st.Current = &Generation{ID: cand.ID, Path: finalDir, Digest: cand.Assets[0].Digest, Size: fe.size, Source: cand.Source,
		InstalledAt: now, Asset: cand.Assets[0].Name, FileSHA256: out.NewSHA256}
	e.prune(vroot, lay.Dir, st)
	return result{"updated", fmt.Sprintf("%s now points at %s (%s -> %s, previous generation kept in %s)", lay.Dir, finalDir, curID, cand.ID, prior), ExitOK, notes}
}

// restoreGens flips the link back to the previous generation after a build that nodeswap called good but llama-swap
// then failed to serve, and rejects the build.
func (e *Env) restoreGens(ctx context.Context, def *compDef, st *State, cand *Candidate, live string, lay layout, l *genLink,
	finalDir, why string, notes []string, drop func()) result {
	exeName := def.exeName()
	priorExe := filepath.Join(l.PriorDir(), exeName)
	sha, err := sha256File(priorExe)
	if err != nil {
		return result{"error", "the new build failed (" + why + ") and the previous one cannot be read: " + err.Error() + ": CHECK THE NODE", ExitError, notes}
	}
	back := newGenLink(lay.Dir, exeName, lay.Vroot, "", finalDir, l.PriorDir())
	plan := e.swapPlan(def, live, priorExe, sha, "")
	plan.Generation = back.g
	if out := nodeswap.Run(ctx, plan, e.Deps, e.swapLogger()); !out.OK {
		return result{"error", "the new build failed (" + why + ") and putting the previous one back also failed: " + out.Error + ": CHECK THE NODE", ExitError, notes}
	}
	drop()
	e.reject(st, cand.ID, why)
	return result{"rolled-back", "the new build was rolled back: " + why, ExitRejected, notes}
}

// rollbackSingleGens flips the link back to the previous generation (an operator's decision: nothing is rejected);
// the generation it leaves becomes the previous one, so a second rollback undoes the first.
func (e *Env) rollbackSingleGens(ctx context.Context, def *compDef, st *State, live string, lay layout) result {
	prev, cur := st.Previous, st.Current
	exeName := def.exeName()
	prevExe := filepath.Join(prev.Path, exeName)
	if prev.Path == "" || !exists(prevExe) {
		return result{"error", "the previous generation " + prev.Path + " is gone", ExitError, nil}
	}
	curDir, isLink := resolveLink(lay.Dir)
	if !isLink {
		return result{"error", lay.Dir + " is not a link to a generation: nothing to flip back", ExitError, nil}
	}
	sha, err := sha256File(prevExe)
	if err != nil {
		return result{"error", err.Error(), ExitError, nil}
	}
	if err := nodeswap.Preflight(e.swapPlan(def, live, "preflight", "preflight", "preflight"), e.Deps); err != nil {
		return result{"error", err.Error(), ExitError, nil}
	}
	var notes []string
	if note, busy, derr := e.drainAll(ctx, def); busy {
		return result{"inconclusive", "a seat is still busy; nothing changed: " + derr.Error(), ExitOK, nil}
	} else if derr != nil {
		return result{"error", "draining llama-swap: " + derr.Error(), ExitError, nil}
	} else if note != "" {
		notes = append(notes, note)
	}
	l := newGenLink(lay.Dir, exeName, lay.Vroot, "", curDir, prev.Path)
	plan := e.swapPlan(def, live, prevExe, sha, "")
	plan.Generation = l.g
	out := nodeswap.Run(ctx, plan, e.Deps, e.swapLogger())
	if !out.OK {
		return result{"error", "rollback stopped at " + firstFailure(out) + ": " + out.Error, ExitError, notes}
	}
	if note, verr := e.verifyLive(ctx, def, live, prev.ID); verr != nil {
		return result{"error", "rolled back, but " + verr.Error(), ExitError, notes}
	} else if note != "" {
		notes = append(notes, note)
	}
	now := nowStamp(e.Now())
	newPrev := &Generation{ID: "unknown-" + e.Now().Format("20060102"), Path: curDir, FileSHA256: out.OldSHA256, InstalledAt: now}
	if cur != nil {
		c := *cur
		c.Path, c.FileSHA256 = curDir, out.OldSHA256
		newPrev = &c
	}
	restored := *prev
	restored.FileSHA256 = out.NewSHA256
	st.Current, st.Previous, st.ETag = &restored, newPrev, ""
	return result{"rollback", fmt.Sprintf("%s now points at %s (%s again; the build it replaced is kept as %s)", lay.Dir, prev.Path, restored.ID, newPrev.ID), ExitOK, notes}
}
