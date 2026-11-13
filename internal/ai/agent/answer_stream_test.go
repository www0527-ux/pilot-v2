package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"shanyangcode.com/pilot/internal/ai/progress"
	"shanyangcode.com/pilot/internal/memory"
)

func TestAnswerStreamCommitsTurnOrReturnsPersistenceFailure(t *testing.T) {
	for _, failSave := range []bool{false, true} {
		t.Run(fmt.Sprintf("failSave=%v", failSave), func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&memory.SessionContext{}, &memory.ConversationHistory{}); err != nil {
				t.Fatal(err)
			}
			if failSave {
				if err := db.Exec("CREATE TRIGGER reject_history BEFORE INSERT ON conversation_histories BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			store := memory.NewSessionContextManager(db)
			loaded, err := store.Load(ctx, "owner", "session")
			if err != nil {
				t.Fatal(err)
			}
			run := &contextRun{store: store, loaded: loaded, rawQuery: "继续", release: func() {}}
			ctx = context.WithValue(progress.WithRunID(ctx, "run-one"), contextRunKey{}, run)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var request struct {
					Stream bool `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if !request.Stream {
					fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"investigation done"},"finish_reason":"stop"}]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"final answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			reader, err := newStreamingTestRunnable(t, ctx, server.URL).Stream(ctx, []*schema.Message{schema.SystemMessage("system"), schema.UserMessage("expanded continue instruction")})
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			var answer string
			var streamErr error
			for {
				message, err := reader.Recv()
				if err != nil {
					streamErr = err
					break
				}
				if event, ok := progress.FromMessage(message); ok && event.Kind == "answer" && event.Status == "delta" {
					answer += event.Text
				}
			}
			if answer != "final answer" || calls.Load() != 2 {
				t.Fatalf("answer=%q model calls=%d", answer, calls.Load())
			}
			var history []memory.ConversationHistory
			db.Order("id ASC").Find(&history)
			saved, err := store.Load(ctx, "owner", "session")
			if err != nil {
				t.Fatal(err)
			}
			if failSave {
				if !errors.Is(streamErr, memory.ErrTurnPersistence) || len(history) != 0 || saved.Record.Revision != 0 {
					t.Fatalf("failed commit: err=%v history=%v snapshot=%+v", streamErr, history, saved)
				}
			} else {
				if streamErr != io.EOF || len(history) != 2 || history[0].Content != "继续" || len(saved.Messages) != 2 || saved.Messages[0].Content != "expanded continue instruction" {
					t.Fatalf("commit: err=%v history=%v messages=%v", streamErr, history, saved.Messages)
				}
				for _, message := range saved.Messages {
					if message.Role == schema.System {
						t.Fatal("persisted temporary system prompt")
					}
				}
			}
		})
	}
}

func TestSeparatedStreamPublishesOnlyFinalModelTextLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var calls atomic.Int32
	finish := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream   bool              `json:"stream"`
			Tools    []any             `json:"tools"`
			Messages []*schema.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		n := calls.Add(1)
		if n <= 2 {
			if req.Stream {
				t.Error("investigation must not masquerade as final stream")
			}
			message := map[string]any{"role": "assistant", "content": "这是一段收集过程，不是回答"}
			if n == 1 {
				message["tool_calls"] = []any{map[string]any{"id": "clock", "type": "function", "function": map[string]any{"name": "get_current_time", "arguments": "{}"}}}
			}
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": "stop"}}})
			return
		}
		if n != 3 || !req.Stream || len(req.Tools) != 0 {
			t.Errorf("final request: round=%d stream=%v tools=%d", n, req.Stream, len(req.Tools))
		}
		found := false
		for _, message := range req.Messages {
			if message.Role == schema.Tool && message.ToolCallID == "clock" && strings.Contains(message.Content, "true") {
				found = true
			}
		}
		if !found {
			t.Error("final model lost actual tool evidence")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"## 最终结论\\n\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"已根据真实工具结果生成。\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	reader, err := newStreamingTestRunnable(t, ctx, server.URL).Stream(ctx, []*schema.Message{schema.UserMessage("查询时间")})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	started, ended, released, toolStarted := false, false, false, false
	var answer strings.Builder
	for {
		message, err := reader.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		event, ok := progress.FromMessage(message)
		if !ok || message.Content != "" {
			t.Fatalf("unclassified text: %+v", message)
		}
		if event.Kind == "tool" && event.Status == "running" {
			toolStarted = true
			if event.GroupID == "" || event.Round == 0 {
				t.Fatal("tool has no group or round")
			}
		}
		if event.Kind == "answer" {
			switch event.Status {
			case "started":
				started = true
			case "delta":
				if !started || ended {
					t.Fatal("delta crossed lifecycle boundary")
				}
				answer.WriteString(event.Text)
				if !released {
					released = true
					close(finish)
				} // proves live delivery before provider EOF
			case "ended":
				ended = true
			}
		}
	}
	if !started || !ended || !toolStarted || answer.String() != "## 最终结论\n已根据真实工具结果生成。" {
		t.Fatalf("answer=%q started=%v ended=%v tool=%v", answer.String(), started, ended, toolStarted)
	}
}

func TestAnswerStreamCancellationAndProviderFailureKeepPartialAnswer(t *testing.T) {
	for _, cancelled := range []bool{true, false} {
		t.Run(fmt.Sprintf("cancelled=%v", cancelled), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Stream bool `json:"stream"`
				}
				json.NewDecoder(r.Body).Decode(&req)
				if !req.Stream {
					fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"Investigation finished"},"finish_reason":"stop"}]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial answer\"}}]}\n\n")
				w.(http.Flusher).Flush()
				if cancelled {
					<-r.Context().Done()
				} else {
					select {
					case <-release:
						fmt.Fprint(w, "data: malformed-json\n\n")
					case <-r.Context().Done():
					}
				}
			}))
			defer server.Close()
			reader, err := newStreamingTestRunnable(t, ctx, server.URL).Stream(ctx, []*schema.Message{schema.UserMessage("test")})
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			partial, ended := "", false
			for {
				message, err := reader.Recv()
				if err != nil {
					if err == io.EOF || partial != "partial answer" || ended {
						t.Fatalf("invalid termination: partial=%q ended=%v err=%v", partial, ended, err)
					}
					break
				}
				event, ok := progress.FromMessage(message)
				if !ok {
					t.Fatal("unclassified model output")
				}
				if event.Kind == "answer" && event.Status == "ended" {
					ended = true
				}
				if event.Kind == "answer" && event.Status == "delta" {
					partial += event.Text
					if cancelled {
						cancel()
					} else {
						close(release)
					}
				}
			}
		})
	}
}

func TestAnswerStreamSlowConsumerCloseCancelsProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	streamStarted, providerCancelled := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"Investigation finished"},"finish_reason":"stop"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 65; i++ {
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer chunk\"}}]}\n\n")
		}
		w.(http.Flusher).Flush()
		close(streamStarted)
		<-r.Context().Done()
		close(providerCancelled)
	}))
	defer server.Close()
	reader, err := newStreamingTestRunnable(t, ctx, server.URL).Stream(ctx, []*schema.Message{schema.UserMessage("test")})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-streamStarted:
	case <-ctx.Done():
		t.Fatal("final model stream did not start")
	}
	reader.Close()
	select {
	case <-providerCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("closing a blocked output reader left the provider running")
	}
}
