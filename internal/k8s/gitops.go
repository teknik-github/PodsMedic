package k8s

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GitOpsManager inspects a workload's labels and annotations and returns the
// name of the GitOps/packaging tool that owns it ("argocd", "flux", "helm"), or
// "" if none is detected. Pure so the marker matching is unit-tested without a
// cluster.
//
// The point: a controller reconciled from Git is the source of truth, not the
// live object. Patching it in place is at best reverted on the next sync and at
// worst starts a fight between podsmedic and the GitOps controller — so such a
// workload should be fixed in its repository, not auto-healed.
func GitOpsManager(labels, annotations map[string]string) string {
	if labels["argocd.argoproj.io/instance"] != "" || annotations["argocd.argoproj.io/tracking-id"] != "" {
		return "argocd"
	}
	// Flux stamps <kind>.toolkit.fluxcd.io/name (kustomize, helm, ...).
	for k := range labels {
		if strings.HasSuffix(k, ".toolkit.fluxcd.io/name") {
			return "flux"
		}
	}
	if annotations["meta.helm.sh/release-name"] != "" {
		return "helm"
	}
	switch strings.ToLower(labels["app.kubernetes.io/managed-by"]) {
	case "helm":
		return "helm"
	case "flux", "flux-controller":
		return "flux"
	case "argocd", "argo-cd":
		return "argocd"
	}
	return ""
}

// WorkloadManagedBy fetches the controller and reports its GitOps manager, or ""
// when it is not GitOps-managed.
func (c *Client) WorkloadManagedBy(ctx context.Context, ctrl ControllerRef) (string, error) {
	var meta metav1.ObjectMeta
	switch ctrl.Kind {
	case "Deployment":
		d, err := c.cs.AppsV1().Deployments(ctrl.Namespace).Get(ctx, ctrl.Name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		meta = d.ObjectMeta
	case "StatefulSet":
		s, err := c.cs.AppsV1().StatefulSets(ctrl.Namespace).Get(ctx, ctrl.Name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		meta = s.ObjectMeta
	case "DaemonSet":
		ds, err := c.cs.AppsV1().DaemonSets(ctrl.Namespace).Get(ctx, ctrl.Name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		meta = ds.ObjectMeta
	default:
		return "", nil
	}
	return GitOpsManager(meta.Labels, meta.Annotations), nil
}
