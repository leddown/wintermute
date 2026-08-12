package accounting

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"wintermute/internal/tool"
)

// Tools the model can call. These are thin adapters over the same Service
// methods the HTTP layer uses, so an invoice the model raises goes through the
// same validation, the same period checks and the same ledger posting as one
// typed into the UI.
//
// On risk levels, which are the part worth getting right:
//
//   - Reads are RiskRead. Looking at a balance changes nothing.
//   - Recording a payment or an expense is RiskWrite. Both are reversible: the
//     module posts a reversal rather than deleting, and the operator can undo
//     one without consequence outside the books.
//   - Issuing an invoice is RiskDestructive, and that is not a typo. It is not
//     destructive in the sense of losing data — it is irreversible. It consumes
//     a number from a legally gap-free sequence, posts to the ledger, marks
//     timesheet entries as billed, and produces a document that cannot be
//     edited or deleted afterwards. Under-declaring it would let `-yes` push
//     invoices out of the door without anyone seeing them first, which is the
//     one confirmation an operator most wants to keep.
//
// Deliberately absent: anything that edits the chart of accounts, changes VAT
// rates, or locks a period. Those are setup decisions with consequences the
// model has no way to weigh, and the UI is the right place for them.
func Register(reg *tool.Registry, svc *Service) error {
	tools := []struct {
		def     tool.Definition
		handler tool.Handler
	}{
		{
			def: tool.Definition{
				Name: "accounting_overview",
				Description: "The current financial position: cash, what clients owe, what is overdue, " +
					"unbilled time waiting to go out, income and profit so far this year, and the VAT " +
					"position. Start here for any question about how the business is doing.",
				Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
				Risk:       tool.RiskRead,
			},
			handler: overviewHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "list_invoices",
				Description: "List invoices and credit notes, newest first. Filter by client, status " +
					"(draft, issued, part_paid, paid, void) or date range. Use this before raising an " +
					"invoice so an existing draft is edited rather than duplicated.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"client_id": {"type": "integer", "description": "Only this CRM client's documents."},
						"status": {"type": "string", "description": "draft, issued, part_paid, paid or void."},
						"from": {"type": "string", "description": "Earliest issue date, YYYY-MM-DD."},
						"to": {"type": "string", "description": "Latest issue date, YYYY-MM-DD."},
						"search": {"type": "string", "description": "Match on number, client name or notes."}
					}
				}`),
				Risk: tool.RiskRead,
			},
			handler: listInvoicesHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "get_invoice",
				Description: "One invoice in full, with its lines, VAT, payments applied and what is " +
					"still outstanding.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {"invoice_id": {"type": "integer", "description": "The invoice's id."}},
					"required": ["invoice_id"]
				}`),
				Risk: tool.RiskRead,
			},
			handler: getInvoiceHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "list_unbilled_time",
				Description: "Billable time logged in the CRM that has not reached an invoice, with " +
					"hours and value. Use this to answer 'what is ready to bill' and before drafting " +
					"an invoice from time.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"client_id": {"type": "integer", "description": "Only this client's work."},
						"engagement_id": {"type": "integer", "description": "Only this engagement."},
						"up_to": {"type": "string", "description": "Only work on or before this date, YYYY-MM-DD."}
					}
				}`),
				Risk: tool.RiskRead,
			},
			handler: unbilledHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "draft_invoice_from_time",
				Description: "Build a DRAFT invoice from a client's unbilled time, one line per " +
					"timesheet entry. Nothing is sent, nothing is posted and no number is used: the " +
					"draft can be reviewed and edited, and must be issued separately. Prefer this over " +
					"writing invoice lines by hand, because it keeps the link back to the timesheet.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"client_id": {"type": "integer", "description": "The CRM client to bill."},
						"engagement_id": {"type": "integer", "description": "Restrict to one engagement."},
						"up_to": {"type": "string", "description": "Only bill work on or before this date, YYYY-MM-DD."}
					}
				}`),
				Risk: tool.RiskWrite,
			},
			handler: draftFromTimeHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "issue_invoice",
				Description: "Issue a draft invoice. This is IRREVERSIBLE: it allocates the next " +
					"number in a sequence that must have no gaps, posts the entry to the ledger, and " +
					"marks the timesheet entries as billed. Afterwards the invoice cannot be edited or " +
					"deleted — only voided or corrected with a credit note. Show the draft to the user " +
					"and get an explicit go-ahead before calling this.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {"invoice_id": {"type": "integer", "description": "The draft's id."}},
					"required": ["invoice_id"]
				}`),
				Risk: tool.RiskDestructive,
			},
			handler: issueInvoiceHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "record_payment",
				Description: "Record money received against an issued invoice. Partial payments are " +
					"fine; paying more than is outstanding is refused. The invoice's status follows " +
					"from the total paid.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"invoice_id": {"type": "integer", "description": "The invoice being paid."},
						"amount": {"type": "string", "description": "Amount received, as a decimal such as \"1210.00\"."},
						"paid_on": {"type": "string", "description": "Date received, YYYY-MM-DD. Defaults to today."},
						"reference": {"type": "string", "description": "Bank reference or note."},
						"method": {"type": "string", "description": "bank, card, cash. Defaults to bank."}
					},
					"required": ["invoice_id", "amount"]
				}`),
				Risk: tool.RiskWrite,
			},
			handler: recordPaymentHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "record_expense",
				Description: "Record a business cost. Needs a category account — call " +
					"list_accounts first and choose one, rather than guessing a code. Recoverable VAT " +
					"is split out automatically; set vat_reclaimable false for things like " +
					"entertainment where it is not.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"vendor": {"type": "string", "description": "Who was paid."},
						"description": {"type": "string", "description": "What it was for."},
						"account_id": {"type": "integer", "description": "Expense category account id."},
						"paid_from_id": {"type": "integer", "description": "Bank or card account id. Defaults to the main bank account."},
						"net": {"type": "string", "description": "Amount before VAT, as a decimal such as \"100.00\"."},
						"vat_rate_id": {"type": "integer", "description": "VAT rate id; omit for no VAT."},
						"vat_reclaimable": {"type": "boolean", "description": "Whether the VAT can be reclaimed. Defaults to true."},
						"spent_on": {"type": "string", "description": "Date, YYYY-MM-DD. Defaults to today."},
						"billable": {"type": "boolean", "description": "Whether it will be rebilled to a client."},
						"client_id": {"type": "integer", "description": "Required when billable."}
					},
					"required": ["account_id", "net"]
				}`),
				Risk: tool.RiskWrite,
			},
			handler: recordExpenseHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "list_accounts",
				Description: "The chart of accounts: code, name and type. Call this before recording " +
					"an expense or reading a report, so categories are chosen from what exists.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"type": {"type": "string", "description": "Filter to asset, liability, equity, income or expense."}
					}
				}`),
				Risk: tool.RiskRead,
			},
			handler: listAccountsHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "financial_report",
				Description: "Run a report: profit_and_loss, balance_sheet, trial_balance, " +
					"aged_receivables or vat_return. All are computed from the ledger. The VAT return " +
					"is a summary to fill a form in from, not a filing.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"report": {"type": "string", "description": "profit_and_loss, balance_sheet, trial_balance, aged_receivables or vat_return."},
						"from": {"type": "string", "description": "Period start, YYYY-MM-DD. Required for vat_return."},
						"to": {"type": "string", "description": "Period end, YYYY-MM-DD. Required for vat_return."},
						"as_of": {"type": "string", "description": "Date for balance_sheet and aged_receivables. Defaults to today."}
					},
					"required": ["report"]
				}`),
				Risk: tool.RiskRead,
			},
			handler: reportHandler(svc),
		},
	}

	for _, t := range tools {
		if err := reg.Register(t.def, t.handler); err != nil {
			return err
		}
	}
	return nil
}

// ---- handlers ----

func overviewHandler(svc *Service) tool.Handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		d, err := svc.Dashboard()
		if err != nil {
			return "", err
		}
		set, err := svc.Settings()
		if err != nil {
			return "", err
		}
		cur := set.Currency

		var b strings.Builder
		fmt.Fprintf(&b, "Position as at %s (%s)\n\n", d.AsOf, cur)
		fmt.Fprintf(&b, "  Bank balance:        %s\n", d.BankBalance)
		fmt.Fprintf(&b, "  Owed by clients:     %s (%s overdue)\n", d.OutstandingTotal, d.OverdueTotal)
		fmt.Fprintf(&b, "  Unbilled time:       %s across %s hours\n", d.UnbilledTimeAmount, d.UnbilledTimeHours)
		fmt.Fprintf(&b, "  Draft invoices:      %d\n", d.DraftInvoiceCount)
		fmt.Fprintf(&b, "\n  Income this year:    %s\n", d.IncomeThisYear)
		fmt.Fprintf(&b, "  Expenses this year:  %s\n", d.ExpenseThisYear)
		fmt.Fprintf(&b, "  Profit this year:    %s\n", d.ProfitThisYear)
		if d.VATPosition >= 0 {
			fmt.Fprintf(&b, "\n  VAT payable:         %s\n", d.VATPosition)
		} else {
			fmt.Fprintf(&b, "\n  VAT reclaimable:     %s\n", -d.VATPosition)
		}
		if len(d.RecentInvoices) > 0 {
			b.WriteString("\nRecent invoices:\n")
			for _, inv := range d.RecentInvoices {
				num := inv.Number
				if num == "" {
					num = "(draft)"
				}
				fmt.Fprintf(&b, "  %-10s %-24s %10s  %s\n", num, truncate(inv.ClientName, 24), inv.Total, inv.Status)
			}
		}
		return b.String(), nil
	}
}

func listInvoicesHandler(svc *Service) tool.Handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var in struct {
			ClientID int64  `json:"client_id"`
			Status   string `json:"status"`
			From     string `json:"from"`
			To       string `json:"to"`
			Search   string `json:"search"`
		}
		if err := unmarshal(raw, &in); err != nil {
			return "", err
		}
		invoices, err := svc.ListInvoices(InvoiceFilter{
			ClientID: in.ClientID, Status: InvoiceStatus(in.Status),
			From: in.From, To: in.To, Search: in.Search,
		})
		if err != nil {
			return "", err
		}
		if len(invoices) == 0 {
			return "No invoices match.", nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d invoice(s):\n", len(invoices))
		for _, inv := range invoices {
			num := inv.Number
			if num == "" {
				num = fmt.Sprintf("draft #%d", inv.ID)
			}
			fmt.Fprintf(&b, "  [%d] %-12s %-22s %-10s issued %s  total %s  outstanding %s\n",
				inv.ID, num, truncate(inv.ClientName, 22), inv.Status,
				dashIfEmpty(inv.IssueDate), inv.Total, inv.Outstanding())
		}
		return b.String(), nil
	}
}

func getInvoiceHandler(svc *Service) tool.Handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var in struct {
			InvoiceID int64 `json:"invoice_id"`
		}
		if err := unmarshal(raw, &in); err != nil {
			return "", err
		}
		inv, err := svc.GetInvoice(in.InvoiceID)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		num := inv.Number
		if num == "" {
			num = fmt.Sprintf("draft #%d", inv.ID)
		}
		fmt.Fprintf(&b, "%s — %s (%s)\n", num, inv.ClientName, inv.Status)
		fmt.Fprintf(&b, "Issued %s, due %s\n\n", dashIfEmpty(inv.IssueDate), dashIfEmpty(inv.DueDate))
		for _, l := range inv.Lines {
			fmt.Fprintf(&b, "  %-40s %8s x %10s = %10s (VAT %s)\n",
				truncate(l.Description, 40), l.Quantity, l.UnitPrice, l.Net, l.VAT)
		}
		fmt.Fprintf(&b, "\n  Subtotal %s, VAT %s, total %s\n", inv.Subtotal, inv.VAT, inv.Total)
		fmt.Fprintf(&b, "  Paid %s, outstanding %s\n", inv.Paid, inv.Outstanding())
		if inv.ReverseCharge {
			fmt.Fprintf(&b, "  Reverse charge — customer VAT number %s\n", inv.CustomerVAT)
		}
		return b.String(), nil
	}
}

func unbilledHandler(svc *Service) tool.Handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var in struct {
			ClientID     int64  `json:"client_id"`
			EngagementID int64  `json:"engagement_id"`
			UpTo         string `json:"up_to"`
		}
		if err := unmarshal(raw, &in); err != nil {
			return "", err
		}
		entries, err := svc.UnbilledTime(UnbilledFilter{
			ClientID: in.ClientID, EngagementID: in.EngagementID, UpTo: in.UpTo,
		})
		if err != nil {
			return "", err
		}
		if len(entries) == 0 {
			return "There is no unbilled billable time matching that.", nil
		}
		var total Money
		var hours Milli
		var b strings.Builder
		for _, e := range entries {
			total += e.Amount
			hours += e.Hours
			fmt.Fprintf(&b, "  %s  %-20s %-28s %6sh at %8s = %9s\n",
				e.EntryDate, truncate(e.ClientName, 20), truncate(e.Description, 28),
				e.Hours, e.Rate, e.Amount)
		}
		return fmt.Sprintf("%d unbilled entries, %s hours, %s in total:\n%s",
			len(entries), hours, total, b.String()), nil
	}
}

func draftFromTimeHandler(svc *Service) tool.Handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var in struct {
			ClientID     int64  `json:"client_id"`
			EngagementID int64  `json:"engagement_id"`
			UpTo         string `json:"up_to"`
		}
		if err := unmarshal(raw, &in); err != nil {
			return "", err
		}
		inv, err := svc.DraftFromUnbilledTime(UnbilledFilter{
			ClientID: in.ClientID, EngagementID: in.EngagementID, UpTo: in.UpTo,
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(
			"Drafted invoice #%d for %s: %d lines, subtotal %s, VAT %s, total %s. "+
				"It has no number and nothing has been posted — issue it to send it out.",
			inv.ID, inv.ClientName, len(inv.Lines), inv.Subtotal, inv.VAT, inv.Total), nil
	}
}

func issueInvoiceHandler(svc *Service) tool.Handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var in struct {
			InvoiceID int64 `json:"invoice_id"`
		}
		if err := unmarshal(raw, &in); err != nil {
			return "", err
		}
		inv, err := svc.Issue(in.InvoiceID)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(
			"Issued %s to %s for %s, due %s. Posted to the ledger; the invoice can no longer "+
				"be edited or deleted.", inv.Number, inv.ClientName, inv.Total, inv.DueDate), nil
	}
}

func recordPaymentHandler(svc *Service) tool.Handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var in struct {
			InvoiceID int64  `json:"invoice_id"`
			Amount    string `json:"amount"`
			PaidOn    string `json:"paid_on"`
			Reference string `json:"reference"`
			Method    string `json:"method"`
		}
		if err := unmarshal(raw, &in); err != nil {
			return "", err
		}
		amount, err := ParseMoney(in.Amount)
		if err != nil {
			return "", invalid("amount: %s", err)
		}
		p, err := svc.RecordPayment(Payment{
			InvoiceID: in.InvoiceID, Amount: amount, PaidOn: in.PaidOn,
			Reference: in.Reference, Method: in.Method,
		})
		if err != nil {
			return "", err
		}
		inv, err := svc.GetInvoice(p.InvoiceID)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Recorded %s against %s on %s. Invoice is now %s; %s outstanding.",
			p.Amount, p.InvoiceNumber, p.PaidOn, inv.Status, inv.Outstanding()), nil
	}
}

func recordExpenseHandler(svc *Service) tool.Handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var in struct {
			Vendor         string `json:"vendor"`
			Description    string `json:"description"`
			AccountID      int64  `json:"account_id"`
			PaidFromID     int64  `json:"paid_from_id"`
			Net            string `json:"net"`
			VATRateID      int64  `json:"vat_rate_id"`
			VATReclaimable *bool  `json:"vat_reclaimable"`
			SpentOn        string `json:"spent_on"`
			Billable       bool   `json:"billable"`
			ClientID       int64  `json:"client_id"`
		}
		if err := unmarshal(raw, &in); err != nil {
			return "", err
		}
		net, err := ParseMoney(in.Net)
		if err != nil {
			return "", invalid("net: %s", err)
		}
		reclaimable := true
		if in.VATReclaimable != nil {
			reclaimable = *in.VATReclaimable
		}
		e, err := svc.RecordExpense(Expense{
			Vendor: in.Vendor, Description: in.Description, AccountID: in.AccountID,
			PaidFromID: in.PaidFromID, Net: net, VATRateID: in.VATRateID,
			VATReclaimable: reclaimable, SpentOn: in.SpentOn,
			Billable: in.Billable, ClientID: in.ClientID,
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Recorded %s to %s on %s: net %s, VAT %s, total %s, paid from %s.",
			dashIfEmpty(e.Vendor), e.AccountName, e.SpentOn, e.Net, e.VAT, e.Total, e.PaidFromName), nil
	}
}

func listAccountsHandler(svc *Service) tool.Handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var in struct {
			Type string `json:"type"`
		}
		if err := unmarshal(raw, &in); err != nil {
			return "", err
		}
		accounts, err := svc.ListAccounts(false)
		if err != nil {
			return "", err
		}
		want := AccountType(strings.ToLower(strings.TrimSpace(in.Type)))
		var b strings.Builder
		for _, a := range accounts {
			if want != "" && a.Type != want {
				continue
			}
			fmt.Fprintf(&b, "  [%d] %-6s %-30s %s\n", a.ID, a.Code, a.Name, a.Type)
		}
		if b.Len() == 0 {
			return "No accounts match.", nil
		}
		return "Chart of accounts:\n" + b.String(), nil
	}
}

func reportHandler(svc *Service) tool.Handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var in struct {
			Report string `json:"report"`
			From   string `json:"from"`
			To     string `json:"to"`
			AsOf   string `json:"as_of"`
		}
		if err := unmarshal(raw, &in); err != nil {
			return "", err
		}

		var b strings.Builder
		switch strings.ToLower(strings.TrimSpace(in.Report)) {
		case "profit_and_loss", "profit_loss", "pl":
			pl, err := svc.ProfitAndLoss(in.From, in.To)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "Profit and loss, %s to %s\n\nIncome\n", pl.From, pl.To)
			for _, l := range pl.Income {
				fmt.Fprintf(&b, "  %-6s %-30s %12s\n", l.Code, l.Name, l.Amount)
			}
			fmt.Fprintf(&b, "  %-37s %12s\n\nExpenses\n", "Total income", pl.TotalIncome)
			for _, l := range pl.Expenses {
				fmt.Fprintf(&b, "  %-6s %-30s %12s\n", l.Code, l.Name, l.Amount)
			}
			fmt.Fprintf(&b, "  %-37s %12s\n\n  %-37s %12s\n",
				"Total expenses", pl.TotalExpenses, "Net profit", pl.NetProfit)

		case "balance_sheet", "balance":
			bs, err := svc.BalanceSheet(in.AsOf)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "Balance sheet as at %s\n\nAssets\n", bs.AsOf)
			for _, l := range bs.Assets {
				fmt.Fprintf(&b, "  %-6s %-30s %12s\n", l.Code, l.Name, l.Amount)
			}
			fmt.Fprintf(&b, "  %-37s %12s\n\nLiabilities\n", "Total assets", bs.TotalAssets)
			for _, l := range bs.Liabilities {
				fmt.Fprintf(&b, "  %-6s %-30s %12s\n", l.Code, l.Name, l.Amount)
			}
			fmt.Fprintf(&b, "  %-37s %12s\n\nEquity\n", "Total liabilities", bs.TotalLiabilities)
			for _, l := range bs.Equity {
				fmt.Fprintf(&b, "  %-6s %-30s %12s\n", l.Code, l.Name, l.Amount)
			}
			fmt.Fprintf(&b, "  %-37s %12s\n  %-37s %12s\n",
				"Earnings not yet closed", bs.CurrentEarnings, "Total equity",
				bs.TotalEquity+bs.CurrentEarnings)
			if !bs.Balanced {
				b.WriteString("\n  WARNING: this balance sheet does not balance.\n")
			}

		case "trial_balance", "trial":
			tb, err := svc.TrialBalance(in.From, in.To)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "Trial balance, %s to %s\n\n", tb.From, tb.To)
			for _, r := range tb.Rows {
				fmt.Fprintf(&b, "  %-6s %-30s %12s %12s\n", r.Code, r.Name, r.Debit, r.Credit)
			}
			fmt.Fprintf(&b, "  %-37s %12s %12s\n", "", tb.TotalDebit, tb.TotalCredit)
			if !tb.Balanced {
				b.WriteString("\n  WARNING: the ledger does not balance.\n")
			}

		case "aged_receivables", "aging", "ageing":
			ar, err := svc.AgedReceivables(in.AsOf)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "Aged receivables as at %s\n\n", ar.AsOf)
			fmt.Fprintf(&b, "  %-24s %10s %10s %10s %10s %10s %10s\n",
				"Client", "Current", "1-30", "31-60", "61-90", "90+", "Total")
			for _, row := range ar.Rows {
				k := row.Buckets
				fmt.Fprintf(&b, "  %-24s %10s %10s %10s %10s %10s %10s\n",
					truncate(row.ClientName, 24), k.Current, k.Days30, k.Days60, k.Days90, k.Older, k.Total)
			}
			k := ar.Totals
			fmt.Fprintf(&b, "  %-24s %10s %10s %10s %10s %10s %10s\n",
				"", k.Current, k.Days30, k.Days60, k.Days90, k.Older, k.Total)

		case "vat_return", "vat":
			if in.From == "" || in.To == "" {
				return "", invalid("a VAT return needs a from and to date")
			}
			v, err := svc.VATReturnSummary(in.From, in.To)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "VAT summary, %s to %s\n\n", v.From, v.To)
			fmt.Fprintf(&b, "  Standard-rated sales   %12s\n", v.NetSalesStandard)
			fmt.Fprintf(&b, "  Zero-rated sales       %12s\n", v.NetSalesZero)
			fmt.Fprintf(&b, "  Exempt sales           %12s\n", v.NetSalesExempt)
			fmt.Fprintf(&b, "  Reverse-charge sales   %12s\n", v.NetSalesReverse)
			fmt.Fprintf(&b, "  Output VAT             %12s\n\n", v.OutputVAT)
			fmt.Fprintf(&b, "  Purchases              %12s\n", v.NetPurchases)
			fmt.Fprintf(&b, "  Input VAT              %12s\n\n", v.InputVAT)
			if v.NetDue >= 0 {
				fmt.Fprintf(&b, "  Payable                %12s\n", v.NetDue)
			} else {
				fmt.Fprintf(&b, "  Reclaimable            %12s\n", -v.NetDue)
			}
			if v.Note != "" {
				fmt.Fprintf(&b, "\n  %s\n", v.Note)
			}
			b.WriteString("\n  This is a summary to fill a return in from, not a filing.\n")

		default:
			return "", invalid(
				"unknown report %q; choose profit_and_loss, balance_sheet, trial_balance, "+
					"aged_receivables or vat_return", in.Report)
		}
		return b.String(), nil
	}
}

// ---- helpers ----

// unmarshal tolerates an absent argument object, which is what a model sends
// for a tool whose parameters are all optional.
func unmarshal(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return invalid("could not read the tool arguments: %s", err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
