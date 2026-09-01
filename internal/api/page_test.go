package api

import (
	"net/url"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/store"
)

func TestCursorRoundTrips(t *testing.T) {
	want := store.Position{
		StartedAt: time.Date(2026, 9, 1, 10, 30, 0, 123456789, time.UTC),
		ID:        "0aca8405-4e80-4ac9-8bdd-057a56dc0281",
	}

	got, err := decodeCursor(encodeCursor(want))
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt = %v, want %v: the nanoseconds must survive", got.StartedAt, want.StartedAt)
	}
	if got.ID != want.ID {
		t.Errorf("ID = %q, want %q", got.ID, want.ID)
	}
}

func TestCursorIsOpaqueButUrlSafe(t *testing.T) {
	c := encodeCursor(store.Position{StartedAt: time.Now(), ID: "abc"})

	if url.QueryEscape(c) != c {
		t.Errorf("cursor %q needs escaping in a query string; it must be base64url", c)
	}
}

func TestGarbageCursorsAreRefused(t *testing.T) {
	for _, bad := range []string{"", "!!!", "YWJj", "bm90LWEtdGltZXxhYmM"} {
		if _, err := decodeCursor(bad); err == nil {
			t.Errorf("decodeCursor(%q) accepted garbage", bad)
		}
	}
}

func TestParseLimit(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"", store.DefaultListLimit, false},
		{"10", 10, false},
		{"0", 0, true},
		{"-1", 0, true},
		{"abc", 0, true},
		{"1000", maxPageSize, false}, // capped, not refused
	}
	for _, tc := range cases {
		got, err := parseLimit(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseLimit(%q) = %d, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseLimit(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("parseLimit(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		in   string
		want time.Time
	}{
		{"30d", now.AddDate(0, 0, -30)},
		{"12h", now.Add(-12 * time.Hour)},
		{"2026-08-01", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-08-01T09:00:00Z", time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		got, err := parseSince(tc.in, now)
		if err != nil {
			t.Fatalf("parseSince(%q): %v", tc.in, err)
		}
		if !got.Equal(tc.want) {
			t.Errorf("parseSince(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if _, err := parseSince("last tuesday", now); err == nil {
		t.Error("parseSince accepted something it cannot mean: silently listing the wrong window is worse than an error")
	}
}
