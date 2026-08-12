# Consulting CRM Framework

The CRM module (`internal/crm`) is a lightweight client / practice-management
system for running a consulting business out of the same app. It tracks who you
work for, what you do for them, the billable time you spend, and what is owed
versus invoiced.

## Why this shape

A survey of open-source small-business and consulting tooling — Dolibarr and
Odoo (modular CRM + projects + invoicing + time), SuiteCRM (contacts/accounts),
and the freelancer/consultant billing tools Invoice Ninja, SolidInvoice,
Solidtime, and TimeTracker — shows the same core loop regardless of size:

```
Client ──< Engagement ──< Time Entry ──> Billing
```

Time tracking feeds billing directly so hours don't leak unbilled. This module
implements that core loop intentionally small (no invoices-as-documents, no
payments ledger, no client portal) with clean extension points.

## Entities

### Client
A company/contact you do work for.

| Field | Notes |
|-------|-------|
| `name` | Required. |
| `contact_name`, `email`, `phone` | Primary contact details. |
| `status` | `Prospect` / `Active` / `Inactive`. |
| `hourly_rate` | Default billing rate, used when an engagement has none. |
| `notes` | Free text. |

Derived (read-only): engagement count, total logged hours, unbilled amount.

### Engagement
A project or retainer performed for one client.

| Field | Notes |
|-------|-------|
| `client_id` | Required; FK to a client (cascade delete). |
| `name` | Required. |
| `status` | `Proposed` / `Active` / `On Hold` / `Completed`. |
| `hourly_rate` | Optional override of the client rate. |
| `budget_hours` | Planned hours for tracking burn. |
| `start_date`, `end_date`, `notes` | Scheduling/context. |

Derived (read-only): client name, total logged hours.

### Time Entry
A logged block of work against an engagement.

| Field | Notes |
|-------|-------|
| `engagement_id` | Required; FK to an engagement (cascade delete). |
| `entry_date` | Defaults to today. |
| `hours` | Required, 0 < hours ≤ 24. |
| `description` | Free text. |
| `billable` | Non-billable entries contribute hours but $0. |
| `rate` | **Snapshotted at log time** from the engagement rate, falling back to the client rate, unless explicitly set. |
| `invoiced` | Whether the entry has been billed. |

Derived (read-only): engagement name, client id/name, and `amount`
(`hours × rate` when billable, else `0`).

## Billing model

- **Unbilled** = billable entries that are not yet invoiced.
- **Invoiced** = billable entries marked invoiced.
- Rates are snapshotted on each time entry, so changing a client/engagement rate
  later never rewrites the value of already-logged work.
- The Billing page rolls these up per engagement and offers a one-click
  **Mark Invoiced** that flags every outstanding billable entry on an engagement
  as invoiced (`POST /crm/engagements/:id/invoice`).

## Pages & API

Read pages: `/crm` (dashboard), `/crm/clients`, `/crm/engagements`,
`/crm/time`, `/crm/billing`. Each list also has a `…/manage` editor variant for
the clients/engagements/time grids.

Read JSON: `/crm/dashboard/data`, `/crm/clients/data` (`?search,&status`),
`/crm/engagements/data` (`?client_id,&search,&status`), `/crm/time/data`
(`?client_id,&engagement_id,&search`), `/crm/billing/data`.

Mutations (admin-guarded, like the rest of the app — open in `LOCAL_MODE`):
`POST/PUT/DELETE /crm/clients`, `/crm/engagements`, `/crm/time`, plus
`POST /crm/engagements/:id/invoice`.

## Architecture

Standard repository → service → handler layering on SQLite, matching the risk
register module:

- `model.go` — entity and rollup structs.
- `repository.go` — SQL with joined display fields and aggregations.
- `service.go` — validation, status normalization, and rate snapshotting.
- `grid.go` — a reusable, config-driven JS data grid (clients, engagements,
  time) with relation dropdowns; one grid keeps the three CRUD screens
  consistent.
- `pages.go` — the bespoke dashboard and billing pages.
- `handler.go` — Gin handlers, grid configs, and route registration.

## Extension points (not built yet)

- Invoices as first-class documents (numbering, PDF via `internal/reporting`,
  payment status) — the data already distinguishes billable/unbilled/invoiced.
- Expenses and fixed-fee/milestone billing alongside hourly time.
- Pipeline/opportunity tracking for `Prospect` clients.
- A client-facing portal and per-user time ownership.
