package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"shanyangcode.com/pilot/internal/ai/agent"
	"shanyangcode.com/pilot/internal/ai/embedder"
	"shanyangcode.com/pilot/internal/ai/models"
	"shanyangcode.com/pilot/internal/ai/observe"
	"shanyangcode.com/pilot/internal/ai/retriever"
	"shanyangcode.com/pilot/internal/config"
	"shanyangcode.com/pilot/internal/controller"
	"shanyangcode.com/pilot/internal/health"
	"shanyangcode.com/pilot/internal/httpapi"
	"shanyangcode.com/pilot/internal/knowledge"
	"shanyangcode.com/pilot/internal/logging"
	"shanyangcode.com/pilot/internal/memory"
	"shanyangcode.com/pilot/internal/observability"
	"shanyangcode.com/pilot/internal/observability/correlate"
	promptcatalog "shanyangcode.com/pilot/internal/prompts"
	"shanyangcode.com/pilot/internal/security"
	"shanyangcode.com/pilot/pkg/elasticsearch"
	"shanyangcode.com/pilot/pkg/envloader"
	"shanyangcode.com/pilot/pkg/redis"
)

func main() {
	configPath := flag.String("config", "", "配置文件路径（默认 manifest/config/config.yaml，可用 PILOT_CONFIG 覆盖）")
	flag.Parse()

	ctx := context.Background()

	// 加载根目录 .env（若存在），再加载类型化配置。
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
	catalog, err := promptcatalog.Load(cfg.Prompts.Path)
	if err != nil {
		fatal("load prompts failed", err)
	}
	if err := promptcatalog.Configure(catalog); err != nil {
		fatal("configure prompts failed", err)
	}

	// 核心依赖：Redis / Elasticsearch / MySQL。缺一启动失败（Chat/知识库无法诚实服务）。
	redisClient, err := redis.New(ctx, cfg.Redis)
	if err != nil {
		fatal("init redis failed", err)
	}
	defer func() {
		if shutdownErr := redisClient.Close(); shutdownErr != nil {
			slog.Warn("shutdown redis failed", "error", shutdownErr)
		}
	}()

	esClient, err := elasticsearch.New(ctx, cfg.Elasticsearch)
	if err != nil {
		fatal("init elasticsearch failed", err)
	}

	db, err := memory.OpenDB(ctx, cfg.Database)
	if err != nil {
		fatal("init mysql failed", err)
	}
	defer func() {
		if shutdownErr := memory.CloseDB(db); shutdownErr != nil {
			slog.Warn("shutdown mysql failed", "error", shutdownErr)
		}
	}()

	// 观测运行时（trace exporter + 请求指标 + 运行日志）。
	// allow_degraded=true 时，OTLP/运行日志可降级；核心依赖仍要求可用。
	runtime, err := observability.NewRuntime(ctx, esClient, cfg)
	if err != nil {
		fatal("init observability runtime failed", err)
	}
	defer func() {
		if shutdownErr := runtime.Shutdown(ctx); shutdownErr != nil {
			slog.Warn("shutdown observability runtime failed", "error", shutdownErr)
		}
	}()
	observe.Install()

	// 可观测性数据源客户端（Prometheus / ES 日志 / Jaeger）。
	collector := observability.NewDataCollector(
		observability.NewPrometheusClient(cfg.Observability.Prometheus.URL, time.Duration(cfg.Observability.Prometheus.TimeoutSeconds)*time.Second),
		observability.NewLogClient(cfg.Observability.Log.URL, cfg.Observability.Log.Index),
		observability.NewTraceClient(cfg.Observability.Trace.URL),
		observability.NewTopologySources(cfg),
	)

	// AI 组件：模型、Embedding、知识检索、记忆。
	chatModel, err := models.NewChatModel(ctx, cfg.LLM.MainModel)
	if err != nil {
		fatal("init chat model failed", err)
	}
	embeddingModel, err := embedder.NewEmbedder(ctx, cfg.Embedding)
	if err != nil {
		fatal("init embedder failed", err)
	}
	knowledgeRetriever, err := retriever.NewKnowledgeRetriever(ctx, esClient, embeddingModel, cfg)
	if err != nil {
		fatal("init knowledge retriever failed", err)
	}
	if cfg.RAG.RerankerMode == "model" {
		modelReranker, rerankerErr := retriever.NewChatModelReranker(chatModel.GetModel())
		if rerankerErr != nil {
			fatal("init knowledge model reranker failed", rerankerErr)
		}
		knowledgeRetriever.WithModelReranker(modelReranker)
	}
	shortTermMemory := memory.NewShortTermMemory(redisClient, cfg.Redis.Memory)
	historyManager := memory.NewConversationHistoryManager(db)
	// Feedback is informational; experience collection has its own explicit trigger.
	feedbackManager := memory.NewFeedbackManager(db, nil)
	experiences, err := knowledge.NewService(ctx, db)
	if err != nil {
		fatal("migrate experience knowledge failed", err)
	}
	if err := experiences.EnableIssueTracking(); err != nil {
		fatal("enable issue tracking failed", err)
	}
	accounts, err := security.NewAccountStore(db)
	if err != nil {
		fatal("migrate console accounts failed", err)
	}
	curator, err := knowledge.NewAgentGenerator(ctx, chatModel.GetModel(), experiences, models.BackgroundOptions(cfg.LLM.MainModel)...)
	if err != nil {
		fatal("create knowledge agent failed", err)
	}
	experienceTool, err := knowledge.NewSearchTool(experiences, true)
	if err != nil {
		fatal("create experience search tool failed", err)
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); knowledge.NewWorker(experiences, curator).Run(workerCtx) }()
	defer func() { stopWorker(); <-workerDone }()

	fullAgent, err := agent.NewFullPilotAgent(ctx, agent.Deps{
		ChatModel:          chatModel,
		Collector:          collector,
		ShortTermMemory:    shortTermMemory,
		KnowledgeRetriever: knowledgeRetriever,
		MaxIterations:      cfg.Agent.MaxIterations,
		SessionContext:     memory.NewSessionContextManager(db),
		ExtraTools:         []tool.BaseTool{experienceTool},
		ContextTokens:      cfg.Agent.ContextTokens,
		StaticContext:      "configured service=" + cfg.Observability.ServiceName + "; dependencies=Redis, MySQL, Elasticsearch, Prometheus, Jaeger, LLM; static configuration is not runtime evidence",
	})
	if err != nil {
		fatal("create full pilot agent failed", err)
	}

	corrEngine := correlate.NewEngine(collector, topologyNeighbors{collector})

	// 控制器装配。
	agentTimeout := time.Duration(cfg.Agent.TimeoutSeconds) * time.Second
	chatController := controller.NewChatController(fullAgent, nil, shortTermMemory, historyManager).
		WithRunFinished(experiences.MaybeEnqueue).
		WithFeedbackStore(feedbackManager).
		WithAgentTimeout(func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, agentTimeout)
		})
	knowledgeController := controller.NewKnowledgeController(knowledgeRetriever)
	observabilityController := controller.NewObservabilityController(collector, cfg.Observability.ServiceName).
		WithCorrelate(corrEngine)

	healthChecker := newHealthChecker(cfg.Observability.ServiceName, redisClient, db, esClient, runtime.DegradedCapabilities())
	engine := buildEngine(cfg, runtime, healthChecker, chatController, knowledgeController, observabilityController, experienceRuntime{accounts: accounts, controller: controller.NewExperienceController(experiences)})

	server := &http.Server{
		Addr:    cfg.Server.Address,
		Handler: engine,
	}

	// 优雅退出：确保 trace/日志队列刷出、连接池关闭。
	shutdownCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("Pilot AI Service starting",
			"address", cfg.Server.Address,
			"allow_degraded", cfg.Server.AllowDegraded,
			"auth_enabled", cfg.Security.Auth.Enabled,
			"rate_limit_enabled", cfg.Security.RateLimit.Enabled,
			"degraded", runtime.DegradedCapabilities(),
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server failed", "error", err)
			stop()
		}
	}()

	<-shutdownCtx.Done()
	slog.Info("shutting down http server")
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(stopCtx); err != nil {
		slog.Warn("http server shutdown failed", "error", err)
	}
}

func newHealthChecker(
	serviceName string,
	redisClient *redis.Client,
	db *gorm.DB,
	esClient *elasticsearch.Client,
	degraded []string,
) *health.Checker {
	return &health.Checker{
		ServiceName: serviceName,
		Degraded:    degraded,
		Core: []health.Dependency{
			{Name: "redis", Check: redisClient.Ping},
			{Name: "mysql", Check: func(ctx context.Context) error { return memory.PingDB(ctx, db) }},
			{Name: "elasticsearch", Check: esClient.Ping},
		},
	}
}

// buildEngine 组装 Gin 引擎：安全中间件、观测中间件、统一 API 路由与前端静态资源。
func buildEngine(
	cfg *config.Config,
	runtime *observability.Runtime,
	healthChecker *health.Checker,
	chatController *controller.ChatController,
	knowledgeController *controller.KnowledgeController,
	observabilityController *controller.ObservabilityController,
	experience ...experienceRuntime,
) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(recoveryMiddleware())
	for _, mw := range security.Middleware(cfg.Security) {
		engine.Use(mw)
	}
	runtime.BindServer(engine)

	api := engine.Group("/api/v1")
	{
		healthChecker.RegisterRoutes(api)
		if len(experience) > 0 {
			api.POST("/account/session", experience[0].accounts.Session)
			experience[0].controller.Register(api.Group("/experiences", experience[0].accounts.RequireAccount()))
		}

		chat := api.Group("/chat")
		if len(experience) > 0 {
			chat.Use(experience[0].accounts.RequireAccount())
		}
		{
			// 兼容历史客户端的带斜杠写法。
			chat.POST("", chatController.Chat)
			chat.POST("/", chatController.Chat)
			chat.POST("/stream", chatController.StreamChat)
			chat.POST("/feedback", chatController.SubmitFeedback)
			chat.GET("/history", chatController.GetHistory)
			chat.GET("/runs", chatController.RunHistory)
			chat.GET("/runs/:run_id/events", chatController.RunEvents)
			chat.POST("/runs/:run_id/cancel", chatController.CancelRun)
			chat.DELETE("/history", chatController.ClearHistory)
		}

		knowledge := api.Group("/knowledge")
		{
			knowledge.POST("/upload", knowledgeController.Upload)
			knowledge.POST("/search", knowledgeController.Search)
			knowledge.POST("/search/detailed", knowledgeController.SearchDetailed)
			knowledge.DELETE("", knowledgeController.Delete)
			knowledge.DELETE("/", knowledgeController.Delete)
		}

		ob := api.Group("/observability")
		{
			ob.POST("/overview", observabilityController.GetOverview)
			ob.POST("/alerts", observabilityController.GetAlerts)
			ob.POST("/metrics", observabilityController.GetMetrics)
			ob.POST("/logs", observabilityController.GetLogs)
			ob.POST("/correlate", observabilityController.Correlate)
		}
	}

	// 前端静态资源（同域部署）；API 前缀下的未知路径返回统一 404。
	staticServer := http.FileServer(http.Dir("web"))
	engine.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			httpapi.Fail(c, httpapi.NotFound("接口不存在"))
			return
		}
		staticServer.ServeHTTP(c.Writer, c.Request)
	})

	return engine
}

type experienceRuntime struct {
	accounts   *security.AccountStore
	controller *controller.ExperienceController
}

// recoveryMiddleware 捕获 panic 并按统一契约返回 500。
func recoveryMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.ErrorContext(c.Request.Context(), "panic recovered", "panic", recovered)
				if !c.Writer.Written() {
					httpapi.Fail(c, httpapi.Internal("服务内部错误，请稍后重试"))
				}
				c.Abort()
			}
		}()
		c.Next()
	}
}

func fatal(message string, err error) {
	slog.Error(message, "error", err)
	os.Exit(1)
}

type topologyNeighbors struct {
	collector *observability.DataCollector
}

func (t topologyNeighbors) Neighbors(service string) []string {
	return t.collector.DependencyNeighbors(service)
}
