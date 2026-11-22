package tricky

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultSize(t *testing.T) {
	if got := DefaultSize(); got != 8 {
		t.Fatalf("DefaultSize = %d, want 8", got)
	}
}

func TestPositive(t *testing.T) {
	if Positive(nil) {
		t.Error("Positive(nil) = true")
	}
	if !Positive(&Node{Val: 1}) {
		t.Error("Positive(1) = false")
	}
	if Positive(&Node{Val: 0}) {
		t.Error("Positive(0) = true")
	}
}

func TestFlags(t *testing.T) {
	if Both(true, false) || !Both(true, true) {
		t.Error("Both is wrong")
	}
	if !Less(1, 2) || Less(2, 1) {
		t.Error("Less is wrong")
	}
}

func TestLocked(t *testing.T) {
	if Locked(Open) || !Locked(Closed) {
		t.Error("Locked is wrong")
	}
}

func TestRepeat(t *testing.T) {
	if got := Repeat("a", 3); len(got) != 3 {
		t.Fatalf("Repeat = %v", got)
	}
}

func TestKnownAndBuffer(t *testing.T) {
	if !Known(Open) || !Known(Closed) || Known(State(7)) {
		t.Error("Known is wrong")
	}
	if err := (Buffer{}).Close(); err != nil {
		t.Error(err)
	}
}

func TestNote(t *testing.T) {
	dir := t.TempDir()
	if err := Note(dir, "x"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "note.txt")); err != nil || string(b) != "x" {
		t.Fatalf("read back: %q %v", b, err)
	}
}

func TestSum(t *testing.T) {
	if got := Sum(1, 2, 3); got != 6 {
		t.Errorf("Sum ints = %d", got)
	}
	if got := Sum(1.5, 2.5); got != 4 {
		t.Errorf("Sum floats = %g", got)
	}
}

func TestSign(t *testing.T) {
	for n, want := range map[int]int{-5: -1, 0: 0, 5: 1} {
		if got := Sign(n); got != want {
			t.Errorf("Sign(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestFind(t *testing.T) {
	if i, err := Find([]int{4, 5, 6}, 5); err != nil || i != 1 {
		t.Errorf("Find = %d, %v", i, err)
	}
	if _, err := Find([]int{4}, 7); !errors.Is(err, ErrNotFound) {
		t.Errorf("Find err = %v", err)
	}
}

func TestSumTo(t *testing.T) {
	if got := SumTo(4); got != 10 {
		t.Fatalf("SumTo(4) = %d, want 10", got)
	}
}
