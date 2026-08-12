// Package accounting is a double-entry general ledger with EU VAT invoicing,
// payments and expenses, sized for a consultancy of a handful of people.
//
// The shape follows what the mature open-source systems converge on — Bigcapital
// routes every money event through one ledger module that writes balanced
// debit/credit entries atomically; GnuCash keeps typed accounts with a normal
// balance derived from the type; Beancount refuses a transaction that does not
// balance rather than repairing it. This package takes all three positions.
//
// The rule that matters: invoices, payments and expenses are *sources*. Each
// one posts a balanced journal entry and records its id. Nothing sums an
// invoice table to learn what revenue was, and nothing adjusts a balance
// directly. Reports read the ledger, and only the ledger.
package accounting

// AccountType fixes an account's normal balance and which statement it lands
// on. These five are the whole vocabulary; there is no sixth.
type AccountType string

const (
	AccountAsset     AccountType = "asset"
	AccountLiability AccountType = "liability"
	AccountEquity    AccountType = "equity"
	AccountIncome    AccountType = "income"
	AccountExpense   AccountType = "expense"
)

// DebitNormal reports whether a debit increases this kind of account. Assets
// and expenses grow by debit; liabilities, equity and income grow by credit.
// Reports derive presentation sign from this rather than storing one.
func (t AccountType) DebitNormal() bool { return t == AccountAsset || t == AccountExpense }

// OnBalanceSheet reports whether the type belongs on the balance sheet rather
// than the profit and loss.
func (t AccountType) OnBalanceSheet() bool {
	return t == AccountAsset || t == AccountLiability || t == AccountEquity
}

func (t AccountType) valid() bool {
	switch t {
	case AccountAsset, AccountLiability, AccountEquity, AccountIncome, AccountExpense:
		return true
	}
	return false
}

// System account keys. The code needs to find a handful of accounts without
// hardcoding a code number the operator is free to renumber or rename.
const (
	SysBank      = "bank"       // default asset account money lands in
	SysAR        = "ar"         // accounts receivable
	SysAP        = "ap"         // accounts payable
	SysVATOutput = "vat_output" // VAT charged on sales, owed to the authority
	SysVATInput  = "vat_input"  // VAT paid on purchases, reclaimable
	SysSales     = "sales"      // default income account for invoice lines
	SysRecharged = "recharged"  // rebilled client costs
	SysRetained  = "retained"   // retained earnings
	SysRounding  = "rounding"   // sub-cent VAT residue
)

// Account is one line of the chart of accounts.
type Account struct {
	ID          int64       `json:"id"`
	Code        string      `json:"code"`
	Name        string      `json:"name"`
	Type        AccountType `json:"type"`
	ParentID    int64       `json:"parent_id"`
	SystemKey   string      `json:"system_key"`
	Description string      `json:"description"`
	Archived    bool        `json:"archived"`
	CreatedAt   string      `json:"created_at"`

	// Derived by balance queries, in minor units.
	DebitTotal  Money `json:"debit_total"`
	CreditTotal Money `json:"credit_total"`
	// Balance is signed in the account's own normal direction: positive means
	// "more of what this account normally holds". A reader never has to know
	// which side a type sits on to read it.
	Balance Money `json:"balance"`
}

// VATKind separates cases that share a 0% rate but are not the same line on a
// VAT return. Collapsing them loses the distinction the return depends on.
type VATKind string

const (
	VATStandard      VATKind = "standard"
	VATReduced       VATKind = "reduced"
	VATZero          VATKind = "zero"
	VATExempt        VATKind = "exempt"
	VATReverseCharge VATKind = "reverse_charge"
)

func (k VATKind) valid() bool {
	switch k {
	case VATStandard, VATReduced, VATZero, VATExempt, VATReverseCharge:
		return true
	}
	return false
}

// VATRate is a rate in basis points: 2100 is 21%.
type VATRate struct {
	ID       int64   `json:"id"`
	Code     string  `json:"code"`
	Name     string  `json:"name"`
	RateBP   int64   `json:"rate_bp"`
	Kind     VATKind `json:"kind"`
	Archived bool    `json:"archived"`
}

// JournalSource records what caused an entry, so a posting is always traceable
// to the document that produced it.
type JournalSource string

const (
	SourceManual     JournalSource = "manual"
	SourceOpening    JournalSource = "opening"
	SourceInvoice    JournalSource = "invoice"
	SourceCreditNote JournalSource = "credit_note"
	SourcePayment    JournalSource = "payment"
	SourceExpense    JournalSource = "expense"
)

func (s JournalSource) valid() bool {
	switch s {
	case SourceManual, SourceOpening, SourceInvoice, SourceCreditNote, SourcePayment, SourceExpense:
		return true
	}
	return false
}

// JournalEntry is one balanced transaction. It is never edited or deleted once
// posted; a mistake is corrected by posting its reversal, which is what
// ReversesID records.
type JournalEntry struct {
	ID         int64         `json:"id"`
	EntryDate  string        `json:"entry_date"`
	Memo       string        `json:"memo"`
	SourceType JournalSource `json:"source_type"`
	SourceID   int64         `json:"source_id"`
	ReversesID int64         `json:"reverses_id"`
	CreatedAt  string        `json:"created_at"`

	Lines []JournalLine `json:"lines"`
}

// Total returns the entry's debit total, which for a balanced entry is also its
// credit total and the figure a human means by "how big was it".
func (e JournalEntry) Total() Money {
	var sum Money
	for _, l := range e.Lines {
		sum += l.Debit
	}
	return sum
}

// Balanced reports whether debits equal credits. An unbalanced entry is never
// persisted, so this is a precondition check rather than a property of stored
// data.
func (e JournalEntry) Balanced() bool {
	var debit, credit Money
	for _, l := range e.Lines {
		debit += l.Debit
		credit += l.Credit
	}
	return debit == credit
}

// JournalLine is one side of one entry. Exactly one of Debit and Credit is
// non-zero; the database enforces it too, so a line that contributes nothing to
// a trial balance cannot be written by accident.
type JournalLine struct {
	ID          int64  `json:"id"`
	EntryID     int64  `json:"entry_id"`
	AccountID   int64  `json:"account_id"`
	Debit       Money  `json:"debit"`
	Credit      Money  `json:"credit"`
	Description string `json:"description"`
	Ordinal     int    `json:"ordinal"`

	// Derived for display.
	AccountCode string      `json:"account_code"`
	AccountName string      `json:"account_name"`
	AccountType AccountType `json:"account_type"`
}

// Period is a window that has been closed. Nothing may be posted into a locked
// one, including — especially — a backdated entry.
type Period struct {
	ID       int64  `json:"id"`
	StartsOn string `json:"starts_on"`
	EndsOn   string `json:"ends_on"`
	Locked   bool   `json:"locked"`
	LockedAt string `json:"locked_at"`
	Note     string `json:"note"`
}

// Settings is the single-row module configuration. Single currency by design:
// no FX rates, no revaluation, no gain/loss accounts.
type Settings struct {
	Currency             string `json:"currency"`
	CurrencySymbol       string `json:"currency_symbol"`
	DefaultTermsDays     int    `json:"default_terms_days"`
	DefaultVATRateID     int64  `json:"default_vat_rate_id"`
	FiscalYearStartMonth int    `json:"fiscal_year_start_month"`
	UpdatedAt            string `json:"updated_at"`
}

// Posting is the input to the ledger: an account and an amount on one side.
// Callers build a slice of these and hand it to the service, which balances,
// validates and writes them as one entry or not at all.
type Posting struct {
	AccountID   int64
	Debit       Money
	Credit      Money
	Description string
}

// Debit is a convenience constructor for a debit posting.
func Debit(accountID int64, amount Money, desc string) Posting {
	return Posting{AccountID: accountID, Debit: amount, Description: desc}
}

// Credit is a convenience constructor for a credit posting.
func Credit(accountID int64, amount Money, desc string) Posting {
	return Posting{AccountID: accountID, Credit: amount, Description: desc}
}
