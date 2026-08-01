// Package report turns podsmedic's learned playbook and heal history into a
// document a human can study.
//
// The audience is someone learning how this cluster actually fails: not "what
// is an OOMKill" in the abstract, but "in this cluster, pb-demo OOMs at 128Mi
// and 384Mi fixed it, verified, replayed once". That framing decides the shape —
// grouped by problem kind, with the diagnosis's own reasoning kept alongside
// each fix, and the change history at the end so a reader can see which fixes
// held and which were rolled back.
//
// Everything here is a pure function of its inputs: no cluster calls, no LLM.
// A study document that quietly invented a remedy would be worse than none.
package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/peceldev/podsmedic/internal/audit"
	"github.com/peceldev/podsmedic/internal/heal"
	"github.com/peceldev/podsmedic/internal/playbook"
)

// Format selects the rendering.
type Format string

const (
	// Markdown reads well in a terminal, a Git host, or a notes app.
	Markdown Format = "md"
	// HTML carries print CSS, so a browser's Print → Save as PDF produces a
	// usable document without podsmedic taking on a PDF dependency.
	HTML Format = "html"
)

// Input is everything the document is built from.
type Input struct {
	// Entries are the learned remedies. Order does not matter; the report sorts.
	Entries []playbook.Entry
	// Events are audit-trail entries, oldest first, as the store returns them.
	Events []audit.Event
	// GeneratedAt stamps the document. Passed in rather than read from the clock
	// so the output is reproducible in tests.
	GeneratedAt time.Time
	// Scope describes what podsmedic watches, for the header ("all namespaces",
	// or a comma-separated list).
	Scope string
	// Applying records whether heals are real or dry-run, because a reader needs
	// to know whether these changes actually happened.
	Applying bool
}

// Document is a rendered report ready to hand to a human.
type Document struct {
	Filename string
	MIMEType string
	Content  []byte
}

// maxHistoryRows bounds the change-history table. The trail holds hundreds; a
// study document wants the recent, readable tail.
const maxHistoryRows = 40

// Render builds the document.
func Render(in Input, f Format) Document {
	switch f {
	case HTML:
		return Document{
			Filename: "podsmedic-playbook.html",
			MIMEType: "text/html",
			Content:  []byte(renderHTML(in)),
		}
	default:
		return Document{
			Filename: "podsmedic-playbook.md",
			MIMEType: "text/markdown",
			Content:  []byte(renderMarkdown(in)),
		}
	}
}

// ParseFormat maps a user's word to a format, defaulting to Markdown.
func ParseFormat(s string) Format {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "html", "pdf", "print":
		// "pdf" lands here deliberately: the HTML carries print CSS, and a
		// browser turns it into a PDF better than a bundled generator would.
		return HTML
	default:
		return Markdown
	}
}

// group is one problem kind and the remedies learned for it.
type group struct {
	Kind    string
	Entries []playbook.Entry
}

// grouped organises remedies by problem kind, each group sorted most-replayed
// first, and the groups themselves by size — so the failure this cluster hits
// most is what a reader meets first.
func grouped(entries []playbook.Entry) []group {
	byKind := map[string][]playbook.Entry{}
	for _, e := range entries {
		byKind[e.Kind] = append(byKind[e.Kind], e)
	}
	out := make([]group, 0, len(byKind))
	for kind, list := range byKind {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Hits != list[j].Hits {
				return list[i].Hits > list[j].Hits
			}
			return list[i].Controller < list[j].Controller
		})
		out = append(out, group{Kind: kind, Entries: list})
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Entries) != len(out[j].Entries) {
			return len(out[i].Entries) > len(out[j].Entries)
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// totals summarises the book.
func totals(entries []playbook.Entry) (remedies, replays int) {
	for _, e := range entries {
		replays += e.Hits
	}
	return len(entries), replays
}

// decode turns a stored action into its human description and the diagnosis's
// own reasoning — the latter is the actual teaching material, since it says why
// the fix was the right one.
func decode(e playbook.Entry) (what, why string) {
	var a heal.Action
	if err := json.Unmarshal([]byte(e.ActionJSON), &a); err != nil {
		return "(stored remedy could not be decoded)", ""
	}
	return a.Describe(), strings.TrimSpace(a.Reason)
}

func stamp(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return fmt.Sprintf("%s (%s ago)", t.UTC().Format("2006-01-02 15:04 UTC"), age(now.Sub(t)))
}

// age renders a duration the way a reader wants it in prose — "25h", not
// "25h0m0s", which is what Duration.String gives for a rounded value.
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func replayPhrase(e playbook.Entry, now time.Time) string {
	switch e.Hits {
	case 0:
		return "never replayed yet"
	case 1:
		return fmt.Sprintf("replayed once, %s ago", age(now.Sub(e.LastHit)))
	default:
		return fmt.Sprintf("replayed %d times, last %s ago", e.Hits, age(now.Sub(e.LastHit)))
	}
}

// historyTail returns the most recent events, newest first.
func historyTail(events []audit.Event) []audit.Event {
	if len(events) > maxHistoryRows {
		events = events[len(events)-maxHistoryRows:]
	}
	out := make([]audit.Event, 0, len(events))
	for i := len(events) - 1; i >= 0; i-- {
		out = append(out, events[i])
	}
	return out
}

// changeText renders an audit entry's before/after pairs compactly.
func changeText(e audit.Event) string {
	keys := make([]string, 0, len(e.New))
	for k := range e.New {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		if old, ok := e.Old[k]; ok && old != "" {
			parts = append(parts, fmt.Sprintf("%s %s→%s", k, old, e.New[k]))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s", k, e.New[k]))
	}
	if len(parts) == 0 {
		return e.Summary
	}
	return strings.Join(parts, ", ")
}

const howToRead = `A remedy reaches this book only after podsmedic applied it **and** a later sweep
confirmed the workload recovered. A fix that did not hold is rolled back and evicted, so
everything listed here worked at least once on this cluster.

Each replay is a diagnosis that cost no tokens: the remembered action is re-validated against
the cluster's *current* state and re-executed, with the same safety checks, circuit breaker,
and verification as the original. A remedy that stops fitting is declined and the model takes
over again.`

const disclaimerDryRun = `**These changes were never applied.** podsmedic is running with
PODSMEDIC_HEAL_APPLY=false, so every heal below was a server-side dry run: the API server
validated it and nothing persisted.`
