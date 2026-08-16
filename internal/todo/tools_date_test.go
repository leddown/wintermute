package todo

import "testing"

func TestNormaliseToolDate(t *testing.T) {
	ok := map[string]string{
		"2026-08-18":      "2026-08-18",
		"18/8/2026":       "2026-08-18", // the reported failure
		"18/08/2026":      "2026-08-18",
		"18-8-2026":       "2026-08-18",
		"8/18/2026":       "2026-08-18", // month-first, unambiguous
		"2026/08/18":      "2026-08-18",
		"18 August 2026":  "2026-08-18",
		"18 Aug 2026":     "2026-08-18",
		"August 18, 2026": "2026-08-18",
		"":                "",
	}
	for in, want := range ok {
		got, err := normaliseToolDate(in)
		if err != nil {
			t.Errorf("normaliseToolDate(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("normaliseToolDate(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"5/6/2026", "31/2/2026", "not a date", "18/8/26"} {
		if got, err := normaliseToolDate(in); err == nil {
			t.Errorf("normaliseToolDate(%q) = %q, want an error", in, got)
		}
	}
}
