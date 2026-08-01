// Package k8s wraps the Kubernetes API access podsmedic needs: listing pods,
// and collecting the evidence bundle for a problematic one.
package k8s

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Client is a thin facade over client-go.
type Client struct {
	cs *kubernetes.Clientset
}

// New builds a client from the in-cluster service account when running inside
// a pod, falling back to a kubeconfig file for local development.
func New(kubeconfig string) (*Client, error) {
	cfg, err := restConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}
	return &Client{cs: cs}, nil
}

// Clientset exposes the underlying client-go interface.
//
// Deliberately the only escape hatch from this facade, and used by exactly one
// caller: leader election, which needs the coordination API this package
// otherwise has no business knowing about. Everything else goes through a named
// method here, so the set of API calls podsmedic makes stays readable — and
// stays matched to the RBAC.
func (c *Client) Clientset() kubernetes.Interface { return c.cs }

func restConfig(kubeconfig string) (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	if kubeconfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate kubeconfig: %w", err)
		}
		kubeconfig = filepath.Join(home, ".kube", "config")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig %s: %w", kubeconfig, err)
	}
	return cfg, nil
}

// ListPods returns pods in the given namespaces. An empty slice means every
// namespace the service account can see.
func (c *Client) ListPods(ctx context.Context, namespaces []string) ([]corev1.Pod, error) {
	if len(namespaces) == 0 {
		list, err := c.cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list pods (all namespaces): %w", err)
		}
		return list.Items, nil
	}

	var out []corev1.Pod
	for _, ns := range namespaces {
		list, err := c.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list pods in %s: %w", ns, err)
		}
		out = append(out, list.Items...)
	}
	return out, nil
}
