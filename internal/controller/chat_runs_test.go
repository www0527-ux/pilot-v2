package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"shanyangcode.com/pilot/internal/ai/progress"
)

type replayAgent struct {
	fakeAgent
	calls         atomic.Int32
	ready, finish chan struct{}
	fail          bool
}

func (a *replayAgent) Stream(ctx context.Context, _, _, _ string) (*schema.StreamReader[*schema.Message], error) {
	a.calls.Add(1)
	reader, writer := schema.Pipe[*schema.Message](16)
	go func() {
		defer writer.Close()
		for _, event := range []progress.Event{{Kind: "tool", ID: "t1", Name: "query_logs", Status: "running"}, {Kind: "tool", ID: "t1", Name: "query_logs", Status: "completed"}, {Kind: "answer", Status: "started"}, {Kind: "answer", Status: "delta", Text: "正文第一段"}} {
			writer.Send(progress.Message(event), nil)
		}
		close(a.ready)
		select {
		case <-a.finish:
		case <-ctx.Done():
			writer.Send(nil, ctx.Err())
			return
		}
		if a.fail {
			writer.Send(nil, io.ErrUnexpectedEOF)
			return
		}
		writer.Send(progress.Message(progress.Event{Kind: "answer", Status: "delta", Text: "，第二段"}), nil)
		writer.Send(progress.Message(progress.Event{Kind: "answer", Status: "ended"}), nil)
	}()
	return reader, nil
}
func runTestServer(a *replayAgent) (*httptest.Server, *ChatController) {
	c := NewChatController(a, nil, &fakeShortTerm{}, &fakeHistory{})
	engine := gin.New()
	engine.POST("/stream", c.StreamChat)
	engine.GET("/runs/:run_id/events", c.RunEvents)
	engine.POST("/runs/:run_id/cancel", c.CancelRun)
	return httptest.NewServer(engine), c
}
func TestRunDisconnectReplayIsOrderedAndDoesNotExecuteTwice(t *testing.T) {
	a := &replayAgent{ready: make(chan struct{}), finish: make(chan struct{})}
	server, c := runTestServer(a)
	defer server.Close()
	id := uuid.NewString()
	body := `{"user_id":"u1","session_id":"s1","query":"测试","include_progress":true,"run_id":"` + id + `"}`
	response, err := http.Post(server.URL+"/stream", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	<-a.ready
	response.Body.Close() // disconnect does not cancel the actual task
	r, _ := c.findRun(context.Background(), id, "u1", "s1")
	first, _, _ := r.replay(0)
	after := first[len(first)-1].Sequence
	close(a.finish)
	replayed, err := http.Get(server.URL + "/runs/" + id + "/events?user_id=u1&session_id=s1&after=" + jsonNumber(after))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(replayed.Body)
	replayed.Body.Close()
	if !strings.Contains(string(data), "[DONE]") || !strings.Contains(string(data), `"status":"completed"`) || a.calls.Load() != 1 {
		t.Fatalf("calls=%d replay=%s", a.calls.Load(), data)
	}
	events, _, _ := r.replay(0)
	for i, event := range events {
		if event.Sequence != uint64(i+1) || event.RunID != id {
			t.Fatalf("event=%+v", event)
		}
	}
	if r.snapshot().Answer != "正文第一段，第二段" {
		t.Fatal(r.snapshot().Answer)
	}
	// Replaying a terminal journal is read-only, including duplicate POSTs.
	response, err = http.Post(server.URL+"/stream", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if a.calls.Load() != 1 {
		t.Fatal("duplicate POST executed tools again")
	}
}
func jsonNumber(value uint64) string { data, _ := json.Marshal(value); return string(data) }
func TestRunFailureAndCancellationKeepPartialAnswer(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "provider failure", true: "user cancellation"}[cancelled], func(t *testing.T) {
			a := &replayAgent{ready: make(chan struct{}), finish: make(chan struct{}), fail: !cancelled}
			server, c := runTestServer(a)
			defer server.Close()
			id := uuid.NewString()
			body := `{"user_id":"u1","session_id":"s1","query":"测试","include_progress":true,"run_id":"` + id + `"}`
			response, err := http.Post(server.URL+"/stream", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			<-a.ready
			if cancelled {
				cancelledResponse, err := http.Post(server.URL+"/runs/"+id+"/cancel?user_id=u1&session_id=s1", "application/json", nil)
				if err != nil {
					t.Fatal(err)
				}
				cancelledResponse.Body.Close()
			} else {
				close(a.finish)
			}
			data, _ := io.ReadAll(response.Body)
			response.Body.Close()
			r, _ := c.findRun(context.Background(), id, "u1", "s1")
			wanted := "failed"
			if cancelled {
				wanted = "cancelled"
			}
			if r.snapshot().Status != wanted || !strings.HasPrefix(r.snapshot().Answer, "正文第一段") || strings.Contains(string(data), `"kind":"run","status":"completed"`) {
				t.Fatalf("snapshot=%+v SSE=%s", r.snapshot(), data)
			}
			before, _, _ := r.replay(0)
			r.append(progress.RunEvent("completed", "late success"))
			r.append(progress.Event{Kind: "answer", Status: "delta", Text: "late token"})
			after, _, _ := r.replay(0)
			if len(before) != len(after) {
				t.Fatal("late events overwrote termination")
			}
		})
	}
}
func TestRunCannotCompleteBeforeAnswerEnd(t *testing.T) {
	r := &chatRun{notify: make(chan struct{})}
	r.append(progress.RunEvent("executing", "start"))
	r.append(progress.RunEvent("completed", "wrong"))
	if r.snapshot().Status != "failed" {
		t.Fatal(r.snapshot().Status)
	}
}
