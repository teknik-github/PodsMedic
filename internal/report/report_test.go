package report

import (
	"strings"
	"testing"
	"time"

	"github.com/peceldev/podsmedic/internal/audit"
	"github.com/peceldev/podsmedic/internal/playbook"
)

var now = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func entry(kind, controller, actionJSON string, hits int, agoHours int) playbook.Entry {
	t := now.Add(-time.Duration(agoHours) * time.Hour)
	return playbook.Entry{
		Kind: kind, Controller: controller, ActionJSON: actionJSON,
		Confidence: "high", Hits: hits,
		Recorded: t, LastVerified: t, LastHit: t,
	}
}

const oomAction = `{"kind":"patch_resources","container":"hog","memory_limit":"384Mi","reason":"The workload allocates 300MiB but the 128Mi limit triggers the OOM killer."}`

func sample() Input {
	return Input{
		Entries: []playbook.Entry{
			entry("OOMKilled", "oom-test/Deployment/pb-demo", oomAction, 3, 4),
			entry("OOMKilled", "oom-test/Deployment/other", oomAction, 0, 2),
			entry("CPUPressure", "oom-test/Deployment/scale-demo", `{"kind":"scale_replicas","replicas":5,"reason":"CPU throttled at the 200m limit."}`, 1, 6),
		},
		Events: []audit.Event{
			{Time: now.Add(-3 * time.Hour), Namespace: "oom-test", Controller: "Deployment/pb-demo",
				Action: "patch_resources", Outcome: "applied",
				Old: map[string]string{"limit.memory": "128Mi"}, New: map[string]string{"limit.memory": "384Mi"}},
			{Time: now.Add(-2 * time.Hour), Namespace: "oom-test", Controller: "Deployment/pb-demo",
				Action: "patch_resources", Outcome: "verified", Summary: "held"},
		},
		GeneratedAt: now,
		Scope:       "oom-test",
		Applying:    true,
	}
}

func TestMarkdownCarriesTheTeachingContent(t *testing.T) {
	out := string(Render(sample(), Markdown).Content)

	// The reasoning is the reason this document exists: a reader learns why the
	// fix was right, not just what it was.
	if !strings.Contains(out, "triggers the OOM killer") {
		t.Fatalf("the diagnosis's reasoning must survive into the document:\n%s", out)
	}
	if !strings.Contains(out, "384Mi") {
		t.Fatal("the concrete fix must appear")
	}
	if !strings.Contains(out, "3 verified remedies") {
		t.Fatalf("expected the remedy total, got:\n%s", out)
	}
	if !strings.Contains(out, "4 replays") {
		t.Fatalf("expected the replay total (3+0+1), got:\n%s", out)
	}
}

func TestGroupsByKindMostCommonFirst(t *testing.T) {
	out := string(Render(sample(), Markdown).Content)

	oom := strings.Index(out, "### OOMKilled")
	cpu := strings.Index(out, "### CPUPressure")
	if oom < 0 || cpu < 0 {
		t.Fatalf("both kinds should have a section:\n%s", out)
	}
	// OOMKilled has two workloads to CPUPressure's one, so it leads.
	if oom > cpu {
		t.Fatal("the failure this cluster hits most should come first")
	}
}

func TestMostReplayedRemedyLeadsItsGroup(t *testing.T) {
	out := string(Render(sample(), Markdown).Content)

	busy := strings.Index(out, "pb-demo")
	idle := strings.Index(out, "Deployment/other")
	if busy < 0 || idle < 0 {
		t.Fatal("both remedies should appear")
	}
	if busy > idle {
		t.Fatal("the remedy actually earning its keep should come first")
	}
}

func TestScaleRemedyHidesStaleReplicaCount(t *testing.T) {
	// Same rule as the chat listing: the count is re-derived from live load at
	// replay, so printing the old one would describe something that will not
	// happen.
	out := string(Render(sample(), Markdown).Content)
	if !strings.Contains(out, "re-derived from load at replay") {
		t.Fatalf("expected the scale remedy to say the count is re-derived:\n%s", out)
	}
}

func TestDryRunIsCalledOut(t *testing.T) {
	in := sample()
	in.Applying = false
	out := string(Render(in, Markdown).Content)

	if !strings.Contains(out, "never applied") {
		t.Fatalf("a reader must not think dry-run changes really happened:\n%s", out)
	}

	if strings.Contains(string(Render(sample(), Markdown).Content), "never applied") {
		t.Fatal("the warning must not appear when heals are real")
	}
}

func TestEmptyPlaybookStillProducesAUsableDocument(t *testing.T) {
	for _, f := range []Format{Markdown, HTML} {
		doc := Render(Input{GeneratedAt: now}, f)
		out := string(doc.Content)
		if !strings.Contains(out, "No remedies learned yet") {
			t.Fatalf("%s: expected an explanation rather than a blank page:\n%s", f, out)
		}
		if len(doc.Content) == 0 || doc.Filename == "" {
			t.Fatalf("%s: expected a named, non-empty document", f)
		}
	}
}

func TestHistoryIsNewestFirstAndShowsTransitions(t *testing.T) {
	out := string(Render(sample(), Markdown).Content)

	verified := strings.Index(out, "verified")
	applied := strings.Index(out, "| applied |")
	if verified < 0 || applied < 0 {
		t.Fatalf("both outcomes should appear in the history:\n%s", out)
	}
	if verified > applied {
		t.Fatal("history should be newest first")
	}
	if !strings.Contains(out, "limit.memory 128Mi→384Mi") {
		t.Fatalf("the before→after transition is the useful part:\n%s", out)
	}
}

func TestHistoryIsBounded(t *testing.T) {
	in := sample()
	in.Events = nil
	for i := 0; i < 200; i++ {
		in.Events = append(in.Events, audit.Event{
			Time:      now.Add(-time.Duration(200-i) * time.Minute),
			Namespace: "ns", Controller: "Deployment/x", Outcome: "applied", Summary: "change",
		})
	}
	out := string(Render(in, Markdown).Content)

	if got := strings.Count(out, "| applied |"); got != maxHistoryRows {
		t.Fatalf("expected the history capped at %d rows, got %d", maxHistoryRows, got)
	}
}

func TestHTMLIsSelfContainedAndPrintable(t *testing.T) {
	doc := Render(sample(), HTML)
	out := string(doc.Content)

	if doc.Filename != "podsmedic-playbook.html" || doc.MIMEType != "text/html" {
		t.Fatalf("unexpected document metadata: %+v", doc.Filename)
	}
	if !strings.HasPrefix(out, "<!doctype html>") {
		t.Fatal("expected a complete HTML document")
	}
	// Print CSS is the whole reason HTML exists here — it is what makes the
	// browser's Save as PDF produce something readable.
	if !strings.Contains(out, "@media print") {
		t.Fatal("expected print styling")
	}
	// Self-contained: no external fetches, so it works offline and in a PDF.
	for _, external := range []string{"<script", "src=\"http", "href=\"http", "@import"} {
		if strings.Contains(out, external) {
			t.Fatalf("the document must not reference anything external, found %q", external)
		}
	}
}

func TestHTMLEscapesContent(t *testing.T) {
	// The reason text comes from the model, which has read pod logs. It must not
	// be able to inject markup into a document someone opens in a browser.
	in := sample()
	in.Entries = []playbook.Entry{entry("OOMKilled", "ns/Deployment/<script>alert(1)</script>",
		`{"kind":"patch_resources","reason":"<img src=x onerror=alert(1)>"}`, 0, 1)}

	out := string(Render(in, HTML).Content)
	if strings.Contains(out, "<script>alert(1)</script>") || strings.Contains(out, "<img src=x") {
		t.Fatalf("content must be escaped:\n%s", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Fatal("expected the workload name to appear escaped")
	}
}

func TestMarkdownEscapesTablePipes(t *testing.T) {
	in := sample()
	in.Events = []audit.Event{{
		Time: now.Add(-time.Hour), Namespace: "ns", Controller: "Deployment/x",
		Outcome: "applied", Summary: "a | b | c",
	}}
	out := string(Render(in, Markdown).Content)
	if strings.Contains(out, "| a | b | c |") {
		t.Fatalf("an unescaped pipe would break the table row:\n%s", out)
	}
}

func TestParseFormat(t *testing.T) {
	cases := map[string]Format{
		"":      Markdown,
		"md":    Markdown,
		"text":  Markdown,
		"html":  HTML,
		"HTML":  HTML,
		" pdf":  HTML, // "pdf" means "the printable one"
		"print": HTML,
	}
	for in, want := range cases {
		if got := ParseFormat(in); got != want {
			t.Fatalf("ParseFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUndecodableRemedyIsReportedNotInvented(t *testing.T) {
	in := sample()
	in.Entries = []playbook.Entry{entry("OOMKilled", "ns/Deployment/x", `{broken`, 0, 1)}

	out := string(Render(in, Markdown).Content)
	if !strings.Contains(out, "could not be decoded") {
		t.Fatalf("a document that invented a remedy would be worse than none:\n%s", out)
	}
}

func TestAgeReadsAsProse(t *testing.T) {
	// Duration.String on a rounded value gives "25h0m0s", which reads badly in a
	// sentence. A document is prose, so the units are trimmed.
	cases := map[time.Duration]string{
		45 * time.Second:   "45s",
		20 * time.Minute:   "20m",
		25 * time.Hour:     "25h",
		5 * 24 * time.Hour: "5d",
	}
	for d, want := range cases {
		if got := age(d); got != want {
			t.Fatalf("age(%s) = %q, want %q", d, got, want)
		}
	}
}
