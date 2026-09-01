package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const insertTokenSQL = `
INSERT INTO api_tokens (id, name, hash, created_at, last_used_at, revoked_at, scopes)
VALUES (?, ?, ?, ?, NULL, NULL, ?)`

// CreateToken records a new API token. The caller has already hashed the
// secret: this package never sees one.
func (s *sqlStore) CreateToken(ctx context.Context, t APIToken) error {
	if t.Hash == "" {
		return fmt.Errorf("store: refusing to record a token with no hash")
	}
	return s.exec(ctx, insertTokenSQL,
		t.ID, t.Name, t.Hash, formatTime(t.CreatedAt), encodeScopes(t.Scopes))
}

// encodeScopes renders a token's scopes for storage. Empty means read only.
func encodeScopes(scopes []string) string {
	if len(scopes) == 0 {
		return ScopeRead
	}
	return strings.Join(scopes, ",")
}

// decodeScopes parses what encodeScopes wrote.
func decodeScopes(raw string) []string {
	if raw == "" {
		return []string{ScopeRead}
	}
	return strings.Split(raw, ",")
}

const selectTokenColumns = `id, name, hash, created_at, last_used_at, revoked_at, scopes`

const selectTokenByHashSQL = `
SELECT ` + selectTokenColumns + `
FROM api_tokens WHERE hash = ? AND revoked_at IS NULL`

// TokenByHash returns the live token with this hash.
//
// Revocation is filtered in SQL rather than by the caller: a caller that
// forgets the check would keep honouring a revoked credential, and that is
// not a mistake worth leaving available.
func (s *sqlStore) TokenByHash(ctx context.Context, hash string) (*APIToken, error) {
	t, err := scanToken(s.queryRow(ctx, selectTokenByHashSQL, hash).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

const selectTokensSQL = `
SELECT ` + selectTokenColumns + `
FROM api_tokens ORDER BY created_at, id`

// ListTokens returns every token, oldest first, revoked ones included: the
// point of `token list` is to see what exists, and a revoked token is part of
// the answer.
func (s *sqlStore) ListTokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.query(ctx, selectTokensSQL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []APIToken
	for rows.Next() {
		t, err := scanToken(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

const revokeTokenSQL = `
UPDATE api_tokens SET revoked_at = ? WHERE name = ? AND revoked_at IS NULL`

// RevokeToken marks the named token revoked.
func (s *sqlStore) RevokeToken(ctx context.Context, name string, at time.Time) error {
	n, err := s.execCount(ctx, revokeTokenSQL, formatTime(at), name)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: token %s", ErrNotFound, name)
	}
	return nil
}

const touchTokenSQL = `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`

// TouchToken records that a token was used.
func (s *sqlStore) TouchToken(ctx context.Context, id string, at time.Time) error {
	return s.exec(ctx, touchTokenSQL, formatTime(at), id)
}

// scanToken reads one row from either a *sql.Row or a *sql.Rows: both expose
// the same Scan signature, and passing the method is enough to share the
// column order between the single-row and the listing query.
func scanToken(scan func(dest ...any) error) (APIToken, error) {
	var (
		t                     APIToken
		createdAt             string
		lastUsedAt, revokedAt sql.NullString
		scopes                sql.NullString
	)
	if err := scan(&t.ID, &t.Name, &t.Hash, &createdAt, &lastUsedAt, &revokedAt, &scopes); err != nil {
		return APIToken{}, err
	}
	t.Scopes = decodeScopes(scopes.String)

	var err error
	if t.CreatedAt, err = parseTime(createdAt); err != nil {
		return APIToken{}, fmt.Errorf("store: token %s has an unreadable created_at: %w", t.ID, err)
	}
	if t.LastUsedAt, err = parseNullTime(nullable(lastUsedAt)); err != nil {
		return APIToken{}, fmt.Errorf("store: token %s has an unreadable last_used_at: %w", t.ID, err)
	}
	if t.RevokedAt, err = parseNullTime(nullable(revokedAt)); err != nil {
		return APIToken{}, fmt.Errorf("store: token %s has an unreadable revoked_at: %w", t.ID, err)
	}
	return t, nil
}
