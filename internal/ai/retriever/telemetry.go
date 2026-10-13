package retriever

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	ragTelemetryOnce sync.Once
	ragOperations    = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "pilot_rag_operations_total", Help: "RAG operations by bounded operation/mode/status."}, []string{"operation", "mode", "status"})
	ragDuration      = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "pilot_rag_operation_duration_seconds", Help: "RAG operation duration by bounded operation/mode.", Buckets: prometheus.DefBuckets}, []string{"operation", "mode"})
	ragItems         = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "pilot_rag_operation_items", Help: "RAG input/output item counts by bounded operation/mode.", Buckets: []float64{0, 1, 2, 5, 10, 20, 50, 100, 500}}, []string{"operation", "mode"})
)

func installRAGTelemetry() {
	ragTelemetryOnce.Do(func() {
		prometheus.MustRegister(ragOperations, ragDuration, ragItems)
	})
}

func observeRAG(operation, mode string, started time.Time, err error, count int) {
	installRAGTelemetry()
	status := "ok"
	if err != nil {
		status = "error"
	}
	ragOperations.WithLabelValues(operation, mode, status).Inc()
	ragDuration.WithLabelValues(operation, mode).Observe(time.Since(started).Seconds())
	ragItems.WithLabelValues(operation, mode).Observe(float64(count))
}

func observeRAGFallback(channel string) {
	installRAGTelemetry()
	ragOperations.WithLabelValues("fallback", channel, "degraded").Inc()
}
