// Package stock keeps a concurrency-safe inventory.
package stock

import (
	"errors"
	"sync"
)

// ErrInsufficient is returned when taking more than is in stock.
var ErrInsufficient = errors.New("stock: insufficient quantity")

// Store is an inventory.
type Store struct {
	mu    sync.Mutex
	items map[string]int
}

// New returns an empty store.
func New() *Store {
	return &Store{items: map[string]int{}}
}

// Add puts n pieces into stock.
func (s *Store) Add(name string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[name] += n
}

// Take removes n pieces from stock.
func (s *Store) Take(name string, n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items[name] < n {
		return ErrInsufficient
	}
	s.items[name] -= n
	return nil
}

// Count returns the pieces in stock.
func (s *Store) Count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.items[name]
}

// TryNotify delivers ev without blocking and reports whether it was taken.
func TryNotify(ch chan<- string, ev string) bool {
	select {
	case ch <- ev:
		return true
	default:
		return false
	}
}

// Batches returns how many halvings it takes to split n into single pieces.
// The loop only terminates because n shrinks.
func Batches(n int) int {
	steps := 0
	for n > 1 {
		n /= 2
		steps++
	}
	return steps
}
