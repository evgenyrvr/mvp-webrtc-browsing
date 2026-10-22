package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mvp1/internal/egress"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrLimit        = errors.New("session limit reached")
	ErrInvalid      = errors.New("invalid input")
	ErrNotFound     = errors.New("not found")
	ErrLastAdmin    = errors.New("at least one enabled administrator is required")
)

const AuthSessionLifetime = time.Hour

type Store struct {
	db  *sql.DB
	now func() time.Time
}
type User struct {
	ID      int64   `json:"id"`
	Login   string  `json:"login"`
	Enabled bool    `json:"enabled"`
	Admin   bool    `json:"admin"`
	RoleIDs []int64 `json:"role_ids"`
}
type Role struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Admin bool   `json:"admin"`
}
type Application struct {
	StartURL      string         `json:"start_url"`
	Access        *egress.Policy `json:"access,omitempty"`
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Template      string         `json:"template"`
	Enabled       bool           `json:"enabled"`
	Launch        bool           `json:"can_launch"`
	Control       bool           `json:"can_control"`
	Print         bool           `json:"can_print"`
	PolicyVersion int64          `json:"-"`
}
type Grant struct {
	RoleID  int64  `json:"role_id"`
	AppID   string `json:"application_id"`
	Launch  bool   `json:"can_launch"`
	Control bool   `json:"can_control"`
	Print   bool   `json:"can_print"`
}
type Session struct {
	ID        string `json:"id"`
	UserID    int64  `json:"user_id"`
	AppID     string `json:"application_id"`
	State     string `json:"state"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}
type Identity struct {
	User       User   `json:"user"`
	CSRF       string `json:"csrf_token"`
	ExpiresAt  int64  `json:"expires_at"`
	ServerTime int64  `json:"server_time"`
}
type UserInput struct {
	ID       int64   `json:"id"`
	Login    string  `json:"login"`
	Password string  `json:"password"`
	Enabled  bool    `json:"enabled"`
	RoleIDs  []int64 `json:"role_ids"`
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err = db.Exec(`PRAGMA secure_delete=ON; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL;`); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	// Finish page/WAL cleanup on every startup, including retry after interrupted migration.
	if _, err = db.Exec("VACUUM"); err != nil {
		db.Close()
		return nil, err
	}
	var busy, pages, done int
	if err = db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &pages, &done); err != nil || busy != 0 {
		db.Close()
		return nil, errors.New("cannot complete database checkpoint")
	}
	return s, nil
}
func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) migrate() error {
	if err := s.migrateLegacy(); err != nil {
		return err
	}
	return s.migratePublications()
}
func (s *Store) migrateLegacy() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > 5 {
		return errors.New("database schema is newer than this binary")
	}
	if version >= 4 {
		return nil
	}
	if version == 3 {
		return s.migratePasswords()
	}
	if version == 2 {
		if err := s.migrateFalkon(); err != nil {
			return err
		}
		return s.migratePasswords()
	}
	if version == 1 {
		if err := s.migratePolicy(); err != nil {
			return err
		}
		if err := s.migrateFalkon(); err != nil {
			return err
		}
		return s.migratePasswords()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
 CREATE TABLE users (id INTEGER PRIMARY KEY, login TEXT UNIQUE NOT NULL, password TEXT NOT NULL, enabled INTEGER NOT NULL CHECK(enabled IN (0,1)));
 CREATE TABLE roles (id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, admin INTEGER NOT NULL DEFAULT 0 CHECK(admin IN (0,1)));
 CREATE TABLE user_roles (user_id INTEGER REFERENCES users(id) ON DELETE CASCADE, role_id INTEGER REFERENCES roles(id) ON DELETE CASCADE, PRIMARY KEY(user_id,role_id));
 CREATE TABLE applications (id TEXT PRIMARY KEY, name TEXT NOT NULL, template TEXT NOT NULL, enabled INTEGER NOT NULL CHECK(enabled IN (0,1)));
 CREATE TABLE role_app_permissions (role_id INTEGER REFERENCES roles(id) ON DELETE CASCADE, app_id TEXT REFERENCES applications(id) ON DELETE CASCADE, can_launch INTEGER NOT NULL CHECK(can_launch IN (0,1)), can_control INTEGER NOT NULL CHECK(can_control IN (0,1)), can_print INTEGER NOT NULL CHECK(can_print IN (0,1)), PRIMARY KEY(role_id,app_id));
 CREATE TABLE auth_sessions (token_hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, csrf TEXT NOT NULL, expires_at INTEGER NOT NULL);
 CREATE TABLE app_sessions (id TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id), app_id TEXT NOT NULL REFERENCES applications(id), state TEXT NOT NULL CHECK(state IN ('preparing','ready','connecting','active','reconnecting','stopping','closed','failed')), created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
 CREATE INDEX sessions_by_user ON app_sessions(user_id,state);
 INSERT INTO roles(id,name,admin) VALUES(1,'admin',1),(2,'user',0);
 INSERT INTO applications VALUES('firefox','Firefox','firefox',1),('terminal','Терминал','terminal',1);
 INSERT INTO role_app_permissions VALUES(1,'firefox',1,1,1),(1,'terminal',1,1,0);
 PRAGMA user_version=1;
 `)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := s.migratePolicy(); err != nil {
		return err
	}
	if err := s.migrateFalkon(); err != nil {
		return err
	}
	return s.migratePasswords()
}

func (s *Store) migratePolicy() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("CREATE TABLE policy_revision (id INTEGER PRIMARY KEY CHECK(id=1), version INTEGER NOT NULL); INSERT INTO policy_revision VALUES(1,1)"); err != nil {
		return err
	}
	for _, table := range []string{"users", "roles", "user_roles", "role_app_permissions", "applications"} {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			statement := fmt.Sprintf("CREATE TRIGGER policy_%s_%s AFTER %s ON %s BEGIN UPDATE policy_revision SET version=version+1 WHERE id=1; END", table, event, event, table)
			if _, err = tx.Exec(statement); err != nil {
				return err
			}
		}
	}
	if _, err = tx.Exec("PRAGMA user_version=2"); err != nil {
		return err
	}
	return tx.Commit()
}

// Preserve existing sessions and role restrictions while adding the primary browser.
func (s *Store) migrateFalkon() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO applications(id,name,template,enabled)
 SELECT 'falkon','Falkon','falkon',enabled FROM applications WHERE id='firefox';
 INSERT INTO role_app_permissions(role_id,app_id,can_launch,can_control,can_print)
 SELECT role_id,'falkon',can_launch,can_control,can_print FROM role_app_permissions WHERE app_id='firefox';
 PRAGMA user_version=3;`); err != nil {
		return err
	}
	return tx.Commit()
}

func Token() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func hash(v string) string { sum := sha256.Sum256([]byte(v)); return hex.EncodeToString(sum[:]) }

// Bootstrap only creates the first administrator. Subsequent starts never reset passwords.
func (s *Store) Bootstrap(ctx context.Context, login, password string) error {
	login = strings.TrimSpace(login)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	if login == "" || password == "" || len(login) > 80 || len(password) > 1024 {
		return errors.New("empty database: set MVP_ADMIN_LOGIN and MVP_ADMIN_PASSWORD")
	}
	encoded, err := hashPassword(ctx, password)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "INSERT INTO users(login,password_hash,enabled) VALUES(?,?,1)", login, encoded)
	if err != nil {
		return err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO user_roles VALUES(?,1)", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Login(ctx context.Context, login, password string) (string, error) {
	var id int64
	var saved string
	var enabled bool
	err := s.db.QueryRowContext(ctx, "SELECT id,password_hash,enabled FROM users WHERE login=?", login).Scan(&id, &saved, &enabled)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	valid, checkErr := verifyPassword(ctx, password, saved)
	if checkErr != nil {
		return "", checkErr
	}
	if err != nil || !enabled || !valid {
		return "", ErrUnauthorized
	}
	token, err := Token()
	if err != nil {
		return "", err
	}
	csrf, err := Token()
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, "DELETE FROM auth_sessions WHERE expires_at<=?", s.now().Unix())
	if err != nil {
		return "", err
	}
	result, err := s.db.ExecContext(ctx, "INSERT INTO auth_sessions(token_hash,user_id,csrf,expires_at) SELECT ?,id,?,? FROM users WHERE id=? AND enabled=1 AND password_hash=?", hash(token), csrf, s.now().Add(AuthSessionLifetime).Unix(), id, saved)
	if err != nil {
		return "", err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", ErrUnauthorized
	}
	return token, nil
}

func (s *Store) Identity(ctx context.Context, token string) (Identity, error) {
	var v Identity
	v.ServerTime = s.now().Unix()
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.login,u.enabled,a.csrf,a.expires_at,
 EXISTS(SELECT 1 FROM user_roles ur JOIN roles r ON r.id=ur.role_id WHERE ur.user_id=u.id AND r.admin=1)
 FROM auth_sessions a JOIN users u ON u.id=a.user_id WHERE a.token_hash=? AND a.expires_at>? AND u.enabled=1`, hash(token), s.now().Unix()).Scan(&v.User.ID, &v.User.Login, &v.User.Enabled, &v.CSRF, &v.ExpiresAt, &v.User.Admin)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrUnauthorized
	}
	return v, err
}
func (s *Store) LogoutUser(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM auth_sessions WHERE user_id=?", id)
	return err
}

func (s *Store) Applications(ctx context.Context, userID int64) ([]Application, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.name,a.template,a.enabled,a.start_url,a.access_policy,MAX(p.can_launch),MAX(p.can_control),MAX(p.can_print),(SELECT version FROM policy_revision WHERE id=1)
 FROM applications a JOIN role_app_permissions p ON p.app_id=a.id JOIN user_roles ur ON ur.role_id=p.role_id JOIN users u ON u.id=ur.user_id
 WHERE ur.user_id=? AND u.enabled=1 AND a.enabled=1 GROUP BY a.id HAVING MAX(p.can_launch)=1 ORDER BY a.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Application{}
	for rows.Next() {
		var a Application
		var policy string
		if err := rows.Scan(&a.ID, &a.Name, &a.Template, &a.Enabled, &a.StartURL, &policy, &a.Launch, &a.Control, &a.Print, &a.PolicyVersion); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(policy), &a.Access); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Store) Application(ctx context.Context, userID int64, appID string) (Application, error) {
	apps, err := s.Applications(ctx, userID)
	if err != nil {
		return Application{}, err
	}
	for _, a := range apps {
		if a.ID == appID {
			return a, nil
		}
	}
	return Application{}, ErrForbidden
}

func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.id,u.login,u.enabled,EXISTS(SELECT 1 FROM user_roles ur JOIN roles r ON r.id=ur.role_id WHERE ur.user_id=u.id AND r.admin=1) FROM users u ORDER BY u.id`)
	if err != nil {
		return nil, err
	}
	out := []User{}
	for rows.Next() {
		var u User
		u.RoleIDs = []int64{}
		if err := rows.Scan(&u.ID, &u.Login, &u.Enabled, &u.Admin); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, u)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		rows, err := s.db.QueryContext(ctx, "SELECT role_id FROM user_roles WHERE user_id=? ORDER BY role_id", out[i].ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out[i].RoleIDs = append(out[i].RoleIDs, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func (s *Store) Roles(ctx context.Context) ([]Role, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,name,admin FROM roles ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Role{}
	for rows.Next() {
		var r Role
		if err := rows.Scan(&r.ID, &r.Name, &r.Admin); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) Grants(ctx context.Context) ([]Grant, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT role_id,app_id,can_launch,can_control,can_print FROM role_app_permissions ORDER BY role_id,app_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Grant{}
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.RoleID, &g.AppID, &g.Launch, &g.Control, &g.Print); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
func (s *Store) AllApplications(ctx context.Context) ([]Application, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,name,template,enabled,start_url,access_policy FROM applications ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Application{}
	for rows.Next() {
		var a Application
		var policy string
		if err := rows.Scan(&a.ID, &a.Name, &a.Template, &a.Enabled, &a.StartURL, &policy); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(policy), &a.Access); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func lastAdmin(ctx context.Context, tx *sql.Tx) error {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users u WHERE enabled=1 AND EXISTS(SELECT 1 FROM user_roles ur JOIN roles r ON r.id=ur.role_id WHERE ur.user_id=u.id AND r.admin=1)`).Scan(&n)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLastAdmin
	}
	return nil
}
func (s *Store) SaveUser(ctx context.Context, v UserInput) (int64, error) {
	v.Login = strings.TrimSpace(v.Login)
	if v.Login == "" || len(v.Login) > 80 || len(v.Password) > 1024 || len(v.RoleIDs) > 32 || v.ID < 0 {
		return 0, ErrInvalid
	}
	encoded := ""
	var err error
	if v.Password != "" {
		encoded, err = hashPassword(ctx, v.Password)
		if err != nil {
			return 0, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if v.ID == 0 {
		if v.Password == "" {
			return 0, ErrInvalid
		}
		r, err := tx.ExecContext(ctx, "INSERT INTO users(login,password_hash,enabled) VALUES(?,?,?)", v.Login, encoded, v.Enabled)
		if err != nil {
			return 0, fmt.Errorf("%w: login must be unique", ErrInvalid)
		}
		v.ID, err = r.LastInsertId()
		if err != nil {
			return 0, err
		}
	} else {
		r, err := tx.ExecContext(ctx, "UPDATE users SET login=?,enabled=?,password_hash=CASE WHEN ?='' THEN password_hash ELSE ? END WHERE id=?", v.Login, v.Enabled, encoded, encoded, v.ID)
		if err != nil {
			return 0, fmt.Errorf("%w: cannot update user", ErrInvalid)
		}
		n, _ := r.RowsAffected()
		if n == 0 {
			return 0, ErrNotFound
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM user_roles WHERE user_id=?", v.ID); err != nil {
		return 0, err
	}
	for _, id := range v.RoleIDs {
		if _, err = tx.ExecContext(ctx, "INSERT INTO user_roles(user_id,role_id) VALUES(?,?)", v.ID, id); err != nil {
			return 0, fmt.Errorf("%w: invalid or repeated role", ErrInvalid)
		}
	}
	if err = lastAdmin(ctx, tx); err != nil {
		return 0, err
	}
	if !v.Enabled || v.Password != "" {
		if _, err = tx.ExecContext(ctx, "DELETE FROM auth_sessions WHERE user_id=?", v.ID); err != nil {
			return 0, err
		}
	}
	return v.ID, tx.Commit()
}
func (s *Store) SaveRole(ctx context.Context, v Role) error {
	v.Name = strings.TrimSpace(v.Name)
	if v.Name == "" || len(v.Name) > 80 || v.ID < 0 {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if v.ID == 0 {
		_, err = tx.ExecContext(ctx, "INSERT INTO roles(name,admin) VALUES(?,?)", v.Name, v.Admin)
	} else {
		var r sql.Result
		r, err = tx.ExecContext(ctx, "UPDATE roles SET name=?,admin=? WHERE id=?", v.Name, v.Admin, v.ID)
		if err == nil {
			n, _ := r.RowsAffected()
			if n == 0 {
				return ErrNotFound
			}
		}
	}
	if err != nil {
		return fmt.Errorf("%w: role must have a unique name", ErrInvalid)
	}
	if err = lastAdmin(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) SaveGrant(ctx context.Context, g Grant) error {
	if !g.Launch && (g.Control || g.Print) {
		return ErrInvalid
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO role_app_permissions VALUES(?,?,?,?,?) ON CONFLICT(role_id,app_id) DO UPDATE SET can_launch=excluded.can_launch,can_control=excluded.can_control,can_print=excluded.can_print`, g.RoleID, g.AppID, g.Launch, g.Control, g.Print)
	if err != nil {
		return fmt.Errorf("%w: unknown role or application", ErrInvalid)
	}
	return nil
}
func (s *Store) SetApplication(ctx context.Context, id string, enabled bool) error {
	r, err := s.db.ExecContext(ctx, "UPDATE applications SET enabled=? WHERE id=?", enabled, id)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ReserveSession(ctx context.Context, userID int64, appID string) (Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback()
	var allowed int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM users u JOIN user_roles ur ON ur.user_id=u.id JOIN role_app_permissions p ON p.role_id=ur.role_id JOIN applications a ON a.id=p.app_id WHERE u.id=? AND u.enabled=1 AND a.id=? AND a.enabled=1 AND p.can_launch=1`, userID, appID).Scan(&allowed)
	if err != nil {
		return Session{}, err
	}
	if allowed == 0 {
		return Session{}, ErrForbidden
	}
	var total, own, users int
	err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(SUM(user_id=?),0),count(DISTINCT user_id) FROM app_sessions WHERE state NOT IN ('closed','failed')`, userID).Scan(&total, &own, &users)
	if err != nil {
		return Session{}, err
	}
	if total >= 4 || own >= 2 || (own == 0 && users >= 2) {
		return Session{}, ErrLimit
	}
	id, err := Token()
	if err != nil {
		return Session{}, err
	}
	now := s.now().Unix()
	v := Session{ID: id, UserID: userID, AppID: appID, State: "preparing", CreatedAt: now, UpdatedAt: now}
	_, err = tx.ExecContext(ctx, "INSERT INTO app_sessions VALUES(?,?,?,?,?,?)", id, userID, appID, v.State, now, now)
	if err != nil {
		return Session{}, err
	}
	return v, tx.Commit()
}
func (s *Store) Sessions(ctx context.Context, userID int64) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,user_id,app_id,state,created_at,updated_at FROM app_sessions WHERE (?=0 OR user_id=?) AND state NOT IN ('closed','failed') ORDER BY created_at,id", userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var v Session
		if err := rows.Scan(&v.ID, &v.UserID, &v.AppID, &v.State, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) Session(ctx context.Context, id string) (Session, error) {
	var v Session
	err := s.db.QueryRowContext(ctx, "SELECT id,user_id,app_id,state,created_at,updated_at FROM app_sessions WHERE id=?", id).Scan(&v.ID, &v.UserID, &v.AppID, &v.State, &v.CreatedAt, &v.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}
func (s *Store) State(ctx context.Context, id, state string) error {
	r, err := s.db.ExecContext(ctx, "UPDATE app_sessions SET state=?,updated_at=? WHERE id=?", state, s.now().Unix(), id)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// LiveState cannot resurrect a session which is stopping or already terminal.
func (s *Store) LiveState(ctx context.Context, id, state string) (bool, error) {
	if state != "active" && state != "reconnecting" && state != "connecting" {
		return false, ErrInvalid
	}
	result, err := s.db.ExecContext(ctx, "UPDATE app_sessions SET state=?,updated_at=? WHERE id=? AND state IN ('ready','connecting','active','reconnecting')", state, s.now().Unix(), id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
