package notify

import (
	"encoding/json"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// The embed colours, as the decimal integers the Discord API takes.
//
// Degraded has its own colour and it is not green. The most visible defect
// this product ever shipped was a degraded run captioned as a success next to
// its amber icon (6f31637); the same mistake in a chat message is the same
// lie told in a place nobody can go back and correct.
//
// A transition carrying no verdict at all, a workload that stopped being
// evaluable, is grey rather than red. It is not a failure, and colouring it
// like one would teach an operator that red sometimes means "we could not
// look", which is exactly the confusion INCONCLUSIVE exists to remove.
const (
	colourSuccess   = 0x2ea043
	colourDegraded  = 0xd29922
	colourFailed    = 0xda3633
	colourNoVerdict = 0x8b949e
)

type discordChannel struct{}

func (discordChannel) Kind() string { return "discord" }

type discordPayload struct {
	Embeds []discordEmbed `json:"embeds"`
}

type discordEmbed struct {
	Title string `json:"title"`
	// URL turns the title into a link. It is omitted rather than emitted
	// empty: an installation with no server.base_url must get a message with
	// no link, not a title that looks clickable and goes nowhere.
	URL         string              `json:"url,omitempty"`
	Description string              `json:"description"`
	Color       int                 `json:"color"`
	Timestamp   string              `json:"timestamp"`
	Fields      []discordEmbedField `json:"fields"`
	Footer      discordEmbedFooter  `json:"footer"`
}

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type discordEmbedFooter struct {
	Text string `json:"text"`
}

func (discordChannel) Render(m Message) ([]byte, error) {
	fields := []discordEmbedField{
		{Name: "Plan", Value: m.PlanName, Inline: true},
		{Name: "Proof", Value: m.proofText(), Inline: true},
		{Name: "Recovery time", Value: m.rtoText(), Inline: true},
	}
	if p := m.Transition.Previous; p != nil {
		fields = append(fields, discordEmbedField{
			Name:   "Previously",
			Value:  string(p.Result) + " at " + p.ProofLevel.String(),
			Inline: true,
		})
	}

	return json.Marshal(discordPayload{Embeds: []discordEmbed{{
		Title:       m.Workload,
		URL:         m.Link,
		Description: m.Transition.Headline,
		Color:       embedColour(m.Transition),
		Timestamp:   m.At.UTC().Format(time.RFC3339),
		Fields:      fields,
		// The run id rather than the workload id, because it is what an
		// operator types into the CLI next.
		Footer: discordEmbedFooter{Text: "run " + m.RunID},
	}}})
}

// embedColour picks the embed colour for a transition.
//
// It is the verdict's colour, with one exception. A collapsed value fires on a
// run that passed every bound it was given, because none was given, so the
// verdict is SUCCESS and the verdict colour is green. Painting "this workload
// now holds an empty database" green would be the reassuring green this whole
// product exists to remove, one level further out: the drill was honest, and
// the message about it would not be.
//
// Amber rather than red, and the distinction is the same one DEGRADED already
// makes. Nothing failed. Nobody declared this wrong. RestoreLab is saying it
// noticed, which is worth an operator's eyes and not their night.
func embedColour(t Transition) int {
	if t.Kind == KindValueCollapsed {
		return colourDegraded
	}
	return verdictColour(t.Current.Result)
}

func verdictColour(r core.RunResult) int {
	switch r {
	case core.ResultSuccess:
		return colourSuccess
	case core.ResultDegraded:
		return colourDegraded
	case core.ResultFailed:
		return colourFailed
	default:
		return colourNoVerdict
	}
}
