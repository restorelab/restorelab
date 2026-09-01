package api

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/restorelab/restorelab/internal/store"
)

// maxPageSize caps a page however large a limit is asked for. A client that
// wants everything gets it a page at a time; a client that asks for a hundred
// thousand rows gets two hundred and a cursor.
const maxPageSize = 200

// cursorLayout is the fixed-width RFC 3339 form the history stores. Fixed
// width matters for the same reason it matters in the database: it makes the
// text comparison a chronological one.
const cursorLayout = "2006-01-02T15:04:05.000000000Z"

// page is the envelope every listing returns. NextCursor is absent when there
// is nothing after this page.
type page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// encodeCursor renders a listing position as an opaque string.
//
// Opaque is the contract: a client must treat it as a token to hand back, so
// that what it encodes can change (B2 adds a queue) without breaking anyone.
// base64url keeps it safe in a query string without escaping.
func encodeCursor(p store.Position) string {
	raw := p.StartedAt.UTC().Format(cursorLayout) + "|" + p.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses what encodeCursor produced.
func decodeCursor(s string) (store.Position, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return store.Position{}, fmt.Errorf("cursor is not valid base64url")
	}
	text, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return store.Position{}, fmt.Errorf("cursor is malformed")
	}
	at, err := time.Parse(cursorLayout, text)
	if err != nil {
		return store.Position{}, fmt.Errorf("cursor carries an unreadable timestamp")
	}
	return store.Position{StartedAt: at.UTC(), ID: id}, nil
}

// parseLimit reads the page size a client asked for.
//
// A limit above the cap is honoured up to the cap rather than refused: the
// client gets a smaller page and a cursor, which is what it wanted anyway.
// A limit of zero or below is refused, because it means the caller computed
// it and got it wrong.
func parseLimit(raw string) (int, error) {
	if raw == "" {
		return store.DefaultListLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("limit must be a number")
	}
	if n <= 0 {
		return 0, fmt.Errorf("limit must be greater than zero")
	}
	if n > maxPageSize {
		return maxPageSize, nil
	}
	return n, nil
}

// parseSince accepts a day count ("30d"), a Go duration ("12h"), a date
// ("2026-08-01") or a full RFC 3339 instant.
//
// Anything else is refused rather than guessed at, exactly as `runs list
// --since` does: a listing silently covering the wrong window is worse than
// an error.
func parseSince(s string, now time.Time) (time.Time, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n >= 0 {
			return now.AddDate(0, 0, -n), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("could not read %q as a date: use 30d, 12h, 2026-08-01, or an RFC 3339 instant", s)
}
