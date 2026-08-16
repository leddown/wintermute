// Package twire is wintermute's honeypot / canary tripwire. It opens TCP
// listeners that impersonate common well-known services (databases, Windows
// services, remote-access protocols, ...) on their usual ports and records —
// and optionally emails an alert about — any connection attempt. Nothing on a
// home network should ever touch these ports, so a hit is a strong signal that
// something is scanning or probing the network.
//
// Listeners are entirely opt-in (every canary defaults to disabled) and a
// failure to bind a port (already in use, or insufficient privilege) is
// surfaced as per-canary status rather than ever crashing the app.
//
// This moved here from morpheus. It was already a global resource there — every
// signed-in user saw the same canaries — so nothing had to be unscoped on the
// way across, unlike the ledger in the fintech package. What did change is
// where it stores things: SQLite rather than PostgreSQL, and a key derived from
// WINTERMUTE_SECRET rather than from a JWT signing secret this server does not
// have.
package twire

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrUnknownCanary is returned when a profile key isn't in the catalog.
	ErrUnknownCanary = errors.New("twire: unknown canary profile")
	// ErrValidation is returned for invalid alert-configuration input.
	ErrValidation = errors.New("twire: validation error")
	// ErrPortReserved is returned when a custom canary's port collides with
	// a built-in catalog profile.
	ErrPortReserved = errors.New("twire: port is used by a built-in canary")
	// ErrPortTaken is returned when a custom canary's port is already taken
	// by another custom canary.
	ErrPortTaken = errors.New("twire: port is already used by a custom canary")
	// ErrNotCustom is returned when a delete targets a built-in canary,
	// which cannot be removed.
	ErrNotCustom = errors.New("twire: canary is built-in and cannot be deleted")
	// ErrNoSecret is returned when an SMTP password would have to be written
	// to the database but WINTERMUTE_SECRET is unset, leaving nothing to
	// encrypt it with. Refusing is the point: the alternative is storing a
	// live credential in plaintext in a file on disk.
	ErrNoSecret = errors.New("twire: WINTERMUTE_SECRET is not set, so the SMTP password cannot be stored")
)

// customProfileKey is the stable profile key for an operator-defined canary
// on the given port. Port uniqueness (enforced in the DB) therefore makes
// the key unique, and the "custom-" prefix can never collide with a
// built-in catalog key.
func customProfileKey(port int) string {
	return fmt.Sprintf("custom-%d", port)
}

// ServiceProfile is one built-in fake-service definition from the catalog.
type ServiceProfile struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Port        int    `json:"port"`
	Description string `json:"description"`
	// Banner is sent to a client immediately on connect to mimic a real
	// service that greets first (e.g. SSH/SMTP/FTP). Empty = send nothing
	// (correct for protocols where the client speaks first).
	Banner string `json:"-"`
}

// CanaryStatus is a catalog profile plus its live runtime state. Custom is
// true for operator-defined canaries (which can be deleted), false for the
// built-in catalog.
type CanaryStatus struct {
	ServiceProfile
	Enabled           bool   `json:"enabled"`
	Listening         bool   `json:"listening"`
	LastError         string `json:"last_error,omitempty"`
	HitCount          int64  `json:"hit_count"`
	Custom            bool   `json:"custom"`
	PrivilegeRequired bool   `json:"privilege_required,omitempty"`
}

// Event is a single recorded connection attempt against a canary.
type Event struct {
	ID          int64     `json:"id"`
	ProfileKey  string    `json:"profile_key"`
	ServiceName string    `json:"service_name"`
	Port        int       `json:"port"`
	RemoteIP    string    `json:"remote_ip"`
	RemotePort  int       `json:"remote_port"`
	DataPreview string    `json:"data_preview"`
	OccurredAt  time.Time `json:"occurred_at"`
}

// AlertConfig is the email/SMTP alerting configuration. Emails are sent via
// Google SMTP (smtp.gmail.com:587); use a Google App Password for
// SMTPPassword. SMTPPassword is only ever held in memory in plaintext; at
// rest it is AES-256-GCM encrypted (see crypto.go).
type AlertConfig struct {
	Enabled      bool
	SMTPUsername string
	SMTPPassword string
	From         string
	Recipients   []string
}

// deliverable reports whether alerts can actually be sent with this config.
func (c AlertConfig) deliverable() bool {
	return c.Enabled && c.From != "" && len(c.Recipients) > 0
}
