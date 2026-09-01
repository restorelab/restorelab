// Package api serves RestoreLab's HTTP API.
//
// It reads the drill history and it queues work; it does not execute any. No
// handler in this package calls a mutating provider method, and the fake
// provider the tests run against fails the test if one is ever reached.
// Triggering and cancelling write a row that a worker picks up, and the one
// destructive endpoint - /cleanup - goes through worker.Cleanup, so the only
// package holding a mutating provider call stays the one that carries the
// guards and the tests for them.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"

	"github.com/restorelab/restorelab/internal/catalog"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

// problemBase namespaces every problem type URI. The URLs need not resolve;
// they identify the problem kind, which is what RFC 9457 asks of them.
const problemBase = "https://restorelab.dev/problems/"

// problemContentType is what RFC 9457 mandates.
const problemContentType = "application/problem+json"

// Problem is an RFC 9457 problem document.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// newProblem builds a problem, scrubbing the detail on the way in so that no
// caller has to remember to.
func newProblem(kind, title string, status int, detail string) Problem {
	return Problem{
		Type:   problemBase + kind,
		Title:  title,
		Status: status,
		Detail: scrubSecrets(detail),
	}
}

// writeProblem renders p as the response.
func writeProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	p.Instance = r.URL.Path
	p.Detail = scrubSecrets(p.Detail)

	body, err := json.Marshal(p)
	if err != nil {
		// Marshalling a struct of strings and an int cannot fail, but a
		// silent 200 with an empty body would be the worst possible way to
		// find out otherwise.
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", problemContentType)
	w.WriteHeader(p.Status)
	_, _ = w.Write(body)
}

// problemFor maps an error from RestoreLab's own storage and domain onto the
// status a client can act on. An error it does not recognise is ours, not the
// caller's: 500.
func problemFor(err error) Problem {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return newProblem("not-found", "No such recovery run", http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrAmbiguous):
		return newProblem("ambiguous-id", "Ambiguous run id", http.StatusConflict,
			"that prefix matches more than one drill: give a few more characters")
	case errors.Is(err, store.ErrNoHistory):
		return newProblem("history-unavailable", "The drill history is unavailable",
			http.StatusServiceUnavailable,
			"this RestoreLab has no usable history database; see `restorelab db status`")
	case errors.Is(err, store.ErrAlreadySettled):
		return newProblem("already-settled", "This drill is already over", http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrDuplicate):
		return newProblem("name-taken", "That name is already used by another plan",
			http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrVersionConflict):
		return newProblem("version-conflict", "The plan changed since you read it",
			http.StatusConflict,
			"reload the plan, apply your change to the current version, and send it again")
	// Last of the recognised refusals rather than first: a document that is
	// invalid is the caller's mistake, and saying so must not shadow a store
	// error that happens to travel alongside it.
	case errors.Is(err, catalog.ErrInvalid):
		return newProblem("invalid-plan", "This document is not a valid recovery plan",
			http.StatusBadRequest, err.Error())
	case errors.Is(err, core.ErrNotFound):
		return newProblem("not-found", "Not found", http.StatusNotFound, err.Error())
	case errors.Is(err, core.ErrNoBackup):
		return newProblem("no-backup", "No backup available", http.StatusNotFound, err.Error())
	default:
		return newProblem("internal", "Internal error", http.StatusInternalServerError, err.Error())
	}
}

// problemForUpstream maps an error that came from talking to a cluster.
//
// The distinction this function exists for: core.ErrUnauthorized here means
// *Proxmox* refused *our* token, which is an outage upstream of the caller.
// Answering 401 would make the caller check its own credentials, which are
// fine, and it would do that for as long as it takes to give up.
func problemForUpstream(err error) Problem {
	switch {
	case errors.Is(err, core.ErrUnauthorized):
		return newProblem("upstream-rejected", "The cluster refused RestoreLab's credentials",
			http.StatusBadGateway,
			"RestoreLab's own provider token was rejected; this is not a problem with your API token. Run `restorelab doctor`.")
	case errors.Is(err, core.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return newProblem("upstream-timeout", "The cluster did not answer in time",
			http.StatusGatewayTimeout, err.Error())
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrAmbiguous),
		errors.Is(err, store.ErrNoHistory), errors.Is(err, core.ErrNotFound),
		errors.Is(err, core.ErrNoBackup):
		return problemFor(err)
	default:
		return newProblem("upstream-failure", "The cluster could not be queried",
			http.StatusBadGateway, err.Error())
	}
}

// writeUnauthorized is the only 401 this API emits: it always means the
// caller's own bearer token.
//
// It takes no detail argument on purpose. Every failed authentication must
// answer with the same words - see the `rejection` constant - and a
// parameter here would be an invitation to say "malformed" to one caller and
// "unknown token" to another, which is exactly the distinction a guesser
// wants. Making the message unreachable from outside makes the rule
// unbreakable rather than merely documented.
func writeUnauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="restorelab"`)
	writeProblem(w, r, newProblem("unauthorized", "Authentication required",
		http.StatusUnauthorized, rejection))
}

// writeBadRequest answers a malformed query parameter.
func writeBadRequest(w http.ResponseWriter, r *http.Request, detail string) {
	writeProblem(w, r, newProblem("invalid-parameter", "Invalid parameter",
		http.StatusBadRequest, detail))
}

// secretPatterns are the shapes a secret takes in this codebase's error
// messages. This is a second line of defence, not the first: nothing is
// supposed to put a secret in an error at all.
var secretPatterns = []*regexp.Regexp{
	// A RestoreLab API token, whole.
	regexp.MustCompile(`rl_[A-Za-z0-9_-]{20,}`),
	// A sealed provider secret.
	regexp.MustCompile(`rlsec:v1:[A-Za-z0-9+/=_-]+`),
	// The password inside any URL: scheme://user:password@host.
	regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+):[^\s@]+@`),
	// key=value forms for the usual suspects.
	regexp.MustCompile(`(?i)(password|passwd|secret|token|apitoken|pveapitoken)=[^\s,;)"']+`),
}

// scrubSecrets replaces anything that looks like a credential with "***".
func scrubSecrets(s string) string {
	for i, re := range secretPatterns {
		switch i {
		case 2:
			s = re.ReplaceAllString(s, "$1:***@")
		case 3:
			s = re.ReplaceAllString(s, "***")
		default:
			s = re.ReplaceAllString(s, "***")
		}
	}
	return s
}
