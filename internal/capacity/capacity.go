// Package capacity models how much room a cluster actually has left, so a
// scale-up can be bounded by measured headroom instead of a number someone
// typed into an env var.
//
// It answers two questions, both purely:
//
//   - How many more pods of a given size can the cluster still place?
//     (Snapshot.FitAdditional — bin-packed per node, because a pod must fit on
//     one node, and bounded by pod-count slots as well as CPU/memory, because
//     pod count is itself a resource that degrades a kubelet and the API server
//     long before CPU runs out.)
//   - How many replicas does this workload's measured load call for?
//     (TargetReplicas — the standard utilisation ratio.)
//
// Everything here is arithmetic over a value snapshot: no cluster calls, so the
// policy is unit-testable and the numbers a heal is bounded by are the same
// numbers shown in the alert.
package capacity

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// ErrNoHeadroom means the cluster cannot place even one more pod of this size.
var ErrNoHeadroom = errors.New("no schedulable headroom")

// ErrNotComputable means there is not enough evidence to derive a target, so
// the caller must refuse rather than guess.
var ErrNotComputable = errors.New("replica target not computable")

// Requests is one pod's effective resource request — what the scheduler has to
// find room for. CPU in millicores, memory in bytes.
type Requests struct {
	CPUMilli int64 `json:"cpuMilli,omitempty"`
	MemBytes int64 `json:"memBytes,omitempty"`
}

// IsZero reports a BestEffort pod: nothing requested, so only pod-count slots
// bound how many will fit.
func (r Requests) IsZero() bool { return r.CPUMilli <= 0 && r.MemBytes <= 0 }

func (r Requests) String() string {
	if r.IsZero() {
		return "no requests (BestEffort)"
	}
	return fmt.Sprintf("%dm CPU / %dMi memory", r.CPUMilli, r.MemBytes>>20)
}

// Node is one node's allocatable capacity and what is already committed to the
// pods bound to it. Used* is the sum of scheduled pod *requests*, not live
// usage: requests are what the scheduler reserves, so they are what determines
// whether the next pod fits.
type Node struct {
	Name          string
	AllocCPUMilli int64
	AllocMemBytes int64
	AllocPods     int64
	UsedCPUMilli  int64
	UsedMemBytes  int64
	UsedPods      int64
	// Schedulable is false for a cordoned or NotReady node, whose free space is
	// not really free. Taints and affinity are not modelled — see the package
	// note on FitAdditional being necessary, not sufficient.
	Schedulable bool
}

// Snapshot is the cluster's capacity for one sweep.
//
// Reserve is carried here rather than passed per call so that the headroom the
// validator enforces and the headroom described to the model and the operator
// are always the same number.
type Snapshot struct {
	Nodes []Node
	// Reserve is the fraction of each node's allocatable held back and never
	// offered to a heal (0.2 = keep 20% free). This is the guard against
	// healing a cluster into unresponsiveness: bursting above requests, DaemonSet
	// churn, and rescheduling all need room that a purely requests-based
	// calculation would happily hand out.
	Reserve float64
}

// headroom is one node's remaining space after the reserve.
type headroom struct{ cpuMilli, memBytes, pods int64 }

func (s Snapshot) reserve() float64 {
	switch {
	case s.Reserve < 0:
		return 0
	case s.Reserve > 0.9:
		return 0.9 // never hold back everything; that would disable scaling silently
	default:
		return s.Reserve
	}
}

func (s Snapshot) headroomOn(n Node) headroom {
	r := s.reserve()
	return headroom{
		cpuMilli: nonNegative(n.AllocCPUMilli - n.UsedCPUMilli - int64(float64(n.AllocCPUMilli)*r)),
		memBytes: nonNegative(n.AllocMemBytes - n.UsedMemBytes - int64(float64(n.AllocMemBytes)*r)),
		pods:     nonNegative(n.AllocPods - n.UsedPods - int64(float64(n.AllocPods)*r)),
	}
}

// FitAdditional reports how many more pods of the given size the cluster can
// still place.
//
// It bin-packs per node — a pod must fit entirely on one node, so summing free
// CPU across the cluster and dividing would overcount — and takes the tightest
// of the CPU, memory, and pod-slot bounds on each node.
//
// This is a necessary, not sufficient, condition: taints, affinity, topology
// spread, and PVC zone constraints can all still leave a pod Pending. It exists
// to make the outright-impossible case impossible, not to reimplement the
// scheduler.
func (s Snapshot) FitAdditional(req Requests) int64 {
	var total int64
	for _, n := range s.Nodes {
		if !n.Schedulable {
			continue
		}
		free := s.headroomOn(n)
		slots := free.pods
		if req.CPUMilli > 0 {
			slots = minInt64(slots, free.cpuMilli/req.CPUMilli)
		}
		if req.MemBytes > 0 {
			slots = minInt64(slots, free.memBytes/req.MemBytes)
		}
		if slots > 0 {
			total += slots
		}
	}
	return total
}

// Fits reports whether a single pod of this size can be placed on some node,
// with the reason it cannot when it cannot. Used to reject a resource *request*
// raise that would make the workload unschedulable.
func (s Snapshot) Fits(req Requests) error {
	if s.FitAdditional(req) > 0 {
		return nil
	}
	return fmt.Errorf("%w: no schedulable node has room for %s after holding back %d%% of allocatable (%s)",
		ErrNoHeadroom, req, int(s.reserve()*100), s.Summary().Describe())
}

// SchedulableNodes counts nodes a pod could actually land on.
func (s Snapshot) SchedulableNodes() int {
	n := 0
	for _, node := range s.Nodes {
		if node.Schedulable {
			n++
		}
	}
	return n
}

// Summary is the compact, LLM- and operator-facing view of a snapshot. A
// cluster can have hundreds of nodes; dumping every one would cost tokens
// without adding signal, so the bundle carries totals and the single largest
// free node (which is what actually decides whether one more pod fits).
type Summary struct {
	Nodes            int    `json:"nodes"`
	SchedulableNodes int    `json:"schedulableNodes"`
	CPUFreeMilli     int64  `json:"cpuFreeMillicores"`
	CPUAllocMilli    int64  `json:"cpuAllocatableMillicores"`
	MemFreeBytes     int64  `json:"memoryFreeBytes"`
	MemAllocBytes    int64  `json:"memoryAllocatableBytes"`
	PodSlotsFree     int64  `json:"podSlotsFree"`
	PodSlotsTotal    int64  `json:"podSlotsTotal"`
	LargestFreeNode  string `json:"largestFreeNode,omitempty"`
	ReservePercent   int    `json:"reservePercentHeldBack"`
	Note             string `json:"note"`
}

// Summary reduces the snapshot to its totals, with the reserve already applied
// to every "free" figure.
func (s Snapshot) Summary() Summary {
	out := Summary{
		Nodes:            len(s.Nodes),
		SchedulableNodes: s.SchedulableNodes(),
		ReservePercent:   int(s.reserve() * 100),
		Note: "Free figures already exclude the reserve held back for burst and rescheduling. " +
			"They are requests-based headroom, so they say what the scheduler can place, not what is idle.",
	}
	var bestCPU int64 = -1
	for _, n := range s.Nodes {
		out.CPUAllocMilli += n.AllocCPUMilli
		out.MemAllocBytes += n.AllocMemBytes
		out.PodSlotsTotal += n.AllocPods
		if !n.Schedulable {
			continue
		}
		free := s.headroomOn(n)
		out.CPUFreeMilli += free.cpuMilli
		out.MemFreeBytes += free.memBytes
		out.PodSlotsFree += free.pods
		if free.cpuMilli > bestCPU {
			bestCPU = free.cpuMilli
			out.LargestFreeNode = fmt.Sprintf("%s (%dm CPU, %dMi memory, %d pod slots free)",
				n.Name, free.cpuMilli, free.memBytes>>20, free.pods)
		}
	}
	return out
}

// Describe is the one-line human form used in refusals and alerts.
func (s Summary) Describe() string {
	return fmt.Sprintf("cluster free: %dm CPU, %dMi memory, %d pod slots across %d schedulable nodes",
		s.CPUFreeMilli, s.MemFreeBytes>>20, s.PodSlotsFree, s.SchedulableNodes)
}

// MarshalJSON emits the summary rather than the node list, so an evidence
// bundle stays small on a large cluster.
func (s Snapshot) MarshalJSON() ([]byte, error) { return json.Marshal(s.Summary()) }

// Load is the aggregate live CPU load of one workload's replicas, summed across
// the pods that had a metrics sample.
type Load struct {
	// Replicas is the workload's desired replica count.
	Replicas int32
	// Sampled is how many of those replicas actually had a usage sample. A
	// target derived from a subset is still the best available estimate, but the
	// explanation says so.
	Sampled int32
	// CPUMilli is summed live CPU usage across the sampled replicas.
	CPUMilli int64
	// RefMilli is the summed CPU reference those samples are measured against —
	// the limit where one is set, otherwise the request. Zero means the workload
	// is unbounded and no ratio can be computed.
	RefMilli int64
	// RefIsLimit records which reference was used, for the explanation.
	RefIsLimit bool
}

// String is the one-line form carried in the evidence bundle, so the model sees
// the same load figures the replica target was derived from.
func (l Load) String() string {
	if l.Sampled <= 0 || l.RefMilli <= 0 {
		return fmt.Sprintf("%d replicas, no usable CPU measurements", l.Replicas)
	}
	return fmt.Sprintf("%d replicas (%d sampled): CPU %dm against a %dm total %s — %d%% utilisation",
		l.Replicas, l.Sampled, l.CPUMilli, l.RefMilli, refName(l.RefIsLimit), int(l.Ratio()*100))
}

// Ratio is measured utilisation against the reference, averaged over the
// sampled replicas.
func (l Load) Ratio() float64 {
	if l.RefMilli <= 0 {
		return 0
	}
	return float64(l.CPUMilli) / float64(l.RefMilli)
}

// TargetReplicas derives the replica count this workload's measured load calls
// for, using the standard utilisation formula the HorizontalPodAutoscaler uses:
//
//	desired = ceil(current × observedUtilisation / targetUtilisation)
//
// So four replicas averaging 95% of their CPU limit, against a 70% target,
// yields ceil(4 × 0.95/0.70) = 6.
//
// It returns ErrNotComputable when the evidence is too thin to do arithmetic on
// — no replica count, no sample, or no CPU reference to measure against. The
// caller must refuse the heal in that case rather than fall back to a guess:
// scaling blind is exactly the failure this package exists to prevent.
func TargetReplicas(l Load, targetRatio float64) (int32, string, error) {
	if l.Replicas <= 0 {
		return 0, "", fmt.Errorf("%w: workload replica count is unknown or not scalable", ErrNotComputable)
	}
	if l.Sampled <= 0 || l.CPUMilli <= 0 {
		return 0, "", fmt.Errorf("%w: no live CPU samples for this workload (metrics-server missing or not yet reporting)", ErrNotComputable)
	}
	if l.RefMilli <= 0 {
		return 0, "", fmt.Errorf("%w: no CPU limit or request on this workload to measure utilisation against", ErrNotComputable)
	}
	if targetRatio <= 0 || targetRatio > 1 {
		return 0, "", fmt.Errorf("%w: target utilisation %.2f is outside (0,1]", ErrNotComputable, targetRatio)
	}

	ratio := l.Ratio()
	if ratio <= targetRatio {
		return l.Replicas, "", fmt.Errorf("%w: CPU at %d%% of %s is already at or below the %d%% target",
			ErrNotComputable, int(ratio*100), refName(l.RefIsLimit), int(targetRatio*100))
	}

	desired := int32(math.Ceil(float64(l.Replicas) * ratio / targetRatio))
	if desired <= l.Replicas {
		// Defensive: ratio > target should always round up past current.
		desired = l.Replicas + 1
	}

	why := fmt.Sprintf("CPU at %d%% of %s across %d/%d replicas; %d%% target needs %d",
		int(ratio*100), refName(l.RefIsLimit), l.Sampled, l.Replicas, int(targetRatio*100), desired)
	if l.Sampled < l.Replicas {
		why += fmt.Sprintf(" (estimated from %d sampled replica(s))", l.Sampled)
	}
	return desired, why, nil
}

func refName(isLimit bool) string {
	if isLimit {
		return "limit"
	}
	return "request"
}

func nonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
