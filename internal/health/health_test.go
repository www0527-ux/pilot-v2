package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"shanyangcode.com/pilot/internal/httpapi"
)

func TestLiveAlwaysOK(t *testing.T) {
	checker := &Checker{ServiceName: "pilot-api", Degraded: []string{"trace_export"}}
	got := checker.Live()
	if got.Status != "ok" || got.Service != "pilot-api" {
		t.Fatalf("unexpected live result: %+v", got)
	}
}

func TestReadyFailsWhenCoreDependencyDown(t *testing.T) {
	checker := &Checker{
		ServiceName: "pilot-api",
		Core: []Dependency{
			{Name: "redis", Check: func(ctx context.Context) error { return nil }},
			{Name: "mysql", Check: func(ctx context.Context) error { return errors.New("down") }},
		},
	}
	result, ready := checker.Ready(context.Background())
	if ready {
		t.Fatal("expected not ready")
	}
	if result.Checks["mysql"] != "fail" || result.Checks["redis"] != "ok" {
		t.Fatalf("unexpected checks: %+v", result.Checks)
	}
}

func TestStatusMarksDegradedCapabilities(t *testing.T) {
	checker := &Checker{
		ServiceName: "pilot-api",
		Core:        []Dependency{{Name: "redis", Check: func(ctx context.Context) error { return nil }}},
		Degraded:    []string{"runtime_logs"},
	}
	status := checker.Status(context.Background())
	if status.Status != "degraded" || !status.Ready {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestReadyHTTPRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api := engine.Group("/api/v1")
	checker := &Checker{
		ServiceName: "pilot-api",
		Core: []Dependency{
			{Name: "redis", Check: func(ctx context.Context) error { return errors.New("down") }},
		},
	}
	checker.RegisterRoutes(api)

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/health/ready", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), httpapi.CodeDependencyUnavailable) {
		t.Fatalf("body=%s", recorder.Body.String())
	}
}
