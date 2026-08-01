package report

import (
	"fmt"
	"html"
	"sort"
	"strings"
	"time"

	"github.com/teknik-github/PodsMedic/internal/rightsize"
)

// RightsizeInput is everything the rightsizing document is built from.
type RightsizeInput struct {
	Findings    []rightsize.Finding
	GeneratedAt time.Time
	Scope       string
	// Tracked and Ready say how much of the cluster the report actually covers,
	// so a short list reads as "not enough evidence yet" rather than "your
	// cluster is perfectly sized".
	Tracked int
	// MinWindow and MinSamples are the thresholds a container must clear before
	// it can appear, quoted so a reader knows how long to wait.
	MinWindow  time.Duration
	MinSamples int
}

// RenderRightsize builds the sizing document.
//
// Like the playbook report this is a pure function of its input — no cluster
// calls, no model. A sizing document that invented a number would be worse than
// no document, because someone would apply it.
func RenderRightsize(in RightsizeInput, f Format) Document {
	switch f {
	case HTML:
		return Document{
			Filename: "podsmedic-rightsizing.html",
			MIMEType: "text/html",
			Content:  []byte(renderRightsizeHTML(in)),
		}
	default:
		return Document{
			Filename: "podsmedic-rightsizing.md",
			MIMEType: "text/markdown",
			Content:  []byte(renderRightsizeMD(in)),
		}
	}
}

const rightsizeIntro = `These are **suggestions, not changes**. podsmedic will never apply them.

Automated healing only ever raises a value — that is what makes it safe to act on a model's
proposal, since the worst case is a workload with too much. Lowering a request is the opposite
bet: it moves the workload's scheduling floor and its eviction priority, so a wrong number here
gets a pod evicted under pressure it used to survive. That judgement stays with you.

Every recommendation is the measured **peak** multiplied by a headroom factor, never the mean —
a container that idles at 10m and spikes to 900m needs the 900m. Apply them in your manifests,
not by hand on the cluster, or the next deploy will undo the change.`

func rightsizeGroups(fs []rightsize.Finding) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range fs {
		if !seen[string(f.Kind)] {
			seen[string(f.Kind)] = true
			out = append(out, string(f.Kind))
		}
	}
	sort.Strings(out)
	return out
}

func byKind(fs []rightsize.Finding, kind string) []rightsize.Finding {
	var out []rightsize.Finding
	for _, f := range fs {
		if string(f.Kind) == kind {
			out = append(out, f)
		}
	}
	return out
}

// amount renders a value in the units of its resource.
func amount(r rightsize.Resource, v int64) string {
	if r == rightsize.CPU {
		return rightsize.FormatCPU(v)
	}
	return rightsize.FormatMem(v)
}

// savingsLine states what the whole report is worth, in the units an operator
// thinks in.
func savingsLine(fs []rightsize.Finding) string {
	cpu, mem := rightsize.Totals(fs)
	if cpu == 0 && mem == 0 {
		return ""
	}
	var parts []string
	if cpu > 0 {
		parts = append(parts, rightsize.FormatCPU(cpu)+" of CPU")
	}
	if mem > 0 {
		parts = append(parts, rightsize.FormatMem(mem)+" of memory")
	}
	return strings.Join(parts, " and ")
}

func kindHeading(kind string) (title, blurb string) {
	switch rightsize.Kind(kind) {
	case rightsize.KindOversized:
		return "Reserving more than they use",
			"The reservation is subtracted from every scheduling decision, so the cluster runs out of room while the nodes sit idle."
	case rightsize.KindUndersized:
		return "Using more than they reserved",
			"These run on capacity the scheduler never set aside. The node is overcommitted, and these pods are the likely eviction victims."
	case rightsize.KindNoRequests:
		return "Declaring no requests at all",
			"Scheduled as best-effort, evicted first under pressure, and counted as zero by every capacity check — including the one that decides whether a scale-up fits."
	default:
		return kind, ""
	}
}

func renderRightsizeMD(in RightsizeInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# podsmedic rightsizing\n\n")
	fmt.Fprintf(&b, "Generated %s · watching %s · %d container%s under observation\n\n",
		in.GeneratedAt.UTC().Format("2006-01-02 15:04 UTC"), orAll(in.Scope),
		in.Tracked, plural(in.Tracked, "", "s"))

	if len(in.Findings) == 0 {
		fmt.Fprintf(&b, "Nothing to suggest yet.\n\nA container is only judged after **%d samples over at least %s**, "+
			"because every workload has a quiet ten minutes and sizing it from one would be worse than saying nothing. "+
			"Leave podsmedic running and check back.\n", in.MinSamples, roundDur(in.MinWindow))
		return b.String()
	}

	if s := savingsLine(in.Findings); s != "" {
		fmt.Fprintf(&b, "Applying every reduction below would return **%s** to the cluster.\n\n", s)
	}
	b.WriteString(rightsizeIntro + "\n\n")

	for _, kind := range rightsizeGroups(in.Findings) {
		rows := byKind(in.Findings, kind)
		title, blurb := kindHeading(kind)
		fmt.Fprintf(&b, "## %s — %d finding%s\n\n", title, len(rows), plural(len(rows), "", "s"))
		if blurb != "" {
			b.WriteString(blurb + "\n\n")
		}
		b.WriteString("| Workload | Container | Resource | Requested | Peak | Mean | Suggested | Change | Evidence |\n")
		b.WriteString("|---|---|---|---:|---:|---:|---:|---:|---|\n")
		for _, f := range rows {
			fmt.Fprintf(&b, "| %s/%s | %s | %s | %s | %s | %s | %s | %s | %d samples over %s |\n",
				mdEscape(f.Namespace), mdEscape(f.Workload), mdEscape(f.Container), f.Resource,
				amount(f.Resource, f.Current), amount(f.Resource, f.Peak), amount(f.Resource, f.Mean),
				amount(f.Resource, f.Recommended), signed(f.Resource, f.Delta),
				f.Samples, roundDur(f.Window))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// signed renders a delta with its direction, because "-512Mi" and "512Mi" mean
// opposite things to whoever edits the manifest.
func signed(r rightsize.Resource, delta int64) string {
	if delta < 0 {
		return "−" + amount(r, -delta)
	}
	return "+" + amount(r, delta)
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func renderRightsizeHTML(in RightsizeInput) string {
	var b strings.Builder
	b.WriteString(htmlHead("podsmedic rightsizing"))
	fmt.Fprintf(&b, "<h1>podsmedic rightsizing</h1>\n")
	fmt.Fprintf(&b, "<p class=\"sub\">Generated %s · watching %s · %d container%s under observation</p>\n",
		html.EscapeString(in.GeneratedAt.UTC().Format("2006-01-02 15:04 UTC")),
		html.EscapeString(orAll(in.Scope)), in.Tracked, plural(in.Tracked, "", "s"))

	if len(in.Findings) == 0 {
		fmt.Fprintf(&b, "<p>Nothing to suggest yet. A container is only judged after %d samples over at least %s, "+
			"because every workload has a quiet ten minutes and sizing it from one would be worse than saying nothing.</p>\n",
			in.MinSamples, html.EscapeString(roundDur(in.MinWindow)))
		b.WriteString("</body></html>\n")
		return b.String()
	}

	if s := savingsLine(in.Findings); s != "" {
		fmt.Fprintf(&b, "<p class=\"lede\">Applying every reduction below would return <b>%s</b> to the cluster.</p>\n",
			html.EscapeString(s))
	}
	fmt.Fprintf(&b, "<div class=\"warn\">%s</div>\n", paragraphs(stripMD(rightsizeIntro)))

	for _, kind := range rightsizeGroups(in.Findings) {
		rows := byKind(in.Findings, kind)
		title, blurb := kindHeading(kind)
		fmt.Fprintf(&b, "<h2>%s — %d</h2>\n", html.EscapeString(title), len(rows))
		if blurb != "" {
			fmt.Fprintf(&b, "<p>%s</p>\n", html.EscapeString(blurb))
		}
		b.WriteString("<table><thead><tr><th>Workload<th>Container<th>Resource<th>Requested<th>Peak<th>Mean<th>Suggested<th>Change<th>Evidence</tr></thead><tbody>\n")
		for _, f := range rows {
			fmt.Fprintf(&b, "<tr><td>%s/%s<td>%s<td>%s<td>%s<td>%s<td>%s<td><b>%s</b><td>%s<td>%d samples over %s</tr>\n",
				html.EscapeString(f.Namespace), html.EscapeString(f.Workload), html.EscapeString(f.Container),
				f.Resource, amount(f.Resource, f.Current), amount(f.Resource, f.Peak), amount(f.Resource, f.Mean),
				amount(f.Resource, f.Recommended), html.EscapeString(signed(f.Resource, f.Delta)),
				f.Samples, html.EscapeString(roundDur(f.Window)))
		}
		b.WriteString("</tbody></table>\n")
	}
	b.WriteString("</body></html>\n")
	return b.String()
}
