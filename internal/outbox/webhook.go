package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// WebhookNotifier POSTs notification batches to an application endpoint.
//
// BoltQ deliberately does not speak APNs or FCM. Push credentials rotate,
// payload formats are platform-specific and change, and the decision of what a
// notification should say is product logic, not broker logic. Handing the
// application a batch and letting it decide keeps all of that out of the
// broker while still giving it a reliable, cursor-tracked trigger.
type WebhookNotifier struct {
	url        string
	authHeader string
	client     *http.Client
}

// WebhookOptions configures the notifier.
type WebhookOptions struct {
	URL        string
	AuthHeader string
	// Timeout bounds a single POST. The dispatcher retries with backoff, so a
	// short timeout is better than a long one: it fails fast and lets the
	// retry schedule — not the TCP stack — decide the pacing.
	Timeout time.Duration
}

// NewWebhookNotifier builds a webhook notifier.
func NewWebhookNotifier(opts WebhookOptions) (*WebhookNotifier, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("outbox: webhook url is required")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	return &WebhookNotifier{
		url:        opts.URL,
		authHeader: opts.AuthHeader,
		client: &http.Client{
			Timeout: opts.Timeout,
			Transport: &http.Transport{
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
				DialContext: (&net.Dialer{
					Timeout:   3 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			},
		},
	}, nil
}

type webhookPayload struct {
	Notifications []Notification `json:"notifications"`
	Count         int            `json:"count"`
	SentAt        int64          `json:"sent_at"`
}

// Notify implements Notifier.
func (w *WebhookNotifier) Notify(ctx context.Context, batch []Notification) error {
	if len(batch) == 0 {
		return nil
	}

	body, err := json.Marshal(webhookPayload{
		Notifications: batch,
		Count:         len(batch),
		SentAt:        time.Now().UnixNano(),
	})
	if err != nil {
		return fmt.Errorf("encode webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if w.authHeader != "" {
		req.Header.Set("Authorization", w.authHeader)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook request: %w", err)
	}
	defer resp.Body.Close()

	// Drain a bounded amount so the connection can be reused rather than torn
	// down and redialled for the next batch.
	preview, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook returned %d: %s", resp.StatusCode, preview)
	}
	return nil
}
