//go:build windows

package capture

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	createSuspended     = 0x00000004
	processSetQuota     = 0x00000100
	threadSuspendResume = 0x0002
	// ResumeThread returns a 32-bit DWORD on every Windows architecture.
	resumeFailed = uintptr(0xffffffff)
)

var (
	kernel32                   = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW       = kernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJob     = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject     = kernel32.NewProc("TerminateJobObject")
	procThread32First          = kernel32.NewProc("Thread32First")
	procThread32Next           = kernel32.NewProc("Thread32Next")
	procOpenThread             = kernel32.NewProc("OpenThread")
	procResumeThread           = kernel32.NewProc("ResumeThread")
	errProcessIsolation        = errors.New("capture: process-tree isolation failed")
	errSuspendedThreadNotFound = errors.New("capture: suspended process thread not found")
)

type processController struct {
	job syscall.Handle
}

func prepareProcess(cmd *exec.Cmd) *processController {
	// Suspending at creation closes the race in which the child could create a
	// descendant before it is assigned to the supervising Job Object.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createSuspended,
	}
	return &processController{}
}

func (p *processController) afterStart(cmd *exec.Cmd) error {
	thread, err := suspendedThread(uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(thread) //nolint:errcheck // best-effort handle cleanup

	job, _, callErr := procCreateJobObjectW.Call(0, 0)
	if job == 0 {
		return fixedWindowsError(callErr)
	}
	p.job = syscall.Handle(job)

	process, err := syscall.OpenProcess(
		syscall.PROCESS_TERMINATE|processSetQuota,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		p.close()
		return errProcessIsolation
	}
	assigned, _, _ := procAssignProcessToJob.Call(
		uintptr(p.job),
		uintptr(process),
	)
	_ = syscall.CloseHandle(process)
	if assigned == 0 {
		p.close()
		return errProcessIsolation
	}

	resumed, _, _ := procResumeThread.Call(uintptr(thread))
	if resumed == resumeFailed {
		_, _, _ = procTerminateJobObject.Call(uintptr(p.job), 1)
		p.close()
		return errProcessIsolation
	}
	return nil
}

func (p *processController) terminate(cmd *exec.Cmd) error {
	if p.job != 0 {
		terminated, _, _ := procTerminateJobObject.Call(uintptr(p.job), 1)
		if terminated != 0 {
			return nil
		}
	}
	return cmd.Process.Kill()
}

func (p *processController) close() {
	if p.job == 0 {
		return
	}
	_ = syscall.CloseHandle(p.job)
	p.job = 0
}

func processSignal(*os.ProcessState) (string, int, bool) {
	// Windows reports process termination through numeric exit statuses rather
	// than Unix wait-status signals.
	return "", 0, false
}

type threadEntry32 struct {
	Size           uint32
	Usage          uint32
	ThreadID       uint32
	OwnerProcessID uint32
	BasePriority   int32
	DeltaPriority  int32
	Flags          uint32
}

func suspendedThread(pid uint32) (syscall.Handle, error) {
	snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return 0, errSuspendedThreadNotFound
	}
	defer syscall.CloseHandle(snapshot) //nolint:errcheck // best-effort handle cleanup

	entry := threadEntry32{Size: uint32(unsafe.Sizeof(threadEntry32{}))}
	found, _, _ := procThread32First.Call(
		uintptr(snapshot),
		uintptr(unsafe.Pointer(&entry)),
	)
	for found != 0 {
		if entry.OwnerProcessID == pid {
			thread, _, _ := procOpenThread.Call(
				threadSuspendResume,
				0,
				uintptr(entry.ThreadID),
			)
			if thread != 0 {
				return syscall.Handle(thread), nil
			}
			return 0, errSuspendedThreadNotFound
		}
		entry.Size = uint32(unsafe.Sizeof(threadEntry32{}))
		found, _, _ = procThread32Next.Call(
			uintptr(snapshot),
			uintptr(unsafe.Pointer(&entry)),
		)
	}
	return 0, errSuspendedThreadNotFound
}

func fixedWindowsError(err error) error {
	if err == nil {
		return errProcessIsolation
	}
	// Do not return operating-system text that could contain command details.
	return errProcessIsolation
}
