// Package httpapi 定义 Pilot 全部 HTTP 接口共用的响应契约：
//
//	成功: HTTP 2xx + {"success":true,"code":"OK","message":...,"data":...}
//	失败: HTTP 4xx/5xx + {"success":false,"code":<稳定错误码>,"message":...}
//
// code 是机器可读的稳定枚举，客户端不应解析 message 做分支判断。
package httpapi

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// 稳定错误码枚举。新增错误场景必须复用或在此登记，禁止散落字符串。
const (
	CodeOK                    = "OK"
	CodeInvalidArgument       = "INVALID_ARGUMENT"       // 400：请求参数不合法
	CodeUnauthenticated       = "UNAUTHENTICATED"        // 401：缺少或错误的 API Key
	CodeNotFound              = "NOT_FOUND"              // 404：资源或路由不存在
	CodeFeatureDisabled       = "FEATURE_DISABLED"       // 404：能力未启用（配置关闭）
	CodeRateLimited           = "RATE_LIMITED"           // 429：触发限流
	CodeDependencyUnavailable = "DEPENDENCY_UNAVAILABLE" // 503：Redis/ES/MySQL/观测数据源不可用
	CodeUpstreamError         = "UPSTREAM_ERROR"         // 502：LLM 等上游模型服务失败
	CodeInternal              = "INTERNAL"               // 500：未分类内部错误
)

// Response 是统一响应封装。
type Response struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Error 是可预期的 API 错误：携带 HTTP 状态码与稳定错误码。
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return e.Message
}

// NewError 构造一个带状态与错误码的 API 错误。
func NewError(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// InvalidArgument 表示请求参数不合法（400）。
func InvalidArgument(message string) *Error {
	return NewError(http.StatusBadRequest, CodeInvalidArgument, message)
}

// Unauthenticated 表示缺少或错误的凭证（401）。
func Unauthenticated(message string) *Error {
	return NewError(http.StatusUnauthorized, CodeUnauthenticated, message)
}

// NotFound 表示资源或路由不存在（404）。
func NotFound(message string) *Error {
	return NewError(http.StatusNotFound, CodeNotFound, message)
}

// FeatureDisabled 表示能力被配置关闭（404）。
func FeatureDisabled(message string) *Error {
	return NewError(http.StatusNotFound, CodeFeatureDisabled, message)
}

// RateLimited 表示触发限流（429）。
func RateLimited(message string) *Error {
	return NewError(http.StatusTooManyRequests, CodeRateLimited, message)
}

// DependencyUnavailable 表示存储或观测数据源不可用（503）。
func DependencyUnavailable(message string) *Error {
	return NewError(http.StatusServiceUnavailable, CodeDependencyUnavailable, message)
}

// UpstreamError 表示 LLM 等上游服务调用失败（502）。
func UpstreamError(message string) *Error {
	return NewError(http.StatusBadGateway, CodeUpstreamError, message)
}

// Internal 表示未分类内部错误（500）。
func Internal(message string) *Error {
	return NewError(http.StatusInternalServerError, CodeInternal, message)
}

// OK 写入成功响应。
func OK(c *gin.Context, message string, data any) {
	c.JSON(http.StatusOK, Response{
		Success: true,
		Code:    CodeOK,
		Message: message,
		Data:    data,
	})
}

// Fail 写入失败响应；识别 *Error，其余错误按 INTERNAL 兜底。
func Fail(c *gin.Context, err error) {
	apiErr := AsError(err)
	c.JSON(apiErr.Status, Response{
		Success: false,
		Code:    apiErr.Code,
		Message: apiErr.Message,
	})
}

// AsError 将任意 error 规整为 *Error；非 *Error 一律视为 INTERNAL，
// 并使用对外安全的通用文案（原始错误应由调用方记录日志）。
func AsError(err error) *Error {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return Internal("服务内部错误，请稍后重试")
}
