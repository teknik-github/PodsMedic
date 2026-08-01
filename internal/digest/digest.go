// Package digest builds the once-a-day summary of what podsmedic did.
//
// Every other message podsmedic sends is triggered by something going wrong,
// which leaves a gap: on a healthy cluster it is silent, and silence is
// ambiguous. "Nothing is broken" and "the agent died three days ago" look
// identical from the outside. The digest closes that gap — it arrives whether or
// not anything happened, and if it stops arriving, that is itself the signal.
//
// Both halves are pure. Schedule decides when, from a clock reading rather than
// a timer, so a missed window (the process was down at 09:00) is caught on the
// next check rather than skipped for a day. Build renders the text from counters
// the agent kept. Neither calls the cluster or the model: a daily summary that
// cost tokens would be a daily summary someone turns off.
package digest

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Schedule is a daily send time.
type Schedule struct {
	Hour   int
	Minute int
	// Loc is the timezone the time is read in. Nil means UTC, which is a poor
	// default for a human-facing daily message — the agent passes the configured
	// zone.
	Loc *time.Location
}

// ParseSchedule reads "HH:MM" in the given location. An empty string disables
// the digest, which is reported as ok=false rather than as an error: "off" is a
// valid configuration, not a mistake.
func ParseSchedule(at string, loc *time.Location) (Schedule, bool, error) {
	at = strings.TrimSpace(at)
	if at == "" {
		return Schedule{}, false, nil
	}
	var h, m int
	if _, err := fmt.Sscanf(at, "%d:%d", &h, &m); err != nil {
		return Schedule{}, false, fmt.Errorf("want HH:MM, got %q", at)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return Schedule{}, false, fmt.Errorf("%q is not a time of day", at)
	}
	if loc == nil {
		loc = time.UTC
	}
	return Schedule{Hour: h, Minute: m, Loc: loc}, true, nil
}

// String renders the schedule for a log line.
func (s Schedule) String() string {
	return fmt.Sprintf("%02d:%02d %s", s.Hour, s.Minute, s.location())
}

func (s Schedule) location() *time.Location {
	if s.Loc == nil {
		return time.UTC
	}
	return s.Loc
}

// LastSlot is the most recent scheduled instant at or before now.
//
// Working backwards from now rather than forwards from the last send is what
// makes a missed window recoverable: if the process was down at 09:00 and comes
// up at 11:00, the 09:00 slot is still the most recent one and the digest goes
// out late rather than not at all.
func (s Schedule) LastSlot(now time.Time) time.Time {
	local := now.In(s.location())
	slot := time.Date(local.Year(), local.Month(), local.Day(), s.Hour, s.Minute, 0, 0, s.location())
	if slot.After(local) {
		slot = slot.AddDate(0, 0, -1)
	}
	return slot
}

// Due reports whether a digest is owed. lastSent is the previous send; a caller
// with no record should seed it with the process start time, so a fresh install
// does not immediately fire a summary of a day it did not watch.
func (s Schedule) Due(now, lastSent time.Time) bool {
	return lastSent.Before(s.LastSlot(now))
}

// Counters is what happened since the last digest.
type Counters struct {
	Sweeps            int `json:"sweeps"`
	IncidentsOpened   int `json:"incidentsOpened"`
	IncidentsResolved int `json:"incidentsResolved"`

	HealsApplied  int `json:"healsApplied"`
	HealsDryRun   int `json:"healsDryRun"`
	HealsDeclined int `json:"healsDeclined"`
	HealsFailed   int `json:"healsFailed"`

	Verified   int `json:"verified"`
	RolledBack int `json:"rolledBack"`

	PlaybookHits        int `json:"playbookHits"`
	PlaybookLearned     int `json:"playbookLearned"`
	PlaybookRetired     int `json:"playbookRetired"`
	PlaybookQuarantined int `json:"playbookQuarantined"`

	NodeFaults int `json:"nodeFaults"`

	LLMCalls   int     `json:"llmCalls"`
	LLMCostUSD float64 `json:"llmCostUSD"`
}

// Quiet reports whether nothing worth narrating happened. A quiet day still gets
// a digest — that is the point of it — but it gets a shorter one.
func (c Counters) Quiet() bool {
	return c.IncidentsOpened == 0 && c.HealsApplied == 0 && c.HealsDryRun == 0 &&
		c.HealsDeclined == 0 && c.HealsFailed == 0 && c.RolledBack == 0 && c.NodeFaults == 0
}

// Tally accumulates counters across sweeps. It is written from the sweep loop
// and read when the digest fires, so it is guarded.
type Tally struct {
	mu sync.Mutex
	c  Counters
	// since is when this accounting period began, so the digest can say what
	// span it covers rather than assuming a full day.
	since time.Time
}

// NewTally starts an accounting period.
func NewTally(start time.Time) *Tally { return &Tally{since: start} }

func (t *Tally) add(f func(*Counters)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f(&t.c)
}

// The recording methods. Deliberately one per event rather than a generic
// counter map: a typo'd string key would silently lose a day's accounting.
func (t *Tally) Sweep()            { t.add(func(c *Counters) { c.Sweeps++ }) }
func (t *Tally) IncidentOpened()   { t.add(func(c *Counters) { c.IncidentsOpened++ }) }
func (t *Tally) IncidentResolved() { t.add(func(c *Counters) { c.IncidentsResolved++ }) }
func (t *Tally) NodeFault()        { t.add(func(c *Counters) { c.NodeFaults++ }) }
func (t *Tally) PlaybookHit()      { t.add(func(c *Counters) { c.PlaybookHits++ }) }
func (t *Tally) PlaybookLearned()  { t.add(func(c *Counters) { c.PlaybookLearned++ }) }
func (t *Tally) PlaybookRetired(n int) {
	t.add(func(c *Counters) { c.PlaybookRetired += n })
}
func (t *Tally) PlaybookQuarantined() { t.add(func(c *Counters) { c.PlaybookQuarantined++ }) }

// Heal records one heal attempt by outcome, matching the values the heals
// metric uses.
func (t *Tally) Heal(outcome string) {
	t.add(func(c *Counters) {
		switch outcome {
		case "applied":
			c.HealsApplied++
		case "dryrun":
			c.HealsDryRun++
		case "skipped":
			c.HealsDeclined++
		case "failed":
			c.HealsFailed++
		}
	})
}

// Verification records a verify verdict.
func (t *Tally) Verification(verdict string) {
	t.add(func(c *Counters) {
		switch verdict {
		case "verified":
			c.Verified++
		case "rolledback":
			c.RolledBack++
		}
	})
}

// LLM records one diagnosis call and its estimated cost.
func (t *Tally) LLM(costUSD float64) {
	t.add(func(c *Counters) { c.LLMCalls++; c.LLMCostUSD += costUSD })
}

// Take returns the accumulated counters and the span they cover, and starts a
// fresh period. Reading and resetting together is what stops an event that
// lands mid-digest being counted twice or lost.
func (t *Tally) Take(now time.Time) (Counters, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, span := t.c, now.Sub(t.since)
	t.c, t.since = Counters{}, now
	return c, span
}

// Peek reads the counters without resetting, for a /digest command that must
// not disturb the daily accounting.
func (t *Tally) Peek(now time.Time) (Counters, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.c, now.Sub(t.since)
}

// Input is everything the digest text is built from.
type Input struct {
	Counters Counters
	// Span is how long this digest covers. Usually a day, but shorter after a
	// restart, and saying so stops a half-day of numbers reading as a full one.
	Span        time.Duration
	GeneratedAt time.Time
	Scope       string

	// Live state at the moment the digest is built.
	Pods          int
	Problems      int
	IncidentsOpen int
	Nodes         int
	NodeFaults    []string
	// Capacity is the headroom summary, already rendered by the capacity package.
	Capacity string
	// BreakersOpen and Quarantined are the standing brakes: workloads podsmedic
	// has given up on for now. They matter more than any single day's numbers,
	// because nothing else will remind anyone they exist.
	BreakersOpen int
	Quarantined  int

	// Rightsizing, when enabled.
	RightsizeFindings int
	RightsizeCPU      string
	RightsizeMemory   string

	PlaybookEntries int

	// Applying says whether heals were real. A digest of dry runs that read as
	// real changes would be actively misleading.
	Applying bool
	AutoHeal bool
}

// Build renders the digest.
func Build(in Input) string {
	var b strings.Builder

	fmt.Fprintf(&b, "podsmedic daily digest — %s\n", in.GeneratedAt.Format("Mon 2 Jan 2006, 15:04 MST"))
	fmt.Fprintf(&b, "Covering the last %s · watching %s\n\n", span(in.Span), scope(in.Scope))

	c := in.Counters
	fmt.Fprintf(&b, "NOW: %d pods, %d problem(s), %d incident(s) open, %d node(s).\n",
		in.Pods, in.Problems, in.IncidentsOpen, in.Nodes)
	if in.Capacity != "" {
		fmt.Fprintf(&b, "Headroom: %s\n", in.Capacity)
	}

	if c.Quiet() {
		// "Nothing failed" printed directly beneath "5 problems" reads as a
		// contradiction and costs the whole digest its credibility. Nothing *new*
		// failing is a different claim from nothing being wrong, and when
		// problems are standing the digest has to make that distinction itself.
		if in.Problems > 0 || in.IncidentsOpen > 0 {
			fmt.Fprintf(&b, "\nNo change: %d sweep(s), nothing new failed and nothing needed changing — "+
				"but the %d problem(s) above are still open and are not being fixed automatically.\n",
				c.Sweeps, in.Problems)
		} else {
			fmt.Fprintf(&b, "\nA quiet period: %d sweep(s), nothing failed and nothing needed changing.\n", c.Sweeps)
		}
	} else {
		fmt.Fprintf(&b, "\nHAPPENED: %d sweep(s), %d incident(s) opened, %d resolved.\n",
			c.Sweeps, c.IncidentsOpened, c.IncidentsResolved)
		writeHeals(&b, c, in)
	}

	writeStandingBrakes(&b, in)

	if len(in.NodeFaults) > 0 {
		fmt.Fprintf(&b, "\nNODES: %d fault(s) reported.\n", len(in.NodeFaults))
		for _, f := range in.NodeFaults {
			fmt.Fprintf(&b, "  • %s\n", f)
		}
	}

	if in.RightsizeFindings > 0 {
		fmt.Fprintf(&b, "\nSIZING: %d suggestion(s) — up to %s CPU and %s memory could be returned to the cluster. "+
			"Suggestions only; run /rightsize html for the detail.\n",
			in.RightsizeFindings, in.RightsizeCPU, in.RightsizeMemory)
	}

	if c.LLMCalls > 0 {
		fmt.Fprintf(&b, "\nMODEL: %d diagnosis call(s)", c.LLMCalls)
		if c.LLMCostUSD > 0 {
			fmt.Fprintf(&b, ", about $%.2f", c.LLMCostUSD)
		}
		if c.PlaybookHits > 0 {
			fmt.Fprintf(&b, ". %d further fix(es) were replayed from the playbook at no cost", c.PlaybookHits)
		}
		b.WriteString(".\n")
	}

	if !in.AutoHeal {
		b.WriteString("\nAuto-heal is off: I diagnose and alert, I do not change anything.\n")
	} else if !in.Applying {
		b.WriteString("\nAuto-heal is in dry-run: every change above was validated by the API server and discarded.\n")
	}
	return b.String()
}

func writeHeals(b *strings.Builder, c Counters, in Input) {
	if c.HealsApplied+c.HealsDryRun+c.HealsDeclined+c.HealsFailed == 0 {
		return
	}
	verb := "applied"
	if !in.Applying {
		verb = "dry-run"
	}
	fmt.Fprintf(b, "HEALS: %d %s, %d declined by the validator", max0(c.HealsApplied, c.HealsDryRun), verb, c.HealsDeclined)
	if c.HealsFailed > 0 {
		fmt.Fprintf(b, ", %d failed", c.HealsFailed)
	}
	b.WriteString(".\n")

	if c.Verified+c.RolledBack > 0 {
		fmt.Fprintf(b, "Of those checked afterwards: %d held, %d were rolled back.\n", c.Verified, c.RolledBack)
	}
	if c.RolledBack > 0 {
		b.WriteString("A rollback means the change did not fix the workload and its prior values were restored — worth a look.\n")
	}
	// Declines are the normal case, not a fault, and saying so stops a healthy
	// number reading as a problem.
	if c.HealsDeclined > 0 && c.HealsApplied == 0 && c.HealsDryRun == 0 {
		b.WriteString("Nothing was applied: every proposal was refused by the safety checks, which is the expected outcome when a failure is not something a resource change can fix.\n")
	}
	if c.PlaybookLearned > 0 {
		fmt.Fprintf(b, "Learned %d new remed%s into the playbook.\n", c.PlaybookLearned, plural(c.PlaybookLearned, "y", "ies"))
	}
	if c.PlaybookRetired > 0 {
		fmt.Fprintf(b, "Retired %d remed%s that had gone too long without confirmation.\n",
			c.PlaybookRetired, plural(c.PlaybookRetired, "y", "ies"))
	}
}

// writeStandingBrakes reports the things podsmedic has stopped doing. These
// persist across days and nothing else will surface them, so a workload that
// tripped its breaker a week ago would otherwise sit forgotten and unhealed.
func writeStandingBrakes(b *strings.Builder, in Input) {
	if in.BreakersOpen == 0 && in.Quarantined == 0 {
		return
	}
	b.WriteString("\nSTANDING BRAKES — healing is suspended here until someone looks:\n")
	if in.BreakersOpen > 0 {
		fmt.Fprintf(b, "  • %d workload(s) with an open circuit breaker (too many heals or rollbacks).\n", in.BreakersOpen)
	}
	if in.Quarantined > 0 {
		fmt.Fprintf(b, "  • %d workload+problem pair(s) quarantined from the playbook — repeated fixes there did not hold.\n", in.Quarantined)
	}
}

func max0(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func scope(s string) string {
	if strings.TrimSpace(s) == "" {
		return "all namespaces"
	}
	return s
}

func span(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "moment"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 36*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

// SortedFaults renders node findings deterministically, so two digests built
// from the same state read the same.
func SortedFaults(summaries []string) []string {
	out := append([]string(nil), summaries...)
	sort.Strings(out)
	return out
}
