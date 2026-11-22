//go:build unix

package run

import (
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// watchGuard polls the resident memory of the test process and kills its
// process group above the limit. macOS does not enforce RLIMIT_DATA for the
// anonymous mappings the Go runtime uses, so a hard limit is not available.
// The poll interval leaves some slack: a process can overshoot by what it
// allocates within one interval.
type watchGuard struct {
	limit    int64
	stop     chan struct{}
	done     sync.WaitGroup
	peak     atomic.Int64
	exceeded atomic.Bool
}

const watchInterval = 50 * time.Millisecond

func newWatchGuard(cmd *exec.Cmd, limit int64) *watchGuard {
	ownProcessGroup(cmd)
	return &watchGuard{limit: limit, stop: make(chan struct{})}
}

func (g *watchGuard) started(cmd *exec.Cmd) error {
	pid := cmd.Process.Pid
	g.done.Add(1)
	go func() {
		defer g.done.Done()
		t := time.NewTicker(watchInterval)
		defer t.Stop()
		for {
			select {
			case <-g.stop:
				return
			case <-t.C:
			}
			rss, err := residentMemory(pid)
			if err != nil {
				continue // the process may just have exited
			}
			if rss > g.peak.Load() {
				g.peak.Store(rss)
			}
			if rss > g.limit {
				g.exceeded.Store(true)
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				return
			}
		}
	}()
	return nil
}

func (g *watchGuard) finished(cmd *exec.Cmd) (int64, bool) {
	close(g.stop)
	g.done.Wait()
	peak := g.peak.Load()
	if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		maxrss := int64(ru.Maxrss)
		if runtime.GOOS != "darwin" {
			maxrss <<= 10 // kilobytes on the BSDs, bytes on macOS
		}
		peak = max(peak, maxrss)
	}
	return peak, g.exceeded.Load()
}

// residentMemory asks ps for the resident set size, which works the same on
// macOS and the BSDs without cgo.
func residentMemory(pid int) (int64, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return kb << 10, err
}
