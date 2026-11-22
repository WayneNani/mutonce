//go:build unix

package run

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestHelperAllocate is not a test: run as a subprocess with
// MUTONCE_HELPER_ALLOCATE set, it allocates until it is stopped.
func TestHelperAllocate(t *testing.T) {
	if os.Getenv("MUTONCE_HELPER_ALLOCATE") == "" {
		t.Skip("helper process")
	}
	var keep [][]byte
	for {
		b := make([]byte, 1<<20)
		for i := range b {
			b[i] = 1 // touch it so it is resident
		}
		keep = append(keep, b)
	}
}

// The watch guard is what macOS and the BSDs use; it relies only on ps and
// signals, so it is tested here on every Unix.
func TestWatchGuardStopsRunawayAllocation(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates")
	}
	const limit = 128 << 20
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperAllocate$")
	cmd.Env = append(os.Environ(), "MUTONCE_HELPER_ALLOCATE=1")
	g := newWatchGuard(cmd, limit)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := g.started(cmd); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	peak, exceeded := g.finished(cmd)
	if !exceeded || peak < limit {
		t.Fatalf("exceeded = %v, peak = %d MiB; want the guard to stop the process above %d MiB", exceeded, peak>>20, limit>>20)
	}
}
