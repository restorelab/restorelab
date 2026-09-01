package store

import (
	"encoding/json"
	"time"
)

// timeLayout is RFC 3339 in UTC with every nanosecond digit written, so the
// text form is fixed width.
//
// This is not cosmetic. Both engines store timestamps as text here, so
// "ORDER BY started_at DESC" is a lexicographic sort — the order of the whole
// history. time.RFC3339Nano trims trailing zeros, which makes "…:05.1Z" sort
// after "…:05.05Z" even though it is the earlier instant. A fixed width
// removes that problem, and forcing UTC removes the other one: a column
// holding mixed offsets cannot be ordered at all.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// formatNullTime encodes the zero time as SQL NULL, so "never completed" and
// "completed at the zero instant" stay distinguishable.
func formatNullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return formatTime(t)
}

func parseNullTime(s *string) (time.Time, error) {
	if s == nil || *s == "" {
		return time.Time{}, nil
	}
	return parseTime(*s)
}

// encodeJSON renders a value as compact JSON for a text column. A nil value
// encodes as the empty string, which nullString then turns into SQL NULL.
func encodeJSON(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decodeJSON fills out from raw, treating empty input as "nothing to do"
// rather than as malformed JSON: most of these columns are legitimately null.
func decodeJSON(raw []byte, out any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// boolToInt and intToBool bridge SQLite's missing boolean type. PostgreSQL
// accepts the same integer column, so one schema serves both engines.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func intToBool(i int) bool { return i != 0 }

// nowUTC exists so a test can be written against a stable clock later without
// reaching for a package-level variable.
func nowUTC() time.Time { return time.Now().UTC() }
