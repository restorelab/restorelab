package api

import (
	"bytes"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// uiCSP is the dashboard's Content-Security-Policy.
//
// This is the header that matters most on this surface. A session cookie with
// the manage scope lives in this browser, and HttpOnly stops a script from
// *reading* it, not from using it: the defence against an injected script is
// not letting one run. Everything the page needs is same-origin, because the
// bundle is served by this binary.
//
// 'unsafe-inline' is conceded on styles alone: Radix positions its popovers
// with a style attribute, which style-src blocks without it. It opens no
// script execution.
const uiCSP = "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; " +
	"object-src 'none'; style-src 'self' 'unsafe-inline'"

// noDashboardPage is what / answers when no dashboard was compiled in.
//
// A 404 would be more correct for a machine and useless for the person who
// just typed the address into a browser. Handing them the sentence that
// unblocks them is the same instinct that produced `connect` and `doctor`.
const noDashboardPage = `<!doctype html>
<meta charset="utf-8">
<title>RestoreLab</title>
<style>body{font:16px system-ui;margin:4rem auto;max-width:40rem;padding:0 1rem}
code{background:#eee;padding:.15em .35em;border-radius:3px}</style>
<h1>The dashboard is not compiled into this binary</h1>
<p>The API is running and answering on <code>/api/v1</code>. The web interface
was not built into this build.</p>
<p>Build it from the repository with <code>make ui</code>, or use the API
directly &mdash; see <code>docs/api.md</code>.</p>
`

// handleRoot serves the dashboard, and answers as the API for API paths.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	// A path under /api/ that reached the catch-all matched no route. It must
	// answer as the API: a client that asked for JSON and got index.html back
	// gets a parse error where it expected a 404, and spends an hour on it.
	if strings.HasPrefix(r.URL.Path, "/api/") || s.ui == nil {
		s.handleUnknown(w, r)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeProblem(w, r, newProblem("method-not-allowed", "That method is not allowed here",
			http.StatusMethodNotAllowed,
			"the dashboard is served over GET; the API that accepts writes is under /api/v1"))
		return
	}

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "." {
		s.serveIndex(w, r)
		return
	}

	data, err := fs.ReadFile(s.ui, name)
	if err != nil {
		// Not a file. This is the SPA fallback: the client's router owns
		// /runs/94bce70d, and it needs the application, not a 404.
		s.serveIndex(w, r)
		return
	}

	// Vite hashes asset names, so a given URL's bytes never change. Anything
	// else - index.html above all - must not be cached, or a deploy would be
	// invisible until someone reloaded hard.
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	setUIHeaders(w)
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

// serveIndex hands back the application shell.
func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(s.ui, "index.html")
	if err != nil {
		setUIHeaders(w)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(noDashboardPage))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	setUIHeaders(w)
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(data))
}

// setUIHeaders posts the security headers every dashboard response carries.
func setUIHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", uiCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
}
