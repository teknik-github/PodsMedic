// Package notify delivers a finished diagnosis to humans.
package notify

import (
	"context"
	"fmt"
	"strings"

	"github.com/peceldev/podsmedic/internal/detect"
	"github.com/peceldev/podsmedic/internal/llm"
)

// Alert is one finished diagnosis, ready to be delivered.
type Alert struct {
	Problem   detect.Problem
	Diagnosis *llm.Diagnosis
	// Heal describes an auto-heal attempt, when one was made.
	Heal *HealResult
	// CorrelatedKinds are other symptom kinds seen for the same incident, shown
	// so one alert makes clear it covers several symptoms of one failure.
	CorrelatedKinds []string
}

// correlatedLine renders the "also seen" line, or "" when there are none.
func correlatedLine(a Alert) string {
	if len(a.CorrelatedKinds) == 0 {
		return ""
	}
	return "Also seen on this pod: " + strings.Join(a.CorrelatedKinds, ", ")
}

// HealResult summarises an auto-heal attempt for the notification. It is a flat
// value type so notifiers do not depend on the heal package.
type HealResult struct {
	Attempted  bool
	Applied    bool   // false means it was a dry run
	Controller string // the workload changed (or that would be changed)
	Summary    string // what changed
	Skipped    string // why healing was skipped, if it was
	Error      string // failure detail, if the attempt failed
}

// Notifier delivers an alert to a single destination.
type Notifier interface {
	Notify(ctx context.Context, a Alert) error
	// Notice delivers a short standalone message (a heal verification or
	// rollback), not tied to a full diagnosis.
	Notice(ctx context.Context, text string) error
	// Check verifies the sink is usable (reachable, credentials valid) so a
	// misconfiguration surfaces at startup rather than on the first real alert.
	Check(ctx context.Context) error
	Name() string
}

// SinkCheck is the result of validating one sink.
type SinkCheck struct {
	Name string
	Err  error
}

// Multi fans an alert out to every configured notifier, collecting failures so
// one broken sink does not silence the others.
type Multi []Notifier

// Notify sends to all sinks and joins any errors.
func (m Multi) Notify(ctx context.Context, a Alert) error {
	var errs []string
	for _, n := range m {
		if err := n.Notify(ctx, a); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", n.Name(), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("notify failures: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Notice fans a standalone message out to all sinks.
func (m Multi) Notice(ctx context.Context, text string) error {
	var errs []string
	for _, n := range m {
		if err := n.Notice(ctx, text); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", n.Name(), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("notice failures: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Check validates every sink, joining failures.
func (m Multi) Check(ctx context.Context) error {
	var errs []string
	for _, c := range m.CheckEach(ctx) {
		if c.Err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", c.Name, c.Err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("sink check failures: %s", strings.Join(errs, "; "))
	}
	return nil
}

// CheckEach validates every sink and returns a per-sink result, so the caller
// can log and meter each one individually.
func (m Multi) CheckEach(ctx context.Context) []SinkCheck {
	out := make([]SinkCheck, 0, len(m))
	for _, n := range m {
		out = append(out, SinkCheck{Name: n.Name(), Err: n.Check(ctx)})
	}
	return out
}

// Name identifies the fan-out sink.
func (m Multi) Name() string {
	names := make([]string, 0, len(m))
	for _, n := range m {
		names = append(names, n.Name())
	}
	return strings.Join(names, "+")
}

// healLine renders the auto-heal footer as a single plain-text line, or "" when
// no heal was attempted. Notifiers wrap it in their own markup.
func healLine(h *HealResult) string {
	if h == nil || !h.Attempted {
		return ""
	}
	switch {
	case h.Error != "":
		return "Auto-heal failed: " + h.Error
	case h.Skipped != "":
		return "Auto-heal skipped: " + h.Skipped
	case h.Applied:
		return "Auto-healed " + h.Controller + ": " + h.Summary
	default:
		return "Auto-heal (dry run) " + h.Controller + ": " + h.Summary
	}
}

func severityEmoji(sev string) string {
	switch strings.ToLower(sev) {
	case "critical":
		return "🔴"
	case "warning":
		return "🟠"
	default:
		return "🔵"
	}
}
