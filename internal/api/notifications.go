package api

// The notification channels' HTTP surface: the CRUD the dashboard's settings
// screen drives, and the one route that makes the product speak on purpose.
//
// The rule the whole file is built around: the webhook URL is write-only
// across this API. POST and PUT accept it, no response ever contains it, and
// a PUT that leaves it out keeps the stored one. See NotificationChannel,
// which is the type that makes the first two true by construction.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/notify"
)

// notificationDTO describes a configured channel.
//
// What it leaves out is the point: the URL never comes back, not truncated,
// not with its path starred out. A Discord webhook URL is a bearer credential
// and the only safe redaction of one is its absence. The host is included
// because an operator has to be able to tell two channels apart.
//
// The four last_* fields are the channel's health, read off its most recent
// delivery. They are here rather than behind a second request because the
// question "is this channel still working" is the reason somebody opens the
// screen at all, and a row that showed only what was configured would let a
// revoked webhook look exactly like a quiet one. last_state travels beside
// last_error on purpose: a pending delivery that will be retried in thirty
// seconds carries an error too, and rendering it as a dead channel would be
// the same overstatement in the other direction.
type notificationDTO struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Host    string `json:"host"`
	Enabled bool   `json:"enabled"`

	LastState  string     `json:"last_state,omitempty"`
	LastSent   *time.Time `json:"last_sent,omitempty"`
	LastStatus int        `json:"last_status,omitempty"`
	LastError  string     `json:"last_error,omitempty"`
}

// notificationTestDTO is what one deliberate message produced.
//
// Status is the far end's own answer, not ours: Discord answers 204 and Slack
// answers 200, and somebody who has never seen this path fire needs to see
// which of them replied rather than a 200 this server invented.
type notificationTestDTO struct {
	ID     string    `json:"id"`
	Kind   string    `json:"kind"`
	Status int       `json:"status"`
	SentAt time.Time `json:"sent_at"`
}

// notificationRequest is the body of POST and PUT.
//
// URL is a plain string, and an empty one means "keep what is stored" rather
// than "clear it". There is no legitimate empty webhook URL, so nothing is
// lost by refusing to distinguish an absent field from an empty one, and the
// trap this closes is the expensive one: the dashboard cannot prefill a field
// whose value the API never returns, so every edit of a name or a toggle
// arrives with it blank.
//
// Enabled is a pointer for the same reason config.Notification.Enabled is: an
// absent field must not mean "off". On a POST it means the channel is on, on
// a PUT it means "leave it as it is".
type notificationRequest struct {
	ID      string `json:"id,omitempty"`
	Kind    string `json:"kind,omitempty"`
	URL     string `json:"url,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

// The sample a test send delivers.
//
// It is worded as a test in the field a human reads first, because it lands
// in the same channel as the real thing: Discord puts sampleWorkload in the
// embed title, and somebody scrolling past must not spend a second wondering
// whether a workload just changed verdict. Everything else about it is real -
// the transition comes out of notify.Decide, so what arrives is shaped
// exactly like the message this channel will carry at three in the morning.
const (
	sampleWorkload = "restorelab notification test"
	samplePlan     = "sent by hand, no drill ran"
	sampleRunID    = "test"
	sampleRTO      = 3 * time.Minute
)

// channels returns the channel configuration, or answers the request when
// this deployment has none.
//
// The refusal is a 503 rather than an empty list, on the reasoning the slot
// routes already carry: "no channel is configured" and "this server cannot
// tell you" are different statements, and only one of them is true here.
func (s *Server) channels(w http.ResponseWriter, r *http.Request) (Notifications, bool) {
	if s.notifications == nil {
		writeProblem(w, r, newProblem("notifications-unavailable",
			"Notification channels are unavailable", http.StatusServiceUnavailable,
			"this RestoreLab was started without a configuration file to write back to; "+
				"configure channels with `restorelab notify add`"))
		return nil, false
	}
	return s.notifications, true
}

// findChannel resolves the {id} in the path, answering 404 when it names
// nothing.
//
// Every write route calls it before reading a body: a PUT onto a channel that
// does not exist is a 404 about the URL, and answering that first keeps a bad
// reference from being reported as a bad document. The catalogue's PUT reads
// in the same order for the same reason.
func (s *Server) findChannel(w http.ResponseWriter, r *http.Request) (Notifications, NotificationChannel, bool) {
	channels, ok := s.channels(w, r)
	if !ok {
		return nil, NotificationChannel{}, false
	}
	id := r.PathValue("id")
	for _, ch := range channels.Channels() {
		if ch.ID == id {
			return channels, ch, true
		}
	}
	writeProblem(w, r, newProblem("not-found", "No such notification channel",
		http.StatusNotFound, "no channel is configured with the id "+id))
	return nil, NotificationChannel{}, false
}

// handleListNotifications serves GET /api/v1/notifications.
func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	channels, ok := s.channels(w, r)
	if !ok {
		return
	}

	configured := channels.Channels()
	items := make([]notificationDTO, 0, len(configured))
	for _, ch := range configured {
		items = append(items, newNotificationDTO(ch))
	}
	s.withLastDeliveries(r.Context(), items)
	writeJSON(w, r, page[notificationDTO]{Items: items})
}

// handleCreateNotification serves POST /api/v1/notifications.
func (s *Server) handleCreateNotification(w http.ResponseWriter, r *http.Request) {
	channels, ok := s.channels(w, r)
	if !ok {
		return
	}
	req, ok := readNotification(w, r)
	if !ok {
		return
	}

	if !validNotificationID(w, r, req.ID) {
		return
	}
	for _, existing := range channels.Channels() {
		if existing.ID == req.ID {
			// Creating means creating. Replacing somebody else's channel
			// because the id collided would point their alerts at a webhook
			// they never chose, and silently.
			writeProblem(w, r, newProblem("id-taken", "That channel id is already used",
				http.StatusConflict,
				"a channel called "+req.ID+" already exists: edit it, or choose another id"))
			return
		}
	}
	if !validNotificationKind(w, r, req.Kind) {
		return
	}
	if req.URL == "" {
		writeBadRequest(w, r, "url is required: a channel with no destination would never send anything")
		return
	}
	if !validNotificationURL(w, r, req.URL) {
		return
	}

	ch := NotificationChannel{ID: req.ID, Kind: req.Kind, Enabled: true}
	if req.Enabled != nil {
		ch.Enabled = *req.Enabled
	}
	if err := channels.Save(ch, req.URL); err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}

	// Read back rather than echoing what was posted: the host is derived from
	// the URL on the far side of the interface, and a DTO built here would be
	// this handler's guess at what was stored.
	saved, ok := s.readBack(w, r, channels, ch.ID)
	if !ok {
		return
	}
	w.Header().Set("Location", "/api/v1/notifications/"+ch.ID)
	writeJSONStatus(w, r, http.StatusCreated, saved)
}

// handleUpdateNotification serves PUT /api/v1/notifications/{id}.
//
// Every field is optional and an absent one keeps what is stored. That is a
// PATCH's semantics under a PUT, and it is deliberate: the resource this URL
// names has a field the API refuses to hand back, so a client can never send
// the whole thing and a strict PUT would make an edit impossible without
// retyping the credential.
func (s *Server) handleUpdateNotification(w http.ResponseWriter, r *http.Request) {
	channels, current, ok := s.findChannel(w, r)
	if !ok {
		return
	}
	req, ok := readNotification(w, r)
	if !ok {
		return
	}

	updated := current
	if req.Kind != "" {
		if !validNotificationKind(w, r, req.Kind) {
			return
		}
		updated.Kind = req.Kind
	}
	if req.Enabled != nil {
		updated.Enabled = *req.Enabled
	}
	if req.URL != "" && !validNotificationURL(w, r, req.URL) {
		return
	}

	// req.URL empty travels through as empty, and Save keeps the stored one.
	if err := channels.Save(updated, req.URL); err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}
	saved, ok := s.readBack(w, r, channels, updated.ID)
	if !ok {
		return
	}
	writeJSON(w, r, saved)
}

// handleDeleteNotification serves DELETE /api/v1/notifications/{id}.
//
// No condition, and none needed: deliveries already written keep their
// channel id, so removing a channel stops future messages without rewriting
// what was said about past runs.
func (s *Server) handleDeleteNotification(w http.ResponseWriter, r *http.Request) {
	channels, ch, ok := s.findChannel(w, r)
	if !ok {
		return
	}
	if err := channels.Remove(ch.ID); err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTestNotification serves POST /api/v1/notifications/{id}/test.
//
// It is not a convenience. Nobody trusts an alerting path they have never
// seen fire, and a channel configured six months ago that quietly stopped
// working is the exact failure this whole slice exists to prevent.
//
// Nothing is recorded. A test message is not about a run, and the delivery
// table is keyed by (run, channel): writing a row for this would either need
// a run that does not exist or a second meaning for a column that already has
// one. What the operator needs is the answer, and they are looking at it.
func (s *Server) handleTestNotification(w http.ResponseWriter, r *http.Request) {
	channels, ch, ok := s.findChannel(w, r)
	if !ok {
		return
	}

	// A kind this product cannot render reaches here only by hand-editing the
	// configuration file, which is why it is answered here rather than
	// assumed away: the API validates on the way in, a text editor does not.
	// It is the caller's to fix, by sending a PUT, so it is a 400.
	renderer, err := notify.ChannelFor(ch.Kind)
	if err != nil {
		writeInvalidKind(w, r, ch.Kind)
		return
	}
	body, err := renderer.Render(sampleMessage(s.now()))
	if err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}

	target, err := channels.Target(ch.ID)
	if err != nil {
		// The channel exists and cannot be used: its URL was written by
		// something that bypassed the sealing, or by a different master key.
		// That is this installation's problem, not the caller's request's,
		// and it says so with the one action that fixes it.
		writeProblem(w, r, newProblem("channel-unusable",
			"This channel's webhook URL cannot be read", http.StatusInternalServerError,
			"re-add the channel so its url is sealed with the current master key: "+err.Error()))
		return
	}

	res := s.sender.Post(r.Context(), target, body)
	if res.Err != nil {
		writeProblem(w, r, refusedChannelProblem(res.Status, res.Err, target))
		return
	}
	writeJSON(w, r, notificationTestDTO{
		ID:     ch.ID,
		Kind:   ch.Kind,
		Status: res.Status,
		SentAt: s.now().UTC(),
	})
}

// refusedChannelProblem turns a failed test send into a 502.
//
// 502 and never 500: the request was fine, RestoreLab was fine, and the
// destination said no. Answering 500 would send whoever reads it looking
// through this server's logs for a fault that is at the other end of the
// wire - the same reasoning problemForUpstream applies to a cluster that
// refuses our token.
//
// The detail is checked against the target before it is sent. notify.Post
// already strips the URL out of a transport failure, and that is where the
// rule belongs; this is the second lock on the same door, because everything
// in net/http reports a failure as a *url.Error carrying the whole request
// line, the path of a webhook URL is the credential, and this text goes
// straight to whoever called the endpoint. scrubSecrets cannot help: a
// webhook URL has no password in it and no rlsec: prefix, it is a secret
// purely because of where it points.
func refusedChannelProblem(status int, err error, target string) Problem {
	detail := err.Error()
	if target != "" && strings.Contains(detail, target) {
		detail = "the channel could not be reached"
	}
	if status > 0 {
		detail = "the channel answered " + http.StatusText(status) + ": " + detail
	}
	return newProblem("channel-refused", "The channel refused the message",
		http.StatusBadGateway, detail)
}

// sampleMessage is what a test send delivers.
//
// The transition comes out of notify.Decide rather than being written here,
// so the sample is decided and worded by the same function that decides a
// real one: a hand-written headline would drift away from the product's
// wording the first time it changed. The pair below is the "failure becomes
// success" row of TestDecide, which is the transition an operator most wants
// to have seen arrive before they need it.
//
// Link is left empty. There is no run to link to, and every renderer omits an
// empty link rather than emitting a dead one.
func sampleMessage(now time.Time) notify.Message {
	current := notify.Story{Result: core.ResultSuccess, ProofLevel: core.ProofService}
	previous := &notify.Story{Result: core.ResultFailed, ProofLevel: core.ProofBoot}
	transition, _ := notify.Decide(core.RunSuccess, current, previous, false)

	return notify.Message{
		Workload:   sampleWorkload,
		WorkloadID: sampleRunID,
		PlanName:   samplePlan,
		RunID:      sampleRunID,
		At:         now.UTC(),
		Transition: transition,
		RTO:        sampleRTO,
	}
}

func newNotificationDTO(ch NotificationChannel) notificationDTO {
	return notificationDTO{ID: ch.ID, Kind: ch.Kind, Host: ch.Host, Enabled: ch.Enabled}
}

// readBack renders the channel as it now stands, after a write.
func (s *Server) readBack(w http.ResponseWriter, r *http.Request, channels Notifications, id string) (notificationDTO, bool) {
	for _, ch := range channels.Channels() {
		if ch.ID == id {
			dto := newNotificationDTO(ch)
			items := []notificationDTO{dto}
			s.withLastDeliveries(r.Context(), items)
			return items[0], true
		}
	}
	// Saved and then not there: the configuration changed underneath this
	// request, or the implementation did not store what it said it would.
	// Either way this handler cannot describe what it just wrote.
	writeProblem(w, r, newProblem("internal", "Internal error",
		http.StatusInternalServerError, "the channel was saved but could not be read back"))
	return notificationDTO{}, false
}

// withLastDeliveries fills in the health of a page of channels.
//
// Best-effort, exactly like the last-drill lookup in the workload listing: a
// configuration that can be read is still a configuration when the history
// database cannot be. The rows then arrive without their last_* keys, which
// is the same shape a channel that has never sent anything has, and that is
// the honest thing to show.
func (s *Server) withLastDeliveries(ctx context.Context, items []notificationDTO) {
	if len(items) == 0 {
		return
	}
	ids := make([]string, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	last, err := s.deliveries.LastDeliveries(ctx, ids)
	if err != nil {
		return
	}
	for i := range items {
		d, ok := last[items[i].ID]
		if !ok {
			continue
		}
		items[i].LastState = string(d.State)
		items[i].LastStatus = d.Status
		// The stored reason is the far end's own words, read off a webhook
		// somebody configured. Scrubbing is cheap and this is the only place
		// it reaches a browser.
		items[i].LastError = scrubSecrets(d.Err)
		if !d.SentAt.IsZero() {
			sent := d.SentAt
			items[i].LastSent = &sent
		}
	}
}

// readNotification reads a channel description from a request body, capped by
// the same maxRequestBody every other write uses.
func readNotification(w http.ResponseWriter, r *http.Request) (notificationRequest, bool) {
	var req notificationRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody)).Decode(&req); err != nil {
		writeBadRequest(w, r, "the body is not the JSON this endpoint expects")
		return notificationRequest{}, false
	}
	return req, true
}

// validNotificationID refuses an id that would not survive being put in a URL.
//
// The id is the last segment of every other route on this resource, and one
// carrying a slash would make the Location header of a 201 point at something
// that does not resolve. Refusing it beats inventing an escaping rule the CLI
// would then have to agree with.
func validNotificationID(w http.ResponseWriter, r *http.Request, id string) bool {
	if id == "" {
		writeBadRequest(w, r, "id is required: it is how this channel is named everywhere else")
		return false
	}
	if strings.ContainsAny(id, "/ \t") {
		writeBadRequest(w, r, "id must not contain a slash or a space: it is a path segment")
		return false
	}
	return true
}

func validNotificationKind(w http.ResponseWriter, r *http.Request, kind string) bool {
	if _, err := notify.ChannelFor(kind); err != nil {
		writeInvalidKind(w, r, kind)
		return false
	}
	return true
}

// writeInvalidKind names every kind that exists.
//
// The caller is an operator who typed one word wrong, and a refusal that only
// said "invalid" would send them to the source to find out what is not.
func writeInvalidKind(w http.ResponseWriter, r *http.Request, kind string) {
	writeProblem(w, r, newProblem("invalid-channel-kind",
		"That is not a channel RestoreLab can render for", http.StatusBadRequest,
		fmt.Sprintf("kind %q is not supported: use one of %s", kind, strings.Join(notify.Kinds(), ", "))))
}

// validNotificationURL applies the rule config already owns.
//
// config.ValidateNotificationURL is exported precisely so that the CLI and
// this package share one definition of an acceptable webhook URL rather than
// two that drift. Calling it here buys a 400 with a sentence somebody can act
// on; Config.SetNotificationURL calls it again on the far side, which is the
// check that cannot be forgotten.
func validNotificationURL(w http.ResponseWriter, r *http.Request, raw string) bool {
	if err := config.ValidateNotificationURL(raw); err != nil {
		// err names the scheme and never the URL, which is what makes it
		// safe to echo. See config.ValidateNotificationURL.
		writeProblem(w, r, newProblem("invalid-channel-url",
			"That webhook URL cannot be used", http.StatusBadRequest, err.Error()))
		return false
	}
	return true
}
