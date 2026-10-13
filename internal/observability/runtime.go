package observability

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	oteltrace "go.opentelemetry.io/otel/trace"

	"shanyangcode.com/pilot/internal/config"
	"shanyangcode.com/pilot/pkg/elasticsearch"
	"shanyangcode.com/pilot/pkg/types"
)

var (
	metricsRegisterOnce sync.Once

	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pilot_http_requests_total",
			Help: "Total number of processed HTTP requests.",
		},
		[]string{"method", "route", "status"},
	)
	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "pilot_http_request_duration_seconds",
			Help:    "HTTP request latency distribution in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "route", "status"},
	)
	httpRequestsInflight = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pilot_http_requests_inflight",
			Help: "Number of inflight HTTP requests.",
		},
		[]string{"method", "route"},
	)
)

// Runtime 挂接项目自身的指标、日志和 trace 产出。
type Runtime struct {
	logger         *EventLogger
	serviceName    string
	slowRequestMS  int64
	tracerProvider *sdktrace.TracerProvider
	degraded       []string
}

// EventLogger 将运行时日志异步写入 Elasticsearch。
type EventLogger struct {
	es         *elasticsearch.Client
	index      string
	service    string
	queue      chan types.LogEntry
	wg         sync.WaitGroup
	counter    atomic.Uint64
	dropNotice atomic.Uint64
}

// NewRuntime 初始化真实观测运行时。
// 当 cfg.Server.AllowDegraded 为 true 时，OTLP 导出或运行日志索引失败会降级继续启动。
func NewRuntime(ctx context.Context, es *elasticsearch.Client, cfg *config.Config) (*Runtime, error) {
	var degraded []string

	tp, err := initTracing(ctx, cfg)
	if err != nil {
		if !cfg.Server.AllowDegraded {
			return nil, err
		}
		slog.WarnContext(ctx, "trace exporter unavailable; continuing in degraded mode", "error", err)
		degraded = append(degraded, "trace_export")
	}

	logger, err := NewEventLogger(ctx, es, cfg.Observability.Log.Index, cfg.Observability.ServiceName)
	if err != nil {
		if !cfg.Server.AllowDegraded {
			if tp != nil {
				_ = tp.Shutdown(context.Background())
			}
			return nil, err
		}
		slog.WarnContext(ctx, "runtime log writer unavailable; continuing in degraded mode", "error", err)
		degraded = append(degraded, "runtime_logs")
		logger = nil
	}

	metricsRegisterOnce.Do(func() {
		prometheus.MustRegister(httpRequestsTotal, httpRequestDuration, httpRequestsInflight)
	})

	slowRequestMS := cfg.Observability.Log.SlowRequestMS
	if slowRequestMS <= 0 {
		slowRequestMS = 1000
	}

	return &Runtime{
		logger:         logger,
		serviceName:    cfg.Observability.ServiceName,
		slowRequestMS:  slowRequestMS,
		tracerProvider: tp,
		degraded:       degraded,
	}, nil
}

// DegradedCapabilities 返回启动期降级的可选能力名称。
func (r *Runtime) DegradedCapabilities() []string {
	if r == nil || len(r.degraded) == 0 {
		return nil
	}
	out := make([]string, len(r.degraded))
	copy(out, r.degraded)
	return out
}

// BindServer 将 metrics 端点和观测中间件接入 Gin 引擎。
func (r *Runtime) BindServer(engine *gin.Engine) {
	engine.GET("/metrics", gin.WrapH(promhttp.Handler()))
	engine.Use(r.Middleware())
}

// Shutdown 关闭后台 worker 与 trace exporter。
func (r *Runtime) Shutdown(ctx context.Context) error {
	var errs []error
	if r.logger != nil {
		if err := r.logger.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if r.tracerProvider != nil {
		if err := r.tracerProvider.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("shutdown runtime failed: %v", errs)
}

// NewEventLogger 创建异步日志写入器。
func NewEventLogger(ctx context.Context, es *elasticsearch.Client, index, service string) (*EventLogger, error) {
	if index == "" {
		index = "pilot_logs"
	}
	if err := ensureLogIndex(ctx, es, index); err != nil {
		return nil, err
	}

	logger := &EventLogger{
		es:      es,
		index:   index,
		service: service,
		queue:   make(chan types.LogEntry, 1024),
	}
	logger.wg.Add(1)
	go logger.run()

	return logger, nil
}

// Enqueue 以非阻塞方式写入日志队列，拥塞时丢弃并周期性告警。
func (l *EventLogger) Enqueue(entry types.LogEntry) {
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	if entry.Service == "" {
		entry.Service = l.service
	}

	select {
	case l.queue <- entry:
	default:
		dropped := l.dropNotice.Add(1)
		if dropped%100 == 1 {
			slog.Warn("observability log queue full, dropping entries", "dropped", dropped)
		}
	}
}

// Close 停止日志 worker。
func (l *EventLogger) Close() error {
	close(l.queue)
	l.wg.Wait()
	return nil
}

func (l *EventLogger) run() {
	defer l.wg.Done()

	ctx := context.Background()
	for entry := range l.queue {
		docID := l.nextDocID(entry.TraceID)
		if err := l.es.IndexDocumentWithRefresh(ctx, l.index, docID, entry, "false"); err != nil {
			slog.Warn("index runtime log failed", "error", err)
		}
	}
}

func (l *EventLogger) nextDocID(traceID string) string {
	id := l.counter.Add(1)
	traceID = strings.ReplaceAll(traceID, "-", "")
	if traceID == "" {
		return fmt.Sprintf("%d-%d", time.Now().UnixNano(), id)
	}
	return fmt.Sprintf("%d-%d-%s", time.Now().UnixNano(), id, traceID)
}

// Middleware 返回观测中间件：创建 HTTP server span、记录请求指标并异步写入运行日志。
func (r *Runtime) Middleware() gin.HandlerFunc {
	tracer := otel.Tracer("pilot/http")
	propagator := otel.GetTextMapPropagator()

	return func(c *gin.Context) {
		start := time.Now()
		method := c.Request.Method
		initialRoute := c.Request.URL.Path

		// 提取上游 trace 上下文并创建 server span（原 GoFrame 自动创建，Gin 下显式实现）。
		ctx := propagator.Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))
		ctx, span := tracer.Start(
			ctx,
			fmt.Sprintf("HTTP %s %s", method, initialRoute),
			oteltrace.WithSpanKind(oteltrace.SpanKindServer),
			oteltrace.WithAttributes(
				attribute.String("http.request.method", method),
				attribute.String("url.path", initialRoute),
				attribute.String("user_agent.original", c.Request.UserAgent()),
			),
		)
		defer span.End()
		c.Request = c.Request.WithContext(ctx)

		httpRequestsInflight.WithLabelValues(method, initialRoute).Inc()
		defer httpRequestsInflight.WithLabelValues(method, initialRoute).Dec()

		c.Next()

		route := c.FullPath()
		if route == "" {
			route = initialRoute
		}
		status := c.Writer.Status()
		if status == 0 {
			status = 200
		}
		statusText := strconv.Itoa(status)
		duration := time.Since(start)

		span.SetName(fmt.Sprintf("HTTP %s %s", method, route))
		span.SetAttributes(
			attribute.String("http.route", route),
			attribute.Int("http.response.status_code", status),
		)
		if status >= 500 {
			span.SetStatus(codes.Error, statusText)
		}

		httpRequestsTotal.WithLabelValues(method, route, statusText).Inc()
		httpRequestDuration.WithLabelValues(method, route, statusText).Observe(duration.Seconds())

		if route == "/metrics" || r.logger == nil {
			return
		}

		level := requestLogLevel(status, duration.Milliseconds(), r.slowRequestMS)
		traceID := ""
		if spanContext := span.SpanContext(); spanContext.HasTraceID() {
			traceID = spanContext.TraceID().String()
		}
		message := fmt.Sprintf(
			"request completed, status=%d, latency_ms=%d, method=%s, path=%s",
			status,
			duration.Milliseconds(),
			method,
			route,
		)

		r.logger.Enqueue(types.LogEntry{
			Timestamp: time.Now().UTC(),
			Level:     level,
			Service:   r.serviceName,
			TraceID:   traceID,
			Message:   message,
			Fields: map[string]string{
				"method":       method,
				"path":         route,
				"status":       statusText,
				"latency_ms":   strconv.FormatInt(duration.Milliseconds(), 10),
				"remote_addr":  c.ClientIP(),
				"user_agent":   c.Request.UserAgent(),
				"bytes":        strconv.Itoa(c.Writer.Size()),
				"content_type": c.Request.Header.Get("Content-Type"),
			},
		})
	}
}

func requestLogLevel(status int, latencyMS, slowRequestMS int64) string {
	switch {
	case status >= 500:
		return "error"
	case status >= 400:
		return "warn"
	case latencyMS >= slowRequestMS:
		return "warn"
	default:
		return "info"
	}
}

func ensureLogIndex(ctx context.Context, es *elasticsearch.Client, index string) error {
	mapping := map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"timestamp": map[string]interface{}{"type": "date"},
				"level":     map[string]interface{}{"type": "keyword"},
				"message":   map[string]interface{}{"type": "text"},
				"service":   map[string]interface{}{"type": "keyword"},
				"trace_id":  map[string]interface{}{"type": "keyword"},
				"fields":    map[string]interface{}{"type": "object", "dynamic": true},
			},
		},
	}

	if err := es.CreateIndex(ctx, index, mapping); err != nil {
		return fmt.Errorf("create log index failed: %w", err)
	}
	mappings, _ := mapping["mappings"].(map[string]interface{})
	if err := es.PutMapping(ctx, index, mappings); err != nil {
		if isLogIndexMappingConflict(err) {
			slog.WarnContext(ctx, "log index mapping conflict detected, keep existing mapping", "index", index, "error", err)
			return nil
		}
		return fmt.Errorf("put log index mapping failed: %w", err)
	}
	return nil
}

func isLogIndexMappingConflict(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "cannot be changed from type") ||
		strings.Contains(msg, "mapper [") ||
		strings.Contains(msg, "illegal_argument_exception")
}

func initTracing(ctx context.Context, cfg *config.Config) (*sdktrace.TracerProvider, error) {
	exporter, err := newOTLPHTTPExporter(ctx, cfg.Observability.Trace.OTLPHTTPEndpoint)
	if err != nil {
		return nil, fmt.Errorf("init trace exporter failed: %w", err)
	}

	serviceName := cfg.Observability.ServiceName
	environment := cfg.Server.Env
	sampleRatio := cfg.Observability.Trace.SampleRatio
	if sampleRatio <= 0 || sampleRatio > 1 {
		sampleRatio = 1.0
	}

	resource, err := sdkresource.Merge(
		sdkresource.Default(),
		sdkresource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
			attribute.String("deployment.environment", environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build trace resource failed: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp, nil
}

func newOTLPHTTPExporter(ctx context.Context, rawEndpoint string) (sdktrace.SpanExporter, error) {
	endpointURL, err := url.Parse(strings.TrimSpace(rawEndpoint))
	if err != nil {
		return nil, fmt.Errorf("parse otlp endpoint failed: %w", err)
	}
	if endpointURL.Host == "" {
		return nil, fmt.Errorf("invalid otlp endpoint: %s", rawEndpoint)
	}

	options := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(endpointURL.Host),
	}
	if endpointURL.Scheme == "http" {
		options = append(options, otlptracehttp.WithInsecure())
	}
	if endpointURL.Path != "" && endpointURL.Path != "/" {
		options = append(options, otlptracehttp.WithURLPath(strings.TrimPrefix(endpointURL.Path, "/")))
	}

	return otlptracehttp.New(ctx, options...)
}
