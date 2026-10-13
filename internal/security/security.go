// Package security 提供进程内生产边界中间件：共享 API Key 鉴权、CORS 白名单与按 IP 限流。
//
// 产品边界（刻意收窄，避免伪装成完整 IAM）：
//   - 认证主体：单一共享服务密钥（Bearer 或 X-API-Key），不是用户/租户模型。
//   - 豁免路径：健康检查与 /metrics，便于探针与 Prometheus 抓取。
//   - CORS：仅对配置中的来源生效；空列表表示同域部署，不注入 CORS 头。
//   - 限流：按客户端 IP 的固定窗口计数；0/关闭表示不限流。
package security

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"shanyangcode.com/pilot/internal/config"
	"shanyangcode.com/pilot/internal/httpapi"
)

// Middleware 按配置组装 CORS → 限流 → API Key 鉴权。
func Middleware(cfg config.Security) []gin.HandlerFunc {
	handlers := make([]gin.HandlerFunc, 0, 3)
	if len(normalizeOrigins(cfg.CORS.AllowedOrigins)) > 0 {
		handlers = append(handlers, CORS(cfg.CORS.AllowedOrigins))
	}
	if cfg.RateLimit.Enabled {
		handlers = append(handlers, RateLimit(cfg.RateLimit.RequestsPerMinute))
	}
	if cfg.Auth.Enabled {
		handlers = append(handlers, APIKey(cfg.Auth.APIKey))
	}
	return handlers
}

// IsExemptPath 判断路径是否豁免鉴权与限流（健康与指标）。
func IsExemptPath(path string) bool {
	switch {
	case path == "/metrics":
		return true
	case path == "/api/v1/health", path == "/api/v1/health/", path == "/api/v1/health/live", path == "/api/v1/health/ready":
		return true
	default:
		return false
	}
}

// APIKey 校验 Authorization: Bearer <key> 或 X-API-Key。
func APIKey(expected string) gin.HandlerFunc {
	expected = strings.TrimSpace(expected)
	return func(c *gin.Context) {
		if IsExemptPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		provided := extractAPIKey(c.Request)
		if expected == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			httpapi.Fail(c, httpapi.Unauthenticated("缺少或无效的 API Key"))
			c.Abort()
			return
		}
		c.Next()
	}
}

func extractAPIKey(r *http.Request) string {
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		return key
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) >= 7 && strings.EqualFold(auth[:7], "Bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

// CORS 仅允许配置来源；支持单独配置 "*"。
func CORS(allowedOrigins []string) gin.HandlerFunc {
	origins := normalizeOrigins(allowedOrigins)
	allowAll := false
	allowed := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		if origin == "*" {
			allowAll = true
			continue
		}
		allowed[origin] = struct{}{}
	}

	return func(c *gin.Context) {
		origin := strings.TrimSpace(c.GetHeader("Origin"))
		if origin != "" {
			if allowAll {
				c.Header("Access-Control-Allow-Origin", "*")
			} else if _, ok := allowed[origin]; ok {
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Vary", "Origin")
			}
			if c.Writer.Header().Get("Access-Control-Allow-Origin") != "" {
				c.Header("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
				c.Header("Access-Control-Allow-Headers", "Authorization,Content-Type,X-API-Key")
				c.Header("Access-Control-Max-Age", "600")
			}
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func normalizeOrigins(origins []string) []string {
	out := make([]string, 0, len(origins))
	for _, origin := range origins {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		out = append(out, origin)
	}
	return out
}

// RateLimit 按客户端 IP 的每分钟固定窗口限流。
func RateLimit(requestsPerMinute int) gin.HandlerFunc {
	limiter := newIPLimiter(requestsPerMinute, time.Minute)
	return func(c *gin.Context) {
		if IsExemptPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		if !limiter.allow(c.ClientIP()) {
			httpapi.Fail(c, httpapi.RateLimited("请求过于频繁，请稍后重试"))
			c.Abort()
			return
		}
		c.Next()
	}
}

type ipLimiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	counters map[string]*windowCounter
}

type windowCounter struct {
	count   int
	resetAt time.Time
}

func newIPLimiter(limit int, window time.Duration) *ipLimiter {
	if limit < 1 {
		limit = 1
	}
	if window <= 0 {
		window = time.Minute
	}
	return &ipLimiter{
		limit:    limit,
		window:   window,
		counters: make(map[string]*windowCounter),
	}
}

func (l *ipLimiter) allow(key string) bool {
	if key == "" {
		key = "unknown"
	}
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	counter, ok := l.counters[key]
	if !ok || now.After(counter.resetAt) {
		l.counters[key] = &windowCounter{count: 1, resetAt: now.Add(l.window)}
		if len(l.counters) > 4096 {
			l.gcLocked(now)
		}
		return true
	}
	if counter.count >= l.limit {
		return false
	}
	counter.count++
	return true
}

func (l *ipLimiter) gcLocked(now time.Time) {
	for key, counter := range l.counters {
		if now.After(counter.resetAt) {
			delete(l.counters, key)
		}
	}
}
