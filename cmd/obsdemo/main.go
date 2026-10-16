// obsdemo generates real HTTP telemetry for a repeatable, isolated latency demo.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"shanyangcode.com/pilot/internal/config"
	"shanyangcode.com/pilot/internal/observability"
	"shanyangcode.com/pilot/pkg/elasticsearch"
	"shanyangcode.com/pilot/pkg/envloader"
)

const demoIndex = "pilot_demo_logs"

type delayControl struct {
	mu      sync.Mutex
	delayMS int
	until   time.Time
}

func (d *delayControl) duration() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	if time.Now().Before(d.until) {
		return time.Duration(d.delayMS) * time.Millisecond
	}
	return 40 * time.Millisecond
}

// The switch is operator-only: check the TCP peer, never a forwarded header.
func (d *delayControl) update(c *gin.Context) {
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	var input struct {
		DelayMS    int `json:"delay_ms"`
		TTLSeconds int `json:"ttl_seconds"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || input.DelayMS < 0 || input.DelayMS > 2500 || input.TTLSeconds < 1 || input.TTLSeconds > 900 {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.delayMS, d.until = input.DelayMS, time.Now().Add(time.Duration(input.TTLSeconds)*time.Second)
	d.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{"updated": true})
}

func inventory(control *delayControl) gin.HandlerFunc {
	return func(c *gin.Context) {
		timer := time.NewTimer(control.duration())
		defer timer.Stop()
		select {
		case <-c.Request.Context().Done():
			c.AbortWithStatus(499)
			return
		case <-timer.C:
		}
		c.Header("X-Trace-ID", trace.SpanContextFromContext(c.Request.Context()).TraceID().String())
		c.JSON(http.StatusOK, gin.H{"available": true})
	}
}

func gateway(client *http.Client, downstream string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, span := otel.Tracer("pilot/obsdemo").Start(c.Request.Context(), "GET /inventory", trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attribute.String("http.request.method", "GET"), attribute.String("server.address", "pilot-demo-inventory")))
		defer span.End()
		c.Header("X-Trace-ID", span.SpanContext().TraceID().String())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, downstream+"/inventory", nil)
		if err == nil {
			otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
			var response *http.Response
			response, err = client.Do(req)
			if err == nil {
				_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
				_ = response.Body.Close()
				span.SetAttributes(attribute.Int("http.response.status_code", response.StatusCode))
				if err == nil && response.StatusCode != http.StatusOK {
					err = fmt.Errorf("downstream returned HTTP %d", response.StatusCode)
				}
			}
		}
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "downstream request failed")
			status := http.StatusBadGateway
			if errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			c.JSON(status, gin.H{"error": "downstream request failed"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"checkout": "ok"})
	}
}

func demoMetrics(registry *prometheus.Registry) gin.HandlerFunc {
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "pilot_demo_http_requests_total", Help: "Actual demo business HTTP requests."}, []string{"method", "route", "status"})
	latency := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "pilot_demo_http_request_duration_seconds", Help: "Actual demo business HTTP latency.", Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 1.25, 1.5, 1.75, 2, 2.5, 3}}, []string{"method", "route", "status"})
	registry.MustRegister(requests, latency)
	return func(c *gin.Context) {
		started := time.Now()
		c.Next()
		route := c.FullPath()
		if route != "/checkout" && route != "/inventory" {
			return
		}
		status := strconv.Itoa(c.Writer.Status())
		requests.WithLabelValues(c.Request.Method, route, status).Inc()
		latency.WithLabelValues(c.Request.Method, route, status).Observe(time.Since(started).Seconds())
	}
}

func businessRouter(role string, observer gin.HandlerFunc, downstream string) *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Recovery())
	registry := prometheus.NewRegistry()
	// Neither scrapes nor operator commands are business evidence for the Agent.
	engine.GET("/metrics", gin.WrapH(promhttp.HandlerFor(registry, promhttp.HandlerOpts{})))
	control := &delayControl{}
	if role == "inventory" {
		engine.POST("/__demo/delay", control.update)
	}
	engine.Use(observer, demoMetrics(registry))
	engine.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"service": "pilot-demo-" + role, "demo": true}) })
	if role == "gateway" {
		engine.GET("/checkout", gateway(&http.Client{Timeout: 3 * time.Second}, downstream))
	} else {
		engine.GET("/inventory", inventory(control))
	}
	return engine
}

func main() {
	role := flag.String("role", "", "gateway, inventory, load, or prepare")
	configPath := flag.String("config", "", "existing local Pilot config")
	listen := flag.String("listen", "", "override HTTP listen address")
	downstream := flag.String("downstream", "http://127.0.0.1:18092", "inventory endpoint")
	target := flag.String("target", "http://127.0.0.1:18091", "load: gateway endpoint")
	output := flag.String("out", "artifacts/observability-demo/config.local.yaml", "prepare: generated isolated Pilot config")
	flag.Parse()
	var err error
	if *role == "load" {
		err = runLoad(*target, *output)
	} else {
		err = run(*role, config.ResolvePath(*configPath), *listen, *downstream, *output)
	}
	if err != nil {
		slog.Error("observability demo failed", "error", err)
		os.Exit(1)
	}
}

func run(role, configPath, listen, downstream, output string) error {
	if role != "gateway" && role != "inventory" && role != "prepare" {
		return errors.New("-role must be gateway, inventory, or prepare")
	}
	if err := envloader.Load(); err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if role == "prepare" {
		return prepare(configPath, output, cfg)
	}
	cfg.Server.Env = "demo"
	cfg.Observability.ServiceName = "pilot-demo-" + role
	cfg.Observability.Log.Index = demoIndex
	cfg.Observability.Log.SlowRequestMS = 1000
	cfg.Observability.Trace.SampleRatio = 1
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	es, err := elasticsearch.New(ctx, cfg.Elasticsearch)
	if err != nil {
		return err
	}
	runtime, err := observability.NewRuntime(ctx, es, cfg)
	if err != nil {
		return err
	}
	defer func() {
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = runtime.Shutdown(shutdown)
	}()
	gin.SetMode(gin.ReleaseMode)
	engine := businessRouter(role, runtime.Middleware(), downstream)
	if listen == "" {
		listen = map[string]string{"gateway": ":18091", "inventory": ":18092"}[role]
	}
	server := &http.Server{Addr: listen, Handler: engine, ReadHeaderTimeout: 5 * time.Second}
	finished := make(chan error, 1)
	go func() { finished <- server.ListenAndServe() }()
	slog.Info("demo service started", "role", role, "address", listen, "log_index", demoIndex)
	select {
	case err := <-finished:
		return err
	case <-ctx.Done():
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		return server.Shutdown(shutdown)
	}
}
