package notify

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Stdout prints alerts to a writer. Used for dry runs and local development.
type Stdout struct {
	w io.Writer
}

// NewStdout builds a writer-backed notifier.
func NewStdout(w io.Writer) *Stdout { return &Stdout{w: w} }

// Name identifies the sink.
func (s *Stdout) Name() string { return "stdout" }

// Notify prints the diagnosis in plain text.
func (s *Stdout) Notify(_ context.Context, a Alert) error {
	d := a.Diagnosis
	var b strings.Builder

	b.WriteString("\n" + strings.Repeat("─", 72) + "\n")
	fmt.Fprintf(&b, "%s [%s] %s\n", severityEmoji(d.Severity), strings.ToUpper(d.Severity), d.Title)
	fmt.Fprintf(&b, "%s/%s", a.Problem.Namespace, a.Problem.Pod)
	if a.Problem.Container != "" {
		fmt.Fprintf(&b, " container=%s", a.Problem.Container)
	}
	fmt.Fprintf(&b, " kind=%s confidence=%s\n\n", a.Problem.Kind, d.Confidence)

	fmt.Fprintf(&b, "%s\n\nROOT CAUSE\n  %s\n", d.Summary, d.RootCause)

	if len(d.Evidence) > 0 {
		b.WriteString("\nEVIDENCE\n")
		for _, e := range d.Evidence {
			fmt.Fprintf(&b, "  - %s\n", truncate(e, 400))
		}
	}
	if len(d.Remediation) > 0 {
		b.WriteString("\nSUGGESTED FIX\n")
		for i, step := range d.Remediation {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, step.Description)
			if step.Command != "" {
				fmt.Fprintf(&b, "     $ %s\n", step.Command)
			}
		}
	}
	if cl := correlatedLine(a); cl != "" {
		fmt.Fprintf(&b, "\n%s\n", cl)
	}
	if line := healLine(a.Heal); line != "" {
		fmt.Fprintf(&b, "\nAUTO-HEAL\n  %s\n", line)
	}

	b.WriteString(strings.Repeat("─", 72) + "\n")

	_, err := io.WriteString(s.w, b.String())
	return err
}

// Notice prints a standalone one-line message.
func (s *Stdout) Notice(_ context.Context, text string) error {
	_, err := io.WriteString(s.w, "\n🩺 "+text+"\n")
	return err
}

// Check always succeeds: a writer sink has nothing to validate.
func (s *Stdout) Check(_ context.Context) error { return nil }
