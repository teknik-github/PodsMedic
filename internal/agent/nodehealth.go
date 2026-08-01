package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/teknik-github/PodsMedic/internal/live"
	"github.com/teknik-github/PodsMedic/internal/metrics"
	"github.com/teknik-github/PodsMedic/internal/nodes"

	corev1 "k8s.io/api/core/v1"
)

// checkNodes reports node-level faults to a human and to the live view.
//
// It never heals. podsmedic has no write permission on nodes and must not
// acquire any — cordoning or draining is a decision whose blast radius dwarfs
// patching one workload — so the entire feature ends at a notification.
//
// Failures here are logged and dropped like every other per-sweep read: losing
// node visibility for a cycle must not stop podsmedic watching pods.
func (a *Agent) checkNodes(ctx context.Context, pods []corev1.Pod) {
	if !a.cfg.NodeHealth {
		return
	}
	states, err := a.kube.NodeStates(ctx, pods)
	if err != nil {
		a.log.Warn("node health unreadable — node-level faults will go unreported this sweep", "err", err)
		return
	}

	now := time.Now()
	findings := nodes.Check(states, nodes.Options{
		Grace:          a.cfg.NodeGrace,
		ReportCordoned: a.cfg.NodeReportCordoned,
	}, now)
	a.publishNodes(states, findings)
	metrics.NodesWatched.Set(float64(len(states)))
	metrics.NodeFaults.Set(float64(len(findings)))
	if len(findings) == 0 {
		return
	}

	for _, f := range findings {
		// A node stays NotReady for as long as it stays NotReady. Without the
		// cooldown this would be the same message every sweep, which is how a
		// real alert gets muted.
		if !a.nodeSeen.ShouldAlert(f.Key()) {
			continue
		}
		metrics.NodeFaultsTotal.Inc(string(f.Kind))
		a.tally.NodeFault()
		a.log.Warn("node fault", "node", f.Node, "kind", string(f.Kind),
			"severity", f.Severity, "pods", f.Pods, "detail", f.Summary)

		// The live view groups by workload; a node has none, so it is keyed by
		// its own name and shows as a wire from the globe to nothing in
		// particular — which is the honest picture of a fault that belongs to no
		// single workload.
		a.live.Publish(live.Event{
			At: now, Class: live.ClassProblem, Workload: "node/" + f.Node,
			Pod: f.Node, Reason: string(f.Kind), Detail: f.Summary,
		})
		if err := a.notifier.Notice(ctx, f.Summary); err != nil {
			a.log.Error("node health: notice failed", "err", err)
		}
	}
	a.nodeSeen.Sweep()
}

// publishNodes makes the sweep's node picture available to the chat path. It
// crosses sweeps, so unlike sweepState it needs the lock.
func (a *Agent) publishNodes(states []nodes.State, findings []nodes.Finding) {
	a.sweepMu.Lock()
	defer a.sweepMu.Unlock()
	a.lastNodes = states
	a.lastNodeFaults = findings
}

// nodesText answers /nodes from the last sweep, costing no tokens.
func (a *Agent) nodesText() string {
	if !a.cfg.NodeHealth {
		return "Node health checks are off. Set PODSMEDIC_NODE_HEALTH=true and I will tell you when a node goes NotReady or runs short of disk, memory, or PIDs — before its pods start failing."
	}
	a.sweepMu.RLock()
	states, faults := a.lastNodes, a.lastNodeFaults
	a.sweepMu.RUnlock()

	if len(states) == 0 {
		return "No node information yet — the first sweep has not finished, or I cannot list nodes (check RBAC)."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d node(s), %d fault(s).\n\n", len(states), len(faults))
	for _, f := range faults {
		fmt.Fprintf(&b, "• [%s] %s\n", f.Severity, f.Summary)
	}
	if len(faults) > 0 {
		b.WriteString("\n")
	}
	for _, s := range states {
		status := "ready"
		for _, c := range s.Conditions {
			if c.Type == "Ready" && !c.Active {
				status = "NOT READY"
			}
		}
		if s.Unschedulable {
			status += ", cordoned"
		}
		fmt.Fprintf(&b, "%s — %s, %d pod(s), kubelet %s\n", s.Name, status, s.Pods, orUnknown(s.KubeletVersion))
	}
	b.WriteString("\nI only report on nodes. Cordoning or draining one is a decision with a far larger blast radius than patching a workload, and podsmedic has no write permission on nodes by design.")
	return b.String()
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
