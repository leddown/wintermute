package todo

import (
	"fmt"
	"strings"

	"wintermute/internal/tabular"
)

// Bulk import of notes and events from a spreadsheet, moved here from morpheus
// along with the features themselves.
//
// Both importers go through the same Service.Create* path as manual entry, so
// a row is validated exactly as a typed-in entry would be. A row that fails is
// recorded in the result and the import carries on: one bad date in a file of
// two hundred should not send the whole file back.

// ImportNotes bulk-creates notes from already-parsed spreadsheet rows. The
// first row must be a header naming the columns; recognised columns are:
//
//	body        (required) — the note text
//	event_date  (optional) — "YYYY-MM-DD" to pin the note to a calendar day
//
// Column order is irrelevant and unrecognised columns are ignored.
func (s *Service) ImportNotes(rows [][]string) (tabular.ImportResult, error) {
	if len(rows) == 0 {
		return tabular.ImportResult{}, fmt.Errorf("the file is empty")
	}

	headers := rows[0]
	bodyIdx := tabular.HeaderIndex(headers, "body")
	if bodyIdx < 0 {
		return tabular.ImportResult{}, fmt.Errorf("missing required column %q", "body")
	}
	dateIdx := tabular.HeaderIndex(headers, "event_date")

	data := rows[1:]
	result := tabular.ImportResult{TotalRows: len(data)}
	for i, row := range data {
		rowNum := i + 2 // human-facing 1-based row, accounting for the header
		if tabular.AllEmpty(row) {
			result.TotalRows--
			continue
		}
		if _, err := s.CreateNote(tabular.Cell(row, bodyIdx), tabular.Cell(row, dateIdx)); err != nil {
			result.Errors = append(result.Errors, tabular.RowError{Row: rowNum, Message: err.Error()})
			result.Failed++
			continue
		}
		result.Imported++
	}
	return result, nil
}

// ImportEvents bulk-creates calendar events from already-parsed spreadsheet
// rows. The first row must be a header naming the columns; recognised columns
// are:
//
//	title        (required) — event title
//	start        (required) — "YYYY-MM-DD" (all-day) or an RFC3339 timestamp
//	end          (optional) — same formats as start
//	description  (optional) — free text
//	all_day      (optional) — true/false/yes/no/1/0
//
// Column order is irrelevant and unrecognised columns are ignored.
func (s *Service) ImportEvents(rows [][]string) (tabular.ImportResult, error) {
	if len(rows) == 0 {
		return tabular.ImportResult{}, fmt.Errorf("the file is empty")
	}

	headers := rows[0]
	titleIdx := tabular.HeaderIndex(headers, "title")
	startIdx := tabular.HeaderIndex(headers, "start")
	if titleIdx < 0 || startIdx < 0 {
		return tabular.ImportResult{}, fmt.Errorf("missing required column(s); both %q and %q are required", "title", "start")
	}
	endIdx := tabular.HeaderIndex(headers, "end")
	descIdx := tabular.HeaderIndex(headers, "description")
	allDayIdx := tabular.HeaderIndex(headers, "all_day")

	data := rows[1:]
	result := tabular.ImportResult{TotalRows: len(data)}
	for i, row := range data {
		rowNum := i + 2
		if tabular.AllEmpty(row) {
			result.TotalRows--
			continue
		}

		allDay, err := parseBoolCell(tabular.Cell(row, allDayIdx))
		if err != nil {
			result.Errors = append(result.Errors, tabular.RowError{Row: rowNum, Message: "all_day: " + err.Error()})
			result.Failed++
			continue
		}

		// Validate settles the rest — the formats of start and end, their
		// order, and whether a date-only start makes the event all-day.
		if _, err := s.CreateEvent(Event{
			Title:       tabular.Cell(row, titleIdx),
			Description: tabular.Cell(row, descIdx),
			StartAt:     tabular.Cell(row, startIdx),
			EndAt:       tabular.Cell(row, endIdx),
			AllDay:      allDay,
		}); err != nil {
			result.Errors = append(result.Errors, tabular.RowError{Row: rowNum, Message: err.Error()})
			result.Failed++
			continue
		}
		result.Imported++
	}
	return result, nil
}

// parseBoolCell interprets a spreadsheet truthiness cell. An empty cell is
// false, because the column is optional; anything unrecognised is a row error
// rather than a guess.
func parseBoolCell(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return false, nil
	case "true", "yes", "y", "1":
		return true, nil
	case "false", "no", "n", "0":
		return false, nil
	default:
		return false, fmt.Errorf("must be true or false")
	}
}
