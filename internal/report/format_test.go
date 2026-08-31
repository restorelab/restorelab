package report

import (
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero", 0, "0s"},
		{"sub-millisecond", 500 * time.Microsecond, "500µs"},
		{"sub-millisecond max", 999 * time.Microsecond, "999µs"},
		{"exactly one millisecond", time.Millisecond, "1ms"},
		{"milliseconds", 510 * time.Millisecond, "510ms"},
		{"milliseconds max", 999 * time.Millisecond, "999ms"},
		{"one second", time.Second, "1.0s"},
		{"sub-minute decimal", 2100 * time.Millisecond, "2.1s"},
		{"sub-minute decimal 2", 29 * time.Second, "29.0s"},
		{"exactly one minute", time.Minute, "1m00s"},
		{"minutes and seconds", 84 * time.Second, "1m24s"},
		{"minutes zero-padded seconds", 2*time.Minute + 6*time.Second, "2m06s"},
		{"minutes flat", 5 * time.Minute, "5m00s"},
		{"exactly one hour", time.Hour, "1h00m00s"},
		{"hours minutes seconds", time.Hour + 2*time.Minute + 3*time.Second, "1h02m03s"},
		{"negative sub-millisecond", -500 * time.Microsecond, "-500µs"},
		{"negative minutes", -84 * time.Second, "-1m24s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatDuration(tt.d); got != tt.want {
				t.Errorf("FormatDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		name string
		n    int64
		want string
	}{
		{"zero", 0, "0 B"},
		{"small", 512, "512 B"},
		{"just under a KiB", 1023, "1023 B"},
		{"exactly 1024", 1024, "1.0 KiB"},
		{"fractional KiB", 1536, "1.5 KiB"},
		{"exactly one MiB", 1024 * 1024, "1.0 MiB"},
		{"realistic GiB", 4_509_715_660, "4.2 GiB"},
		{"negative", -2048, "-2.0 KiB"},
		{"negative small", -512, "-512 B"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatBytes(tt.n); got != tt.want {
				t.Errorf("FormatBytes(%d) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}
