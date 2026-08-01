package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// telegramMaxLen is the Bot API limit for one message body.
const telegramMaxLen = 4096

// Options configures the inbound bot.
type Options struct {
	Token string
	// Allowed is the set of chat IDs permitted to ask questions. Empty serves
	// nobody — see the package comment.
	Allowed Allowlist
	// Answerer produces replies.
	Answerer Answerer
	// MaxPerMinute caps questions per chat. Zero disables the limit.
	MaxPerMinute int
	// AnswerTimeout bounds one question's total handling time, so a slow model
	// cannot stall the poll loop indefinitely.
	AnswerTimeout time.Duration
	Log           *slog.Logger
}

// Bot long-polls Telegram for messages and replies with answers.
//
// Long polling rather than a webhook is deliberate: podsmedic runs as a single
// replica with no inbound network exposure, and getUpdates keeps it that way.
// It also means the Bot API's own offset acknowledgement is the only cursor
// there is to keep, so nothing needs persisting across restarts.
type Bot struct {
	opts    Options
	baseURL string
	http    *http.Client
	limiter *Limiter
	log     *slog.Logger

	// offset is the Bot API update cursor: the next update ID to fetch.
	offset int64
	// startedAt drops any message that predates this process.
	startedAt time.Time
}

// New builds a bot. It returns an error when the configuration could not serve
// anyone, rather than starting a loop that silently ignores every message.
func New(opts Options) (*Bot, error) {
	if opts.Token == "" {
		return nil, errors.New("no Telegram bot token")
	}
	if len(opts.Allowed) == 0 {
		return nil, errors.New("no allowed chat IDs: set PODSMEDIC_TELEGRAM_ALLOWED_CHATS or TELEGRAM_CHAT_ID")
	}
	if opts.Answerer == nil {
		return nil, errors.New("no answerer")
	}
	if opts.AnswerTimeout <= 0 {
		opts.AnswerTimeout = 2 * time.Minute
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Bot{
		opts:    opts,
		baseURL: "https://api.telegram.org",
		// Must exceed the long-poll timeout below, or every poll would abort.
		http:      &http.Client{Timeout: 70 * time.Second},
		limiter:   NewLimiter(opts.MaxPerMinute),
		log:       log,
		startedAt: time.Now(),
	}, nil
}

// pollTimeout is how long the Bot API holds an empty getUpdates open.
const pollTimeout = 50 * time.Second

// Run polls until the context is cancelled. Transport errors are logged and
// retried with a short backoff: an unreachable Telegram must not take the agent
// down, and the sweep loop keeps running regardless.
func (b *Bot) Run(ctx context.Context) error {
	b.log.Info("telegram chat listener started", "chats", len(b.opts.Allowed))

	var backoff time.Duration
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		updates, err := b.getUpdates(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			backoff = nextBackoff(backoff)
			b.log.Warn("telegram getUpdates failed", "err", err, "retryIn", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0

		for _, u := range updates {
			// Acknowledge before handling: a message that makes handling panic or
			// hang must not be redelivered forever.
			if u.UpdateID >= b.offset {
				b.offset = u.UpdateID + 1
			}
			b.handle(ctx, u)
		}
	}
}

func nextBackoff(cur time.Duration) time.Duration {
	switch {
	case cur <= 0:
		return time.Second
	case cur >= 30*time.Second:
		return 30 * time.Second
	default:
		return cur * 2
	}
}

// update is the subset of a Telegram Update podsmedic reads.
type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64  `json:"message_id"`
		Date      int64  `json:"date"`
		Text      string `json:"text"`
		From      *struct {
			Username  string `json:"username"`
			FirstName string `json:"first_name"`
		} `json:"from"`
		Chat *struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
}

func (b *Bot) getUpdates(ctx context.Context) ([]update, error) {
	body, err := json.Marshal(map[string]any{
		"offset":          b.offset,
		"timeout":         int(pollTimeout.Seconds()),
		"allowed_updates": []string{"message"},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.apiURL("getUpdates"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var parsed struct {
		OK          bool     `json:"ok"`
		Description string   `json:"description"`
		Result      []update `json:"result"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode getUpdates: %w", err)
	}
	if !parsed.OK {
		desc := strings.TrimSpace(parsed.Description)
		if desc == "" {
			desc = resp.Status
		}
		return nil, errors.New(desc)
	}
	return parsed.Result, nil
}

// handle authorises, rate limits, and answers one message.
func (b *Bot) handle(ctx context.Context, u update) {
	msg := u.Message
	if msg == nil || msg.Chat == nil || strings.TrimSpace(msg.Text) == "" {
		return
	}
	chatID := msg.Chat.ID

	// Drop the backlog. Telegram retains undelivered updates for ~24h, so a
	// restart after an outage would otherwise replay every question at once —
	// each one an LLM call answering a long-stale cluster state.
	if msg.Date > 0 && time.Unix(msg.Date, 0).Before(b.startedAt) {
		b.log.Debug("ignoring message from before startup", "chat", chatID)
		return
	}

	if !b.opts.Allowed.Permits(chatID) {
		// Do not reply. An unauthorised sender learns nothing, not even that the
		// chat ID was wrong.
		b.log.Warn("ignoring message from unauthorised chat", "chat", chatID, "from", sender(u))
		return
	}
	if !b.limiter.Allow(chatID, time.Now()) {
		b.reply(ctx, chatID, "Rate limited — too many questions in the last minute. Try again shortly.")
		return
	}

	cmd, text := Parse(msg.Text)
	if cmd == CmdAsk && text == "" {
		b.reply(ctx, chatID, HelpText)
		return
	}

	q := Question{ChatID: chatID, From: sender(u), Command: cmd, Text: text}
	b.log.Info("chat question", "chat", chatID, "from", q.From, "command", string(cmd))

	answerCtx, cancel := context.WithTimeout(ctx, b.opts.AnswerTimeout)
	defer cancel()

	reply, err := b.opts.Answerer.Answer(answerCtx, q)
	if err != nil {
		b.log.Error("answering chat question failed", "chat", chatID, "err", err)
		b.reply(ctx, chatID, "Sorry — I could not answer that: "+err.Error())
		return
	}
	if reply.Document != nil {
		if err := b.sendDocument(ctx, chatID, reply); err != nil {
			b.log.Error("chat document upload failed", "chat", chatID, "err", err)
			b.reply(ctx, chatID, "I built the document but could not upload it: "+err.Error())
		}
		return
	}
	b.reply(ctx, chatID, reply.Text)
}

// telegramCaptionMax is the Bot API limit for a document caption. Longer text is
// trimmed rather than rejected, since losing the file over a long caption would
// be the worse failure.
const telegramCaptionMax = 1024

// sendDocument uploads a generated file as a chat attachment.
//
// This is the one place podsmedic speaks multipart rather than JSON: sendDocument
// takes the file as form data. It is hand-rolled against mime/multipart from the
// standard library, in keeping with the project's net/http-only stance.
func (b *Bot) sendDocument(ctx context.Context, chatID int64, reply Reply) error {
	doc := reply.Document
	var body bytes.Buffer
	form := multipart.NewWriter(&body)

	if err := form.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return err
	}
	if caption := strings.TrimSpace(reply.Text); caption != "" {
		if len(caption) > telegramCaptionMax {
			caption = caption[:telegramCaptionMax-1] + "…"
		}
		if err := form.WriteField("caption", caption); err != nil {
			return err
		}
	}

	part, err := form.CreateFormFile("document", doc.Filename)
	if err != nil {
		return err
	}
	if _, err := part.Write(doc.Content); err != nil {
		return err
	}
	if err := form.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.apiURL("sendDocument"), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", form.FormDataContentType())

	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bot api returned %s: %s", resp.Status, strings.TrimSpace(string(payload)))
	}
	b.log.Info("chat document sent", "chat", chatID, "file", doc.Filename, "bytes", len(doc.Content))
	return nil
}

func sender(u update) string {
	if u.Message == nil || u.Message.From == nil {
		return "unknown"
	}
	if u.Message.From.Username != "" {
		return "@" + u.Message.From.Username
	}
	return u.Message.From.FirstName
}

// reply sends text back, splitting it to the Bot API length limit. Answers are
// plain text escaped into HTML rather than sent as markdown: a model reply can
// contain arbitrary characters, and an unbalanced markdown entity makes the Bot
// API reject the whole message.
func (b *Bot) reply(ctx context.Context, chatID int64, text string) {
	for _, chunk := range splitMessage(text, telegramMaxLen-16) {
		body, err := json.Marshal(map[string]any{
			"chat_id":                  chatID,
			"text":                     html.EscapeString(chunk),
			"parse_mode":               "HTML",
			"disable_web_page_preview": true,
		})
		if err != nil {
			b.log.Error("chat reply: marshal failed", "err", err)
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.apiURL("sendMessage"), bytes.NewReader(body))
		if err != nil {
			b.log.Error("chat reply: build request failed", "err", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := b.http.Do(req)
		if err != nil {
			b.log.Error("chat reply: send failed", "err", err)
			return
		}
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.log.Error("chat reply rejected", "status", resp.Status, "body", strings.TrimSpace(string(payload)))
			return
		}
	}
}

func (b *Bot) apiURL(method string) string {
	return fmt.Sprintf("%s/bot%s/%s", b.baseURL, b.opts.Token, method)
}

// splitMessage breaks text on line boundaries so a reply longer than the Bot
// API limit arrives as several readable messages.
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
