package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
)

// ANSI colours. Kept as raw escapes rather than pulling in a colour library:
// the palette is five entries and this is the whole of it.
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorDim    = "\033[2m"
	colorBold   = "\033[1m"
)

func (a *app) paint(color, s string) string {
	if a.noColor || color == "" {
		return s
	}
	return color + s + colorReset
}

// isTerminal reports whether f is a character device, which is a good enough
// proxy for "a human is looking at this" and needs no dependency.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// table renders aligned columns, the way every list command in this CLI does.
type table struct {
	w      *tabwriter.Writer
	app    *app
	header []string
}

func (a *app) table(out io.Writer, header ...string) *table {
	t := &table{
		w:      tabwriter.NewWriter(out, 0, 0, 3, ' ', 0),
		app:    a,
		header: header,
	}
	if len(header) > 0 {
		fmt.Fprintln(t.w, a.paint(colorDim, strings.Join(header, "\t")))
	}
	return t
}

func (t *table) row(cells ...string) {
	fmt.Fprintln(t.w, strings.Join(cells, "\t"))
}

func (t *table) flush() { _ = t.w.Flush() }

// ok/fail/warn are the status glyphs used across the CLI. The ASCII fallback
// matters on Windows consoles that are not in UTF-8 mode.
func (a *app) ok() string   { return a.paint(colorGreen, glyph("✓", "[OK]")) }
func (a *app) fail() string { return a.paint(colorRed, glyph("✗", "[!!]")) }
func (a *app) warn() string { return a.paint(colorYellow, glyph("!", "[ !]")) }

func glyph(unicode, ascii string) string {
	if asciiOnly() {
		return ascii
	}
	return unicode
}

// asciiOnly is true when the terminal is unlikely to render box glyphs.
func asciiOnly() bool {
	if os.Getenv("RESTORELAB_ASCII") != "" {
		return true
	}
	// Windows Terminal, VS Code and modern PowerShell set these; a bare
	// cmd.exe sets neither and is the case worth protecting.
	if os.Getenv("WT_SESSION") != "" || os.Getenv("TERM_PROGRAM") != "" || os.Getenv("TERM") != "" {
		return false
	}
	return os.Getenv("OS") == "Windows_NT"
}

// humanBytes formats a byte count for humans (binary units, as storage tools do).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
