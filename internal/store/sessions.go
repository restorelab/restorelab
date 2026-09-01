package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const insertSessionSQL = `
INSERT INTO api_sessions (id, hash, token_id, created_at, expires_at, user_agent)
VALUES (?, ?, ?, ?, ?, ?)`

const deleteExpiredSessionsSQL = `DELETE FROM api_sessions WHERE expires_at <= ?`

const deleteSessionSQL = `DELETE FROM api_sessions WHERE hash = ?`

// selectSessionByHashSQL joins the session to the token it names.
//
// The token half of the column list is built from selectTokenColumns rather
// than written again: one list, qualified for the join, so the two cannot
// drift. A hand-written copy of a column list beside the original is the
// shape of bug this codebase has already paid for once.
var selectSessionByHashSQL = `
SELECT s.id, s.hash, s.token_id, s.created_at, s.expires_at, s.user_agent, ` +
	qualify("t", selectTokenColumns) + `
FROM api_sessions s
JOIN api_tokens t ON t.id = s.token_id
WHERE s.hash = ? AND s.expires_at > ? AND t.revoked_at IS NULL`

// qualify prefixes every column of a select list with a table alias.
func qualify(alias, columns string) string {
	parts := strings.Split(columns, ", ")
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return strings.Join(parts, ", ")
}

// maxUserAgent is how much of a User-Agent is kept: a label for a human, not
// evidence.
const maxUserAgent = 256

// CreateSession records a session and sweeps the expired ones.
func (s *sqlStore) CreateSession(ctx context.Context, sess Session, now time.Time) error {
	if sess.Hash == "" {
		return fmt.Errorf("store: refusing to record a session with no hash")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// No-op once Commit has succeeded; it only matters on the error returns.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		rebind(s.dialect, deleteExpiredSessionsSQL), formatTime(now)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, rebind(s.dialect, insertSessionSQL),
		sess.ID, sess.Hash, sess.TokenID,
		formatTime(sess.CreatedAt), formatTime(sess.ExpiresAt),
		encodeUserAgent(sess.UserAgent),
	); err != nil {
		return err
	}
	return tx.Commit()
}

// encodeUserAgent stores a bounded, valid-UTF-8 label, or SQL NULL.
//
// The cut is by bytes and then repaired: a header is attacker-controlled
// text, and a column holding half a rune is a row that reads back as
// mojibake in a listing.
func encodeUserAgent(ua string) any {
	if ua == "" {
		return nil
	}
	if len(ua) > maxUserAgent {
		ua = strings.ToValidUTF8(ua[:maxUserAgent], "")
	}
	return ua
}

// SessionByHash returns a live session and the live token it names.
func (s *sqlStore) SessionByHash(ctx context.Context, hash string, now time.Time) (*Session, *APIToken, error) {
	var (
		sess                 Session
		createdAt, expiresAt string
		userAgent            sql.NullString
	)
	row := s.queryRow(ctx, selectSessionByHashSQL, hash, formatTime(now))

	// scanToken owns the token column order and the parsing of its three
	// timestamps. Handing it a closure that prepends this row's own columns
	// keeps that knowledge in one place: there is still exactly one function
	// that knows what a token row looks like.
	dest := []any{&sess.ID, &sess.Hash, &sess.TokenID, &createdAt, &expiresAt, &userAgent}
	tok, err := scanToken(func(tokenDest ...any) error {
		return row.Scan(append(dest, tokenDest...)...)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}

	sess.UserAgent = userAgent.String
	if sess.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, nil, fmt.Errorf("store: session %s has an unreadable created_at: %w", sess.ID, err)
	}
	if sess.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return nil, nil, fmt.Errorf("store: session %s has an unreadable expires_at: %w", sess.ID, err)
	}
	return &sess, &tok, nil
}

// DeleteSession removes a session. Removing nothing is a success.
func (s *sqlStore) DeleteSession(ctx context.Context, hash string) error {
	return s.exec(ctx, deleteSessionSQL, hash)
}

// DeleteExpiredSessions removes every session past its expiry.
func (s *sqlStore) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	return s.execCount(ctx, deleteExpiredSessionsSQL, formatTime(now))
}
