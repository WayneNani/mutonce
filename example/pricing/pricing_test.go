package pricing

import (
	"errors"
	"testing"
)

func TestTotal(t *testing.T) {
	got, err := Total([]Item{{"Book", 1500, 2}, {"Pen", 250, 1}})
	if err != nil || got != 3250 {
		t.Fatalf("Total = %d, %v; want 3250", got, err)
	}
}

func TestTotalEmpty(t *testing.T) {
	if _, err := Total(nil); !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}
}

// Deliberately weak: no test at the boundaries 5000 and 10000.
func TestDiscount(t *testing.T) {
	for _, tc := range []struct{ total, want int }{{20000, 2000}, {6000, 300}, {1000, 0}} {
		if got := Discount(tc.total); got != tc.want {
			t.Errorf("Discount(%d) = %d, want %d", tc.total, got, tc.want)
		}
	}
}

// Deliberately weak: never checks that shipping is charged.
func TestFinal(t *testing.T) {
	if got, err := Final([]Item{{"Mug", 3000, 1}}, false); err != nil || got != 3000 {
		t.Errorf("Final = %d, %v; want 3000", got, err)
	}
	if got, err := Final([]Item{{"Mug", 1000, 1}}, true); err != nil || got != 1000 {
		t.Errorf("Final premium = %d, %v; want 1000", got, err)
	}
}
