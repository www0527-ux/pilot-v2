package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"syscall"
	"time"

	openai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	openaiapi "github.com/meguminnnnnnnnn/go-openai"
	"shanyangcode.com/pilot/internal/ai/progress"
)

const maxModelRetries = 2

// Retry individual model calls, never the whole Agent or completed tools.
func modelRetryConfig() *adk.ModelRetryConfig {
	return &adk.ModelRetryConfig{
		MaxRetries: maxModelRetries,
		ShouldRetry: func(ctx context.Context, attempt *adk.RetryContext) *adk.RetryDecision {
			retry := retryableModelError(ctx, attempt.Err)
			if retry && attempt.RetryAttempt <= maxModelRetries {
				emitModelRetry(ctx, attempt.RetryAttempt)
			}
			return &adk.RetryDecision{Retry: retry}
		},
		BackoffFunc: modelRetryBackoff,
	}
}

func retryableModelError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var adapterErr *openai.APIError
	if errors.As(err, &adapterErr) {
		if adapterErr.Type == "insufficient_quota" || adapterErr.Code == "insufficient_quota" {
			return false
		}
		return retryableModelStatus(adapterErr.HTTPStatusCode)
	}
	var apiErr *openaiapi.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Type == "insufficient_quota" || apiErr.Code == "insufficient_quota" {
			return false
		}
		return retryableModelStatus(apiErr.HTTPStatusCode)
	}
	var requestErr *openaiapi.RequestError
	if errors.As(err, &requestErr) && requestErr.HTTPStatusCode != 0 {
		return retryableModelStatus(requestErr.HTTPStatusCode)
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && (networkErr.Timeout() || networkErr.Temporary()) {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ETIMEDOUT)
}

func retryableModelStatus(status int) bool {
	switch status {
	case 408, 429, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}

func modelRetryBackoff(_ context.Context, attempt int) time.Duration {
	if attempt > 2 {
		attempt = 2
	}
	if attempt < 1 {
		attempt = 1
	}
	return 300*time.Millisecond*time.Duration(1<<(attempt-1)) + time.Duration(rand.Int64N(int64(150*time.Millisecond)))
}

func emitModelRetry(ctx context.Context, attempt int) {
	progress.Emit(ctx, progress.Event{Kind: "notice", Status: "warning", Summary: fmt.Sprintf("模型连接暂时异常，正在重试（%d/%d）", attempt, maxModelRetries)})
}

// The final call is outside ADK. Retry only before any answer text is emitted,
// keeping live output and avoiding duplicate text or re-executing tools.
func readFinalAnswer(ctx context.Context, finalModel model.ToolCallingChatModel, messages []*schema.Message) (string, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		stream, err := finalModel.Stream(ctx, messages)
		var answer string
		if err == nil {
			answer, err = progress.ReadAnswer(ctx, stream)
		}
		if err == nil || answer != "" || attempt >= maxModelRetries || !retryableModelError(ctx, err) {
			return answer, err
		}
		emitModelRetry(ctx, attempt+1)
		timer := time.NewTimer(modelRetryBackoff(ctx, attempt+1))
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}
