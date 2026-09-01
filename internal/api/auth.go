package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/restorelab/restorelab/internal/store"
)

// TokenPrefix marks a RestoreLab API token.
//
// It exists so that a secret pasted into a log, a ticket or a public
// repository is recognisable as one - which is what lets it be revoked
// instead of puzzled over. GitHub's ghp_ and Stripe's sk_live_ are the same
// idea for the same reason.
const TokenPrefix = "rl_"

// tokenEntropyBytes is how much randomness a token carries.
const tokenEntropyBytes = 32

// DefaultTouchInterval is how often a token's last_used_at is written at
// most. An exact counter would mean one write per request for a field nobody
// reads to the second.
const DefaultTouchInterval = time.Minute

// NewToken mints a token. It returns the secret - the only time it will ever
// exist - and the record to store.
func NewToken(name string, now time.Time) (string, store.APIToken, error) {
	raw := make([]byte, tokenEntropyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", store.APIToken{}, fmt.Errorf("api: read random bytes: %w", err)
	}
	secret := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)

	return secret, store.APIToken{
		ID:        uuid.NewString(),
		Name:      name,
		Hash:      HashToken(secret),
		CreatedAt: now,
	}, nil
}

// HashToken returns the stored form of a secret: SHA-256, hex encoded.
//
// Not argon2id, and not as a shortcut. argon2id exists to slow down cracking
// *guessable* secrets - passwords a human chose. This token is 32 bytes from
// crypto/rand: there is nothing to guess, and brute force costs 2^256
// operations however fast the hash is. A slow hash would add no security
// here, would burn CPU on every request, and would hand any unauthenticated
// caller a trivial denial of service by forcing an argon2 computation per
// attempt. It also keeps golang.org/x/crypto out of the dependency list.
func HashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// tokenKey types the request-context key holding the authenticated token.
type tokenKey struct{}

// authed wraps a handler so that it only runs for an authenticated caller.
//
// It is requireScope(read), which is why every read route reads exactly as it
// did before scopes existed: read is implied by every token, so a token that
// only holds operate still passes here.
func (s *Server) authed(h http.HandlerFunc) http.Handler {
	return s.requireScope(store.ScopeRead, h)
}

// requireScope wraps a handler so that it only runs for a token holding the
// scope. It authenticates first: an anonymous request is 401, an
// insufficiently privileged one is 403, and confusing the two is how a
// caller ends up regenerating a token that was never the problem.
func (s *Server) requireScope(scope string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		if !tok.Can(scope) {
			writeProblem(w, r, newProblem("insufficient-scope", "This token may not do that",
				http.StatusForbidden,
				fmt.Sprintf("this endpoint needs the %q scope; create a token with `restorelab token create <name> --operate`", scope)))
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), tokenKey{}, tok)))
	})
}

// rejection is the single message every failed authentication gets.
//
// One message for every failure - absent, malformed, unknown, revoked - is
// the point: a reply that distinguished "malformed" from "unknown" would
// confirm to a guesser that the shape of their guess was right.
const rejection = "this request needs a valid API token: send `Authorization: Bearer rl_...` (create one with `restorelab token create`)"

// authenticate returns the live token the request carries, answering the
// request itself when it carries none.
//
// It returns the token rather than a bare bool because authorisation needs
// it: the scopes are on the record, and a second lookup to read them would be
// a second chance for the two answers to disagree.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*store.APIToken, bool) {
	secret, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		writeUnauthorized(w, r)
		return nil, false
	}

	hash := HashToken(secret)
	rec, err := s.tokens.TokenByHash(r.Context(), hash)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeUnauthorized(w, r)
		return nil, false
	case err != nil:
		// Not a 401: the caller's token may be perfect and our database
		// unreachable. store.ErrNoHistory maps to 503.
		writeProblem(w, r, problemFor(err))
		return nil, false
	case rec == nil:
		writeUnauthorized(w, r)
		return nil, false
	}

	// The lookup already matched on the hash; this is the belt to its
	// braces. It is constant time so that no comparison in the path leaks how
	// many bytes matched.
	if subtle.ConstantTimeCompare([]byte(rec.Hash), []byte(hash)) != 1 {
		writeUnauthorized(w, r)
		return nil, false
	}

	s.touch.record(r.Context(), s.tokens, rec.ID, s.now())
	return rec, true
}

// bearerToken extracts the credential from an Authorization header.
func bearerToken(header string) (string, bool) {
	scheme, rest, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	secret := strings.TrimSpace(rest)
	if secret == "" {
		return "", false
	}
	return secret, true
}

// touchThrottle keeps last_used_at roughly right without a write per request.
type touchThrottle struct {
	interval time.Duration

	mu   sync.Mutex
	last map[string]time.Time
}

func newTouchThrottle(interval time.Duration) *touchThrottle {
	return &touchThrottle{interval: interval, last: map[string]time.Time{}}
}

// record notes that a token was used, writing to the store at most once per
// interval.
//
// The write is best-effort, exactly like the drill journal: failing a request
// because its own bookkeeping failed would be the tail wagging the dog.
func (t *touchThrottle) record(ctx context.Context, tokens TokenStore, id string, now time.Time) {
	t.mu.Lock()
	if seen, ok := t.last[id]; ok && now.Sub(seen) < t.interval {
		t.mu.Unlock()
		return
	}
	t.last[id] = now
	t.mu.Unlock()

	_ = tokens.TouchToken(ctx, id, now)
}
