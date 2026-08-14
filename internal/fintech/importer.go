package fintech

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/csv"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrUploadGone is returned by ConfirmImport when uploadID has expired or
// was never created by PreviewImport.
var ErrUploadGone = errors.New("fintech: upload expired or not found; please re-upload the file")

// ErrParseFailed is returned when the uploaded file can't be parsed as CSV.
var ErrParseFailed = errors.New("fintech: failed to parse file")

// ErrTooManyUploads is returned by PreviewImport when too many previews are
// pending confirmation, to bound the in-memory upload buffer.
var ErrTooManyUploads = errors.New("fintech: too many pending uploads; confirm or wait for existing ones to expire")

const (
	maxPreviewRows = 15
	uploadTTL      = 30 * time.Minute
	// maxPendingUploads bounds how many parsed-but-unconfirmed uploads are
	// held in memory at once (each up to maxImportUploadBytes), so a client
	// can't exhaust memory by repeatedly previewing without confirming.
	maxPendingUploads = 32
)

// ColumnMapping describes how to map a CSV's columns onto a trade. One
// mapping is saved per user (a home app, not multi-broker-per-user in v1)
// so repeat imports from the same broker export don't need re-mapping.
type ColumnMapping struct {
	DateColumn     string `json:"date_column"`
	SymbolColumn   string `json:"symbol_column"`
	SideColumn     string `json:"side_column"`
	QuantityColumn string `json:"quantity_column"`
	PriceColumn    string `json:"price_column"`
	FeeColumn      string `json:"fee_column"`
	DateFormat     string `json:"date_format"`
	HasHeader      bool   `json:"has_header"`
}

// PreviewResult is returned by PreviewImport.
type PreviewResult struct {
	UploadID     string         `json:"upload_id"`
	Filename     string         `json:"filename"`
	Headers      []string       `json:"headers"`
	PreviewRows  [][]string     `json:"preview_rows"`
	TotalRows    int            `json:"total_rows"`
	SavedMapping *ColumnMapping `json:"saved_mapping"`
}

// RowError describes one row that failed to import.
type RowError struct {
	Row     int    `json:"row"`
	Message string `json:"message"`
}

// ImportResult is returned by ConfirmImport.
type ImportResult struct {
	TotalRows         int        `json:"total_rows"`
	Inserted          int        `json:"inserted"`
	SkippedDuplicates int        `json:"skipped_duplicates"`
	Errors            []RowError `json:"errors"`
}

type pendingUpload struct {
	filename  string
	rows      [][]string
	createdAt time.Time
}

// importerState holds in-memory pending uploads between PreviewImport and
// ConfirmImport. There is no DB table for this transient state — only the
// confirmed result and saved column mapping are persisted.
type importerState struct {
	mu      sync.Mutex
	uploads map[string]pendingUpload
}

func newImporterState() *importerState {
	return &importerState{uploads: make(map[string]pendingUpload)}
}

func (st *importerState) evictExpiredLocked() {
	cutoff := time.Now().Add(-uploadTTL)
	for id, u := range st.uploads {
		if u.createdAt.Before(cutoff) {
			delete(st.uploads, id)
		}
	}
}

// PreviewImport parses content as CSV and stashes it in memory under a
// fresh upload ID for ConfirmImport to act on once the user has reviewed
// (and possibly adjusted) the column mapping.
func (s *Service) PreviewImport(ctx context.Context, filename string, content []byte) (PreviewResult, error) {
	rows, err := parseCSV(content)
	if err != nil {
		return PreviewResult{}, err
	}
	if len(rows) == 0 {
		return PreviewResult{}, fmt.Errorf("%w: file is empty", ErrValidation)
	}

	uploadID, err := randomToken(16)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("fintech: generate upload id: %w", err)
	}

	s.importer.mu.Lock()
	s.importer.evictExpiredLocked()
	if len(s.importer.uploads) >= maxPendingUploads {
		s.importer.mu.Unlock()
		return PreviewResult{}, ErrTooManyUploads
	}
	s.importer.uploads[uploadID] = pendingUpload{filename: filename, rows: rows, createdAt: time.Now()}
	s.importer.mu.Unlock()

	headers := columnLabels(rows, true)
	preview := dataRows(rows, true)
	if len(preview) > maxPreviewRows {
		preview = preview[:maxPreviewRows]
	}

	savedMapping, err := s.repo.GetImportMapping(ctx)
	if err != nil {
		return PreviewResult{}, err
	}

	return PreviewResult{
		UploadID:     uploadID,
		Filename:     filename,
		Headers:      headers,
		PreviewRows:  preview,
		TotalRows:    len(rows) - 1,
		SavedMapping: savedMapping,
	}, nil
}

// ConfirmImport applies mapping to the upload identified by uploadID,
// inserting one ledger transaction per valid row (source=csv_import)
// through the same RecordTrade path used by manual entry, so dedupe and
// validation behave identically regardless of where a trade came from.
func (s *Service) ConfirmImport(ctx context.Context, uploadID string, mapping ColumnMapping) (ImportResult, error) {
	if err := validateMapping(mapping); err != nil {
		return ImportResult{}, err
	}

	s.importer.mu.Lock()
	upload, ok := s.importer.uploads[uploadID]
	if ok {
		delete(s.importer.uploads, uploadID)
	}
	s.importer.mu.Unlock()
	if !ok {
		return ImportResult{}, ErrUploadGone
	}

	headers := columnLabels(upload.rows, mapping.HasHeader)
	rows := dataRows(upload.rows, mapping.HasHeader)

	dateIdx := columnIndex(headers, mapping.DateColumn)
	symbolIdx := columnIndex(headers, mapping.SymbolColumn)
	sideIdx := columnIndex(headers, mapping.SideColumn)
	quantityIdx := columnIndex(headers, mapping.QuantityColumn)
	priceIdx := columnIndex(headers, mapping.PriceColumn)
	feeIdx := columnIndex(headers, mapping.FeeColumn)

	result := ImportResult{TotalRows: len(rows)}
	for i, row := range rows {
		rowNum := i + 1
		if mapping.HasHeader {
			rowNum++
		}
		if allEmpty(row) {
			continue
		}

		executedAt, err := time.Parse(mapping.DateFormat, cellAt(row, dateIdx))
		if err != nil {
			result.Errors = append(result.Errors, RowError{Row: rowNum, Message: "invalid date: " + err.Error()})
			continue
		}
		side := Side(strings.ToLower(cellAt(row, sideIdx)))
		priceCents, err := parseUnsignedCents(cellAt(row, priceIdx))
		if err != nil {
			result.Errors = append(result.Errors, RowError{Row: rowNum, Message: "invalid price: " + err.Error()})
			continue
		}
		feeCents := int64(0)
		if feeIdx >= 0 {
			feeCents, err = parseUnsignedCents(cellAt(row, feeIdx))
			if err != nil {
				result.Errors = append(result.Errors, RowError{Row: rowNum, Message: "invalid fee: " + err.Error()})
				continue
			}
		}

		_, err = s.RecordTrade(ctx, RecordTradeInput{
			Symbol:     cellAt(row, symbolIdx),
			Side:       side,
			Quantity:   cellAt(row, quantityIdx),
			PriceCents: priceCents,
			FeeCents:   feeCents,
			ExecutedAt: executedAt,
			Source:     SourceCSVImport,
		})
		switch {
		case errors.Is(err, ErrDuplicate):
			result.SkippedDuplicates++
		case err != nil:
			result.Errors = append(result.Errors, RowError{Row: rowNum, Message: err.Error()})
		default:
			result.Inserted++
		}
	}

	if err := s.repo.SaveImportMapping(ctx, mapping); err != nil {
		return result, err
	}
	if err := s.repo.CreateImportBatch(ctx, upload.filename, result.Inserted); err != nil {
		return result, err
	}
	return result, nil
}

func validateMapping(m ColumnMapping) error {
	for name, v := range map[string]string{
		"date_column": m.DateColumn, "symbol_column": m.SymbolColumn, "side_column": m.SideColumn,
		"quantity_column": m.QuantityColumn, "price_column": m.PriceColumn, "date_format": m.DateFormat,
	} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%w: %s is required", ErrValidation, name)
		}
	}
	return nil
}

func parseCSV(content []byte) ([][]string, error) {
	content = bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF}) // strip UTF-8 BOM
	reader := csv.NewReader(bytes.NewReader(content))
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	rows, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParseFailed, err)
	}
	return rows, nil
}

func columnLabels(rows [][]string, hasHeader bool) []string {
	width := 0
	if len(rows) > 0 {
		width = len(rows[0])
	}
	headers := make([]string, width)
	if hasHeader && len(rows) > 0 {
		for i, cell := range rows[0] {
			headers[i] = strings.TrimSpace(cell)
			if headers[i] == "" {
				headers[i] = fmt.Sprintf("Column %d", i+1)
			}
		}
		return headers
	}
	for i := range headers {
		headers[i] = fmt.Sprintf("Column %d", i+1)
	}
	return headers
}

func dataRows(rows [][]string, hasHeader bool) [][]string {
	if hasHeader && len(rows) > 0 {
		return rows[1:]
	}
	return rows
}

func columnIndex(headers []string, name string) int {
	for i, h := range headers {
		if h == name {
			return i
		}
	}
	return -1
}

func cellAt(row []string, idx int) string {
	if idx < 0 || idx >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[idx])
}

func allEmpty(row []string) bool {
	for _, cell := range row {
		if strings.TrimSpace(cell) != "" {
			return false
		}
	}
	return true
}

// parseUnsignedCents parses a plain dollar string (e.g. "123.45", "$1,234.56")
// into integer cents. Unlike go_fintech's parseSignedCents, trade prices/fees
// are never negative, so there is no sign handling to get wrong here.
func parseUnsignedCents(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	s = strings.ReplaceAll(s, ",", "")
	s = strings.ReplaceAll(s, "$", "")
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}
	parts := strings.SplitN(s, ".", 2)
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q", raw)
	}
	var frac int64
	if len(parts) == 2 {
		fracStr := parts[1]
		if len(fracStr) == 1 {
			fracStr += "0"
		}
		if len(fracStr) > 2 {
			fracStr = fracStr[:2]
		}
		frac, err = strconv.ParseInt(fracStr, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid amount %q", raw)
		}
	}
	return whole*100 + frac, nil
}

func randomToken(numBytes int) (string, error) {
	buf := make([]byte, numBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// GetImportMapping returns the saved column mapping, or nil when nothing has
// been imported yet.
func (r *Repository) GetImportMapping(ctx context.Context) (*ColumnMapping, error) {
	var m ColumnMapping
	var hasHeader int
	err := r.db.QueryRowContext(ctx, `
		SELECT date_column, symbol_column, side_column, quantity_column,
		       price_column, fee_column, date_format, has_header
		FROM fintech_import_mapping WHERE id = 1`,
	).Scan(&m.DateColumn, &m.SymbolColumn, &m.SideColumn, &m.QuantityColumn,
		&m.PriceColumn, &m.FeeColumn, &m.DateFormat, &hasHeader)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fintech: get import mapping: %w", err)
	}
	m.HasHeader = hasHeader == 1
	return &m, nil
}

// SaveImportMapping remembers the mapping for the next import.
func (r *Repository) SaveImportMapping(ctx context.Context, m ColumnMapping) error {
	hasHeader := 0
	if m.HasHeader {
		hasHeader = 1
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO fintech_import_mapping
			(id, date_column, symbol_column, side_column, quantity_column,
			 price_column, fee_column, date_format, has_header, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			date_column = excluded.date_column, symbol_column = excluded.symbol_column,
			side_column = excluded.side_column, quantity_column = excluded.quantity_column,
			price_column = excluded.price_column, fee_column = excluded.fee_column,
			date_format = excluded.date_format, has_header = excluded.has_header,
			updated_at = excluded.updated_at`,
		m.DateColumn, m.SymbolColumn, m.SideColumn, m.QuantityColumn,
		m.PriceColumn, m.FeeColumn, m.DateFormat, hasHeader, timestamp(time.Now()))
	if err != nil {
		return fmt.Errorf("fintech: save import mapping: %w", err)
	}
	return nil
}

// CreateImportBatch records one completed import, so a bad one can be
// recognised after the fact.
func (r *Repository) CreateImportBatch(ctx context.Context, filename string, rowCount int) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO fintech_import_batches (filename, row_count, imported_at)
		VALUES (?, ?, ?)`, filename, rowCount, timestamp(time.Now()))
	if err != nil {
		return fmt.Errorf("fintech: create import batch: %w", err)
	}
	return nil
}
