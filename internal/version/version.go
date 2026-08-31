// Package version exposes build metadata, injected at link time.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Overridden with -ldflags "-X github.com/restorelab/restorelab/internal/version.Version=v0.1.0".
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// String renders a one-line version banner.
func String() string {
	commit := Commit
	if commit == "" {
		commit = vcsRevision()
	}
	s := "restorelab " + Version
	if commit != "" {
		if len(commit) > 12 {
			commit = commit[:12]
		}
		s += " (" + commit + ")"
	}
	if Date != "" {
		s += " built " + Date
	}
	return fmt.Sprintf("%s %s/%s %s", s, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// vcsRevision digs the commit out of the build info when it was not injected,
// which is what happens with `go run` and `go install`.
func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}
