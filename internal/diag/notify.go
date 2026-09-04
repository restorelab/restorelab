package diag

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/restorelab/restorelab/internal/store"
)

// Channel is a notification channel as this package is allowed to see one.
//
// It exists so that no URL, sealed or not, can reach here at all. This package
// used to take config.Notification, which carries the sealed webhook URL, and
// the only thing keeping that value out of a finding was a comment saying not
// to read it. A finding is printed by `restorelab doctor`, rendered in the
// dashboard and returned by GET /api/v1/doctor, so the cost of somebody one
// day formatting the wrong field is a bearer credential published three ways.
// A type with no URL field cannot make that mistake, and structure beats
// discipline for a rule that has to hold forever.
//
// Enabled is a plain bool rather than config's *bool: the caller already knows
// what an absent value means (On() says enabled), and repeating that rule here
// would be a second copy of it to keep in step.
type Channel struct {
	ID      string
	Kind    string
	Enabled bool
}

// DeliveryHistory is what the diagnostic needs from the drill history to say
// whether a channel still works.
//
// It is a one-method interface rather than store.Store for the reason
// storageInspector is one: this package must be callable with the small part
// of a subsystem it actually reads, and a diagnostic that held a whole Store
// could write to it. The CLI and the API both pass their real store; a caller
// with no history passes nothing and gets a section that says only what is
// configured.
type DeliveryHistory interface {
	LastDeliveries(ctx context.Context, channelIDs []string) (map[string]store.Delivery, error)
}

// bearerURL matches anything shaped like a URL, and sealedSecret anything
// shaped like a sealed value.
//
// Both exist because of one column: the dispatcher records why an attempt
// failed, and net/http wraps every failure in *url.Error, whose message
// carries the whole request URL. A Discord webhook URL is a bearer credential
// with its authorisation in its path, so copying that message into a finding
// would publish the credential to doctor's output, to the dashboard and to
// GET /api/v1/doctor at once.
//
// Truncating the URL instead of removing it was considered and rejected: for
// a webhook the path is the secret, so keeping any of it redacts nothing. The
// only safe redaction of a bearer URL is its absence, which is the same
// conclusion config.Notification.Redacted reached.
var (
	bearerURL    = regexp.MustCompile(`(?i)[a-z][a-z0-9+.\-]*://[^\s"'<>]*`)
	sealedSecret = regexp.MustCompile(`rlsec:v1:[A-Za-z0-9+/=_-]+`)
)

// redactSecrets removes credentials from text this package did not write.
//
// It is applied to the stored failure reason and to nothing else: the rest of
// a notification finding is composed here from a channel id and a kind, and
// scrubbing our own sentences would be a way to stop noticing that one of
// them started carrying something it should not.
func redactSecrets(s string) string {
	s = bearerURL.ReplaceAllString(s, "***")
	return sealedSecret.ReplaceAllString(s, "***")
}

// appendNotifications reports whether the alerting path still works.
//
// The finding this whole section exists for is the failing one. A channel
// configured six months ago whose webhook was revoked keeps an operator
// believing they are being watched, and nothing else in the product would
// ever say otherwise: the drills keep passing, the dashboard keeps filling,
// and the silence reads as good news. That is the failure C5 was built
// against, one level up.
//
// No finding here ever contains the channel's URL, in any form. See
// redactSecrets.
func (r *Report) appendNotifications(ctx context.Context, in Input) {
	if len(in.Notifications) == 0 {
		return
	}

	if in.NotifyDispatcherOff {
		// Said before the per-channel findings, because it changes what they
		// mean: an operator who thinks alerts are on reads every silence as
		// good news, and a channel that delivered perfectly last week is
		// still going to say nothing tonight.
		r.warn(AreaNotifications, "notifications are configured but the dispatcher is off",
			fmt.Sprintf("%d channel(s) are configured and none of them will receive anything while "+
				"the server runs with --no-notify: what a workload proves can change tonight and "+
				"nobody will be told", len(in.Notifications)))
	}

	var enabled []string
	for _, n := range in.Notifications {
		if n.Enabled {
			enabled = append(enabled, n.ID)
		}
	}

	last := map[string]store.Delivery{}
	if in.Deliveries != nil && len(enabled) > 0 {
		got, err := in.Deliveries.LastDeliveries(ctx, enabled)
		if err != nil {
			// A warning, not a failure, and the section stops here. "I could
			// not check" is not "it is broken", and inventing a per-channel
			// state out of a map we never received would be worse than
			// saying nothing about them.
			r.warn(AreaNotifications, "cannot read what the notification channels last delivered",
				fmt.Sprintf("%d configured channel(s) were not checked: %s",
					len(in.Notifications), redactSecrets(err.Error())))
			return
		}
		last = got
	}

	for _, n := range in.Notifications {
		r.appendChannel(n, last[n.ID], in.Deliveries != nil)
	}
}

// appendChannel reports on one configured channel. delivered says whether the
// history was readable at all, so that "nothing came back" is not confused
// with "we never asked".
func (r *Report) appendChannel(n Channel, last store.Delivery, checked bool) {
	name := fmt.Sprintf("notification channel %q (%s)", n.ID, n.Kind)

	if !n.Enabled {
		// A disabled channel looks configured in every listing and receives
		// nothing, which is the same trap as a dispatcher that is off, one
		// channel at a time.
		r.warn(AreaNotifications, name+" is switched off",
			"it is configured but disabled, so it receives nothing; `restorelab notify` can enable it again")
		return
	}

	if !checked {
		r.ok(AreaNotifications, name+" is configured",
			"the drill history was not available here, so what this channel has actually delivered was not checked")
		return
	}

	switch {
	case last.ChannelID == "":
		// Silence is this product's normal state: it speaks only when what a
		// workload proves changes. A failure here would be crying wolf on
		// every fresh installation, and a section that cries wolf is one
		// nobody reads on the day it is right.
		r.warn(AreaNotifications, name+" has never delivered a message",
			"that is not necessarily a fault: RestoreLab speaks only when what a workload proves "+
				"changes, so a correctly configured channel can stay quiet for weeks. It also means "+
				"nothing has ever proven this channel works. `restorelab notify test "+n.ID+
				"` posts a sample message, which is the only way to find out before it matters")

	case last.State == store.DeliveryFailed:
		r.fail(AreaNotifications, name+" is not delivering",
			fmt.Sprintf("the last message (%s) was given up on after %d attempt(s)%s%s. "+
				"Anything this channel should have announced since then went unsaid",
				deliveryKind(last), last.Attempts, deliveryStatus(last), deliveryReason(last)))

	case last.State == store.DeliveryPending:
		// Neither a pass nor a failure: the attempts are not exhausted, so
		// claiming either would be a statement the operator acts on and one
		// of the two would be wrong.
		r.warn(AreaNotifications, name+" has a message still in flight",
			fmt.Sprintf("the last message (%s) has been attempted %d time(s) and is still being "+
				"retried%s%s; run doctor again in a few minutes to see how it ended",
				deliveryKind(last), last.Attempts, deliveryStatus(last), deliveryReason(last)))

	default:
		r.ok(AreaNotifications, name+" is delivering",
			fmt.Sprintf("last message (%s) accepted on %s%s",
				deliveryKind(last), deliveryTime(last).UTC().Format(time.RFC3339),
				deliveryStatus(last)))
	}
}

// deliveryKind names the transition a message was about, for a reader who is
// trying to remember what they last received.
func deliveryKind(d store.Delivery) string {
	if d.Kind == "" {
		return "unknown transition"
	}
	return d.Kind
}

// deliveryTime is when the delivery is best dated: when it was accepted, or
// failing that when it was written. A settled row that carries no sent_at
// would otherwise print the zero time, which reads as 1970 and sends the
// reader looking for a clock problem.
func deliveryTime(d store.Delivery) time.Time {
	if !d.SentAt.IsZero() {
		return d.SentAt
	}
	return d.CreatedAt
}

// deliveryStatus renders the HTTP status, and nothing at all when there was
// none. A transport error never reached a status, and printing "HTTP 0" would
// send an operator looking up a code that does not exist.
func deliveryStatus(d store.Delivery) string {
	if d.Status == 0 {
		return ""
	}
	return fmt.Sprintf(", HTTP %d", d.Status)
}

// deliveryReason renders why the attempt failed, with any credential removed.
func deliveryReason(d store.Delivery) string {
	if d.Err == "" {
		return ""
	}
	return ": " + redactSecrets(d.Err)
}
