# Accounting

`internal/accounting` is a double-entry general ledger with EU VAT invoicing,
payments and expenses, sized for a consultancy of a handful of people. It sits
next to the CRM and uses it as the customer list rather than keeping its own.

## Why this shape

A survey of the mature open-source systems shows the same core, and each
contributes something worth copying:

- **Bigcapital** routes *every* money event — invoice, bill, expense, manual
  journal — through one ledger module that writes balanced debit/credit entries
  atomically, enforcing double entry at the domain model rather than in
  reporting. That is the structural idea this module is built on.
- **GnuCash** keeps typed accounts whose normal balance follows from the type.
- **Beancount** refuses a transaction that does not balance instead of
  repairing it. Strictness is a feature in books.
- **Akaunting / Dolibarr / Odoo** show the SMB feature envelope, and where it
  bloats: inventory, warehouses and payroll are not a five-person consultancy's
  problem.

EU VAT law supplies two hard constraints rather than preferences: invoice
numbers must be unique, sequential and **gap-free**, and an issued invoice may
not be deleted or amended — corrections go through credit notes.

## Three rules

**1. Every balance comes from the ledger.** Invoices, payments and expenses are
*sources*. Each posts a balanced journal entry and stores its id. No report sums
an invoice table, and no writer adjusts a balance directly. A report that
summed documents would quietly disagree with the ledger the moment anything was
posted by hand.

**2. Money is `int64` minor units.** The CRM next door stores rates and hours as
`REAL`, which is fine for "roughly how much is outstanding" and unusable here: a
ledger's whole claim is that debits equal credits exactly, and binary floating
point cannot keep that promise across a few thousand rows. Conversion happens
once, at the CRM boundary, in `crm_bridge.go`. Hours become `Milli` — exact
thousandths — before ever touching a price.

**3. Issued documents are immutable.** `InvoiceStatus.Editable()` is the single
place that rule lives. Drafts carry no number and have no accounting
consequence, which is exactly why they can be edited freely.

## Entities

| Table | Notes |
|-------|-------|
| `acct_accounts` | Chart of accounts. `type` fixes the normal balance; `system_key` names the handful the code must find without hardcoding a code number. |
| `acct_journal_entries` / `_lines` | The ledger. A line carries exactly one of debit/credit, enforced by CHECK. |
| `acct_invoices` / `_lines` | Invoices and credit notes. Totals stored, not recomputed. Lines carry `time_entry_id` provenance. |
| `acct_payments` | Receipts against an invoice; partial payments are the norm. |
| `acct_funding` | The owner's own money in and out. `kind` separates capital from a loan, which decides the account the other side posts to. |
| `acct_expenses` | Costs, with recoverable VAT split out. |
| `acct_vat_rates` | Rates in basis points. `kind` separates zero-rated, exempt and reverse charge, which share 0% but are different lines on a return. |
| `acct_sequences` | Gap-free numbering, read and bumped inside the issuing transaction. |
| `acct_periods` | Closed windows. Nothing posts into a locked one, including — especially — a backdated entry. |
| `acct_settings` | One row. Single currency by design. |

## The billing loop

```
CRM time entry ──> draft invoice ──> issue ──> payment
     (unbilled)      (editable)     (immutable)
```

`DraftFromUnbilledTime` builds one invoice line per time entry, carrying its id.
That keeps the document readable against the timesheet and is what lets issuing
flag exactly the right entries. Rolling the hours into a single "Consulting
services" line would lose both.

Two conditions stop double-billing: the CRM's own `invoiced` flag, and a
`NOT EXISTS` against invoice lines that covers the window between drafting an
invoice and issuing it. Lines on a void invoice are excluded from that guard,
because voiding is what releases the hours again.

## Issuing

`Issue` is the irreversible step, and everything it does is one transaction:

1. allocate the next number from `acct_sequences`
2. post the ledger entry — `Dr` receivable, `Cr` income per account, `Cr` output VAT
3. stamp the invoice with its number, status and entry id
4. flag the CRM time entries it billed

If any of it fails, none of it happened. That matters most for the number: one
allocated by a transaction that then rolled back would leave a gap in a sequence
required by law to have none.

Afterwards the invoice cannot be edited or deleted. `Void` reverses the ledger
entry and keeps the number — a void invoice is a numbered zero-value record, not
a hole. `CreditNote` raises a correction; the pair nets to the corrected
position.

## VAT

Rounding is **per line**, which is the EU invoicing convention: each line shows
its own VAT and the total is the sum of what is printed. This deliberately does
not always equal VAT computed on the total —
`TestPerLineVATCanDifferFromTotalVAT` pins the behaviour with a worked example.

Reverse charge sets the line rate to zero *and* tags the line with the
reverse-charge treatment. Tagged only as 0%, the return would count it as an
ordinary standard-rated sale. A reverse-charge invoice without the customer's
VAT number is refused, because the document would be unusable.

`VATReturnSummary` totals the period from the documents and then cross-checks
the two control accounts. A mismatch produces a note naming both figures rather
than being silently reconciled — it means something was posted to a VAT account
by hand, and that is worth knowing before filing. It is a summary to fill a
return in from, not a filing.

## Owner funding

Before there is anything to invoice, the money in the bank got there because
somebody put it there. That event had no home here until `acct_funding`: the
only way to record it was a hand-written journal entry, which asks the operator
to know double entry — the one thing this module exists to avoid asking.

The distinction the table is built around is **capital versus loan**, and it is
not presentational:

| Kind | Posts | Because |
|------|-------|---------|
| `capital` | `Dr` bank, `Cr` **3000 Owner Capital** (equity) | The business does not owe it back. |
| `loan` | `Dr` bank, `Cr` **2500 Loan from Owner** (liability) | It does owe it back, and the balance is the answer to "how much". |
| `repayment` | `Dr` 2500, `Cr` bank | The loan running down. |

Recording a loan as capital understates liabilities; recording capital as a loan
invents a debt. Neither is visible in the amount or the date afterwards, so the
kind is stored per event and **there is no default** — `RecordFunding` refuses a
deposit that does not say which it was, and the tool description tells the model
to ask rather than guess.

A repayment is a kind rather than a negative amount. A negative amount would
defeat the CHECK that stops a typo posting a credit where a debit belongs, and
"what was repaid" should not require the reader to know a sign convention.

**A repayment cannot exceed what is outstanding.** The balance is read from the
ledger as of the repayment's own date, not summed from this table — a loan
repaid by a manual journal entry is real whether or not this module wrote it,
and a loan made *after* the repayment should not retroactively fund it. Letting
an over-repayment through would leave the liability with a debit balance: the
business appearing to have lent the owner money it never received. Deleting a
loan that has since been partly repaid is refused for the same reason, from the
other end.

Drawings — the owner taking money out that was never a loan — are deliberately
not here. That is a different event with different tax treatment, and `3100
Owner Drawings` is where it goes, by manual entry.

> **Renaming for a limited company.** The seeded names are sole-trader shaped.
> If you incorporate, rename 3000 to *Share Capital* and 2500 to *Director's
> Loan Account* in the chart of accounts. The system keys `capital` and
> `owner_loan` are what the code resolves, and they survive a rename — only the
> `system_key` is load-bearing, never the code or the name.

## Reports

Trial balance, profit and loss, balance sheet, aged receivables and the VAT
summary, all read from the ledger. Accrual basis.

The balance sheet carries `CurrentEarnings` explicitly: this module posts no
year-end closing entry, so profit not yet moved to retained earnings has to
appear or the sheet does not balance. `Balanced` is computed, not asserted — if
it is ever false, something wrote to the ledger outside this package.

## API

`/api/v1/accounting/*`, behind the same bearer token as everything else.
`POST` for events with consequences (issue, void, credit), `PUT` for editing a
draft. A locked period returns **409**, not 400: the request was fine, the books
are closed.

`GET /api/v1/accounting/funding` returns the rows *and* `loan_outstanding`.
The balance ships with the listing rather than being summed in the browser, so
there is one implementation of "still owed" and not two that can disagree.

## Agent tools

Eleven tools on the shared registry. The risk levels are the approval policy:

| Risk | Tools |
|------|-------|
| `RiskRead` | `accounting_overview`, `list_invoices`, `get_invoice`, `list_unbilled_time`, `list_accounts`, `financial_report` |
| `RiskWrite` | `draft_invoice_from_time`, `record_payment`, `record_expense`, `record_funding` |
| `RiskDestructive` | `issue_invoice` |

`issue_invoice` is destructive in the sense that matters: irreversible. It
consumes a gap-free number, posts to the ledger, marks timesheet entries billed,
and produces a document that cannot afterwards be edited. Under-declaring it
would let `-yes` push invoices out of the door unseen.

Deliberately not exposed to the model: editing the chart of accounts, changing
VAT rates, locking periods. Those are setup decisions with consequences it has
no way to weigh.

`TestToolRiskLevels` asserts every level by name and fails if a tool is added
without one.

## Setup notes

- **VAT rates are seeded at 21% standard and 9% reduced.** These are a starting
  point, not advice — rates differ by member state. Correct them before issuing
  anything.
- **The chart of accounts is 40 accounts**, the range small-business guidance
  converges on. Accounts with postings are archived, never deleted, and their
  type cannot change once posted to.
- **`acct_invoices.client_id` is `ON DELETE RESTRICT`.** A CRM client with
  invoices cannot be deleted. This is a deliberate change to CRM behaviour:
  losing an issued invoice to a tidy-up would break the sequence and the audit
  trail.
