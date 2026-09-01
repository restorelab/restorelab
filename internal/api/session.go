package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/restorelab/restorelab/internal/store"
)

// SessionCookie is the name of the dashboard's session cookie.
//
// The __Host- prefix is not decoration. A browser refuses to store a cookie
// carrying it unless the cookie is Secure, is scoped to Path=/, and names no
// Domain - three properties enforced by the client itself, which no bug on
// this side can weaken and no sibling subdomain can work around.
const SessionCookie = "__Host-restorelab_session"

// SessionPrefix marks a session secret, for the reason TokenPrefix marks a
// token: a secret that lands in a log should be recognisable as one, so it
// can be revoked rather than puzzled over.
const SessionPrefix = "rls_"

// SessionLifetime is how long a session lasts from the moment it is opened.
//
// Absolute, and never extended. A sliding expiry would be more comfortable -
// nobody would be logged out mid-drill - but an open tab polling a listing
// would then hold a session forever, and this one can destroy machines.
// Twelve hours covers a working day; coming back tomorrow means logging in.
const SessionLifetime = 12 * time.Hour

// maxLoginBody caps the login body. It carries one token and nothing else.
const maxLoginBody = 4 << 10

// NewSession mints a session. It returns the secret - the only time it will
// ever exist - and the record to store.
func NewSession(tokenID, userAgent string, now time.Time) (string, store.Session, error) {
	// UTC, because the response says so out loud. The store writes UTC and
	// reads it back as UTC, so GET /session already answers in UTC; leaving
	// this one as the process's local time would make the two routes render
	// the same session with two different offsets.
	now = now.UTC()

	raw := make([]byte, tokenEntropyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", store.Session{}, fmt.Errorf("api: read random bytes: %w", err)
	}
	secret := SessionPrefix + base64.RawURLEncoding.EncodeToString(raw)

	return secret, store.Session{
		ID:        uuid.NewString(),
		Hash:      HashToken(secret),
		TokenID:   tokenID,
		CreatedAt: now,
		ExpiresAt: now.Add(SessionLifetime),
		UserAgent: userAgent,
	}, nil
}

// sessionDTO is what a client is told about its own session.
type sessionDTO struct {
	TokenName string    `json:"token_name"`
	Scopes    []string  `json:"scopes"`
	ExpiresAt time.Time `json:"expires_at"`
}

// effectiveScopes lists what the caller can actually do.
//
// read is added explicitly even though no token stores it: Can implies it for
// every token, and a UI deciding which screens to offer needs the answer it
// will actually get, not the row as it happens to be written.
func effectiveScopes(t store.APIToken) []string {
	out := []string{store.ScopeRead}
	for _, s := range t.Scopes {
		if s != store.ScopeRead {
			out = append(out, s)
		}
	}
	return out
}

// handleCreateSession exchanges a token for a session cookie.
//
// It is the one route that authenticates nothing beforehand: it is what
// creates the credential everything else checks.
//
// 200 rather than 201: a session has no URL that identifies *this* session
// rather than whoever's cookie arrives, so there is no Location to give and
// nothing a client could hand to somebody else.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if !secureTransport(r) {
		writeBadRequest(w, r,
			"the dashboard session cookie is Secure, so a browser would never send it back over plain HTTP: "+
				"put TLS in front of RestoreLab, or reach it on localhost")
		return
	}

	var body struct {
		Token string `json:"token"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxLoginBody))
	if err != nil || json.Unmarshal(raw, &body) != nil || body.Token == "" {
		writeBadRequest(w, r, `the body must be {"token":"rl_..."}`)
		return
	}

	tok, err := s.tokens.TokenByHash(r.Context(), HashToken(body.Token))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeUnauthorized(w, r)
		return
	case err != nil:
		// Not a 401: the token may be perfect and our database unreachable.
		// store.ErrNoHistory maps to 503, which is what a deployment with no
		// history database gets here.
		writeProblem(w, r, problemFor(err))
		return
	case tok == nil:
		writeUnauthorized(w, r)
		return
	}

	now := s.now()
	secret, sess, err := NewSession(tok.ID, r.UserAgent(), now)
	if err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}
	if err := s.sessions.CreateSession(r.Context(), sess, now); err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    secret,
		Path:     "/",
		Expires:  sess.ExpiresAt,
		MaxAge:   int(SessionLifetime / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, r, sessionDTO{
		TokenName: tok.Name,
		Scopes:    effectiveScopes(*tok),
		ExpiresAt: sess.ExpiresAt,
	})
}

// handleGetSession describes the session this request carries.
//
// It is what a dashboard calls on load to decide between its login screen and
// its application, and what tells it which actions to offer at all.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	if sess == nil {
		writeBadRequest(w, r,
			"this route describes the session a cookie carries; this request was authenticated with a token")
		return
	}
	tok := tokenFrom(r)
	if tok == nil {
		writeUnauthorized(w, r)
		return
	}
	writeJSON(w, r, sessionDTO{
		TokenName: tok.Name,
		Scopes:    effectiveScopes(*tok),
		ExpiresAt: sess.ExpiresAt,
	})
}

// handleDeleteSession logs out. Logging out twice is not a failure.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		if err := s.sessions.DeleteSession(r.Context(), HashToken(c.Value)); err != nil {
			writeProblem(w, r, problemFor(err))
			return
		}
	}
	// Expire the cookie whatever happened: a browser holding a cookie for a
	// row that is gone would keep sending it on every request forever.
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// secureTransport reports whether a browser will send back a Secure cookie
// issued on this request.
//
// The cookie is always Secure, so on http://192.168.1.5:8080 a browser stores
// nothing: the login appears to succeed and every request afterwards is
// anonymous, with no error anywhere to explain it. Refusing here, naming TLS,
// is the only place that confusion can be prevented.
//
// Loopback is exempt because browsers treat localhost as a trustworthy
// origin. X-Forwarded-Proto is believed: this guard exists against a
// misconfiguration, not an attacker - anyone able to forge that header is
// already speaking to this process directly, and already has the network.
func secureTransport(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	return isLoopbackHost(r.Host)
}

// isLoopbackHost reports whether a Host header names the local machine.
func isLoopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// safeMethod reports whether a method is one that does not change state.
func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// sameOriginRequest reports whether a cookie-authenticated write came from
// the page this server serves.
//
// SameSite=Strict already blocks a request made from another site. What it
// does not block is a sibling subdomain: app.example.com and
// evil.example.com are the same site to a cookie and two different origins to
// everything else. Origin is what tells them apart.
//
// The reference is the request's own Host rather than a configured origin.
// The dashboard is served by this same binary, so the legitimate origin is by
// construction the one just reached - and a value to configure is a value to
// get wrong. The deployment corollary: a reverse proxy that does not pass the
// original Host makes every dashboard write a 403.
func sameOriginRequest(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}
