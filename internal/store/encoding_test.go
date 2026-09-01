package store

import (
	"sort"
	"testing"
	"time"
)

// The whole point of a fixed-width layout: ORDER BY started_at DESC is a
// lexicographic sort in both engines, so the text form must sort the same way
// the instants do. time.RFC3339Nano trims trailing zeros, which breaks this.
func TestFormatTimeSortsLexicographically(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	instants := []time.Time{
		base.Add(2 * time.Second),
		base.Add(time.Nanosecond),
		base,
		base.Add(time.Millisecond),
		base.Add(999999999 * time.Nanosecond),
	}

	encoded := make([]string, len(instants))
	for i, tm := range instants {
		encoded[i] = formatTime(tm)
	}
	sort.Strings(encoded)

	chronological := append([]time.Time(nil), instants...)
	sort.Slice(chronological, func(i, j int) bool { return chronological[i].Before(chronological[j]) })

	for i := range encoded {
		if want := formatTime(chronological[i]); encoded[i] != want {
			t.Fatalf("position %d: lexicographic order gives %q, chronological order gives %q",
				i, encoded[i], want)
		}
	}
}

func TestFormatTimeIsFixedWidth(t *testing.T) {
	widths := map[int]bool{}
	for _, tm := range []time.Time{
		time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 1, 12, 0, 0, 1, time.UTC),
		time.Date(2026, 9, 1, 12, 0, 0, 123456789, time.UTC),
		time.Date(2026, 12, 31, 23, 59, 59, 999999999, time.UTC),
	} {
		widths[len(formatTime(tm))] = true
	}
	if len(widths) != 1 {
		t.Fatalf("formatTime produced %d different widths, want 1: %v", len(widths), widths)
	}
}

// A local-time instant must come back as the same instant, in UTC. Mixed
// zones in one column make ORDER BY meaningless.
func TestFormatTimeForcesUTC(t *testing.T) {
	zone := time.FixedZone("CEST", 2*60*60)
	local := time.Date(2026, 9, 1, 14, 30, 0, 0, zone)

	got, err := parseTime(formatTime(local))
	if err != nil {
		t.Fatalf("parseTime: %v", err)
	}
	if !got.Equal(local) {
		t.Errorf("round trip gave %v, want the same instant as %v", got, local)
	}
	if got.Location() != time.UTC {
		t.Errorf("Location = %v, want UTC", got.Location())
	}
}

func TestFormatTimeKeepsNanoseconds(t *testing.T) {
	want := time.Date(2026, 9, 1, 10, 0, 0, 123456789, time.UTC)
	got, err := parseTime(formatTime(want))
	if err != nil {
		t.Fatalf("parseTime: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("round trip gave %v, want %v", got, want)
	}
}

func TestNullTimeRoundTrip(t *testing.T) {
	if v := formatNullTime(time.Time{}); v != nil {
		t.Errorf("a zero time must encode as NULL, got %v", v)
	}

	instant := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	encoded, ok := formatNullTime(instant).(string)
	if !ok {
		t.Fatalf("a real instant must encode as a string, got %T", formatNullTime(instant))
	}
	decoded, err := parseNullTime(&encoded)
	if err != nil {
		t.Fatalf("parseNullTime: %v", err)
	}
	if !decoded.Equal(instant) {
		t.Errorf("round trip gave %v, want %v", decoded, instant)
	}

	got, err := parseNullTime(nil)
	if err != nil {
		t.Fatalf("parseNullTime(nil): %v", err)
	}
	if !got.IsZero() {
		t.Errorf("NULL must decode as the zero time, got %v", got)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	in := map[string]any{"exit_code": float64(0), "stdout": "active"}
	encoded, err := encodeJSON(in)
	if err != nil {
		t.Fatalf("encodeJSON: %v", err)
	}
	var out map[string]any
	if err := decodeJSON([]byte(encoded), &out); err != nil {
		t.Fatalf("decodeJSON: %v", err)
	}
	if out["stdout"] != "active" || out["exit_code"] != float64(0) {
		t.Fatalf("round trip gave %v, want %v", out, in)
	}
}

func TestEncodeJSONOfNilIsEmpty(t *testing.T) {
	got, err := encodeJSON(nil)
	if err != nil {
		t.Fatalf("encodeJSON(nil): %v", err)
	}
	if got != "" {
		t.Fatalf("encodeJSON(nil) = %q, want the empty string so it lands as SQL NULL", got)
	}
}

func TestDecodeJSONToleratesEmpty(t *testing.T) {
	var out map[string]any
	if err := decodeJSON(nil, &out); err != nil {
		t.Fatalf("decodeJSON(nil): %v, want nil", err)
	}
	if out != nil {
		t.Fatalf("decodeJSON(nil) set %v, want it left alone", out)
	}
}

func TestBoolMapping(t *testing.T) {
	if boolToInt(true) != 1 || boolToInt(false) != 0 {
		t.Fatal("boolToInt must map to 1 and 0")
	}
	if !intToBool(1) || intToBool(0) {
		t.Fatal("intToBool must map 1 to true and 0 to false")
	}
}

func TestNowUTCIsUTC(t *testing.T) {
	if nowUTC().Location() != time.UTC {
		t.Fatal("nowUTC must return a UTC instant")
	}
}

// A nil pointer wrapped in an interface is not == nil in Go, so encodeJSON
// has to catch it after marshalling too. Without this, a run with no backup
// stored "null" and read back as an empty Backup instead of as no backup.
func TestEncodeJSONOfATypedNilPointerIsEmpty(t *testing.T) {
	type payload struct{ Field string }
	var p *payload

	got, err := encodeJSON(p)
	if err != nil {
		t.Fatalf("encodeJSON: %v", err)
	}
	if got != "" {
		t.Fatalf("encodeJSON(typed nil) = %q, want the empty string so it lands as SQL NULL", got)
	}
}
