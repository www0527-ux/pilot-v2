package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func jsonClient(t *testing.T, responder func(*http.Request) (int, string)) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		status, body := responder(request)
		return &http.Response{
			StatusCode: status,
			Status:     fmt.Sprintf("%d test", status),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
}

func TestCheckHealthValidatesEnvelope(t *testing.T) {
	client := jsonClient(t, func(request *http.Request) (int, string) {
		if request.URL.Path != "/api/v1/health" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		return http.StatusOK, `{"success":true,"code":"OK","message":"ok","data":{"status":"ok"}}`
	})

	if err := checkHealth(context.Background(), client, "http://pilot.test"); err != nil {
		t.Fatalf("checkHealth() error = %v", err)
	}
}

func TestCheckHealthRejectsMalformedSuccessEnvelope(t *testing.T) {
	client := jsonClient(t, func(*http.Request) (int, string) {
		return http.StatusOK, `{"success":true,"code":"OK","message":"ok","data":{"status":"degraded"}}`
	})

	if err := checkHealth(context.Background(), client, "http://pilot.test"); err == nil {
		t.Fatal("expected malformed health envelope to be rejected")
	}
}

func TestUploadKnowledgeRequiresDocumentID(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "id present", body: `{"success":true,"code":"OK","message":"uploaded","data":{"id":"doc-1"}}`},
		{name: "id missing", body: `{"success":true,"code":"OK","message":"uploaded","data":{}}`, wantErr: true},
		{name: "wrong success code", body: `{"success":true,"code":"UPSTREAM_ERROR","message":"uploaded","data":{"id":"doc-1"}}`, wantErr: true},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			client := jsonClient(t, func(request *http.Request) (int, string) {
				if request.Method != http.MethodPost || request.URL.Path != "/api/v1/knowledge/upload" {
					t.Fatalf("request = %s %s", request.Method, request.URL.Path)
				}
				return http.StatusOK, testCase.body
			})

			err := uploadKnowledge(context.Background(), client, "http://pilot.test", "runbook")
			if (err != nil) != testCase.wantErr {
				t.Fatalf("uploadKnowledge() error = %v, wantErr=%t", err, testCase.wantErr)
			}
		})
	}
}

func TestRequireSuccessEnvelope(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		success bool
		code    string
		wantErr bool
	}{
		{name: "standard success", success: true, code: "OK"},
		{name: "false success flag", code: "OK", wantErr: true},
		{name: "wrong code", success: true, code: "INVALID_ARGUMENT", wantErr: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := requireSuccessEnvelope("test", testCase.success, testCase.code, "message")
			if (err != nil) != testCase.wantErr {
				t.Fatalf("requireSuccessEnvelope() error = %v, wantErr=%t", err, testCase.wantErr)
			}
		})
	}
}
