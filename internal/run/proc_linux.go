package run

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// rlimitGuard caps the memory the Go runtime can map with RLIMIT_DATA. A
// mutant beyond it dies with "fatal error: runtime: out of memory", which
// runTests recognises in the output.
type rlimitGuard struct{ limit int64 }

func newGuard(cmd *exec.Cmd, limit int64) procGuard {
	ownProcessGroup(cmd)
	return rlimitGuard{limit}
}

// started limits the child right after it starts; it passes the limit on to
// processes it spawns itself. The runtime has mapped little more than its
// initial arenas by then.
func (g rlimitGuard) started(cmd *exec.Cmd) error {
	lim := unix.Rlimit{Cur: uint64(g.limit), Max: uint64(g.limit)}
	if err := unix.Prlimit(cmd.Process.Pid, unix.RLIMIT_DATA, &lim, nil); err != nil {
		return fmt.Errorf("setting memory limit: %w", err)
	}
	return nil
}

func (rlimitGuard) finished(cmd *exec.Cmd) (int64, bool) {
	if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		return ru.Maxrss << 10, false // kilobytes on Linux
	}
	return 0, false
}

func memAvailable() (int64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemAvailable:" {
			kb, err := strconv.ParseInt(f[1], 10, 64)
			return kb << 10, err
		}
	}
	return 0, errors.New("MemAvailable missing from /proc/meminfo")
}
