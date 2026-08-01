package k8s

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/teknik-github/PodsMedic/internal/capacity"
)

// ClusterCapacity reads every node's allocatable capacity and every scheduled
// pod's requests, and returns the resulting headroom snapshot.
//
// It deliberately lists pods across *all* namespaces even when podsmedic is
// scoped to a few with PODSMEDIC_NAMESPACES: capacity is a property of the
// cluster, and ignoring the pods you are not watching would overstate free
// space by exactly the amount someone else is using. The base ClusterRole
// already grants both reads.
//
// An error here is not softened into an empty snapshot: a caller that cannot
// see capacity must refuse to add pods, not assume there is room.
func (c *Client) ClusterCapacity(ctx context.Context, reserve float64) (*capacity.Snapshot, error) {
	nodes, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	pods, err := c.cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	byName := make(map[string]*capacity.Node, len(nodes.Items))
	snap := &capacity.Snapshot{Reserve: reserve, Nodes: make([]capacity.Node, len(nodes.Items))}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		snap.Nodes[i] = capacity.Node{
			Name:          n.Name,
			AllocCPUMilli: n.Status.Allocatable.Cpu().MilliValue(),
			AllocMemBytes: n.Status.Allocatable.Memory().Value(),
			AllocPods:     n.Status.Allocatable.Pods().Value(),
			Schedulable:   nodeSchedulable(n),
		}
		byName[n.Name] = &snap.Nodes[i]
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName == "" || !holdsResources(pod) {
			continue
		}
		node, ok := byName[pod.Spec.NodeName]
		if !ok {
			continue // bound to a node we cannot see; nothing to charge it to
		}
		req := PodRequests(pod)
		node.UsedCPUMilli += req.CPUMilli
		node.UsedMemBytes += req.MemBytes
		node.UsedPods++
	}

	return snap, nil
}

// nodeSchedulable reports whether the scheduler would place a new pod here at
// all. A cordoned or NotReady node's free space is not really free, and
// counting it would let a scale-up be approved against capacity that does not
// exist. Taints are not considered — see capacity.Snapshot.FitAdditional on why
// this is a necessary rather than sufficient check.
func nodeSchedulable(n *corev1.Node) bool {
	if n.Spec.Unschedulable {
		return false
	}
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false // no Ready condition reported: treat as unusable
}

// holdsResources reports whether a pod still occupies its node's capacity.
// Succeeded and Failed pods do not — the scheduler ignores them — but a
// terminating pod does until it is actually gone, so it is counted. Erring
// toward "still occupied" understates free space, which is the safe direction.
func holdsResources(pod *corev1.Pod) bool {
	return pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// PodRequests computes a pod's effective resource request — what the scheduler
// must find room for.
//
// This is the upstream formula, not a simple sum: regular containers add up,
// native sidecars (init containers with restartPolicy: Always) add up alongside
// them because they run for the pod's whole life, and ordinary init containers
// contribute only their peak, because they run one at a time before the rest
// start. Pod overhead is added last.
func PodRequests(pod *corev1.Pod) capacity.Requests {
	var cpu, mem int64
	for i := range pod.Spec.Containers {
		r := pod.Spec.Containers[i].Resources.Requests
		cpu += r.Cpu().MilliValue()
		mem += r.Memory().Value()
	}

	// Native sidecars run for the pod's lifetime, so they are additive. Ordinary
	// init containers run sequentially before the app starts, so the pod only
	// ever needs the largest of them — measured on top of the sidecars already
	// running by that point.
	var sidecarCPU, sidecarMem, peakCPU, peakMem int64
	for i := range pod.Spec.InitContainers {
		ic := &pod.Spec.InitContainers[i]
		r := ic.Resources.Requests
		if ic.RestartPolicy != nil && *ic.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecarCPU += r.Cpu().MilliValue()
			sidecarMem += r.Memory().Value()
			continue
		}
		if v := sidecarCPU + r.Cpu().MilliValue(); v > peakCPU {
			peakCPU = v
		}
		if v := sidecarMem + r.Memory().Value(); v > peakMem {
			peakMem = v
		}
	}
	cpu += sidecarCPU
	mem += sidecarMem
	if peakCPU > cpu {
		cpu = peakCPU
	}
	if peakMem > mem {
		mem = peakMem
	}

	if pod.Spec.Overhead != nil {
		cpu += pod.Spec.Overhead.Cpu().MilliValue()
		mem += pod.Spec.Overhead.Memory().Value()
	}
	return capacity.Requests{CPUMilli: cpu, MemBytes: mem}
}

// WorkloadLoad aggregates live CPU usage across the replicas of one controller,
// so a scale target is derived from the whole workload rather than from the
// single pod that happened to trip the detector.
//
// It reuses the sweep's already-listed pods and usage samples, and memoises the
// owner walk, so a Deployment's N replicas cost one ReplicaSet lookup rather
// than N. Replicas is the controller's desired count, which may exceed the
// number actually sampled; the shortfall is reported so the estimate can say so.
func (c *Client) WorkloadLoad(ctx context.Context, ctrl ControllerRef, replicas int32, pods []corev1.Pod, usage []ContainerUsage) capacity.Load {
	load := capacity.Load{Replicas: replicas}

	owned := c.podsOfController(ctx, ctrl, pods)
	if len(owned) == 0 {
		return load
	}

	// Reference to measure utilisation against: limits when the workload sets
	// them, otherwise requests. Mixing the two across containers would produce a
	// ratio that means nothing, so it is one or the other for the whole workload.
	var limitMilli, requestMilli int64
	for _, pod := range owned {
		for i := range pod.Spec.Containers {
			r := &pod.Spec.Containers[i].Resources
			limitMilli += r.Limits.Cpu().MilliValue()
			requestMilli += r.Requests.Cpu().MilliValue()
		}
	}
	if limitMilli > 0 {
		load.RefMilli, load.RefIsLimit = limitMilli, true
	} else {
		load.RefMilli = requestMilli
	}

	sampled := map[string]bool{}
	for _, u := range usage {
		if u.Namespace != ctrl.Namespace {
			continue
		}
		if _, ok := owned[u.Pod]; !ok {
			continue
		}
		load.CPUMilli += u.CPUMilli
		sampled[u.Pod] = true
	}
	load.Sampled = int32(len(sampled))
	return load
}

// podsOfController returns the controller's pods from an already-listed set,
// keyed by pod name.
func (c *Client) podsOfController(ctx context.Context, ctrl ControllerRef, pods []corev1.Pod) map[string]*corev1.Pod {
	out := map[string]*corev1.Pod{}
	memo := map[string]ControllerRef{}
	for i := range pods {
		pod := &pods[i]
		if pod.Namespace != ctrl.Namespace || pod.DeletionTimestamp != nil {
			continue
		}
		if owner, ok := c.controllerOfPod(ctx, pod, memo); ok && owner == ctrl {
			out[pod.Name] = pod
		}
	}
	return out
}

// controllerOfPod resolves an already-listed pod's top-level controller,
// memoising by owner reference so sibling replicas share one lookup. It mirrors
// ResolveController but avoids re-fetching a pod the caller already holds.
func (c *Client) controllerOfPod(ctx context.Context, pod *corev1.Pod, memo map[string]ControllerRef) (ControllerRef, bool) {
	owner := controllerOwner(pod.OwnerReferences)
	if owner == nil {
		return ControllerRef{}, false
	}
	cacheKey := pod.Namespace + "/" + owner.Kind + "/" + owner.Name
	if ref, ok := memo[cacheKey]; ok {
		return ref, ref != ControllerRef{}
	}

	var ref ControllerRef
	switch owner.Kind {
	case "ReplicaSet":
		rs, err := c.cs.AppsV1().ReplicaSets(pod.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
		if err == nil {
			if dep := controllerOwner(rs.OwnerReferences); dep != nil && dep.Kind == "Deployment" {
				ref = ControllerRef{Kind: "Deployment", Name: dep.Name, Namespace: pod.Namespace}
			}
		}
	case "StatefulSet", "DaemonSet":
		ref = ControllerRef{Kind: owner.Kind, Name: owner.Name, Namespace: pod.Namespace}
	}

	memo[cacheKey] = ref
	return ref, ref != ControllerRef{}
}
