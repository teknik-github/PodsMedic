package metrics

import (
	"strings"
	"testing"
)

func render(r *Registry) string {
	var b strings.Builder
	r.Render(&b)
	return b.String()
}

func TestCounterExposition(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("podsmedic_alerts_total", "Alerts.", "result")
	c.Inc("delivered")
	c.Inc("delivered")
	c.Inc("failed")

	out := render(r)
	for _, want := range []string{
		"# TYPE podsmedic_alerts_total counter",
		`podsmedic_alerts_total{result="delivered"} 2`,
		`podsmedic_alerts_total{result="failed"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestUnlabeledCounterAndGauge(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("podsmedic_sweeps_total", "Sweeps.").Add(5)
	r.NewGauge("podsmedic_pods_scanned", "Pods.").Set(29)

	out := render(r)
	if !strings.Contains(out, "podsmedic_sweeps_total 5\n") {
		t.Fatalf("counter without labels wrong:\n%s", out)
	}
	if !strings.Contains(out, "podsmedic_pods_scanned 29\n") {
		t.Fatalf("gauge wrong:\n%s", out)
	}
}

func TestGaugeSetReplaces(t *testing.T) {
	r := NewRegistry()
	g := r.NewGauge("podsmedic_problems_detected", "Problems.")
	g.Set(3)
	g.Set(1) // latest wins
	if out := render(r); !strings.Contains(out, "podsmedic_problems_detected 1\n") {
		t.Fatalf("gauge should hold the latest value:\n%s", out)
	}
}

func TestHistogramExposition(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("podsmedic_llm_latency_seconds", "Latency.", []float64{1, 5, 10}, "provider")
	h.Observe(0.5, "openai") // <=1
	h.Observe(3, "openai")   // <=5, <=10
	h.Observe(20, "openai")  // only +Inf

	out := render(r)
	for _, want := range []string{
		"# TYPE podsmedic_llm_latency_seconds histogram",
		`podsmedic_llm_latency_seconds_bucket{provider="openai",le="1"} 1`,
		`podsmedic_llm_latency_seconds_bucket{provider="openai",le="5"} 2`,
		`podsmedic_llm_latency_seconds_bucket{provider="openai",le="10"} 2`,
		`podsmedic_llm_latency_seconds_bucket{provider="openai",le="+Inf"} 3`,
		`podsmedic_llm_latency_seconds_count{provider="openai"} 3`,
		`podsmedic_llm_latency_seconds_sum{provider="openai"} 23.5`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestFloatCounter(t *testing.T) {
	r := NewRegistry()
	c := r.NewFloatCounter("podsmedic_llm_cost_usd_total", "Cost.", "provider")
	c.Add(0.5, "openai")
	c.Add(0.25, "openai")
	if out := render(r); !strings.Contains(out, `podsmedic_llm_cost_usd_total{provider="openai"} 0.75`) {
		t.Fatalf("float counter should accumulate:\n%s", out)
	}
}

func TestLabelValueEscaping(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("podsmedic_test", "T.", "msg")
	c.Inc(`a"b\c`)
	if out := render(r); !strings.Contains(out, `podsmedic_test{msg="a\"b\\c"} 1`) {
		t.Fatalf("label value not escaped:\n%s", out)
	}
}
