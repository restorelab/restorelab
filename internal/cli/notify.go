package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/api"
	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/crypto"
	"github.com/restorelab/restorelab/internal/diag"
	"github.com/restorelab/restorelab/internal/notify"
)

// notifyURLEnv lets deployments pass the webhook URL without putting it in
// shell history or in a process listing, exactly as tokenSecretEnv does for a
// provider token. A webhook URL is the same kind of thing: a bearer
// credential with no second factor.
const notifyURLEnv = "RESTORELAB_NOTIFY_URL"

func newNotifyCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "notify",
		Aliases: []string{"notifications"},
		Short:   "Manage where RestoreLab speaks when what a workload proves changes",
		Long: `Manages the channels RestoreLab posts to when a drill changes what a
workload proves.

RestoreLab speaks when something moved: a verdict changed in either direction,
the proof level dropped, a workload stopped being evaluable, or a workload was
drilled for the first time. It stays quiet otherwise. Twenty green messages a
night is how a channel gets muted, and how the one red message that mattered
goes unread.

    restorelab notify add ops --kind discord --url-file hook.txt
    restorelab notify test ops
    restorelab notify list`,
	}
	cmd.AddCommand(
		newNotifyAddCmd(a),
		newNotifyListCmd(a),
		newNotifyTestCmd(a),
		newNotifyRemoveCmd(a),
	)
	return cmd
}

// notifyFlags is what `notify add` was given.
type notifyFlags struct {
	kind    string
	rawURL  string
	urlFile string
}

func newNotifyAddCmd(a *app) *cobra.Command {
	f := &notifyFlags{}
	cmd := &cobra.Command{
		Use:   "add <id>",
		Short: "Add a notification channel and seal its URL",
		Long: `Adds a channel and seals its URL with the master key.

Avoid --url on a shared machine: it lands in your shell history and in the
process list. Prefer --url-file, '-' to read stdin, or the ` + notifyURLEnv + `
environment variable.

An id that already exists is replaced, which is how a rotated webhook is
installed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return a.addNotification(args[0], f)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&f.kind, "kind", "", "channel kind: "+strings.Join(notify.Kinds(), ", ")+" (required)")
	fs.StringVar(&f.rawURL, "url", "", "webhook URL (prefer --url-file or $"+notifyURLEnv+")")
	fs.StringVar(&f.urlFile, "url-file", "", "read the webhook URL from a file, or '-' for stdin")
	return cmd
}

func (a *app) addNotification(id string, f *notifyFlags) error {
	if id == "" {
		return fmt.Errorf("an id is required: it is what `notify test` and the dashboard call this channel")
	}
	// Refused before the URL is even read, because the kind is the thing
	// somebody typos and the error naming the three that exist is cheaper to
	// act on than one that arrives after a file has been opened. The registry
	// is asked rather than a second list being written here: a fourth kind
	// must not need editing in two places.
	if _, err := notify.ChannelFor(f.kind); err != nil {
		return err
	}

	target, err := a.readNotifyURL(f)
	if err != nil {
		return err
	}

	cfg, err := a.config()
	if err != nil {
		return err
	}
	key, err := a.masterKey()
	if err != nil {
		return err
	}

	cfg.UpsertNotification(config.Notification{ID: id, Kind: f.kind})
	// SetNotificationURL validates the URL and then seals it. The https rule
	// lives there on purpose, at the single door every plaintext URL comes
	// through, so nothing here re-implements it: a second copy is a second
	// thing to keep in step.
	if err := cfg.SetNotificationURL(id, target, key); err != nil {
		return err
	}

	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := config.Save(a.path(), cfg); err != nil {
		return err
	}

	fmt.Fprintf(a.out, "%s notification channel %s added (%s)\n",
		a.ok(), a.paint(colorBold, id), f.kind)
	// Nobody trusts an alerting path they have never seen fire, and a channel
	// that was configured and never tried is exactly the one that turns out
	// to be broken on the night it matters.
	fmt.Fprintf(a.out, "  send a sample message with %s\n",
		a.paint(colorCyan, "restorelab notify test "+id))
	return nil
}

// readNotifyURL resolves the webhook URL from the flags, a file, stdin or the
// environment, in that order. It mirrors readSecret, including the warning,
// because it is reading the same kind of thing.
func (a *app) readNotifyURL(f *notifyFlags) (string, error) {
	if f.rawURL != "" {
		fmt.Fprintf(a.err, "%s the webhook url was passed on the command line; it is now in your shell history\n", a.warn())
		return f.rawURL, nil
	}

	if f.urlFile != "" {
		var (
			data []byte
			err  error
		)
		if f.urlFile == "-" {
			data, err = io.ReadAll(bufio.NewReader(os.Stdin))
		} else {
			data, err = os.ReadFile(f.urlFile)
		}
		if err != nil {
			return "", fmt.Errorf("read webhook url: %w", err)
		}
		target := strings.TrimSpace(string(data))
		if target == "" {
			return "", fmt.Errorf("the webhook url file is empty")
		}
		return target, nil
	}

	if target := os.Getenv(notifyURLEnv); target != "" {
		return target, nil
	}

	return "", fmt.Errorf("no webhook url provided: use --url-file, --url, or $%s", notifyURLEnv)
}

func newNotifyListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List configured notification channels",
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			cfg, err := a.config()
			if err != nil {
				return err
			}
			if len(cfg.Notifications) == 0 {
				fmt.Fprintf(a.out, "No notification channels configured. Add one with %s\n",
					a.paint(colorCyan, "restorelab notify add --help"))
				return nil
			}
			key, err := a.masterKey()
			if err != nil {
				return err
			}

			t := a.table(a.out, "ID", "KIND", "ENABLED", "HOST")
			for _, n := range cfg.Notifications {
				enabled := "yes"
				if !n.On() {
					enabled = "no"
				}
				t.row(n.ID, n.Kind, enabled, notificationHost(n, key))
			}
			t.flush()
			return nil
		},
	}
}

// channelHost is the host of a channel's URL, and never its path.
//
// The path is the secret half of a Discord webhook URL: it carries the id and
// the token, and anyone holding it can post into that channel forever. The
// host is what an operator needs to tell two channels apart, and it is the
// most that can be shown without handing the credential to whoever is looking
// over their shoulder or reading the terminal recording afterwards.
//
// An empty answer means the URL could not be read at all. The error is not
// returned with it because both callers render a list: the channel id is
// already on the row, and a listing that turns into a paragraph per broken
// channel stops being a listing.
func channelHost(n config.Notification, key crypto.Key) string {
	target, err := n.Target(key)
	if err != nil {
		return ""
	}
	u, err := url.Parse(target)
	if err != nil {
		return ""
	}
	return u.Host
}

// notificationHost is channelHost with words a terminal can show.
func notificationHost(n config.Notification, key crypto.Key) string {
	if host := channelHost(n, key); host != "" {
		return host
	}
	return "(url unreadable)"
}

func newNotifyTestCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "test <id>",
		Short: "Send a sample message to a channel",
		Long: `Sends a sample message to a channel and reports what the far end said.

This is not a convenience. Nobody trusts an alerting path they have never seen
fire, and a channel configured six months ago that silently stopped working is
the exact failure notifications exist to prevent.

The message says it is a test, so whoever reads it at three in the morning
does not go looking for a workload that never broke.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.testNotification(cmd.Context(), args[0])
		},
	}
}

func (a *app) testNotification(ctx context.Context, id string) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	key, err := a.masterKey()
	if err != nil {
		return err
	}

	n, err := cfg.Notification(id)
	if err != nil {
		return err
	}
	channel, err := notify.ChannelFor(n.Kind)
	if err != nil {
		return err
	}
	target, err := n.Target(key)
	if err != nil {
		return err
	}

	body, err := channel.Render(sampleMessage(cfg.Server.BaseURL))
	if err != nil {
		return fmt.Errorf("rendering a sample message for channel %q: %w", id, err)
	}

	if !n.On() {
		// Sent anyway. The question this command answers is "does this path
		// work", and refusing would leave somebody unable to check a channel
		// before turning it back on.
		fmt.Fprintf(a.err, "%s channel %s is disabled: this message is going out, but real ones will not\n",
			a.warn(), id)
	}

	result := notify.NewSender(0).Post(ctx, target, body)
	if result.Err != nil {
		// result.Err carries no URL: notify.Post strips it, because this text
		// ends up in terminals, logs and support threads.
		return fmt.Errorf("channel %q refused the message: %w", id, result.Err)
	}

	fmt.Fprintf(a.out, "%s channel %s accepted the message (HTTP %d)\n",
		a.ok(), a.paint(colorBold, id), result.Status)
	return nil
}

// sampleMessage is a real transition, rendered by the real renderer, so that
// what arrives is what a genuine alert would look like.
//
// The headline says it is a test in the first words. A message that looked
// exactly like a real failure would send somebody hunting a workload that
// never broke, and that costs more than the reassurance is worth.
func sampleMessage(baseURL string) notify.Message {
	previous := notify.Story{Result: core.ResultFailed, ProofLevel: core.ProofBoot}
	link := ""
	if baseURL != "" {
		link = strings.TrimSuffix(baseURL, "/") + "/runs"
	}
	return notify.Message{
		Workload:   "example-workload",
		WorkloadID: "0",
		PlanName:   "notification test",
		RunID:      "00000000-0000-0000-0000-000000000000",
		Link:       link,
		At:         time.Now().UTC(),
		Transition: notify.Transition{
			Kind:     notify.KindVerdict,
			Current:  notify.Story{Result: core.ResultSuccess, ProofLevel: core.ProofService},
			Previous: &previous,
			Headline: "test message from `restorelab notify test`: SUCCESS, was FAILED",
		},
		RTO: 4 * time.Minute,
	}
}

func newNotifyRemoveCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "remove <id>",
		Aliases: []string{"rm"},
		Short:   "Remove a notification channel from the configuration",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := a.config()
			if err != nil {
				return err
			}
			// RemoveNotification reports an unknown id rather than succeeding
			// silently: somebody removing a channel is trying to stop messages
			// going somewhere, and a false success leaves them believing they
			// did.
			if err := cfg.RemoveNotification(args[0]); err != nil {
				return err
			}
			if err := config.Save(a.path(), cfg); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "%s notification channel %s removed\n", a.ok(), args[0])
			return nil
		},
	}
}

// cliNotifications adapts the CLI's channel configuration to what the API
// needs.
//
// It lives on this side of the interface for the reason cliProviders does:
// sealing and unsealing a webhook URL needs the master key, and internal/api
// must never learn what one is - imports_test.go fails the day it does. What
// crosses the interface is a channel with no credential on it, plus the one
// plaintext URL the test-send route has to have, for the length of that
// handler.
type cliNotifications struct {
	a *app

	// mu serialises the read-modify-write of the configuration.
	//
	// Every write here reads the whole channel list, edits it, and rewrites
	// config.yaml. HTTP handlers run one per request, so two edits arriving
	// together without this would interleave into a file missing one of them.
	// The race detector cannot run on the machine this was written on; it
	// runs in CI, and correctness here cannot wait for that either way.
	mu sync.Mutex
}

var _ api.Notifications = (*cliNotifications)(nil)

// Channels lists the configured channels, in configuration order.
//
// A configuration that cannot be read gives an empty list rather than an
// error, because the interface has no error to give: `serve` already loaded
// it once to get this far, so the only way here is a file that changed
// underneath a running process.
func (c *cliNotifications) Channels() []api.NotificationChannel {
	c.mu.Lock()
	defer c.mu.Unlock()

	cfg, err := c.a.config()
	if err != nil {
		return nil
	}
	// The key is read once for the whole listing rather than per channel: it
	// is the same key, and a failure to load it is a fact about the process,
	// not about any one channel. Without it the hosts are simply absent, and
	// the channels are still reported - they are still configured.
	key, keyErr := c.a.masterKey()

	out := make([]api.NotificationChannel, 0, len(cfg.Notifications))
	for _, n := range cfg.Notifications {
		ch := api.NotificationChannel{ID: n.ID, Kind: n.Kind, Enabled: n.On()}
		if keyErr == nil {
			ch.Host = channelHost(n, key)
		}
		out = append(out, ch)
	}
	return out
}

// configured returns the channel configuration as it stands, as a copy.
//
// It is what the dispatcher is given instead of cfg.Notifications, and it
// answers two problems with one mechanism.
//
// The first is staleness: the dispatcher calls this on every tick, so a
// channel added from the dashboard is used a minute later instead of after the
// next restart of `restorelab serve`.
//
// The second is the data race. Save and Remove mutate cfg.Notifications in
// place under c.mu, from whichever goroutine is serving an HTTP request, while
// the dispatcher reads it from its own; the slice header alone makes that a
// race the detector will report and a torn read production will not. The copy
// is the point of the method: returning the real slice under the lock and then
// reading it after the lock is released would be the same race with more
// ceremony, because the writer replaces elements of the array the header
// points at.
//
// A configuration that cannot be read gives an empty list rather than an
// error, for the reason Channels gives one: the interface has no error to
// give, and `serve` already loaded it once to get this far.
func (c *cliNotifications) configured() []config.Notification {
	c.mu.Lock()
	defer c.mu.Unlock()

	cfg, err := c.a.config()
	if err != nil {
		return nil
	}
	out := make([]config.Notification, len(cfg.Notifications))
	copy(out, cfg.Notifications)
	return out
}

// diagChannels is the same list in the shape the diagnostic is allowed to see:
// no URL, sealed or otherwise. The enabled rule is applied here, where On()
// lives, rather than restated on the far side.
func (c *cliNotifications) diagChannels() []diag.Channel {
	configured := c.configured()
	out := make([]diag.Channel, 0, len(configured))
	for _, n := range configured {
		out = append(out, diag.Channel{ID: n.ID, Kind: n.Kind, Enabled: n.On()})
	}
	return out
}

// Save creates or replaces a channel and writes the configuration.
//
// An empty target keeps the stored URL. That is the rule this method most
// needs to get right: the API never hands a URL back, so the dashboard cannot
// prefill the field, and every edit of a kind or an enabled flag arrives with
// it blank. A blank field that wiped a working webhook would take alerting
// down without anybody being told, which is the failure the whole slice
// exists to prevent, caused by the feature meant to prevent it.
func (c *cliNotifications) Save(ch api.NotificationChannel, target string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	cfg, err := c.a.config()
	if err != nil {
		return err
	}
	key, err := c.a.masterKey()
	if err != nil {
		return err
	}

	enabled := ch.Enabled
	entry := config.Notification{ID: ch.ID, Kind: ch.Kind, Enabled: &enabled}

	if target == "" {
		stored, err := storedNotification(cfg, ch.ID)
		if err != nil {
			return err
		}
		entry.URL = stored.URL
	}

	cfg.UpsertNotification(entry)
	if target != "" {
		// Through SetNotificationURL, which validates the URL and then seals
		// it. The https rule lives there, at the single door every plaintext
		// URL comes through, and nothing here re-implements it.
		if err := cfg.SetNotificationURL(ch.ID, target, key); err != nil {
			return err
		}
	}

	if err := cfg.Validate(); err != nil {
		return err
	}
	return config.Save(c.a.path(), cfg)
}

// Remove deletes a channel and writes the configuration.
func (c *cliNotifications) Remove(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	cfg, err := c.a.config()
	if err != nil {
		return err
	}
	if err := cfg.RemoveNotification(id); err != nil {
		return err
	}
	return config.Save(c.a.path(), cfg)
}

// Target returns a channel's plaintext webhook URL.
//
// It is the one place a credential crosses into the API package, for the one
// route that has to post to it. Unsealing happens here so the key does not.
func (c *cliNotifications) Target(id string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cfg, err := c.a.config()
	if err != nil {
		return "", err
	}
	key, err := c.a.masterKey()
	if err != nil {
		return "", err
	}
	n, err := cfg.Notification(id)
	if err != nil {
		return "", err
	}
	return n.Target(key)
}

// storedNotification looks a channel up and says what to do when it is
// absent.
//
// It exists so that "keep the stored url" fails with the one sentence that
// resolves it, rather than with config's lookup error, which is written for
// somebody holding a config file rather than somebody holding an HTTP client.
func storedNotification(cfg *config.Config, id string) (*config.Notification, error) {
	n, err := cfg.Notification(id)
	if err != nil {
		return nil, fmt.Errorf("channel %q does not exist yet, so there is no stored url to keep: send one", id)
	}
	return n, nil
}
