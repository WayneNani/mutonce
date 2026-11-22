package run

import (
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobGuard puts the test process into a job object. The job limits the
// memory each process may commit, so a mutant beyond it fails with "out of
// memory", and closing or terminating the job kills the whole process tree.
type jobGuard struct {
	limit int64
	job   windows.Handle
}

func newGuard(cmd *exec.Cmd, limit int64) procGuard {
	g := &jobGuard{limit: limit}
	cmd.Cancel = func() error {
		if g.job != 0 {
			_ = windows.TerminateJobObject(g.job, 1)
		}
		return cmd.Process.Kill()
	}
	return g
}

func (g *jobGuard) started(cmd *exec.Cmd) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("creating job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY | windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
		ProcessMemoryLimit: uintptr(g.limit),
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("setting memory limit: %w", err)
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("opening test process: %w", err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("assigning test process to job: %w", err)
	}
	g.job = job
	return nil
}

func (g *jobGuard) finished(*exec.Cmd) (int64, bool) {
	if g.job == 0 {
		return 0, false
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	err := windows.QueryInformationJobObject(g.job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
	windows.CloseHandle(g.job)
	g.job = 0
	if err != nil {
		return 0, false
	}
	return int64(info.PeakProcessMemoryUsed), false
}

// memoryStatusEx is MEMORYSTATUSEX, which golang.org/x/sys/windows lacks.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

var globalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

func memAvailable() (int64, error) {
	m := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, err := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0, fmt.Errorf("GlobalMemoryStatusEx: %w", err)
	}
	return int64(m.AvailPhys), nil
}
