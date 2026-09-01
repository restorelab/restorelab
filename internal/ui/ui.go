// Package ui carries the compiled dashboard.
//
// It exists so that internal/api never learns where its files come from: the
// HTTP package takes an fs.FS and serves it. That is the same line that keeps
// api out of the secrets path, drawn again for a different subject.
package ui

import (
	"embed"
	"io/fs"
)

// dist holds the Vite build output.
//
// The sources live in web/; the build writes here, because go:embed cannot
// reach above its own directory and embedding web/dist would mean a package
// at the module root - publicly importable, where every other package in this
// project is internal.
//
// The all: prefix earns its place twice: it is what lets a directory holding
// only .gitkeep compile at all - without it go:embed fails the build with
// "cannot embed directory dist: contains no embeddable files" on any machine
// that has never run the front-end build, because a bare pattern skips names
// starting with a dot - and it is the same rule that would drop files a
// bundler may emit with a leading dot once the build does exist.
//
//go:embed all:dist
var dist embed.FS

// FS returns the dashboard's files, rooted at the build output.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// dist is embedded at compile time. Failing here would mean the
		// binary itself is malformed, which is not a condition a caller can
		// do anything useful about.
		panic("ui: the embedded dist directory is missing: " + err.Error())
	}
	return sub
}

// Built reports whether a dashboard was compiled into this binary.
//
// It is what lets `serve` say so once at startup, instead of leaving an
// operator to discover it by getting a strange page.
func Built() bool {
	_, err := fs.Stat(FS(), "index.html")
	return err == nil
}
