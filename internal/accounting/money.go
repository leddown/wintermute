package accounting

import (
	"fmt"
	"strconv"
	"strings"
)

// Money is an amount in minor units — cents — held as an integer.
//
// The CRM next door uses float64 for rates and amounts, which is fine for
// "roughly how much is outstanding" and unusable here. A ledger's entire claim
// is that debits equal credits exactly; binary floating point cannot represent
// 0.10 and so cannot keep that promise across a few thousand rows. Every
// amount in this package is Money, and the only places it becomes a float are
// the CRM boundary (ParseHours, FromFloat) and display.
type Money int64

// FromFloat converts a decimal amount — a rate or total that arrived from the
// CRM's REAL columns or from JSON — into minor units, rounding half away from
// zero. This is the one lossy step, and it happens once, at the edge.
func FromFloat(v float64) Money {
	if v < 0 {
		return Money(int64(v*100 - 0.5))
	}
	return Money(int64(v*100 + 0.5))
}

// Float renders the amount for display or for a JSON field that a browser will
// format. Never feed the result back into a calculation.
func (m Money) Float() float64 { return float64(m) / 100 }

// String formats the amount with two decimals and no currency symbol.
func (m Money) String() string {
	neg := m < 0
	if neg {
		m = -m
	}
	s := fmt.Sprintf("%d.%02d", int64(m)/100, int64(m)%100)
	if neg {
		return "-" + s
	}
	return s
}

// ParseMoney reads a decimal string such as "1234.56" into minor units. It
// accepts a missing or single decimal place, thousands separators, and a
// leading minus, and rejects anything else rather than guessing.
func ParseMoney(s string) (Money, error) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, ",", "")
	s = strings.ReplaceAll(s, " ", "")
	if s == "" {
		return 0, nil
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")

	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not an amount: %q", s)
	}
	var cents int64
	if hasFrac {
		switch len(frac) {
		case 0:
		case 1:
			f, err := strconv.ParseInt(frac, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("not an amount: %q", s)
			}
			cents = f * 10
		case 2:
			f, err := strconv.ParseInt(frac, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("not an amount: %q", s)
			}
			cents = f
		default:
			return 0, fmt.Errorf("amount %q has more than two decimal places", s)
		}
	}
	total := w*100 + cents
	if neg {
		total = -total
	}
	return Money(total), nil
}

// Milli is a quantity in thousandths — 1.5 hours is 1500. Hours reach this
// package from a REAL column, so they are pinned to an exact integer before
// they are ever multiplied by a price.
type Milli int64

// MilliFromFloat converts an hours figure to thousandths, rounding half up.
func MilliFromFloat(v float64) Milli {
	if v < 0 {
		return Milli(int64(v*1000 - 0.5))
	}
	return Milli(int64(v*1000 + 0.5))
}

// Float renders the quantity for display.
func (q Milli) Float() float64 { return float64(q) / 1000 }

// String formats the quantity with up to three decimals, trailing zeros
// trimmed, so 1500 reads as "1.5" rather than "1.500".
func (q Milli) String() string {
	s := fmt.Sprintf("%d.%03d", int64(q)/1000, abs64(int64(q))%1000)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// Extend multiplies a quantity in thousandths by a unit price in minor units,
// rounding half away from zero. 1.5 hours at 90.00 is 135.00, and 0.333 hours
// at 100.00 is 33.30 rather than 33.2999…
func (q Milli) Extend(unit Money) Money {
	n := int64(q) * int64(unit)
	if n < 0 {
		return Money((n - 500) / 1000)
	}
	return Money((n + 500) / 1000)
}

// VAT computes the tax on a net amount at a rate in basis points (2100 = 21%),
// rounding half away from zero.
//
// Rounding per line rather than on the invoice total is the convention EU
// invoices are read with: each line shows its own VAT and the total is the sum
// of what is printed. It also means the sum of line VAT can differ by a unit or
// two from VAT computed on the total — which is exactly the residue the
// rounding account exists to absorb.
func VAT(net Money, rateBP int64) Money {
	n := int64(net) * rateBP
	if n < 0 {
		return Money((n - 5000) / 10000)
	}
	return Money((n + 5000) / 10000)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
