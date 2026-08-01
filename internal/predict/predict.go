// Package predict turns live memory usage into a forward-looking signal: a
// container whose usage sits near its limit for several consecutive checks is
// about to be OOM-killed, so podsmedic can raise the limit *before* the kill
// rather than after. The predictor is pure and stateful only in the streak
// counters it keeps between sweeps, so its policy is unit-testable without a
// cluster. It emits ordinary detect.Problems (Kind MemoryPressure), which flow
// through the same incident → diagnose → heal → verify → playbook pipeline as a
// real failure.
package predict

import (
	"fmt"
	"time"

	"github.com/peceldev/podsmedic/internal/detect"
)

// Sample is one container's live memory and CPU usage against its configured
// limits. A zero limit for a resource (none set) is ignored: nothing to breach.
type Sample struct {
	Namespace  string
	Pod        string
	Container  string
	UsageBytes int64 // memory usage
	LimitBytes int64 // memory limit
	CPUMilli   int64 // CPU usage, millicores
	CPULimit   int64 // CPU limit, millicores
}

// Options tunes the trigger.
type Options struct {
	// HighRatio is the usage/limit fraction considered "near the limit".
	HighRatio float64
	// MinConsecutive is how many consecutive sweeps a container must stay at or
	// above HighRatio before it is flagged, filtering transient spikes.
	MinConsecutive int
}

// Predictor tracks per-container high-usage streaks across sweeps.
type Predictor struct {
	opts Options
	prev map[string]int
}

// New builds a predictor.
func New(o Options) *Predictor {
	return &Predictor{opts: o, prev: map[string]int{}}
}

func key(s Sample) string { return s.Namespace + "/" + s.Pod + "/" + s.Container }

// Observe consumes this sweep's samples and returns a MemoryPressure and/or
// CPUPressure problem for every container whose usage has stayed at/above
// HighRatio for at least MinConsecutive sweeps. Memory and CPU streaks are
// tracked independently. Streaks reset when usage drops or a container vanishes,
// so the state never grows unbounded.
func (p *Predictor) Observe(samples []Sample, now time.Time) []detect.Problem {
	next := make(map[string]int, len(samples))
	var problems []detect.Problem
	for _, s := range samples {
		k := key(s)
		if s.LimitBytes > 0 {
			ratio := float64(s.UsageBytes) / float64(s.LimitBytes)
			if ratio >= p.opts.HighRatio {
				mk := "mem|" + k
				streak := p.prev[mk] + 1
				next[mk] = streak
				if streak >= p.opts.MinConsecutive {
					problems = append(problems, detect.Problem{
						Namespace: s.Namespace, Pod: s.Pod, Container: s.Container,
						Kind:       detect.KindMemoryPressure,
						Message:    fmt.Sprintf("memory at %d%% of limit (%dMi/%dMi) for %d consecutive checks — OOM kill likely soon", int(ratio*100), s.UsageBytes>>20, s.LimitBytes>>20, streak),
						DetectedAt: now,
					})
				}
			}
		}
		if s.CPULimit > 0 {
			ratio := float64(s.CPUMilli) / float64(s.CPULimit)
			if ratio >= p.opts.HighRatio {
				ck := "cpu|" + k
				streak := p.prev[ck] + 1
				next[ck] = streak
				if streak >= p.opts.MinConsecutive {
					problems = append(problems, detect.Problem{
						Namespace: s.Namespace, Pod: s.Pod, Container: s.Container,
						Kind:       detect.KindCPUPressure,
						Message:    fmt.Sprintf("CPU at %d%% of limit (%dm/%dm) for %d consecutive checks — heavy throttling, likely needs more replicas", int(ratio*100), s.CPUMilli, s.CPULimit, streak),
						DetectedAt: now,
					})
				}
			}
		}
	}
	p.prev = next
	return problems
}

// Tracking is the number of containers with an active high-usage streak, for
// metrics.
func (p *Predictor) Tracking() int { return len(p.prev) }
