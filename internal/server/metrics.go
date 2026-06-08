package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/dvcrn/codex-proxy/internal/metrics"
)

// Metric names exposed on /metrics. Kept together so the contract is easy to audit.
const (
	metricBuildInfo       = "codex_proxy_build_info"
	metricRequestsTotal   = "codex_proxy_requests_total"
	metricRequestDuration = "codex_proxy_request_duration_seconds"
	metricTokensTotal     = "codex_proxy_tokens_total"
	metricTokenRefreshes  = "codex_proxy_upstream_token_refreshes_total"
	metricCredsExpiresAt  = "codex_proxy_credentials_expires_at_seconds"
)

// durationBuckets mirrors the Prometheus client default buckets (seconds),
// which cover the sub-second to ten-second range typical of LLM proxy calls.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// newMetrics builds the registry and declares every family up front so the
// exposition output is stable and self-documenting even before any traffic.
func newMetrics() *metrics.Registry {
	r := metrics.NewRegistry()
	r.RegisterGauge(metricBuildInfo, "Build information; constant 1 labelled by version.", []string{"version"})
	r.RegisterCounter(metricRequestsTotal, "Total HTTP requests handled, by route, method and status code.", []string{"route", "method", "status"})
	r.RegisterHistogram(metricRequestDuration, "HTTP request latency in seconds, by route and method.", []string{"route", "method"}, durationBuckets)
	r.RegisterCounter(metricTokensTotal, "Total tokens reported by upstream Codex usage, by model and type (prompt|completion).", []string{"model", "type"})
	r.RegisterCounter(metricTokenRefreshes, "Total OAuth token refresh attempts triggered by upstream 401s, by result (success|failure).", []string{"result"})
	r.RegisterGauge(metricCredsExpiresAt, "Unix timestamp (seconds) at which the active Codex credential expires; 0 if unknown.", nil)
	return r
}

// Registry exposes the metrics registry so the entrypoint can wire a separate
// metrics listener and seed process-level gauges.
func (s *Server) Registry() *metrics.Registry { return s.metrics }

// SetBuildInfo records the build_info gauge for the given version string.
func (s *Server) SetBuildInfo(version string) {
	s.metrics.GaugeSet(metricBuildInfo, map[string]string{"version": version}, 1)
}

// SetCredentialsExpiry records when the active credential expires (unix seconds).
func (s *Server) SetCredentialsExpiry(unixSeconds float64) {
	s.metrics.GaugeSet(metricCredsExpiresAt, nil, unixSeconds)
}

func (s *Server) recordTokenUsage(model string, prompt, completion int) {
	if prompt > 0 {
		s.metrics.CounterAdd(metricTokensTotal, map[string]string{"model": model, "type": "prompt"}, float64(prompt))
	}
	if completion > 0 {
		s.metrics.CounterAdd(metricTokensTotal, map[string]string{"model": model, "type": "completion"}, float64(completion))
	}
}

// usageFromTransformedChunk extracts the usage object from an OpenAI-style
// chat.completion.chunk emitted by the SSE transformer. Only the stream's final
// chunk carries usage, so the cheap substring guard skips the common case.
func usageFromTransformedChunk(chunk []byte) (Usage, bool) {
	if !bytes.Contains(chunk, []byte(`"usage"`)) {
		return Usage{}, false
	}
	var parsed struct {
		Usage *Usage `json:"usage"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(chunk), &parsed); err != nil || parsed.Usage == nil {
		return Usage{}, false
	}
	return *parsed.Usage, true
}

// usageFromUpstreamResponseEvent extracts token usage from a raw upstream Codex
// SSE event on the /v1/responses passthrough path. Usage lives on the
// "response.completed" event under response.usage, which (depending on the
// model transport) uses input_tokens/output_tokens or prompt_tokens/
// completion_tokens. The substring guard skips the many events that carry none.
func usageFromUpstreamResponseEvent(raw []byte) (Usage, bool) {
	if !bytes.Contains(raw, []byte(`"usage"`)) {
		return Usage{}, false
	}
	var evt struct {
		Response struct {
			Usage *struct {
				InputTokens      int `json:"input_tokens"`
				OutputTokens     int `json:"output_tokens"`
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &evt); err != nil || evt.Response.Usage == nil {
		return Usage{}, false
	}
	u := evt.Response.Usage
	prompt := u.InputTokens
	if prompt == 0 {
		prompt = u.PromptTokens
	}
	completion := u.OutputTokens
	if completion == 0 {
		completion = u.CompletionTokens
	}
	if prompt == 0 && completion == 0 {
		return Usage{}, false
	}
	return Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: u.TotalTokens}, true
}

func (s *Server) recordTokenRefresh(success bool) {
	result := "failure"
	if success {
		result = "success"
	}
	s.metrics.CounterInc(metricTokenRefreshes, map[string]string{"result": result})
}

// MetricsHandler serves the registry in Prometheus text exposition format.
// It is intended to be mounted on a separate, cluster-internal listener (see
// cmd/codex-proxy), never on the public API port.
func (s *Server) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if err := s.metrics.WriteText(w); err != nil {
			s.logger.Error().Err(err).Msg("Failed to write metrics")
		}
	})
}

// statusRecorder wraps a ResponseWriter to capture the response status code
// while preserving http.Flusher so streaming (SSE) responses keep flushing.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}

// Flush implements http.Flusher, delegating when the underlying writer supports
// it. The method is always present so writeResponse's w.(http.Flusher) assertion
// continues to succeed through this wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// metricsMiddleware records request counts and latency for every route. It uses
// the mux-matched pattern (r.Pattern) as the route label to keep cardinality
// bounded rather than the raw request URI.
func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		elapsed := time.Since(start).Seconds()

		// r.Pattern is the mux-matched pattern (bounded cardinality). It is only
		// empty for synthesised responses like the trailing-slash redirect; fall
		// back to a constant so those never leak the raw URI into the label.
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		labels := map[string]string{"route": route, "method": r.Method, "status": strconv.Itoa(rec.status)}
		s.metrics.CounterInc(metricRequestsTotal, labels)
		s.metrics.HistogramObserve(metricRequestDuration, map[string]string{"route": route, "method": r.Method}, elapsed)
	})
}
