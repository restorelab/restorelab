package notify

import "encoding/json"

type slackChannel struct{}

func (slackChannel) Kind() string { return "slack" }

// slackPayload is Block Kit, plus the plain text Slack needs alongside it.
//
// Text is not a duplicate of the blocks: it is what a push notification and a
// screen reader are given, and a payload without it arrives on a phone as
// "this content cannot be displayed". The line it carries is the same line
// the blocks lead with.
type slackPayload struct {
	Text   string       `json:"text"`
	Blocks []slackBlock `json:"blocks"`
}

type slackBlock struct {
	Type     string         `json:"type"`
	Text     *slackText     `json:"text,omitempty"`
	Fields   []slackText    `json:"fields,omitempty"`
	Elements []slackElement `json:"elements,omitempty"`
}

type slackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type slackElement struct {
	Type string     `json:"type"`
	Text *slackText `json:"text,omitempty"`
	URL  string     `json:"url,omitempty"`
}

func (slackChannel) Render(m Message) ([]byte, error) {
	fields := []slackText{
		{Type: "mrkdwn", Text: "*Plan*\n" + m.PlanName},
		{Type: "mrkdwn", Text: "*Proof*\n" + m.proofText()},
		{Type: "mrkdwn", Text: "*Recovery time*\n" + m.rtoText()},
		{Type: "mrkdwn", Text: "*Run*\n" + m.RunID},
	}

	blocks := []slackBlock{
		// plain_text, never mrkdwn, for anything that came from an operator.
		// A workload named with an asterisk in it would otherwise render in
		// bold with the asterisks eaten, and the name in the alert would not
		// be the name in the product.
		{Type: "header", Text: &slackText{Type: "plain_text", Text: m.Workload}},
		{Type: "section", Text: &slackText{Type: "plain_text", Text: m.Transition.Headline}},
		{Type: "section", Fields: fields},
	}

	// The actions block exists only when there is somewhere to go. Slack
	// rejects a button carrying an empty url outright, so an installation
	// with no server.base_url would get no message at all rather than a
	// message with no button.
	if m.Link != "" {
		blocks = append(blocks, slackBlock{Type: "actions", Elements: []slackElement{{
			Type: "button",
			Text: &slackText{Type: "plain_text", Text: "Open in RestoreLab"},
			URL:  m.Link,
		}}})
	}

	return json.Marshal(slackPayload{Text: m.title(), Blocks: blocks})
}
