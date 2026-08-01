package k8s

import "testing"

func TestParsePodMetricsNormalizes(t *testing.T) {
	raw := []byte(`{
		"containers": [
			{"name": "web", "usage": {"cpu": "12m", "memory": "215040Ki"}},
			{"name": "sidecar", "usage": {"cpu": "0", "memory": "8Mi"}}
		]
	}`)

	got := parsePodMetrics(raw)
	if got == nil {
		t.Fatal("expected usage, got nil")
	}
	if got["web"]["cpu"] != "12m" {
		t.Fatalf("web cpu = %q, want 12m", got["web"]["cpu"])
	}
	// 215040Ki is exactly 210Mi; resource.Quantity keeps binary form as Mi.
	if got["web"]["memory"] != "210Mi" {
		t.Fatalf("web memory = %q, want 210Mi", got["web"]["memory"])
	}
	if got["sidecar"]["memory"] != "8Mi" {
		t.Fatalf("sidecar memory = %q, want 8Mi", got["sidecar"]["memory"])
	}
}

func TestParsePodMetricsRejectsGarbage(t *testing.T) {
	if got := parsePodMetrics([]byte("not json")); got != nil {
		t.Fatalf("garbage should yield nil, got %v", got)
	}
	// A container whose usage values are all unparseable is dropped entirely.
	raw := []byte(`{"containers":[{"name":"web","usage":{"memory":"bogus"}}]}`)
	if got := parsePodMetrics(raw); got != nil {
		t.Fatalf("unparseable usage should yield nil, got %v", got)
	}
}

func TestMergeUsageMemAndCPU(t *testing.T) {
	raw := []byte(`{"items":[
		{"metadata":{"name":"web-1","namespace":"api"},
		 "containers":[{"name":"web","usage":{"cpu":"250m","memory":"256Mi"}}]},
		{"metadata":{"name":"web-2","namespace":"api"},
		 "containers":[{"name":"web","usage":{"cpu":"1"}}]}
	]}`)
	out := map[string]usageVals{}
	mergeUsage(out, raw)
	if got := out["api/web-1/web"].memBytes; got != 256*1024*1024 {
		t.Fatalf("web-1 memory = %d, want %d", got, 256*1024*1024)
	}
	if got := out["api/web-1/web"].cpuMilli; got != 250 {
		t.Fatalf("web-1 cpu = %dm, want 250m", got)
	}
	// "1" CPU = 1000m; memory absent → 0.
	if got := out["api/web-2/web"].cpuMilli; got != 1000 {
		t.Fatalf("web-2 cpu = %dm, want 1000m", got)
	}
	if got := out["api/web-2/web"].memBytes; got != 0 {
		t.Fatalf("web-2 memory = %d, want 0", got)
	}
}
