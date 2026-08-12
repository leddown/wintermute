package accounting

import "strings"

// InvoiceKind separates a bill from its correction. They share a table because
// they share every field and most of the logic; they draw from separate number
// sequences because the law treats them as separate document series.
type InvoiceKind string

const (
	KindInvoice    InvoiceKind = "invoice"
	KindCreditNote InvoiceKind = "credit_note"
)

// InvoiceStatus is the document lifecycle. It only ever moves forward:
// draft -> issued -> part_paid -> paid, with void as a terminal branch off
// issued. Nothing returns to draft, because a draft can be edited and an issued
// document cannot.
type InvoiceStatus string

const (
	StatusDraft    InvoiceStatus = "draft"
	StatusIssued   InvoiceStatus = "issued"
	StatusPartPaid InvoiceStatus = "part_paid"
	StatusPaid     InvoiceStatus = "paid"
	StatusVoid     InvoiceStatus = "void"
)

// Editable reports whether the document may still be changed. This is the
// single place the immutability rule is expressed.
func (s InvoiceStatus) Editable() bool { return s == StatusDraft }

// Invoice is a sales document. Totals are stored rather than recomputed: they
// are the figures that appeared on a document that has left the building, and
// recalculating them later from current VAT rates would let a rate change
// rewrite history.
type Invoice struct {
	ID            int64         `json:"id"`
	Kind          InvoiceKind   `json:"kind"`
	Number        string        `json:"number"`
	ClientID      int64         `json:"client_id"`
	EngagementID  int64         `json:"engagement_id"`
	Status        InvoiceStatus `json:"status"`
	IssueDate     string        `json:"issue_date"`
	DueDate       string        `json:"due_date"`
	TermsDays     int           `json:"terms_days"`
	Subtotal      Money         `json:"subtotal"`
	VAT           Money         `json:"vat"`
	Total         Money         `json:"total"`
	Paid          Money         `json:"paid"`
	ReverseCharge bool          `json:"reverse_charge"`
	CustomerVAT   string        `json:"customer_vat_number"`
	BillTo        string        `json:"bill_to"`
	Notes         string        `json:"notes"`
	CorrectsID    int64         `json:"corrects_id"`
	JournalEntry  int64         `json:"journal_entry_id"`
	CreatedAt     string        `json:"created_at"`
	IssuedAt      string        `json:"issued_at"`

	Lines []InvoiceLine `json:"lines"`

	// Derived for display.
	ClientName     string `json:"client_name"`
	EngagementName string `json:"engagement_name"`
}

// Outstanding is what is still owed. Negative on a credit note, which is the
// point of one.
func (i Invoice) Outstanding() Money { return i.Total - i.Paid }

// InvoiceLine is one billed item.
type InvoiceLine struct {
	ID              int64  `json:"id"`
	InvoiceID       int64  `json:"invoice_id"`
	Description     string `json:"description"`
	Quantity        Milli  `json:"quantity"`
	UnitPrice       Money  `json:"unit_price"`
	Net             Money  `json:"net"`
	VATRateID       int64  `json:"vat_rate_id"`
	VATRateBP       int64  `json:"vat_rate_bp"`
	VAT             Money  `json:"vat"`
	IncomeAccountID int64  `json:"income_account_id"`
	TimeEntryID     int64  `json:"time_entry_id"`
	ExpenseID       int64  `json:"expense_id"`
	Ordinal         int    `json:"ordinal"`
}

// InvoiceFilter narrows a listing. Empty fields are ignored.
type InvoiceFilter struct {
	ClientID     int64
	EngagementID int64
	Status       InvoiceStatus
	Kind         InvoiceKind
	From         string
	To           string
	Search       string
	Limit        int
}

// ---- service ----

func (s *Service) ListInvoices(f InvoiceFilter) ([]Invoice, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	return s.repo.ListInvoices(f)
}

func (s *Service) GetInvoice(id int64) (Invoice, error) { return s.repo.GetInvoice(id) }

// CreateDraft stores a new draft. No number is allocated and nothing is posted
// to the ledger: a draft is a working document with no accounting consequence,
// which is exactly why it may be edited freely.
func (s *Service) CreateDraft(in Invoice) (Invoice, error) {
	if in.Kind == "" {
		in.Kind = KindInvoice
	}
	if in.Kind != KindInvoice && in.Kind != KindCreditNote {
		return Invoice{}, invalid("invoice kind must be invoice or credit_note")
	}
	if err := s.prepareDraft(&in); err != nil {
		return Invoice{}, err
	}
	in.Status = StatusDraft
	in.Number = ""
	in.CreatedAt = nowStamp()
	return s.repo.CreateInvoice(in)
}

// UpdateDraft replaces a draft's contents. It refuses anything that is not a
// draft — the check that makes the immutability rule real rather than a
// convention the UI happens to follow.
func (s *Service) UpdateDraft(id int64, in Invoice) (Invoice, error) {
	existing, err := s.repo.GetInvoice(id)
	if err != nil {
		return Invoice{}, err
	}
	if !existing.Status.Editable() {
		return Invoice{}, invalid(
			"invoice %s is %s and cannot be edited; issue a credit note to correct it",
			existing.Number, existing.Status)
	}
	in.Kind = existing.Kind
	if err := s.prepareDraft(&in); err != nil {
		return Invoice{}, err
	}
	return s.repo.UpdateDraftInvoice(id, in)
}

// DeleteDraft removes a draft. Only a draft: deleting an issued invoice would
// leave a gap in the number sequence, which is the thing the sequence exists to
// make impossible.
func (s *Service) DeleteDraft(id int64) error {
	inv, err := s.repo.GetInvoice(id)
	if err != nil {
		return err
	}
	if !inv.Status.Editable() {
		return invalid("invoice %s is %s and cannot be deleted; void it instead",
			inv.Number, inv.Status)
	}
	return s.repo.DeleteDraftInvoice(id)
}

// prepareDraft normalises, validates and recomputes every derived amount. Line
// totals are always recomputed here rather than trusted from input, so a client
// that posts a wrong total cannot store one.
func (s *Service) prepareDraft(in *Invoice) error {
	if in.ClientID == 0 {
		return invalid("an invoice needs a client")
	}
	if in.IssueDate == "" {
		in.IssueDate = today()
	}
	if err := validDate(in.IssueDate); err != nil {
		return err
	}
	settings, err := s.repo.Settings()
	if err != nil {
		return err
	}
	if in.TermsDays <= 0 {
		in.TermsDays = settings.DefaultTermsDays
	}
	if in.DueDate == "" {
		in.DueDate = addDays(in.IssueDate, in.TermsDays)
	}
	if err := validDate(in.DueDate); err != nil {
		return err
	}
	if in.DueDate < in.IssueDate {
		return invalid("due date %s is before the issue date %s", in.DueDate, in.IssueDate)
	}
	if len(in.Lines) == 0 {
		return invalid("an invoice needs at least one line")
	}

	in.Notes = strings.TrimSpace(in.Notes)
	in.BillTo = strings.TrimSpace(in.BillTo)
	in.CustomerVAT = strings.TrimSpace(in.CustomerVAT)

	// Reverse charge means the customer accounts for the VAT, and their VAT
	// number has to be on the document for that to be lawful. Refusing here
	// beats printing an invoice that cannot be used.
	if in.ReverseCharge && in.CustomerVAT == "" {
		return invalid("a reverse-charge invoice must carry the customer's VAT number")
	}

	defaultIncome, err := s.repo.AccountBySystemKey(SysSales)
	if err != nil {
		return err
	}

	// A reverse-charge line charges no VAT, but it still has to carry the
	// treatment. Tagged only as 0%, the return would count it as an ordinary
	// standard-rated sale and the cross-border total would read zero.
	var reverseRate VATRate
	if in.ReverseCharge {
		reverseRate, err = s.repo.VATRateByKind(VATReverseCharge)
		if err != nil {
			return err
		}
	}

	var subtotal, vatTotal Money
	for i := range in.Lines {
		l := &in.Lines[i]
		l.Description = strings.TrimSpace(l.Description)
		if l.Description == "" {
			return invalid("line %d needs a description", i+1)
		}
		if l.Quantity == 0 {
			l.Quantity = 1000
		}
		if l.IncomeAccountID == 0 {
			l.IncomeAccountID = defaultIncome.ID
		}
		acct, err := s.repo.GetAccount(l.IncomeAccountID)
		if err != nil {
			return invalid("line %d: income account %d does not exist", i+1, l.IncomeAccountID)
		}
		if acct.Type != AccountIncome {
			return invalid("line %d: account %s (%s) is a %s account, not income",
				i+1, acct.Code, acct.Name, acct.Type)
		}

		l.Net = l.Quantity.Extend(l.UnitPrice)

		// Reverse charge overrides whatever rate was chosen: no VAT is charged,
		// and the rate is recorded as zero so the document and the return agree.
		if in.ReverseCharge {
			l.VATRateID = reverseRate.ID
			l.VATRateBP = 0
			l.VAT = 0
		} else {
			if l.VATRateID == 0 {
				l.VATRateID = settings.DefaultVATRateID
			}
			if l.VATRateID != 0 {
				rate, err := s.repo.GetVATRate(l.VATRateID)
				if err != nil {
					return invalid("line %d: VAT rate %d does not exist", i+1, l.VATRateID)
				}
				l.VATRateBP = rate.RateBP
			}
			l.VAT = VAT(l.Net, l.VATRateBP)
		}

		l.Ordinal = i
		subtotal += l.Net
		vatTotal += l.VAT
	}

	in.Subtotal = subtotal
	in.VAT = vatTotal
	in.Total = subtotal + vatTotal
	return nil
}

// Issue turns a draft into a document: it allocates the next number in the
// series, posts the entry to the ledger, and marks any time entries it billed.
//
// This is the irreversible step. Afterwards the invoice cannot be edited or
// deleted, and the number it consumed is gone from the sequence for good.
func (s *Service) Issue(id int64) (Invoice, error) {
	inv, err := s.repo.GetInvoice(id)
	if err != nil {
		return Invoice{}, err
	}
	if !inv.Status.Editable() {
		return Invoice{}, invalid("invoice %s is already %s", inv.Number, inv.Status)
	}
	if len(inv.Lines) == 0 {
		return Invoice{}, invalid("cannot issue an invoice with no lines")
	}
	if err := s.checkPeriodOpen(inv.IssueDate); err != nil {
		return Invoice{}, err
	}

	postings, err := s.invoicePostings(inv)
	if err != nil {
		return Invoice{}, err
	}
	return s.repo.IssueInvoice(id, postings)
}

// invoicePostings builds the ledger entry for an issued invoice.
//
//	Dr  Accounts Receivable      gross
//	  Cr  AccountIncome (per line)        net
//	  Cr  VAT on Sales             vat
//
// A credit note is the same shape with the sides swapped, which is why the
// amounts are negated rather than the structure duplicated.
func (s *Service) invoicePostings(inv Invoice) ([]Posting, error) {
	ar, err := s.repo.AccountBySystemKey(SysAR)
	if err != nil {
		return nil, err
	}

	sign := Money(1)
	if inv.Kind == KindCreditNote {
		sign = -1
	}

	var postings []Posting
	label := inv.Number
	if label == "" {
		label = "invoice"
	}

	// Receivable side.
	postings = append(postings, posting(ar.ID, sign*inv.Total, label+" — "+inv.ClientName, true))

	// AccountIncome, grouped by account so a twelve-line invoice does not produce
	// twelve identical credits to the same account in the ledger view.
	byAccount := map[int64]Money{}
	order := []int64{}
	for _, l := range inv.Lines {
		if _, seen := byAccount[l.IncomeAccountID]; !seen {
			order = append(order, l.IncomeAccountID)
		}
		byAccount[l.IncomeAccountID] += l.Net
	}
	for _, acct := range order {
		postings = append(postings, posting(acct, sign*byAccount[acct], label, false))
	}

	// VAT, when any was charged.
	if inv.VAT != 0 {
		vatAcct, err := s.repo.AccountBySystemKey(SysVATOutput)
		if err != nil {
			return nil, err
		}
		postings = append(postings, posting(vatAcct.ID, sign*inv.VAT, label+" VAT", false))
	}

	// The invoice total is defined as subtotal plus the sum of per-line VAT, and
	// these postings use those same figures, so the two sides agree by
	// construction. The check is here anyway: if a future change (a discount
	// line, VAT computed on the total) breaks that identity, this says so at the
	// point of the mistake.
	//
	// Deliberately not plugged into the rounding account. Absorbing a difference
	// automatically would turn a structural bug into a silent one-cent expense
	// appearing forever, which is precisely the failure this module refuses
	// everywhere else.
	var debits, credits Money
	for _, p := range postings {
		debits += p.Debit
		credits += p.Credit
	}
	if debits != credits {
		return nil, invalid(
			"invoice %s does not balance: debits %s against credits %s. "+
				"This is a bug in the invoice arithmetic, not in your data",
			label, debits, credits)
	}

	return postings, nil
}

// posting places a signed amount on the correct side. debitNormal says which
// side a positive amount belongs on, so a negated credit note flips naturally.
func posting(accountID int64, amount Money, desc string, debitNormal bool) Posting {
	if !debitNormal {
		amount = -amount
	}
	if amount >= 0 {
		return Debit(accountID, amount, desc)
	}
	return Credit(accountID, -amount, desc)
}

// Void cancels an issued invoice by reversing its ledger entry. The document
// and its number stay exactly where they are — a void invoice is a visible,
// numbered, zero-value record, not a hole in the sequence.
func (s *Service) Void(id int64, reason string) (Invoice, error) {
	inv, err := s.repo.GetInvoice(id)
	if err != nil {
		return Invoice{}, err
	}
	switch inv.Status {
	case StatusDraft:
		return Invoice{}, invalid("invoice is still a draft; delete it instead of voiding")
	case StatusVoid:
		return Invoice{}, invalid("invoice %s is already void", inv.Number)
	}
	if inv.Paid != 0 {
		return Invoice{}, invalid(
			"invoice %s has %s of payments against it; refund or credit it instead of voiding",
			inv.Number, inv.Paid)
	}
	if err := s.checkPeriodOpen(today()); err != nil {
		return Invoice{}, err
	}

	memo := "Void of " + inv.Number
	if reason = strings.TrimSpace(reason); reason != "" {
		memo += " — " + reason
	}
	if inv.JournalEntry != 0 {
		if _, err := s.Reverse(inv.JournalEntry, today(), memo); err != nil {
			return Invoice{}, err
		}
	}
	return s.repo.SetInvoiceStatus(id, StatusVoid)
}

// CreditNote raises a correction against an issued invoice. This is how an
// issued document is amended: the original stays as it was, and the pair of
// documents nets to the corrected position.
//
// With no lines supplied it credits the invoice in full, which is the common
// case and the one worth making a single click.
func (s *Service) CreditNote(invoiceID int64, lines []InvoiceLine, reason string) (Invoice, error) {
	orig, err := s.repo.GetInvoice(invoiceID)
	if err != nil {
		return Invoice{}, err
	}
	if orig.Kind != KindInvoice {
		return Invoice{}, invalid("only an invoice can be credited")
	}
	if orig.Status == StatusDraft {
		return Invoice{}, invalid("invoice is still a draft; edit it instead of crediting it")
	}
	if orig.Status == StatusVoid {
		return Invoice{}, invalid("invoice %s is void; there is nothing to credit", orig.Number)
	}

	if len(lines) == 0 {
		// Full credit: copy the lines, dropping the provenance links so the
		// time entries are not re-flagged by the credit note's own issue.
		for _, l := range orig.Lines {
			l.ID, l.InvoiceID, l.TimeEntryID, l.ExpenseID = 0, 0, 0, 0
			lines = append(lines, l)
		}
	}

	note := Invoice{
		Kind:          KindCreditNote,
		ClientID:      orig.ClientID,
		EngagementID:  orig.EngagementID,
		IssueDate:     today(),
		TermsDays:     orig.TermsDays,
		ReverseCharge: orig.ReverseCharge,
		CustomerVAT:   orig.CustomerVAT,
		BillTo:        orig.BillTo,
		Notes:         strings.TrimSpace("Credit note for " + orig.Number + ". " + reason),
		CorrectsID:    orig.ID,
		Lines:         lines,
	}
	return s.CreateDraft(note)
}

// applyPaymentStatus recomputes a document's status from what has been paid
// against it. Kept in one place so "paid" always means the same thing.
func paymentStatus(inv Invoice) InvoiceStatus {
	switch {
	case inv.Status == StatusVoid || inv.Status == StatusDraft:
		return inv.Status
	case inv.Paid == 0:
		return StatusIssued
	case inv.Paid < inv.Total:
		return StatusPartPaid
	default:
		return StatusPaid
	}
}
