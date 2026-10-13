package observability

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseMetricValuePoint(t *testing.T) {
	ts, value, ok := parseMetricValuePoint([]interface{}{float64(1710000000), "1.25"})
	if !ok {
		t.Fatal("expected metric value to parse")
	}
	if !ts.Equal(time.Unix(1710000000, 0)) {
		t.Fatalf("unexpected timestamp: %v", ts)
	}
	if value != 1.25 {
		t.Fatalf("unexpected value: %v", value)
	}
}

func TestParseMetricValuePointRejectsMalformedPoint(t *testing.T) {
	if _, _, ok := parseMetricValuePoint([]interface{}{"bad-ts", "1.25"}); ok {
		t.Fatal("expected malformed point to be rejected")
	}
}

func TestParseLogTimestampSupportsMilliseconds(t *testing.T) {
	got := parseLogTimestamp(float64(1710000000123))
	want := time.UnixMilli(1710000000123)
	if !got.Equal(want) {
		t.Fatalf("unexpected timestamp: want %v, got %v", want, got)
	}
}

func TestMatchAlertFilters(t *testing.T) {
	labels := map[string]string{
		"service":  "checkout",
		"severity": "critical",
	}

	tests := []struct {
		name    string
		state   string
		filters map[string]string
		want    bool
	}{
		{
			name:    "all match",
			state:   "firing",
			filters: map[string]string{"service": "checkout", "severity": "critical", "status": "firing"},
			want:    true,
		},
		{
			name:    "label mismatch",
			state:   "firing",
			filters: map[string]string{"service": "payments"},
			want:    false,
		},
		{
			name:    "status mismatch still rejects even if labels match",
			state:   "pending",
			filters: map[string]string{"service": "checkout", "status": "firing"},
			want:    false,
		},
		{
			name:    "empty filters accept",
			state:   "firing",
			filters: map[string]string{},
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchAlertFilters(labels, tt.state, tt.filters)
			if got != tt.want {
				t.Fatalf("matchAlertFilters() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetAlertsAppliesLabelAndStatusFilters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/alerts" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "success",
			"data": map[string]interface{}{
				"alerts": []map[string]interface{}{
					{
						"labels":      map[string]string{"alertname": "HighError", "service": "checkout", "severity": "critical"},
						"annotations": map[string]string{"description": "error rate high"},
						"state":       "firing",
						"activeAt":    time.Unix(1710000000, 0).UTC().Format(time.RFC3339),
					},
					{
						"labels":      map[string]string{"alertname": "HighLatency", "service": "checkout", "severity": "warning"},
						"annotations": map[string]string{"description": "latency high"},
						"state":       "firing",
						"activeAt":    time.Unix(1710000060, 0).UTC().Format(time.RFC3339),
					},
					{
						"labels":      map[string]string{"alertname": "DiskFull", "service": "payments", "severity": "critical"},
						"annotations": map[string]string{"description": "disk full"},
						"state":       "pending",
						"activeAt":    time.Unix(1710000120, 0).UTC().Format(time.RFC3339),
					},
				},
			},
		})
	}))
	defer server.Close()

	client := NewPrometheusClient(server.URL, time.Second)
	alerts, err := client.GetAlerts(t.Context(), map[string]string{
		"service":  "checkout",
		"severity": "critical",
		"status":   "firing",
	})
	if err != nil {
		t.Fatalf("GetAlerts failed: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("len(alerts) = %d, want 1", len(alerts))
	}
	if alerts[0].Name != "HighError" {
		t.Fatalf("alert name = %q, want HighError", alerts[0].Name)
	}
}
