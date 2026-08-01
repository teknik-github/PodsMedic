// Package chat turns podsmedic's Telegram bot into a two-way channel: as well
// as pushing alerts out, it long-polls for operator questions and replies with
// answers drawn from what the agent already knows.
//
// The inbound direction is a genuinely different security posture from the
// outbound one, so the rules live here, pure and tested:
//
//   - Only allowlisted chat IDs are served. A bot's username is discoverable,
//     so without this anyone could DM it and read your cluster's state.
//   - Every reply is text. There is no path from a chat message to heal.Execute;
//     the LLM used for answers is given no action to propose.
//   - Questions are rate limited per chat, because each one costs tokens.
//   - Messages predating the process are dropped, so a restart after an outage
//     does not replay a week of backlog into the model.
package chat

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Command is a recognised slash command, or CmdAsk for a free-form question.
type Command string

const (
	// CmdAsk is anything that is not a known command: a question for the model.
	CmdAsk Command = ""
	// The remaining commands are answered from local state, costing no tokens.
	CmdHelp      Command = "help"
	CmdStatus    Command = "status"
	CmdIncidents Command = "incidents"
	CmdCapacity  Command = "capacity"
	CmdHeals     Command = "heals"
	CmdPlaybook  Command = "playbook"
	CmdExport    Command = "export"
	// CmdRightsize reports containers whose declared requests do not match what
	// they actually use. Suggestions only — nothing here ever changes a
	// workload, which is why it is safe on the read-only chat path.
	CmdRightsize Command = "rightsize"
	// CmdNodes reports the health of the machines under the workloads.
	CmdNodes Command = "nodes"
	// CmdDigest previews the daily summary without disturbing its schedule.
	CmdDigest Command = "digest"
)

// Question is one inbound message, already parsed and authorised.
type Question struct {
	ChatID int64
	// From is the sender's display name, for logging only — never a trust signal.
	From string
	// Command is the recognised command, or CmdAsk.
	Command Command
	// Text is the free-form question (for CmdAsk) or the command's argument.
	Text string
}

// Reply is what an Answerer produces: prose, a file to upload, or both.
//
// The document case exists because some answers are documents — a playbook
// people want to study does not belong pasted into a chat bubble.
type Reply struct {
	Text string
	// Document, when set, is uploaded as a file attachment. Text becomes its
	// caption.
	Document *Document
}

// Document is a file to send to the chat.
type Document struct {
	Filename string
	MIMEType string
	Content  []byte
}

// Answerer produces a reply. The agent implements it, since it holds the
// cluster state a question is answered from.
type Answerer interface {
	Answer(ctx context.Context, q Question) (Reply, error)
}

// Say builds a plain prose reply.
func Say(text string) Reply { return Reply{Text: text} }

// Parse splits an inbound message into a command and its remaining text.
//
// Telegram appends "@botname" to commands sent in a group, and clients vary on
// surrounding whitespace, so both are normalised away. Anything that is not a
// recognised command — including a message that merely starts with a slash — is
// treated as a question, because refusing a typo'd command is more annoying
// than answering it.
func Parse(text string) (Command, string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return CmdAsk, text
	}

	word, rest, _ := strings.Cut(text, " ")
	word = strings.TrimPrefix(word, "/")
	if at := strings.IndexByte(word, '@'); at >= 0 {
		word = word[:at] // "/status@podsmedic_bot" in a group chat
	}
	rest = strings.TrimSpace(rest)

	switch strings.ToLower(word) {
	case "help", "start":
		return CmdHelp, rest
	case "status":
		return CmdStatus, rest
	case "incidents", "incident":
		return CmdIncidents, rest
	case "capacity":
		return CmdCapacity, rest
	case "heals", "heal", "audit":
		return CmdHeals, rest
	case "playbook", "playbooks", "remedies":
		return CmdPlaybook, rest
	case "export", "report", "pdf":
		return CmdExport, rest
	case "rightsize", "rightsizing", "sizing":
		return CmdRightsize, rest
	case "nodes", "node":
		return CmdNodes, rest
	case "digest", "summary", "daily":
		return CmdDigest, rest
	case "ask":
		return CmdAsk, rest
	default:
		return CmdAsk, text
	}
}

// Allowlist decides which chats podsmedic will talk to.
//
// An empty allowlist serves nobody. That is deliberate: an inbound channel that
// defaults to open would expose cluster state to anyone who found the bot.
type Allowlist map[int64]bool

// NewAllowlist builds an allowlist from chat IDs.
func NewAllowlist(ids []int64) Allowlist {
	out := make(Allowlist, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// Permits reports whether this chat may ask questions.
func (a Allowlist) Permits(chatID int64) bool { return a[chatID] }

// Limiter caps how many questions a single chat may ask per minute, so a stuck
// client or a chatty operator cannot run up an unbounded LLM bill.
type Limiter struct {
	mu      sync.Mutex
	perMin  int
	window  time.Duration
	recent  map[int64][]time.Time
	maxKeys int
}

// NewLimiter builds a per-chat rate limiter. A non-positive rate disables
// limiting entirely.
func NewLimiter(perMinute int) *Limiter {
	return &Limiter{
		perMin:  perMinute,
		window:  time.Minute,
		recent:  map[int64][]time.Time{},
		maxKeys: 1000,
	}
}

// Allow records an attempt and reports whether it is within the limit.
func (l *Limiter) Allow(chatID int64, now time.Time) bool {
	if l == nil || l.perMin <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	// Drop timestamps that have aged out, so the map does not grow without
	// bound on a long-running agent.
	cutoff := now.Add(-l.window)
	kept := l.recent[chatID][:0]
	for _, t := range l.recent[chatID] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.perMin {
		l.recent[chatID] = kept
		return false
	}
	if len(kept) == 0 && len(l.recent) > l.maxKeys {
		// Pathological case: a flood of distinct chat IDs. They are already
		// rejected by the allowlist, but do not let bookkeeping grow anyway.
		return false
	}
	l.recent[chatID] = append(kept, now)
	return true
}

// HelpText is the reply to /help. It states the read-only boundary plainly,
// because an operator who thinks "restart the api deployment" will work needs
// to find out here rather than during an incident.
const HelpText = `podsmedic — ask me about this cluster.

Commands (answered from local state, no LLM cost):
/status     what the last sweep found
/incidents  currently open incidents
/capacity   cluster headroom, as the heal validator sees it
/heals      recent automated changes, newest first
/playbook   verified remedies I can replay without asking the model
/nodes      health of the machines under your workloads
/digest     preview today's daily summary (it still sends on schedule)
/rightsize  containers whose requests do not match what they use (add "html" for a document)
/export     the playbook as a document to study (add "html" for a printable one)
/help       this message

Anything else is a question, answered by the model from what I know. For example:
- why does api/web keep restarting?
- is there room to scale the checkout deployment?
- what did you change today, and did it hold?

I answer questions; I do not take orders. Automated healing is driven by my own
sweep and its safety checks, never by a chat message.`
