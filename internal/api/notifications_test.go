package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/store"
)

// The webhook URL every test in this file configures.
//
// It is written out whole, path included, because the path is the secret half
// of a Discord webhook URL: asserting that neither this string nor its last
// segment ever appears in a response is what the write-only rule actually
// means. A test asserting only that "rlsec:" is absent would pass against a
// handler that returned the plaintext.
const (
	testWebhookURL  = "https://discord.com/api/webhooks/1234567890/s3cr3t-path-segment"
	testWebhookHost = "discord.com"
	testWebhookPath = "s3cr3t-path-segment"
)

// --- test doubles -----------------------------------------------------------

// fakeNotifications is the channel configuration, in memory.
//
// It keeps the plaintext URL in a map beside the channel rather than on it,
// exactly as the real implementation keeps it sealed on the other side of the
// interface: nothing a handler can reach carries a credential, and a test
// asserting a URL was kept has to ask this map rather than read a response.
type fakeNotifications struct {
	channels []NotificationChannel
	targets  map[string]string

	// err, when set, is what every write answers. It drives the failure
	// paths that have nothing to do with HTTP.
	err error
}

func newFakeNotifications() *fakeNotifications {
	return &fakeNotifications{targets: map[string]string{}}
}

// seed adds a channel the way a configuration file already carrying one would.
func (f *fakeNotifications) seed(id, kind, target string) {
	f.targets[id] = target
	f.channels = append(f.channels, NotificationChannel{
		ID: id, Kind: kind, Host: fakeHost(target), Enabled: true,
	})
}

func (f *fakeNotifications) Channels() []NotificationChannel { return f.channels }

// Save mirrors what internal/cli does with the master key: a non-empty target
// replaces the stored URL, and an empty one keeps it. Getting that wrong here
// would make the handler test pass against an implementation that wipes a
// working webhook.
func (f *fakeNotifications) Save(ch NotificationChannel, target string) error {
	if f.err != nil {
		return f.err
	}
	if target != "" {
		f.targets[ch.ID] = target
	}
	ch.Host = fakeHost(f.targets[ch.ID])
	for i := range f.channels {
		if f.channels[i].ID == ch.ID {
			f.channels[i] = ch
			return nil
		}
	}
	f.channels = append(f.channels, ch)
	return nil
}

func (f *fakeNotifications) Remove(id string) error {
	if f.err != nil {
		return f.err
	}
	for i := range f.channels {
		if f.channels[i].ID == id {
			f.channels = append(f.channels[:i], f.channels[i+1:]...)
			delete(f.targets, id)
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeNotifications) Target(id string) (string, error) {
	if target, ok := f.targets[id]; ok {
		return target, nil
	}
	return "", store.ErrNotFound
}

// fakeHost is what the real implementation derives from a URL it has just
// unsealed. The host is the only part of a webhook URL that is safe to show.
func fakeHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// fakeDeliveries is the delivery half of the store, in memory.
type fakeDeliveries struct {
	last  map[string]store.Delivery
	err   error
	calls int
}

func (f *fakeDeliveries) LastDeliveries(_ context.Context, ids []string) (map[string]store.Delivery, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]store.Delivery{}
	for _, id := range ids {
		if d, ok := f.last[id]; ok {
			out[id] = d
		}
	}
	return out, nil
}

// --- helpers ----------------------------------------------------------------

// notifyServer wires a server over a channel configuration, knowing all three
// tokens: read, operate and manage. The CRUD needs manage, the test send
// needs operate, and neither implies the other.
func notifyServer(t *testing.T) (*Server, *fakeNotifications, *fakeDeliveries) {
	t.Helper()
	channels := newFakeNotifications()
	deliveries := &fakeDeliveries{last: map[string]store.Delivery{}}
	s, tokens := newTestServer(t, Options{
		Notifications: channels,
		Deliveries:    deliveries,
		Now:           func() time.Time { return fixtureNow },
	})
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	tokens.byHash[HashToken(operateSecret)] = store.APIToken{
		ID: "tok-operate", Name: "ops", Hash: HashToken(operateSecret),
		CreatedAt: created, Scopes: []string{store.ScopeOperate},
	}
	tokens.byHash[HashToken(manageSecret)] = store.APIToken{
		ID: "tok-manage", Name: "channels", Hash: HashToken(manageSecret),
		CreatedAt: created, Scopes: []string{store.ScopeManage},
	}
	return s, channels, deliveries
}

// assertNoCredential is the assertion this whole file exists for.
func assertNoCredential(t *testing.T, what, body string) {
	t.Helper()
	for _, forbidden := range []string{testWebhookURL, testWebhookPath, "rlsec:", "/api/webhooks/"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("%s leaked %q:\n%s", what, forbidden, body)
		}
	}
}

// --- the list ---------------------------------------------------------------

func TestListingChannelsNeverCarriesTheURL(t *testing.T) {
	s, channels, _ := notifyServer(t)
	channels.seed("ops-discord", "discord", testWebhookURL)

	rec := do(s, http.MethodGet, "/api/v1/notifications")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	assertNoCredential(t, "GET /notifications", rec.Body.String())

	var p page[notificationDTO]
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a page: %v", err)
	}
	if len(p.Items) != 1 {
		t.Fatalf("items = %+v, want the one configured channel", p.Items)
	}
	got := p.Items[0]
	if got.ID != "ops-discord" || got.Kind != "discord" || !got.Enabled {
		t.Errorf("channel = %+v, want the seeded one", got)
	}
	// The host is what lets an operator tell two channels apart. Without it
	// the screen shows two rows that differ only by an id somebody chose.
	if got.Host != testWebhookHost {
		t.Errorf("host = %q, want %q", got.Host, testWebhookHost)
	}
}

func TestListingChannelsCarriesTheirHealth(t *testing.T) {
	s, channels, deliveries := notifyServer(t)
	channels.seed("ops-discord", "discord", testWebhookURL)
	channels.seed("ops-slack", "slack", "https://hooks.slack.com/services/T0/B0/zzz")
	sent := fixtureNow.Add(-90 * time.Minute)
	deliveries.last["ops-discord"] = store.Delivery{
		ID: "d1", ChannelID: "ops-discord", Kind: "verdict_changed",
		State: store.DeliverySent, Status: 204, Attempts: 1, SentAt: sent,
	}
	deliveries.last["ops-slack"] = store.Delivery{
		ID: "d2", ChannelID: "ops-slack", Kind: "verdict_changed",
		State: store.DeliveryFailed, Status: 404, Attempts: 1,
		Err: "notify: refused with status 404: no_service",
	}

	rec := do(s, http.MethodGet, "/api/v1/notifications")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var p page[notificationDTO]
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a page: %v", err)
	}
	if len(p.Items) != 2 {
		t.Fatalf("items = %+v, want two channels", p.Items)
	}

	byID := map[string]notificationDTO{}
	for _, item := range p.Items {
		byID[item.ID] = item
	}
	ok := byID["ops-discord"]
	if ok.LastState != string(store.DeliverySent) || ok.LastStatus != 204 {
		t.Errorf("healthy channel = %+v, want a sent delivery answering 204", ok)
	}
	if ok.LastSent == nil || !ok.LastSent.Equal(sent) {
		t.Errorf("last_sent = %v, want %v", ok.LastSent, sent)
	}
	// The channel that stopped working is the whole reason this endpoint
	// reports health at all: a dashboard that only showed the good one would
	// leave an operator believing they are being watched.
	broken := byID["ops-slack"]
	if broken.LastState != string(store.DeliveryFailed) || broken.LastStatus != 404 {
		t.Errorf("broken channel = %+v, want a failed delivery answering 404", broken)
	}
	if !strings.Contains(broken.LastError, "404") {
		t.Errorf("last_error = %q, want the reason the far end gave", broken.LastError)
	}
	if deliveries.calls != 1 {
		t.Errorf("LastDeliveries called %d times, want one call for the whole page", deliveries.calls)
	}
}

// TestChannelHealthIsBestEffort: a history database that cannot be read is not
// a reason to refuse the list. The channels are configuration, and they are
// still configured.
func TestChannelHealthIsBestEffort(t *testing.T) {
	s, channels, deliveries := notifyServer(t)
	channels.seed("ops-discord", "discord", testWebhookURL)
	deliveries.err = store.ErrNoHistory

	rec := do(s, http.MethodGet, "/api/v1/notifications")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with no history: %s", rec.Code, rec.Body)
	}
	var p page[notificationDTO]
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a page: %v", err)
	}
	if len(p.Items) != 1 || p.Items[0].LastState != "" {
		t.Errorf("items = %+v, want the channel with no delivery reported", p.Items)
	}
}

// TestNotificationRoutesNeedAConfiguration guards the same reasoning as the
// slot routes: an empty list would read as "no channel is configured", which
// is a different statement from "this server cannot tell you".
func TestNotificationRoutesNeedAConfiguration(t *testing.T) {
	s, tokens := newTestServer(t, Options{})
	tokens.byHash[HashToken(manageSecret)] = store.APIToken{
		ID: "tok-manage", Name: "channels", Hash: HashToken(manageSecret),
		CreatedAt: fixtureNow, Scopes: []string{store.ScopeManage},
	}

	if rec := do(s, http.MethodGet, "/api/v1/notifications"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET without a configuration = %d, want 503: %s", rec.Code, rec.Body)
	}
	rec := send(s, http.MethodPost, manageSecret, "/api/v1/notifications",
		`{"id":"ops","kind":"discord","url":"`+testWebhookURL+`"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("POST without a configuration = %d, want 503: %s", rec.Code, rec.Body)
	}
}

// --- creating ---------------------------------------------------------------

func TestCreatingAChannelTakesTheURLAndNeverGivesItBack(t *testing.T) {
	s, channels, _ := notifyServer(t)

	rec := send(s, http.MethodPost, manageSecret, "/api/v1/notifications",
		`{"id":"ops-discord","kind":"discord","url":"`+testWebhookURL+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	assertNoCredential(t, "POST /notifications", rec.Body.String())

	if got := channels.targets["ops-discord"]; got != testWebhookURL {
		t.Errorf("stored url = %q, want the one that was posted", got)
	}
	var dto notificationDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("body is not a channel: %v", err)
	}
	if dto.ID != "ops-discord" || dto.Host != testWebhookHost || !dto.Enabled {
		t.Errorf("created = %+v, want the channel that was asked for, enabled", dto)
	}
	if got := rec.Header().Get("Location"); got != "/api/v1/notifications/ops-discord" {
		t.Errorf("Location = %q, want the created channel's URL", got)
	}
}

func TestCreatingAChannelRefusesAnUnknownKind(t *testing.T) {
	s, _, _ := notifyServer(t)

	rec := send(s, http.MethodPost, manageSecret, "/api/v1/notifications",
		`{"id":"ops","kind":"telegram","url":"`+testWebhookURL+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != problemContentType {
		t.Errorf("Content-Type = %q, want %q", got, problemContentType)
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a problem document: %v", err)
	}
	// Naming the kinds that exist is the difference between a refusal
	// somebody can act on and a trip to the source.
	for _, kind := range []string{"discord", "slack", "webhook"} {
		if !strings.Contains(p.Detail, kind) {
			t.Errorf("detail %q does not name %q", p.Detail, kind)
		}
	}
}

func TestCreatingAChannelRefusesAURLThatIsNotHTTPS(t *testing.T) {
	s, channels, _ := notifyServer(t)

	// A webhook URL is a bearer credential in the request line itself. The
	// rule lives in config.ValidateNotificationURL; this asserts the API
	// calls it rather than sealing whatever it was handed.
	rec := send(s, http.MethodPost, manageSecret, "/api/v1/notifications",
		`{"id":"ops","kind":"webhook","url":"http://example.com/ingest"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	if len(channels.channels) != 0 {
		t.Errorf("the channel was stored anyway: %+v", channels.channels)
	}
}

func TestCreatingAChannelRefusesAnIDThatIsTaken(t *testing.T) {
	s, channels, _ := notifyServer(t)
	channels.seed("ops-discord", "discord", testWebhookURL)

	rec := send(s, http.MethodPost, manageSecret, "/api/v1/notifications",
		`{"id":"ops-discord","kind":"slack","url":"https://hooks.slack.com/services/T0/B0/zzz"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
	}
	if channels.targets["ops-discord"] != testWebhookURL {
		t.Error("a POST onto an existing id replaced somebody else's channel")
	}
}

func TestCreatingAChannelNeedsAnIDAndAURL(t *testing.T) {
	s, _, _ := notifyServer(t)

	for _, body := range []string{
		`{"kind":"discord","url":"` + testWebhookURL + `"}`,
		`{"id":"ops","kind":"discord"}`,
		`not json at all`,
	} {
		rec := send(s, http.MethodPost, manageSecret, "/api/v1/notifications", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s = %d, want 400: %s", body, rec.Code, rec.Body)
		}
	}
}

// --- updating ---------------------------------------------------------------

// TestUpdatingWithoutAURLKeepsTheStoredOne is the trap this endpoint is most
// likely to fall into. The dashboard cannot prefill the field - the API never
// returns the URL - so every edit of anything else arrives with it empty, and
// an empty field that wipes a working webhook would break alerting silently.
func TestUpdatingWithoutAURLKeepsTheStoredOne(t *testing.T) {
	s, channels, _ := notifyServer(t)
	channels.seed("ops-discord", "discord", testWebhookURL)

	rec := send(s, http.MethodPut, manageSecret, "/api/v1/notifications/ops-discord",
		`{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	assertNoCredential(t, "PUT /notifications/{id}", rec.Body.String())

	if got := channels.targets["ops-discord"]; got != testWebhookURL {
		t.Fatalf("stored url = %q, want it untouched", got)
	}
	var dto notificationDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("body is not a channel: %v", err)
	}
	if dto.Enabled {
		t.Error("the channel is still enabled: the update did not take")
	}
	if dto.Kind != "discord" || dto.Host != testWebhookHost {
		t.Errorf("updated = %+v, want the untouched kind and host", dto)
	}
}

func TestUpdatingWithAURLReplacesIt(t *testing.T) {
	s, channels, _ := notifyServer(t)
	channels.seed("ops-discord", "discord", testWebhookURL)

	const replacement = "https://discord.com/api/webhooks/999/rotated"
	rec := send(s, http.MethodPut, manageSecret, "/api/v1/notifications/ops-discord",
		`{"url":"`+replacement+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := channels.targets["ops-discord"]; got != replacement {
		t.Errorf("stored url = %q, want the rotated one", got)
	}
	if strings.Contains(rec.Body.String(), "rotated") {
		t.Errorf("the response echoed the new url: %s", rec.Body)
	}
}

func TestUpdatingAnUnknownChannelIs404(t *testing.T) {
	s, _, _ := notifyServer(t)

	for _, c := range []struct{ method, target, body string }{
		{http.MethodPut, "/api/v1/notifications/nothing-here", `{"enabled":true}`},
		{http.MethodDelete, "/api/v1/notifications/nothing-here", ""},
		{http.MethodPost, "/api/v1/notifications/nothing-here/test", ""},
	} {
		secret := manageSecret
		if strings.HasSuffix(c.target, "/test") {
			secret = operateSecret
		}
		rec := send(s, c.method, secret, c.target, c.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404: %s", c.method, c.target, rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Content-Type"); got != problemContentType {
			t.Errorf("%s %s Content-Type = %q, want %q", c.method, c.target, got, problemContentType)
		}
	}
}

func TestDeletingAChannelStopsIt(t *testing.T) {
	s, channels, _ := notifyServer(t)
	channels.seed("ops-discord", "discord", testWebhookURL)

	rec := send(s, http.MethodDelete, manageSecret, "/api/v1/notifications/ops-discord", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body)
	}
	if len(channels.channels) != 0 {
		t.Errorf("the channel is still configured: %+v", channels.channels)
	}
}

// --- scopes -----------------------------------------------------------------

// TestReadingAChannelIsNotWritingOne: deciding what the product says out loud
// is the same kind of power as deciding what a drill is, and making it speak
// is a third. A read token holds neither.
func TestAReadTokenMayNotWriteOrTestAChannel(t *testing.T) {
	s, channels, _ := notifyServer(t)
	channels.seed("ops-discord", "discord", testWebhookURL)

	for _, c := range []struct{ method, target, body string }{
		{http.MethodPost, "/api/v1/notifications", `{"id":"x","kind":"discord","url":"` + testWebhookURL + `"}`},
		{http.MethodPut, "/api/v1/notifications/ops-discord", `{"enabled":false}`},
		{http.MethodDelete, "/api/v1/notifications/ops-discord", ""},
		{http.MethodPost, "/api/v1/notifications/ops-discord/test", ""},
	} {
		rec := send(s, c.method, testSecret, c.target, c.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with a read token = %d, want 403: %s", c.method, c.target, rec.Code, rec.Body)
		}
	}
	if len(channels.channels) != 1 || !channels.channels[0].Enabled {
		t.Errorf("a read token changed the configuration: %+v", channels.channels)
	}
}

// TestOperateMayTestButNotWrite pins the split the plan asks for: the test
// send makes the product act on the world without changing what it is.
func TestOperateMayTestButNotWrite(t *testing.T) {
	s, channels, _ := notifyServer(t)
	far := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer far.Close()
	channels.seed("ops-webhook", "webhook", far.URL)

	if rec := send(s, http.MethodPost, operateSecret, "/api/v1/notifications/ops-webhook/test", ""); rec.Code != http.StatusOK {
		t.Fatalf("test send with an operate token = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := send(s, http.MethodPut, operateSecret, "/api/v1/notifications/ops-webhook", `{"enabled":false}`); rec.Code != http.StatusForbidden {
		t.Errorf("PUT with an operate token = %d, want 403: %s", rec.Code, rec.Body)
	}
	if rec := send(s, http.MethodPost, manageSecret, "/api/v1/notifications/ops-webhook/test", ""); rec.Code != http.StatusForbidden {
		t.Errorf("test send with a manage token = %d, want 403: %s", rec.Code, rec.Body)
	}
}

// --- the test send ----------------------------------------------------------

func TestTestingAChannelPostsTheChannelsOwnPayload(t *testing.T) {
	s, channels, _ := notifyServer(t)

	var got struct {
		body        []byte
		contentType string
	}
	far := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.body, _ = io.ReadAll(r.Body)
		got.contentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer far.Close()
	channels.seed("ops-webhook", "webhook", far.URL)

	rec := send(s, http.MethodPost, operateSecret, "/api/v1/notifications/ops-webhook/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var dto notificationTestDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("body is not a test result: %v", err)
	}
	// The far end's own status, not ours. Discord answers 204 and Slack 200,
	// and an operator who has never seen this path fire needs to see which.
	if dto.Status != http.StatusNoContent || dto.ID != "ops-webhook" || dto.Kind != "webhook" {
		t.Errorf("result = %+v, want the channel and the status the far end gave", dto)
	}

	if got.contentType != "application/json" {
		t.Errorf("Content-Type sent = %q, want application/json", got.contentType)
	}
	var payload map[string]any
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("the delivered body is not JSON: %v", err)
	}
	// The generic webhook schema, rendered by the same code that renders a
	// real transition: a test that posted a hand-written body would prove the
	// path works and nothing about what arrives on it.
	if payload["schema"] != "restorelab.notification.v1" {
		t.Errorf("schema = %v, want the versioned webhook schema", payload["schema"])
	}
	if payload["kind"] != "verdict_changed" {
		t.Errorf("kind = %v, want a real transition kind", payload["kind"])
	}
	if headline, _ := payload["headline"].(string); !strings.Contains(headline, "SUCCESS") {
		t.Errorf("headline = %v, want the wording Decide produces", payload["headline"])
	}
}

// TestTestingAChannelThatIsRefusedIs502 is the classification this endpoint
// exists to get right: the far end saying no is an outage upstream of the
// caller, and a 500 would send whoever reads it looking at RestoreLab.
func TestTestingAChannelThatIsRefusedIs502(t *testing.T) {
	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusForbidden,
		http.StatusNotFound,
	} {
		s, channels, _ := notifyServer(t)
		far := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no_service", status)
		}))
		channels.seed("ops-webhook", "webhook", far.URL)

		rec := send(s, http.MethodPost, operateSecret, "/api/v1/notifications/ops-webhook/test", "")
		far.Close()

		if rec.Code != http.StatusBadGateway {
			t.Errorf("far end answered %d, we answered %d, want 502: %s", status, rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Content-Type"); got != problemContentType {
			t.Errorf("Content-Type = %q, want %q", got, problemContentType)
		}
		var p Problem
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("body is not a problem document: %v", err)
		}
		if !strings.Contains(p.Detail, "no_service") {
			t.Errorf("detail = %q, want the far end's own words", p.Detail)
		}
	}
}

// TestTestingAChannelThatCannotBeReachedHidesItsURL is the same 502, on the
// path where the credential is in real danger: net/http builds a *url.Error
// around the whole request line, so the transport error carries the webhook
// URL, path and all. Echoing it into a problem document would publish the
// credential to anyone who can reach this endpoint.
func TestTestingAChannelThatCannotBeReachedHidesItsURL(t *testing.T) {
	s, channels, _ := notifyServer(t)
	far := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed := far.URL + "/" + testWebhookPath
	far.Close()
	channels.seed("ops-webhook", "webhook", closed)

	rec := send(s, http.MethodPost, operateSecret, "/api/v1/notifications/ops-webhook/test", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body)
	}
	assertNoCredential(t, "a refused test send", rec.Body.String())
}

func TestTestingAChannelWithAnUnknownKindIs400(t *testing.T) {
	s, channels, _ := notifyServer(t)
	// A kind that reached the configuration file by hand: the API validates
	// on the way in, a text editor does not.
	channels.seed("ops-telegram", "telegram", testWebhookURL)

	rec := send(s, http.MethodPost, operateSecret, "/api/v1/notifications/ops-telegram/test", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a problem document: %v", err)
	}
	for _, kind := range []string{"discord", "slack", "webhook"} {
		if !strings.Contains(p.Detail, kind) {
			t.Errorf("detail %q does not name %q", p.Detail, kind)
		}
	}
}
