package company

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Store persists the profile. One row, fixed at id 1 by a CHECK constraint,
// for the reason in the package comment: the install belongs to one firm.
//
// The profile is a JSON document rather than a column per field. Nothing ever
// filters on it, and a column each would mean a migration every time the firm
// records another identifier.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Load returns the saved profile and whether one exists. A fresh install has no
// row and gets an empty profile rather than an error — nobody has filled it in
// yet, which is a state the UI has to render anyway.
func (s *Store) Load() (Profile, bool, error) {
	var payload, updatedAt, updatedBy string
	err := s.db.QueryRow(
		`SELECT profile_json, updated_at, updated_by FROM company_profile WHERE id = 1`,
	).Scan(&payload, &updatedAt, &updatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, false, nil
	}
	if err != nil {
		return Profile{}, false, fmt.Errorf("loading company profile: %w", err)
	}

	var profile Profile
	if err := json.Unmarshal([]byte(payload), &profile); err != nil {
		return Profile{}, false, fmt.Errorf("decoding company profile: %w", err)
	}
	profile.UpdatedAt = updatedAt
	profile.UpdatedBy = updatedBy
	return profile, true, nil
}

func (s *Store) Save(profile Profile) error {
	payload, err := json.Marshal(profile)
	if err != nil {
		return fmt.Errorf("encoding company profile: %w", err)
	}
	_, err = s.db.Exec(`
		INSERT INTO company_profile (id, profile_json, updated_at, updated_by)
		VALUES (1, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			profile_json = excluded.profile_json,
			updated_at   = excluded.updated_at,
			updated_by   = excluded.updated_by`,
		string(payload), profile.UpdatedAt, profile.UpdatedBy)
	if err != nil {
		return fmt.Errorf("saving company profile: %w", err)
	}
	return nil
}

// Clear drops the row, returning the install to "not filled in yet".
func (s *Store) Clear() error {
	if _, err := s.db.Exec(`DELETE FROM company_profile WHERE id = 1`); err != nil {
		return fmt.Errorf("clearing company profile: %w", err)
	}
	return nil
}

// Service is the module's API.
type Service struct {
	store *Store
	now   func() time.Time
}

func NewService(store *Store) *Service {
	return &Service{store: store, now: time.Now}
}

// Profile returns the saved profile, or an empty one when nothing is saved.
// There is no shipped default: a firm's legal name has no sensible stand-in,
// and inventing one would put a placeholder company number in front of somebody
// who would reasonably assume it had been checked.
func (s *Service) Profile() (Profile, error) {
	profile, _, err := s.store.Load()
	return profile, err
}

// Save validates and stores the profile, stamping who changed it and when.
func (s *Service) Save(profile Profile, actor string) (Profile, error) {
	if err := profile.Validate(); err != nil {
		return Profile{}, err
	}
	profile.UpdatedBy = actor
	profile.UpdatedAt = s.now().UTC().Format(time.RFC3339)
	if err := s.store.Save(profile); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

// Clear removes the saved profile.
func (s *Service) Clear() error { return s.store.Clear() }
