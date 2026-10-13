package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	defaultHealthURL = "http://localhost:8080/api/v1/health"
	defaultTimeout   = 10 * time.Second
)

func main() {
	targetURL := defaultHealthURL
	if len(os.Args) > 1 {
		targetURL = os.Args[1]
	}

	client := &http.Client{Timeout: defaultTimeout}
	resp, err := client.Get(targetURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Health check failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read response: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Status: %s\n", resp.Status)
	fmt.Println(string(body))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return
	}
	os.Exit(1)
}
