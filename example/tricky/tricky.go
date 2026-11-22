// Package tricky collects constructs a mutant schema must handle without
// breaking compilation or changing the original semantics.
package tricky

import (
	"errors"
	"os"
	"path/filepath"
)

// Flag is a named bool type: expressions of this type must keep it, so the
// schema leaves them alone.
type Flag bool

// Limit is a constant expression; it cannot hold a runtime switch.
const Limit = 3 * 4

// defaultSize is initialised by mutated code during package initialisation.
var defaultSize = computeSize(4)

func computeSize(n int) int {
	if n > Limit {
		return Limit
	}
	return n * 2
}

// DefaultSize returns the size computed at start-up.
func DefaultSize() int { return defaultSize }

// Node is a linked list element.
type Node struct {
	Val  int
	Next *Node
}

// Positive relies on short-circuit evaluation: n.Val must not be read for nil.
func Positive(n *Node) bool {
	return n != nil && n.Val > 0
}

// Both combines two flags.
func Both(a, b Flag) Flag { return a && b }

// Less compares into a named bool.
func Less(a, b int) Flag { return a < b }

// Sum is generic; its zero value must be spelled as *new(T).
func Sum[T int | float64](xs ...T) T {
	var s T
	for _, x := range xs {
		s += x
	}
	return s
}

// Sign ends in a terminating if/else chain: removing one of its returns
// leaves a missing return, which the type-check filter must catch.
func Sign(n int) int {
	if n < 0 {
		return -1
	} else if n == 0 {
		return 0
	} else {
		return 1
	}
}

// State is enum-like: comparing it with its constants is categorical, so
// ordering variants such as s > Closed are not generated.
type State int

// States.
const (
	Open State = iota
	Closed
)

// Locked reports whether s is closed.
func Locked(s State) bool { return s == Closed }

// Known reports whether s is one of the states. Both sides of || cannot hold
// at once, so the LCR variant != would be equivalent.
func Known(s State) bool { return s == Open || s == Closed }

// Buffer needs no cleanup.
type Buffer struct{}

// Close only returns nil: RET's zero variant would be equivalent, only
// an injected error can change it.
func (Buffer) Close() error { return nil }

// Repeat returns n copies of v. With its loop condition mutated to true it
// appends forever; the memory limit has to stop that mutant before the
// machine runs out of memory.
func Repeat(v string, n int) []string {
	var out []string
	for i := 1; i <= n; i++ {
		out = append(out, v)
	}
	return out
}

func notePath(base string) string { return filepath.Join(base, "note.txt") }

// Note writes v atomically to base/note.txt. With notePath mutated to
// return "", the temporary file becomes ".tmp" in the working directory:
// the sandbox must keep that out of the real source tree.
func Note(base, v string) error {
	path := notePath(base)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(v), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ErrNotFound is returned by Find.
var ErrNotFound = errors.New("not found")

// Find has named results.
func Find(xs []int, v int) (idx int, err error) {
	for i, x := range xs {
		if x == v {
			return i, nil
		}
	}
	return -1, ErrNotFound
}

// SumTo uses a label and goto.
func SumTo(n int) int {
	total, i := 0, 1
loop:
	if i <= n {
		total += i
		i++
		goto loop
	}
	return total
}
