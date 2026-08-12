// Package company holds the operator's own business details: who the firm is,
// where it is registered, and how to reach it.
//
// One row per install. This is the firm's own identity as fact — a legal name,
// a company number, an address — as distinct from a CRM client record, which
// describes somebody else. The two move on different schedules and merging them
// would mean editing your own VAT number to correct a customer's address.
//
// Moved here from the RCSA application, which used it to letterhead generated
// policy documents. That renderer stayed behind with the policy module; what
// came across is the record itself, which is the part that is about the
// business rather than about compliance paperwork.
package company

import (
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
)

// Profile is the firm's own record. One per install — this application is run
// by one consultancy about its own engagements, and a second profile would be a
// different product.
type Profile struct {
	LegalName    string `json:"legal_name"`
	TradingName  string `json:"trading_name"`
	Tagline      string `json:"tagline"`
	Description  string `json:"description"`
	Founded      string `json:"founded"`
	Jurisdiction string `json:"jurisdiction"`

	RegistrationNumber string `json:"registration_number"`
	TaxID              string `json:"tax_id"`

	Email   string `json:"email"`
	Phone   string `json:"phone"`
	Website string `json:"website"`

	AddressLine1 string `json:"address_line1"`
	AddressLine2 string `json:"address_line2"`
	City         string `json:"city"`
	Region       string `json:"region"`
	PostalCode   string `json:"postal_code"`
	Country      string `json:"country"`

	ContactName  string `json:"contact_name"`
	ContactRole  string `json:"contact_role"`
	ContactEmail string `json:"contact_email"`

	UpdatedAt string `json:"updated_at"`
	UpdatedBy string `json:"updated_by"`
}

// Field length caps. A company profile is a set of short facts; anything longer
// is a paste accident, and the page lays out on the assumption that a line fits
// on a line.
const (
	maxNameLen        = 160
	maxLineLen        = 120
	maxEmailLen       = 254
	maxWebsiteLen     = 300
	maxDescriptionLen = 4000
)

var (
	// A phone number as people actually write one, including extensions.
	phonePattern = regexp.MustCompile(`^[0-9 +().extEXT/#-]{4,40}$`)
	yearPattern  = regexp.MustCompile(`^[0-9]{4}$`)

	// A URL scheme, with one carve-out: a colon followed by digits is a port on
	// a bare host ("example.com:8443"), not a scheme. Without it the helpful
	// "assume https" path would reject a perfectly ordinary address.
	schemePattern = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.\-]*):(?:[^0-9]|$)`)
)

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// Validate normalises the profile in place and reports the first problem found.
//
// It runs on every save. The rules are ordinary length and format checks with
// two exceptions worth knowing about — the website scheme and the control
// characters — both noted where they are applied.
func (p *Profile) Validate() error {
	lines := []struct {
		name  string
		value *string
		limit int
	}{
		{"legal name", &p.LegalName, maxNameLen},
		{"trading name", &p.TradingName, maxNameLen},
		{"tagline", &p.Tagline, maxLineLen},
		{"jurisdiction", &p.Jurisdiction, maxLineLen},
		{"registration number", &p.RegistrationNumber, maxLineLen},
		{"tax id", &p.TaxID, maxLineLen},
		{"address line 1", &p.AddressLine1, maxLineLen},
		{"address line 2", &p.AddressLine2, maxLineLen},
		{"city", &p.City, maxLineLen},
		{"region", &p.Region, maxLineLen},
		{"postal code", &p.PostalCode, maxLineLen},
		{"country", &p.Country, maxLineLen},
		{"contact name", &p.ContactName, maxNameLen},
		{"contact role", &p.ContactRole, maxLineLen},
	}
	for _, f := range lines {
		*f.value = strings.TrimSpace(*f.value)
		// Control characters are rejected rather than stripped. These values are
		// single-line facts that end up in a page, a header and eventually a
		// document; a newline or a NUL arriving in one means the input is not
		// what it claims to be, and silently repairing it hides that.
		if strings.ContainsFunc(*f.value, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return fmt.Errorf("%s must be a single line without control characters", f.name)
		}
		if len([]rune(*f.value)) > f.limit {
			return fmt.Errorf("%s must be %d characters or fewer", f.name, f.limit)
		}
	}
	if p.LegalName == "" {
		return fmt.Errorf("legal name is required")
	}

	// The description is the one multi-line field: it is a paragraph about the
	// firm, so newlines are content. Carriage returns are normalised away first
	// so a value pasted from Windows compares equal to the same text typed here.
	p.Description = strings.TrimSpace(strings.ReplaceAll(p.Description, "\r\n", "\n"))
	if strings.ContainsFunc(p.Description, func(r rune) bool { return (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f }) {
		return fmt.Errorf("description must not contain control characters")
	}
	if len([]rune(p.Description)) > maxDescriptionLen {
		return fmt.Errorf("description must be %d characters or fewer", maxDescriptionLen)
	}

	emails := []struct {
		name  string
		value *string
	}{
		{"email", &p.Email},
		{"contact email", &p.ContactEmail},
	}
	for _, f := range emails {
		*f.value = strings.TrimSpace(*f.value)
		if *f.value == "" {
			continue
		}
		if len(*f.value) > maxEmailLen {
			return fmt.Errorf("%s must be %d characters or fewer", f.name, maxEmailLen)
		}
		addr, err := mail.ParseAddress(*f.value)
		if err != nil || addr.Name != "" {
			return fmt.Errorf("%s must be a plain address like info@example.com, got %q", f.name, *f.value)
		}
		*f.value = addr.Address
	}

	p.Phone = strings.TrimSpace(p.Phone)
	if p.Phone != "" && !phonePattern.MatchString(p.Phone) {
		return fmt.Errorf("phone must be digits and the usual separators, got %q", p.Phone)
	}

	p.Founded = strings.TrimSpace(p.Founded)
	if p.Founded != "" {
		if !yearPattern.MatchString(p.Founded) {
			return fmt.Errorf("founded must be a four-digit year, got %q", p.Founded)
		}
		if p.Founded < "1600" {
			return fmt.Errorf("founded must be a plausible year, got %q", p.Founded)
		}
	}

	return p.validateWebsite()
}

// validateWebsite is the one check here that is a security boundary rather than
// a typo guard. The value is rendered as a clickable link, so an unconstrained
// URL means the page offers whatever scheme was stored — javascript: being the
// obvious one, but data: and file: are no better. Allowing exactly http and
// https closes that at the point the value is written, so no renderer has to
// remember to re-check it.
func (p *Profile) validateWebsite() error {
	p.Website = strings.TrimSpace(p.Website)
	if p.Website == "" {
		return nil
	}
	if len(p.Website) > maxWebsiteLen {
		return fmt.Errorf("website must be %d characters or fewer", maxWebsiteLen)
	}
	switch {
	case hasPrefixFold(p.Website, "http://"), hasPrefixFold(p.Website, "https://"):
		// Take it as typed.
	case schemePattern.MatchString(p.Website):
		// It declares a scheme, and it is not one of the two allowed. Rejected
		// here rather than after url.Parse so the message says what is wrong —
		// prefixing "https://" onto "javascript:alert(1)" also fails, but with a
		// parser error about an invalid port, which explains nothing.
		return fmt.Errorf("website must be an http or https address, got %q",
			schemePattern.FindStringSubmatch(p.Website)[1])
	default:
		// A person types "carelockconsulting.com", not a URL. Assuming https is
		// the safe half of being helpful: the cases above have already ruled out
		// anything carrying a scheme of its own.
		p.Website = "https://" + p.Website
	}

	parsed, err := url.Parse(p.Website)
	if err != nil {
		return fmt.Errorf("website is not a valid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("website must be an http or https address, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("website must include a host, got %q", p.Website)
	}
	p.Website = parsed.String()
	return nil
}

// Complete reports whether the profile carries the fields a letterhead needs.
// The page uses it to say so plainly: a half-filled profile is the normal state
// after a first save, and it should be visible without reading every box.
func (p Profile) Complete() bool {
	return p.LegalName != "" && p.AddressLine1 != "" && p.City != "" &&
		p.Country != "" && p.Email != ""
}
