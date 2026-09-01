package store

// La suite de conformité des tokens d'API. Comme le reste de la conformité,
// elle vit hors d'un _test.go pour que les deux moteurs puissent l'appeler,
// et elle est la seule défense contre une requête qui marcherait d'un côté
// seulement.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// sampleToken construit un token de test à un instant fixe.
func sampleToken(id, name, hash string) APIToken {
	return APIToken{
		ID:        id,
		Name:      name,
		Hash:      hash,
		CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
}

// TokensConformance exerce le cycle de vie complet d'un token d'API.
func TokensConformance(t *testing.T, open OpenFunc) {
	ctx := context.Background()

	t.Run("create then look up by hash", func(t *testing.T) {
		s := open(t)
		want := sampleToken("t-1", "dashboard", "aaaa0000")

		if err := s.CreateToken(ctx, want); err != nil {
			t.Fatalf("CreateToken: %v", err)
		}
		got, err := s.TokenByHash(ctx, want.Hash)
		if err != nil {
			t.Fatalf("TokenByHash: %v", err)
		}
		if got.ID != want.ID || got.Name != want.Name || got.Hash != want.Hash {
			t.Errorf("token = %+v, want %+v", got, want)
		}
		if !got.CreatedAt.Equal(want.CreatedAt) {
			t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
		}
		if !got.LastUsedAt.IsZero() || !got.RevokedAt.IsZero() {
			t.Errorf("a fresh token must have no last_used_at and no revoked_at: %+v", got)
		}
	})

	t.Run("an unknown hash is not found", func(t *testing.T) {
		s := open(t)
		if _, err := s.TokenByHash(ctx, "nothing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("TokenByHash on an unknown hash = %v, want ErrNotFound", err)
		}
	})

	t.Run("a revoked token stops being found", func(t *testing.T) {
		s := open(t)
		tok := sampleToken("t-2", "ci", "bbbb0000")
		if err := s.CreateToken(ctx, tok); err != nil {
			t.Fatalf("CreateToken: %v", err)
		}

		at := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
		if err := s.RevokeToken(ctx, tok.Name, at); err != nil {
			t.Fatalf("RevokeToken: %v", err)
		}
		if _, err := s.TokenByHash(ctx, tok.Hash); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a revoked token was still accepted: err = %v", err)
		}
		// Révoquer deux fois n'est pas une opération silencieuse : la
		// deuxième fois, il n'y a plus rien de vivant à révoquer.
		if err := s.RevokeToken(ctx, tok.Name, at); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second RevokeToken = %v, want ErrNotFound", err)
		}
	})

	t.Run("revoking an unknown name is not found", func(t *testing.T) {
		s := open(t)
		err := s.RevokeToken(ctx, "never-existed", time.Now())
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("RevokeToken on an unknown name = %v, want ErrNotFound", err)
		}
	})

	t.Run("two tokens cannot share a name", func(t *testing.T) {
		s := open(t)
		if err := s.CreateToken(ctx, sampleToken("t-3", "same", "cccc0000")); err != nil {
			t.Fatalf("CreateToken: %v", err)
		}
		if err := s.CreateToken(ctx, sampleToken("t-4", "same", "dddd0000")); err == nil {
			t.Fatal("a duplicate name was accepted: `token revoke <name>` would become ambiguous")
		}
	})

	t.Run("the listing shows revoked tokens too", func(t *testing.T) {
		s := open(t)
		live := sampleToken("t-5", "live", "eeee0000")
		dead := sampleToken("t-6", "dead", "ffff0000")
		dead.CreatedAt = live.CreatedAt.Add(time.Hour)
		for _, tok := range []APIToken{live, dead} {
			if err := s.CreateToken(ctx, tok); err != nil {
				t.Fatalf("CreateToken %s: %v", tok.Name, err)
			}
		}
		revokedAt := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
		if err := s.RevokeToken(ctx, dead.Name, revokedAt); err != nil {
			t.Fatalf("RevokeToken: %v", err)
		}

		got, err := s.ListTokens(ctx)
		if err != nil {
			t.Fatalf("ListTokens: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d tokens, want 2: a revoked token must stay visible", len(got))
		}
		if got[0].Name != "live" || got[1].Name != "dead" {
			t.Fatalf("listing order = %q, %q; want oldest first", got[0].Name, got[1].Name)
		}
		if !got[1].RevokedAt.Equal(revokedAt) {
			t.Errorf("RevokedAt = %v, want %v", got[1].RevokedAt, revokedAt)
		}
	})

	t.Run("touching a token records when it was last used", func(t *testing.T) {
		s := open(t)
		tok := sampleToken("t-7", "touched", "12340000")
		if err := s.CreateToken(ctx, tok); err != nil {
			t.Fatalf("CreateToken: %v", err)
		}

		used := time.Date(2026, 9, 4, 7, 30, 0, 0, time.UTC)
		if err := s.TouchToken(ctx, tok.ID, used); err != nil {
			t.Fatalf("TouchToken: %v", err)
		}
		got, err := s.TokenByHash(ctx, tok.Hash)
		if err != nil {
			t.Fatalf("TokenByHash: %v", err)
		}
		if !got.LastUsedAt.Equal(used) {
			t.Errorf("LastUsedAt = %v, want %v", got.LastUsedAt, used)
		}
	})
}
