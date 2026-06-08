package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

type fakeCreds struct{}

func (fakeCreds) GetCredentials() (string, string, error) { return "tok", "acct", nil }
func (fakeCreds) RefreshCredentials() error               { return nil }

func newTestServer() *Server { return New(zerolog.Nop(), fakeCreds{}) }

func scrape(t *testing.T, s *Server) string {
	t.Helper()
	rr := httptest.NewRecorder()
	s.MetricsHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics scrape status = %d, want 200", rr.Code)
	}
	return rr.Body.String()
}

func TestMetricsMiddlewareRecordsRequestAndDuration(t *testing.T) {
	s := newTestServer()
	s.SetBuildInfo("test-1.2.3")

	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/health status = %d, want 200", rr.Code)
	}

	body := scrape(t, s)
	for _, want := range []string{
		`codex_proxy_build_info{version="test-1.2.3"} 1`,
		`codex_proxy_requests_total{route="/health",method="GET",status="200"} 1`,
		`codex_proxy_request_duration_seconds_count{route="/health",method="GET"} 1`,
		`codex_proxy_request_duration_seconds_bucket{route="/health",method="GET",le="+Inf"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q\n---\n%s", want, body)
		}
	}
}

func TestMetricsHandlerContentTypeAndMethod(t *testing.T) {
	s := newTestServer()

	rr := httptest.NewRecorder()
	s.MetricsHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if ct := rr.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("unexpected content-type %q", ct)
	}

	rrPost := httptest.NewRecorder()
	s.MetricsHandler().ServeHTTP(rrPost, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rrPost.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /metrics status = %d, want 405", rrPost.Code)
	}
}

func TestRecordTokenUsage(t *testing.T) {
	s := newTestServer()
	s.recordTokenUsage("gpt-5", 100, 40)
	s.recordTokenUsage("gpt-5", 0, 0) // zero usage should not add series noise

	body := scrape(t, s)
	for _, want := range []string{
		`codex_proxy_tokens_total{model="gpt-5",type="prompt"} 100`,
		`codex_proxy_tokens_total{model="gpt-5",type="completion"} 40`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
}

func TestRecordTokenRefresh(t *testing.T) {
	s := newTestServer()
	s.recordTokenRefresh(true)
	s.recordTokenRefresh(false)
	s.recordTokenRefresh(false)

	body := scrape(t, s)
	if !strings.Contains(body, `codex_proxy_upstream_token_refreshes_total{result="success"} 1`) {
		t.Fatalf("missing success refresh counter:\n%s", body)
	}
	if !strings.Contains(body, `codex_proxy_upstream_token_refreshes_total{result="failure"} 2`) {
		t.Fatalf("missing failure refresh counter:\n%s", body)
	}
}

func TestSetCredentialsExpiry(t *testing.T) {
	s := newTestServer()
	s.SetCredentialsExpiry(1700000000)
	if body := scrape(t, s); !strings.Contains(body, "codex_proxy_credentials_expires_at_seconds 1.7e+09") {
		t.Fatalf("expiry gauge not set as expected:\n%s", body)
	}
}

func TestStatusRecorderCapturesStatusAndPreservesFlusher(t *testing.T) {
	// httptest.ResponseRecorder implements http.Flusher.
	rr := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: rr, status: http.StatusOK}

	if _, ok := http.ResponseWriter(rec).(http.Flusher); !ok {
		t.Fatal("statusRecorder must implement http.Flusher so SSE streaming keeps working")
	}

	rec.WriteHeader(http.StatusNotFound)
	rec.WriteHeader(http.StatusInternalServerError) // ignored; first status wins
	if rec.status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.status)
	}
	rec.Flush()
	if !rr.Flushed {
		t.Fatal("Flush did not propagate to the underlying ResponseWriter")
	}
}

func TestStatusRecorderDefaultsTo200OnWrite(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: rr, status: http.StatusOK}
	if _, err := rec.Write([]byte("ok")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if rec.status != http.StatusOK || !rec.wroteHeader {
		t.Fatalf("expected implicit 200 on first write, got status=%d wrote=%v", rec.status, rec.wroteHeader)
	}
}

func TestUsageFromUpstreamResponseEvent(t *testing.T) {
	completed := []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":50,"output_tokens":7,"total_tokens":57}}}`)
	u, ok := usageFromUpstreamResponseEvent(completed)
	if !ok || u.PromptTokens != 50 || u.CompletionTokens != 7 || u.TotalTokens != 57 {
		t.Fatalf("unexpected parse: %+v ok=%v", u, ok)
	}

	// prompt_tokens/completion_tokens variant.
	alt := []byte(`{"type":"response.completed","response":{"usage":{"prompt_tokens":9,"completion_tokens":2}}}`)
	if u, ok := usageFromUpstreamResponseEvent(alt); !ok || u.PromptTokens != 9 || u.CompletionTokens != 2 {
		t.Fatalf("alt parse failed: %+v ok=%v", u, ok)
	}

	for _, noUsage := range [][]byte{
		[]byte(`{"type":"response.output_text.delta","delta":"hi"}`),
		[]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":0,"output_tokens":0}}}`),
	} {
		if _, ok := usageFromUpstreamResponseEvent(noUsage); ok {
			t.Fatalf("expected no usage for %s", noUsage)
		}
	}
}

func TestPassThroughSSEStreamWithCallbackObservesAndCopies(t *testing.T) {
	in := "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\ndata: [DONE]\n\n"
	var seen [][]byte
	var out strings.Builder
	if err := PassThroughSSEStreamWithCallback(strings.NewReader(in), &out, func(raw []byte) {
		seen = append(seen, append([]byte(nil), raw...))
	}); err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	if len(seen) != 1 || !strings.Contains(string(seen[0]), `"input_tokens":3`) {
		t.Fatalf("callback did not observe the completed event: %v", seen)
	}
	// The stream must still be copied verbatim, including the DONE sentinel.
	if !strings.Contains(out.String(), `"output_tokens":1`) || !strings.Contains(out.String(), "data: [DONE]") {
		t.Fatalf("stream not copied verbatim:\n%s", out.String())
	}
}

func TestUsageFromTransformedChunk(t *testing.T) {
	final := []byte(`{"id":"x","object":"chat.completion.chunk","usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`)
	u, ok := usageFromTransformedChunk(final)
	if !ok || u.PromptTokens != 12 || u.CompletionTokens != 3 {
		t.Fatalf("unexpected parse: %+v ok=%v", u, ok)
	}

	content := []byte(`{"id":"x","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}]}`)
	if _, ok := usageFromTransformedChunk(content); ok {
		t.Fatal("content chunk without usage should not parse usage")
	}
}
