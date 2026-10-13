// Package health 提供 liveness / readiness 探测。
//
// 产品边界：
//   - live：进程存活，不检查依赖。
//   - ready：核心依赖（Redis / MySQL / Elasticsearch）必须可用；失败返回 503。
//   - /api/v1/health：兼容入口，返回 live 视角，并附带 degraded 能力列表与 ready 摘要。
package health

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"

	"shanyangcode.com/pilot/internal/httpapi"
)

// Dependency 是可探测的依赖项。
type Dependency struct {
	Name  string
	Check func(ctx context.Context) error
}

// Checker 聚合核心依赖探测与启动期降级标记。
type Checker struct {
	ServiceName string
	Core        []Dependency
	Degraded    []string // 启动期可选能力缺失，如 trace_export、runtime_logs
}

// LiveResult 是存活探测结果。
type LiveResult struct {
	Status  string `json:"status"`
	Service string `json:"service"`
}

// ReadyResult 是就绪探测结果。
type ReadyResult struct {
	Status  string            `json:"status"`
	Service string            `json:"service"`
	Checks  map[string]string `json:"checks"`
}

// StatusResult 是兼容健康接口结果。
type StatusResult struct {
	Status   string            `json:"status"` // ok | degraded | not_ready
	Service  string            `json:"service"`
	Ready    bool              `json:"ready"`
	Checks   map[string]string `json:"checks,omitempty"`
	Degraded []string          `json:"degraded,omitempty"`
}

// Live 始终表示进程存活。
func (c *Checker) Live() LiveResult {
	return LiveResult{Status: "ok", Service: c.ServiceName}
}

// Ready 检查核心依赖。
func (c *Checker) Ready(ctx context.Context) (ReadyResult, bool) {
	checks := make(map[string]string, len(c.Core))
	ready := true
	for _, dep := range c.Core {
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := dep.Check(checkCtx)
		cancel()
		if err != nil {
			ready = false
			checks[dep.Name] = "fail"
			continue
		}
		checks[dep.Name] = "ok"
	}
	status := "ok"
	if !ready {
		status = "not_ready"
	}
	return ReadyResult{
		Status:  status,
		Service: c.ServiceName,
		Checks:  checks,
	}, ready
}

// Status 返回兼容健康视图：进程存活，并附带 ready/degraded 信息。
func (c *Checker) Status(ctx context.Context) StatusResult {
	readyResult, ready := c.Ready(ctx)
	status := "ok"
	switch {
	case !ready:
		status = "not_ready"
	case len(c.Degraded) > 0:
		status = "degraded"
	}
	return StatusResult{
		Status:   status,
		Service:  c.ServiceName,
		Ready:    ready,
		Checks:   readyResult.Checks,
		Degraded: append([]string(nil), c.Degraded...),
	}
}

// RegisterRoutes 注册 health / live / ready。
func (c *Checker) RegisterRoutes(api *gin.RouterGroup) {
	api.GET("/health", func(g *gin.Context) {
		httpapi.OK(g, "ok", c.Status(g.Request.Context()))
	})
	api.GET("/health/live", func(g *gin.Context) {
		httpapi.OK(g, "ok", c.Live())
	})
	api.GET("/health/ready", func(g *gin.Context) {
		result, ready := c.Ready(g.Request.Context())
		if !ready {
			httpapi.Fail(g, httpapi.DependencyUnavailable("核心依赖未就绪"))
			return
		}
		httpapi.OK(g, "ok", result)
	})
}
