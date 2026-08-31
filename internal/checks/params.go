// Package checks implements RestoreLab's built-in check engine: the checks
// that turn "the VM booted" into "the service actually works", plus the
// registry that runs them with retries and timeouts.
package checks

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// Params provides typed, template-expanding access to a check's free-form
// parameters (core.CheckConfig.Params, decoded from YAML). String-typed
// values are expanded as a Go text/template against the target's template
// variables (core.Target.TemplateVars), so every check gets
// "{{ .ip }}"-style templating for free. Decoding problems (missing
// required fields, bad types, bad templates) are accumulated rather than
// returned immediately, so a bad config reports every problem at once; call
// Err after decoding every field to check for them.
type Params struct {
	raw  map[string]any
	vars map[string]string
	errs []error
}

// NewParams builds a Params view over cfg's params for the given target.
func NewParams(params map[string]any, target core.Target) *Params {
	return &Params{raw: params, vars: target.TemplateVars()}
}

// Err returns a combined error for every problem recorded while decoding
// parameters, or nil if there were none.
func (p *Params) Err() error {
	if len(p.errs) == 0 {
		return nil
	}
	return errors.Join(p.errs...)
}

func (p *Params) addErrf(format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf(format, args...))
}

func (p *Params) lookup(key string) (any, bool) {
	if p.raw == nil {
		return nil, false
	}
	v, ok := p.raw[key]
	return v, ok
}

// expand renders s as a text/template against the target's template
// variables. A template referencing an unknown key is recorded as an
// error rather than silently rendering "<no value>".
func (p *Params) expand(key, s string) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	tmpl, err := template.New(key).Option("missingkey=error").Parse(s)
	if err != nil {
		p.addErrf("param %q: invalid template: %w", key, err)
		return s
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p.vars); err != nil {
		p.addErrf("param %q: template references an unknown variable: %w", key, err)
		return s
	}
	return buf.String()
}

// String returns the string value of key, template-expanded, or def if the
// key is unset.
func (p *Params) String(key, def string) string {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	s, ok := v.(string)
	if !ok {
		s = fmt.Sprintf("%v", v)
	}
	return p.expand(key, s)
}

// RequireString returns the string value of key, recording an error if it
// is missing or blank.
func (p *Params) RequireString(key string) string {
	_, ok := p.lookup(key)
	if !ok {
		p.addErrf("param %q is required", key)
		return ""
	}
	s := p.String(key, "")
	if strings.TrimSpace(s) == "" {
		p.addErrf("param %q must not be empty", key)
	}
	return s
}

// Any returns the raw value of key (template-expanded if it is a string),
// or nil, false if the key is unset. Useful for values whose type isn't
// known ahead of time, such as json_equals.
func (p *Params) Any(key string) (any, bool) {
	v, ok := p.lookup(key)
	if !ok {
		return nil, false
	}
	if s, isStr := v.(string); isStr {
		return p.expand(key, s), true
	}
	return v, true
}

func toInt(v any) (int, error) {
	switch t := v.(type) {
	case int:
		return t, nil
	case int32:
		return int(t), nil
	case int64:
		return int(t), nil
	case float64:
		return int(t), nil
	case float32:
		return int(t), nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, fmt.Errorf("expected an integer, got %q", t)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("expected an integer, got %T", v)
	}
}

// Int coerces the value of key to int, or returns def if unset. YAML (and
// JSON-shaped params) may hand back int, int64, float64 or a numeric
// string; all are accepted.
func (p *Params) Int(key string, def int) int {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	n, err := toInt(v)
	if err != nil {
		p.addErrf("param %q: %w", key, err)
		return def
	}
	return n
}

// RequireInt returns the int value of key, recording an error if it is
// missing or not coercible to an integer.
func (p *Params) RequireInt(key string) int {
	v, ok := p.lookup(key)
	if !ok {
		p.addErrf("param %q is required", key)
		return 0
	}
	n, err := toInt(v)
	if err != nil {
		p.addErrf("param %q: %w", key, err)
		return 0
	}
	return n
}

// Bool coerces the value of key to bool, or returns def if unset.
func (p *Params) Bool(key string, def bool) bool {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		if err != nil {
			p.addErrf("param %q: expected a boolean, got %q", key, t)
			return def
		}
		return b
	default:
		p.addErrf("param %q: expected a boolean, got %T", key, v)
		return def
	}
}

// Duration coerces the value of key to a time.Duration, or returns def if
// unset. A string is parsed with time.ParseDuration ("500ms", "5s"); a bare
// number is treated as a count of seconds.
func (p *Params) Duration(key string, def time.Duration) time.Duration {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	switch t := v.(type) {
	case string:
		s := p.expand(key, t)
		d, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			p.addErrf("param %q: expected a duration (e.g. \"500ms\"), got %q", key, s)
			return def
		}
		return d
	case int:
		return time.Duration(t) * time.Second
	case int64:
		return time.Duration(t) * time.Second
	case float64:
		return time.Duration(t * float64(time.Second))
	default:
		p.addErrf("param %q: expected a duration, got %T", key, v)
		return def
	}
}

// StringSlice returns the value of key as a slice of template-expanded
// strings. A single scalar is treated as a one-element slice. Missing keys
// return nil.
func (p *Params) StringSlice(key string) []string {
	v, ok := p.lookup(key)
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []string:
		out := make([]string, len(t))
		for i, s := range t {
			out[i] = p.expand(key, s)
		}
		return out
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			s, ok := item.(string)
			if !ok {
				s = fmt.Sprintf("%v", item)
			}
			out = append(out, p.expand(key, s))
		}
		return out
	case string:
		return []string{p.expand(key, t)}
	default:
		p.addErrf("param %q: expected a list of strings, got %T", key, v)
		return nil
	}
}

// IntSlice returns the value of key as a slice of ints. Missing keys return
// nil.
func (p *Params) IntSlice(key string) []int {
	v, ok := p.lookup(key)
	if !ok {
		return nil
	}
	asSlice, ok := v.([]any)
	if !ok {
		p.addErrf("param %q: expected a list of integers, got %T", key, v)
		return nil
	}
	out := make([]int, 0, len(asSlice))
	for _, item := range asSlice {
		n, err := toInt(item)
		if err != nil {
			p.addErrf("param %q: %w", key, err)
			continue
		}
		out = append(out, n)
	}
	return out
}

// Map returns the value of key as a map[string]any. Missing keys return
// nil.
func (p *Params) Map(key string) map[string]any {
	v, ok := p.lookup(key)
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case map[string]any:
		return t
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fmt.Sprintf("%v", k)] = val
		}
		return out
	default:
		p.addErrf("param %q: expected a mapping, got %T", key, v)
		return nil
	}
}

// StringMap returns the value of key as a map[string]string, with every
// value template-expanded. Missing keys return nil.
func (p *Params) StringMap(key string) map[string]string {
	m := p.Map(key)
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			s = fmt.Sprintf("%v", v)
		}
		out[k] = p.expand(key+"."+k, s)
	}
	return out
}
