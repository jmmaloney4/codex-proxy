package metrics

import (
	"strings"
	"sync"
	"testing"
)

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	if err := r.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return b.String()
}

func TestCounterAndGaugeExposition(t *testing.T) {
	r := NewRegistry()
	r.RegisterCounter("codex_requests_total", "Total requests.", []string{"route", "status"})
	r.RegisterGauge("codex_build_info", "Build info.", []string{"version"})

	r.CounterInc("codex_requests_total", map[string]string{"route": "/health", "status": "200"})
	r.CounterAdd("codex_requests_total", map[string]string{"route": "/health", "status": "200"}, 2)
	r.GaugeSet("codex_build_info", map[string]string{"version": "1.2.3"}, 1)

	got := render(t, r)
	want := strings.Join([]string{
		"# HELP codex_requests_total Total requests.",
		"# TYPE codex_requests_total counter",
		`codex_requests_total{route="/health",status="200"} 3`,
		"# HELP codex_build_info Build info.",
		"# TYPE codex_build_info gauge",
		`codex_build_info{version="1.2.3"} 1`,
		"",
	}, "\n")
	if got != want {
		t.Fatalf("exposition mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestCounterIgnoresNegativeDelta(t *testing.T) {
	r := NewRegistry()
	r.RegisterCounter("c_total", "", nil)
	r.CounterAdd("c_total", nil, 5)
	r.CounterAdd("c_total", nil, -3)
	if got := render(t, r); !strings.Contains(got, "c_total 5\n") {
		t.Fatalf("expected counter to stay at 5, got:\n%s", got)
	}
}

func TestUnregisteredMetricIsNoop(t *testing.T) {
	r := NewRegistry()
	// Should not panic and should produce no output.
	r.CounterInc("missing_total", map[string]string{"a": "b"})
	r.GaugeSet("missing_gauge", nil, 1)
	if got := render(t, r); got != "" {
		t.Fatalf("expected empty output, got: %q", got)
	}
}

func TestTypeMismatchIsNoop(t *testing.T) {
	r := NewRegistry()
	r.RegisterCounter("c_total", "", nil)
	// Wrong accessor for the registered type -> ignored.
	r.GaugeSet("c_total", nil, 99)
	if got := render(t, r); strings.Contains(got, "99") {
		t.Fatalf("gauge write to a counter should be ignored, got:\n%s", got)
	}
}

func TestLabelEscaping(t *testing.T) {
	r := NewRegistry()
	r.RegisterCounter("c_total", "help with \\ and\nnewline", []string{"path"})
	r.CounterInc("c_total", map[string]string{"path": `a"b\c` + "\n"})
	got := render(t, r)
	if !strings.Contains(got, `# HELP c_total help with \\ and\nnewline`) {
		t.Fatalf("help not escaped:\n%s", got)
	}
	if !strings.Contains(got, `c_total{path="a\"b\\c\n"} 1`) {
		t.Fatalf("label value not escaped:\n%s", got)
	}
}

func TestNoLabelsOmitsBraces(t *testing.T) {
	r := NewRegistry()
	r.RegisterGauge("g", "", nil)
	r.GaugeSet("g", nil, 42)
	if got := render(t, r); !strings.Contains(got, "g 42\n") {
		t.Fatalf("expected 'g 42' with no braces, got:\n%s", got)
	}
}

func TestHistogramCumulativeBuckets(t *testing.T) {
	r := NewRegistry()
	r.RegisterHistogram("lat_seconds", "Latency.", []string{"route"}, []float64{0.1, 0.5, 1})
	lbl := map[string]string{"route": "/v1"}
	for _, v := range []float64{0.05, 0.2, 0.2, 0.7, 5} { // distribution across buckets + overflow
		r.HistogramObserve("lat_seconds", lbl, v)
	}
	got := render(t, r)
	// le=0.1 -> 1 (0.05); le=0.5 -> 3 (+0.2,0.2); le=1 -> 4 (+0.7); +Inf -> 5 (+5)
	for _, want := range []string{
		`lat_seconds_bucket{route="/v1",le="0.1"} 1`,
		`lat_seconds_bucket{route="/v1",le="0.5"} 3`,
		`lat_seconds_bucket{route="/v1",le="1"} 4`,
		`lat_seconds_bucket{route="/v1",le="+Inf"} 5`,
		`lat_seconds_sum{route="/v1"} 6.15`,
		`lat_seconds_count{route="/v1"} 5`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestSeriesAreSortedDeterministically(t *testing.T) {
	r := NewRegistry()
	r.RegisterCounter("c_total", "", []string{"k"})
	r.CounterInc("c_total", map[string]string{"k": "zebra"})
	r.CounterInc("c_total", map[string]string{"k": "apple"})
	r.CounterInc("c_total", map[string]string{"k": "mango"})
	got := render(t, r)
	ai := strings.Index(got, `k="apple"`)
	mi := strings.Index(got, `k="mango"`)
	zi := strings.Index(got, `k="zebra"`)
	if !(ai < mi && mi < zi) {
		t.Fatalf("series not sorted by label value:\n%s", got)
	}
}

func TestConcurrentRecording(t *testing.T) {
	r := NewRegistry()
	r.RegisterCounter("c_total", "", []string{"w"})
	r.RegisterHistogram("h_seconds", "", nil, []float64{0.5, 1})

	const workers, perWorker = 16, 1000
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			lbl := map[string]string{"w": "shared"}
			for i := 0; i < perWorker; i++ {
				r.CounterInc("c_total", lbl)
				r.HistogramObserve("h_seconds", nil, 0.3)
			}
		}(w)
	}
	// Concurrent reader to shake out data races under -race.
	go func() {
		var b strings.Builder
		for i := 0; i < 50; i++ {
			_ = r.WriteText(&b)
			b.Reset()
		}
	}()
	wg.Wait()

	got := render(t, r)
	if !strings.Contains(got, "c_total{w=\"shared\"} 16000\n") {
		t.Fatalf("expected aggregated counter of 16000, got:\n%s", got)
	}
	if !strings.Contains(got, "h_seconds_count 16000\n") {
		t.Fatalf("expected histogram count of 16000, got:\n%s", got)
	}
}
