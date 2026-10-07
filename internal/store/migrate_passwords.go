package store

import "context"

// Version 3 contains plaintext, even when a password happens to look like a hash.
// Schema version, never a string prefix, decides how legacy data is interpreted.
func (s *Store) migratePasswords() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT id,password FROM users")
	if err != nil {
		return err
	}
	type credential struct {
		id       int64
		password string
	}
	var users []credential
	for rows.Next() {
		var u credential
		if err = rows.Scan(&u.id, &u.password); err != nil {
			rows.Close()
			return err
		}
		users = append(users, u)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, u := range users {
		encoded, err := hashPassword(context.Background(), u.password)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE users SET password=? WHERE id=?", encoded, u.id); err != nil {
			return err
		}
	}
	// Existing twelve-hour cookies cannot survive the security-policy migration.
	if _, err = tx.Exec("ALTER TABLE users RENAME COLUMN password TO password_hash; DELETE FROM auth_sessions; PRAGMA user_version=4"); err != nil {
		return err
	}
	return tx.Commit()
}
