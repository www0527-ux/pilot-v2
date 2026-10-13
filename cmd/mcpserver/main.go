package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"time"

	"shanyangcode.com/pilot/internal/ai/embedder"
	"shanyangcode.com/pilot/internal/ai/retriever"
	"shanyangcode.com/pilot/internal/config"
	"shanyangcode.com/pilot/internal/logging"
	"shanyangcode.com/pilot/internal/mcp"
	"shanyangcode.com/pilot/internal/observability"
	"shanyangcode.com/pilot/internal/observability/correlate"
	"shanyangcode.com/pilot/pkg/elasticsearch"
	"shanyangcode.com/pilot/pkg/envloader"
	"shanyangcode.com/pilot/pkg/types"
)

func main() {
	configPath := flag.String("config", "", "config path")
	flag.Parse()
	if err := envloader.Load(); err != nil {
		slog.Error("load .env failed", "error", err)
		os.Exit(1)
	}
	cfg, err := config.Load(config.ResolvePath(*configPath))
	if err != nil {
		slog.Error("load config failed", "error", err)
		os.Exit(1)
	}
	logging.Setup(cfg.Logger.Level)
	if !cfg.MCP.Enabled {
		slog.Error("mcp.enabled=false; set mcp.enabled=true to start the read-only facade")
		os.Exit(1)
	}

	ctx := context.Background()
	esClient, err := elasticsearch.New(ctx, cfg.Elasticsearch)
	if err != nil {
		slog.Error("elasticsearch init failed", "error", err)
		os.Exit(1)
	}
	emb, err := embedder.NewEmbedder(ctx, cfg.Embedding)
	if err != nil {
		slog.Error("embedder init failed", "error", err)
		os.Exit(1)
	}
	knowledgeRetriever, err := retriever.NewKnowledgeRetriever(ctx, esClient, emb, cfg)
	if err != nil {
		slog.Error("knowledge retriever init failed", "error", err)
		os.Exit(1)
	}

	collector := observability.NewDataCollector(
		observability.NewPrometheusClient(cfg.Observability.Prometheus.URL, time.Duration(cfg.Observability.Prometheus.TimeoutSeconds)*time.Second),
		observability.NewLogClient(cfg.Observability.Log.URL, cfg.Observability.Log.Index),
		observability.NewTraceClient(cfg.Observability.Trace.URL),
		observability.NewTopologySources(cfg),
	)
	engine := correlate.NewEngine(collector, topologyAdapter{collector})

	server := mcp.NewServer(mcp.Deps{
		APIKey:  cfg.Security.Auth.APIKey,
		Alerts:  collector.GetAlerts,
		Logs:    collector.QueryLogs,
		Metrics: collector.QueryMetrics,
		Traces:  collector.GetTrace,
		Knowledge: func(ctx context.Context, query string) ([]types.KnowledgeDocument, error) {
			return knowledgeRetriever.Search(ctx, query)
		},
		Correlate:   engine,
		ServiceName: cfg.Observability.ServiceName,
	})

	slog.Info("starting read-only MCP facade", "listen", cfg.MCP.Listen)
	if err := server.ListenAndServe(cfg.MCP.Listen); err != nil {
		slog.Error("mcp server failed", "error", err)
		os.Exit(1)
	}
}

type topologyAdapter struct{ c *observability.DataCollector }

func (t topologyAdapter) Neighbors(service string) []string {
	return t.c.DependencyNeighbors(service)
}
