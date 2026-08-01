package k8s

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/peceldev/podsmedic/internal/capacity"
	"github.com/peceldev/podsmedic/internal/detect"
)

// Bundle is everything the LLM gets about one problem: the pod as described,
// the events around it, and the logs from the failing container.
type Bundle struct {
	Problem detect.Problem    `json:"problem"`
	Pod     PodSummary        `json:"pod"`
	Events  []EventSummary    `json:"events"`
	Logs    map[string]string `json:"logs,omitempty"`
	Node    *NodeSummary      `json:"node,omitempty"`
	// Replicas is the desired replica count of the owning controller (0 if
	// unknown or not scalable), so a scale_replicas heal can be bounded purely.
	Replicas int32 `json:"replicas,omitempty"`
	// Controller is the resolved owning workload, kept so the agent can attach
	// aggregate load without walking owner references a second time. Zero when
	// the pod is bare or owned by a kind podsmedic will not patch.
	Controller ControllerRef `json:"-"`
	// Capacity is the cluster's schedulable headroom, attached by the agent once
	// per sweep. Nil when nodes or pods could not be read — which makes any heal
	// that would add or enlarge pods refuse rather than guess.
	Capacity *capacity.Snapshot `json:"clusterCapacity,omitempty"`
	// PodRequests is what one replica of this workload reserves, so the
	// validator can ask how many more of exactly this pod the cluster can place.
	PodRequests capacity.Requests `json:"podRequests,omitempty"`
	// Load is the workload's aggregate live CPU usage across its replicas, which
	// is what a replica target is derived from. Nil without metrics-server.
	Load *capacity.Load `json:"-"`
	// LoadSummary is the human/LLM-readable form of Load.
	LoadSummary string `json:"workloadLoad,omitempty"`
	// Claims describes every PersistentVolumeClaim the pod mounts, with the
	// claim's own events — which is where the reason an unbound volume will not
	// bind actually lives.
	Claims []ClaimSummary `json:"volumeClaims,omitempty"`
	// Autoscaler is the HorizontalPodAutoscaler that owns this workload's replica
	// count, when one exists. Its presence forbids a scale_replicas heal: two
	// controllers writing spec.replicas overwrite each other.
	Autoscaler *AutoscalerRef `json:"autoscaler,omitempty"`
}

// PodSummary is a trimmed `kubectl describe pod` — everything a human would
// read, none of the managed fields noise.
type PodSummary struct {
	Name        string             `json:"name"`
	Namespace   string             `json:"namespace"`
	Node        string             `json:"node,omitempty"`
	Phase       string             `json:"phase"`
	QOSClass    string             `json:"qosClass,omitempty"`
	StartTime   string             `json:"startTime,omitempty"`
	Age         string             `json:"age,omitempty"`
	OwnerKind   string             `json:"ownerKind,omitempty"`
	OwnerName   string             `json:"ownerName,omitempty"`
	Labels      map[string]string  `json:"labels,omitempty"`
	Conditions  []ConditionSummary `json:"conditions,omitempty"`
	Containers  []ContainerSummary `json:"containers"`
	Volumes     []string           `json:"volumes,omitempty"`
	NodeSelect  map[string]string  `json:"nodeSelector,omitempty"`
	Tolerations []string           `json:"tolerations,omitempty"`
}

// ContainerSummary merges a container's spec with its live status.
type ContainerSummary struct {
	Name         string            `json:"name"`
	Image        string            `json:"image"`
	Init         bool              `json:"init,omitempty"`
	Ready        bool              `json:"ready"`
	RestartCount int32             `json:"restartCount"`
	Requests     map[string]string `json:"requests,omitempty"`
	Limits       map[string]string `json:"limits,omitempty"`
	// Usage is live consumption from metrics-server (cpu/memory), when
	// available. It is what lets the LLM size a limit from real numbers.
	Usage   map[string]string `json:"usage,omitempty"`
	Command []string          `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	EnvKeys []string          `json:"envKeys,omitempty"`
	// Probes carries the liveness/readiness probe config, keyed "liveness"/
	// "readiness". Structured (not a string) so the heal validator can bound a
	// proposed loosening against the current numbers.
	Probes    map[string]*ProbeInfo `json:"probes,omitempty"`
	State     string                `json:"state,omitempty"`
	LastState string                `json:"lastState,omitempty"`
}

// ProbeInfo is one probe's target and timing. Target is human-readable for the
// LLM; the numeric fields are what the validator reasons about.
type ProbeInfo struct {
	Target              string `json:"target,omitempty"`
	InitialDelaySeconds int32  `json:"initialDelaySeconds,omitempty"`
	PeriodSeconds       int32  `json:"periodSeconds,omitempty"`
	TimeoutSeconds      int32  `json:"timeoutSeconds,omitempty"`
	FailureThreshold    int32  `json:"failureThreshold,omitempty"`
	SuccessThreshold    int32  `json:"successThreshold,omitempty"`
}

// ConditionSummary is one pod condition.
type ConditionSummary struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// EventSummary is one Kubernetes event, newest first in the bundle.
type EventSummary struct {
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Age     string `json:"age"`
	Count   int32  `json:"count,omitempty"`
	Message string `json:"message"`
}

// NodeSummary carries node capacity, which is what makes an Unschedulable or
// evicted pod explainable.
type NodeSummary struct {
	Name        string            `json:"name"`
	Allocatable map[string]string `json:"allocatable,omitempty"`
	Conditions  []string          `json:"conditions,omitempty"`
	Taints      []string          `json:"taints,omitempty"`
}

// CollectOptions tunes how much evidence is gathered.
type CollectOptions struct {
	LogTailLines int64
	MaxEvents    int
}

// Collect gathers the evidence bundle for one detected problem.
//
// Log and event collection are best-effort: a pod that was already deleted, or
// a container that never started, should still produce a bundle rather than an
// error, so the LLM can reason about whatever evidence survived.
func (c *Client) Collect(ctx context.Context, p detect.Problem, opts CollectOptions) (*Bundle, error) {
	pod, err := c.cs.CoreV1().Pods(p.Namespace).Get(ctx, p.Pod, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get pod %s/%s: %w", p.Namespace, p.Pod, err)
	}

	b := &Bundle{
		Problem: p,
		Pod:     summarizePod(pod),
		Events:  c.events(ctx, pod, opts.MaxEvents),
		Logs:    c.logs(ctx, pod, p, opts.LogTailLines),
		// What one replica of this workload reserves. Computed from the live pod
		// so a capacity check bounds the real thing, not a rounded summary.
		PodRequests: PodRequests(pod),
	}

	if pod.Spec.NodeName != "" {
		b.Node = c.node(ctx, pod.Spec.NodeName)
	}

	// Volume claims, for the storage problems and for any pod that mounts one:
	// a wedged claim explains failures that look unrelated from pod status alone.
	b.Claims = c.claims(ctx, pod, opts.MaxEvents)

	// Desired replicas of the owning controller, for a possible scale heal.
	// Best-effort: a bare pod or a resolve error just leaves Replicas at 0.
	if ctrl, err := c.ResolveController(ctx, p.Namespace, pod.Name); err == nil {
		b.Replicas = c.workloadReplicas(ctx, ctrl)
		b.Controller = ctrl
	}

	// Merge live usage onto each container. Best-effort: absent metrics leave
	// the summary unchanged.
	if usage := c.containerUsage(ctx, p.Namespace, pod.Name); usage != nil {
		for i := range b.Pod.Containers {
			if u := usage[b.Pod.Containers[i].Name]; len(u) > 0 {
				b.Pod.Containers[i].Usage = u
			}
		}
	}

	return b, nil
}

func summarizePod(pod *corev1.Pod) PodSummary {
	s := PodSummary{
		Name:       pod.Name,
		Namespace:  pod.Namespace,
		Node:       pod.Spec.NodeName,
		Phase:      string(pod.Status.Phase),
		QOSClass:   string(pod.Status.QOSClass),
		Labels:     pod.Labels,
		NodeSelect: pod.Spec.NodeSelector,
	}
	if pod.Status.StartTime != nil {
		s.StartTime = pod.Status.StartTime.Format(time.RFC3339)
		s.Age = time.Since(pod.Status.StartTime.Time).Round(time.Second).String()
	}
	if len(pod.OwnerReferences) > 0 {
		s.OwnerKind = pod.OwnerReferences[0].Kind
		s.OwnerName = pod.OwnerReferences[0].Name
	}
	for _, cond := range pod.Status.Conditions {
		s.Conditions = append(s.Conditions, ConditionSummary{
			Type: string(cond.Type), Status: string(cond.Status),
			Reason: cond.Reason, Message: cond.Message,
		})
	}
	for _, v := range pod.Spec.Volumes {
		s.Volumes = append(s.Volumes, volumeDescription(v))
	}
	for _, t := range pod.Spec.Tolerations {
		s.Tolerations = append(s.Tolerations, fmt.Sprintf("%s%s=%s:%s", t.Key, t.Operator, t.Value, t.Effect))
	}

	statuses := make(map[string]corev1.ContainerStatus, len(pod.Status.ContainerStatuses))
	for _, cs := range pod.Status.InitContainerStatuses {
		statuses[cs.Name] = cs
	}
	for _, cs := range pod.Status.ContainerStatuses {
		statuses[cs.Name] = cs
	}
	for _, c := range pod.Spec.InitContainers {
		s.Containers = append(s.Containers, summarizeContainer(c, statuses[c.Name], true))
	}
	for _, c := range pod.Spec.Containers {
		s.Containers = append(s.Containers, summarizeContainer(c, statuses[c.Name], false))
	}
	return s
}

func summarizeContainer(spec corev1.Container, status corev1.ContainerStatus, init bool) ContainerSummary {
	c := ContainerSummary{
		Name:         spec.Name,
		Image:        spec.Image,
		Init:         init,
		Ready:        status.Ready,
		RestartCount: status.RestartCount,
		Requests:     resourceList(spec.Resources.Requests),
		Limits:       resourceList(spec.Resources.Limits),
		Command:      spec.Command,
		Args:         spec.Args,
		State:        stateDescription(status.State),
		LastState:    stateDescription(status.LastTerminationState),
	}
	probes := map[string]*ProbeInfo{}
	if pi := probeInfo(spec.LivenessProbe); pi != nil {
		probes["liveness"] = pi
	}
	if pi := probeInfo(spec.ReadinessProbe); pi != nil {
		probes["readiness"] = pi
	}
	if len(probes) > 0 {
		c.Probes = probes
	}
	// Env values can hold secrets — only the keys go to the LLM.
	for _, e := range spec.Env {
		c.EnvKeys = append(c.EnvKeys, e.Name)
	}
	return c
}

func resourceList(rl corev1.ResourceList) map[string]string {
	if len(rl) == 0 {
		return nil
	}
	out := make(map[string]string, len(rl))
	for k, v := range rl {
		out[string(k)] = v.String()
	}
	return out
}

func probeInfo(p *corev1.Probe) *ProbeInfo {
	if p == nil {
		return nil
	}
	return &ProbeInfo{
		Target:              probeTarget(p),
		InitialDelaySeconds: p.InitialDelaySeconds,
		PeriodSeconds:       p.PeriodSeconds,
		TimeoutSeconds:      p.TimeoutSeconds,
		FailureThreshold:    p.FailureThreshold,
		SuccessThreshold:    p.SuccessThreshold,
	}
}

func probeTarget(p *corev1.Probe) string {
	switch {
	case p.HTTPGet != nil:
		return fmt.Sprintf("http-get %s:%s%s", p.HTTPGet.Scheme, p.HTTPGet.Port.String(), p.HTTPGet.Path)
	case p.TCPSocket != nil:
		return "tcp-socket " + p.TCPSocket.Port.String()
	case p.Exec != nil:
		return "exec " + strings.Join(p.Exec.Command, " ")
	case p.GRPC != nil:
		return fmt.Sprintf("grpc :%d", p.GRPC.Port)
	default:
		return "unknown"
	}
}

func stateDescription(s corev1.ContainerState) string {
	switch {
	case s.Running != nil:
		return fmt.Sprintf("running since %s", s.Running.StartedAt.Format(time.RFC3339))
	case s.Waiting != nil:
		return strings.TrimSpace(fmt.Sprintf("waiting: %s %s", s.Waiting.Reason, s.Waiting.Message))
	case s.Terminated != nil:
		t := s.Terminated
		return strings.TrimSpace(fmt.Sprintf("terminated: %s exitCode=%d signal=%d finishedAt=%s %s",
			t.Reason, t.ExitCode, t.Signal, t.FinishedAt.Format(time.RFC3339), t.Message))
	default:
		return ""
	}
}

func volumeDescription(v corev1.Volume) string {
	switch {
	case v.ConfigMap != nil:
		return fmt.Sprintf("%s (configMap %s)", v.Name, v.ConfigMap.Name)
	case v.Secret != nil:
		return fmt.Sprintf("%s (secret %s)", v.Name, v.Secret.SecretName)
	case v.PersistentVolumeClaim != nil:
		return fmt.Sprintf("%s (pvc %s)", v.Name, v.PersistentVolumeClaim.ClaimName)
	case v.EmptyDir != nil:
		return v.Name + " (emptyDir)"
	default:
		return v.Name
	}
}

func (c *Client) events(ctx context.Context, pod *corev1.Pod, max int) []EventSummary {
	selector := fmt.Sprintf("involvedObject.name=%s,involvedObject.namespace=%s", pod.Name, pod.Namespace)
	list, err := c.cs.CoreV1().Events(pod.Namespace).List(ctx, metav1.ListOptions{FieldSelector: selector})
	if err != nil {
		return nil
	}

	items := list.Items
	sort.Slice(items, func(i, j int) bool {
		return eventTime(items[i]).After(eventTime(items[j]))
	})
	if max > 0 && len(items) > max {
		items = items[:max]
	}

	out := make([]EventSummary, 0, len(items))
	for _, e := range items {
		out = append(out, EventSummary{
			Type:    e.Type,
			Reason:  e.Reason,
			Age:     time.Since(eventTime(e)).Round(time.Second).String(),
			Count:   e.Count,
			Message: e.Message,
		})
	}
	return out
}

func eventTime(e corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.FirstTimestamp.Time
}

// logs fetches the tail of the failing container's logs. For a crashed
// container the interesting output is in the *previous* instance, so that is
// tried first.
func (c *Client) logs(ctx context.Context, pod *corev1.Pod, p detect.Problem, tail int64) map[string]string {
	targets := []string{p.Container}
	if p.Container == "" {
		targets = nil
		for _, cs := range pod.Status.ContainerStatuses {
			targets = append(targets, cs.Name)
		}
	}

	out := make(map[string]string, len(targets))
	for _, name := range targets {
		if name == "" {
			continue
		}
		if body, err := c.containerLog(ctx, pod, name, tail, true); err == nil && strings.TrimSpace(body) != "" {
			out[name+" (previous)"] = body
			continue
		}
		if body, err := c.containerLog(ctx, pod, name, tail, false); err == nil && strings.TrimSpace(body) != "" {
			out[name] = body
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (c *Client) containerLog(ctx context.Context, pod *corev1.Pod, container string, tail int64, previous bool) (string, error) {
	req := c.cs.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: container,
		TailLines: &tail,
		Previous:  previous,
	})
	raw, err := req.DoRaw(ctx)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (c *Client) node(ctx context.Context, name string) *NodeSummary {
	n, err := c.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil // node read is optional; RBAC may not grant it
	}
	s := &NodeSummary{Name: n.Name, Allocatable: resourceList(n.Status.Allocatable)}
	for _, cond := range n.Status.Conditions {
		if cond.Status == corev1.ConditionTrue {
			s.Conditions = append(s.Conditions, fmt.Sprintf("%s=%s (%s)", cond.Type, cond.Status, cond.Reason))
		}
	}
	for _, t := range n.Spec.Taints {
		s.Taints = append(s.Taints, fmt.Sprintf("%s=%s:%s", t.Key, t.Value, t.Effect))
	}
	return s
}
