package chat

import (
	"context"
	"testing"
	"time"
)

func TestParseCommands(t *testing.T) {
	cases := []struct {
		in       string
		wantCmd  Command
		wantText string
	}{
		{"/status", CmdStatus, ""},
		{"  /status  ", CmdStatus, ""},
		{"/STATUS", CmdStatus, ""},
		// Telegram appends the bot name to commands sent in a group chat.
		{"/status@podsmedic_bot", CmdStatus, ""},
		{"/incidents@podsmedic_bot now", CmdIncidents, "now"},
		{"/help", CmdHelp, ""},
		{"/start", CmdHelp, ""},
		{"/capacity", CmdCapacity, ""},
		{"/heals", CmdHeals, ""},
		{"/audit", CmdHeals, ""},
		{"/playbook", CmdPlaybook, ""},
		{"/playbooks", CmdPlaybook, ""},
		{"/remedies", CmdPlaybook, ""},
		{"/playbook@podsmedic_bot", CmdPlaybook, ""},
		{"/export", CmdExport, ""},
		{"/export html", CmdExport, "html"},
		{"/pdf", CmdExport, ""},
		{"/report", CmdExport, ""},
		{"/rightsize", CmdRightsize, ""},
		{"/rightsize html", CmdRightsize, "html"},
		{"/sizing", CmdRightsize, ""},
		{"/nodes", CmdNodes, ""},
		{"/node", CmdNodes, ""},
		{"/digest", CmdDigest, ""},
		{"/pods", CmdPods, ""},
		{"/pods longhorn-system", CmdPods, "longhorn-system"},
		{"/ps all", CmdPods, "all"},
		{"/summary", CmdDigest, ""},
		{"/ask why is web crashing?", CmdAsk, "why is web crashing?"},
		// Free text is a question.
		{"why is api/web restarting?", CmdAsk, "why is api/web restarting?"},
		// An unknown command is answered as a question rather than rejected: a
		// typo should not produce a scolding.
		{"/statuss", CmdAsk, "/statuss"},
		{"", CmdAsk, ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			cmd, text := Parse(tc.in)
			if cmd != tc.wantCmd || text != tc.wantText {
				t.Fatalf("got (%q, %q), want (%q, %q)", cmd, text, tc.wantCmd, tc.wantText)
			}
		})
	}
}

func TestAllowlistIsClosedByDefault(t *testing.T) {
	// The security property that matters: an empty allowlist serves nobody, so a
	// misconfiguration cannot silently expose cluster state.
	var empty Allowlist
	if empty.Permits(12345) {
		t.Fatal("an empty allowlist must permit nobody")
	}
	if NewAllowlist(nil).Permits(12345) {
		t.Fatal("an allowlist built from no IDs must permit nobody")
	}

	a := NewAllowlist([]int64{111, 222})
	if !a.Permits(111) || !a.Permits(222) {
		t.Fatal("listed chats must be permitted")
	}
	if a.Permits(333) || a.Permits(-111) {
		t.Fatal("unlisted chats must be refused")
	}
}

func TestLimiterCapsPerChat(t *testing.T) {
	l := NewLimiter(3)
	now := time.Now()

	for i := 0; i < 3; i++ {
		if !l.Allow(1, now) {
			t.Fatalf("question %d should be allowed", i+1)
		}
	}
	if l.Allow(1, now) {
		t.Fatal("the fourth question in a minute must be refused")
	}
	// A different chat has its own budget.
	if !l.Allow(2, now) {
		t.Fatal("a separate chat must not inherit another's limit")
	}
	// The window slides.
	if !l.Allow(1, now.Add(61*time.Second)) {
		t.Fatal("the budget must refresh after the window")
	}
}

func TestLimiterDisabledWhenNonPositive(t *testing.T) {
	l := NewLimiter(0)
	now := time.Now()
	for i := 0; i < 50; i++ {
		if !l.Allow(1, now) {
			t.Fatal("a non-positive rate must disable limiting")
		}
	}
	var nilLimiter *Limiter
	if !nilLimiter.Allow(1, now) {
		t.Fatal("a nil limiter must not block")
	}
}

func TestNewRejectsConfigurationThatServesNobody(t *testing.T) {
	stub := answerFunc(func() (Reply, error) { return Say(""), nil })
	cases := []struct {
		name string
		opts Options
	}{
		{"no token", Options{Allowed: NewAllowlist([]int64{1}), Answerer: stub}},
		{"no allowed chats", Options{Token: "t", Answerer: stub}},
		{"no answerer", Options{Token: "t", Allowed: NewAllowlist([]int64{1})}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.opts); err == nil {
				t.Fatal("expected an error rather than a listener that ignores everything")
			}
		})
	}
}

func TestSplitMessageKeepsChunksUnderLimit(t *testing.T) {
	var long string
	for i := 0; i < 500; i++ {
		long += "a line of an answer that is reasonably long\n"
	}
	chunks := splitMessage(long, 4080)
	if len(chunks) < 2 {
		t.Fatalf("expected the reply to be split, got %d chunk(s)", len(chunks))
	}
	var rejoined string
	for _, c := range chunks {
		if len(c) > 4080 {
			t.Fatalf("chunk of %d bytes exceeds the limit", len(c))
		}
		rejoined += c
	}
	if rejoined != long {
		t.Fatal("splitting must not lose or reorder content")
	}
}

// answerFunc adapts a function to the Answerer interface for tests.
type answerFunc func() (Reply, error)

func (f answerFunc) Answer(_ context.Context, _ Question) (Reply, error) { return f() }
