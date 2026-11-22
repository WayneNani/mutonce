// Package mathx has no tests of its own: its mutants are killed (or not) by
// the tests of the packages that use it.
package mathx

import "cmp"

// Clamp limits v to the range [lo, hi].
func Clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Max returns the larger of a and b.
func Max[T cmp.Ordered](a, b T) T {
	if a > b {
		return a
	}
	return b
}
