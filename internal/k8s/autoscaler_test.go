package k8s

import (
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/resource"
)

func utilTarget(pct int32) autoscalingv2.MetricTarget {
	return autoscalingv2.MetricTarget{AverageUtilization: &pct}
}

func TestAutoscalerMatchesOnlyItsTarget(t *testing.T) {
	index := map[string]AutoscalerRef{
		autoscalerKey("api", "Deployment", "web"): {Name: "web-hpa", MaxReplicas: 10},
	}

	if got := Autoscaler(index, ControllerRef{Kind: "Deployment", Name: "web", Namespace: "api"}); got == nil {
		t.Fatal("expected the HPA targeting this workload to match")
	}
	// Same name, different namespace or kind: a different workload entirely.
	if got := Autoscaler(index, ControllerRef{Kind: "Deployment", Name: "web", Namespace: "other"}); got != nil {
		t.Fatal("an HPA in another namespace must not match")
	}
	if got := Autoscaler(index, ControllerRef{Kind: "StatefulSet", Name: "web", Namespace: "api"}); got != nil {
		t.Fatal("an HPA targeting a different kind must not match")
	}
	if got := Autoscaler(nil, ControllerRef{Kind: "Deployment", Name: "web", Namespace: "api"}); got != nil {
		t.Fatal("an empty index must match nothing")
	}
	// A bare pod has no controller, so nothing can be scaling it.
	if got := Autoscaler(index, ControllerRef{}); got != nil {
		t.Fatal("an empty controller ref must match nothing")
	}
}

func TestAutoscalerMetricsRendering(t *testing.T) {
	avg := resource.MustParse("500m")
	cases := []struct {
		name string
		hpa  autoscalingv2.HorizontalPodAutoscaler
		want string
	}{
		{
			"cpu utilisation",
			hpaWith(autoscalingv2.MetricSpec{
				Type:     autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{Name: "cpu", Target: utilTarget(70)},
			}),
			"cpu 70%",
		},
		{
			"average value",
			hpaWith(autoscalingv2.MetricSpec{
				Type:     autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{Name: "cpu", Target: autoscalingv2.MetricTarget{AverageValue: &avg}},
			}),
			"cpu 500m",
		},
		{
			"external metric",
			hpaWith(autoscalingv2.MetricSpec{
				Type: autoscalingv2.ExternalMetricSourceType,
				External: &autoscalingv2.ExternalMetricSource{
					Metric: autoscalingv2.MetricIdentifier{Name: "queue_depth"},
				},
			}),
			"queue_depth",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := autoscalerMetrics(&tc.hpa); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func hpaWith(m autoscalingv2.MetricSpec) autoscalingv2.HorizontalPodAutoscaler {
	return autoscalingv2.HorizontalPodAutoscaler{
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{Metrics: []autoscalingv2.MetricSpec{m}},
	}
}
