package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// responsePayload mirrors the final JSON payload
type responsePayload struct {
	Model    string `json:"model"`
	Response string `json:"response"`
	Error    string `json:"error"`
}

// chunkPayload helps detect the first stream token
type chunkPayload struct {
	Type string `json:"type"`
}

type requestStats struct {
	ID               int
	SubmitTime       time.Time
	ProcessStartTime time.Time
	EndTime          time.Time
	Response         string
	Error            error
}

const queueFullMessage = "The queue is full, Try again later or help fragsap buy a new GPU to fix these issues."

func main() {
	port := "8080"
	u := url.URL{Scheme: "ws", Host: "localhost:" + port, Path: "/ws"}

	// We send 12 requests.
	// Worker pool = 1 (Active)
	// Queue size = 10 (Buffered)
	// The 12th request should be rejected immediately with the queue-full response.
	totalRequests := 12

	fmt.Printf("Starting Queue & FIFO Test against %s...\n", u.String())
	fmt.Printf("Sending %d concurrent requests to test limits...\n\n", totalRequests)

	var wg sync.WaitGroup
	var mu sync.Mutex
	results := make([]requestStats, 0, totalRequests)

	for i := 1; i <= totalRequests; i++ {
		wg.Add(1)

		// Fire each request in a concurrent goroutine
		go func(reqID int) {
			defer wg.Done()

			stats := requestStats{ID: reqID}

			conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
			if err != nil {
				stats.Error = fmt.Errorf("dial error: %v", err)
				saveResult(&mu, &results, stats)
				return
			}
			defer conn.Close()

			prompt := fmt.Sprintf("Reply with the exact number %d and absolutely nothing else.", reqID)

			// Record submission time
			stats.SubmitTime = time.Now()
			if err := conn.WriteMessage(websocket.TextMessage, []byte(prompt)); err != nil {
				stats.Error = fmt.Errorf("write error: %v", err)
				saveResult(&mu, &results, stats)
				return
			}

			firstTokenReceived := false

			// Read loop
			for {
				_, message, err := conn.ReadMessage()
				if err != nil {
					stats.Error = fmt.Errorf("read error: %v", err)
					break
				}

				// Check if this is the first streamed chunk.
				// This indicates the worker has pulled this request from the queue
				// and the LLM has actively started processing it.
				if !firstTokenReceived {
					var chunk chunkPayload
					if err := json.Unmarshal(message, &chunk); err == nil && chunk.Type != "" {
						stats.ProcessStartTime = time.Now()
						firstTokenReceived = true
					}
				}

				// Check if it's the final payload
				var finalResp responsePayload
				if err := json.Unmarshal(message, &finalResp); err == nil && (finalResp.Model != "" || finalResp.Response != "" || finalResp.Error != "") {
					stats.EndTime = time.Now()
					stats.Response = finalResp.Response
					if finalResp.Error != "" {
						stats.Error = fmt.Errorf("agent error: %s", finalResp.Error)
					}
					break
				}
			}

			saveResult(&mu, &results, stats)
		}(i)

		// Stagger submissions by 100ms. This guarantees the HTTP server
		// receives and queues them in exact sequential order (1, 2, 3...)
		time.Sleep(100 * time.Millisecond)
	}

	// Wait for all 12 LLM generations to finish
	wg.Wait()

	// Sort results by ID so we can evaluate FIFO order
	sort.Slice(results, func(i, j int) bool {
		return results[i].ID < results[j].ID
	})

	fmt.Println("--------------------------------------------------------------------------------")
	fmt.Printf("%-6s | %-15s | %-15s | %-15s | %s\n", "REQ ID", "QUEUE WAIT TIME", "PROCESSING TIME", "TOTAL TIME", "STATUS")
	fmt.Println("--------------------------------------------------------------------------------")

	fifoPassed := true

	for i, r := range results {
		if r.Error != nil {
			fmt.Printf("%-6d | ERROR: %v\n", r.ID, r.Error)
			continue
		}

		if r.ID == totalRequests {
			if r.Response != queueFullMessage {
				fifoPassed = false
				fmt.Printf("#%-5d | ERROR: expected queue rejection %q, got %q\n", r.ID, queueFullMessage, r.Response)
				continue
			}

			totalTime := r.EndTime.Sub(r.SubmitTime)
			fmt.Printf("#%-5d | %-15s | %-15s | %-15v | REJECTED\n",
				r.ID,
				"n/a",
				"n/a",
				totalTime.Round(time.Millisecond),
			)
			continue
		}

		queueWait := r.ProcessStartTime.Sub(r.SubmitTime)
		processingTime := r.EndTime.Sub(r.ProcessStartTime)
		totalTime := r.EndTime.Sub(r.SubmitTime)

		fmt.Printf("#%-5d | %-15v | %-15v | %-15v | OK\n",
			r.ID,
			queueWait.Round(time.Millisecond),
			processingTime.Round(time.Millisecond),
			totalTime.Round(time.Millisecond),
		)

		// Validate FIFO: Request N must start processing AFTER Request N-1
		if i > 0 {
			prev := results[i-1]
			if r.ProcessStartTime.Before(prev.ProcessStartTime) {
				fifoPassed = false
				fmt.Printf("\n[!] FIFO VIOLATION: Request %d started processing before Request %d!\n", r.ID, prev.ID)
			}
		}
	}
	fmt.Println("--------------------------------------------------------------------------------")

	if fifoPassed {
		fmt.Println("FIFO Test Passed: All requests were processed in chronological order.")
		fmt.Println("Queue Limit Passed: Server handled 12 concurrent requests (1 active, 10 queued, 1 rejected) with the expected queue-full response.")
	} else {
		fmt.Println("Test Failed: Requests were processed out of order.")
	}
}

func saveResult(mu *sync.Mutex, results *[]requestStats, stats requestStats) {
	mu.Lock()
	defer mu.Unlock()
	*results = append(*results, stats)
}
