package checks

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// defaultMaxBodyBytes bounds how much of an HTTP response body the check
// buffers, so a check never tries to hold a whole disk image in memory.
const defaultMaxBodyBytes = 1 << 20 // 1 MiB

// maxBodySnippet bounds how much of the body is kept for CheckResult.Details.
const maxBodySnippet = 512

// httpCheck implements the "http" and "https" check types. The two are the
// same implementation registered under different type names: the scheme
// that matters lives in the url param, not in the check type.
//
// Params:
//   - url: request URL, template-expanded (required)
//   - method: HTTP method (default: GET)
//   - expected_status: status code that counts as a pass (default: 200)
//   - expected_statuses: list of acceptable status codes; overrides
//     expected_status when set
//   - headers: map of request headers, values template-expanded
//   - body: request body
//   - body_contains: substring the response body must contain
//   - body_matches: regular expression the response body must match
//   - json_path: dotted path into a JSON response body (e.g.
//     "status.database", "items.0.name")
//   - json_equals: value the json_path lookup must equal (compared
//     numerically when both sides are numbers, otherwise as text)
//   - insecure_tls: skip TLS certificate verification (default: false)
//   - follow_redirects: follow HTTP redirects (default: true)
//   - max_body_bytes: cap on response body bytes read (default: 1 MiB)
type httpCheck struct {
	typ string
}

func newHTTPCheck(typ string) *httpCheck { return &httpCheck{typ: typ} }

func (c *httpCheck) Type() string { return c.typ }

func (c *httpCheck) Run(ctx context.Context, target core.Target, cfg core.CheckConfig) core.CheckResult {
	p := NewParams(cfg.Params, target)
	rawURL := p.RequireString("url")
	method := strings.ToUpper(p.String("method", http.MethodGet))
	expectedStatus := p.Int("expected_status", http.StatusOK)
	expectedStatuses := p.IntSlice("expected_statuses")
	headers := p.StringMap("headers")
	body := p.String("body", "")
	bodyContains := p.String("body_contains", "")
	bodyMatches := p.String("body_matches", "")
	jsonPath := p.String("json_path", "")
	jsonEquals, hasJSONEquals := p.Any("json_equals")
	insecureTLS := p.Bool("insecure_tls", false)
	followRedirects := p.Bool("follow_redirects", true)
	maxBodyBytes := p.Int("max_body_bytes", defaultMaxBodyBytes)
	if err := p.Err(); err != nil {
		return core.CheckResult{Status: core.CheckError, Message: err.Error()}
	}
	if maxBodyBytes <= 0 {
		maxBodyBytes = defaultMaxBodyBytes
	}

	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	if err != nil {
		return core.CheckResult{Status: core.CheckError, Message: fmt.Sprintf("http: invalid request %s %s: %v", method, rawURL, err)}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	transport := &http.Transport{}
	if insecureTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in via params
	}
	client := &http.Client{Transport: transport}
	if !followRedirects {
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start)
	if err != nil {
		details := map[string]any{"latency_ms": float64(latency) / float64(time.Millisecond)}
		// Nothing answered at the transport level, so there is no HTTP verdict
		// to give: see reachability.go. A server that answered with a bad
		// status is handled further down, and that one IS a failure.
		if cause, silent := dialFailure(err); silent {
			return core.CheckResult{
				Status:  core.CheckError,
				Message: fmt.Sprintf("%s %s: %s after %s - %s", method, rawURL, cause, latency.Round(time.Millisecond), noRouteHint),
				Details: details,
			}
		}
		return core.CheckResult{
			Status:  core.CheckFail,
			Message: fmt.Sprintf("%s %s failed after %s: %s", method, rawURL, latency.Round(time.Millisecond), describeHTTPErr(err)),
			Details: details,
		}
	}
	defer func() { _ = resp.Body.Close() }()

	limited := io.LimitReader(resp.Body, int64(maxBodyBytes))
	data, err := io.ReadAll(limited)
	if err != nil {
		return core.CheckResult{
			Status:  core.CheckFail,
			Message: fmt.Sprintf("%s %s -> %d but reading the body failed: %v", method, rawURL, resp.StatusCode, err),
			Details: map[string]any{"status_code": resp.StatusCode, "latency_ms": float64(latency) / float64(time.Millisecond)},
		}
	}

	details := map[string]any{
		"status_code":  resp.StatusCode,
		"latency_ms":   float64(latency) / float64(time.Millisecond),
		"body_size":    len(data),
		"body_snippet": truncate(string(data), maxBodySnippet),
	}

	statusOK := len(expectedStatuses) > 0
	if statusOK {
		statusOK = false
		for _, s := range expectedStatuses {
			if s == resp.StatusCode {
				statusOK = true
				break
			}
		}
	} else {
		statusOK = resp.StatusCode == expectedStatus
	}
	if !statusOK {
		wanted := strconv.Itoa(expectedStatus)
		if len(expectedStatuses) > 0 {
			wanted = intsToString(expectedStatuses)
		}
		return core.CheckResult{
			Status:  core.CheckFail,
			Message: fmt.Sprintf("%s %s -> %d (expected %s) in %s", method, rawURL, resp.StatusCode, wanted, latency.Round(time.Millisecond)),
			Details: details,
		}
	}

	if bodyContains != "" && !bytes.Contains(data, []byte(bodyContains)) {
		return core.CheckResult{
			Status:  core.CheckFail,
			Message: fmt.Sprintf("%s %s -> %d but body does not contain %q (body: %s)", method, rawURL, resp.StatusCode, bodyContains, truncate(string(data), 200)),
			Details: details,
		}
	}

	if bodyMatches != "" {
		re, err := regexp.Compile(bodyMatches)
		if err != nil {
			return core.CheckResult{Status: core.CheckError, Message: fmt.Sprintf("http: invalid body_matches regex %q: %v", bodyMatches, err), Details: details}
		}
		if !re.Match(data) {
			return core.CheckResult{
				Status:  core.CheckFail,
				Message: fmt.Sprintf("%s %s -> %d but body does not match /%s/ (body: %s)", method, rawURL, resp.StatusCode, bodyMatches, truncate(string(data), 200)),
				Details: details,
			}
		}
	}

	if jsonPath != "" {
		var root any
		if err := json.Unmarshal(data, &root); err != nil {
			return core.CheckResult{
				Status:  core.CheckFail,
				Message: fmt.Sprintf("%s %s -> %d but body is not valid JSON: %v", method, rawURL, resp.StatusCode, err),
				Details: details,
			}
		}
		got, err := jsonPathLookup(root, jsonPath)
		if err != nil {
			return core.CheckResult{
				Status:  core.CheckFail,
				Message: fmt.Sprintf("%s %s -> %d but json_path %q: %v", method, rawURL, resp.StatusCode, jsonPath, err),
				Details: details,
			}
		}
		if hasJSONEquals && !jsonValuesEqual(got, jsonEquals) {
			return core.CheckResult{
				Status:  core.CheckFail,
				Message: fmt.Sprintf("%s %s -> %d but json_path %q = %v (expected %v)", method, rawURL, resp.StatusCode, jsonPath, got, jsonEquals),
				Details: details,
			}
		}
	}

	return core.CheckResult{
		Status:  core.CheckPass,
		Message: fmt.Sprintf("%s %s -> %d in %s", method, rawURL, resp.StatusCode, latency.Round(time.Millisecond)),
		Details: details,
	}
}

func describeHTTPErr(err error) string {
	msg := err.Error()
	if idx := strings.Index(msg, `"`); idx != -1 {
		// net/url wraps dial errors as `Get "http://...": dial tcp ...: ...`;
		// keep the root cause after the quoted URL.
		if end := strings.Index(msg[idx+1:], `"`); end != -1 {
			rest := msg[idx+1+end+1:]
			rest = strings.TrimPrefix(rest, ": ")
			if rest != "" {
				return rest
			}
		}
	}
	return msg
}

func intsToString(ints []int) string {
	parts := make([]string, len(ints))
	for i, n := range ints {
		parts[i] = strconv.Itoa(n)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// jsonPathLookup walks a decoded JSON value (map[string]any / []any /
// scalars) following a dotted path such as "status.database" or
// "items.0.name".
func jsonPathLookup(root any, path string) (any, error) {
	cur := root
	for _, part := range strings.Split(path, ".") {
		if part == "" {
			continue
		}
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[part]
			if !ok {
				return nil, fmt.Errorf("key %q not found", part)
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("expected an array index, got %q", part)
			}
			if idx < 0 || idx >= len(node) {
				return nil, fmt.Errorf("array index %d out of range (length %d)", idx, len(node))
			}
			cur = node[idx]
		default:
			return nil, fmt.Errorf("cannot descend into %T at %q", cur, part)
		}
	}
	return cur, nil
}

// jsonValuesEqual compares a JSON-decoded value against an expected value
// from params, treating numbers as equal regardless of Go type (JSON always
// decodes numbers as float64, but params may hand back int).
func jsonValuesEqual(got, want any) bool {
	if gf, ok := toFloatValue(got); ok {
		if wf, ok := toFloatValue(want); ok {
			return gf == wf
		}
	}
	return fmt.Sprintf("%v", got) == fmt.Sprintf("%v", want)
}

func toFloatValue(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	default:
		return 0, false
	}
}

var _ core.Check = (*httpCheck)(nil)
