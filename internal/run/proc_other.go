//go:build !unix && !windows

package run

import (
	"errors"
	"os/exec"
)

// noGuard is used where no process isolation is available; only the
// timeout and the shared memory budget apply.
type noGuard struct{}

func newGuard(*exec.Cmd, int64) procGuard        { return noGuard{} }
func (noGuard) started(*exec.Cmd) error          { return nil }
func (noGuard) finished(*exec.Cmd) (int64, bool) { return 0, false }
func memAvailable() (int64, error)               { return 0, errors.New("unknown on this platform") }
