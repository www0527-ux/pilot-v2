package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"gopkg.in/yaml.v3"
)

func TestGatewayPropagatesActualClientSpan(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldProvider, oldPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	defer func() { otel.SetTracerProvider(oldProvider); otel.SetTextMapPropagator(oldPropagator) }()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer provider.Shutdown(context.Background())
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	ctx, parent := provider.Tracer("test").Start(context.Background(), "gateway request")
	defer parent.End()
	var received trace.SpanContext
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = trace.SpanContextFromContext(otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header)))
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer downstream.Close()
	engine := gin.New()
	engine.GET("/checkout", gateway(downstream.Client(), downstream.URL))
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest("GET", "/checkout", nil).WithContext(ctx))
	if response.Code != 200 || response.Header().Get("X-Trace-ID") != parent.SpanContext().TraceID().String() {
		t.Fatalf("response status=%d trace=%s", response.Code, response.Header().Get("X-Trace-ID"))
	}
	ended := recorder.Ended()
	if len(ended) != 1 || ended[0].SpanKind() != trace.SpanKindClient || ended[0].Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Fatalf("expected one real client span linked to the request, got %d", len(ended))
	}
	if !received.IsRemote() || received.TraceID() != parent.SpanContext().TraceID() || received.SpanID() != ended[0].SpanContext().SpanID() {
		t.Fatal("downstream did not receive the actual client span's traceparent")
	}
	if ended[0].EndTime().Sub(ended[0].StartTime()) < 20*time.Millisecond {
		t.Fatal("client span does not include actual downstream wait")
	}
}

func TestDelayControlAndCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	control := &delayControl{}
	engine := gin.New()
	engine.POST("/__demo/delay", control.update)
	engine.GET("/inventory", inventory(control))
	for _, tc := range []struct {
		name, peer, body string
		status           int
	}{
		{"remote cannot control", "192.0.2.1:1234", `{"delay_ms":1500,"ttl_seconds":60}`, 403},
		{"limit", "127.0.0.1:1234", `{"delay_ms":3000,"ttl_seconds":60}`, 400},
		{"unknown field", "127.0.0.1:1234", `{"delay_ms":1500,"ttl_seconds":60,"secret":1}`, 400},
		{"trailing JSON", "127.0.0.1:1234", `{"delay_ms":1500,"ttl_seconds":60}{}`, 400},
		{"unbounded lifetime", "127.0.0.1:1234", `{"delay_ms":1500,"ttl_seconds":0}`, 400},
		{"local operator", "127.0.0.1:1234", `{"delay_ms":1500,"ttl_seconds":60}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/__demo/delay", strings.NewReader(tc.body))
			request.RemoteAddr = tc.peer
			request.Header.Set("X-Forwarded-For", "127.0.0.1")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("status %d, want %d", response.Code, tc.status)
			}
		})
	}
	if control.duration() != 1500*time.Millisecond {
		t.Fatal("local switch was not applied")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response := httptest.NewRecorder()
	started := time.Now()
	engine.ServeHTTP(response, httptest.NewRequest("GET", "/inventory", nil).WithContext(ctx))
	if response.Code != 499 || time.Since(started) > 200*time.Millisecond {
		t.Fatal("cancelled request kept waiting or was reported as HTTP 200")
	}
	control.mu.Lock()
	control.until = time.Now().Add(-time.Second)
	control.mu.Unlock()
	if control.duration() != 40*time.Millisecond {
		t.Fatal("expired fault did not recover automatically")
	}
}

func TestIsolatedConfigRetainsReferencesAndSeparatesStorage(t *testing.T) {
	raw := []byte("llm:\n  main_model:\n    api_key: ${DEEPSEEK_API_KEY}\ndatabase:\n  name: pilot\n  password: ${MYSQL_ROOT_PASSWORD}\nelasticsearch:\n  addresses: [http://localhost:9200]\n")
	data, err := isolatedConfig(raw, 768)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "${DEEPSEEK_API_KEY}") || !strings.Contains(string(data), "${MYSQL_ROOT_PASSWORD}") {
		t.Fatal("env references were removed or expanded")
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if document["database"].(map[string]any)["name"] != "pilot_demo" || document["redis"].(map[string]any)["db"] != 1 {
		t.Fatal("demo storage was not isolated")
	}
	if document["elasticsearch"].(map[string]any)["knowledge_index"] != "pilot_demo_knowledge_768" {
		t.Fatal("reference knowledge index not separated")
	}
	if _, err := isolatedConfig([]byte("elasticsearch: {}"), 768); err == nil {
		t.Fatal("missing source endpoint should fail instead of panicking")
	}
}

func TestDemoMetricsExcludeControlTrafficAndProductionMetricNames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := prometheus.NewRegistry()
	engine := gin.New()
	engine.Use(demoMetrics(registry))
	engine.GET("/health", func(c *gin.Context) { c.Status(200) })
	engine.POST("/__demo/delay", func(c *gin.Context) { c.Status(200) })
	engine.GET("/checkout", func(c *gin.Context) { c.Status(200) })
	for _, request := range []*http.Request{
		httptest.NewRequest("GET", "/health", nil),
		httptest.NewRequest("POST", "/__demo/delay", nil),
		httptest.NewRequest("GET", "/checkout", nil),
	} {
		engine.ServeHTTP(httptest.NewRecorder(), request)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 2 {
		t.Fatalf("expected only demo counter/histogram, got %d families", len(families))
	}
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), "pilot_demo_http_") || len(family.Metric) != 1 {
			t.Fatal("control traffic or production metrics leaked into the demo registry")
		}
		if family.Metric[0].GetCounter() != nil && family.Metric[0].GetCounter().GetValue() != 1 {
			t.Fatal("business request counter should be 1")
		}
		if family.Metric[0].GetHistogram() != nil && family.Metric[0].GetHistogram().GetSampleCount() != 1 {
			t.Fatal("latency histogram should contain one actual business observation")
		}
	}
}

func TestOperatorSwitchBypassesTelemetryButBusinessDoesNot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	observed := 0
	engine := businessRouter("inventory", func(c *gin.Context) { observed++; c.Next() }, "")
	request := httptest.NewRequest("POST", "/__demo/delay", strings.NewReader(`{"delay_ms":0,"ttl_seconds":60}`))
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != 200 || observed != 0 {
		t.Fatal("operator switch must not produce a clue in telemetry")
	}
	response = httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest("GET", "/inventory", nil))
	if response.Code != 200 || observed != 1 {
		t.Fatal("actual business requests must still be observed")
	}
}
