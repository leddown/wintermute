package tabular

import (
	"bytes"
	"errors"
	"testing"

	"github.com/xuri/excelize/v2"
)

func TestParseCSV(t *testing.T) {
	csv := "body,event_date\nBuy milk,\nDentist,2026-07-14\n"
	rows, err := Parse("notes.csv", []byte(csv))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if rows[0][0] != "body" || rows[0][1] != "event_date" {
		t.Errorf("header = %v", rows[0])
	}
	if rows[2][0] != "Dentist" || rows[2][1] != "2026-07-14" {
		t.Errorf("row 2 = %v", rows[2])
	}
}

func TestParseCSVStripsBOM(t *testing.T) {
	csv := append([]byte{0xEF, 0xBB, 0xBF}, []byte("body\nhello\n")...)
	rows, err := Parse("notes.csv", csv)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if rows[0][0] != "body" {
		t.Errorf("BOM not stripped: header = %q", rows[0][0])
	}
}

func TestParseXLSX(t *testing.T) {
	f := excelize.NewFile()
	sheet := f.GetSheetName(0)
	f.SetSheetRow(sheet, "A1", &[]any{"title", "start"})
	f.SetSheetRow(sheet, "A2", &[]any{"Offsite", "2026-07-20"})
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatalf("write xlsx: %v", err)
	}

	rows, err := Parse("events.xlsx", buf.Bytes())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0][0] != "title" || rows[1][0] != "Offsite" || rows[1][1] != "2026-07-20" {
		t.Errorf("rows = %v", rows)
	}
}

func TestParseUnsupported(t *testing.T) {
	if _, err := Parse("data.json", []byte("{}")); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("got %v, want ErrUnsupportedFormat", err)
	}
}

func TestHelpers(t *testing.T) {
	headers := []string{"Body", " Event_Date "}
	if got := HeaderIndex(headers, "event_date"); got != 1 {
		t.Errorf("HeaderIndex case/space-insensitive: got %d, want 1", got)
	}
	if got := HeaderIndex(headers, "missing"); got != -1 {
		t.Errorf("HeaderIndex absent: got %d, want -1", got)
	}
	if got := Cell([]string{"a", " b "}, 1); got != "b" {
		t.Errorf("Cell trims: got %q", got)
	}
	if got := Cell([]string{"a"}, 5); got != "" {
		t.Errorf("Cell out-of-range: got %q", got)
	}
	if !AllEmpty([]string{"", "  ", ""}) || AllEmpty([]string{"", "x"}) {
		t.Errorf("AllEmpty wrong")
	}
}
