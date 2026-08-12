package api

import (
	"errors"
	"net/http"
	"strconv"

	"wintermute/internal/accounting"
)

// Accounting routes. Same bearer-token middleware as everything else.
//
// The split between what is a POST and what is a PUT follows the module's own
// rule rather than REST habit: a draft invoice is a mutable resource and takes
// PUT, while issuing, voiding and crediting are POSTs to sub-paths because they
// are events with consequences, not edits to a field.

func (s *Server) registerAccountingRoutes(authed func(string, http.HandlerFunc)) {
	if s.workspace.Accounting == nil {
		return
	}
	authed("GET /api/v1/accounting/dashboard", s.handleAcctDashboard)
	authed("GET /api/v1/accounting/settings", s.handleAcctGetSettings)
	authed("PUT /api/v1/accounting/settings", s.handleAcctSaveSettings)

	authed("GET /api/v1/accounting/accounts", s.handleAcctListAccounts)
	authed("POST /api/v1/accounting/accounts", s.handleAcctCreateAccount)
	authed("PUT /api/v1/accounting/accounts/{id}", s.handleAcctUpdateAccount)
	authed("POST /api/v1/accounting/accounts/{id}/archive", s.handleAcctArchiveAccount)

	authed("GET /api/v1/accounting/vat-rates", s.handleAcctListVATRates)
	authed("PUT /api/v1/accounting/vat-rates", s.handleAcctSaveVATRate)

	authed("GET /api/v1/accounting/journal", s.handleAcctListEntries)
	authed("GET /api/v1/accounting/journal/{id}", s.handleAcctGetEntry)
	authed("POST /api/v1/accounting/journal", s.handleAcctPostEntry)
	authed("POST /api/v1/accounting/journal/{id}/reverse", s.handleAcctReverseEntry)

	authed("GET /api/v1/accounting/invoices", s.handleAcctListInvoices)
	authed("GET /api/v1/accounting/invoices/{id}", s.handleAcctGetInvoice)
	authed("POST /api/v1/accounting/invoices", s.handleAcctCreateDraft)
	authed("PUT /api/v1/accounting/invoices/{id}", s.handleAcctUpdateDraft)
	authed("DELETE /api/v1/accounting/invoices/{id}", s.handleAcctDeleteDraft)
	authed("POST /api/v1/accounting/invoices/{id}/issue", s.handleAcctIssueInvoice)
	authed("POST /api/v1/accounting/invoices/{id}/void", s.handleAcctVoidInvoice)
	authed("POST /api/v1/accounting/invoices/{id}/credit", s.handleAcctCreditInvoice)

	authed("GET /api/v1/accounting/unbilled", s.handleAcctUnbilledTime)
	authed("POST /api/v1/accounting/unbilled/draft", s.handleAcctDraftFromTime)

	authed("GET /api/v1/accounting/payments", s.handleAcctListPayments)
	authed("POST /api/v1/accounting/payments", s.handleAcctRecordPayment)
	authed("DELETE /api/v1/accounting/payments/{id}", s.handleAcctDeletePayment)

	authed("GET /api/v1/accounting/expenses", s.handleAcctListExpenses)
	authed("POST /api/v1/accounting/expenses", s.handleAcctRecordExpense)
	authed("DELETE /api/v1/accounting/expenses/{id}", s.handleAcctDeleteExpense)

	authed("GET /api/v1/accounting/reports/trial-balance", s.handleAcctTrialBalance)
	authed("GET /api/v1/accounting/reports/profit-loss", s.handleAcctProfitAndLoss)
	authed("GET /api/v1/accounting/reports/balance-sheet", s.handleAcctBalanceSheet)
	authed("GET /api/v1/accounting/reports/aged-receivables", s.handleAcctAgedReceivables)
	authed("GET /api/v1/accounting/reports/vat", s.handleAcctVATReturn)

	authed("GET /api/v1/accounting/periods", s.handleAcctListPeriods)
	authed("POST /api/v1/accounting/periods", s.handleAcctSavePeriod)
}

// acctError maps the module's three error kinds. A locked period is its own
// status: 409 says "the request was fine, the books are closed", where a 400
// would send the operator looking for a typo.
func (s *Server) acctError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, accounting.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case accounting.IsPeriodLocked(err):
		writeError(w, http.StatusConflict, err.Error())
	case accounting.IsValidation(err):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.fail(w, op, err)
	}
}

// ---- dashboard and settings ----

func (s *Server) handleAcctDashboard(w http.ResponseWriter, r *http.Request) {
	d, err := s.workspace.Accounting.Dashboard()
	if err != nil {
		s.acctError(w, "accounting dashboard", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleAcctGetSettings(w http.ResponseWriter, r *http.Request) {
	set, err := s.workspace.Accounting.Settings()
	if err != nil {
		s.acctError(w, "accounting settings", err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

func (s *Server) handleAcctSaveSettings(w http.ResponseWriter, r *http.Request) {
	var in accounting.Settings
	if !decode(w, r, &in) {
		return
	}
	set, err := s.workspace.Accounting.SaveSettings(in)
	if err != nil {
		s.acctError(w, "save accounting settings", err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

// ---- chart of accounts ----

func (s *Server) handleAcctListAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.workspace.Accounting.ListAccounts(r.URL.Query().Get("archived") == "1")
	if err != nil {
		s.acctError(w, "list accounts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

func (s *Server) handleAcctCreateAccount(w http.ResponseWriter, r *http.Request) {
	var in accounting.Account
	if !decode(w, r, &in) {
		return
	}
	out, err := s.workspace.Accounting.CreateAccount(in)
	if err != nil {
		s.acctError(w, "create account", err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleAcctUpdateAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in accounting.Account
	if !decode(w, r, &in) {
		return
	}
	out, err := s.workspace.Accounting.UpdateAccount(id, in)
	if err != nil {
		s.acctError(w, "update account", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAcctArchiveAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Archived bool `json:"archived"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.workspace.Accounting.ArchiveAccount(id, body.Archived); err != nil {
		s.acctError(w, "archive account", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"archived": body.Archived})
}

// ---- VAT rates ----

func (s *Server) handleAcctListVATRates(w http.ResponseWriter, r *http.Request) {
	rates, err := s.workspace.Accounting.ListVATRates(r.URL.Query().Get("archived") == "1")
	if err != nil {
		s.acctError(w, "list vat rates", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rates": rates})
}

func (s *Server) handleAcctSaveVATRate(w http.ResponseWriter, r *http.Request) {
	var in accounting.VATRate
	if !decode(w, r, &in) {
		return
	}
	out, err := s.workspace.Accounting.SaveVATRate(in)
	if err != nil {
		s.acctError(w, "save vat rate", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- journal ----

func (s *Server) handleAcctListEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	entries, err := s.workspace.Accounting.ListEntries(accounting.EntryFilter{
		From:      q.Get("from"),
		To:        q.Get("to"),
		AccountID: queryID(q.Get("account_id")),
		Limit:     int(queryID(q.Get("limit"))),
	})
	if err != nil {
		s.acctError(w, "list journal", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleAcctGetEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	e, err := s.workspace.Accounting.GetEntry(id)
	if err != nil {
		s.acctError(w, "get journal entry", err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleAcctPostEntry(w http.ResponseWriter, r *http.Request) {
	var in accounting.JournalEntry
	if !decode(w, r, &in) {
		return
	}
	out, err := s.workspace.Accounting.Post(in)
	if err != nil {
		s.acctError(w, "post journal entry", err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleAcctReverseEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Date string `json:"date"`
		Memo string `json:"memo"`
	}
	if !decode(w, r, &body) {
		return
	}
	out, err := s.workspace.Accounting.Reverse(id, body.Date, body.Memo)
	if err != nil {
		s.acctError(w, "reverse journal entry", err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// ---- invoices ----

func (s *Server) handleAcctListInvoices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	invoices, err := s.workspace.Accounting.ListInvoices(accounting.InvoiceFilter{
		ClientID:     queryID(q.Get("client_id")),
		EngagementID: queryID(q.Get("engagement_id")),
		Status:       accounting.InvoiceStatus(q.Get("status")),
		Kind:         accounting.InvoiceKind(q.Get("kind")),
		From:         q.Get("from"),
		To:           q.Get("to"),
		Search:       q.Get("search"),
		Limit:        int(queryID(q.Get("limit"))),
	})
	if err != nil {
		s.acctError(w, "list invoices", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoices": invoices})
}

func (s *Server) handleAcctGetInvoice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	inv, err := s.workspace.Accounting.GetInvoice(id)
	if err != nil {
		s.acctError(w, "get invoice", err)
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

func (s *Server) handleAcctCreateDraft(w http.ResponseWriter, r *http.Request) {
	var in accounting.Invoice
	if !decode(w, r, &in) {
		return
	}
	out, err := s.workspace.Accounting.CreateDraft(in)
	if err != nil {
		s.acctError(w, "create invoice draft", err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleAcctUpdateDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in accounting.Invoice
	if !decode(w, r, &in) {
		return
	}
	out, err := s.workspace.Accounting.UpdateDraft(id, in)
	if err != nil {
		s.acctError(w, "update invoice draft", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAcctDeleteDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.workspace.Accounting.DeleteDraft(id); err != nil {
		s.acctError(w, "delete invoice draft", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAcctIssueInvoice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	out, err := s.workspace.Accounting.Issue(id)
	if err != nil {
		s.acctError(w, "issue invoice", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAcctVoidInvoice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	out, err := s.workspace.Accounting.Void(id, body.Reason)
	if err != nil {
		s.acctError(w, "void invoice", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAcctCreditInvoice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason string                   `json:"reason"`
		Lines  []accounting.InvoiceLine `json:"lines"`
	}
	if !decode(w, r, &body) {
		return
	}
	out, err := s.workspace.Accounting.CreditNote(id, body.Lines, body.Reason)
	if err != nil {
		s.acctError(w, "credit invoice", err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// ---- unbilled time ----

func (s *Server) handleAcctUnbilledTime(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	entries, err := s.workspace.Accounting.UnbilledTime(accounting.UnbilledFilter{
		ClientID:     queryID(q.Get("client_id")),
		EngagementID: queryID(q.Get("engagement_id")),
		UpTo:         q.Get("up_to"),
	})
	if err != nil {
		s.acctError(w, "unbilled time", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleAcctDraftFromTime(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientID     int64  `json:"client_id"`
		EngagementID int64  `json:"engagement_id"`
		UpTo         string `json:"up_to"`
	}
	if !decode(w, r, &body) {
		return
	}
	out, err := s.workspace.Accounting.DraftFromUnbilledTime(accounting.UnbilledFilter{
		ClientID:     body.ClientID,
		EngagementID: body.EngagementID,
		UpTo:         body.UpTo,
	})
	if err != nil {
		s.acctError(w, "draft from unbilled time", err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// ---- payments ----

func (s *Server) handleAcctListPayments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	payments, err := s.workspace.Accounting.ListPayments(accounting.PaymentFilter{
		InvoiceID: queryID(q.Get("invoice_id")),
		ClientID:  queryID(q.Get("client_id")),
		From:      q.Get("from"),
		To:        q.Get("to"),
	})
	if err != nil {
		s.acctError(w, "list payments", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"payments": payments})
}

func (s *Server) handleAcctRecordPayment(w http.ResponseWriter, r *http.Request) {
	var in accounting.Payment
	if !decode(w, r, &in) {
		return
	}
	out, err := s.workspace.Accounting.RecordPayment(in)
	if err != nil {
		s.acctError(w, "record payment", err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleAcctDeletePayment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.workspace.Accounting.DeletePayment(id); err != nil {
		s.acctError(w, "delete payment", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- expenses ----

func (s *Server) handleAcctListExpenses(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	expenses, err := s.workspace.Accounting.ListExpenses(accounting.ExpenseFilter{
		From:         q.Get("from"),
		To:           q.Get("to"),
		AccountID:    queryID(q.Get("account_id")),
		ClientID:     queryID(q.Get("client_id")),
		BillableOnly: q.Get("billable") == "1",
		Unrebilled:   q.Get("unrebilled") == "1",
		Search:       q.Get("search"),
	})
	if err != nil {
		s.acctError(w, "list expenses", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"expenses": expenses})
}

func (s *Server) handleAcctRecordExpense(w http.ResponseWriter, r *http.Request) {
	var in accounting.Expense
	if !decode(w, r, &in) {
		return
	}
	out, err := s.workspace.Accounting.RecordExpense(in)
	if err != nil {
		s.acctError(w, "record expense", err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleAcctDeleteExpense(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.workspace.Accounting.DeleteExpense(id); err != nil {
		s.acctError(w, "delete expense", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- reports ----

func (s *Server) handleAcctTrialBalance(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tb, err := s.workspace.Accounting.TrialBalance(q.Get("from"), q.Get("to"))
	if err != nil {
		s.acctError(w, "trial balance", err)
		return
	}
	writeJSON(w, http.StatusOK, tb)
}

func (s *Server) handleAcctProfitAndLoss(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pl, err := s.workspace.Accounting.ProfitAndLoss(q.Get("from"), q.Get("to"))
	if err != nil {
		s.acctError(w, "profit and loss", err)
		return
	}
	writeJSON(w, http.StatusOK, pl)
}

func (s *Server) handleAcctBalanceSheet(w http.ResponseWriter, r *http.Request) {
	bs, err := s.workspace.Accounting.BalanceSheet(r.URL.Query().Get("as_of"))
	if err != nil {
		s.acctError(w, "balance sheet", err)
		return
	}
	writeJSON(w, http.StatusOK, bs)
}

func (s *Server) handleAcctAgedReceivables(w http.ResponseWriter, r *http.Request) {
	aging, err := s.workspace.Accounting.AgedReceivables(r.URL.Query().Get("as_of"))
	if err != nil {
		s.acctError(w, "aged receivables", err)
		return
	}
	writeJSON(w, http.StatusOK, aging)
}

func (s *Server) handleAcctVATReturn(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v, err := s.workspace.Accounting.VATReturnSummary(q.Get("from"), q.Get("to"))
	if err != nil {
		s.acctError(w, "vat return", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ---- periods ----

func (s *Server) handleAcctListPeriods(w http.ResponseWriter, r *http.Request) {
	periods, err := s.workspace.Accounting.ListPeriods()
	if err != nil {
		s.acctError(w, "list periods", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"periods": periods})
}

func (s *Server) handleAcctSavePeriod(w http.ResponseWriter, r *http.Request) {
	var in accounting.Period
	if !decode(w, r, &in) {
		return
	}
	out, err := s.workspace.Accounting.LockPeriod(in)
	if err != nil {
		s.acctError(w, "save period", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// queryID parses an optional numeric query parameter. A missing or malformed
// value means "no filter" rather than an error: a stale bookmark should show
// everything, not a 400.
func queryID(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
