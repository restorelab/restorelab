package pbs

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// jsonNumber decodes a numeric field that PBS may encode either as a JSON
// number (1735689600) or, on some endpoints/versions, as a JSON string
// ("1735689600"). encoding/json's own json.Number only handles the former,
// so this type tries both and stores the canonical string form.
type jsonNumber string

// UnmarshalJSON accepts a bare number, a quoted number, or a JSON null
// (treated as empty/zero).
func (n *jsonNumber) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" {
		*n = ""
		return nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var unquoted string
		if err := json.Unmarshal(b, &unquoted); err != nil {
			return fmt.Errorf("pbs: decoding quoted number: %w", err)
		}
		*n = jsonNumber(unquoted)
		return nil
	}
	*n = jsonNumber(s)
	return nil
}

// Int64 parses the value as an int64, returning 0 for an empty value.
func (n jsonNumber) Int64() (int64, error) {
	if n == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(string(n), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("pbs: %q is not an integer: %w", string(n), err)
	}
	return v, nil
}
