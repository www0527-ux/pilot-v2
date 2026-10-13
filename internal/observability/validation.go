package observability

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

const (
	MaxFieldLength        = 128
	MaxLogKeywordLength   = 500
	MaxPromQLLength       = 500
	MaxQueryRange         = 24 * time.Hour
	DefaultQueryRange     = 60 * time.Minute
	MaxLogLimit           = 500
	DefaultLogLimit       = 100
	MinLogOutputRunes     = 256
	MaxLogOutputRunes     = 100000
	DefaultLogOutputRunes = 12000
	DefaultDurationMins   = 60
	MaxDurationMins       = int(MaxQueryRange / time.Minute)
)

// NormalizePromQL keeps user/model supplied PromQL within predictable bounds.
func NormalizePromQL(query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", fmt.Errorf("PromQL 查询不能为空")
	}
	if len([]rune(query)) > MaxPromQLLength {
		return "", fmt.Errorf("PromQL 查询长度不能超过%d个字符", MaxPromQLLength)
	}
	if hasControlRune(query) {
		return "", fmt.Errorf("PromQL 查询不能包含控制字符")
	}
	return query, nil
}

func NormalizeService(value, defaultValue string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = strings.TrimSpace(defaultValue)
	}
	if value == "" {
		return "", nil
	}
	if len([]rune(value)) > MaxFieldLength {
		return "", fmt.Errorf("service 长度不能超过%d个字符", MaxFieldLength)
	}
	if !isSafeLabelValue(value) {
		return "", fmt.Errorf("service 只能包含字母、数字、点、下划线和短横线")
	}
	return value, nil
}

func NormalizeSeverity(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	switch value {
	case "critical", "warning", "info":
		return value, nil
	default:
		return "", fmt.Errorf("severity 仅支持 critical、warning、info")
	}
}

func NormalizeLogLevel(value, defaultValue string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(defaultValue))
	}
	if value == "" {
		return "", nil
	}
	switch value {
	case "all", "全部":
		return "all", nil
	case "debug", "info", "warn", "warning", "error", "fatal":
		if value == "warning" {
			return "warn", nil
		}
		return value, nil
	default:
		return "", fmt.Errorf("level 仅支持 all、debug、info、warn、error、fatal")
	}
}

// LogLevels is the canonical ordered level vocabulary shared by HTTP, Agent
// tools, MCP, and the Web UI. "all" means no level filter.
func LogLevels() []string {
	return []string{"all", "debug", "info", "warn", "error", "fatal"}
}

func NormalizeLogKeyword(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len([]rune(value)) > MaxLogKeywordLength {
		return "", fmt.Errorf("日志关键词长度不能超过%d个字符", MaxLogKeywordLength)
	}
	if hasControlRune(value) {
		return "", fmt.Errorf("日志关键词不能包含控制字符")
	}
	return value, nil
}

func NormalizeTraceID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("trace id cannot be empty")
	}
	if len([]rune(value)) > MaxFieldLength {
		return "", fmt.Errorf("trace id 长度不能超过%d个字符", MaxFieldLength)
	}
	if !isSafeLabelValue(value) {
		return "", fmt.Errorf("trace id 只能包含字母、数字、点、下划线和短横线")
	}
	return value, nil
}

func NormalizeDurationMinutes(value int) int {
	if value <= 0 {
		return DefaultDurationMins
	}
	if value > MaxDurationMins {
		return MaxDurationMins
	}
	return value
}

func NormalizeLimit(value int) int {
	if value <= 0 {
		return DefaultLogLimit
	}
	if value > MaxLogLimit {
		return MaxLogLimit
	}
	return value
}

// NormalizeLogOutputRunes bounds response/prompt log text without counting
// bytes, so CJK and emoji are never cut in the middle of an encoded rune.
func NormalizeLogOutputRunes(value int) int {
	if value <= 0 {
		return DefaultLogOutputRunes
	}
	if value < MinLogOutputRunes {
		return MinLogOutputRunes
	}
	if value > MaxLogOutputRunes {
		return MaxLogOutputRunes
	}
	return value
}

func NormalizeTimeRange(startTime, endTime time.Time) (time.Time, time.Time) {
	if startTime.IsZero() || endTime.IsZero() || !startTime.Before(endTime) {
		endTime = time.Now()
		startTime = endTime.Add(-DefaultQueryRange)
	}
	if endTime.Sub(startTime) > MaxQueryRange {
		startTime = endTime.Add(-MaxQueryRange)
	}
	return startTime, endTime
}

func hasControlRune(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) && r != '\t' {
			return true
		}
	}
	return false
}

func isSafeLabelValue(value string) bool {
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}
