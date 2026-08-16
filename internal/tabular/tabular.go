// Package tabular parses uploaded spreadsheet files (CSV, modern .xlsx, and
// legacy binary .xls) into rows of string cells, and provides small helpers
// shared by the modules that import them (see internal/todo/import.go). It is
// a support package — it knows nothing about any particular import schema; the
// caller maps the parsed rows onto its own model.
//
// Moved here from morpheus along with the notes and calendar imports it exists
// to serve. It does not replace the fintech module's own CSV reader: that one
// negotiates an unknown broker column layout with the operator, where this one
// reads a file whose header names are fixed and documented.
package tabular

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/extrame/xls"
	"github.com/xuri/excelize/v2"
)

var (
	// ErrUnsupportedFormat is returned for a filename whose extension is not
	// one of .csv, .xlsx, or .xls.
	ErrUnsupportedFormat = errors.New("tabular: unsupported file type (use .csv, .xlsx, or .xls)")
	// ErrParseFailed is returned when a file can't be parsed as its format.
	ErrParseFailed = errors.New("tabular: failed to parse file")
)

// Parse reads content as the format indicated by filename's extension and
// returns its rows (each a slice of cell strings). The first row is returned
// as-is; deciding whether it is a header is the caller's job. Rows may have
// differing lengths, so callers should use Cell for safe indexed access.
func Parse(filename string, content []byte) ([][]string, error) {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".csv":
		return parseCSV(content)
	case ".xlsx":
		return parseXLSX(content)
	case ".xls":
		return parseXLS(content)
	default:
		return nil, ErrUnsupportedFormat
	}
}

func parseCSV(content []byte) ([][]string, error) {
	content = bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF}) // strip UTF-8 BOM
	reader := csv.NewReader(bytes.NewReader(content))
	reader.FieldsPerRecord = -1 // tolerate ragged rows
	reader.TrimLeadingSpace = true
	rows, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParseFailed, err)
	}
	return rows, nil
}

// parseXLSX reads the first sheet of an .xlsx workbook. Like parseXLS the
// parse is panic-guarded: the input is an uploaded file, and a malformed one
// reaching a panic in the decoder would otherwise take down whatever
// goroutine is parsing it.
func parseXLSX(content []byte) (rows [][]string, err error) {
	defer func() {
		if r := recover(); r != nil {
			rows, err = nil, fmt.Errorf("%w: %v", ErrParseFailed, r)
		}
	}()

	f, err := excelize.OpenReader(bytes.NewReader(content))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParseFailed, err)
	}
	defer f.Close()
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, nil
	}
	rows, err = f.GetRows(sheets[0])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParseFailed, err)
	}
	return rows, nil
}

// parseXLS reads the first sheet of a legacy binary .xls workbook. The
// underlying library is known to panic on malformed input, so the parse is
// guarded and any panic is converted into ErrParseFailed.
func parseXLS(content []byte) (rows [][]string, err error) {
	defer func() {
		if r := recover(); r != nil {
			rows, err = nil, fmt.Errorf("%w: %v", ErrParseFailed, r)
		}
	}()

	wb, err := xls.OpenReader(bytes.NewReader(content), "utf-8")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParseFailed, err)
	}
	if wb.NumSheets() == 0 {
		return nil, nil
	}
	sheet := wb.GetSheet(0)
	if sheet == nil {
		return nil, nil
	}
	for i := 0; i <= int(sheet.MaxRow); i++ {
		row := sheet.Row(i)
		if row == nil {
			rows = append(rows, nil)
			continue
		}
		last := row.LastCol()
		cells := make([]string, 0, last+1)
		for j := 0; j <= last; j++ {
			cells = append(cells, row.Col(j))
		}
		rows = append(rows, cells)
	}
	return rows, nil
}

// RowError describes one input row that could not be imported.
type RowError struct {
	Row     int    `json:"row"`
	Message string `json:"message"`
}

// ImportResult is the per-import summary returned to the client: how many
// rows were created, how many failed, and why each failed.
type ImportResult struct {
	TotalRows int        `json:"total_rows"`
	Imported  int        `json:"imported"`
	Failed    int        `json:"failed"`
	Errors    []RowError `json:"errors"`
}

// HeaderIndex returns the index of the column named name (case-insensitive,
// whitespace-trimmed), or -1 if absent.
func HeaderIndex(headers []string, name string) int {
	for i, h := range headers {
		if strings.EqualFold(strings.TrimSpace(h), name) {
			return i
		}
	}
	return -1
}

// Cell returns the trimmed value at idx, or "" if idx is out of range (which
// happens for ragged rows or an absent optional column).
func Cell(row []string, idx int) string {
	if idx < 0 || idx >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[idx])
}

// AllEmpty reports whether every cell in row is blank, so blank lines (common
// at the end of spreadsheet exports) can be skipped rather than counted as
// failures.
func AllEmpty(row []string) bool {
	for _, c := range row {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}
