package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"
)

// telegramMaxLen is the Telegram Bot API limit for a single message body.
const telegramMaxLen = 4096

// Telegram posts alerts through the Bot API.
type Telegram struct {
	token   string
	chatID  string
	baseURL string // Bot API base; overridable in tests
	http    *http.Client
}

// NewTelegram builds a Telegram notifier.
func NewTelegram(token, chatID string) *Telegram {
	return &Telegram{
		token:   token,
		chatID:  chatID,
		baseURL: "https://api.telegram.org",
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Name identifies the sink.
func (t *Telegram) Name() string { return "telegram" }

// Notify sends the diagnosis as an HTML-formatted message, splitting it if it
// exceeds the Bot API length limit.
func (t *Telegram) Notify(ctx context.Context, a Alert) error {
	for _, chunk := range splitMessage(telegramText(a), telegramMaxLen) {
		if err := t.send(ctx, chunk); err != nil {
			return err
		}
	}
	return nil
}

// Notice sends a standalone message (heal verification or rollback).
func (t *Telegram) Notice(ctx context.Context, text string) error {
	return t.send(ctx, "🩺 "+html.EscapeString(text))
}

// Check validates the bot token via getMe and the target chat via getChat, so a
// bad token or an unreachable/wrong chat ID is caught at startup. Both are
// read-only Bot API calls; neither posts a message.
func (t *Telegram) Check(ctx context.Context) error {
	if err := t.apiOK(ctx, "getMe", nil); err != nil {
		return fmt.Errorf("token check (getMe) failed: %w", err)
	}
	if err := t.apiOK(ctx, "getChat", map[string]any{"chat_id": t.chatID}); err != nil {
		return fmt.Errorf("chat check (getChat) failed: %w", err)
	}
	return nil
}

// apiOK calls a Bot API method and reports whether it returned ok:true.
func (t *Telegram) apiOK(ctx context.Context, method string, params map[string]any) error {
	var body []byte
	if len(params) > 0 {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		body = b
	}
	url := fmt.Sprintf("%s/bot%s/%s", t.baseURL, t.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode != http.StatusOK || !out.OK {
		desc := strings.TrimSpace(out.Description)
		if desc == "" {
			desc = resp.Status
		}
		return fmt.Errorf("%s", desc)
	}
	return nil
}

func (t *Telegram) send(ctx context.Context, text string) error {
	body, err := json.Marshal(map[string]any{
		"chat_id":                  t.chatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", t.baseURL, t.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.http.Do(req)
	if err != nil {
		return fmt.Errorf("call bot api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("bot api returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func telegramText(a Alert) string {
	d := a.Diagnosis
	esc := html.EscapeString
	var b strings.Builder

	fmt.Fprintf(&b, "%s <b>%s</b>\n", severityEmoji(d.Severity), esc(d.Title))
	fmt.Fprintf(&b, "<code>%s/%s</code>", esc(a.Problem.Namespace), esc(a.Problem.Pod))
	if a.Problem.Container != "" {
		fmt.Fprintf(&b, " container <code>%s</code>", esc(a.Problem.Container))
	}
	fmt.Fprintf(&b, "\n%s · confidence %s\n\n", esc(string(a.Problem.Kind)), esc(d.Confidence))

	fmt.Fprintf(&b, "%s\n\n", esc(d.Summary))
	fmt.Fprintf(&b, "<b>Root cause</b>\n%s\n", esc(d.RootCause))

	if len(d.Evidence) > 0 {
		b.WriteString("\n<b>Evidence</b>\n")
		for _, e := range d.Evidence {
			fmt.Fprintf(&b, "• <code>%s</code>\n", esc(truncate(e, 400)))
		}
	}

	if len(d.Remediation) > 0 {
		b.WriteString("\n<b>Suggested fix</b>\n")
		for i, step := range d.Remediation {
			fmt.Fprintf(&b, "%d. %s\n", i+1, esc(step.Description))
			if step.Command != "" {
				fmt.Fprintf(&b, "<pre>%s</pre>\n", esc(step.Command))
			}
		}
	}

	if cl := correlatedLine(a); cl != "" {
		fmt.Fprintf(&b, "\n<i>%s</i>\n", esc(cl))
	}
	if line := healLine(a.Heal); line != "" {
		fmt.Fprintf(&b, "\n🤖 <b>%s</b>\n", esc(line))
	}

	return b.String()
}

// splitMessage breaks text on line boundaries so HTML tags are not cut in half
// mid-element.
func splitMessage(text string, max int) []string {
	if len(text) <= max {
		return []string{text}
	}

	var out []string
	var cur strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		if cur.Len()+len(line) > max && cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
		// A single line longer than the limit is hard-cut; rare in practice
		// because evidence lines are already truncated.
		for len(line) > max {
			out = append(out, line[:max])
			line = line[max:]
		}
		cur.WriteString(line)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
