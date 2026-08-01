package k8s

import (
	"context"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GetConfigMap returns a ConfigMap's data, or (nil, nil) when it does not exist
// yet — a missing state ConfigMap is a normal first-run condition, not an error.
func (c *Client) GetConfigMap(ctx context.Context, namespace, name string) (map[string]string, error) {
	cm, err := c.cs.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get configmap %s/%s: %w", namespace, name, err)
	}
	return cm.Data, nil
}

// PutConfigMap creates the ConfigMap if absent, otherwise replaces its data.
func (c *Client) PutConfigMap(ctx context.Context, namespace, name string, data map[string]string) error {
	cms := c.cs.CoreV1().ConfigMaps(namespace)
	existing, err := cms.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Data:       data,
		}, metav1.CreateOptions{FieldManager: "podsmedic"})
		if err != nil {
			return fmt.Errorf("create configmap %s/%s: %w", namespace, name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get configmap %s/%s: %w", namespace, name, err)
	}
	existing.Data = data
	if _, err := cms.Update(ctx, existing, metav1.UpdateOptions{FieldManager: "podsmedic"}); err != nil {
		return fmt.Errorf("update configmap %s/%s: %w", namespace, name, err)
	}
	return nil
}

// Namespace returns the namespace podsmedic runs in, for storing its own state.
// In-cluster it comes from the service-account mount; otherwise from
// POD_NAMESPACE, falling back to "podsmedic".
func Namespace() string {
	const mount = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	if b, err := os.ReadFile(mount); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	if ns := strings.TrimSpace(os.Getenv("POD_NAMESPACE")); ns != "" {
		return ns
	}
	return "podsmedic"
}
