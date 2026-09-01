package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/store"
)

func TestNewTokenLooksLikeARestoreLabToken(t *testing.T) {
	secret, rec, err := NewToken("dashboard", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	if !strings.HasPrefix(secret, TokenPrefix) {
		t.Errorf("secret = %q, want the %q prefix so a leaked one is recognisable", secret, TokenPrefix)
	}
	// 32 bytes of entropy, base64url without padding: 43 characters.
	if body := strings.TrimPrefix(secret, TokenPrefix); len(body) != 43 {
		t.Errorf("secret body is %d characters, want 43 (32 random bytes)", len(body))
	}
	if rec.Name != "dashboard" || rec.ID == "" {
		t.Errorf("record = %+v, want a name and an id", rec)
	}
	if rec.Hash != HashToken(secret) {
		t.Error("the record does not carry the hash of the secret it was minted with")
	}
	if strings.Contains(rec.Hash, secret) {
		t.Fatal("the record stores the secret itself")
	}
}

func TestTwoTokensAreNeverTheSame(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		secret, _, err := NewToken("x", time.Now())
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if seen[secret] {
			t.Fatal("crypto/rand produced the same token twice: the source is not what it claims")
		}
		seen[secret] = true
	}
}

func TestAValidTokenPasses(t *testing.T) {
	s, _ := newTestServer(t, Options{})

	rec := do(s, http.MethodGet, "/api/v1/recovery-runs")

	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("a valid token was refused: %s", rec.Body)
	}
}

func TestBadTokensAreAllRejectedTheSameWay(t *testing.T) {
	s, _ := newTestServer(t, Options{})

	cases := []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"empty bearer", "Bearer "},
		{"wrong scheme", "Basic " + testSecret},
		{"unknown token", "Bearer rl_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{"no prefix", "Bearer TESTTESTTESTTESTTESTTESTTESTTESTTESTTESTTES"},
		{"truncated", "Bearer " + testSecret[:len(testSecret)-1]},
		// Differs only in the last character: if the comparison short
		// circuited on the first byte, this would still be rejected - but it
		// would be rejected *later*, and the response must be identical.
		{"differs at the end", "Bearer " + testSecret[:len(testSecret)-1] + "X"},
		// Differs at the first character of the body.
		{"differs at the start", "Bearer rl_XESTTESTTESTTESTTESTTESTTESTTESTTESTTESTTES"},
	}

	var bodies []string
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/recovery-runs", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, r)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tc.name, rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
			t.Errorf("%s: WWW-Authenticate = %q, want a Bearer challenge", tc.name, got)
		}
		bodies = append(bodies, rec.Body.String())
	}

	// Every rejection must read the same. A response that says "unknown
	// token" for one and "malformed" for another tells an attacker which
	// half of the guess was right.
	for i := 3; i < len(bodies); i++ {
		if bodies[i] != bodies[3] {
			t.Errorf("%s produced a different body from the other rejected tokens:\n%s\n%s",
				cases[i].name, bodies[i], bodies[3])
		}
	}
}

func TestARevokedTokenIsRejected(t *testing.T) {
	// The store filters revoked tokens out, so the API sees ErrNotFound.
	tokens := &fakeTokens{byHash: map[string]store.APIToken{}}
	s := New(Options{History: newFakeHistory(), Tokens: tokens, Providers: fakeProviders{}})

	rec := do(s, http.MethodGet, "/api/v1/recovery-runs")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestNoDatabaseIsOurProblemNotTheClientsToken(t *testing.T) {
	tokens := &fakeTokens{err: store.ErrNoHistory}
	s := New(Options{History: newFakeHistory(), Tokens: tokens, Providers: fakeProviders{}})

	rec := do(s, http.MethodGet, "/api/v1/recovery-runs")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: a missing database must not read as a bad token", rec.Code)
	}
}

func TestLastUsedIsWrittenAtMostOncePerInterval(t *testing.T) {
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	tokens := &fakeTokens{byHash: map[string]store.APIToken{
		HashToken(testSecret): {ID: "tok-1", Name: "test", Hash: HashToken(testSecret)},
	}}
	s := New(Options{
		History: newFakeHistory(), Tokens: tokens, Providers: fakeProviders{},
		Now: clock, TouchInterval: time.Minute,
	})

	for i := 0; i < 5; i++ {
		do(s, http.MethodGet, "/api/v1/recovery-runs")
	}
	if tokens.touched != 1 {
		t.Fatalf("wrote last_used_at %d times for 5 requests in the same minute, want 1", tokens.touched)
	}

	now = now.Add(2 * time.Minute)
	do(s, http.MethodGet, "/api/v1/recovery-runs")
	if tokens.touched != 2 {
		t.Fatalf("wrote last_used_at %d times, want 2 after the interval elapsed", tokens.touched)
	}
}

func TestBookkeepingFailureDoesNotFailTheRequest(t *testing.T) {
	tokens := &failingTouch{fakeTokens{byHash: map[string]store.APIToken{
		HashToken(testSecret): {ID: "tok-1", Hash: HashToken(testSecret)},
	}}}
	s := New(Options{History: newFakeHistory(), Tokens: tokens, Providers: fakeProviders{}})

	rec := do(s, http.MethodGet, "/api/v1/recovery-runs")

	if rec.Code == http.StatusInternalServerError {
		t.Fatal("a failed last_used_at write failed the request it was bookkeeping")
	}
}

type failingTouch struct{ fakeTokens }

func (f *failingTouch) TouchToken(context.Context, string, time.Time) error {
	return errors.New("disk full")
}
