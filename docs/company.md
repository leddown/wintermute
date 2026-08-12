# Company Info

The firm's own details — one record for the install, at `/company`.

| Route | |
|---|---|
| `GET /company` | The page: a letterhead card and the edit form |
| `GET /company/data` | The profile plus a `complete` flag |
| `PUT /company` | Save (admin) |
| `POST /company/clear` | Delete the saved profile (admin) |

Reading is open to any signed-in user; editing sits behind the admin middleware,
the same read-open / write-admin split as the CRM, policy and template modules.
The address a consultant puts on a deliverable is not a secret, but the identity
every deliverable is issued under is not something any account should be able to
rewrite.

---

## Why it is its own module

It sits between two things it is deliberately not part of:

- **The template brand** (`/templates/manage`) is how a document *looks* —
  palette, fonts, margins, a firm name for the wordmark. It is consumed by a
  typesetter, and every field in it is validated as template source.
- **A CRM client** (`/crm/clients`) describes somebody else.

This is the firm's own identity as fact: a legal name, a company number, a
registered address. A rebrand is not a change of registered office, and the two
should not be edited in the same form — correcting a VAT number should not mean
opening the typography settings.

---

## The record

One row, `company_profile`, pinned to `id = 1` by a CHECK constraint, stored as
a JSON blob. Same reasoning as `doc_template_brand`: no query ever filters on
these fields, and a column each would mean a migration every time the firm
records another identifier.

**There is no default profile.** A fresh install returns an empty one, and the
page says so. Unlike a brand, a legal name has no sensible stand-in — a
placeholder company number in front of someone who reasonably assumes it was
checked is worse than a blank field.

`updated_at` / `updated_by` are stamped by the service from the session, never
from the request body.

### Validation

Ordinary length and shape checks, with two that are load-bearing:

- **The website must be `http` or `https`.** This is a security boundary, not a
  typo guard: the card renders the value as a clickable link, so an
  unconstrained URL means the page offers whatever scheme was stored —
  `javascript:` being the obvious one, `data:` and `file:` no better. A value
  carrying any other scheme is rejected with a message naming it; a value
  carrying none ("carelockconsulting.com") gets `https://`. A bare host with a
  port ("example.com:8443") is not a scheme and is left alone.
- **Single-line fields reject control characters** rather than stripping them.
  These are short facts that end up in a page, a header and eventually a
  document; a newline arriving in a city name means the input is not what it
  claims to be, and silently repairing it hides that. The description is the one
  multi-line field — newlines there are content, and CRLF is normalised so a
  pasted value compares equal to a typed one.

`Complete()` reports whether the profile carries what a letterhead needs — name,
address, city, country, email. The page says which state it is in rather than
leaving it to be inferred from empty boxes; a half-filled profile is the normal
state after a first save.

---

## Backup and sync

`company_profile` is in the `dbsync` table set, keyed on `id` with an upsert —
the only strategy that survives a second sync into the same destination, given
the CHECK. It is authored content by the same argument as the brand: typed in
once and then relied on, and an install that lost it would keep working while
quietly having no letterhead.

This took `SnapshotVersion` to 5. A v4 backup restores with the table empty,
which reads correctly as "not filled in yet".
