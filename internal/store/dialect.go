package store

import (
	"strconv"
	"strings"
)

// Dialect names one of the two supported engines.
//
// The query set is written once, in the SQL subset both engines accept; only
// the placeholder syntax differs, and rebind is the whole of that difference.
// One query set cannot drift from itself, which is the point.
type Dialect string

const (
	dialectSQLite   Dialect = "sqlite"
	dialectPostgres Dialect = "postgres"
)

// rebind rewrites a query written with "?" placeholders into the dialect's
// own syntax. SQLite takes "?" as-is; PostgreSQL wants "$1", "$2", ...
//
// Question marks inside single-quoted literals are left alone: they are data,
// not placeholders. The queries in this package never contain a literal with
// an embedded escaped quote, so a simple in-literal toggle is enough and a
// real SQL lexer is not.
func rebind(d Dialect, query string) string {
	if d != dialectPostgres {
		return query
	}

	var b strings.Builder
	b.Grow(len(query) + 8)

	inLiteral := false
	n := 0
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'':
			inLiteral = !inLiteral
			b.WriteByte(c)
		case c == '?' && !inLiteral:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
