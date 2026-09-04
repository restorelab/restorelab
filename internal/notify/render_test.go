package notify

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// sealedURL is what a configured channel's credential looks like on disk. No
// rendering may ever contain it, or a chat channel becomes a place where the
// key to that chat channel is published.
const sealedURL = "rlsec:v1:Zm9vYmFyYmF6"

// plainURL is the same credential unsealed. Same rule.
const plainURL = "https://discord.com/api/webhooks/1234567890/aVerySecretPath"

func sampleMessage() Message {
	return Message{
		Workload:   "web-01",
		WorkloadID: "101",
		PlanName:   "nightly-web",
		RunID:      "run-7f3a",
		Link:       "https://restorelab.example.com/runs/run-7f3a",
		At:         time.Date(2026, 9, 4, 3, 12, 0, 0, time.UTC),
		RTO:        3*time.Minute + 20*time.Second,
		Transition: Transition{
			Kind:     KindVerdict,
			Current:  Story{core.ResultFailed, core.ProofBoot},
			Previous: &Story{core.ResultSuccess, core.ProofService},
			Headline: "FAILED, was SUCCESS",
		},
	}
}

func TestChannelForNamesTheKindsThatExist(t *testing.T) {
	for _, kind := range Kinds() {
		c, err := ChannelFor(kind)
		if err != nil {
			t.Fatalf("ChannelFor(%q) returned an error for a kind Kinds() advertises: %v", kind, err)
		}
		if c.Kind() != kind {
			t.Errorf("ChannelFor(%q).Kind() = %q", kind, c.Kind())
		}
	}

	_, err := ChannelFor("telegram")
	if err == nil {
		t.Fatal("ChannelFor accepted a kind that does not exist")
	}
	// An operator who typed the wrong word needs to read the right ones. The
	// same courtesy checks.Registry extends for an unknown check type.
	for _, kind := range Kinds() {
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("error for an unknown kind does not name %q: %v", kind, err)
		}
	}
}

// TestRenderNeverCarriesTheCredential is the structural half of the promise.
// The renderers cannot leak the webhook URL because they are never handed it,
// and this fails the day somebody adds it to Message for convenience.
func TestRenderMessageCarriesNoCredential(t *testing.T) {
	banned := []string{"url", "secret", "token", "credential", "webhook", "key"}
	msg := reflect.TypeOf(Message{})
	for i := range msg.NumField() {
		name := strings.ToLower(msg.Field(i).Name)
		for _, b := range banned {
			if strings.Contains(name, b) {
				t.Errorf("Message.%s reaches every renderer; a credential must not travel with the "+
					"thing being rendered into a chat channel", msg.Field(i).Name)
			}
		}
	}
}

func TestRenderCarriesTheStoryAndNotTheCredential(t *testing.T) {
	m := sampleMessage()
	for _, kind := range Kinds() {
		t.Run(kind, func(t *testing.T) {
			c, err := ChannelFor(kind)
			if err != nil {
				t.Fatalf("ChannelFor(%q): %v", kind, err)
			}
			body, err := c.Render(m)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}

			var parsed any
			if err := json.Unmarshal(body, &parsed); err != nil {
				t.Fatalf("rendered body is not valid JSON: %v\n%s", err, body)
			}

			s := string(body)
			if !strings.Contains(s, m.Workload) {
				t.Errorf("body never names the workload:\n%s", s)
			}
			if !strings.Contains(s, m.Transition.Headline) {
				t.Errorf("body never says what changed:\n%s", s)
			}
			if strings.Contains(s, "rlsec:") || strings.Contains(s, sealedURL) || strings.Contains(s, plainURL) {
				t.Errorf("body carries the channel credential:\n%s", s)
			}
		})
	}
}

// TestRenderWithoutALinkHasNoLink guards the trap in the middle of this
// feature: server.base_url is optional, so most installations render with an
// empty Link. An empty href in a Discord embed or a Slack button is a dead
// control an operator clicks once and stops trusting.
func TestRenderWithoutALinkHasNoLink(t *testing.T) {
	m := sampleMessage()
	m.Link = ""

	for _, kind := range Kinds() {
		t.Run(kind, func(t *testing.T) {
			c, _ := ChannelFor(kind)
			body, err := c.Render(m)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			var parsed any
			if err := json.Unmarshal(body, &parsed); err != nil {
				t.Fatalf("rendered body is not valid JSON: %v\n%s", err, body)
			}
			if strings.Contains(string(body), `"url":""`) || strings.Contains(string(body), `"link":""`) {
				t.Errorf("body carries an empty link rather than no link:\n%s", body)
			}
		})
	}
}

// TestWebhookSchemaIsFrozen states the wire contract literally rather than
// reading it back from the constants that produce it. A test written against
// the constant would follow a rename silently, and a rename is exactly the
// change that breaks somebody's filter.
func TestWebhookSchemaIsFrozen(t *testing.T) {
	c, err := ChannelFor("webhook")
	if err != nil {
		t.Fatalf("ChannelFor(webhook): %v", err)
	}

	kinds := []struct {
		kind Kind
		want string
	}{
		{KindFirstVerdict, "first_verdict"},
		{KindVerdict, "verdict_changed"},
		{KindProofDropped, "proof_dropped"},
		{KindUnevaluable, "became_unevaluable"},
		{KindEvaluable, "became_evaluable_again"},
	}

	for _, tc := range kinds {
		m := sampleMessage()
		m.Transition.Kind = tc.kind
		body, err := c.Render(m)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		s := string(body)
		if !strings.Contains(s, `"schema":"restorelab.notification.v1"`) {
			t.Errorf("webhook payload does not carry the versioned schema:\n%s", s)
		}
		if !strings.Contains(s, `"kind":"`+tc.want+`"`) {
			t.Errorf("webhook payload does not carry kind %q verbatim:\n%s", tc.want, s)
		}
	}
}

// TestDiscordColourMatchesTheVerdict. Amber is not green. The most visible
// defect on the public landing page was a degraded run captioned "Succeeded"
// next to its amber icon (6f31637); the same mistake in a chat message is the
// same lie, in a place nobody can correct it afterwards.
func TestDiscordColourMatchesTheVerdict(t *testing.T) {
	cases := []struct {
		name   string
		result core.RunResult
		want   int
	}{
		{"success is green", core.ResultSuccess, colourSuccess},
		{"degraded is amber, not green", core.ResultDegraded, colourDegraded},
		{"failed is red", core.ResultFailed, colourFailed},
		{"no verdict at all is grey", "", colourNoVerdict},
	}

	c, err := ChannelFor("discord")
	if err != nil {
		t.Fatalf("ChannelFor(discord): %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleMessage()
			m.Transition.Current.Result = tc.result
			body, err := c.Render(m)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			var payload struct {
				Embeds []struct {
					Color int `json:"color"`
				} `json:"embeds"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("decode: %v\n%s", err, body)
			}
			if len(payload.Embeds) != 1 {
				t.Fatalf("got %d embeds, want exactly 1:\n%s", len(payload.Embeds), body)
			}
			if payload.Embeds[0].Color != tc.want {
				t.Errorf("colour = %d, want %d", payload.Embeds[0].Color, tc.want)
			}
		})
	}

	if colourSuccess == colourDegraded {
		t.Error("degraded renders in the same colour as success, which is the lie this test exists to prevent")
	}
}
