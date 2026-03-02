package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sync/errgroup"
)

type UserAggregator struct {
	timeout time.Duration
	logger  *slog.Logger
}

type Option func(*UserAggregator)

func WithTimeout(timeout time.Duration) Option {
	return func(u *UserAggregator) {
		u.timeout = timeout
	}
}

func WithLogger(logger *slog.Logger) Option {
	return func(u *UserAggregator) {
		u.logger = logger
	}
}

func NewUserAggregator(opts ...Option) *UserAggregator {
	ua := &UserAggregator{}
	for _, opt := range opts {
		opt(ua)
	}
	return ua
}

func (u UserAggregator) Aggregate(ctx context.Context, id int) {
	ctx, cancel := context.WithTimeout(ctx, u.timeout)
	defer cancel()

	pool, childCtx := errgroup.WithContext(ctx)

	var (
		user  User
		order Order
	)

	u.logger.Info("Starting aggregation")

	pool.Go(func() error {
		var err error
		user, err = FetchProfile(childCtx, id)
		if err != nil {
			return fmt.Errorf("profile fetch failed: %w", err)
		}
		return nil
	})

	pool.Go(func() error {
		var err error
		order, err = FetchOrder(childCtx, id)
		if err != nil {
			return fmt.Errorf("order fetch failed: %w", err)
		}
		return nil
	})

	if err := pool.Wait(); err != nil {
		u.logger.Error("failed to wait for aggregator process", "error", err)
		return
	}

	u.logger.Info("aggregator process finished", "id", id, "profile", user, "order", order)
}

type User struct {
	Name string
}

func FetchProfile(ctx context.Context, id int) (User, error) {
	select {
	case <-ctx.Done():
		return User{}, ctx.Err()
	case <-time.After(10 * time.Second):
		return User{
			Name: "Alice",
		}, nil
	}
}

type Order struct {
	Orders int
}

func FetchOrder(ctx context.Context, id int) (Order, error) {
	select {
	case <-ctx.Done():
		return Order{}, ctx.Err()
	case <-time.After(100 * time.Millisecond):
		// return Order{
		// 	Orders: 5,
		// }, nil
		return Order{}, errors.New("internal error")
	}
}

func main() {
	userAggregator := NewUserAggregator(
		WithLogger(slog.New(slog.NewTextHandler(os.Stdout, nil))),
		WithTimeout(time.Second),
	)

	userAggregator.Aggregate(context.Background(), 10)

}
