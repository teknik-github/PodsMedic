// Package metrics is a tiny, dependency-free Prometheus-exposition registry —
// enough counters, gauges, and histograms for podsmedic to report its own
// health, matching the net/http-only stance of the rest of the project.
package metrics

import (
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds a set of metrics and writes them in Prometheus text format.
type Registry struct {
	mu      sync.Mutex
	ordered []exposer
}

type exposer interface{ write(w io.Writer) }

// NewRegistry builds an empty registry.
func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) add(e exposer) {
	r.mu.Lock()
	r.ordered = append(r.ordered, e)
	r.mu.Unlock()
}

// Render writes every metric in Prometheus text format. Registry order is
// preserved; series within a metric are sorted so output is stable.
func (r *Registry) Render(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.ordered {
		e.write(w)
	}
}

// --- counter ---

// CounterVec is a set of monotonically increasing counters sharing a name,
// partitioned by label values.
type CounterVec struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	series     map[string]*uint64
}

// NewCounter registers a counter, optionally partitioned by the given labels.
func (r *Registry) NewCounter(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{name: name, help: help, labels: labels, series: map[string]*uint64{}}
	r.add(c)
	return c
}

// Inc adds one to the series for the given label values (order matches the
// labels passed to NewCounter).
func (c *CounterVec) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Add increases the series by n.
func (c *CounterVec) Add(n uint64, labelValues ...string) {
	p := c.get(labelValues)
	atomic.AddUint64(p, n)
}

func (c *CounterVec) get(lv []string) *uint64 {
	key := joinValues(lv)
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.series[key]
	if !ok {
		var v uint64
		p = &v
		c.series[key] = p
	}
	return p
}

func (c *CounterVec) write(w io.Writer) {
	writeHeader(w, c.name, c.help, "counter")
	c.mu.Lock()
	keys := sortedKeys(c.series)
	c.mu.Unlock()
	for _, k := range keys {
		io.WriteString(w, c.name)
		io.WriteString(w, labelString(c.labels, k))
		io.WriteString(w, " "+strconv.FormatUint(atomic.LoadUint64(c.series[k]), 10)+"\n")
	}
}

// --- float counter ---

// FloatCounterVec is a monotonic counter carrying a fractional value (e.g. an
// accumulating cost in USD), which the integer CounterVec cannot represent.
type FloatCounterVec struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	series     map[string]*uint64 // float64 bits
}

// NewFloatCounter registers a float-valued counter.
func (r *Registry) NewFloatCounter(name, help string, labels ...string) *FloatCounterVec {
	c := &FloatCounterVec{name: name, help: help, labels: labels, series: map[string]*uint64{}}
	r.add(c)
	return c
}

// Add increases the series by v (v should be non-negative for counter
// semantics).
func (c *FloatCounterVec) Add(v float64, labelValues ...string) {
	key := joinValues(labelValues)
	c.mu.Lock()
	p, ok := c.series[key]
	if !ok {
		var b uint64
		p = &b
		c.series[key] = p
	}
	c.mu.Unlock()
	for {
		old := atomic.LoadUint64(p)
		nw := math.Float64bits(math.Float64frombits(old) + v)
		if atomic.CompareAndSwapUint64(p, old, nw) {
			return
		}
	}
}

func (c *FloatCounterVec) write(w io.Writer) {
	writeHeader(w, c.name, c.help, "counter")
	c.mu.Lock()
	keys := sortedKeys(c.series)
	c.mu.Unlock()
	for _, k := range keys {
		v := math.Float64frombits(atomic.LoadUint64(c.series[k]))
		io.WriteString(w, c.name)
		io.WriteString(w, labelString(c.labels, k))
		io.WriteString(w, " "+formatFloat(v)+"\n")
	}
}

// --- gauge ---

// GaugeVec is a set of gauges (values that go up and down).
type GaugeVec struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	series     map[string]*uint64 // float64 bits
}

// NewGauge registers a gauge.
func (r *Registry) NewGauge(name, help string, labels ...string) *GaugeVec {
	g := &GaugeVec{name: name, help: help, labels: labels, series: map[string]*uint64{}}
	r.add(g)
	return g
}

// Set replaces the gauge's value for the given label values.
func (g *GaugeVec) Set(v float64, labelValues ...string) {
	key := joinValues(labelValues)
	g.mu.Lock()
	p, ok := g.series[key]
	if !ok {
		var b uint64
		p = &b
		g.series[key] = p
	}
	g.mu.Unlock()
	atomic.StoreUint64(p, math.Float64bits(v))
}

func (g *GaugeVec) write(w io.Writer) {
	writeHeader(w, g.name, g.help, "gauge")
	g.mu.Lock()
	keys := sortedKeys(g.series)
	g.mu.Unlock()
	for _, k := range keys {
		v := math.Float64frombits(atomic.LoadUint64(g.series[k]))
		io.WriteString(w, g.name)
		io.WriteString(w, labelString(g.labels, k))
		io.WriteString(w, " "+formatFloat(v)+"\n")
	}
}

// --- histogram ---

// HistogramVec is a set of cumulative histograms with fixed buckets.
type HistogramVec struct {
	name, help string
	labels     []string
	buckets    []float64
	mu         sync.Mutex
	series     map[string]*histSeries
}

type histSeries struct {
	counts []uint64 // per bucket, cumulative computed at write time
	sum    uint64   // float64 bits
	total  uint64
}

// NewHistogram registers a histogram with the given upper-bound buckets (a
// +Inf bucket is added automatically).
func (r *Registry) NewHistogram(name, help string, buckets []float64, labels ...string) *HistogramVec {
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	h := &HistogramVec{name: name, help: help, labels: labels, buckets: b, series: map[string]*histSeries{}}
	r.add(h)
	return h
}

// Observe records one value.
func (h *HistogramVec) Observe(v float64, labelValues ...string) {
	key := joinValues(labelValues)
	h.mu.Lock()
	s, ok := h.series[key]
	if !ok {
		s = &histSeries{counts: make([]uint64, len(h.buckets))}
		h.series[key] = s
	}
	for i, ub := range h.buckets {
		if v <= ub {
			s.counts[i]++
		}
	}
	s.total++
	s.sum = math.Float64bits(math.Float64frombits(s.sum) + v)
	h.mu.Unlock()
}

func (h *HistogramVec) write(w io.Writer) {
	writeHeader(w, h.name, h.help, "histogram")
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range sortedKeys(h.series) {
		s := h.series[k]
		base := labelPairs(h.labels, k)
		// counts are already cumulative (Observe bumps every bucket a value
		// falls under), so emit them directly.
		for i, ub := range h.buckets {
			io.WriteString(w, h.name+"_bucket"+withLE(base, formatFloat(ub))+" "+strconv.FormatUint(s.counts[i], 10)+"\n")
		}
		io.WriteString(w, h.name+"_bucket"+withLE(base, "+Inf")+" "+strconv.FormatUint(s.total, 10)+"\n")
		io.WriteString(w, h.name+"_sum"+bracket(base)+" "+formatFloat(math.Float64frombits(s.sum))+"\n")
		io.WriteString(w, h.name+"_count"+bracket(base)+" "+strconv.FormatUint(s.total, 10)+"\n")
	}
}

// --- shared helpers ---

const valueSep = "\xff"

func joinValues(lv []string) string { return strings.Join(lv, valueSep) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func writeHeader(w io.Writer, name, help, typ string) {
	io.WriteString(w, "# HELP "+name+" "+help+"\n")
	io.WriteString(w, "# TYPE "+name+" "+typ+"\n")
}

type labelPair struct{ name, value string }

func labelPairs(names []string, key string) []labelPair {
	if len(names) == 0 || key == "" {
		return nil
	}
	vals := strings.Split(key, valueSep)
	out := make([]labelPair, 0, len(names))
	for i, n := range names {
		v := ""
		if i < len(vals) {
			v = vals[i]
		}
		out = append(out, labelPair{n, v})
	}
	return out
}

// labelString renders {a="1",b="2"} for a counter/gauge series key.
func labelString(names []string, key string) string { return bracket(labelPairs(names, key)) }

func bracket(pairs []labelPair) string {
	if len(pairs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(p.name + `="` + escape(p.value) + `"`)
	}
	b.WriteByte('}')
	return b.String()
}

// withLE renders the histogram bucket labels plus the le bound.
func withLE(base []labelPair, le string) string {
	return bracket(append(append([]labelPair(nil), base...), labelPair{"le", le}))
}

func escape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

func formatFloat(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
