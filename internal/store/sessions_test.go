package store

import (
	"strings"
	"testing"
	"time"
)

// La jointure de session réutilise selectTokenColumns via qualify. Ce test
// est ce qui rend cette réutilisation vérifiable : si quelqu'un remplace
// l'appel par une liste écrite à la main, le préfixe disparaît et le test
// tombe.
func TestSessionJoinReusesTokenColumns(t *testing.T) {
	want := qualify("t", selectTokenColumns)
	if !strings.Contains(selectSessionByHashSQL, want) {
		t.Fatalf("the session join does not carry the qualified token columns.\n got: %s\nwant to contain: %s",
			selectSessionByHashSQL, want)
	}
	if strings.Contains(selectSessionByHashSQL, "t.id, t.name, t.hash, t.created_at") &&
		!strings.Contains(selectTokenColumns, "id, name, hash, created_at") {
		t.Fatal("the token column list changed but the join did not follow it")
	}
}

func TestQualifyPrefixesEveryColumn(t *testing.T) {
	if got := qualify("t", "a, b, c"); got != "t.a, t.b, t.c" {
		t.Fatalf("qualify = %q, want %q", got, "t.a, t.b, t.c")
	}
}

// Un User-Agent trop long est coupé sans laisser un demi-rune derrière.
func TestEncodeUserAgentTruncatesToValidUTF8(t *testing.T) {
	long := strings.Repeat("é", maxUserAgent)
	got, ok := encodeUserAgent(long).(string)
	if !ok {
		t.Fatalf("encodeUserAgent returned %T, want a string", encodeUserAgent(long))
	}
	if len(got) > maxUserAgent {
		t.Errorf("len = %d, want <= %d", len(got), maxUserAgent)
	}
	if !utf8ValidString(got) {
		t.Errorf("encodeUserAgent produced invalid UTF-8: %q", got)
	}
	if encodeUserAgent("") != nil {
		t.Error("an empty User-Agent must be stored as NULL")
	}
}

// utf8ValidString évite d'importer unicode/utf8 pour une seule ligne.
func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// Le zéro d'une durée n'a pas de sens ici : une session sans expiration
// serait une session éternelle, et rien dans le schéma ne l'empêche. Ce test
// documente que c'est l'appelant qui la fixe.
func TestSessionExpiryIsCallerSupplied(t *testing.T) {
	var s Session
	if !s.ExpiresAt.IsZero() {
		t.Fatal("the zero Session must carry no expiry")
	}
	s.ExpiresAt = time.Unix(0, 0)
	if s.ExpiresAt.IsZero() {
		t.Fatal("time.Unix(0, 0) is not the zero time")
	}
}
