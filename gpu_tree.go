package main

// The wrapper's side of process-tree containment (plan P10, ADR 0084).
//
// A wrapper that runs a command under a lease records the tree it started in the lease record,
// so that a reader that finds the wrapper gone can tell whether the job it started is still
// running (a dead wrapper with a live tree keeps its claim, internal/gpulease/tree.go), and it
// stops that tree as a whole when the lease is lost or the operator interrupts it. Everything
// here is the action of the wrapper on its OWN tree: nothing reaches a process the wrapper did
// not start, except to spare it.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/comfyinst"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/proctree"
)

// treeRecorder writes the wrapper's tree into its lease record. record is cheap to call on every
// tick: the record is rewritten (under the epoch lock) only when the set of processes changed.
type treeRecorder struct {
	lease  *gpulease.Lease
	tree   *proctree.Tree
	out    io.Writer
	warned bool
}

func newTreeRecorder(lease *gpulease.Lease, tree *proctree.Tree, out io.Writer) *treeRecorder {
	return &treeRecorder{lease: lease, tree: tree, out: out}
}

// members are the live processes of the command's tree, each with the start identity a reader
// compares: what the containment lists, plus what the process table shows below the command (the
// only source when the command is not contained, and a second opinion on Unix, where a process
// that left the group is still found through its parent chain). The wrapper itself is not a
// member: the record already names it as the holder.
func (r *treeRecorder) members() []gpulease.TreeMember {
	pids := map[int]bool{}
	if listed, err := r.tree.PIDs(); err == nil {
		for _, pid := range listed {
			pids[pid] = true
		}
	}
	if procs, err := gpulease.ProcessTree(r.tree.PID()); err == nil {
		for _, p := range procs {
			pids[p.PID] = true
		}
	}
	pids[r.tree.PID()] = true
	delete(pids, os.Getpid())
	var out []gpulease.TreeMember
	for pid := range pids {
		// A process whose start identity cannot be read has exited (or cannot be told apart from
		// a recycled pid): it is not a member a reader could ever recognise.
		if start, ok := gpulease.ProcessStart(pid); ok {
			out = append(out, gpulease.TreeMember{PID: pid, StartMs: start})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// record stores the tree in the lease record. A failure is said once: it is never a lost lease
// (Renew proves that, separately) and never a reason to stop the command, but it means a reader
// that finds this wrapper gone will not know the job is still running.
func (r *treeRecorder) record() {
	if err := r.lease.RecordTree(r.members()); err != nil {
		if !r.warned {
			r.warned = true
			fmt.Fprintf(r.out, "gpu reserve: could not record the command's process tree in the lease (%v); if this wrapper dies, a reader cannot tell that the job it started is still running\n", err)
		}
		return
	}
	r.warned = false
}

// protectedFromOtherLeases is the set of processes a wrapper that lost its lease must NOT stop: the
// live ComfyUI instances its launch markers tie to another lease, and everything below them. A
// kept instance that a later lease reused was re-stamped to that lease, and killing it would kill
// the job that lease is running on it.
func protectedFromOtherLeases(cfg config.Config, epoch uint64) map[int]bool {
	pids := comfyinst.PIDsOfOtherLeases(cfg.ComfyDir, epoch, gpulease.PIDAlive)
	if len(pids) == 0 {
		return nil
	}
	out := map[int]bool{}
	for _, pid := range pids {
		out[pid] = true
		if procs, err := gpulease.ProcessTree(pid); err == nil {
			for _, p := range procs {
				out[p.PID] = true
			}
		}
	}
	return out
}

// stopWrappedTree stops the command the wrapper started and everything it started: the
// containment's whole tree, minus what another lease took over. A command that runs without a
// containment (its start said so) is stopped, and so is whatever the process table shows below
// it.
func stopWrappedTree(tree *proctree.Tree, cmd *exec.Cmd, cfg config.Config, epoch uint64, out io.Writer) {
	protect := protectedFromOtherLeases(cfg, epoch)
	var left []int
	var err error
	if tree != nil && tree.Contained() {
		left, err = tree.TerminateExcept(protect)
	} else {
		left, err = killBelow(cmd, protect)
	}
	if err != nil {
		fmt.Fprintf(out, "gpu reserve: could not stop every process of the command's tree: %v\n", err)
	}
	if len(left) > 0 {
		parts := make([]string, len(left))
		for i, p := range left {
			parts[i] = fmt.Sprintf("pid %d", p)
		}
		fmt.Fprintf(out, "gpu reserve: left running %s: a ComfyUI instance this lease kept, which another lease took over and is running a job on, so it is that lease's now\n", strings.Join(parts, ", "))
	}
}

// killBelow is the uncontained fallback: the command, then what the process table shows below it,
// except what is protected.
func killBelow(cmd *exec.Cmd, protect map[int]bool) (left []int, err error) {
	if procs, perr := gpulease.ProcessTree(cmd.Process.Pid); perr == nil {
		for _, p := range procs {
			if p.PID == cmd.Process.Pid || p.PID == os.Getpid() {
				continue
			}
			if protect[p.PID] {
				left = append(left, p.PID)
				continue
			}
			if kerr := proctree.Kill(p.PID); kerr != nil && err == nil {
				err = kerr
			}
		}
	}
	if kerr := cmd.Process.Kill(); kerr != nil && err == nil && !isProcessDone(kerr) {
		err = kerr
	}
	return left, err
}

func isProcessDone(err error) bool {
	return err != nil && strings.Contains(err.Error(), "process already finished")
}
