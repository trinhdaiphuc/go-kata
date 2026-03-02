package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type Server struct {
	Options
	workerPool       chan struct{}
	server           *http.Server
	backgroundTicker *time.Ticker
}

type Options struct {
	addr                     string
	requestTimeout           time.Duration
	workerPoolSize           int
	backgroundTickerInterval time.Duration
	backgroundJobFunc        func(context.Context)
	handler                  http.HandlerFunc
	db                       net.Conn
}

func main() {
	_, db := net.Pipe()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	srv := NewServer(
		WithDB(db),
		WithRequestTimeout(time.Second),
		WithWorkerPoolSize(10),
		WithAddr("localhost:8080"),
		WithBackgroundTicker(30*time.Second),
		WithBackgroundJobFunc(func(ctx context.Context) {
			slog.Info("Warming up the cache...")
			select {
			case <-ctx.Done():
				slog.Info("Cache warm cancelled")
				return
			case <-time.After(time.Second):
				slog.Info("Cache warmed successfully")
			}
		}),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	err := srv.Start(ctx)
	if err != nil {
		return
	}

	<-ctx.Done()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	err = srv.Stop(stopCtx)
	if err != nil {
		slog.Error("server stop error", "error", err)
	}
}

func NewServer(options ...ServerOption) *Server {
	opts := &Options{}
	for _, option := range options {
		option(opts)
	}

	return &Server{
		Options:    *opts,
		workerPool: make(chan struct{}, opts.workerPoolSize),
	}
}

func defaultOptions() Options {
	return Options{
		addr:           "localhost:8080",
		workerPoolSize: 10,
	}
}

func (s *Server) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	handler := s.HandleRequest
	if s.handler != nil {
		handler = s.handler
	}
	// Wrap handler with worker pool limiting
	mux.HandleFunc("/", s.withWorkerPool(handler))

	s.server = &http.Server{
		Handler:      mux,
		ReadTimeout:  s.requestTimeout,
		WriteTimeout: s.requestTimeout,
		IdleTimeout:  s.requestTimeout,
	}

	go func() {
		slog.Info("server started", "address", s.server.Addr)
		if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("failed to serve", "error", err)
		}
	}()

	if s.backgroundTickerInterval != 0 && s.backgroundJobFunc != nil {
		s.backgroundTicker = time.NewTicker(s.backgroundTickerInterval)
		go func() {
			slog.Info("background job func started")
			for {
				select {
				case <-ctx.Done():
					slog.Info("background job func stopped")
					return
				case <-s.backgroundTicker.C:
					s.backgroundJobFunc(ctx)
				}
			}
		}()
	}

	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	var errs []error

	// 1. Stop accepting new connections and wait for in-flight requests
	if s.server != nil {
		slog.Info("shutting down HTTP server")
		if err := s.server.Shutdown(ctx); err != nil {
			slog.Error("server shutdown error", "error", err)
			errs = append(errs, err)
			
			// If shutdown timed out, force close all connections
			if errors.Is(err, context.DeadlineExceeded) {
				slog.Warn("shutdown timeout exceeded, forcefully closing server")
				if closeErr := s.server.Close(); closeErr != nil {
					slog.Error("server close error", "error", closeErr)
					errs = append(errs, closeErr)
				}
			}
		}
	}

	// 2. Stop background ticker
	if s.backgroundTicker != nil {
		slog.Info("stopping background ticker")
		s.backgroundTicker.Stop()
	}

	// 3. Close worker pool channel (no need to drain - server.Shutdown already waited)
	if s.workerPool != nil {
		close(s.workerPool)
		slog.Info("worker pool closed")
	}

	// 4. Close database connection
	if s.db != nil {
		slog.Info("closing database connection")
		if err := s.db.Close(); err != nil {
			slog.Error("database close error", "error", err)
			errs = append(errs, err)
		}
	}

	slog.Info("shutdown complete")
	return errors.Join(errs...)
}

type ServerOption func(*Options)

func WithRequestTimeout(timeout time.Duration) ServerOption {
	return func(s *Options) {
		s.requestTimeout = timeout
	}
}

func WithAddr(addr string) ServerOption {
	return func(s *Options) {
		s.addr = addr
	}
}

func WithWorkerPoolSize(size int) ServerOption {
	return func(s *Options) {
		s.workerPoolSize = size
	}
}

func WithBackgroundTicker(tickerInterval time.Duration) ServerOption {
	return func(s *Options) {
		s.backgroundTickerInterval = tickerInterval
	}
}

func WithBackgroundJobFunc(f func(context.Context)) ServerOption {
	return func(s *Options) {
		s.backgroundJobFunc = f
	}
}

func WithDB(db net.Conn) ServerOption {
	return func(s *Options) {
		s.db = db
	}
}

func WithHandler(h http.HandlerFunc) ServerOption {
	return func(s *Options) {
		s.handler = h
	}
}

// withWorkerPool wraps a handler with worker pool limiting middleware
func (s *Server) withWorkerPool(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Acquire worker slot (blocks if pool is full)
		if s.workerPool != nil {
			select {
			case s.workerPool <- struct{}{}:
				defer func() {
					<-s.workerPool
				}()
			case <-r.Context().Done():
				http.Error(w, "Request cancelled while waiting for worker", http.StatusServiceUnavailable)
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) HandleRequest(w http.ResponseWriter, r *http.Request) {
	// Simulate some work
	select {
	case <-r.Context().Done():
		http.Error(w, "Request cancelled", http.StatusRequestTimeout)
		return
	case <-time.After(100 * time.Millisecond):
		slog.Info("request handled", "request", r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Hello, World!"))
	}
}
