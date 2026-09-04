package diag

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

// secretPath is the half of a Discord webhook URL that is the credential.
// Every test here asserts it never reaches a finding.
const secretPath = "/api/webhooks/1085551234/AbCdEfGhIjKlMnOpQrStUvWxYz"

// fakeDeliveries answers what each channel last delivered.
type fakeDeliveries struct {
	last  map[string]store.Delivery
	err   error
	asked []string
}

func (f *fakeDeliveries) LastDeliveries(_ context.Context, ids []string) (map[string]store.Delivery, error) {
	f.asked = append(f.asked, ids...)
	return f.last, f.err
}

// ch is an enabled channel, and off the same channel switched off.
//
// Enabled is spelled out rather than left to the zero value because
// diag.Channel is a plain bool: config's "absent means on" rule lives in
// config.Notification.On(), and every caller applies it before the value
// reaches this package.
func ch(id, kind string) Channel { return Channel{ID: id, Kind: kind, Enabled: true} }

func off(id, kind string) Channel { return Channel{ID: id, Kind: kind} }

// notifyReport runs the whole diagnostic against a healthy cluster, so that
// the notification findings are read exactly as doctor renders them.
func notifyReport(t *testing.T, in Input) Report {
	t.Helper()
	if in.Provider == nil {
		in.Provider = &fakeProvider{t: t, nodes: []core.Node{onlineNode("pve1")}}
	}
	in.ProviderID = "pve"
	in.Network = core.NetworkConfig{Bridge: "vmbr99", Isolated: true}
	return Run(context.Background(), in)
}

func TestAChannelThatDeliveredIsAPass(t *testing.T) {
	sent := time.Date(2026, 9, 3, 2, 14, 0, 0, time.UTC)
	r := notifyReport(t, Input{
		Notifications: []Channel{ch("ops-discord", "discord")},
		Deliveries: &fakeDeliveries{last: map[string]store.Delivery{
			"ops-discord": {
				ChannelID: "ops-discord", Kind: "verdict_changed",
				State: store.DeliverySent, Status: 204, SentAt: sent,
			},
		}},
	})

	found := findingsIn(r, AreaNotifications)
	if len(found) != 1 || found[0].Level != LevelOK {
		t.Fatalf("notification findings = %+v, want one ok", found)
	}
	for _, want := range []string{"ops-discord", "discord"} {
		if !strings.Contains(found[0].Title, want) {
			t.Errorf("title %q does not name %q", found[0].Title, want)
		}
	}
	if !strings.Contains(found[0].Detail, "2026-09-03") {
		t.Errorf("detail does not say when the channel last delivered: %q", found[0].Detail)
	}
}

// A quiet channel is the normal state of this product: it speaks only when
// what a workload proves changes. Reporting silence as a failure would train
// operators to ignore the section that carries the real one.
func TestAChannelThatNeverDeliveredIsAWarningNotAFailure(t *testing.T) {
	r := notifyReport(t, Input{
		Notifications: []Channel{ch("ops-discord", "discord")},
		Deliveries:    &fakeDeliveries{last: map[string]store.Delivery{}},
	})

	found := findingsIn(r, AreaNotifications)
	if len(found) != 1 || found[0].Level != LevelWarn {
		t.Fatalf("notification findings = %+v, want one warn", found)
	}
	if r.Problems() != 0 {
		t.Fatalf("Problems() = %d, want 0: a channel with nothing to say is not broken", r.Problems())
	}
	if !strings.Contains(found[0].Title, "ops-discord") {
		t.Errorf("title %q does not name the channel", found[0].Title)
	}
	// The only way to find out whether a channel works is to make it fire,
	// and the detail has to say so or the warning is a dead end.
	if !strings.Contains(found[0].Detail, "notify test") {
		t.Errorf("detail does not point at `restorelab notify test`: %q", found[0].Detail)
	}
}

// This is the finding the whole section exists for.
func TestAChannelWhoseLastDeliveryFailedIsAFailure(t *testing.T) {
	r := notifyReport(t, Input{
		Notifications: []Channel{ch("ops-discord", "discord")},
		Deliveries: &fakeDeliveries{last: map[string]store.Delivery{
			"ops-discord": {
				ChannelID: "ops-discord", Kind: "verdict_changed",
				State: store.DeliveryFailed, Status: 404, Attempts: 4,
				Err:       "unknown webhook",
				CreatedAt: time.Date(2026, 9, 2, 3, 0, 0, 0, time.UTC),
			},
		}},
	})

	found := findingsIn(r, AreaNotifications)
	if len(found) != 1 || found[0].Level != LevelFail {
		t.Fatalf("notification findings = %+v, want one fail", found)
	}
	if r.Problems() != 1 {
		t.Fatalf("Problems() = %d, want 1: a channel that stopped delivering is a problem", r.Problems())
	}
	text := found[0].Title + " " + found[0].Detail
	for _, want := range []string{"ops-discord", "404", "unknown webhook"} {
		if !strings.Contains(text, want) {
			t.Errorf("finding does not name %q, so nobody can act on it:\n%s", want, text)
		}
	}
}

// A transport error never reaches an HTTP status, and a finding that printed
// "HTTP 0" would send an operator looking up a status code that does not
// exist.
func TestAChannelThatWasNeverAnsweredNamesTheTransportError(t *testing.T) {
	r := notifyReport(t, Input{
		Notifications: []Channel{ch("ops-webhook", "webhook")},
		Deliveries: &fakeDeliveries{last: map[string]store.Delivery{
			"ops-webhook": {
				ChannelID: "ops-webhook", State: store.DeliveryFailed,
				Attempts: 4, Err: "dial tcp: connection refused",
			},
		}},
	})

	found := findingsIn(r, AreaNotifications)
	if len(found) != 1 || found[0].Level != LevelFail {
		t.Fatalf("notification findings = %+v, want one fail", found)
	}
	text := found[0].Title + " " + found[0].Detail
	if strings.Contains(text, "0") && strings.Contains(text, "HTTP 0") {
		t.Errorf("finding invents an HTTP status out of a transport error:\n%s", text)
	}
	if !strings.Contains(text, "connection refused") {
		t.Errorf("finding does not carry the reason:\n%s", text)
	}
}

// A delivery still being retried is neither proof the channel works nor proof
// it is broken, and either claim would be a lie the operator acts on.
func TestADeliveryStillInFlightIsAWarning(t *testing.T) {
	r := notifyReport(t, Input{
		Notifications: []Channel{ch("ops-slack", "slack")},
		Deliveries: &fakeDeliveries{last: map[string]store.Delivery{
			"ops-slack": {
				ChannelID: "ops-slack", State: store.DeliveryPending,
				Attempts: 2, Status: 503, Err: "service unavailable",
			},
		}},
	})

	found := findingsIn(r, AreaNotifications)
	if len(found) != 1 || found[0].Level != LevelWarn {
		t.Fatalf("notification findings = %+v, want one warn", found)
	}
	if r.Problems() != 0 {
		t.Fatalf("Problems() = %d, want 0: the attempts are not exhausted yet", r.Problems())
	}
}

// An operator who believes alerts are on misreads every silence as good news,
// which is the same failure as a channel that quietly stopped delivering.
func TestConfiguredChannelsWithTheDispatcherOffWarn(t *testing.T) {
	r := notifyReport(t, Input{
		Notifications:       []Channel{ch("ops-discord", "discord")},
		Deliveries:          &fakeDeliveries{last: map[string]store.Delivery{}},
		NotifyDispatcherOff: true,
	})

	found := findingsIn(r, AreaNotifications)
	if len(found) == 0 {
		t.Fatal("no notification findings at all")
	}
	if found[0].Level != LevelWarn {
		t.Fatalf("first finding = %+v, want the dispatcher warning first", found[0])
	}
	if !strings.Contains(found[0].Title+found[0].Detail, "no-notify") {
		t.Errorf("the warning does not say what turned the dispatcher off:\n%+v", found[0])
	}
	if r.Problems() != 0 {
		t.Fatalf("Problems() = %d, want 0: switching alerts off is a choice, not a fault", r.Problems())
	}
}

// A channel with enabled:false receives nothing while looking configured.
// Saying so is the same duty as reporting a dispatcher that is off.
func TestADisabledChannelIsReportedAsSilent(t *testing.T) {
	deliveries := &fakeDeliveries{last: map[string]store.Delivery{}}
	r := notifyReport(t, Input{
		Notifications: []Channel{off("old-discord", "discord")},
		Deliveries:    deliveries,
	})

	found := findingsIn(r, AreaNotifications)
	if len(found) != 1 || found[0].Level != LevelWarn {
		t.Fatalf("notification findings = %+v, want one warn", found)
	}
	if !strings.Contains(found[0].Title, "old-discord") {
		t.Errorf("title %q does not name the channel", found[0].Title)
	}
	for _, asked := range deliveries.asked {
		if asked == "old-discord" {
			t.Error("the delivery history was read for a channel that is switched off")
		}
	}
}

// Not being able to read the history is not the same as knowing a channel is
// broken, and a fail here would stop a drill over a database hiccup.
func TestAnUnreadableDeliveryHistoryWarnsOnce(t *testing.T) {
	r := notifyReport(t, Input{
		Notifications: []Channel{
			ch("ops-discord", "discord"),
			ch("ops-slack", "slack"),
		},
		Deliveries: &fakeDeliveries{err: errors.New("database is locked")},
	})

	found := findingsIn(r, AreaNotifications)
	if len(found) != 1 || found[0].Level != LevelWarn {
		t.Fatalf("notification findings = %+v, want one warn", found)
	}
	if !strings.Contains(found[0].Detail, "database is locked") {
		t.Errorf("detail does not carry the error: %q", found[0].Detail)
	}
}

func TestNoConfiguredChannelsSaysNothing(t *testing.T) {
	r := notifyReport(t, Input{})

	if found := findingsIn(r, AreaNotifications); len(found) != 0 {
		t.Fatalf("notification findings = %+v, want none: nothing was configured", found)
	}
}

// The security test this section is written around.
//
// A webhook URL is a bearer credential: whoever reads it can post into that
// channel. Finding.Detail is printed by doctor, rendered in the dashboard and
// returned by GET /api/v1/doctor, so a URL that reaches a finding has been
// handed to every reader of any of the three. The sealed form is just as
// forbidden: it is the exact string an attacker needs to feed a key.
func TestNoFindingEverCarriesTheWebhookURL(t *testing.T) {
	plain := "https://discord.com" + secretPath

	r := notifyReport(t, Input{
		// The channels themselves can no longer carry a URL at all: diag.Channel
		// has no such field, so the sealed and plaintext values below exist only
		// as the strings this test forbids in the output. That leaves one way in
		// for a credential, which is the one exercised here: the stored failure
		// reason, written by net/http and copied from the delivery row.
		Notifications: []Channel{
			ch("sealed", "discord"),
			ch("plain", "discord"),
			off("disabled", "slack"),
		},
		Deliveries: &fakeDeliveries{last: map[string]store.Delivery{
			// net/http wraps every failure in *url.Error, whose message
			// carries the whole URL. The dispatcher stores that message, so
			// this is not a hypothetical: it is what the column holds after a
			// channel goes unreachable.
			"plain": {
				ChannelID: "plain", State: store.DeliveryFailed, Attempts: 4,
				Err: `Post "` + plain + `": dial tcp 1.2.3.4:443: connect: connection refused`,
			},
			"sealed": {
				ChannelID: "sealed", State: store.DeliverySent, Status: 204,
				SentAt: time.Now().UTC(),
			},
		}},
	})

	for _, f := range r.Findings {
		text := f.Title + " " + f.Detail
		for _, forbidden := range []string{"rlsec:", secretPath, plain, "discord.com"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("finding leaks %q, which is enough to post into that channel:\n%+v",
					forbidden, f)
			}
		}
	}
	// The reason still has to survive the redaction, or the fail becomes
	// unactionable and somebody removes it.
	var sawReason bool
	for _, f := range findingsIn(r, AreaNotifications) {
		if strings.Contains(f.Detail, "connection refused") {
			sawReason = true
		}
	}
	if !sawReason {
		t.Error("redacting the URL threw away the reason the delivery failed")
	}
}
