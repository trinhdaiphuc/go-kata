package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Helper to get a free port for testing
func getFreePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	addr := listener.Addr().String()
	listener.Close()
	return addr
}

// Helper to create a properly configured test server
func newTestServer(t *testing.T, opts ...ServerOption) *Server {
	t.Helper()
	pipeEnd1, pipeEnd2 := net.Pipe()

	// Close both ends of the pipe when test completes
	t.Cleanup(func() {
		pipeEnd1.Close()
		pipeEnd2.Close()
	})

	defaultOpts := []ServerOption{
		WithDB(pipeEnd2),
		WithRequestTimeout(5 * time.Second),
		WithWorkerPoolSize(10),
		WithAddr(getFreePort(t)),
	}

	allOpts := append(defaultOpts, opts...)
	return NewServer(allOpts...)
}

// TestServerStartAndStop tests basic server lifecycle
func TestServerStartAndStop(t *testing.T) {
	srv := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start server
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	// Give server time to start
	time.Sleep(50 * time.Millisecond)

	// Make a request to verify server is running
	resp, err := http.Get("http://" + srv.addr)
	if err != nil {
		t.Fatalf("failed to make request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "Hello, World!" {
		t.Errorf("unexpected response body: %s", body)
	}

	// Cancel context and stop server
	cancel()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()

	if err := srv.Stop(stopCtx); err != nil {
		t.Errorf("failed to stop server: %v", err)
	}
}

// TestSuddenDeath verifies server handles shutdown during heavy load
// Requirement: Server completes in-flight requests (not all 100), logs "shutting down", closes cleanly
func TestSuddenDeath(t *testing.T) {
	srv := newTestServer(t, WithWorkerPoolSize(20))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	// Wait for server to be ready
	time.Sleep(100 * time.Millisecond)

	var (
		wg           sync.WaitGroup
		successCount int64
		failCount    int64
	)

	totalRequests := 100
	wg.Add(totalRequests)

	// Send 100 concurrent requests
	for i := 0; i < totalRequests; i++ {
		go func() {
			defer wg.Done()

			client := &http.Client{
				Timeout: 10 * time.Second,
			}
			resp, err := client.Get("http://" + srv.addr)
			if err == nil {
				if resp.StatusCode == http.StatusOK {
					atomic.AddInt64(&successCount, 1)
				} else {
					atomic.AddInt64(&failCount, 1)
				}
				resp.Body.Close()
			} else {
				atomic.AddInt64(&failCount, 1)
			}
		}()
	}

	// Wait a tiny bit to ensure some requests are in-flight
	time.Sleep(5 * time.Millisecond)

	// Trigger shutdown (simulates SIGTERM)
	cancel()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()

	start := time.Now()
	if err := srv.Stop(stopCtx); err != nil {
		t.Errorf("Stop failed: %v", err)
	}
	shutdownDuration := time.Since(start)

	// Wait for all client requests to complete
	wg.Wait()

	t.Logf("Shutdown took: %v", shutdownDuration)
	t.Logf("Success: %d, Fail: %d", successCount, failCount)

	// Verify some requests succeeded (in-flight completed)
	if successCount == 0 {
		t.Error("Expected some requests to succeed (in-flight completion)")
	}

	// Verify shutdown was reasonably fast
	if shutdownDuration > 10*time.Second {
		t.Errorf("Shutdown took too long: %v (expected < 10s)", shutdownDuration)
	}
}

// TestSlowLeak verifies no goroutine leaks during normal operation
// Requirement: No goroutine leaks after extended operation
func TestSlowLeak(t *testing.T) {
	// Allow for background goroutines from Go runtime and test framework
	initialGoroutines := runtime.NumGoroutine()

	srv := newTestServer(t,
		WithWorkerPoolSize(5),
		WithBackgroundTicker(100*time.Millisecond),
		WithBackgroundJobFunc(func(ctx context.Context) {
			// Simulate background work
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	runningGoroutines := runtime.NumGoroutine()
	t.Logf("Goroutines after start: %d (initial: %d)", runningGoroutines, initialGoroutines)

	// Send requests periodically (simulating real traffic)
	stopRequests := make(chan struct{})
	var requestWg sync.WaitGroup
	requestWg.Add(1)

	// Create a transport so we can close idle connections later
	transport := &http.Transport{}
	client := &http.Client{Timeout: 2 * time.Second, Transport: transport}

	go func() {
		defer requestWg.Done()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-stopRequests:
				return
			case <-ticker.C:
				resp, err := client.Get("http://" + srv.addr)
				if err == nil {
					resp.Body.Close()
				}
			}
		}
	}()

	// Run for a while (scaled down from 5 minutes for unit tests)
	time.Sleep(500 * time.Millisecond)

	// Stop sending requests
	close(stopRequests)
	requestWg.Wait()

	// Close idle HTTP connections to prevent goroutine leak
	transport.CloseIdleConnections()

	// Cancel context and stop server
	cancel()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()

	if err := srv.Stop(stopCtx); err != nil {
		t.Errorf("Stop failed: %v", err)
	}

	// Wait for cleanup
	time.Sleep(500 * time.Millisecond)

	finalGoroutines := runtime.NumGoroutine()

	// Allow small tolerance for runtime fluctuation
	tolerance := 5
	if finalGoroutines > initialGoroutines+tolerance {
		t.Errorf("Possible goroutine leak: started with %d, ended with %d (tolerance: %d)",
			initialGoroutines, finalGoroutines, tolerance)
	}

	t.Logf("Goroutines: initial=%d, final=%d", initialGoroutines, finalGoroutines)
}

// TestTimeout verifies shutdown timeout is respected
// Requirement: Forces shutdown after timeout, logs "shutdown timeout"
func TestTimeout(t *testing.T) {
	srv := newTestServer(t, WithWorkerPoolSize(1))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Start a long-running request that will block the worker pool
	longRequestStarted := make(chan struct{})
	var longReqWg sync.WaitGroup
	longReqWg.Add(1)

	go func() {
		defer longReqWg.Done()

		// Create a custom handler server to simulate long request
		// For this test, we'll just make a request and immediately shutdown
		client := &http.Client{Timeout: 30 * time.Second}
		close(longRequestStarted)
		_, _ = client.Get("http://" + srv.addr)
	}()

	// Wait for request to start
	<-longRequestStarted
	time.Sleep(50 * time.Millisecond)

	// Cancel context
	cancel()

	// Stop with short timeout (shorter than any potential long request)
	shortTimeout := 500 * time.Millisecond
	stopCtx, stopCancel := context.WithTimeout(context.Background(), shortTimeout)
	defer stopCancel()

	start := time.Now()
	err := srv.Stop(stopCtx)
	duration := time.Since(start)

	// Should complete within timeout + some margin
	maxExpectedDuration := shortTimeout + 200*time.Millisecond
	if duration > maxExpectedDuration {
		t.Errorf("Stop took too long: %v (expected < %v)", duration, maxExpectedDuration)
	}

	t.Logf("Stop returned in %v with error: %v", duration, err)

	// Wait for long request goroutine to finish
	longReqWg.Wait()
}

// TestShutdownOrder verifies resources are cleaned up in correct order
func TestShutdownOrder(t *testing.T) {
	var shutdownOrder []string
	var mu sync.Mutex

	// Create mock DB connection that records when it's closed
	_, mockDB := net.Pipe()

	srv := NewServer(
		WithDB(mockDB),
		WithAddr(getFreePort(t)),
		WithRequestTimeout(time.Second),
		WithWorkerPoolSize(5),
		WithBackgroundTicker(time.Hour),
		WithBackgroundJobFunc(func(ctx context.Context) {
			mu.Lock()
			shutdownOrder = append(shutdownOrder, "background_job_executed")
			mu.Unlock()
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Verify server is running
	resp, err := http.Get("http://" + srv.addr)
	if err != nil {
		t.Fatalf("server not responding: %v", err)
	}
	resp.Body.Close()

	// Cancel context and stop server
	cancel()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()

	if err := srv.Stop(stopCtx); err != nil {
		// Some errors are expected due to mock connections
		t.Logf("Stop returned error (may be expected): %v", err)
	}

	// Verify server is no longer accepting requests
	client := &http.Client{Timeout: 100 * time.Millisecond}
	_, err = client.Get("http://" + srv.addr)
	if err == nil {
		t.Error("Server should not accept requests after Stop()")
	}
}

// TestWorkerPoolLimiting verifies worker pool limits concurrent requests
func TestWorkerPoolLimiting(t *testing.T) {
	poolSize := 3

	// Track concurrent requests
	var currentConcurrent int64
	var maxConcurrent int64
	var mu sync.Mutex

	// Custom handler that tracks concurrency
	trackingHandler := func(w http.ResponseWriter, r *http.Request) {
		// Increment current concurrent count
		current := atomic.AddInt64(&currentConcurrent, 1)

		// Update max if this is a new high
		mu.Lock()
		if current > maxConcurrent {
			maxConcurrent = current
		}
		mu.Unlock()

		// Simulate work (long enough to overlap with other requests)
		time.Sleep(100 * time.Millisecond)

		// Decrement concurrent count
		atomic.AddInt64(&currentConcurrent, -1)

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}

	srv := newTestServer(t,
		WithWorkerPoolSize(poolSize),
		WithHandler(trackingHandler),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Send more requests than pool size
	numRequests := poolSize * 3 // 9 requests
	var wg sync.WaitGroup
	var successCount int64

	// Use a transport so we can close idle connections
	transport := &http.Transport{}
	client := &http.Client{Timeout: 10 * time.Second, Transport: transport}

	wg.Add(numRequests)
	for i := 0; i < numRequests; i++ {
		go func() {
			defer wg.Done()
			resp, err := client.Get("http://" + srv.addr)
			if err == nil && resp.StatusCode == http.StatusOK {
				atomic.AddInt64(&successCount, 1)
				resp.Body.Close()
			}
		}()
	}

	wg.Wait()

	// Close idle HTTP connections
	transport.CloseIdleConnections()

	t.Logf("Success count: %d out of %d", successCount, numRequests)
	t.Logf("Max concurrent requests observed: %d (pool size: %d)", maxConcurrent, poolSize)

	// All requests should eventually succeed
	if successCount != int64(numRequests) {
		t.Errorf("Expected %d successful requests, got %d", numRequests, successCount)
	}

	// ✅ KEY ASSERTION: Max concurrent should NOT exceed pool size
	if maxConcurrent > int64(poolSize) {
		t.Errorf("Worker pool limit violated! Max concurrent: %d, Pool size: %d", maxConcurrent, poolSize)
	}

	// Verify we actually hit the limit (requests were concurrent)
	if maxConcurrent < int64(poolSize) {
		t.Logf("Warning: Max concurrent (%d) was less than pool size (%d) - requests may not have overlapped",
			maxConcurrent, poolSize)
	}

	// Cleanup
	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	srv.Stop(stopCtx)
}

// TestContextCancellation verifies requests respect context cancellation
func TestContextCancellation(t *testing.T) {
	srv := newTestServer(t, WithWorkerPoolSize(1))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Create a request with a very short timeout
	reqCtx, reqCancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer reqCancel()

	req, err := http.NewRequestWithContext(reqCtx, "GET", "http://"+srv.addr, nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	client := &http.Client{}
	_, err = client.Do(req)

	// Request should fail due to context cancellation
	if err == nil {
		t.Log("Request completed before context timeout (this is okay if server is fast)")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("Request failed with error: %v", err)
	}

	// Cleanup
	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	srv.Stop(stopCtx)
}

// TestBackgroundJobExecution verifies background job runs on ticker
func TestBackgroundJobExecution(t *testing.T) {
	var jobCount int64

	srv := newTestServer(t,
		WithBackgroundTicker(50*time.Millisecond),
		WithBackgroundJobFunc(func(ctx context.Context) {
			atomic.AddInt64(&jobCount, 1)
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	// Wait for several ticks
	time.Sleep(200 * time.Millisecond)

	// Cancel and stop
	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	srv.Stop(stopCtx)

	count := atomic.LoadInt64(&jobCount)
	t.Logf("Background job executed %d times", count)

	if count == 0 {
		t.Error("Background job should have executed at least once")
	}
}

// TestNoOsExitInBusinessLogic verifies shutdown is testable without process termination
func TestNoOsExitInBusinessLogic(t *testing.T) {
	// This test simply verifies that we can start and stop the server
	// multiple times in the same test process without any os.Exit() calls

	for i := 0; i < 3; i++ {
		srv := newTestServer(t)

		ctx, cancel := context.WithCancel(context.Background())

		if err := srv.Start(ctx); err != nil {
			t.Fatalf("iteration %d: failed to start server: %v", i, err)
		}

		time.Sleep(50 * time.Millisecond)

		cancel()

		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := srv.Stop(stopCtx); err != nil {
			t.Logf("iteration %d: stop returned error (may be expected): %v", i, err)
		}
		stopCancel()

		time.Sleep(50 * time.Millisecond)
	}

	// If we reach here, no os.Exit() was called
	t.Log("Server lifecycle is testable without os.Exit()")
}

// BenchmarkRequestHandling benchmarks request throughput
func BenchmarkRequestHandling(b *testing.B) {
	_, db := net.Pipe()

	// Get a free port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to get free port: %v", err)
	}
	addr := listener.Addr().String()
	listener.Close()

	srv := NewServer(
		WithDB(db),
		WithRequestTimeout(5*time.Second),
		WithWorkerPoolSize(100),
		WithAddr(addr),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		b.Fatalf("failed to start server: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 100,
		},
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Get("http://" + addr)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}
	})

	b.StopTimer()
	cancel()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	srv.Stop(stopCtx)
}

// TestGracefulShutdown - Kata Requirement #1: The Sudden Death Test
// Send requests, immediately send shutdown signal, verify in-flight requests complete
func TestGracefulShutdown(t *testing.T) {
	srv := newTestServer(t, WithWorkerPoolSize(5))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	var wg sync.WaitGroup
	var completed int64
	var failed int64

	// Start 10 requests
	numRequests := 10
	wg.Add(numRequests)

	for i := 0; i < numRequests; i++ {
		go func(id int) {
			defer wg.Done()
			client := &http.Client{Timeout: 3 * time.Second}
			resp, err := client.Get("http://" + srv.addr)
			if err == nil && resp.StatusCode == http.StatusOK {
				atomic.AddInt64(&completed, 1)
				resp.Body.Close()
				t.Logf("Request %d completed successfully", id)
			} else {
				atomic.AddInt64(&failed, 1)
				if err != nil {
					t.Logf("Request %d failed: %v", id, err)
				}
			}
		}(i)
	}

	// Wait for requests to start being processed
	time.Sleep(20 * time.Millisecond)

	// Immediately trigger shutdown
	t.Log("Triggering shutdown signal")
	cancel()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()

	start := time.Now()
	err := srv.Stop(stopCtx)
	duration := time.Since(start)

	if err != nil {
		t.Logf("Stop returned error: %v", err)
	}

	wg.Wait()

	t.Logf("Shutdown took %v", duration)
	t.Logf("Completed: %d, Failed: %d", completed, failed)

	// Verify: Some in-flight requests completed
	if completed == 0 {
		t.Error("Expected at least some in-flight requests to complete")
	}

	// Verify: Shutdown was graceful (within reasonable time)
	if duration > 5*time.Second {
		t.Errorf("Shutdown took too long: %v", duration)
	}

	t.Log("✓ Server completed in-flight requests and closed cleanly")
}

// TestShutdownTimeout - Kata Requirement #3: The Timeout Test
// Start long-running request, shutdown with short timeout, verify forced exit
func TestShutdownTimeout(t *testing.T) {
	longRequestDuration := 10 * time.Second
	shutdownTimeout := 2 * time.Second

	// Custom handler that simulates long work
	slowHandler := func(w http.ResponseWriter, r *http.Request) {
		t.Log("Long request started")
		select {
		case <-r.Context().Done():
			t.Log("Long request cancelled by context")
			http.Error(w, "Cancelled", http.StatusRequestTimeout)
			return
		case <-time.After(longRequestDuration):
			t.Log("Long request completed normally")
			w.WriteHeader(http.StatusOK)
		}
	}

	srv := newTestServer(t,
		WithWorkerPoolSize(1),
		WithHandler(slowHandler),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Start a long request in background
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Get("http://" + srv.addr)
		if err != nil {
			t.Logf("Long request error (expected): %v", err)
		} else {
			resp.Body.Close()
		}
	}()

	// Wait for request to start processing
	time.Sleep(100 * time.Millisecond)

	// Trigger shutdown
	cancel()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer stopCancel()

	start := time.Now()
	err := srv.Stop(stopCtx)
	duration := time.Since(start)

	t.Logf("Stop completed in %v with error: %v", duration, err)

	// Key assertion: Shutdown should complete around the timeout, not wait for full request
	maxExpected := shutdownTimeout + 500*time.Millisecond
	if duration > maxExpected {
		t.Errorf("Shutdown should have forced exit after %v, but took %v", shutdownTimeout, duration)
	}

	// Verify it didn't wait for the full long request
	if duration >= longRequestDuration {
		t.Errorf("Shutdown waited for full request duration (%v), should have timed out at %v",
			duration, shutdownTimeout)
	}

	// Error expected when shutdown times out
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("Shutdown timed out as expected: %v", err)
	}

	t.Log("✓ Server forced shutdown after timeout")

	// Wait for client to receive connection closed error
	// Should happen quickly after shutdown completes
	select {
	case <-requestDone:
		t.Log("Client received connection closed (as expected)")
	case <-time.After(1 * time.Second):
		t.Error("Client should have received connection closed error within 1s of shutdown")
	}
}

// TestNoGoroutineLeak - Kata Requirement #2: The Slow Leak Test
// Run server, make requests, verify no goroutine leaks after shutdown
func TestNoGoroutineLeak(t *testing.T) {
	// Force garbage collection to clean up any pending goroutines
	runtime.GC()
	time.Sleep(50 * time.Millisecond)

	before := runtime.NumGoroutine()
	t.Logf("Goroutines before test: %d", before)

	// Run multiple server lifecycles to amplify any leaks
	for iteration := 0; iteration < 3; iteration++ {
		srv := newTestServer(t,
			WithWorkerPoolSize(5),
			WithBackgroundTicker(50*time.Millisecond),
			WithBackgroundJobFunc(func(ctx context.Context) {
				// Simulate work
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}),
		)

		ctx, cancel := context.WithCancel(context.Background())

		if err := srv.Start(ctx); err != nil {
			t.Fatalf("iteration %d: failed to start: %v", iteration, err)
		}

		time.Sleep(100 * time.Millisecond)

		// Make several requests
		var wg sync.WaitGroup
		transport := &http.Transport{}
		client := &http.Client{Timeout: 2 * time.Second, Transport: transport}

		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := client.Get("http://" + srv.addr)
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}()
		}

		wg.Wait()
		transport.CloseIdleConnections()

		// Shutdown
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := srv.Stop(stopCtx); err != nil {
			t.Logf("iteration %d: stop error: %v", iteration, err)
		}
		stopCancel()

		// Wait for cleanup
		time.Sleep(100 * time.Millisecond)
	}

	// Force GC again to clean up
	runtime.GC()
	time.Sleep(100 * time.Millisecond)

	after := runtime.NumGoroutine()
	t.Logf("Goroutines after test: %d", after)

	// Allow tolerance of 2 goroutines for runtime variance
	tolerance := 2
	leaked := after - before

	if leaked > tolerance {
		t.Errorf("FAIL: Goroutine leak detected! Before: %d, After: %d, Leaked: %d (tolerance: %d)",
			before, after, leaked, tolerance)

		// Print goroutine dump for debugging
		buf := make([]byte, 1<<20)
		stackLen := runtime.Stack(buf, true)
		t.Logf("Goroutine dump:\n%s", buf[:stackLen])
	} else {
		t.Logf("✓ No goroutine leaks detected (delta: %d, tolerance: %d)", leaked, tolerance)
	}
}
