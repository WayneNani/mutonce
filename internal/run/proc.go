package run

import (
	"os/exec"
	"runtime"
)

// A procGuard isolates one test process: the process and anything it starts
// can be killed together when it times out, and it stays within a memory
// limit. A mutant that removes a loop's exit condition allocates gigabytes
// within seconds; without a hard limit the kernel's OOM killer steps in and
// may take down whatever shares the process's session or cgroup.
//
// The mechanism differs by platform:
//
//   - Linux: own process group, hard limit with RLIMIT_DATA.
//   - macOS and the BSDs: own process group, resident memory watched and the
//     group killed above the limit (macOS does not enforce RLIMIT_DATA).
//   - Windows: a job object with a process memory limit that also kills the
//     whole process tree.
type procGuard interface {
	// started applies the limit right after cmd has started.
	started(cmd *exec.Cmd) error
	// finished returns the peak memory of the process and whether the
	// guard itself stopped it for exceeding the limit.
	finished(cmd *exec.Cmd) (peak int64, exceeded bool)
}

// exeSuffix is appended to built test binaries.
var exeSuffix = func() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}()
