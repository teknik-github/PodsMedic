package k8s

import (
	"context"
	"fmt"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AutoscalerRef is a HorizontalPodAutoscaler that owns a workload's replica
// count.
//
// Its presence is what makes a podsmedic scale-up wrong rather than merely
// redundant: two controllers writing spec.replicas will overwrite each other
// every reconcile. This is the same class of problem as a GitOps-managed
// workload — some other authority owns the field — and gets the same answer:
// stay out of it.
type AutoscalerRef struct {
	Name        string `json:"name"`
	MinReplicas int32  `json:"minReplicas,omitempty"`
	MaxReplicas int32  `json:"maxReplicas"`
	// Targets is what the HPA scales on, rendered for the alert so an operator
	// can see whether it is already handling the pressure podsmedic detected.
	Targets string `json:"targets,omitempty"`
	// CurrentReplicas is what the HPA last set, which is usually a better answer
	// than podsmedic's would have been.
	CurrentReplicas int32 `json:"currentReplicas,omitempty"`
}

func (a AutoscalerRef) String() string {
	s := a.Name
	if a.Targets != "" {
		s += " (" + a.Targets + ")"
	}
	if a.MaxReplicas > 0 {
		s += fmt.Sprintf(", %d–%d replicas", a.MinReplicas, a.MaxReplicas)
	}
	return s
}

// autoscalerKey indexes an HPA by the workload it targets.
func autoscalerKey(namespace, kind, name string) string {
	return namespace + "/" + kind + "/" + name
}

// Key is the index entry for the workload this reference belongs to.
func (c ControllerRef) autoscalerKey() string {
	return autoscalerKey(c.Namespace, c.Kind, c.Name)
}

// ListAutoscalers indexes every HorizontalPodAutoscaler in the cluster by the
// workload it targets, so a sweep can answer "is something else already scaling
// this?" without a lookup per problem.
//
// An error is returned rather than swallowed, but the caller treats it as a
// warning: an unreadable HPA list means podsmedic cannot tell whether a conflict
// exists. That is deliberately *not* fail-closed, unlike the capacity gate. The
// consequences differ — scaling into a cluster with no room strands Pending pods
// that nobody notices, whereas scaling against an HPA is self-limiting and
// visible: the HPA wins, verification sees the workload unchanged, the heal is
// rolled back, and the breaker trips if it keeps happening. Failing closed here
// would instead disable scaling for every cluster that has no HPAs at all and
// never granted the read.
func (c *Client) ListAutoscalers(ctx context.Context) (map[string]AutoscalerRef, error) {
	list, err := c.cs.AutoscalingV2().HorizontalPodAutoscalers(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	out := make(map[string]AutoscalerRef, len(list.Items))
	for i := range list.Items {
		h := &list.Items[i]
		target := h.Spec.ScaleTargetRef
		if target.Kind == "" || target.Name == "" {
			continue
		}
		ref := AutoscalerRef{
			Name:            h.Name,
			MaxReplicas:     h.Spec.MaxReplicas,
			Targets:         autoscalerMetrics(h),
			CurrentReplicas: h.Status.CurrentReplicas,
		}
		if h.Spec.MinReplicas != nil {
			ref.MinReplicas = *h.Spec.MinReplicas
		}
		out[autoscalerKey(h.Namespace, target.Kind, target.Name)] = ref
	}
	return out, nil
}

// Autoscaler returns the HPA targeting this controller, if any.
func Autoscaler(index map[string]AutoscalerRef, ctrl ControllerRef) *AutoscalerRef {
	if len(index) == 0 || ctrl.Name == "" {
		return nil
	}
	if ref, ok := index[ctrl.autoscalerKey()]; ok {
		return &ref
	}
	return nil
}

// autoscalerMetrics renders what an HPA scales on, briefly. The detail matters
// to a reader deciding whether the HPA is already handling the pressure: an HPA
// on CPU makes podsmedic's CPUPressure scale-up plainly redundant, while one on
// a custom queue-depth metric may not.
func autoscalerMetrics(h *autoscalingv2.HorizontalPodAutoscaler) string {
	var parts []string
	for _, m := range h.Spec.Metrics {
		switch m.Type {
		case autoscalingv2.ResourceMetricSourceType:
			if m.Resource != nil {
				parts = append(parts, describeTarget(string(m.Resource.Name), m.Resource.Target))
			}
		case autoscalingv2.ContainerResourceMetricSourceType:
			if m.ContainerResource != nil {
				parts = append(parts, describeTarget(string(m.ContainerResource.Name), m.ContainerResource.Target))
			}
		case autoscalingv2.PodsMetricSourceType:
			if m.Pods != nil {
				parts = append(parts, m.Pods.Metric.Name)
			}
		case autoscalingv2.ObjectMetricSourceType:
			if m.Object != nil {
				parts = append(parts, m.Object.Metric.Name)
			}
		case autoscalingv2.ExternalMetricSourceType:
			if m.External != nil {
				parts = append(parts, m.External.Metric.Name)
			}
		}
	}
	return strings.Join(parts, ", ")
}

func describeTarget(name string, t autoscalingv2.MetricTarget) string {
	if t.AverageUtilization != nil {
		return fmt.Sprintf("%s %d%%", name, *t.AverageUtilization)
	}
	if t.AverageValue != nil {
		return fmt.Sprintf("%s %s", name, t.AverageValue.String())
	}
	return name
}
