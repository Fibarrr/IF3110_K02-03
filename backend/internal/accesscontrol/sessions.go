package accesscontrol

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"
)

type SessionPolicy struct {
	IdleTimeout    time.Duration
	AbsoluteExpiry time.Duration
	RotateEvery    time.Duration
	GracePeriod    time.Duration
}

func DefaultSessionPolicy() SessionPolicy {
	return SessionPolicy{
		IdleTimeout:    30 * time.Minute,
		AbsoluteExpiry: 12 * time.Hour,
		RotateEvery:    15 * time.Minute,
		GracePeriod:    30 * time.Second,
	}
}

type ActiveSession struct {
	User         User
	CSRFToken    string
	ExpiresAt    time.Time
	RotatedToken string
}

type SessionManager struct {
	db     *sql.DB
	policy SessionPolicy
}

func NewSessionManager(db *sql.DB, policy SessionPolicy) *SessionManager {
	return &SessionManager{db: db, policy: policy}
}

func (m *SessionManager) Create(ctx context.Context, userID int64) (token, csrf string, expiresAt time.Time, err error) {
	token, err = randomToken()
	if err != nil {
		return "", "", time.Time{}, err
	}
	csrf, err = randomToken()
	if err != nil {
		return "", "", time.Time{}, err
	}
	expiresAt = time.Now().Add(m.policy.AbsoluteExpiry)
	if _, err = m.db.ExecContext(ctx, `DELETE FROM ac_sessions WHERE expires_at <= NOW() OR last_seen_at <= $1 OR revoked_at IS NOT NULL`, time.Now().Add(-m.policy.IdleTimeout)); err != nil {
		return "", "", time.Time{}, err
	}
	_, err = m.db.ExecContext(ctx, `
		INSERT INTO ac_sessions (user_id, token_hash, csrf_token, expires_at)
		VALUES ($1, $2, $3, $4)`, userID, hashToken(token), csrf, expiresAt)
	return token, csrf, expiresAt, err
}

func (m *SessionManager) Load(ctx context.Context, token string) (ActiveSession, error) {
	if token == "" {
		return ActiveSession{}, ErrNotFound
	}
	tokenHash := hashToken(token)
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return ActiveSession{}, err
	}
	defer tx.Rollback()

	var active ActiveSession
	var sessionID int64
	var currentHash, previousHash []byte
	var previousUntil sql.NullTime
	var lastSeenAt, rotatedAt time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT s.id, s.token_hash, s.previous_token_hash, s.previous_valid_until,
		       s.csrf_token, s.last_seen_at, s.rotated_at, s.expires_at,
		       u.id, u.username, u.is_platform_admin, u.disabled, u.created_at
		FROM ac_sessions s
		JOIN ac_users u ON u.id = s.user_id
		WHERE (s.token_hash = $1 OR (s.previous_token_hash = $1 AND s.previous_valid_until > NOW()))
		  AND s.revoked_at IS NULL
		FOR UPDATE OF s`, tokenHash).
		Scan(&sessionID, &currentHash, &previousHash, &previousUntil, &active.CSRFToken,
			&lastSeenAt, &rotatedAt, &active.ExpiresAt,
			&active.User.ID, &active.User.Username, &active.User.PlatformAdmin, &active.User.Disabled, &active.User.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ActiveSession{}, ErrNotFound
		}
		return ActiveSession{}, err
	}
	if active.User.Disabled || !active.ExpiresAt.After(time.Now()) || time.Since(lastSeenAt) >= m.policy.IdleTimeout {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ac_sessions WHERE id = $1`, sessionID); err != nil {
			return ActiveSession{}, err
		}
		if err := tx.Commit(); err != nil {
			return ActiveSession{}, err
		}
		return ActiveSession{}, ErrNotFound
	}

	isCurrentToken := subtle.ConstantTimeCompare(currentHash, tokenHash) == 1
	now := time.Now()
	if isCurrentToken && now.Sub(rotatedAt) >= m.policy.RotateEvery {
		newToken, err := randomToken()
		if err != nil {
			return ActiveSession{}, err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE ac_sessions
			SET token_hash = $2, previous_token_hash = $3, previous_valid_until = $4,
			    rotated_at = $5, last_seen_at = $5
			WHERE id = $1`, sessionID, hashToken(newToken), currentHash, now.Add(m.policy.GracePeriod), now)
		if err != nil {
			return ActiveSession{}, err
		}
		active.RotatedToken = newToken
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE ac_sessions SET last_seen_at = $2 WHERE id = $1`, sessionID, now)
		if err != nil {
			return ActiveSession{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ActiveSession{}, err
	}
	return active, nil
}

func (m *SessionManager) Revoke(ctx context.Context, token string) error {
	_, err := m.db.ExecContext(ctx, `DELETE FROM ac_sessions WHERE token_hash = $1 OR previous_token_hash = $1`, hashToken(token))
	return err
}

func randomToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func hashToken(token string) []byte {
	hash := sha256.Sum256([]byte(token))
	return hash[:]
}
