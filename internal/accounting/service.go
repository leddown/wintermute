package accounting

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned when a referenced record does not exist.
var ErrNotFound = errors.New("accounting: record not found")

// ValidationError marks a bad-input error so handlers can map it to HTTP 400.
type ValidationError struct{ Msg string }

func (e ValidationError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// IsValidation reports whether err is a ValidationError.
func IsValidation(err error) bool {
	var ve ValidationError
	return errors.As(err, &ve)
}

// PeriodLockedError is returned when a write lands in a closed period. It is a
// distinct type because the fix is different from ordinary bad input: the
// operator must reopen the period or move the date, and the message needs to
// say which period stopped them.
type PeriodLockedError struct {
	Date   string
	Period Period
}

func (e PeriodLockedError) Error() string {
	return fmt.Sprintf("accounting: %s falls in a locked period (%s to %s); "+
		"reopen it or use a date outside it", e.Date, e.Period.StartsOn, e.Period.EndsOn)
}

// IsPeriodLocked reports whether err is a PeriodLockedError.
func IsPeriodLocked(err error) bool {
	var pe PeriodLockedError
	return errors.As(err, &pe)
}

// Overridable in tests for deterministic output.
var (
	nowStamp = func() string { return time.Now().UTC().Format(time.RFC3339) }
	today    = func() string { return time.Now().UTC().Format("2006-01-02") }
)

// Service holds the module's rules. Everything that writes to the ledger goes
// through it, because the balance invariant is only worth anything if there is
// exactly one door.
type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// ---- accounts ----

func (s *Service) ListAccounts(includeArchived bool) ([]Account, error) {
	return s.repo.ListAccounts(includeArchived)
}

func (s *Service) GetAccount(id int64) (Account, error) {
	return s.repo.GetAccount(id)
}

// SystemAccount resolves one of the accounts the module depends on by key.
func (s *Service) SystemAccount(key string) (Account, error) {
	return s.repo.AccountBySystemKey(key)
}

func (s *Service) CreateAccount(a Account) (Account, error) {
	normalizeAccount(&a)
	if err := validateAccount(a); err != nil {
		return Account{}, err
	}
	// A new account never claims a system key. Those are the seed's to hand
	// out; letting an arbitrary account claim "ar" would repoint receivables.
	a.SystemKey = ""
	a.CreatedAt = nowStamp()
	return s.repo.CreateAccount(a)
}

func (s *Service) UpdateAccount(id int64, a Account) (Account, error) {
	existing, err := s.repo.GetAccount(id)
	if err != nil {
		return Account{}, err
	}
	normalizeAccount(&a)
	if err := validateAccount(a); err != nil {
		return Account{}, err
	}
	if a.ParentID == id {
		return Account{}, invalid("an account cannot be its own parent")
	}
	// Changing the type of an account that has been posted to would move
	// history between the balance sheet and the P&L, restating closed periods.
	if a.Type != existing.Type {
		posted, err := s.repo.AccountHasPostings(id)
		if err != nil {
			return Account{}, err
		}
		if posted {
			return Account{}, invalid(
				"account %s has postings, so its type cannot change from %s to %s; "+
					"archive it and create a new one instead",
				existing.Code, existing.Type, a.Type)
		}
	}
	return s.repo.UpdateAccount(id, a)
}

// ArchiveAccount hides an account from pickers without removing it. Accounts
// with history are never deleted: the trial balance still joins to them.
func (s *Service) ArchiveAccount(id int64, archived bool) error {
	a, err := s.repo.GetAccount(id)
	if err != nil {
		return err
	}
	if archived && a.SystemKey != "" {
		return invalid("account %s (%s) is a system account and cannot be archived", a.Code, a.Name)
	}
	return s.repo.SetAccountArchived(id, archived)
}

func normalizeAccount(a *Account) {
	a.Code = strings.TrimSpace(a.Code)
	a.Name = strings.TrimSpace(a.Name)
	a.Description = strings.TrimSpace(a.Description)
	a.Type = AccountType(strings.ToLower(strings.TrimSpace(string(a.Type))))
}

func validateAccount(a Account) error {
	if a.Code == "" {
		return invalid("account code is required")
	}
	if a.Name == "" {
		return invalid("account name is required")
	}
	if !a.Type.valid() {
		return invalid("account type must be asset, liability, equity, income or expense")
	}
	return nil
}

// ---- VAT ----

func (s *Service) ListVATRates(includeArchived bool) ([]VATRate, error) {
	return s.repo.ListVATRates(includeArchived)
}

func (s *Service) GetVATRate(id int64) (VATRate, error) { return s.repo.GetVATRate(id) }

func (s *Service) SaveVATRate(v VATRate) (VATRate, error) {
	v.Code = strings.ToUpper(strings.TrimSpace(v.Code))
	v.Name = strings.TrimSpace(v.Name)
	if v.Kind == "" {
		v.Kind = VATStandard
	}
	if v.Code == "" {
		return VATRate{}, invalid("VAT rate code is required")
	}
	if v.Name == "" {
		return VATRate{}, invalid("VAT rate name is required")
	}
	if !v.Kind.valid() {
		return VATRate{}, invalid("VAT kind must be standard, reduced, zero, exempt or reverse_charge")
	}
	if v.RateBP < 0 || v.RateBP > 10000 {
		return VATRate{}, invalid("VAT rate must be between 0 and 10000 basis points (0–100%%)")
	}
	// A rate that is not zero contradicts the kinds that mean "no VAT charged",
	// and the mismatch would show up much later as a wrong return.
	switch v.Kind {
	case VATZero, VATExempt, VATReverseCharge:
		if v.RateBP != 0 {
			return VATRate{}, invalid("a %s rate must be 0 basis points, got %d", v.Kind, v.RateBP)
		}
	}
	return s.repo.SaveVATRate(v)
}

// ---- the ledger ----

// Post validates a set of postings and writes them as one journal entry. This
// is the only way anything reaches the ledger.
//
// The checks are deliberately unforgiving. An accounting system that repairs an
// unbalanced entry — by plugging the difference somewhere, or by writing what it
// was given and letting a report discover it later — produces books that look
// fine and are not. Refusing is the useful behaviour.
func (s *Service) Post(e JournalEntry) (JournalEntry, error) {
	if e.EntryDate == "" {
		e.EntryDate = today()
	}
	if err := validDate(e.EntryDate); err != nil {
		return JournalEntry{}, err
	}
	if e.SourceType == "" {
		e.SourceType = SourceManual
	}
	if !e.SourceType.valid() {
		return JournalEntry{}, invalid("unknown journal source %q", e.SourceType)
	}
	if err := s.checkPeriodOpen(e.EntryDate); err != nil {
		return JournalEntry{}, err
	}

	if len(e.Lines) < 2 {
		return JournalEntry{}, invalid(
			"a journal entry needs at least two lines; got %d", len(e.Lines))
	}

	var debit, credit Money
	for i, l := range e.Lines {
		n := i + 1
		if l.Debit < 0 || l.Credit < 0 {
			return JournalEntry{}, invalid("line %d: amounts cannot be negative; "+
				"put the amount on the other side instead", n)
		}
		if (l.Debit == 0) == (l.Credit == 0) {
			return JournalEntry{}, invalid(
				"line %d: exactly one of debit and credit must be set", n)
		}
		acct, err := s.repo.GetAccount(l.AccountID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return JournalEntry{}, invalid("line %d: no account with id %d", n, l.AccountID)
			}
			return JournalEntry{}, err
		}
		if acct.Archived {
			return JournalEntry{}, invalid("line %d: account %s (%s) is archived",
				n, acct.Code, acct.Name)
		}
		debit += l.Debit
		credit += l.Credit
	}

	if debit != credit {
		return JournalEntry{}, invalid(
			"entry does not balance: debits %s, credits %s, difference %s",
			debit, credit, debit-credit)
	}
	if debit == 0 {
		return JournalEntry{}, invalid("entry has no value")
	}

	e.Memo = strings.TrimSpace(e.Memo)
	e.CreatedAt = nowStamp()
	return s.repo.PostEntry(e)
}

// PostFrom is the constructor the document code uses: it turns a slice of
// postings into an entry and posts it.
func (s *Service) PostFrom(date, memo string, src JournalSource, srcID int64, postings []Posting) (JournalEntry, error) {
	e := JournalEntry{
		EntryDate:  date,
		Memo:       memo,
		SourceType: src,
		SourceID:   srcID,
	}
	for _, p := range postings {
		// Postings that net to nothing are dropped rather than rejected: a
		// zero-VAT line is a normal thing for a caller to build unconditionally,
		// and the database would refuse the empty line.
		if p.Debit == 0 && p.Credit == 0 {
			continue
		}
		e.Lines = append(e.Lines, JournalLine{
			AccountID:   p.AccountID,
			Debit:       p.Debit,
			Credit:      p.Credit,
			Description: p.Description,
		})
	}
	return s.Post(e)
}

// Reverse posts the mirror image of an entry. Corrections work this way rather
// than by editing or deleting, so the original stays readable and the audit
// trail shows what happened and when.
func (s *Service) Reverse(entryID int64, date, memo string) (JournalEntry, error) {
	orig, err := s.repo.GetEntry(entryID)
	if err != nil {
		return JournalEntry{}, err
	}
	if date == "" {
		date = today()
	}
	if memo == "" {
		memo = "Reversal of entry " + fmt.Sprint(orig.ID)
	}
	rev := JournalEntry{
		EntryDate:  date,
		Memo:       memo,
		SourceType: orig.SourceType,
		SourceID:   orig.SourceID,
		ReversesID: orig.ID,
	}
	for _, l := range orig.Lines {
		rev.Lines = append(rev.Lines, JournalLine{
			AccountID:   l.AccountID,
			Debit:       l.Credit,
			Credit:      l.Debit,
			Description: l.Description,
		})
	}
	return s.Post(rev)
}

func (s *Service) GetEntry(id int64) (JournalEntry, error) { return s.repo.GetEntry(id) }

func (s *Service) ListEntries(f EntryFilter) ([]JournalEntry, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	return s.repo.ListEntries(f)
}

// ---- periods ----

func (s *Service) ListPeriods() ([]Period, error) { return s.repo.ListPeriods() }

// LockPeriod closes a window. Locking is not retroactive validation: it does
// not check what is already in there, it stops anything new arriving.
func (s *Service) LockPeriod(p Period) (Period, error) {
	if err := validDate(p.StartsOn); err != nil {
		return Period{}, err
	}
	if err := validDate(p.EndsOn); err != nil {
		return Period{}, err
	}
	if p.EndsOn < p.StartsOn {
		return Period{}, invalid("period ends (%s) before it starts (%s)", p.EndsOn, p.StartsOn)
	}
	if p.Locked {
		p.LockedAt = nowStamp()
	} else {
		p.LockedAt = ""
	}
	p.Note = strings.TrimSpace(p.Note)
	return s.repo.SavePeriod(p)
}

func (s *Service) checkPeriodOpen(date string) error {
	p, locked, err := s.repo.LockCovering(date)
	if err != nil {
		return err
	}
	if locked {
		return PeriodLockedError{Date: date, Period: p}
	}
	return nil
}

// ---- settings ----

func (s *Service) Settings() (Settings, error) { return s.repo.Settings() }

func (s *Service) SaveSettings(in Settings) (Settings, error) {
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	in.CurrencySymbol = strings.TrimSpace(in.CurrencySymbol)
	if len(in.Currency) != 3 {
		return Settings{}, invalid("currency must be a three-letter code, got %q", in.Currency)
	}
	if in.DefaultTermsDays < 0 || in.DefaultTermsDays > 365 {
		return Settings{}, invalid("payment terms must be between 0 and 365 days")
	}
	if in.FiscalYearStartMonth < 1 || in.FiscalYearStartMonth > 12 {
		return Settings{}, invalid("fiscal year start month must be between 1 and 12")
	}
	if in.DefaultVATRateID != 0 {
		if _, err := s.repo.GetVATRate(in.DefaultVATRateID); err != nil {
			return Settings{}, invalid("default VAT rate does not exist")
		}
	}
	in.UpdatedAt = nowStamp()
	return s.repo.SaveSettings(in)
}

// ---- shared helpers ----

func validDate(s string) error {
	if s == "" {
		return invalid("a date is required (YYYY-MM-DD)")
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return invalid("date %q is not YYYY-MM-DD", s)
	}
	return nil
}

// addDays returns a date string offset from another, used for due dates.
func addDays(date string, days int) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date
	}
	return t.AddDate(0, 0, days).Format("2006-01-02")
}
