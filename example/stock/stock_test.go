package stock

import (
	"errors"
	"sync"
	"testing"
)

func TestStore(t *testing.T) {
	s := New()
	s.Add("apple", 5)
	if err := s.Take("apple", 3); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got := s.Count("apple"); got != 2 {
		t.Fatalf("Count = %d, want 2", got)
	}
	if err := s.Take("apple", 3); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("Take = %v, want ErrInsufficient", err)
	}
}

func TestConcurrentAdd(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Add("pear", 1)
		}()
	}
	wg.Wait()
	if got := s.Count("pear"); got != 50 {
		t.Fatalf("Count = %d, want 50", got)
	}
}

func TestTryNotify(t *testing.T) {
	ch := make(chan string, 1)
	if !TryNotify(ch, "a") {
		t.Fatal("first notification not delivered")
	}
	if TryNotify(ch, "b") {
		t.Fatal("full channel accepted a notification")
	}
}

func TestBatches(t *testing.T) {
	if got := Batches(8); got != 3 {
		t.Fatalf("Batches(8) = %d, want 3", got)
	}
}
