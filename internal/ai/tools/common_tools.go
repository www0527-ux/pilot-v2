package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// GetCurrentTimeInput 获取当前时间输入
type GetCurrentTimeInput struct {
	Format string `json:"format" jsonschema:"description=时间格式，可选值：datetime、timestamp、date"`
}

// GetCurrentTimeOutput 获取当前时间输出
type GetCurrentTimeOutput struct {
	Success   bool   `json:"success"`
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
}

// NewGetCurrentTimeTool 创建获取当前时间工具。
func NewGetCurrentTimeTool() (tool.InvokableTool, error) {
	t, err := utils.InferOptionableTool(
		"get_current_time",
		"获取当前时间，用于时间相关的查询和分析",
		func(ctx context.Context, input *GetCurrentTimeInput, opts ...tool.Option) (string, error) {
			now := time.Now()
			var timestamp string
			format := "datetime"
			if input != nil && input.Format != "" {
				format = input.Format
			}

			switch format {
			case "timestamp":
				timestamp = fmt.Sprintf("%d", now.Unix())
			case "date":
				timestamp = now.Format("2006-01-02")
			default: // datetime
				timestamp = now.Format("2006-01-02 15:04:05")
			}

			output := GetCurrentTimeOutput{
				Success:   true,
				Timestamp: timestamp,
				Message:   "获取时间成功",
			}
			jsonBytes, _ := json.MarshalIndent(output, "", "  ")
			return string(jsonBytes), nil
		})

	if err != nil {
		return nil, fmt.Errorf("create get current time tool: %w", err)
	}

	return t, nil
}
