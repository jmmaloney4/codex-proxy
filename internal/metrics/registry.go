// Package metrics provides a tiny, dependency-free Prometheus metrics registry.
//
// We deliberately avoid github.com/prometheus/client_golang: codex-proxy also
// builds as a Cloudflare Worker (GOOS=js GOARCH=wasm, see justfile build-worker),
// and the upstream client's process/Go collectors pull in procfs and other
// packages that do not cross-compile cleanly to js/wasm. This registry is pure
// Go (sync/sort/strconv/strings only) so it stays portable, and the metric set
// we expose is small and well defined.
//
// The exposition output follows the Prometheus text format version 0.0.4.
package metrics

import (
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type metricType int

const (
	counterType metricType = iota
	gaugeType
	histogramType
)

func (t metricType) String() string {
	switch t {
	case counterType:
		return "counter"
	case gaugeType:
		return "gauge"
	case histogramType:
		return "histogram"
	default:
		return "untyped"
	}
}

// seriesSep separates label values when building a series key. It is a byte that
// cannot appear in a UTF-8 label value, so distinct label sets never collide.
const seriesSep = "\xff"

type sample struct {
	labelValues []string

	// counter / gauge
	value float64

	// histogram: bucketCounts[i] holds observations that fell into finite
	// bucket i (value <= buckets[i] and > buckets[i-1]); cumulative counts are
	// computed at render time. Observations larger than the last finite bucket
	// are reflected only in count (the implicit +Inf bucket).
	bucketCounts []uint64
	sum          float64
	count        uint64
}

type family struct {
	name       string
	help       string
	typ        metricType
	labelNames []string
	buckets    []float64 // histogram only, ascending, finite
	series     map[string]*sample
}

// Registry is a concurrency-safe collection of metric families.
type Registry struct {
	mu       sync.Mutex
	families map[string]*family
	order    []string // registration order, for stable output
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{families: make(map[string]*family)}
}

func (r *Registry) register(name, help string, typ metricType, labelNames []string, buckets []float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.families[name]; exists {
		// Re-registration is a programming error; keep the first definition.
		return
	}
	ln := append([]string(nil), labelNames...)
	var bk []float64
	if typ == histogramType {
		bk = append([]float64(nil), buckets...)
		sort.Float64s(bk)
	}
	r.families[name] = &family{
		name:       name,
		help:       help,
		typ:        typ,
		labelNames: ln,
		buckets:    bk,
		series:     make(map[string]*sample),
	}
	r.order = append(r.order, name)
}

// RegisterCounter declares a counter metric. labelNames may be empty.
func (r *Registry) RegisterCounter(name, help string, labelNames []string) {
	r.register(name, help, counterType, labelNames, nil)
}

// RegisterGauge declares a gauge metric.
func (r *Registry) RegisterGauge(name, help string, labelNames []string) {
	r.register(name, help, gaugeType, labelNames, nil)
}

// RegisterHistogram declares a histogram metric. buckets are upper bounds (le);
// the implicit +Inf bucket is always added at render time.
func (r *Registry) RegisterHistogram(name, help string, labelNames []string, buckets []float64) {
	r.register(name, help, histogramType, labelNames, buckets)
}

// resolve finds (or lazily creates) the series for the given label set. It must
// be called with r.mu held. Returns nil if the metric is not registered or its
// type does not match the expected one.
func (r *Registry) resolve(name string, typ metricType, labels map[string]string) (*family, *sample) {
	f := r.families[name]
	if f == nil || f.typ != typ {
		return nil, nil
	}
	vals := make([]string, len(f.labelNames))
	for i, ln := range f.labelNames {
		vals[i] = labels[ln]
	}
	key := strings.Join(vals, seriesSep)
	s := f.series[key]
	if s == nil {
		s = &sample{labelValues: vals}
		if f.typ == histogramType {
			s.bucketCounts = make([]uint64, len(f.buckets))
		}
		f.series[key] = s
	}
	return f, s
}

// CounterAdd increments a counter series by delta (delta must be >= 0; negative
// deltas are ignored, since counters only go up).
func (r *Registry) CounterAdd(name string, labels map[string]string, delta float64) {
	if delta < 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, s := r.resolve(name, counterType, labels); s != nil {
		s.value += delta
	}
}

// CounterInc increments a counter series by 1.
func (r *Registry) CounterInc(name string, labels map[string]string) {
	r.CounterAdd(name, labels, 1)
}

// GaugeSet sets a gauge series to value.
func (r *Registry) GaugeSet(name string, labels map[string]string, value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, s := r.resolve(name, gaugeType, labels); s != nil {
		s.value = value
	}
}

// HistogramObserve records a single observation for a histogram series.
func (r *Registry) HistogramObserve(name string, labels map[string]string, value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, s := r.resolve(name, histogramType, labels)
	if s == nil {
		return
	}
	for i, ub := range f.buckets {
		if value <= ub {
			s.bucketCounts[i]++
			break
		}
	}
	s.sum += value
	s.count++
}

// WriteText writes all metrics in Prometheus text exposition format (v0.0.4).
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var b strings.Builder
	for _, name := range r.order {
		f := r.families[name]
		if f.help != "" {
			b.WriteString("# HELP ")
			b.WriteString(name)
			b.WriteByte(' ')
			b.WriteString(escapeHelp(f.help))
			b.WriteByte('\n')
		}
		b.WriteString("# TYPE ")
		b.WriteString(name)
		b.WriteByte(' ')
		b.WriteString(f.typ.String())
		b.WriteByte('\n')

		for _, s := range sortedSeries(f.series) {
			switch f.typ {
			case histogramType:
				writeHistogram(&b, f, s)
			default:
				b.WriteString(name)
				writeLabels(&b, f.labelNames, s.labelValues, "", "")
				b.WriteByte(' ')
				b.WriteString(formatFloat(s.value))
				b.WriteByte('\n')
			}
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func writeHistogram(b *strings.Builder, f *family, s *sample) {
	var cumulative uint64
	for i, ub := range f.buckets {
		cumulative += s.bucketCounts[i]
		b.WriteString(f.name)
		b.WriteString("_bucket")
		writeLabels(b, f.labelNames, s.labelValues, "le", formatFloat(ub))
		b.WriteByte(' ')
		b.WriteString(strconv.FormatUint(cumulative, 10))
		b.WriteByte('\n')
	}
	// Implicit +Inf bucket equals the total observation count.
	b.WriteString(f.name)
	b.WriteString("_bucket")
	writeLabels(b, f.labelNames, s.labelValues, "le", "+Inf")
	b.WriteByte(' ')
	b.WriteString(strconv.FormatUint(s.count, 10))
	b.WriteByte('\n')

	b.WriteString(f.name)
	b.WriteString("_sum")
	writeLabels(b, f.labelNames, s.labelValues, "", "")
	b.WriteByte(' ')
	b.WriteString(formatFloat(s.sum))
	b.WriteByte('\n')

	b.WriteString(f.name)
	b.WriteString("_count")
	writeLabels(b, f.labelNames, s.labelValues, "", "")
	b.WriteByte(' ')
	b.WriteString(strconv.FormatUint(s.count, 10))
	b.WriteByte('\n')
}

// writeLabels renders {name="value",...}. If extraName is non-empty it is
// appended as a final label (used for the histogram le label).
func writeLabels(b *strings.Builder, names, values []string, extraName, extraValue string) {
	if len(names) == 0 && extraName == "" {
		return
	}
	b.WriteByte('{')
	first := true
	for i, n := range names {
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(values[i]))
		b.WriteByte('"')
	}
	if extraName != "" {
		if !first {
			b.WriteByte(',')
		}
		b.WriteString(extraName)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(extraValue))
		b.WriteByte('"')
	}
	b.WriteByte('}')
}

func sortedSeries(m map[string]*sample) []*sample {
	out := make([]*sample, 0, len(m))
	for _, s := range m {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].labelValues, out[j].labelValues
		for k := 0; k < len(a) && k < len(b); k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return len(a) < len(b)
	})
	return out
}

// formatFloat renders a float the way Prometheus expects: integers without a
// decimal point, everything else with the shortest round-trippable form.
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func escapeHelp(s string) string {
	// HELP text escapes backslash and newline only.
	if !strings.ContainsAny(s, "\\\n") {
		return s
	}
	r := strings.NewReplacer("\\", `\\`, "\n", `\n`)
	return r.Replace(s)
}

func escapeLabelValue(s string) string {
	// Label values escape backslash, double-quote, and newline.
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	r := strings.NewReplacer("\\", `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}
