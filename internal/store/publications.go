package store

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

func (s *Store) migratePublications() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == 5 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`ALTER TABLE applications ADD COLUMN start_url TEXT NOT NULL DEFAULT '';
 ALTER TABLE applications ADD COLUMN access_policy TEXT NOT NULL DEFAULT 'null';
 PRAGMA user_version=5;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SavePublication only creates policy-enforced browser publications. Legacy
// template entries retain their existing behavior until explicitly migrated.
func (s *Store) SavePublication(ctx context.Context, a Application) error {
	if len(a.ID) < 1 || len(a.ID) > 64 || a.ID == "falkon" || a.ID == "firefox" || a.ID == "terminal" {
		return ErrInvalid
	}
	for _, c := range a.ID {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return ErrInvalid
		}
	}
	if strings.TrimSpace(a.Name) == "" || utf8.RuneCountInString(a.Name) > 120 || (a.Template != "firefox" && a.Template != "falkon") || a.Access == nil {
		return ErrInvalid
	}
	if err := a.Access.ValidateStartURL(a.StartURL); err != nil {
		return ErrInvalid
	}
	policy, err := json.Marshal(a.Access)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Persist revocation before publishing changed rules, including open tunnels.
	_, err = tx.ExecContext(ctx, `UPDATE app_sessions SET state='stopping' WHERE app_id=? AND state NOT IN ('closed','failed');`, a.ID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO applications(id,name,template,enabled,start_url,access_policy) VALUES(?,?,?,?,?,?)
 ON CONFLICT(id) DO UPDATE SET name=excluded.name,template=excluded.template,enabled=excluded.enabled,start_url=excluded.start_url,access_policy=excluded.access_policy`, a.ID, a.Name, a.Template, a.Enabled, a.StartURL, string(policy))
	if err != nil {
		return err
	}
	return tx.Commit()
}
