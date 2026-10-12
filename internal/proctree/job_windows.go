//go:build windows

package proctree

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// supportsDieWithWrapper: a job object created with KILL_ON_JOB_CLOSE ends its processes when the
// last handle to it closes, which is what happens when the wrapper exits or dies.
const supportsDieWithWrapper = true

// Job is a Windows job object: a set of processes the OS enumerates and stops as one. It is the
// primitive both the lease wrapper's containment (below) and the agent shell's cage
// (internal/sandbox) are built on.
type Job struct{ h windows.Handle }

// JobLimits are the limits a job is created with. The zero value is a job with none: a plain set
// of processes that can be listed and terminated.
type JobLimits struct {
	// KillOnClose ends every process in the job when the last handle to it closes.
	KillOnClose bool
	// ActiveProcessLimit caps how many processes the job may hold at once (0 = no cap).
	ActiveProcessLimit uint32
	// ProcessMemoryLimit caps each process's committed memory in bytes (0 = no cap).
	ProcessMemoryLimit uintptr
}

// NewJob creates a job object with the limits.
func NewJob(l JobLimits) (*Job, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if l.KillOnClose {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	}
	if l.ActiveProcessLimit > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		info.BasicLimitInformation.ActiveProcessLimit = l.ActiveProcessLimit
	}
	if l.ProcessMemoryLimit > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY
		info.ProcessMemoryLimit = l.ProcessMemoryLimit
	}
	if info.BasicLimitInformation.LimitFlags != 0 {
		if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
			_ = windows.CloseHandle(h)
			return nil, fmt.Errorf("SetInformationJobObject: %w", err)
		}
	}
	return &Job{h: h}, nil
}

// assignProcess is AssignProcessToJobObject, a seam so a test can make the assignment fail.
var assignProcess = windows.AssignProcessToJobObject

// Assign puts a process (a handle with PROCESS_SET_QUOTA and PROCESS_TERMINATE) into the job.
// Its descendants join the job from then on.
func (j *Job) Assign(process windows.Handle) error {
	if err := assignProcess(j.h, process); err != nil {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

// Terminate ends every process in the job with exitCode.
func (j *Job) Terminate(exitCode uint32) error {
	if err := windows.TerminateJobObject(j.h, exitCode); err != nil {
		return fmt.Errorf("TerminateJobObject: %w", err)
	}
	return nil
}

// Close closes the handle. Processes still in the job keep running unless the job was created
// with KillOnClose.
func (j *Job) Close() error {
	if j == nil || j.h == 0 {
		return nil
	}
	h := j.h
	j.h = 0
	return windows.CloseHandle(h)
}

// KillsOnClose reports whether the job ends its processes when its last handle closes.
func (j *Job) KillsOnClose() (bool, error) {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	var n uint32
	if err := windows.QueryInformationJobObject(j.h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), &n); err != nil {
		return false, fmt.Errorf("QueryInformationJobObject: %w", err)
	}
	return info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE != 0, nil
}

// DisarmKillOnClose clears KILL_ON_JOB_CLOSE, so that closing the last handle leaves the job's processes running. It
// reads the job's current extended limits and writes them back without the one flag, so any other limit stays. A job
// that kills on close and has to let a member live (a process another lease took over) is disarmed first and closed
// after; the members that must die are ended by TerminateExcept before.
func (j *Job) DisarmKillOnClose() error {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	var n uint32
	if err := windows.QueryInformationJobObject(j.h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), &n); err != nil {
		return fmt.Errorf("QueryInformationJobObject: %w", err)
	}
	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 {
		return nil
	}
	info.BasicLimitInformation.LimitFlags &^= windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(j.h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return nil
}

// PIDs lists the processes in the job.
func (j *Job) PIDs() ([]int, error) {
	const header = 8 // NumberOfAssignedProcesses, NumberOfProcessIdsInList (two DWORDs)
	word := int(unsafe.Sizeof(uintptr(0)))
	n := 64
	for tries := 0; tries < 8; tries++ {
		buf := make([]byte, header+n*word)
		var ret uint32
		err := windows.QueryInformationJobObject(j.h, windows.JobObjectBasicProcessIdList,
			uintptr(unsafe.Pointer(&buf[0])), uint32(len(buf)), &ret)
		assigned := int(*(*uint32)(unsafe.Pointer(&buf[0])))
		inList := int(*(*uint32)(unsafe.Pointer(&buf[4])))
		if err != nil && err != windows.ERROR_MORE_DATA {
			return nil, fmt.Errorf("QueryInformationJobObject: %w", err)
		}
		if err == windows.ERROR_MORE_DATA || inList < assigned {
			n = assigned + 16
			continue
		}
		out := make([]int, 0, inList)
		for i := 0; i < inList; i++ {
			out = append(out, int(*(*uintptr)(unsafe.Pointer(&buf[header+i*word]))))
		}
		return out, nil
	}
	return nil, fmt.Errorf("QueryInformationJobObject: the process list kept growing")
}

// platform is the Windows containment of one Tree.
type platform struct {
	job *Job
}

func (p *platform) contained() bool { return p.job != nil }

func (p *platform) mechanism() string {
	if p.job != nil {
		return "job object"
	}
	return "none"
}

var (
	ntdll            = windows.NewLazySystemDLL("ntdll.dll")
	procNtResumeProc = ntdll.NewProc("NtResumeProcess")
)

// resumeProcess resumes every thread of a process created CREATE_SUSPENDED, whose primary thread
// the standard library does not hand back. NtResumeProcess is the one call that needs only the
// process handle.
func resumeProcess(h windows.Handle) error {
	if err := procNtResumeProc.Find(); err != nil {
		return err
	}
	if r, _, _ := procNtResumeProc.Call(uintptr(h)); r != 0 {
		return fmt.Errorf("NtResumeProcess: status 0x%x", r)
	}
	return nil
}

func start(cmd *exec.Cmd, opts Options) (*Tree, error) {
	t := &Tree{cmd: cmd}
	job, jerr := NewJob(JobLimits{KillOnClose: opts.DieWithWrapper})
	if jerr != nil {
		if opts.DieWithWrapper {
			return nil, fmt.Errorf("proctree: %w", jerr)
		}
		t.warning = "the job object could not be created (" + jerr.Error() + ")"
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return t, nil
	}
	// Suspended, so the child is inside the job before it can run and spawn anything.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	if err := cmd.Start(); err != nil {
		_ = job.Close()
		return nil, err
	}
	const access = windows.PROCESS_SET_QUOTA | windows.PROCESS_TERMINATE | windows.PROCESS_SUSPEND_RESUME
	h, err := windows.OpenProcess(access, false, uint32(cmd.Process.Pid))
	if err != nil {
		// A suspended child that cannot be resumed is a hung job: stop it and say so.
		_ = cmd.Process.Kill()
		_ = job.Close()
		return nil, fmt.Errorf("proctree: the suspended command (pid %d) could not be opened to resume it: %w", cmd.Process.Pid, err)
	}
	defer windows.CloseHandle(h)
	if aerr := job.Assign(h); aerr != nil {
		if opts.DieWithWrapper {
			// The caller asked for a guarantee this command cannot get: a refusal, as when the job could not be created,
			// not a render that outlives the wrapper while the operator believes it dies with it.
			_ = cmd.Process.Kill()
			_ = job.Close()
			return nil, fmt.Errorf("proctree: the command (pid %d) could not be put in a job object, so --die-with-wrapper cannot be honoured: %w", cmd.Process.Pid, aerr)
		}
		// Run it uncontained rather than not at all.
		t.warning ="the command could not be put in a job object (" + aerr.Error() + "); it runs uncontained"
		_ = job.Close()
	} else {
		t.p.job = job
	}
	if rerr := resumeProcess(h); rerr != nil {
		_ = cmd.Process.Kill()
		_ = t.p.close()
		return nil, fmt.Errorf("proctree: the command (pid %d) could not be resumed: %w", cmd.Process.Pid, rerr)
	}
	return t, nil
}

func (p *platform) pids(t *Tree) ([]int, error) {
	if p.job == nil {
		return nil, ErrNotContained
	}
	return p.job.PIDs()
}

func (p *platform) terminate(t *Tree) error {
	if p.job == nil {
		return ErrNotContained
	}
	return p.job.Terminate(1)
}

func (p *platform) terminateExcept(t *Tree, protect map[int]bool) ([]int, error) {
	if p.job == nil {
		return nil, ErrNotContained
	}
	pids, err := p.job.PIDs()
	if err != nil {
		return nil, err
	}
	var left []int
	var firstErr error
	for _, pid := range pids {
		if protect[pid] {
			left = append(left, pid)
			continue
		}
		if kerr := p.job.killMember(pid); kerr != nil && firstErr == nil {
			firstErr = kerr
		}
	}
	return left, firstErr
}

var procIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

// killMember ends one process of the job after confirming, on the process handle itself, that it
// is still in the job. The pids were listed a moment ago: one that exited and was reused since is
// not a member, and a stop that only ever touches the tree's own processes leaves it alone.
func (j *Job) killMember(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if err == windows.ERROR_INVALID_PARAMETER { // no such process: already gone
			return nil
		}
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer windows.CloseHandle(h)
	var in int32
	if r, _, e := procIsProcessInJob.Call(uintptr(h), uintptr(j.h), uintptr(unsafe.Pointer(&in))); r == 0 {
		return fmt.Errorf("IsProcessInJob(%d): %w", pid, e)
	}
	if in == 0 {
		return nil // not a member of this job any more
	}
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("TerminateProcess(%d): %w", pid, err)
	}
	return nil
}

// killPID ends one process by pid. A process that is already gone is not an error.
func killPID(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		if err == windows.ERROR_INVALID_PARAMETER { // no such process
			return nil
		}
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("TerminateProcess(%d): %w", pid, err)
	}
	return nil
}

func (p *platform) close() error {
	j := p.job
	p.job = nil
	return j.Close()
}

// KillsOnClose reports whether the tree's job ends its processes when it closes (false when the
// tree is uncontained).
func (t *Tree) KillsOnClose() bool {
	if t.p.job == nil {
		return false
	}
	ok, _ := t.p.job.KillsOnClose()
	return ok
}

// DisarmKillOnClose clears the kill-on-close of the tree's job, so that Close (or the wrapper dying) leaves the processes
// still in it running. The wrapper does it when a member must outlive it, a ComfyUI instance another lease took over,
// after ending everything else (TerminateExcept). It is a no-op on a tree that is uncontained or does not kill on close.
func (t *Tree) DisarmKillOnClose() error {
	if t.p.job == nil {
		return nil
	}
	return t.p.job.DisarmKillOnClose()
}
