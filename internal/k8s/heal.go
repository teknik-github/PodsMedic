package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ControllerRef identifies a workload controller that owns a pod. Only the
// kinds podsmedic is willing to mutate are representable here.
type ControllerRef struct {
	Kind      string // Deployment | StatefulSet | DaemonSet
	Name      string
	Namespace string
}

func (c ControllerRef) String() string {
	return fmt.Sprintf("%s/%s in %s", c.Kind, c.Name, c.Namespace)
}

// ResolveController walks a pod's owner references to the top-level workload
// controller. It returns an error for pods that are bare, or owned by a kind
// podsmedic will not patch — those are never auto-healed.
func (c *Client) ResolveController(ctx context.Context, namespace, podName string) (ControllerRef, error) {
	pod, err := c.cs.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return ControllerRef{}, fmt.Errorf("get pod: %w", err)
	}

	owner := controllerOwner(pod.OwnerReferences)
	if owner == nil {
		return ControllerRef{}, fmt.Errorf("pod %s/%s has no controller owner", namespace, podName)
	}

	switch owner.Kind {
	case "ReplicaSet":
		// A ReplicaSet is itself owned by a Deployment; walk one more level so
		// the patch survives the next rollout.
		rs, err := c.cs.AppsV1().ReplicaSets(namespace).Get(ctx, owner.Name, metav1.GetOptions{})
		if err != nil {
			return ControllerRef{}, fmt.Errorf("get replicaset: %w", err)
		}
		if dep := controllerOwner(rs.OwnerReferences); dep != nil && dep.Kind == "Deployment" {
			return ControllerRef{Kind: "Deployment", Name: dep.Name, Namespace: namespace}, nil
		}
		return ControllerRef{}, fmt.Errorf("replicaset %s is not owned by a Deployment", owner.Name)
	case "StatefulSet", "DaemonSet":
		return ControllerRef{Kind: owner.Kind, Name: owner.Name, Namespace: namespace}, nil
	default:
		return ControllerRef{}, fmt.Errorf("owner kind %q is not auto-healable", owner.Kind)
	}
}

func controllerOwner(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	return nil
}

// PatchContainerResources raises the resources of one container on a
// controller's pod template via a strategic merge patch. When dryRun is true
// the API server validates but does not persist the change.
func (c *Client) PatchContainerResources(ctx context.Context, ctrl ControllerRef, container string, limits, requests map[string]string, dryRun bool) error {
	resources := map[string]any{}
	if len(limits) > 0 {
		resources["limits"] = limits
	}
	if len(requests) > 0 {
		resources["requests"] = requests
	}

	// The container name is the strategic-merge key, so this patches exactly
	// the named container and leaves the others untouched.
	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []map[string]any{
						{"name": container, "resources": resources},
					},
				},
			},
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}

	return c.applyWorkloadPatch(ctx, ctrl, types.StrategicMergePatchType, body, dryRun)
}

// PatchContainerImage sets one container's image on a controller's pod template
// via a strategic merge patch. The container name is the merge key, so only the
// named container is touched.
func (c *Client) PatchContainerImage(ctx context.Context, ctrl ControllerRef, container, image string, dryRun bool) error {
	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []map[string]any{
						{"name": container, "image": image},
					},
				},
			},
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}
	return c.applyWorkloadPatch(ctx, ctrl, types.StrategicMergePatchType, body, dryRun)
}

// PatchContainerProbe sets timing fields on one container's liveness or
// readiness probe via a strategic merge patch. probeType is "liveness" or
// "readiness"; fields are the probe timing keys to set. Strategic merge on the
// probe object updates only the given fields, leaving the target untouched.
func (c *Client) PatchContainerProbe(ctx context.Context, ctrl ControllerRef, container, probeType string, fields map[string]int32, dryRun bool) error {
	probeKey := "livenessProbe"
	if probeType == "readiness" {
		probeKey = "readinessProbe"
	}
	probe := map[string]any{}
	for k, v := range fields {
		probe[k] = v
	}
	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []map[string]any{
						{"name": container, probeKey: probe},
					},
				},
			},
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}
	return c.applyWorkloadPatch(ctx, ctrl, types.StrategicMergePatchType, body, dryRun)
}

// RestartWorkload triggers a rollout restart by stamping the pod template with
// a restart annotation, mirroring `kubectl rollout restart`.
func (c *Client) RestartWorkload(ctx context.Context, ctrl ControllerRef, dryRun bool) error {
	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]any{
						"podsmedic.dev/restartedAt": time.Now().UTC().Format(time.RFC3339),
					},
				},
			},
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}
	return c.applyWorkloadPatch(ctx, ctrl, types.StrategicMergePatchType, body, dryRun)
}

// workloadReplicas returns the desired replica count of a scalable controller,
// or 0 for a DaemonSet, an error, or an unset field. Best-effort for evidence.
func (c *Client) workloadReplicas(ctx context.Context, ctrl ControllerRef) int32 {
	switch ctrl.Kind {
	case "Deployment":
		d, err := c.cs.AppsV1().Deployments(ctrl.Namespace).Get(ctx, ctrl.Name, metav1.GetOptions{})
		if err != nil || d.Spec.Replicas == nil {
			return 0
		}
		return *d.Spec.Replicas
	case "StatefulSet":
		s, err := c.cs.AppsV1().StatefulSets(ctrl.Namespace).Get(ctx, ctrl.Name, metav1.GetOptions{})
		if err != nil || s.Spec.Replicas == nil {
			return 0
		}
		return *s.Spec.Replicas
	default:
		return 0
	}
}

// ScaleWorkload sets a workload's replica count. DaemonSets have no replicas and
// are rejected. The patch is a strategic merge on spec.replicas.
func (c *Client) ScaleWorkload(ctx context.Context, ctrl ControllerRef, replicas int32, dryRun bool) error {
	if ctrl.Kind == "DaemonSet" {
		return fmt.Errorf("cannot scale a DaemonSet (it has no replica count)")
	}
	patch := map[string]any{"spec": map[string]any{"replicas": replicas}}
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}
	return c.applyWorkloadPatch(ctx, ctrl, types.StrategicMergePatchType, body, dryRun)
}

func (c *Client) applyWorkloadPatch(ctx context.Context, ctrl ControllerRef, pt types.PatchType, body []byte, dryRun bool) error {
	opts := metav1.PatchOptions{FieldManager: "podsmedic"}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}

	var err error
	switch ctrl.Kind {
	case "Deployment":
		_, err = c.cs.AppsV1().Deployments(ctrl.Namespace).Patch(ctx, ctrl.Name, pt, body, opts)
	case "StatefulSet":
		_, err = c.cs.AppsV1().StatefulSets(ctrl.Namespace).Patch(ctx, ctrl.Name, pt, body, opts)
	case "DaemonSet":
		_, err = c.cs.AppsV1().DaemonSets(ctrl.Namespace).Patch(ctx, ctrl.Name, pt, body, opts)
	default:
		return fmt.Errorf("unsupported controller kind %q", ctrl.Kind)
	}
	if err != nil {
		return fmt.Errorf("patch %s: %w", ctrl, err)
	}
	return nil
}
