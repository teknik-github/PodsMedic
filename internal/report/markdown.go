package report

import (
	"fmt"
	"strings"
)

func renderMarkdown(in Input) string {
	var b strings.Builder
	remedies, replays := totals(in.Entries)

	fmt.Fprintf(&b, "# podsmedic playbook\n\n")
	fmt.Fprintf(&b, "Generated %s · watching %s\n\n", in.GeneratedAt.UTC().Format("2006-01-02 15:04 UTC"), orAll(in.Scope))

	if remedies == 0 {
		b.WriteString("No remedies learned yet.\n\n")
		b.WriteString("A fix is remembered only after it has been applied and then passed verification, ")
		b.WriteString("so this book fills up as podsmedic actually repairs things.\n")
		writeHistoryMD(&b, in)
		return b.String()
	}

	fmt.Fprintf(&b, "**%d verified remed%s**, serving **%d replay%s** with no LLM diagnosis.\n\n",
		remedies, plural(remedies, "y", "ies"), replays, plural(replays, "", "s"))

	if !in.Applying {
		b.WriteString("> " + strings.ReplaceAll(disclaimerDryRun, "\n", "\n> ") + "\n\n")
	}

	b.WriteString("## How to read this\n\n")
	b.WriteString(howToRead + "\n\n")

	b.WriteString("## Remedies by problem\n\n")
	for _, g := range grouped(in.Entries) {
		fmt.Fprintf(&b, "### %s — %d workload%s\n\n", g.Kind, len(g.Entries), plural(len(g.Entries), "", "s"))
		for _, e := range g.Entries {
			what, why := decode(e)
			fmt.Fprintf(&b, "#### %s\n\n", e.Controller)
			b.WriteString("| | |\n|---|---|\n")
			fmt.Fprintf(&b, "| Fix | `%s` |\n", what)
			fmt.Fprintf(&b, "| Confidence | %s |\n", orDash(e.Confidence))
			fmt.Fprintf(&b, "| Learned | %s |\n", stamp(e.Recorded, in.GeneratedAt))
			fmt.Fprintf(&b, "| Last verified | %s |\n", stamp(e.LastVerified, in.GeneratedAt))
			fmt.Fprintf(&b, "| Reuse | %s |\n\n", replayPhrase(e, in.GeneratedAt))
			if why != "" {
				fmt.Fprintf(&b, "**Why this fix:** %s\n\n", why)
			}
		}
	}

	writeHistoryMD(&b, in)
	return b.String()
}

func writeHistoryMD(b *strings.Builder, in Input) {
	rows := historyTail(in.Events)
	if len(rows) == 0 {
		return
	}
	b.WriteString("## Change history\n\n")
	fmt.Fprintf(b, "The last %d recorded change%s, newest first. Outcomes are `applied`, `dryrun`, "+
		"`verified`, `rolledback`, and `rollback_failed` — the rolled-back ones are the instructive "+
		"ones, since they are fixes that looked right and did not hold.\n\n",
		len(rows), plural(len(rows), "", "s"))

	b.WriteString("| When | Outcome | Workload | Change |\n|---|---|---|---|\n")
	for _, e := range rows {
		fmt.Fprintf(b, "| %s ago | %s | %s/%s | %s |\n",
			age(in.GeneratedAt.Sub(e.Time)), e.Outcome, e.Namespace, e.Controller, mdEscape(changeText(e)))
	}
	b.WriteString("\n")
}

// mdEscape neutralises the pipe, which would otherwise break a table row.
func mdEscape(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func orAll(scope string) string {
	if strings.TrimSpace(scope) == "" {
		return "all namespaces"
	}
	return scope
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
