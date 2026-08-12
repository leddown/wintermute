package accounting

import "testing"

func TestFromFloatRoundsHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		in   float64
		want Money
	}{
		{0, 0},
		{1, 100},
		{1.005, 100}, // float64 cannot hold 1.005; nearest is just below
		{12.34, 1234},
		{12.345, 1235},
		{0.1, 10},
		{-12.34, -1234},
		{-0.005, -1},
		{1234567.89, 123456789},
	}
	for _, c := range cases {
		if got := FromFloat(c.in); got != c.want {
			t.Errorf("FromFloat(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestMoneyString(t *testing.T) {
	cases := []struct {
		in   Money
		want string
	}{
		{0, "0.00"},
		{5, "0.05"},
		{50, "0.50"},
		{100, "1.00"},
		{123456, "1234.56"},
		{-123456, "-1234.56"},
		{-5, "-0.05"},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Errorf("Money(%d).String() = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseMoney(t *testing.T) {
	ok := []struct {
		in   string
		want Money
	}{
		{"", 0},
		{"0", 0},
		{"1", 100},
		{"1.5", 150},
		{"1.50", 150},
		{"1234.56", 123456},
		{"1,234.56", 123456},
		{" 1234.56 ", 123456},
		{"-1234.56", -123456},
		{"+12", 1200},
		{".5", 50},
	}
	for _, c := range ok {
		got, err := ParseMoney(c.in)
		if err != nil {
			t.Errorf("ParseMoney(%q) errored: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseMoney(%q) = %d, want %d", c.in, got, c.want)
		}
	}

	// Three decimals are rejected rather than silently truncated: a price the
	// user typed and a price the ledger stores must not differ in silence.
	bad := []string{"abc", "1.234", "1.2.3", "12x"}
	for _, in := range bad {
		if _, err := ParseMoney(in); err == nil {
			t.Errorf("ParseMoney(%q) should have failed", in)
		}
	}
}

func TestMilliExtend(t *testing.T) {
	cases := []struct {
		hours Milli
		rate  Money
		want  Money
		note  string
	}{
		{1000, 9000, 9000, "1h at 90.00"},
		{1500, 9000, 13500, "1.5h at 90.00"},
		{333, 10000, 3330, "0.333h at 100.00"},
		{1, 10000, 10, "0.001h at 100.00 is 0.10"},
		{2500, 12550, 31375, "2.5h at 125.50"},
		{0, 10000, 0, "no hours"},
		{-1500, 9000, -13500, "negative hours mirror"},
	}
	for _, c := range cases {
		if got := c.hours.Extend(c.rate); got != c.want {
			t.Errorf("%s: Extend = %d, want %d", c.note, got, c.want)
		}
	}
}

func TestMilliString(t *testing.T) {
	cases := []struct {
		in   Milli
		want string
	}{
		{1000, "1"},
		{1500, "1.5"},
		{333, "0.333"},
		{2250, "2.25"},
		{0, "0"},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Errorf("Milli(%d).String() = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestVATRounding(t *testing.T) {
	cases := []struct {
		net    Money
		rateBP int64
		want   Money
		note   string
	}{
		{10000, 2100, 2100, "100.00 at 21%"},
		{10000, 900, 900, "100.00 at 9%"},
		{10000, 0, 0, "zero rated"},
		{10, 2100, 2, "0.10 at 21% rounds 2.1 to 2"},
		{1, 2100, 0, "0.01 at 21% rounds 0.21 to 0"},
		{5, 2100, 1, "0.05 at 21% rounds 1.05 to 1"},
		{13500, 2100, 2835, "135.00 at 21%"},
		{-10000, 2100, -2100, "credit note mirrors"},
	}
	for _, c := range cases {
		if got := VAT(c.net, c.rateBP); got != c.want {
			t.Errorf("%s: VAT(%d, %d) = %d, want %d", c.note, c.net, c.rateBP, got, c.want)
		}
	}
}

// Per-line VAT rounding is the EU invoicing convention, and it does not always
// agree with VAT computed on the total. The difference is real, small, and has
// to land somewhere for the entry to balance — which is what the rounding
// account is for. This test pins the behaviour so nobody "fixes" it later
// without knowing what they are changing.
func TestPerLineVATCanDifferFromTotalVAT(t *testing.T) {
	// 100.07 three times at 21%: each line's VAT is 21.0147, which rounds down
	// to 21.01, while VAT on the 300.21 total is 63.0441, rounding to 63.04.
	// The printed lines sum to 63.03 — a cent adrift of the total.
	lines := []Money{10007, 10007, 10007}
	var perLine, net Money
	for _, l := range lines {
		perLine += VAT(l, 2100)
		net += l
	}
	onTotal := VAT(net, 2100)

	if perLine == onTotal {
		t.Fatalf("expected a rounding difference; per-line %s, on-total %s", perLine, onTotal)
	}
	if diff := perLine - onTotal; diff != -1 {
		t.Errorf("expected the difference to be one cent, got %s", diff)
	}
}
