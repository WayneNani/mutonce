package run

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// memAvailable is the free memory of the machine, or less if a cgroup
// limit (container, CI job, systemd scope) leaves less.
func memAvailable() (int64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemAvailable:" {
			kb, err := strconv.ParseInt(f[1], 10, 64)
			avail := kb << 10
			if cg, ok := cgroupAvailable("/proc/self/cgroup", "/sys/fs/cgroup"); ok {
				avail = min(avail, cg)
			}
			return avail, err
		}
	}
	return 0, errors.New("MemAvailable missing from /proc/meminfo")
}

// cgroupAvailable returns the memory left under the cgroup v2 limits of this
// process and its parent groups, as far as the cgroup namespace shows them.
// A path with ".." lies outside the namespace's view; then only the limits
// from its root up are visible.
func cgroupAvailable(procCgroup, root string) (int64, bool) {
	data, err := os.ReadFile(procCgroup)
	if err != nil {
		return 0, false
	}
	path := "/"
	for _, line := range strings.Split(string(data), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			path = p
		}
	}
	if strings.Contains(path, "..") {
		path = "/"
	}
	read := func(dir, name string) (int64, bool) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return 0, false
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		return n, err == nil // "max" means no limit
	}
	best, found := int64(0), false
	for dir := filepath.Join(root, path); ; dir = filepath.Dir(dir) {
		if limit, ok := read(dir, "memory.max"); ok {
			used, _ := read(dir, "memory.current")
			if left := max(0, limit-used); !found || left < best {
				best, found = left, true
			}
		}
		if dir == root || len(dir) <= len(root) {
			break
		}
	}
	return best, found
}
