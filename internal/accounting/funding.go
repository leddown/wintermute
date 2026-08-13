package accounting

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// FundingKind separates the two ways an owner can put money in, and the one way
// it comes back out. The distinction is not presentational: capital is equity
// the business does not owe, a loan is a liability it does, and which one a
// deposit was cannot be recovered from the amount or the date afterwards. So it
// is recorded per event, and it decides which account the other side lands in.
type FundingKind string

const (
	FundingCapital   FundingKind = "capital"
	FundingLoan      FundingKind = "loan"
	FundingRepayment FundingKind = "repayment"
)

func (k FundingKind) valid() bool {
	switch k {
	case FundingCapital, FundingLoan, FundingRepayment:
		return true
	}
	return false
}

// MovesIn reports whether this kind brings money into the business. Only a
// repayment does not.
func (k FundingKind) MovesIn() bool { return k != FundingRepayment }

// Funding is one movement of the owner's own money: capital introduced, a loan
// made to the business, or a repayment of that loan.
//
// It is a source in the same sense invoices and payments are — it posts a
// balanced entry and stores its id — rather than a balance anything adjusts.
type Funding struct {
	ID         int64       `json:"id"`
	ReceivedOn string      `json:"received_on"`
	Kind       FundingKind `json:"kind"`
	Amount     Money       `json:"amount"`
	FromName   string      `json:"from_name"`
	Reference  string      `json:"reference"`
	Note       string      `json:"note"`
	// CashAccountID is the asset account the money moved through: into it for
	// capital and a loan, out of it for a repayment.
	CashAccountID int64 `json:"cash_account_id"`
	// OwnerAccountID is the other side — equity for capital, the loan liability
	// for a loan or repayment. Derived from the kind unless the caller names one.
	OwnerAccountID int64  `json:"owner_account_id"`
	JournalEntry   int64  `json:"journal_entry_id"`
	CreatedAt      string `json:"created_at"`

	// Derived for display.
	CashAccountName  string `json:"cash_account_name"`
	OwnerAccountName string `json:"owner_account_name"`
}

// FundingFilter narrows a listing.
type FundingFilter struct {
	Kind  FundingKind
	From  string
	To    string
	Limit int
}

// ---- service ----

func (s *Service) ListFunding(f FundingFilter) ([]Funding, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	if f.Kind != "" && !f.Kind.valid() {
		return nil, invalid("funding kind must be capital, loan or repayment")
	}
	return s.repo.ListFunding(f)
}

// RecordFunding books money the owner put in, or took back, and posts it.
//
//	capital    Dr  Bank                 Cr  Owner Capital     (equity)
//	loan       Dr  Bank                 Cr  Loan from Owner   (liability)
//	repayment  Dr  Loan from Owner      Cr  Bank
//
// There is no default kind. Booking an unlabelled deposit as capital would
// understate what the business owes, and as a loan would invent a debt — both
// are wrong in a way that surfaces at tax time rather than here, so the caller
// has to say which it was.
func (s *Service) RecordFunding(f Funding) (Funding, error) {
	if f.Kind == "" {
		return Funding{}, invalid(
			"say which kind of funding this is: capital (equity, not repayable), " +
				"loan (repayable), or repayment")
	}
	if !f.Kind.valid() {
		return Funding{}, invalid("funding kind must be capital, loan or repayment, got %q", f.Kind)
	}
	if f.Amount <= 0 {
		return Funding{}, invalid("funding must be for a positive amount")
	}

	if f.ReceivedOn == "" {
		f.ReceivedOn = today()
	}
	if err := validDate(f.ReceivedOn); err != nil {
		return Funding{}, err
	}
	if err := s.checkPeriodOpen(f.ReceivedOn); err != nil {
		return Funding{}, err
	}

	if f.CashAccountID == 0 {
		bank, err := s.repo.AccountBySystemKey(SysBank)
		if err != nil {
			return Funding{}, err
		}
		f.CashAccountID = bank.ID
	}
	cash, err := s.repo.GetAccount(f.CashAccountID)
	if err != nil {
		return Funding{}, invalid("cash account %d does not exist", f.CashAccountID)
	}
	if cash.Type != AccountAsset {
		return Funding{}, invalid(
			"funding must move through an asset account; %s (%s) is a %s account",
			cash.Code, cash.Name, cash.Type)
	}

	owner, err := s.fundingOwnerAccount(f)
	if err != nil {
		return Funding{}, err
	}
	f.OwnerAccountID = owner.ID

	// A repayment cannot exceed what is owed. Letting it through would leave the
	// loan account with a debit balance — the business having "lent" the owner
	// money it never received — which reads on the balance sheet as an asset
	// nobody created and is exactly the kind of thing that is noticed a year
	// later. Capital is not guarded the same way: reducing equity is drawings,
	// a different event with different tax treatment, and not this.
	if f.Kind == FundingRepayment {
		outstanding, err := s.repo.OwnerLoanBalance(f.ReceivedOn)
		if err != nil {
			return Funding{}, err
		}
		if outstanding <= 0 {
			return Funding{}, invalid(
				"there is no outstanding owner loan at %s to repay", f.ReceivedOn)
		}
		if f.Amount > outstanding {
			return Funding{}, invalid(
				"repayment of %s exceeds the %s outstanding on the owner loan at %s; "+
					"repay at most the balance, or record the excess as drawings",
				f.Amount, outstanding, f.ReceivedOn)
		}
	}

	f.FromName = strings.TrimSpace(f.FromName)
	if f.FromName == "" {
		f.FromName = "Owner"
	}
	f.Reference = strings.TrimSpace(f.Reference)
	f.Note = strings.TrimSpace(f.Note)
	f.CreatedAt = nowStamp()

	memo := fundingMemo(f)
	var postings []Posting
	if f.Kind.MovesIn() {
		postings = []Posting{
			Debit(f.CashAccountID, f.Amount, memo),
			Credit(owner.ID, f.Amount, memo),
		}
	} else {
		postings = []Posting{
			Debit(owner.ID, f.Amount, memo),
			Credit(f.CashAccountID, f.Amount, memo),
		}
	}
	return s.repo.RecordFunding(f, memo, postings)
}

// fundingOwnerAccount resolves the equity or liability side. The kind decides
// it, because that is the whole point of recording the kind; a caller may still
// name an account — a partnership keeps one capital account per partner — but
// only one whose type matches what the kind means.
func (s *Service) fundingOwnerAccount(f Funding) (Account, error) {
	want := AccountEquity
	key := SysCapital
	if f.Kind != FundingCapital {
		want = AccountLiability
		key = SysOwnerLoan
	}

	if f.OwnerAccountID == 0 {
		return s.repo.AccountBySystemKey(key)
	}

	a, err := s.repo.GetAccount(f.OwnerAccountID)
	if err != nil {
		return Account{}, invalid("account %d does not exist", f.OwnerAccountID)
	}
	if a.Type != want {
		return Account{}, invalid(
			"%s funding posts to a %s account; %s (%s) is a %s account",
			f.Kind, want, a.Code, a.Name, a.Type)
	}
	if a.Archived {
		return Account{}, invalid("account %s (%s) is archived", a.Code, a.Name)
	}
	return a, nil
}

func fundingMemo(f Funding) string {
	switch f.Kind {
	case FundingCapital:
		return fmt.Sprintf("Capital introduced — %s", f.FromName)
	case FundingLoan:
		return fmt.Sprintf("Loan from %s", f.FromName)
	default:
		return fmt.Sprintf("Loan repayment to %s", f.FromName)
	}
}

// OwnerLoanOutstanding is what the business still owes the owner as of a date:
// everything credited to the loan account less everything repaid out of it.
// Empty asOf means today.
func (s *Service) OwnerLoanOutstanding(asOf string) (Money, error) {
	if asOf == "" {
		asOf = today()
	}
	if err := validDate(asOf); err != nil {
		return 0, err
	}
	return s.repo.OwnerLoanBalance(asOf)
}

// DeleteFunding removes the record and reverses its entry, on the same reasoning
// as DeletePayment: this is an operational record, not an issued document, and a
// mistyped amount should not need a correcting entry written by hand.
func (s *Service) DeleteFunding(id int64) error {
	f, err := s.repo.GetFunding(id)
	if err != nil {
		return err
	}
	if err := s.checkPeriodOpen(f.ReceivedOn); err != nil {
		return err
	}
	// Removing a loan that has since been repaid would drive the liability
	// negative — the mirror of the repayment guard, and wrong for the same
	// reason. Delete the repayments first.
	if f.Kind == FundingLoan {
		outstanding, err := s.repo.OwnerLoanBalance(today())
		if err != nil {
			return err
		}
		if outstanding < f.Amount {
			return invalid(
				"cannot remove a %s loan: only %s is still outstanding, so reversing it "+
					"would leave the owner loan account overdrawn; remove the repayments first",
				f.Amount, outstanding)
		}
	}
	if f.JournalEntry != 0 {
		if _, err := s.Reverse(f.JournalEntry, today(),
			"Reversal of "+strings.ToLower(fundingMemo(f))); err != nil {
			return err
		}
	}
	return s.repo.DeleteFunding(id)
}

// ---- repository ----

const fundingColumns = `f.id, f.received_on, f.kind, f.amount_minor, f.from_name,
	f.reference, f.note, f.cash_account_id, f.owner_account_id, f.journal_entry_id,
	f.created_at, COALESCE(ca.name,''), COALESCE(oa.name,'')`

const fundingFrom = ` FROM acct_funding f
	LEFT JOIN acct_accounts ca ON ca.id = f.cash_account_id
	LEFT JOIN acct_accounts oa ON oa.id = f.owner_account_id`

func scanFunding(row interface{ Scan(...any) error }) (Funding, error) {
	var f Funding
	var kind string
	err := row.Scan(&f.ID, &f.ReceivedOn, &kind, &f.Amount, &f.FromName,
		&f.Reference, &f.Note, &f.CashAccountID, &f.OwnerAccountID,
		&f.JournalEntry, &f.CreatedAt, &f.CashAccountName, &f.OwnerAccountName)
	f.Kind = FundingKind(kind)
	return f, err
}

func (r *SQLiteRepository) GetFunding(id int64) (Funding, error) {
	f, err := scanFunding(r.db.QueryRow(`SELECT `+fundingColumns+fundingFrom+
		` WHERE f.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Funding{}, ErrNotFound
	}
	return f, err
}

func (r *SQLiteRepository) ListFunding(f FundingFilter) ([]Funding, error) {
	var where []string
	var args []any
	if f.Kind != "" {
		where = append(where, "f.kind = ?")
		args = append(args, string(f.Kind))
	}
	if f.From != "" {
		where = append(where, "f.received_on >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		where = append(where, "f.received_on <= ?")
		args = append(args, f.To)
	}

	q := `SELECT ` + fundingColumns + fundingFrom
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += fmt.Sprintf(" ORDER BY f.received_on DESC, f.id DESC LIMIT %d", f.Limit)

	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list funding: %w", err)
	}
	defer rows.Close()

	out := []Funding{}
	for rows.Next() {
		fn, err := scanFunding(rows)
		if err != nil {
			return nil, fmt.Errorf("list funding: %w", err)
		}
		out = append(out, fn)
	}
	return out, rows.Err()
}

// OwnerLoanBalance reads what is owed straight off the ledger rather than
// summing the funding table. The table is a source, and a loan repaid by a
// manual journal entry — which is how it will happen at least once — is real
// whether or not this module wrote it.
func (r *SQLiteRepository) OwnerLoanBalance(asOf string) (Money, error) {
	var bal Money
	err := r.db.QueryRow(`
		SELECT COALESCE(SUM(l.credit_minor), 0) - COALESCE(SUM(l.debit_minor), 0)
		FROM acct_journal_lines l
		JOIN acct_journal_entries e ON e.id = l.entry_id
		JOIN acct_accounts a ON a.id = l.account_id
		WHERE a.system_key = ? AND e.entry_date <= ?`, SysOwnerLoan, asOf).Scan(&bal)
	if err != nil {
		return 0, fmt.Errorf("owner loan balance: %w", err)
	}
	return bal, nil
}

// RecordFunding writes the record and its ledger entry in one transaction, for
// the same reason RecordPayment does: either both happened or neither did.
func (r *SQLiteRepository) RecordFunding(f Funding, memo string, postings []Posting) (Funding, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return Funding{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	entry := JournalEntry{
		EntryDate:  f.ReceivedOn,
		Memo:       memo,
		SourceType: SourceFunding,
		CreatedAt:  f.CreatedAt,
	}
	for _, ps := range postings {
		entry.Lines = append(entry.Lines, JournalLine{
			AccountID: ps.AccountID, Debit: ps.Debit, Credit: ps.Credit,
			Description: ps.Description,
		})
	}
	entryID, err := insertEntryTx(tx, entry)
	if err != nil {
		return Funding{}, err
	}

	res, err := tx.Exec(`INSERT INTO acct_funding
		(received_on, kind, amount_minor, from_name, reference, note,
		 cash_account_id, owner_account_id, journal_entry_id, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		f.ReceivedOn, string(f.Kind), int64(f.Amount), f.FromName, f.Reference,
		f.Note, f.CashAccountID, f.OwnerAccountID, entryID, f.CreatedAt)
	if err != nil {
		return Funding{}, mapConstraint(err, "funding")
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Funding{}, err
	}
	if _, err := tx.Exec(`UPDATE acct_journal_entries SET source_id = ? WHERE id = ?`,
		id, entryID); err != nil {
		return Funding{}, err
	}
	if err := tx.Commit(); err != nil {
		return Funding{}, err
	}
	return r.GetFunding(id)
}

func (r *SQLiteRepository) DeleteFunding(id int64) error {
	_, err := r.db.Exec(`DELETE FROM acct_funding WHERE id = ?`, id)
	return err
}
