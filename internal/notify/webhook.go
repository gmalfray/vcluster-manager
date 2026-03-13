package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Notifier sends event notifications to a generic webhook endpoint.
// The payload is compatible with Slack/Mattermost/Rocket.Chat incoming webhooks.
type Notifier struct {
	webhookURL string
	client     *http.Client
}

// New creates a Notifier for the given webhook URL.
func New(webhookURL string) *Notifier {
	return &Notifier{
		webhookURL: webhookURL,
		client:     &http.Client{Timeout: 10 * time.Second},
	}
}

type payload struct {
	Text string `json:"text"`
}

// Send posts a plain-text notification message to the webhook.
func (n *Notifier) Send(text string) error {
	body, err := json.Marshal(payload{Text: text})
	if err != nil {
		return fmt.Errorf("webhook marshal: %w", err)
	}
	resp, err := n.client.Post(n.webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}
