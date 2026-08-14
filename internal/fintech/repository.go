package fintech

// The ledger's persistence, on SQLite.
//
// Three things differ from the PostgreSQL original this came from, and each is
// forced by the database rather than chosen:
//
//   - Timestamps are stored as RFC 3339 text, written by this layer. SQLite has
//     no timestamp type, and the workspace tables next door already settled on
//     this shape.
//   - Duplicate detection reads the driver's error text rather than a SQL state
//     code. modernc's driver reports a violated UNIQUE as a message containing
//     "UNIQUE constraint failed"; there is no pgconn.PgError to type-assert.
//   - Quantities go in and come out as decimal strings and are never summed by
//     the database. See the migration's note: SQLite would turn them into
//     float64 and lose the eighth decimal of a bitcoin. Every aggregation of a
//     quantity happens in Go, exactly, with math/big.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Repository persists the fintech ledger to SQLite.
//
// It takes no owner argument anywhere. Morpheus threaded a user id through
// every method because it had signed-in users; wintermute authenticates
// clients at the API edge and holds one portfolio, so the parameter would be a
// constant and the WHERE clause a tautology. See 0008_fintech.sql.
type Repository struct {
	db *sql.DB
}

// NewRepository builds a Repository backed by db.
func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// timestamp is how every time value in this package reaches the database.
func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// parseTime reads a stored timestamp back. An unparseable or empty value
// becomes the zero time rather than an error: a missing timestamp should cost a
// row its date, not its existence.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// isUniqueViolation reports whether err is SQLite refusing a duplicate. The
// text match is what the driver gives us — it exposes no typed error for this —
// so it is kept in one place rather than repeated at every call site.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// UpsertInstrument inserts symbol if it is new and returns the existing row
// otherwise. Name and asset class apply on first insert only: a symbol is
// reference data, and re-importing a CSV should not retype what a ticker is.
func (r *Repository) UpsertInstrument(ctx context.Context, symbol, name string, assetClass AssetClass) (Instrument, error) {
	var in Instrument
	var createdAt string
	err := r.db.QueryRowContext(ctx, `
		SELECT id, symbol, name, asset_class, created_at
		FROM fintech_instruments WHERE symbol = ?`, symbol,
	).Scan(&in.ID, &in.Symbol, &in.Name, &in.AssetClass, &createdAt)
	if err == nil {
		in.CreatedAt = parseTime(createdAt)
		return in, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Instrument{}, fmt.Errorf("fintech: query instrument: %w", err)
	}

	now := time.Now()
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO fintech_instruments (symbol, name, asset_class, created_at)
		VALUES (?, ?, ?, ?)`, symbol, name, string(assetClass), timestamp(now))
	if err != nil {
		// Another writer inserted the same symbol between the read and here.
		// Reading it back is the answer, not an error: both callers wanted the
		// row to exist and now it does.
		if isUniqueViolation(err) {
			return r.UpsertInstrument(ctx, symbol, name, assetClass)
		}
		return Instrument{}, fmt.Errorf("fintech: insert instrument: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Instrument{}, fmt.Errorf("fintech: instrument id: %w", err)
	}
	return Instrument{ID: id, Symbol: symbol, Name: name, AssetClass: assetClass, CreatedAt: now}, nil
}

// GetInstrumentBySymbol returns one instrument, or ErrNotFound.
func (r *Repository) GetInstrumentBySymbol(ctx context.Context, symbol string) (Instrument, error) {
	var in Instrument
	var createdAt string
	err := r.db.QueryRowContext(ctx, `
		SELECT id, symbol, name, asset_class, created_at
		FROM fintech_instruments WHERE symbol = ?`, symbol,
	).Scan(&in.ID, &in.Symbol, &in.Name, &in.AssetClass, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Instrument{}, ErrNotFound
	}
	if err != nil {
		return Instrument{}, fmt.Errorf("fintech: query instrument: %w", err)
	}
	in.CreatedAt = parseTime(createdAt)
	return in, nil
}

// InsertTransaction appends one row to the ledger, returning ErrDuplicate when
// its dedupe hash is already present.
//
// The constraint does the checking, not a preceding SELECT: two imports of the
// same file running at once would both pass a pre-check and both insert.
func (r *Repository) InsertTransaction(ctx context.Context, txn Transaction) (Transaction, error) {
	now := time.Now()
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO fintech_transactions
			(instrument_id, side, quantity, price_cents, fee_cents, total_cents,
			 source, executed_at, broker_order_id, external_id, dedupe_hash, notes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		txn.InstrumentID, string(txn.Side), txn.Quantity, txn.PriceCents, txn.FeeCents, txn.TotalCents,
		string(txn.Source), timestamp(txn.ExecutedAt), txn.BrokerOrderID, txn.ExternalID,
		txn.DedupeHash, txn.Notes, timestamp(now))
	if isUniqueViolation(err) {
		return Transaction{}, ErrDuplicate
	}
	if err != nil {
		return Transaction{}, fmt.Errorf("fintech: insert transaction: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Transaction{}, fmt.Errorf("fintech: transaction id: %w", err)
	}
	out := txn
	out.ID = id
	out.CreatedAt = now
	return out, nil
}

// ListTransactions returns the whole ledger in execution order — which is the
// order the moving-average cost basis in service.go has to replay it in.
// Callers wanting newest-first for display reverse it themselves.
func (r *Repository) ListTransactions(ctx context.Context) ([]Transaction, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT t.id, t.instrument_id, i.symbol, i.asset_class, t.side, t.quantity,
		       t.price_cents, t.fee_cents, t.total_cents, t.source, t.executed_at,
		       t.broker_order_id, t.external_id, t.dedupe_hash, t.notes, t.created_at
		FROM fintech_transactions t
		JOIN fintech_instruments i ON i.id = t.instrument_id
		ORDER BY t.executed_at ASC, t.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("fintech: list transactions: %w", err)
	}
	defer rows.Close()
	return scanTransactions(rows)
}

func scanTransactions(rows *sql.Rows) ([]Transaction, error) {
	var out []Transaction
	for rows.Next() {
		var t Transaction
		var executedAt, createdAt string
		if err := rows.Scan(&t.ID, &t.InstrumentID, &t.Symbol, &t.AssetClass, &t.Side, &t.Quantity,
			&t.PriceCents, &t.FeeCents, &t.TotalCents, &t.Source, &executedAt,
			&t.BrokerOrderID, &t.ExternalID, &t.DedupeHash, &t.Notes, &createdAt); err != nil {
			return nil, fmt.Errorf("fintech: scan transaction: %w", err)
		}
		t.ExecutedAt, t.CreatedAt = parseTime(executedAt), parseTime(createdAt)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fintech: read transaction rows: %w", err)
	}
	return out, nil
}

// RecordAIUsage notes what one model call cost. A local backend's tokens are
// free and a cloud one's are not, and the panel that shows the difference is
// only as good as this row.
func (r *Repository) RecordAIUsage(ctx context.Context, kind string, usage Usage) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO fintech_ai_usage (kind, backend, model, input_tokens, output_tokens, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		kind, usage.Backend, usage.Model, usage.InputTokens, usage.OutputTokens, timestamp(time.Now()))
	if err != nil {
		return fmt.Errorf("fintech: record ai usage: %w", err)
	}
	return nil
}

// AIUsageSummary totals what the forecasting and review calls have cost, all
// time and today.
func (r *Repository) GetAIUsageSummary(ctx context.Context) (AIUsage, error) {
	var u AIUsage
	err := r.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(CASE WHEN date(created_at) = date('now') THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN date(created_at) = date('now') THEN input_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN date(created_at) = date('now') THEN output_tokens ELSE 0 END), 0)
		FROM fintech_ai_usage`,
	).Scan(&u.Calls, &u.InputTokens, &u.OutputTokens, &u.CallsToday, &u.InputTokensToday, &u.OutputTokensToday)
	if err != nil {
		return AIUsage{}, fmt.Errorf("fintech: query ai usage: %w", err)
	}
	return u, nil
}
