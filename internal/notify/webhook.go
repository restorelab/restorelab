package notify

import (
	"encoding/json"
	"time"
)

// WebhookSchema names the shape of the generic payload.
//
// It is versioned in the body rather than in the URL because the operator
// owns the URL: a consumer written today has to be able to tell, from what
// arrives, whether it is still parsing what it was written against. The
// version changes only when a field is removed or given a new meaning.
// Adding a field does not, so a consumer must ignore fields it does not know.
const WebhookSchema = "restorelab.notification.v1"

type webhookChannel struct{}

func (webhookChannel) Kind() string { return "webhook" }

// webhookPayload is a wire contract, not an internal struct. Every json tag
// here is somebody's filter expression, so renaming one is a breaking change
// and WebhookSchema is what announces it.
type webhookPayload struct {
	Schema   string          `json:"schema"`
	Kind     Kind            `json:"kind"`
	Headline string          `json:"headline"`
	At       string          `json:"at"`
	Workload webhookWorkload `json:"workload"`
	Plan     string          `json:"plan"`
	Run      webhookRun      `json:"run"`
	Current  webhookStory    `json:"current"`
	// Previous is absent for a first verdict rather than an empty object: a
	// consumer testing for the field gets the truthful answer "there was
	// nothing before this" instead of a story made of empty strings.
	Previous *webhookStory `json:"previous,omitempty"`
}

type webhookWorkload struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type webhookRun struct {
	ID   string `json:"id"`
	Link string `json:"link,omitempty"`
	// RTOMs is milliseconds because that is the unit the runs table stores
	// and the API returns. Two representations of one duration in one product
	// is how they drift apart.
	RTOMs int64 `json:"rto_ms,omitempty"`
}

type webhookStory struct {
	Result string `json:"result"`
	// ProofLevel is the raw stored level, so an unrecorded one travels as the
	// empty string rather than as the prose core.ProofLevel.String renders
	// for a human. A consumer has to be able to tell "nothing was written
	// down" from any real level, and the empty string is how the rest of the
	// product says it.
	ProofLevel string `json:"proof_level"`
}

func (webhookChannel) Render(m Message) ([]byte, error) {
	payload := webhookPayload{
		Schema:   WebhookSchema,
		Kind:     m.Transition.Kind,
		Headline: m.Transition.Headline,
		At:       m.At.UTC().Format(time.RFC3339),
		Workload: webhookWorkload{ID: m.WorkloadID, Name: m.Workload},
		Plan:     m.PlanName,
		Run: webhookRun{
			ID:    m.RunID,
			Link:  m.Link,
			RTOMs: m.RTO.Milliseconds(),
		},
		Current: webhookStory{
			Result:     string(m.Transition.Current.Result),
			ProofLevel: string(m.Transition.Current.ProofLevel),
		},
	}
	if p := m.Transition.Previous; p != nil {
		payload.Previous = &webhookStory{
			Result:     string(p.Result),
			ProofLevel: string(p.ProofLevel),
		}
	}
	return json.Marshal(payload)
}
