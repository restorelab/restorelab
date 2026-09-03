package store

// La suite de conformité des sessions. Comme le reste de la conformité, elle
// vit hors d'un _test.go pour que les deux moteurs l'appellent.
//
// Le cas qui compte est « un token révoqué » : la révocation écrit
// revoked_at, elle ne supprime pas la ligne, donc ON DELETE CASCADE ne se
// déclenche pas. Une session dont le token vient d'être révoqué doit
// disparaître immédiatement, et c'est la jointure qui le garantit, pas un
// contrôle en Go que quelqu'un finirait par oublier.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// sessionClock fixe l'instant de référence des tests de session.
var sessionClock = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// sampleSession construit une session valide douze heures.
func sampleSession(id, hash, tokenID string) Session {
	return Session{
		ID:        id,
		Hash:      hash,
		TokenID:   tokenID,
		CreatedAt: sessionClock,
		ExpiresAt: sessionClock.Add(12 * time.Hour),
		UserAgent: "Mozilla/5.0 (test)",
	}
}

// SessionsConformance exerce le cycle de vie complet d'une session.
func SessionsConformance(t *testing.T, open OpenFunc) {
	ctx := context.Background()

	// withToken ouvre un store et y pose un token vivant.
	withToken := func(t *testing.T) (Store, APIToken) {
		t.Helper()
		s := open(t)
		tok := sampleToken("tok-1", "dashboard", "hash-tok-1")
		if err := s.CreateToken(ctx, tok); err != nil {
			t.Fatalf("CreateToken: %v", err)
		}
		return s, tok
	}

	t.Run("create then look up by hash", func(t *testing.T) {
		s, tok := withToken(t)
		want := sampleSession("sess-1", "hash-sess-1", tok.ID)

		if err := s.CreateSession(ctx, want, sessionClock); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}

		got, gotTok, err := s.SessionByHash(ctx, want.Hash, sessionClock)
		if err != nil {
			t.Fatalf("SessionByHash: %v", err)
		}
		if got.ID != want.ID || got.TokenID != want.TokenID || got.UserAgent != want.UserAgent {
			t.Errorf("session = %+v, want %+v", got, want)
		}
		if !got.ExpiresAt.Equal(want.ExpiresAt) {
			t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, want.ExpiresAt)
		}
		if gotTok == nil || gotTok.ID != tok.ID || gotTok.Name != tok.Name {
			t.Errorf("token = %+v, want the token the session names (%s)", gotTok, tok.ID)
		}
	})

	t.Run("the token's scopes come back with the session", func(t *testing.T) {
		s := open(t)
		tok := sampleToken("tok-scoped", "operator", "hash-tok-scoped")
		tok.Scopes = []string{ScopeOperate}
		if err := s.CreateToken(ctx, tok); err != nil {
			t.Fatalf("CreateToken: %v", err)
		}
		sess := sampleSession("sess-scoped", "hash-sess-scoped", tok.ID)
		if err := s.CreateSession(ctx, sess, sessionClock); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}

		_, gotTok, err := s.SessionByHash(ctx, sess.Hash, sessionClock)
		if err != nil {
			t.Fatalf("SessionByHash: %v", err)
		}
		if !gotTok.Can(ScopeOperate) || gotTok.Can(ScopeManage) {
			t.Errorf("scopes = %v, want operate and not manage", gotTok.Scopes)
		}
	})

	t.Run("an unknown hash is not found", func(t *testing.T) {
		s, _ := withToken(t)
		if _, _, err := s.SessionByHash(ctx, "nothing", sessionClock); !errors.Is(err, ErrNotFound) {
			t.Fatalf("SessionByHash on an unknown hash = %v, want ErrNotFound", err)
		}
	})

	t.Run("an expired session is not found", func(t *testing.T) {
		s, tok := withToken(t)
		sess := sampleSession("sess-old", "hash-sess-old", tok.ID)
		if err := s.CreateSession(ctx, sess, sessionClock); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}

		later := sess.ExpiresAt.Add(time.Second)
		if _, _, err := s.SessionByHash(ctx, sess.Hash, later); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an expired session was still accepted: err = %v", err)
		}
	})

	t.Run("revoking the token kills the session at once", func(t *testing.T) {
		s, tok := withToken(t)
		sess := sampleSession("sess-rev", "hash-sess-rev", tok.ID)
		if err := s.CreateSession(ctx, sess, sessionClock); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}

		if err := s.RevokeToken(ctx, tok.Name, sessionClock.Add(time.Minute)); err != nil {
			t.Fatalf("RevokeToken: %v", err)
		}

		// La session n'a pas expiré : seule la révocation doit la fermer. Si
		// ce test passe uniquement parce que le temps a avancé, il ne prouve
		// rien.
		if _, _, err := s.SessionByHash(ctx, sess.Hash, sessionClock.Add(2*time.Minute)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a revoked token still carried a live session: err = %v", err)
		}
	})

	t.Run("deleting a session twice is not an error", func(t *testing.T) {
		s, tok := withToken(t)
		sess := sampleSession("sess-del", "hash-sess-del", tok.ID)
		if err := s.CreateSession(ctx, sess, sessionClock); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}

		if err := s.DeleteSession(ctx, sess.Hash); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
		if err := s.DeleteSession(ctx, sess.Hash); err != nil {
			t.Fatalf("DeleteSession a second time: %v", err)
		}
		if _, _, err := s.SessionByHash(ctx, sess.Hash, sessionClock); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a deleted session was still found: err = %v", err)
		}
	})

	t.Run("creating a session sweeps the expired ones", func(t *testing.T) {
		s, tok := withToken(t)
		old := sampleSession("sess-sweep-old", "hash-sweep-old", tok.ID)
		if err := s.CreateSession(ctx, old, sessionClock); err != nil {
			t.Fatalf("CreateSession(old): %v", err)
		}

		later := old.ExpiresAt.Add(time.Hour)
		fresh := Session{
			ID: "sess-sweep-new", Hash: "hash-sweep-new", TokenID: tok.ID,
			CreatedAt: later, ExpiresAt: later.Add(12 * time.Hour),
		}
		if err := s.CreateSession(ctx, fresh, later); err != nil {
			t.Fatalf("CreateSession(fresh): %v", err)
		}

		// La ligne expirée doit avoir disparu, pas seulement être invisible :
		// on le prouve en la supprimant explicitement et en constatant que
		// le balayage l'avait déjà emportée.
		n, err := s.DeleteExpiredSessions(ctx, later)
		if err != nil {
			t.Fatalf("DeleteExpiredSessions: %v", err)
		}
		if n != 0 {
			t.Errorf("DeleteExpiredSessions removed %d row(s); CreateSession should have swept them", n)
		}
	})

	t.Run("deleting the token deletes its sessions", func(t *testing.T) {
		s, tok := withToken(t)
		sess := sampleSession("sess-cascade", "hash-sess-cascade", tok.ID)
		if err := s.CreateSession(ctx, sess, sessionClock); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}

		// Il n'y a pas de DeleteToken sur l'interface : la cascade se prouve
		// par un DELETE direct, ce que la conformité peut faire parce qu'elle
		// vit dans le paquet.
		raw, ok := s.(*sqlStore)
		if !ok {
			t.Skip("the cascade is a schema property; only the SQL store can be asked directly")
		}
		if err := raw.exec(ctx, `DELETE FROM api_tokens WHERE id = ?`, tok.ID); err != nil {
			t.Fatalf("delete the token: %v", err)
		}
		if _, _, err := s.SessionByHash(ctx, sess.Hash, sessionClock); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a session outlived the token row it names: err = %v", err)
		}
	})
}
