package plan

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that reads "180s", "5m" or a plain number of
// seconds from YAML, and marshals back to Go duration syntax.
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML accepts "90s", "2m30s", or an integer number of seconds.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw any
	if err := node.Decode(&raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case string:
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", v, err)
		}
		*d = Duration(parsed)
	case int:
		*d = Duration(time.Duration(v) * time.Second)
	case float64:
		*d = Duration(time.Duration(v * float64(time.Second)))
	case nil:
		*d = 0
	default:
		return fmt.Errorf("invalid duration value %v (%T)", raw, raw)
	}
	return nil
}

// MarshalYAML renders the duration as a Go duration string.
func (d Duration) MarshalYAML() (any, error) {
	if d == 0 {
		return nil, nil
	}
	return time.Duration(d).String(), nil
}

// Or returns the duration, or fallback when unset.
func (d Duration) Or(fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return time.Duration(d)
}
