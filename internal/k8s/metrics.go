package k8s

import (
	"context"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ContainerUsage is one container's live memory and CPU usage against its
// limits — memory in bytes, CPU in millicores. A neutral type so the metrics
// layer stays free of the predict package.
type ContainerUsage struct {
	Namespace  string
	Pod        string
	Container  string
	UsageBytes int64 // memory usage
	LimitBytes int64 // memory limit
	CPUMilli   int64 // CPU usage, millicores
	CPULimit   int64 // CPU limit, millicores
}

// usageVals is per-container live usage decoded from metrics-server.
type usageVals struct {
	memBytes int64
	cpuMilli int64
}

// UsageSamples joins already-listed pods (for their configured limits) with live
// usage from metrics-server, returning one entry per container that has a usage
// sample. Reusing the caller's pod list avoids a second List per sweep. A
// metrics API error is surfaced so the caller can skip prediction this sweep.
func (c *Client) UsageSamples(ctx context.Context, pods []corev1.Pod, namespaces []string) ([]ContainerUsage, error) {
	usage, err := c.listUsage(ctx, namespaces)
	if err != nil {
		return nil, err
	}
	var out []ContainerUsage
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil {
			continue // terminating (e.g. mid-rollout): do not let it drive prediction
		}
		for j := range pod.Spec.Containers {
			ctr := &pod.Spec.Containers[j]
			u, ok := usage[pod.Namespace+"/"+pod.Name+"/"+ctr.Name]
			if !ok {
				continue
			}
			out = append(out, ContainerUsage{
				Namespace:  pod.Namespace,
				Pod:        pod.Name,
				Container:  ctr.Name,
				UsageBytes: u.memBytes,
				LimitBytes: ctr.Resources.Limits.Memory().Value(),
				CPUMilli:   u.cpuMilli,
				CPULimit:   ctr.Resources.Limits.Cpu().MilliValue(),
			})
		}
	}
	return out, nil
}

// podMetrics is the subset of a metrics.k8s.io/v1beta1 PodMetrics object we
// decode. Hitting the metrics API through the discovery REST client with a
// local struct keeps podsmedic free of the k8s.io/metrics module, matching the
// dependency-light stance of the OpenAI backend.
type podMetrics struct {
	Containers []struct {
		Name  string            `json:"name"`
		Usage map[string]string `json:"usage"`
	} `json:"containers"`
}

// podMetricsList is a metrics.k8s.io PodMetricsList, for the cluster-wide sweep
// the predictor needs (one call rather than one per pod).
type podMetricsList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Containers []struct {
			Name  string            `json:"name"`
			Usage map[string]string `json:"usage"`
		} `json:"containers"`
	} `json:"items"`
}

// listUsage returns live memory (bytes) and CPU (millicores) usage per
// container, keyed "namespace/pod/container", for the given namespaces (empty =
// all). Best-effort like containerUsage: no metrics-server or missing RBAC
// yields (nil, err) and the caller simply skips prediction this sweep.
func (c *Client) listUsage(ctx context.Context, namespaces []string) (map[string]usageVals, error) {
	out := map[string]usageVals{}
	scopes := namespaces
	if len(scopes) == 0 {
		scopes = []string{""} // cluster-wide
	}
	for _, ns := range scopes {
		req := c.cs.Discovery().RESTClient().Get()
		if ns == "" {
			req = req.AbsPath("/apis/metrics.k8s.io/v1beta1/pods")
		} else {
			req = req.AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces", ns, "pods")
		}
		raw, err := req.DoRaw(ctx)
		if err != nil {
			return nil, err
		}
		mergeUsage(out, raw)
	}
	return out, nil
}

// mergeUsage decodes a PodMetricsList and adds each container's memory (bytes)
// and CPU (millicores) usage to out. Pure, for unit testing.
func mergeUsage(out map[string]usageVals, raw []byte) {
	var list podMetricsList
	if err := json.Unmarshal(raw, &list); err != nil {
		return
	}
	for _, it := range list.Items {
		for _, cm := range it.Containers {
			var v usageVals
			if mem, ok := cm.Usage["memory"]; ok {
				if q, err := resource.ParseQuantity(mem); err == nil {
					v.memBytes = q.Value()
				}
			}
			if cpu, ok := cm.Usage["cpu"]; ok {
				if q, err := resource.ParseQuantity(cpu); err == nil {
					v.cpuMilli = q.MilliValue()
				}
			}
			out[it.Metadata.Namespace+"/"+it.Metadata.Name+"/"+cm.Name] = v
		}
	}
}

// containerUsage returns live CPU/memory usage per container from
// metrics-server, keyed by container name then resource ("cpu"/"memory").
//
// Best-effort by design: no metrics-server, missing RBAC, or a pod too new to
// have a sample all yield nil, and the bundle is built without usage rather
// than failing. When present, usage turns the LLM's sizing from a guess into
// arithmetic ("using 210Mi against a 128Mi limit → raise to 256Mi").
func (c *Client) containerUsage(ctx context.Context, ns, pod string) map[string]map[string]string {
	raw, err := c.cs.Discovery().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces", ns, "pods", pod).
		DoRaw(ctx)
	if err != nil {
		return nil
	}
	return parsePodMetrics(raw)
}

// parsePodMetrics decodes a PodMetrics document into per-container usage. It is
// pure so the normalization is unit-tested without a live metrics API. Each
// value is canonicalized through resource.Quantity, which also drops any value
// the API server would not have accepted.
func parsePodMetrics(raw []byte) map[string]map[string]string {
	var pm podMetrics
	if err := json.Unmarshal(raw, &pm); err != nil {
		return nil
	}
	out := make(map[string]map[string]string, len(pm.Containers))
	for _, cm := range pm.Containers {
		norm := make(map[string]string, len(cm.Usage))
		for k, v := range cm.Usage {
			if q, err := resource.ParseQuantity(v); err == nil {
				norm[k] = q.String()
			}
		}
		if len(norm) > 0 {
			out[cm.Name] = norm
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
