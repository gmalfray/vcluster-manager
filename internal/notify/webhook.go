package notify

import (
	"bytes"
	"context"
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
// The HTTP request honors ctx for cancellation in addition to the client's
// own 10s timeout.
func (n *Notifier) Send(ctx context.Context, text string) error {
	body, err := json.Marshal(payload{Text: text})
	if err != nil {
		return fmt.Errorf("webhook marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}
