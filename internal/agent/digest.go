package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/peceldev/podsmedic/internal/digest"
	"github.com/peceldev/podsmedic/internal/metrics"
	"github.com/peceldev/podsmedic/internal/rightsize"
)

// maybeDigest sends the daily summary when one is owed.
//
// It is checked at the end of every sweep rather than driven by its own timer.
// A timer would be simpler but would lose a send whenever the process restarts
// across the scheduled minute, and the whole value of a daily message is that
// its absence means something.
func (a *Agent) maybeDigest(ctx context.Context) {
	if !a.digestOn {
		return
	}
	now := time.Now()
	if !a.digestAt.Due(now, a.digestLast) {
		return
	}
	// Stamp before sending. A notifier that fails must not leave the digest due
	// on every subsequent sweep, which would turn one failed send into a flood
	// once the sink recovers.
	a.digestLast = now
	a.digestDirty = true

	text := digest.Build(a.digestInput(now))
	if err := a.notifier.Notice(ctx, text); err != nil {
		metrics.DigestsTotal.Inc("failed")
		a.log.Error("digest: send failed", "err", err)
		return
	}
	metrics.DigestsTotal.Inc("sent")
	a.log.Info("daily digest sent", "at", a.digestAt.String())
}

// digestInput gathers the picture the summary is built from. Everything here is
// already in memory — a daily message that cost an API call or a token would be
// a daily message someone turns off.
func (a *Agent) digestInput(now time.Time) digest.Input {
	counters, span := a.tally.Take(now)
	return a.buildDigest(counters, span, now)
}

// buildDigest assembles the input from live state. Split from digestInput so
// /digest can preview the summary without consuming the daily accounting.
func (a *Agent) buildDigest(c digest.Counters, span time.Duration, now time.Time) digest.Input {
	in := digest.Input{
		Counters:      c,
		Span:          span,
		GeneratedAt:   now.In(a.digestAt.Loc),
		Scope:         namespaceScope(a.cfg.Namespaces),
		IncidentsOpen: a.incidents.OpenCount(),
		AutoHeal:      a.cfg.AutoHeal,
		Applying:      a.cfg.AutoHeal && a.cfg.HealApply,
	}

	if snap := a.latestSweep(); snap != nil {
		in.Pods = snap.pods
		in.Problems = len(snap.problems)
		if snap.state != nil && snap.state.capacity != nil {
			in.Capacity = snap.state.capacity.Summary().Describe()
		}
	}

	a.sweepMu.RLock()
	in.Nodes = len(a.lastNodes)
	for _, f := range a.lastNodeFaults {
		in.NodeFaults = append(in.NodeFaults, f.Summary)
	}
	a.sweepMu.RUnlock()
	in.NodeFaults = digest.SortedFaults(in.NodeFaults)

	if a.breaker != nil {
		in.BreakersOpen = a.breaker.OpenCount(now)
	}
	if a.playbook != nil {
		in.PlaybookEntries = a.playbook.Count()
		in.Quarantined = a.playbook.QuarantineCount(now)
	}
	if a.rightsize != nil {
		findings := a.rightsize.Findings(a.rightsizeOptions(), now)
		cpu, mem := rightsize.Totals(findings)
		in.RightsizeFindings = len(findings)
		in.RightsizeCPU = rightsize.FormatCPU(cpu)
		in.RightsizeMemory = rightsize.FormatMem(mem)
	}
	return in
}

// digestPreview answers /digest: the same summary the scheduled one would
// produce, without resetting the counters it is built from.
func (a *Agent) digestPreview() string {
	if !a.digestOn {
		return "The daily digest is off. Set PODSMEDIC_DIGEST_AT=09:00 and I will send a summary once a day — including on quiet days, so its absence tells you something."
	}
	now := time.Now()
	c, span := a.tally.Peek(now)
	return digest.Build(a.buildDigest(c, span, now)) +
		"\n(Preview — the scheduled digest still goes out at " + a.digestAt.String() + ".)"
}

func namespaceScope(ns []string) string {
	if len(ns) == 0 {
		return ""
	}
	out := ns[0]
	for _, n := range ns[1:] {
		out += ", " + n
	}
	return out
}

// digestLocation resolves the configured timezone. An unknown zone falls back
// to the host's rather than to UTC: a wrong-but-local time is a smaller
// surprise than a summary arriving in the middle of the night.
func digestLocation(name string, log *slog.Logger) *time.Location {
	if name == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		log.Warn("digest: unknown timezone, using the host's", "tz", name, "err", err)
		return time.Local
	}
	return loc
}
