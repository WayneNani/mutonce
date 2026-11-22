// Package pricing computes order totals.
package pricing

import (
	"errors"
	"log/slog"

	"example.com/shop/internal/mathx"
)

// MaxTotal caps a single order (in cents).
const MaxTotal = 1_000_000

// ErrEmpty is returned for an empty cart.
var ErrEmpty = errors.New("pricing: empty cart")

// Item is one position of an order.
type Item struct {
	Name  string
	Cents int
	Qty   int
}

// Total sums up all positions. Items with a non-positive quantity are ignored.
func Total(items []Item) (int, error) {
	if len(items) == 0 {
		return 0, ErrEmpty
	}
	sum := 0
	for _, it := range items {
		if it.Qty <= 0 {
			continue
		}
		sum += it.Cents * it.Qty
	}
	slog.Debug("total computed", "sum", sum, "items", len(items))
	return mathx.Clamp(sum, 0, MaxTotal), nil
}

// Discount returns the discount in cents: 10 % from 100 €, 5 % from 50 €.
func Discount(total int) int {
	switch {
	case total >= 10000:
		return total / 10
	case total >= 5000:
		return total * 5 / 100
	}
	return 0
}

// FreeShipping reports whether shipping is free.
func FreeShipping(total int, premium bool) bool {
	return premium || total > 2500
}

// Final computes the amount to pay including shipping.
func Final(items []Item, premium bool) (int, error) {
	total, err := Total(items)
	if err != nil {
		return 0, err
	}
	total -= Discount(total)
	if !FreeShipping(total, premium) {
		total += 495
	}
	return total, nil
}
