package httpapi

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// SSEDoneSentinel marks the end of the event stream; the run event carries its outcome.
const SSEDoneSentinel = "[DONE]"

// SetSSEHeaders 设置 Server-Sent Events 响应头。
func SetSSEHeaders(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
}

// WriteSSEData 将负载按 SSE 规范写入并立即刷出。
func WriteSSEData(c *gin.Context, payload string) {
	_, _ = c.Writer.WriteString(FormatSSEDataBlock(payload))
	c.Writer.Flush()
}

// FormatSSEDataBlock 将字符串格式化为 SSE 数据块：每行以 "data: " 开头，双换行结尾。
func FormatSSEDataBlock(payload string) string {
	var builder strings.Builder
	normalized := strings.ReplaceAll(payload, "\r\n", "\n")
	for _, line := range strings.Split(normalized, "\n") {
		builder.WriteString("data: ")
		builder.WriteString(line)
		builder.WriteString("\n")
	}
	builder.WriteString("\n")
	return builder.String()
}
