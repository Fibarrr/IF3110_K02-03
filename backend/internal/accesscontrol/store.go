package accesscontrol

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

//go:embed schema.sql
var schema embed.FS

var (
	ErrNotFound           = errors.New("not found")
	ErrConflict           = errors.New("data dengan nama tersebut sudah ada")
	ErrInvalidCredentials = errors.New("username atau password salah")
)

type InputError string

func (e InputError) Error() string { return string(e) }

type User struct {
	ID            int64     `json:"id"`
	Username      string    `json:"username"`
	PlatformAdmin bool      `json:"platformAdmin"`
	Disabled      bool      `json:"disabled"`
	CreatedAt     time.Time `json:"createdAt"`
}

type Group struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

type DatabaseGrant struct {
	DatabaseID string         `json:"databaseId"`
	Access     DatabaseAccess `json:"access"`
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) Migrate(ctx context.Context) error {
	contents, err := schema.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read access control schema: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, string(contents)); err != nil {
		return fmt.Errorf("apply access control schema: %w", err)
	}
	return nil
}

func (s *Store) BootstrapAdmin(ctx context.Context, username, password string) error {
	username, err := normalizeUsername(username)
	if err != nil {
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockAdminChanges(ctx, tx); err != nil {
		return err
	}

	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM ac_users WHERE is_platform_admin AND NOT disabled)`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ac_users (username, password_hash, is_platform_admin) VALUES ($1, $2, TRUE)
		ON CONFLICT (username) DO UPDATE SET password_hash = EXCLUDED.password_hash, is_platform_admin = TRUE, disabled = FALSE`, username, hash); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (s *Store) CreateUser(ctx context.Context, username, password string) (User, error) {
	username, err := normalizeUsername(username)
	if err != nil {
		return User{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	var user User
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO ac_users (username, password_hash)
		VALUES ($1, $2)
		RETURNING id, username, is_platform_admin, disabled, created_at`, username, hash).
		Scan(&user.ID, &user.Username, &user.PlatformAdmin, &user.Disabled, &user.CreatedAt)
	if err != nil {
		return User{}, mapDatabaseError(err)
	}
	return user, nil
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, username, is_platform_admin, disabled, created_at FROM ac_users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := make([]User, 0)
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.ID, &user.Username, &user.PlatformAdmin, &user.Disabled, &user.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *Store) SetUserDisabled(ctx context.Context, userID int64, disabled bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockAdminChanges(ctx, tx); err != nil {
		return err
	}
	if disabled {
		var isAdmin bool
		if err := tx.QueryRowContext(ctx, `SELECT is_platform_admin FROM ac_users WHERE id = $1 FOR UPDATE`, userID).Scan(&isAdmin); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if isAdmin {
			var otherAdmins int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ac_users WHERE is_platform_admin AND NOT disabled AND id <> $1`, userID).Scan(&otherAdmins); err != nil {
				return err
			}
			if otherAdmins == 0 {
				return InputError("Platform Admin terakhir tidak bisa dinonaktifkan")
			}
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE ac_users SET disabled = $2 WHERE id = $1`, userID, disabled)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ac_sessions WHERE user_id = $1`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetUserPassword(ctx context.Context, userID int64, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE ac_users SET password_hash = $2 WHERE id = $1`, userID, hash)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ac_sessions WHERE user_id = $1`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetUserPlatformAdmin(ctx context.Context, userID int64, isAdmin bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockAdminChanges(ctx, tx); err != nil {
		return err
	}
	if !isAdmin {
		var admins int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ac_users WHERE is_platform_admin AND NOT disabled AND id <> $1`, userID).Scan(&admins); err != nil {
			return err
		}
		var currentlyAdmin bool
		if err := tx.QueryRowContext(ctx, `SELECT is_platform_admin FROM ac_users WHERE id = $1 FOR UPDATE`, userID).Scan(&currentlyAdmin); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if currentlyAdmin && admins == 0 {
			return InputError("Platform Admin terakhir tidak bisa dinonaktifkan")
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE ac_users SET is_platform_admin = $2 WHERE id = $1`, userID, isAdmin)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ac_sessions SET rotated_at = TO_TIMESTAMP(0) WHERE user_id = $1`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateGroup(ctx context.Context, name string) (Group, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return Group{}, InputError("nama grup harus berisi 1 sampai 80 karakter")
	}
	var group Group
	err := s.db.QueryRowContext(ctx, `INSERT INTO ac_groups (name) VALUES ($1) RETURNING id, name, created_at`, name).
		Scan(&group.ID, &group.Name, &group.CreatedAt)
	if err != nil {
		return Group{}, mapDatabaseError(err)
	}
	return group, nil
}

func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at FROM ac_groups ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	groups := make([]Group, 0)
	for rows.Next() {
		var group Group
		if err := rows.Scan(&group.ID, &group.Name, &group.CreatedAt); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (s *Store) ListGroupMembers(ctx context.Context, groupID int64) ([]User, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM ac_groups WHERE id = $1)`, groupID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.username, u.is_platform_admin, u.disabled, u.created_at
		FROM ac_group_members m JOIN ac_users u ON u.id = m.user_id
		WHERE m.group_id = $1 ORDER BY u.id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.ID, &user.Username, &user.PlatformAdmin, &user.Disabled, &user.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *Store) ListDatabaseGrants(ctx context.Context, groupID int64) ([]DatabaseGrant, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM ac_groups WHERE id = $1)`, groupID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT database_id, access_level FROM ac_database_grants WHERE group_id = $1 ORDER BY database_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := make([]DatabaseGrant, 0)
	for rows.Next() {
		var grant DatabaseGrant
		if err := rows.Scan(&grant.DatabaseID, &grant.Access); err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

func (s *Store) DeleteGroup(ctx context.Context, groupID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE ac_sessions SET rotated_at = TO_TIMESTAMP(0)
		WHERE user_id IN (SELECT user_id FROM ac_group_members WHERE group_id = $1)`, groupID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM ac_groups WHERE id = $1`, groupID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) AddMember(ctx context.Context, groupID, userID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO ac_group_members (group_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, groupID, userID); err != nil {
		return mapDatabaseError(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ac_sessions SET rotated_at = TO_TIMESTAMP(0) WHERE user_id = $1`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RemoveMember(ctx context.Context, groupID, userID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM ac_group_members WHERE group_id = $1 AND user_id = $2`, groupID, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ac_sessions SET rotated_at = TO_TIMESTAMP(0) WHERE user_id = $1`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetDatabaseAccess(ctx context.Context, groupID int64, databaseID string, access DatabaseAccess) error {
	databaseID = strings.TrimSpace(databaseID)
	if databaseID == "" || len(databaseID) > 200 {
		return InputError("databaseID harus berisi 1 sampai 200 karakter")
	}
	if access != AccessViewOnly && access != AccessViewEdit {
		return InputError("tingkat akses tidak valid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ac_database_grants (group_id, database_id, access_level)
		VALUES ($1, $2, $3)
		ON CONFLICT (group_id, database_id) DO UPDATE SET access_level = EXCLUDED.access_level`, groupID, databaseID, access); err != nil {
		return mapDatabaseError(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ac_sessions SET rotated_at = TO_TIMESTAMP(0) WHERE user_id IN (SELECT user_id FROM ac_group_members WHERE group_id = $1)`, groupID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RemoveDatabaseAccess(ctx context.Context, groupID int64, databaseID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE ac_sessions SET rotated_at = TO_TIMESTAMP(0)
		WHERE user_id IN (SELECT user_id FROM ac_group_members WHERE group_id = $1)`, groupID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM ac_database_grants WHERE group_id = $1 AND database_id = $2`, groupID, databaseID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) EffectiveAccess(ctx context.Context, userID int64, databaseID string) (DatabaseAccess, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT g.access_level
		FROM ac_group_members m
		JOIN ac_database_grants g ON g.group_id = m.group_id
		WHERE m.user_id = $1 AND g.database_id = $2`, userID, databaseID)
	if err != nil {
		return AccessNone, err
	}
	defer rows.Close()

	grants := make([]DatabaseAccess, 0)
	for rows.Next() {
		var grant DatabaseAccess
		if err := rows.Scan(&grant); err != nil {
			return AccessNone, err
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return AccessNone, err
	}
	return EffectiveAccess(grants), nil
}

func (s *Store) VerifyCredentials(ctx context.Context, username, password string) (User, error) {
	username, err := normalizeUsername(username)
	if err != nil {
		return User{}, ErrInvalidCredentials
	}
	var user User
	var hash string
	err = s.db.QueryRowContext(ctx, `
		SELECT id, username, password_hash, is_platform_admin, disabled, created_at
		FROM ac_users WHERE username = $1`, username).
		Scan(&user.ID, &user.Username, &hash, &user.PlatformAdmin, &user.Disabled, &user.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrInvalidCredentials
	}
	if err != nil {
		return User{}, err
	}
	if user.Disabled || !CheckPassword(password, hash) {
		return User{}, ErrInvalidCredentials
	}
	return user, nil
}

func mapDatabaseError(err error) error {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" {
		return ErrConflict
	}
	if errors.As(err, &pqErr) && pqErr.Code == "23503" {
		return ErrNotFound
	}
	return err
}

func lockAdminChanges(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT pg_advisory_xact_lock(311003)`)
	if err != nil {
		return err
	}
	return rows.Close()
}
