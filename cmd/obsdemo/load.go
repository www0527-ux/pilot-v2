package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Continuous low-rate traffic keeps recording/Agent queries backed by fresh
// samples. It never calls Chat or reads the operator's latency switch.
func runLoad(target, output string) error {
	file, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var group sync.WaitGroup
	var writeMu sync.Mutex
	var writeErr error
	encoder := json.NewEncoder(file)
	slots := make(chan struct{}, 4)
	for {
		select {
		case <-ctx.Done():
			group.Wait()
			return writeErr
		case <-ticker.C:
			select {
			case slots <- struct{}{}:
			default:
				continue
			}
			group.Add(1)
			go func() {
				defer group.Done()
				defer func() { <-slots }()
				started := time.Now()
				status, traceID := 0, ""
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"/checkout", nil)
				if err == nil {
					var response *http.Response
					response, err = client.Do(request)
					if err == nil {
						_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
						_ = response.Body.Close()
						status, traceID = response.StatusCode, response.Header.Get("X-Trace-ID")
					}
				}
				if err != nil {
					status = 0
				}
				writeMu.Lock()
				defer writeMu.Unlock()
				if writeErr == nil {
					writeErr = encoder.Encode(struct {
						Timestamp  time.Time `json:"timestamp"`
						DurationMS float64   `json:"duration_ms"`
						Status     int       `json:"status"`
						TraceID    string    `json:"trace_id"`
					}{started.UTC(), float64(time.Since(started).Microseconds()) / 1000, status, traceID})
					if writeErr != nil {
						cancel()
					}
				}
			}()
		}
	}
}
