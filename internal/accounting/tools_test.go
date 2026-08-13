package accounting

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"wintermute/internal/tool"
)

func registerTools(t *testing.T, svc *Service) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry()
	if err := Register(reg, svc); err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

// Risk levels are the approval policy. Getting one wrong silently removes a
// confirmation the operator is relying on, so they are asserted by name rather
// than left to review.
func TestToolRiskLevels(t *testing.T) {
	svc, _ := newTestService(t)
	reg := registerTools(t, svc)

	want := map[string]tool.Risk{
		"accounting_overview":     tool.RiskRead,
		"list_invoices":           tool.RiskRead,
		"get_invoice":             tool.RiskRead,
		"list_unbilled_time":      tool.RiskRead,
		"list_accounts":           tool.RiskRead,
		"financial_report":        tool.RiskRead,
		"draft_invoice_from_time": tool.RiskWrite,
		"record_payment":          tool.RiskWrite,
		"record_expense":          tool.RiskWrite,
		// Write rather than destructive: a funding record can be removed, and
		// removing it reverses its entry rather than erasing it.
		"record_funding": tool.RiskWrite,
		// Irreversible: consumes a gap-free number, posts to the ledger, and
		// produces a document that cannot afterwards be edited or deleted.
		"issue_invoice": tool.RiskDestructive,
	}

	defs := reg.Definitions()
	got := map[string]tool.Risk{}
	for _, d := range defs {
		got[d.Name] = d.Risk
	}
	for name, risk := range want {
		if got[name] != risk {
			t.Errorf("%s has risk %q, want %q", name, got[name], risk)
		}
	}
	if len(defs) != len(want) {
		t.Errorf("registered %d tools, expected %d — a new one needs a risk level here",
			len(defs), len(want))
	}
}

// Every parameter schema crosses the wire to the Messages API. A malformed one
// fails the whole turn, not just its own tool.
func TestToolSchemasAreValidJSON(t *testing.T) {
	svc, _ := newTestService(t)
	reg := registerTools(t, svc)

	for _, d := range reg.Definitions() {
		var schema map[string]any
		if err := json.Unmarshal(d.Parameters, &schema); err != nil {
			t.Errorf("%s: parameters are not valid JSON: %v", d.Name, err)
			continue
		}
		if schema["type"] != "object" {
			t.Errorf("%s: schema type is %v, want object", d.Name, schema["type"])
		}
		if d.Description == "" {
			t.Errorf("%s: no description", d.Name)
		}
	}
}

func callTool(t *testing.T, reg *tool.Registry, name, args string) string {
	t.Helper()
	h, ok := reg.Handler(name)
	if !ok {
		t.Fatalf("no handler for %s", name)
	}
	out, err := h(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

func TestOverviewToolReportsPosition(t *testing.T) {
	svc, repo := newTestService(t)
	pinToday(t, "2026-09-30")
	seedQuarter(t, svc, repo)

	out := callTool(t, registerTools(t, svc), "accounting_overview", `{}`)
	for _, want := range []string{"Bank balance", "1089.00", "Profit this year", "900.00", "VAT payable"} {
		if !strings.Contains(out, want) {
			t.Errorf("overview missing %q:\n%s", want, out)
		}
	}
}

// A tool whose parameters are all optional gets called with no arguments at
// all by the model. That must not be an error.
func TestToolsAcceptEmptyArguments(t *testing.T) {
	svc, _ := newTestService(t)
	reg := registerTools(t, svc)

	for _, name := range []string{"accounting_overview", "list_invoices", "list_unbilled_time", "list_accounts"} {
		h, _ := reg.Handler(name)
		if _, err := h(context.Background(), nil); err != nil {
			t.Errorf("%s with no arguments: %v", name, err)
		}
	}
}

func TestBillingFlowThroughTools(t *testing.T) {
	svc, repo := newTestService(t)
	reg := registerTools(t, svc)

	client := newClient(t, repo, "Acme")
	eng := newEngagement(t, repo, client, "Review", 120)
	newTimeEntry(t, repo, eng, "2026-08-03", 5, 120, "Analysis")

	unbilled := callTool(t, reg, "list_unbilled_time", `{}`)
	if !strings.Contains(unbilled, "600.00") {
		t.Errorf("unbilled time did not report the value:\n%s", unbilled)
	}

	drafted := callTool(t, reg, "draft_invoice_from_time",
		`{"engagement_id":`+itoa(eng)+`}`)
	if !strings.Contains(drafted, "no number") {
		t.Errorf("drafting should say nothing was posted:\n%s", drafted)
	}

	invoices, err := svc.ListInvoices(InvoiceFilter{Status: StatusDraft})
	if err != nil || len(invoices) != 1 {
		t.Fatalf("expected one draft, got %v (%v)", len(invoices), err)
	}

	issued := callTool(t, reg, "issue_invoice", `{"invoice_id":`+itoa(invoices[0].ID)+`}`)
	if !strings.Contains(issued, "INV-0001") {
		t.Errorf("issue did not report the number:\n%s", issued)
	}
	// The model must be told the thing is now immutable, or it will offer to
	// edit it next turn.
	if !strings.Contains(issued, "no longer be edited") {
		t.Errorf("issue did not state immutability:\n%s", issued)
	}

	paid := callTool(t, reg, "record_payment",
		`{"invoice_id":`+itoa(invoices[0].ID)+`,"amount":"726.00"}`)
	if !strings.Contains(paid, "paid") {
		t.Errorf("payment did not report the resulting status:\n%s", paid)
	}
}

// The funding tool's reply has to make the capital/loan distinction visible.
// The model is the one relaying this to the user, and "recorded 25,000.00" says
// nothing about whether the business now owes it.
func TestFundingToolReportsWhatWasRecorded(t *testing.T) {
	svc, _ := newTestService(t)
	reg := registerTools(t, svc)

	capital := callTool(t, reg, "record_funding",
		`{"kind":"capital","amount":"25000.00","from_name":"Founder","received_on":"2026-01-02"}`)
	if !strings.Contains(capital, "does not owe it back") {
		t.Errorf("capital reply did not say it is equity:\n%s", capital)
	}

	loan := callTool(t, reg, "record_funding",
		`{"kind":"loan","amount":"5000.00","from_name":"Founder","received_on":"2026-02-01"}`)
	if !strings.Contains(loan, "5000.00") {
		t.Errorf("loan reply did not report the outstanding balance:\n%s", loan)
	}

	repaid := callTool(t, reg, "record_funding",
		`{"kind":"repayment","amount":"2000.00","received_on":"2026-03-01"}`)
	if !strings.Contains(repaid, "3000.00") {
		t.Errorf("repayment reply did not report the reduced balance:\n%s", repaid)
	}

	// An unlabelled deposit is refused rather than guessed at.
	h, _ := reg.Handler("record_funding")
	if _, err := h(context.Background(), json.RawMessage(`{"amount":"100.00"}`)); err == nil {
		t.Error("funding with no kind came back as success")
	}
}

// A refused action must come back as an error the model can act on, not as a
// success message describing something that did not happen.
func TestToolFailuresAreReportedAsErrors(t *testing.T) {
	svc, repo := newTestService(t)
	reg := registerTools(t, svc)
	inv := issuedInvoice(t, svc, repo, 100000)

	h, _ := reg.Handler("record_payment")
	_, err := h(context.Background(), json.RawMessage(
		`{"invoice_id":`+itoa(inv.ID)+`,"amount":"999999.00"}`))
	if err == nil {
		t.Fatal("an overpayment came back as success")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("unhelpful error: %v", err)
	}

	// Issuing something already issued.
	h, _ = reg.Handler("issue_invoice")
	if _, err := h(context.Background(), json.RawMessage(`{"invoice_id":`+itoa(inv.ID)+`}`)); err == nil {
		t.Error("re-issuing came back as success")
	}
}

func TestReportToolRejectsUnknownReport(t *testing.T) {
	svc, _ := newTestService(t)
	reg := registerTools(t, svc)

	h, _ := reg.Handler("financial_report")
	_, err := h(context.Background(), json.RawMessage(`{"report":"cashflow"}`))
	if err == nil {
		t.Fatal("an unknown report was accepted")
	}
	// The message must list what is available, or the model guesses again.
	if !strings.Contains(err.Error(), "profit_and_loss") {
		t.Errorf("error should name the valid reports: %v", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
