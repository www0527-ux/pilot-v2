package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func newTestContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	return c, recorder
}

func decodeResponse(t *testing.T, recorder *httptest.ResponseRecorder) Response {
	t.Helper()
	var resp Response
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response failed: %v body=%s", err, recorder.Body.String())
	}
	return resp
}

func TestOKEnvelope(t *testing.T) {
	c, recorder := newTestContext(t)
	OK(c, "done", map[string]string{"k": "v"})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	resp := decodeResponse(t, recorder)
	if !resp.Success || resp.Code != CodeOK || resp.Message != "done" {
		t.Fatalf("unexpected envelope: %+v", resp)
	}
	if resp.Data == nil {
		t.Fatal("data missing")
	}
}

func TestFailWithTypedError(t *testing.T) {
	cases := []struct {
		err        *Error
		wantStatus int
		wantCode   string
	}{
		{InvalidArgument("bad"), http.StatusBadRequest, CodeInvalidArgument},
		{Unauthenticated("no key"), http.StatusUnauthorized, CodeUnauthenticated},
		{NotFound("missing"), http.StatusNotFound, CodeNotFound},
		{FeatureDisabled("off"), http.StatusNotFound, CodeFeatureDisabled},
		{RateLimited("slow down"), http.StatusTooManyRequests, CodeRateLimited},
		{DependencyUnavailable("down"), http.StatusServiceUnavailable, CodeDependencyUnavailable},
		{UpstreamError("llm"), http.StatusBadGateway, CodeUpstreamError},
		{Internal("boom"), http.StatusInternalServerError, CodeInternal},
	}

	for _, testCase := range cases {
		c, recorder := newTestContext(t)
		Fail(c, testCase.err)
		if recorder.Code != testCase.wantStatus {
			t.Fatalf("%s: status = %d, want %d", testCase.wantCode, recorder.Code, testCase.wantStatus)
		}
		resp := decodeResponse(t, recorder)
		if resp.Success || resp.Code != testCase.wantCode {
			t.Fatalf("unexpected envelope for %s: %+v", testCase.wantCode, resp)
		}
	}
}

func TestFailWithUnknownErrorFallsBackToInternal(t *testing.T) {
	c, recorder := newTestContext(t)
	Fail(c, errors.New("raw failure detail that must not leak"))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	resp := decodeResponse(t, recorder)
	if resp.Code != CodeInternal {
		t.Fatalf("code = %s, want INTERNAL", resp.Code)
	}
	if resp.Message == "raw failure detail that must not leak" {
		t.Fatal("internal error detail leaked to client")
	}
}

func TestFormatSSEDataBlock(t *testing.T) {
	got := FormatSSEDataBlock("line1\nline2")
	want := "data: line1\ndata: line2\n\n"
	if got != want {
		t.Fatalf("unexpected SSE block:\nwant: %q\ngot:  %q", want, got)
	}
}
