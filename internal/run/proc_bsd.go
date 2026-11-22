//go:build unix && !linux

package run

import (
	"errors"
	"os/exec"

	"golang.org/x/sys/unix"
)

func newGuard(cmd *exec.Cmd, limit int64) procGuard { return newWatchGuard(cmd, limit) }

// memAvailable approximates the available memory by the physical memory;
// these systems do not report a reliable figure for free memory.
func memAvailable() (int64, error) {
	for _, name := range []string{"hw.memsize", "hw.physmem64", "hw.physmem"} {
		if n, err := unix.SysctlUint64(name); err == nil && n > 0 {
			return int64(n), nil
		}
	}
	return 0, errors.New("physical memory size unknown")
}
