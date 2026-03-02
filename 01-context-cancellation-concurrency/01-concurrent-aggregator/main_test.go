package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

// Mock service functions for testing
type ProfileFetcher func(ctx context.Context, id int) (User, error)
type OrderFetcher func(ctx context.Context, id int) (Order, error)

// TestableUserAggregator extends UserAggregator with injectable dependencies
type TestableUserAggregator struct {
	timeout        time.Duration
	logger         *slog.Logger
	profileFetcher ProfileFetcher
	orderFetcher   OrderFetcher
}

func NewTestableUserAggregator(profileFetcher ProfileFetcher, orderFetcher OrderFetcher, opts ...Option) *TestableUserAggregator {
	ua := &TestableUserAggregator{
		profileFetcher: profileFetcher,
		orderFetcher:   orderFetcher,
		timeout:        5 * time.Second, // default timeout
		logger:         slog.New(slog.NewTextHandler(os.Stdout, nil)),
	}
	for _, opt := range opts {
		if opt != nil {
			// Apply options to embedded fields
			temp := &UserAggregator{timeout: ua.timeout, logger: ua.logger}
			opt(temp)
			ua.timeout = temp.timeout
			ua.logger = temp.logger
		}
	}
	return ua
}

func (u *TestableUserAggregator) Aggregate(ctx context.Context, id int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, u.timeout)
	defer cancel()

	pool, childCtx := errgroup.WithContext(ctx)

	var (
		user  User
		order Order
	)

	u.logger.Info("Starting aggregation", "id", id)

	pool.Go(func() error {
		var err error
		user, err = u.profileFetcher(childCtx, id)
		if err != nil {
			return fmt.Errorf("profile fetch failed: %w", err)
		}
		return nil
	})

	pool.Go(func() error {
		var err error
		order, err = u.orderFetcher(childCtx, id)
		if err != nil {
			return fmt.Errorf("order fetch failed: %w", err)
		}
		return nil
	})

	if err := pool.Wait(); err != nil {
		u.logger.Error("failed to wait for aggregator process", "error", err)
		return "", err
	}

	result := fmt.Sprintf("User: %s | Orders: %d", user.Name, order.Orders)
	u.logger.Info("aggregator process finished", "id", id, "result", result)
	return result, nil
}

// Test Cases

func TestAggregate_Success(t *testing.T) {
	profileFetcher := func(ctx context.Context, id int) (User, error) {
		return User{Name: "Alice"}, nil
	}
	orderFetcher := func(ctx context.Context, id int) (Order, error) {
		return Order{Orders: 5}, nil
	}

	aggregator := NewTestableUserAggregator(
		profileFetcher,
		orderFetcher,
		WithTimeout(2*time.Second),
		WithLogger(slog.New(slog.NewTextHandler(os.Stdout, nil))),
	)

	result, err := aggregator.Aggregate(context.Background(), 123)

	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	expected := "User: Alice | Orders: 5"
	if result != expected {
		t.Errorf("Expected result %q, got %q", expected, result)
	}
}

func TestAggregate_ProfileServiceError(t *testing.T) {
	profileFetcher := func(ctx context.Context, id int) (User, error) {
		return User{}, errors.New("profile service down")
	}
	orderFetcher := func(ctx context.Context, id int) (Order, error) {
		time.Sleep(100 * time.Millisecond) // simulate slow service
		return Order{Orders: 5}, nil
	}

	aggregator := NewTestableUserAggregator(
		profileFetcher,
		orderFetcher,
		WithTimeout(5*time.Second),
		WithLogger(slog.New(slog.NewTextHandler(os.Stdout, nil))),
	)

	_, err := aggregator.Aggregate(context.Background(), 123)

	if err == nil {
		t.Fatal("Expected error, got nil")
	}

	if !strings.Contains(err.Error(), "profile fetch failed") {
		t.Errorf("Expected error to contain 'profile fetch failed', got: %v", err)
	}
}

func TestAggregate_OrderServiceError(t *testing.T) {
	profileFetcher := func(ctx context.Context, id int) (User, error) {
		return User{Name: "Bob"}, nil
	}
	orderFetcher := func(ctx context.Context, id int) (Order, error) {
		return Order{}, errors.New("order service unavailable")
	}

	aggregator := NewTestableUserAggregator(
		profileFetcher,
		orderFetcher,
		WithTimeout(2*time.Second),
		WithLogger(slog.New(slog.NewTextHandler(os.Stdout, nil))),
	)

	_, err := aggregator.Aggregate(context.Background(), 123)

	if err == nil {
		t.Fatal("Expected error, got nil")
	}

	if !strings.Contains(err.Error(), "order fetch failed") {
		t.Errorf("Expected error to contain 'order fetch failed', got: %v", err)
	}
}

func TestAggregate_TimeoutSlowService(t *testing.T) {
	// The "Slow Poke" test case from the kata
	profileFetcher := func(ctx context.Context, id int) (User, error) {
		select {
		case <-ctx.Done():
			return User{}, ctx.Err()
		case <-time.After(2 * time.Second): // Slower than timeout
			return User{Name: "Alice"}, nil
		}
	}
	orderFetcher := func(ctx context.Context, id int) (Order, error) {
		return Order{Orders: 5}, nil
	}

	aggregator := NewTestableUserAggregator(
		profileFetcher,
		orderFetcher,
		WithTimeout(1*time.Second), // Timeout is 1s, but service takes 2s
		WithLogger(slog.New(slog.NewTextHandler(os.Stdout, nil))),
	)

	start := time.Now()
	_, err := aggregator.Aggregate(context.Background(), 123)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Expected timeout error, got nil")
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Expected context.DeadlineExceeded, got: %v", err)
	}

	// Should return after ~1s, not 2s
	if elapsed > 1500*time.Millisecond {
		t.Errorf("Expected to timeout after ~1s, took %v", elapsed)
	}
}

func TestAggregate_DominoEffect(t *testing.T) {
	// The "Domino Effect" test case from the kata
	profileFetcher := func(ctx context.Context, id int) (User, error) {
		// Fail immediately
		return User{}, errors.New("profile service error")
	}

	orderCancelled := false
	orderFetcher := func(ctx context.Context, id int) (Order, error) {
		select {
		case <-ctx.Done():
			// Context should be cancelled by errgroup
			orderCancelled = true
			return Order{}, ctx.Err()
		case <-time.After(10 * time.Second):
			// Should never reach here
			return Order{Orders: 5}, nil
		}
	}

	aggregator := NewTestableUserAggregator(
		profileFetcher,
		orderFetcher,
		WithTimeout(30*time.Second), // Long timeout, shouldn't reach
		WithLogger(slog.New(slog.NewTextHandler(os.Stdout, nil))),
	)

	start := time.Now()
	_, err := aggregator.Aggregate(context.Background(), 123)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Expected error, got nil")
	}

	// Should return immediately, not wait 10s
	if elapsed > 1*time.Second {
		t.Errorf("Expected to fail immediately, took %v", elapsed)
	}

	if !strings.Contains(err.Error(), "profile fetch failed") {
		t.Errorf("Expected profile error, got: %v", err)
	}

	// Give a small grace period for context cancellation to propagate
	time.Sleep(100 * time.Millisecond)

	if !orderCancelled {
		t.Error("Expected order service to be cancelled via context, but it wasn't")
	}
}

func TestAggregate_BothServicesError(t *testing.T) {
	profileFetcher := func(ctx context.Context, id int) (User, error) {
		return User{}, errors.New("profile error")
	}
	orderFetcher := func(ctx context.Context, id int) (Order, error) {
		return Order{}, errors.New("order error")
	}

	aggregator := NewTestableUserAggregator(
		profileFetcher,
		orderFetcher,
		WithTimeout(2*time.Second),
		WithLogger(slog.New(slog.NewTextHandler(os.Stdout, nil))),
	)

	_, err := aggregator.Aggregate(context.Background(), 123)

	if err == nil {
		t.Fatal("Expected error, got nil")
	}

	// errgroup returns the first error encountered
	// Could be either profile or order error
	hasError := strings.Contains(err.Error(), "profile fetch failed") ||
		strings.Contains(err.Error(), "order fetch failed")
	if !hasError {
		t.Errorf("Expected fetch error, got: %v", err)
	}
}

func TestAggregate_ParentContextCancellation(t *testing.T) {
	profileFetcher := func(ctx context.Context, id int) (User, error) {
		select {
		case <-ctx.Done():
			return User{}, ctx.Err()
		case <-time.After(5 * time.Second):
			return User{Name: "Alice"}, nil
		}
	}
	orderFetcher := func(ctx context.Context, id int) (Order, error) {
		select {
		case <-ctx.Done():
			return Order{}, ctx.Err()
		case <-time.After(5 * time.Second):
			return Order{Orders: 5}, nil
		}
	}

	aggregator := NewTestableUserAggregator(
		profileFetcher,
		orderFetcher,
		WithTimeout(10*time.Second),
		WithLogger(slog.New(slog.NewTextHandler(os.Stdout, nil))),
	)

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel context after 100ms
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := aggregator.Aggregate(ctx, 123)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Expected error from context cancellation, got nil")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Expected context.Canceled error, got: %v", err)
	}

	// Should complete quickly after cancellation, not wait for full timeout
	if elapsed > 500*time.Millisecond {
		t.Errorf("Expected quick cancellation, took %v", elapsed)
	}
}

func TestFunctionalOptions_CustomTimeout(t *testing.T) {
	profileFetcher := func(ctx context.Context, id int) (User, error) {
		return User{Name: "Alice"}, nil
	}
	orderFetcher := func(ctx context.Context, id int) (Order, error) {
		return Order{Orders: 5}, nil
	}

	customTimeout := 3 * time.Second
	aggregator := NewTestableUserAggregator(
		profileFetcher,
		orderFetcher,
		WithTimeout(customTimeout),
	)

	if aggregator.timeout != customTimeout {
		t.Errorf("Expected timeout %v, got %v", customTimeout, aggregator.timeout)
	}
}

func TestFunctionalOptions_CustomLogger(t *testing.T) {
	profileFetcher := func(ctx context.Context, id int) (User, error) {
		return User{Name: "Alice"}, nil
	}
	orderFetcher := func(ctx context.Context, id int) (Order, error) {
		return Order{Orders: 5}, nil
	}

	customLogger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	aggregator := NewTestableUserAggregator(
		profileFetcher,
		orderFetcher,
		WithLogger(customLogger),
	)

	if aggregator.logger != customLogger {
		t.Error("Expected custom logger to be set")
	}
}

func TestFunctionalOptions_MultipleOptions(t *testing.T) {
	profileFetcher := func(ctx context.Context, id int) (User, error) {
		return User{Name: "Alice"}, nil
	}
	orderFetcher := func(ctx context.Context, id int) (Order, error) {
		return Order{Orders: 5}, nil
	}

	customTimeout := 7 * time.Second
	customLogger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	aggregator := NewTestableUserAggregator(
		profileFetcher,
		orderFetcher,
		WithTimeout(customTimeout),
		WithLogger(customLogger),
	)

	if aggregator.timeout != customTimeout {
		t.Errorf("Expected timeout %v, got %v", customTimeout, aggregator.timeout)
	}

	if aggregator.logger != customLogger {
		t.Error("Expected custom logger to be set")
	}
}

// Benchmark tests
func BenchmarkAggregate_Success(b *testing.B) {
	profileFetcher := func(ctx context.Context, id int) (User, error) {
		return User{Name: "Alice"}, nil
	}
	orderFetcher := func(ctx context.Context, id int) (Order, error) {
		return Order{Orders: 5}, nil
	}

	aggregator := NewTestableUserAggregator(
		profileFetcher,
		orderFetcher,
		WithTimeout(2*time.Second),
		WithLogger(slog.New(slog.NewTextHandler(os.Stdout, nil))),
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = aggregator.Aggregate(context.Background(), i)
	}
}
