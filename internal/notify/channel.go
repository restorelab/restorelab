package notify

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Message is everything a renderer is allowed to know.
//
// What it leaves out is the point. The channel's URL is a bearer credential
// and it is not here, so no renderer can put it in a body that ends up posted
// into the chat channel that URL opens. TestRenderMessageCarriesNoCredential
// is what keeps that true past the next convenient addition.
//
// Link is empty whenever server.base_url is unset, which is the common case.
// Every renderer therefore has to omit its link rather than emit an empty
// one: a dead button is worse than no button.
type Message struct {
	Workload   string
	WorkloadID string
	PlanName   string
	RunID      string
	Link       string
	At         time.Time
	Transition Transition
	RTO        time.Duration
}

// Channel renders a Message into the body one destination expects.
//
// Rendering and sending are split because they fail differently and are
// retried differently: a body that will not render is a bug here, while a
// body that will not post is a fact about the far end. Keeping them apart is
// also what lets the stored payload be rendered exactly once per delivery
// (see store.Delivery) and replayed byte for byte on every retry.
type Channel interface {
	Kind() string
	Render(m Message) ([]byte, error)
}

// registry maps a configured kind to its rendering, on the model of
// checks.Registry. It is populated once at init and only read afterwards, so
// it needs no lock: unlike the check registry there is nothing an operator
// can register at runtime, and three kinds is the whole set D2 ships.
var registry = map[string]Channel{
	discordChannel{}.Kind(): discordChannel{},
	slackChannel{}.Kind():   slackChannel{},
	webhookChannel{}.Kind(): webhookChannel{},
}

// ChannelFor looks up a rendering by configured kind.
//
// The error names every kind that exists, because the caller is an operator
// who typed one word wrong and the alternative is a trip to the source.
func ChannelFor(kind string) (Channel, error) {
	c, ok := registry[kind]
	if !ok {
		return nil, fmt.Errorf("notify: unknown channel kind %q (known kinds: %s)",
			kind, strings.Join(Kinds(), ", "))
	}
	return c, nil
}

// Kinds returns every channel kind that exists, sorted.
func Kinds() []string {
	kinds := make([]string, 0, len(registry))
	for k := range registry {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}

// title is the one line a human reads in a notification list before deciding
// whether to open the message at all, so it carries the two facts that decide
// that: which workload, and what moved.
func (m Message) title() string {
	return m.Workload + ": " + m.Transition.Headline
}

// proofText renders the current proof level for a human.
//
// core.ProofLevel.String already answers "not recorded" for the zero value,
// which is the honest wording: a run written before E1 proved something, we
// simply did not write down what.
func (m Message) proofText() string {
	return m.Transition.Current.ProofLevel.String()
}

// rtoText renders the recovery time, or the product's "no value" glyph.
//
// A zero duration means the run did not record one, not that it recovered
// instantaneously. Rendering it as "0s" would be the same lie the dashboard
// refuses to tell when it prints "--" rather than 0%.
func (m Message) rtoText() string {
	if m.RTO <= 0 {
		return "--"
	}
	return m.RTO.Round(time.Second).String()
}
