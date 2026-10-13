package observability

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func toString(v interface{}) string {
	switch val := v.(type) {
	case string:
		return val
	case []byte:
		return string(val)
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(val), 'f', -1, 32)
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case int32:
		return strconv.FormatInt(int64(val), 10)
	case bool:
		if val {
			return "true"
		}
		return "false"
	default:
		if val == nil {
			return ""
		}
		return fmt.Sprintf("%v", val)
	}
}

func formatHTTPStatusError(operation string, statusCode int, body []byte) error {
	snippet := strings.TrimSpace(string(body))
	if snippet == "" {
		return fmt.Errorf("%s: status=%d", operation, statusCode)
	}
	const maxSnippet = 256
	if len(snippet) > maxSnippet {
		snippet = snippet[:maxSnippet] + "..."
	}
	return fmt.Errorf("%s: status=%d body=%s", operation, statusCode, snippet)
}

func parseLogTimestamp(raw interface{}) time.Time {
	switch val := raw.(type) {
	case string:
		layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"}
		for _, layout := range layouts {
			if t, err := time.Parse(layout, val); err == nil {
				return t
			}
		}
	case float64:
		sec := int64(val)
		// 兼容毫秒时间戳
		if sec > 1_000_000_000_000 {
			return time.UnixMilli(sec)
		}
		return time.Unix(sec, 0)
	case int64:
		if val > 1_000_000_000_000 {
			return time.UnixMilli(val)
		}
		return time.Unix(val, 0)
	}
	return time.Time{}
}

func parseMetricValuePoint(value []interface{}) (time.Time, float64, bool) {
	if len(value) < 2 {
		return time.Time{}, 0, false
	}

	rawTS, ok := value[0].(float64)
	if !ok {
		return time.Time{}, 0, false
	}

	rawValue, ok := value[1].(string)
	if !ok {
		return time.Time{}, 0, false
	}

	parsedValue, err := strconv.ParseFloat(rawValue, 64)
	if err != nil {
		return time.Time{}, 0, false
	}

	return time.Unix(int64(rawTS), 0), parsedValue, true
}

func parseUnixMicroOrMilli(raw int64) time.Time {
	switch {
	case raw > 1_000_000_000_000_000:
		return time.UnixMicro(raw)
	case raw > 1_000_000_000_000:
		return time.UnixMilli(raw)
	default:
		return time.Unix(raw, 0)
	}
}

func startClientSpan(ctx context.Context, tracer trace.Tracer, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	ctx, span := tracer.Start(ctx, name)
	span.SetAttributes(attrs...)
	return ctx, span
}
