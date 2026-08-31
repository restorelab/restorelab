package cli

import (
	"fmt"
	"net/url"
	"strings"
)

// normalizeEndpoint validates and cleans a provider endpoint, and turns the
// mistakes people actually make into an error that says what to type instead.
//
// Without this, a missing colon in "https://" reaches the HTTP client and comes
// back as `unsupported protocol scheme ""`, which tells the user nothing about
// what they typed.
func normalizeEndpoint(raw string, defaultPort int) (string, error) {
	endpoint := strings.TrimSpace(raw)
	if endpoint == "" {
		return "", fmt.Errorf("no endpoint given")
	}
	endpoint = strings.TrimRight(endpoint, "/")

	// "https//host" and "https:/host": a colon or a slash short of a URL.
	for _, scheme := range []string{"https", "http"} {
		for _, broken := range []string{scheme + "//", scheme + ":/"} {
			if strings.HasPrefix(endpoint, broken) && !strings.HasPrefix(endpoint, scheme+"://") {
				fixed := scheme + "://" + strings.TrimPrefix(endpoint, broken)
				return "", fmt.Errorf("invalid endpoint %q: did you mean %q?", raw, fixed)
			}
		}
	}

	// A bare host, with or without a port: HTTPS is the only sane default for
	// a Proxmox API, so fill it in rather than making the user retype.
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint %q: %w", raw, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("invalid endpoint %q: scheme must be https or http, got %q", raw, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid endpoint %q: no host", raw)
	}
	if u.Path != "" && u.Path != "/" {
		// The API path is appended by the client; a pasted browser URL would
		// otherwise produce requests to /#v1:0:18/api2/json/...
		return "", fmt.Errorf("invalid endpoint %q: drop the path, RestoreLab only needs %q", raw, u.Scheme+"://"+u.Host)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid endpoint %q: drop everything after the port, RestoreLab only needs %q", raw, u.Scheme+"://"+u.Host)
	}

	if u.Port() == "" && defaultPort > 0 {
		u.Host = fmt.Sprintf("%s:%d", u.Hostname(), defaultPort)
	}

	return u.Scheme + "://" + u.Host, nil
}

// Default API ports, filled in when the user omits them.
const (
	proxmoxPort = 8006
	pbsPort     = 8007
)
