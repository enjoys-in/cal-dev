package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/enjoys-in/cal-dev/internal/models"
)

// apiKeyPrefix marks CalDev onboarding keys so they are recognisable in logs and config.
const apiKeyPrefix = "cdk_"

// HashAPIKey returns the hex SHA-256 under which a key is stored.
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// EmailDomain returns the lower-case domain of an email address, or "" when there is none.
func EmailDomain(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return email[at+1:]
}

const apiKeyColumns = `id, owner_email, domain, label, key_prefix, created_by, created_at, last_used_at, revoked_at`

func scanAPIKey(row interface{ Scan(...any) error }) (*models.APIKey, error) {
	k := &models.APIKey{}
	var createdBy sql.NullInt64
	if err := row.Scan(&k.ID, &k.OwnerEmail, &k.Domain, &k.Label, &k.KeyPrefix, &createdBy,
		&k.CreatedAt, &k.LastUsedAt, &k.RevokedAt); err != nil {
		return nil, err
	}
	if createdBy.Valid {
		id := int(createdBy.Int64)
		k.CreatedBy = &id
	}
	return k, nil
}

// CreateAPIKey creates a key for ownerEmail (scoped to its domain) and returns the key itself,
// which is not stored and can't be shown again. createdBy is nil for keys made by the admin.
func (s *Store) CreateAPIKey(ctx context.Context, ownerEmail, label string, createdBy *int) (string, *models.APIKey, error) {
	ownerEmail = strings.ToLower(strings.TrimSpace(ownerEmail))
	domain := EmailDomain(ownerEmail)
	if domain == "" {
		return "", nil, errors.New("owner email has no domain")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	key := apiKeyPrefix + base64.RawURLEncoding.EncodeToString(raw)

	var creator any
	if createdBy != nil {
		creator = *createdBy
	}
	k, err := scanAPIKey(s.db.QueryRowContext(ctx,
		`INSERT INTO caldev_api_keys (owner_email, domain, label, key_prefix, key_hash, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+apiKeyColumns,
		ownerEmail, domain, strings.TrimSpace(label), key[:len(apiKeyPrefix)+6], HashAPIKey(key), creator))
	if err != nil {
		return "", nil, err
	}
	return key, k, nil
}

// GetActiveAPIKey returns the non-revoked key matching the given key text, or nil.
func (s *Store) GetActiveAPIKey(ctx context.Context, key string) (*models.APIKey, error) {
	k, err := scanAPIKey(s.db.QueryRowContext(ctx,
		`SELECT `+apiKeyColumns+` FROM caldev_api_keys WHERE key_hash = $1 AND revoked_at IS NULL`,
		HashAPIKey(key)))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return k, err
}

// TouchAPIKey records that a key was just used.
func (s *Store) TouchAPIKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE caldev_api_keys SET last_used_at = NOW() WHERE id = $1`, id)
	return err
}

// ListAPIKeys returns keys, newest first: all of them when ownerEmail is "", else that owner's.
func (s *Store) ListAPIKeys(ctx context.Context, ownerEmail string) ([]*models.APIKey, error) {
	query := `SELECT ` + apiKeyColumns + ` FROM caldev_api_keys`
	var args []any
	if ownerEmail != "" {
		query += ` WHERE lower(owner_email) = lower($1)`
		args = append(args, strings.TrimSpace(ownerEmail))
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*models.APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey revokes a key; when ownerEmail is not "", only a key of that owner. It reports
// whether a key was revoked (false: no such active key).
func (s *Store) RevokeAPIKey(ctx context.Context, id, ownerEmail string) (bool, error) {
	query := `UPDATE caldev_api_keys SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL`
	args := []any{id}
	if ownerEmail != "" {
		query += ` AND lower(owner_email) = lower($2)`
		args = append(args, strings.TrimSpace(ownerEmail))
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
