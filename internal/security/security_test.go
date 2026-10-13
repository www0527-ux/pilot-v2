package security

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"shanyangcode.com/pilot/internal/config"
	"shanyangcode.com/pilot/internal/httpapi"
)

func TestAPIKeyAcceptsBearerAndHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(APIKey("secret"))
	engine.GET("/api/v1/chat", func(c *gin.Context) { httpapi.OK(c, "ok", nil) })

	for _, header := range []http.Header{
		{"Authorization": []string{"Bearer secret"}},
		{"X-Api-Key": []string{"secret"}},
	} {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/chat", nil)
		req.Header = header
		engine.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("header=%v status=%d body=%s", header, recorder.Code, recorder.Body.String())
		}
	}
}

func TestAPIKeyRejectsMissingAndExemptsHealth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(APIKey("secret"))
	engine.GET("/api/v1/chat", func(c *gin.Context) { httpapi.OK(c, "ok", nil) })
	engine.GET("/api/v1/health", func(c *gin.Context) { httpapi.OK(c, "ok", nil) })

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/chat", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("chat status=%d, want 401", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("health status=%d, want 200", recorder.Code)
	}
}

func TestRateLimitBlocksExcessRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(RateLimit(2))
	engine.GET("/api/v1/chat", func(c *gin.Context) { httpapi.OK(c, "ok", nil) })

	for i := 0; i < 2; i++ {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/chat", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d status=%d", i+1, recorder.Code)
		}
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/chat", nil))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429", recorder.Code)
	}
}

func TestCORSAllowsConfiguredOriginAndPreflight(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(CORS([]string{"https://ops.example.com"}))
	engine.GET("/api/v1/chat", func(c *gin.Context) { httpapi.OK(c, "ok", nil) })

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/chat", nil)
	req.Header.Set("Origin", "https://ops.example.com")
	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("preflight status=%d", recorder.Code)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "https://ops.example.com" {
		t.Fatalf("allow-origin=%q", got)
	}
}

func TestMiddlewareDisabledByDefault(t *testing.T) {
	handlers := Middleware(config.Security{})
	if len(handlers) != 0 {
		t.Fatalf("handlers = %d, want 0 when security disabled", len(handlers))
	}
}
