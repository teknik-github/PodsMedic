package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Slack posts alerts to an incoming webhook.
type Slack struct {
	webhookURL string
	http       *http.Client
}

// NewSlack builds a Slack webhook notifier.
func NewSlack(webhookURL string) *Slack {
	return &Slack{
		webhookURL: webhookURL,
		http:       &http.Client{Timeout: 15 * time.Second},
	}
}

// Name identifies the sink.
func (s *Slack) Name() string { return "slack" }

// Notify posts the diagnosis as a mrkdwn message.
func (s *Slack) Notify(ctx context.Context, a Alert) error {
	return s.post(ctx, slackText(a))
}

// Notice posts a standalone message (heal verification or rollback).
func (s *Slack) Notice(ctx context.Context, text string) error {
	return s.post(ctx, ":stethoscope: "+text)
}

// Check validates the webhook URL structurally. A Slack incoming webhook has no
// read endpoint, and a live probe would post a visible message, so this
// verifies shape only: https, the Slack hooks host, and a /services/ path. A
// truly dead webhook (revoked, wrong team) is only detectable on first POST —
// that failure is metered and logged when it happens.
func (s *Slack) Check(_ context.Context) error {
	u, err := url.Parse(s.webhookURL)
	if err != nil {
		return fmt.Errorf("malformed webhook URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("webhook URL must be https, got %q", u.Scheme)
	}
	if !strings.HasSuffix(u.Host, "slack.com") {
		return fmt.Errorf("webhook host %q is not a slack.com host", u.Host)
	}
	if !strings.HasPrefix(u.Path, "/services/") {
		return fmt.Errorf("webhook path %q is not a /services/ incoming-webhook path", u.Path)
	}
	if strings.Contains(s.webhookURL, "T000/B000") || strings.Contains(s.webhookURL, "xxxx") {
		return fmt.Errorf("webhook URL is still the placeholder from the example")
	}
	return nil
}

func (s *Slack) post(ctx context.Context, text string) error {
	body, err := json.Marshal(map[string]any{"text": text, "mrkdwn": true})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("post webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("webhook returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func slackText(a Alert) string {
	d := a.Diagnosis
	var b strings.Builder

	fmt.Fprintf(&b, "%s *%s*\n", severityEmoji(d.Severity), d.Title)
	fmt.Fprintf(&b, "`%s/%s`", a.Problem.Namespace, a.Problem.Pod)
	if a.Problem.Container != "" {
		fmt.Fprintf(&b, " container `%s`", a.Problem.Container)
	}
	fmt.Fprintf(&b, " · %s · confidence %s\n\n", a.Problem.Kind, d.Confidence)

	fmt.Fprintf(&b, "%s\n\n", d.Summary)
	fmt.Fprintf(&b, "*Root cause*\n%s\n", d.RootCause)

	if len(d.Evidence) > 0 {
		b.WriteString("\n*Evidence*\n")
		for _, e := range d.Evidence {
			fmt.Fprintf(&b, "• %s\n", truncate(e, 400))
		}
	}

	if len(d.Remediation) > 0 {
		b.WriteString("\n*Suggested fix*\n")
		for i, step := range d.Remediation {
			fmt.Fprintf(&b, "%d. %s\n", i+1, step.Description)
			if step.Command != "" {
				fmt.Fprintf(&b, "```%s```\n", step.Command)
			}
		}
	}

	if cl := correlatedLine(a); cl != "" {
		fmt.Fprintf(&b, "\n_%s_\n", cl)
	}
	if line := healLine(a.Heal); line != "" {
		fmt.Fprintf(&b, "\n:robot_face: %s\n", line)
	}

	return b.String()
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
