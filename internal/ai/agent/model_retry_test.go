package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	openaiapi "github.com/meguminnnnnnnnn/go-openai"
	"shanyangcode.com/pilot/internal/ai/progress"
)

func TestRetryableModelErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"reset stream", fmt.Errorf("wrapped: %w", io.ErrUnexpectedEOF), true},
		{"rate limit", &openaiapi.APIError{HTTPStatusCode: 429}, true},
		{"adapter rate limit", &openai.APIError{HTTPStatusCode: 429}, true},
		{"bad gateway", &openaiapi.RequestError{HTTPStatusCode: 502}, true},
		{"unauthorized", &openaiapi.APIError{HTTPStatusCode: 401}, false},
		{"bad arguments", &openaiapi.APIError{HTTPStatusCode: 400}, false},
		{"payment required", &openaiapi.APIError{HTTPStatusCode: 402}, false},
		{"quota exhausted", &openaiapi.APIError{HTTPStatusCode: 429, Code: "insufficient_quota"}, false},
		{"cancelled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"invalid response", errors.New("invalid JSON"), false},
		{"success", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableModelError(context.Background(), tc.err); got != tc.want {
				t.Fatalf("retry=%v, want=%v", got, tc.want)
			}
		})
	}
}

func TestModelRetriesDoNotRepeatCompletedTools(t *testing.T) {
	for _, phase := range []string{"investigation", "final"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var generateCalls, streamCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), `"stream":true`) {
					n := streamCalls.Add(1)
					if phase == "final" && n == 1 {
						writeRetryTestError(w, 503, "temporary_failure")
						return
					}
					if !strings.Contains(string(body), `"tool_call_id":"clock"`) {
						t.Error("final model lost completed tool evidence")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"final answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					return
				}
				n := generateCalls.Add(1)
				if n == 1 {
					fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"clock","type":"function","function":{"name":"get_current_time","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
					return
				}
				if !strings.Contains(string(body), `"tool_call_id":"clock"`) {
					t.Error("retry lost completed tool evidence")
				}
				if phase == "investigation" && n == 2 {
					writeRetryTestError(w, 502, "temporary_failure")
					return
				}
				fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"Evidence collected"},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()
			reader, err := newStreamingTestRunnable(t, ctx, server.URL).Stream(ctx, []*schema.Message{schema.UserMessage("test")})
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			toolStarts, notices, answer := 0, 0, ""
			for {
				message, err := reader.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				event, _ := progress.FromMessage(message)
				if event.Kind == "tool" && event.Status == "running" {
					toolStarts++
				}
				if event.Kind == "notice" && strings.Contains(event.Summary, "正在重试") {
					notices++
				}
				if event.Kind == "answer" && event.Status == "delta" {
					answer += event.Text
				}
			}
			wantGenerate, wantStream := int32(3), int32(1)
			if phase == "final" {
				wantGenerate, wantStream = 2, 2
			}
			if toolStarts != 1 || notices != 1 || answer != "final answer" || generateCalls.Load() != wantGenerate || streamCalls.Load() != wantStream {
				t.Fatalf("tools=%d notices=%d answer=%q generate=%d stream=%d", toolStarts, notices, answer, generateCalls.Load(), streamCalls.Load())
			}
		})
	}
}

func writeRetryTestError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"message":"test failure","type":"test","code":%q}}`, code)
}

func TestModelRetryLimitAndPermanentErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		calls  int32
	}{
		{503, "temporary_failure", 3},
		{401, "invalid_api_key", 1},
		{429, "insufficient_quota", 1},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				writeRetryTestError(w, tc.status, tc.code)
			}))
			defer server.Close()
			reader, err := newStreamingTestRunnable(t, ctx, server.URL).Stream(ctx, []*schema.Message{schema.UserMessage("test")})
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			for {
				_, err = reader.Recv()
				if err != nil {
					break
				}
			}
			if errors.Is(err, io.EOF) || calls.Load() != tc.calls {
				t.Fatalf("calls=%d want=%d err=%v", calls.Load(), tc.calls, err)
			}
		})
	}
}

type retryTestStreamModel struct {
	model.ToolCallingChatModel
	stream func(context.Context) (*schema.StreamReader[*schema.Message], error)
}

func (m *retryTestStreamModel) Stream(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return m.stream(ctx)
}

func TestFinalModelMidStreamErrors(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%v", partial), func(t *testing.T) {
			calls, deltas := 0, ""
			ctx := progress.WithEmitter(context.Background(), func(e progress.Event) {
				if e.Kind == "answer" && e.Status == "delta" {
					deltas += e.Text
				}
			})
			m := &retryTestStreamModel{stream: func(context.Context) (*schema.StreamReader[*schema.Message], error) {
				calls++
				if calls > 1 {
					return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("complete", nil)}), nil
				}
				reader, writer := schema.Pipe[*schema.Message](2)
				if partial {
					writer.Send(schema.AssistantMessage("partial", nil), nil)
				}
				writer.Send(nil, io.ErrUnexpectedEOF)
				writer.Close()
				return reader, nil
			}}
			answer, err := readFinalAnswer(ctx, m, nil)
			if partial {
				if calls != 1 || answer != "partial" || deltas != answer || !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("calls=%d answer=%q deltas=%q err=%v", calls, answer, deltas, err)
				}
			} else if calls != 2 || answer != "complete" || deltas != answer || err != nil {
				t.Fatalf("calls=%d answer=%q deltas=%q err=%v", calls, answer, deltas, err)
			}
		})
	}
}

func TestFinalModelCancellationDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = progress.WithEmitter(ctx, func(e progress.Event) {
		if e.Kind == "notice" {
			cancel()
		}
	})
	calls := 0
	m := &retryTestStreamModel{stream: func(context.Context) (*schema.StreamReader[*schema.Message], error) {
		calls++
		return nil, io.ErrUnexpectedEOF
	}}
	_, err := readFinalAnswer(ctx, m, nil)
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
