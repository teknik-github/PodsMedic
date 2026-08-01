package report

import (
	"fmt"
	"html"
	"strings"
)

// printCSS is deliberately plain. The document exists to be read and printed,
// and a browser's Print → Save as PDF is what turns it into one — so the rules
// that matter are the print ones: no orphaned headings, tables that survive a
// page break, and colours that still read in greyscale.
const printCSS = `
:root { --ink:#1a1a1a; --muted:#5b5b5b; --rule:#d8d8d8; --accent:#0b5cad; --warn:#8a4b00; }
* { box-sizing: border-box; }
body {
  font: 15px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
  color: var(--ink); max-width: 46rem; margin: 2.5rem auto; padding: 0 1.25rem;
}
h1 { font-size: 1.9rem; margin: 0 0 .25rem; }
h2 { font-size: 1.3rem; margin: 2.25rem 0 .75rem; padding-bottom: .3rem; border-bottom: 2px solid var(--rule); }
h3 { font-size: 1.05rem; margin: 1.75rem 0 .5rem; color: var(--accent); }
h4 { font-size: .95rem; margin: 1.25rem 0 .5rem; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.sub { color: var(--muted); margin: 0 0 1.5rem; font-size: .9rem; }
.lede { background: #f4f7fa; border-left: 3px solid var(--accent); padding: .85rem 1rem; margin: 1.25rem 0; }
.warn { background: #fdf6ec; border-left: 3px solid var(--warn); padding: .85rem 1rem; margin: 1.25rem 0; }
table { border-collapse: collapse; width: 100%; margin: .6rem 0 1rem; font-size: .9rem; }
th, td { text-align: left; padding: .4rem .6rem; border-bottom: 1px solid var(--rule); vertical-align: top; }
th { font-weight: 600; color: var(--muted); font-size: .8rem; text-transform: uppercase; letter-spacing: .03em; }
td.k { width: 9.5rem; color: var(--muted); }
code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: .87em;
       background: #f2f2f2; padding: .1rem .3rem; border-radius: 3px; }
.why { color: var(--muted); margin: .4rem 0 0; }
.empty { color: var(--muted); font-style: italic; }
@media print {
  body { max-width: none; margin: 0; font-size: 11pt; }
  h2 { break-before: page; }
  h2:first-of-type { break-before: avoid; }
  h3, h4 { break-after: avoid; }
  table, .lede, .warn { break-inside: avoid; }
  code { background: none; }
  a { text-decoration: none; color: inherit; }
}
`

// htmlHead opens a self-contained document. Self-contained is a requirement,
// not a preference: these are mailed around and opened offline, and a report
// that fetched a stylesheet would render as unstyled text exactly when someone
// needed to read it.
func htmlHead(title string) string {
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	fmt.Fprintf(&b, "<title>%s</title>\n<style>", html.EscapeString(title))
	b.WriteString(printCSS)
	b.WriteString("</style>\n</head>\n<body>\n")
	return b.String()
}

func renderHTML(in Input) string {
	var b strings.Builder
	remedies, replays := totals(in.Entries)
	esc := html.EscapeString

	b.WriteString(htmlHead("podsmedic playbook"))
	b.WriteString("<h1>podsmedic playbook</h1>\n")
	fmt.Fprintf(&b, "<p class=\"sub\">Generated %s · watching %s</p>\n",
		esc(in.GeneratedAt.UTC().Format("2006-01-02 15:04 UTC")), esc(orAll(in.Scope)))

	if remedies == 0 {
		b.WriteString("<p class=\"empty\">No remedies learned yet. A fix is remembered only after it has " +
			"been applied and then passed verification, so this book fills up as podsmedic actually " +
			"repairs things.</p>\n")
		writeHistoryHTML(&b, in)
		b.WriteString("</body>\n</html>\n")
		return b.String()
	}

	fmt.Fprintf(&b, "<p><strong>%d verified remed%s</strong>, serving <strong>%d replay%s</strong> with no LLM diagnosis.</p>\n",
		remedies, plural(remedies, "y", "ies"), replays, plural(replays, "", "s"))

	if !in.Applying {
		fmt.Fprintf(&b, "<div class=\"warn\">%s</div>\n", esc(stripMD(disclaimerDryRun)))
	}
	fmt.Fprintf(&b, "<h2>How to read this</h2>\n<div class=\"lede\">%s</div>\n", paragraphs(stripMD(howToRead)))

	b.WriteString("<h2>Remedies by problem</h2>\n")
	for _, g := range grouped(in.Entries) {
		fmt.Fprintf(&b, "<h3>%s — %d workload%s</h3>\n", esc(g.Kind), len(g.Entries), plural(len(g.Entries), "", "s"))
		for _, e := range g.Entries {
			what, why := decode(e)
			fmt.Fprintf(&b, "<h4>%s</h4>\n<table>\n", esc(e.Controller))
			row(&b, "Fix", "<code>"+esc(what)+"</code>")
			row(&b, "Confidence", esc(orDash(e.Confidence)))
			row(&b, "Learned", esc(stamp(e.Recorded, in.GeneratedAt)))
			row(&b, "Last verified", esc(stamp(e.LastVerified, in.GeneratedAt)))
			row(&b, "Reuse", esc(replayPhrase(e, in.GeneratedAt)))
			b.WriteString("</table>\n")
			if why != "" {
				fmt.Fprintf(&b, "<p class=\"why\"><strong>Why this fix:</strong> %s</p>\n", esc(why))
			}
		}
	}

	writeHistoryHTML(&b, in)
	b.WriteString("</body>\n</html>\n")
	return b.String()
}

func row(b *strings.Builder, key, valueHTML string) {
	fmt.Fprintf(b, "<tr><td class=\"k\">%s</td><td>%s</td></tr>\n", html.EscapeString(key), valueHTML)
}

func writeHistoryHTML(b *strings.Builder, in Input) {
	rows := historyTail(in.Events)
	if len(rows) == 0 {
		return
	}
	esc := html.EscapeString
	b.WriteString("<h2>Change history</h2>\n")
	fmt.Fprintf(b, "<p>The last %d recorded change%s, newest first. The <code>rolledback</code> rows are "+
		"the instructive ones: fixes that looked right and did not hold.</p>\n",
		len(rows), plural(len(rows), "", "s"))
	b.WriteString("<table>\n<tr><th>When</th><th>Outcome</th><th>Workload</th><th>Change</th></tr>\n")
	for _, e := range rows {
		fmt.Fprintf(b, "<tr><td>%s ago</td><td>%s</td><td>%s/%s</td><td>%s</td></tr>\n",
			esc(age(in.GeneratedAt.Sub(e.Time))), esc(e.Outcome),
			esc(e.Namespace), esc(e.Controller), esc(changeText(e)))
	}
	b.WriteString("</table>\n")
}

// stripMD removes the light Markdown emphasis the shared prose carries, since
// the HTML path escapes its text rather than rendering it.
func stripMD(s string) string { return strings.ReplaceAll(s, "**", "") }

// paragraphs turns blank-line-separated text into <p> elements, escaping each.
func paragraphs(s string) string {
	var out strings.Builder
	for _, para := range strings.Split(s, "\n\n") {
		para = strings.TrimSpace(strings.ReplaceAll(para, "\n", " "))
		if para == "" {
			continue
		}
		fmt.Fprintf(&out, "<p>%s</p>", html.EscapeString(para))
	}
	return out.String()
}
