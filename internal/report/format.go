// Package report renders a completed core.RecoveryRun as a terminal report,
// a stable JSON document, a self-contained HTML page, and computes the
// Recovery Confidence product indicator.
package report

import (
	"fmt"
	"time"
)

// FormatDuration renders d compactly and readably for humans:
//
//	0                       -> "0s"
//	312 * time.Microsecond  -> "312µs"
//	510 * time.Millisecond  -> "510ms"
//	2100 * time.Millisecond -> "2.1s"
//	84 * time.Second        -> "1m24s"
//	1h2m3s                  -> "1h02m03s"
//
// Negative durations are rendered with a leading "-" and otherwise formatted
// as their absolute value.
func FormatDuration(d time.Duration) string {
	neg := ""
	if d < 0 {
		neg = "-"
		d = -d
	}

	switch {
	case d == 0:
		return "0s"
	case d < time.Millisecond:
		return fmt.Sprintf("%s%dµs", neg, d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%s%dms", neg, d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%s%.1fs", neg, d.Seconds())
	case d < time.Hour:
		m := d / time.Minute
		s := (d % time.Minute) / time.Second
		return fmt.Sprintf("%s%dm%02ds", neg, m, s)
	default:
		h := d / time.Hour
		m := (d % time.Hour) / time.Minute
		s := (d % time.Minute) / time.Second
		return fmt.Sprintf("%s%dh%02dm%02ds", neg, h, m, s)
	}
}

// FormatBytes renders n as a human-readable size using binary (1024-based)
// units: B, KiB, MiB, GiB, TiB, PiB, EiB. Values below 1 KiB are shown as a
// whole number of bytes; larger values are shown with one decimal place.
// Negative values are rendered with a leading "-".
func FormatBytes(n int64) string {
	neg := ""
	if n < 0 {
		neg = "-"
		n = -n
	}

	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%s%d B", neg, n)
	}

	div, exp := int64(unit), 0
	for n/div >= unit && exp < 5 {
		div *= unit
		exp++
	}

	units := [...]string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	return fmt.Sprintf("%s%.1f %s", neg, float64(n)/float64(div), units[exp])
}
