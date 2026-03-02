package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Mock implementations with injectable behavior

type MockAuthService struct {
	authenticateFn func(ctx context.Context, token string) (string, error)
}

func (m MockAuthService) Authenticate(ctx context.Context, token string) (string, error) {
	if m.authenticateFn != nil {
		return m.authenticateFn(ctx, token)
	}
	return "userID123", nil
}

type MockMetadataService struct {
	fetchMetadataFn func(ctx context.Context, userID string) (map[string]string, error)
}

func (m MockMetadataService) FetchMetadata(ctx context.Context, userID string) (map[string]string, error) {
	if m.fetchMetadataFn != nil {
		return m.fetchMetadataFn(ctx, userID)
	}
	return map[string]string{"role": "admin"}, nil
}

type MockStorageService struct {
	storeDataFn func(ctx context.Context, userID string, data map[string]string) error
}

func (m MockStorageService) StoreData(ctx context.Context, userID string, data map[string]string) error {
	if m.storeDataFn != nil {
		return m.storeDataFn(ctx, userID, data)
	}
	return nil
}

// Custom error types with additional context

type SensitiveAuthError struct {
	Message   string
	APIKey    string
	Err       error
	timestamp time.Time
}

func (e *SensitiveAuthError) Error() string {
	// Redact sensitive information when converting to string
	return fmt.Sprintf("%s (key: [REDACTED])", e.Message)
}

func (e *SensitiveAuthError) Unwrap() error {
	return e.Err
}

func (e *SensitiveAuthError) Timeout() bool {
	return errors.Is(e.Err, context.DeadlineExceeded)
}

func (e *SensitiveAuthError) Temporary() bool {
	return e.Timeout()
}

type StorageQuotaError struct {
	Message       string
	QuotaExceeded int64
	CurrentUsage  int64
	Err           error
}

func (e *StorageQuotaError) Error() string {
	return fmt.Sprintf("%s: quota exceeded by %d bytes (current: %d)",
		e.Message, e.QuotaExceeded, e.CurrentUsage)
}

func (e *StorageQuotaError) Unwrap() error {
	return e.Err
}

type TimeoutError struct {
	Message string
	Err     error
}

func (e *TimeoutError) Error() string {
	return e.Message
}

func (e *TimeoutError) Unwrap() error {
	return e.Err
}

func (e *TimeoutError) Timeout() bool {
	return true
}

func (e *TimeoutError) Temporary() bool {
	return true
}

type DatabaseDeadlockError struct {
	Message string
	Err     error
}

func (e *DatabaseDeadlockError) Error() string {
	return e.Message
}

func (e *DatabaseDeadlockError) Unwrap() error {
	return e.Err
}

func (e *DatabaseDeadlockError) Temporary() bool {
	return true
}

// Test Cases

func TestUploadFile_Success(t *testing.T) {
	gateway := NewCloudStorageGateway(
		MockAuthService{},
		MockMetadataService{},
		MockStorageService{},
	)

	err := gateway.UploadFile(context.Background(), []byte("test data"))
	if err != nil {
		t.Errorf("Expected no error, got: %v", err)
	}
}

func TestUploadFile_AuthenticationFailure(t *testing.T) {
	authService := MockAuthService{
		authenticateFn: func(ctx context.Context, token string) (string, error) {
			return "", &AuthError{
				Message: "invalid credentials",
				Err:     errors.New("auth failed"),
			}
		},
	}

	gateway := NewCloudStorageGateway(
		authService,
		MockMetadataService{},
		MockStorageService{},
	)

	err := gateway.UploadFile(context.Background(), []byte("test data"))
	if err == nil {
		t.Fatal("Expected authentication error, got nil")
	}

	if !strings.Contains(err.Error(), "authentication failed") {
		t.Errorf("Expected error to contain 'authentication failed', got: %v", err)
	}

	// Test error unwrapping
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Error("Expected to unwrap AuthError using errors.As")
	}
}

func TestUploadFile_MetadataFetchFailure(t *testing.T) {
	metadataService := MockMetadataService{
		fetchMetadataFn: func(ctx context.Context, userID string) (map[string]string, error) {
			return nil, &MetadataError{
				Message: "database connection failed",
				Err:     errors.New("connection refused"),
			}
		},
	}

	gateway := NewCloudStorageGateway(
		MockAuthService{},
		metadataService,
		MockStorageService{},
	)

	err := gateway.UploadFile(context.Background(), []byte("test data"))
	if err == nil {
		t.Fatal("Expected metadata error, got nil")
	}

	if !strings.Contains(err.Error(), "metadata fetch failed") {
		t.Errorf("Expected error to contain 'metadata fetch failed', got: %v", err)
	}

	var metadataErr *MetadataError
	if !errors.As(err, &metadataErr) {
		t.Error("Expected to unwrap MetadataError using errors.As")
	}
}

func TestUploadFile_StorageFailure(t *testing.T) {
	storageService := MockStorageService{
		storeDataFn: func(ctx context.Context, userID string, data map[string]string) error {
			return &StorageError{
				Message: "storage quota exceeded",
				Err:     errors.New("quota limit reached"),
			}
		},
	}

	gateway := NewCloudStorageGateway(
		MockAuthService{},
		MockMetadataService{},
		storageService,
	)

	err := gateway.UploadFile(context.Background(), []byte("test data"))
	if err == nil {
		t.Fatal("Expected storage error, got nil")
	}

	if !strings.Contains(err.Error(), "data storage failed") {
		t.Errorf("Expected error to contain 'data storage failed', got: %v", err)
	}

	var storageErr *StorageError
	if !errors.As(err, &storageErr) {
		t.Error("Expected to unwrap StorageError using errors.As")
	}
}

// Kata Requirement Tests

func TestSensitiveDataLeak_RedactionInError(t *testing.T) {
	// The "Sensitive Data Leak" test case from the kata
	sensitiveAPIKey := "sk-1234567890abcdef"

	authErr := &SensitiveAuthError{
		Message:   "authentication failed",
		APIKey:    sensitiveAPIKey,
		Err:       errors.New("invalid token"),
		timestamp: time.Now(),
	}

	errorString := fmt.Sprint(authErr)

	// FAIL CONDITION: If fmt.Sprint(err) contains the API key string
	if strings.Contains(errorString, sensitiveAPIKey) {
		t.Errorf("ERROR: Sensitive API key leaked in error message: %s", errorString)
	}

	// Verify redaction works
	if !strings.Contains(errorString, "[REDACTED]") {
		t.Error("Expected error to contain [REDACTED] placeholder")
	}
}

func TestLostContext_ErrorUnwrapping(t *testing.T) {
	// The "Lost Context" test case from the kata
	// Wrap an AuthError three times through different layers

	originalErr := &AuthError{
		Message: "token expired",
		Err:     errors.New("expired at 2026-01-21"),
	}

	// Layer 1: Wrap in authentication context
	layer1Err := fmt.Errorf("authentication layer error: %w", originalErr)

	// Layer 2: Wrap in gateway context
	layer2Err := fmt.Errorf("gateway processing error: %w", layer1Err)

	// Layer 3: Wrap in API context
	layer3Err := fmt.Errorf("API handler error: %w", layer2Err)

	// FAIL CONDITION: If errors.As(err, &AuthError{}) returns false
	var authErr *AuthError
	if !errors.As(layer3Err, &authErr) {
		t.Fatal("ERROR: Lost context - cannot unwrap AuthError after 3 layers of wrapping")
	}

	// Verify we got the original error
	if authErr.Message != "token expired" {
		t.Errorf("Expected original message 'token expired', got: %s", authErr.Message)
	}
}

func TestTimeoutConfusion_ContextAwareErrors(t *testing.T) {
	// The "Timeout Confusion" test case from the kata
	// Create a timeout error in the storage layer

	timeoutErr := &TimeoutError{
		Message: "storage operation timed out",
		Err:     context.DeadlineExceeded,
	}

	storageErr := &StorageError{
		Message: "failed to store data",
		Err:     timeoutErr,
	}

	wrappedErr := fmt.Errorf("upload failed: %w", storageErr)

	// FAIL CONDITION: If errors.Is(err, context.DeadlineExceeded) returns false
	if !errors.Is(wrappedErr, context.DeadlineExceeded) {
		t.Fatal("ERROR: Timeout confusion - cannot detect context.DeadlineExceeded through error chain")
	}

	// Verify Timeout() method works
	var timeoutErrCheck *TimeoutError
	if errors.As(wrappedErr, &timeoutErrCheck) {
		if !timeoutErrCheck.Timeout() {
			t.Error("Expected Timeout() to return true")
		}
		if !timeoutErrCheck.Temporary() {
			t.Error("Expected Temporary() to return true for timeout errors")
		}
	}
}

func TestStorageQuotaError_DetailedInformation(t *testing.T) {
	quotaErr := &StorageQuotaError{
		Message:       "storage quota exceeded",
		QuotaExceeded: 1024 * 1024 * 10,  // 10MB
		CurrentUsage:  1024 * 1024 * 110, // 110MB
		Err:           errors.New("quota limit reached"),
	}

	errorMsg := quotaErr.Error()

	if !strings.Contains(errorMsg, "quota exceeded") {
		t.Error("Expected error message to contain 'quota exceeded'")
	}

	if !strings.Contains(errorMsg, "10485760") { // 10MB in bytes
		t.Error("Expected error message to contain quota exceeded amount")
	}
}

func TestDatabaseDeadlock_TemporaryError(t *testing.T) {
	deadlockErr := &DatabaseDeadlockError{
		Message: "database deadlock detected",
		Err:     errors.New("deadlock on table users"),
	}

	if !deadlockErr.Temporary() {
		t.Error("Expected database deadlock to be a temporary error")
	}

	metadataErr := &MetadataError{
		Message: "failed to fetch metadata",
		Err:     deadlockErr,
	}

	wrappedErr := fmt.Errorf("operation failed: %w", metadataErr)

	var dbErr *DatabaseDeadlockError
	if !errors.As(wrappedErr, &dbErr) {
		t.Error("Expected to unwrap DatabaseDeadlockError")
	}
}

func TestErrorChain_MultipleLayerWrapping(t *testing.T) {
	// Test error wrapping through multiple service layers
	baseErr := errors.New("network connection refused")

	authErr := &AuthError{
		Message: "auth service unreachable",
		Err:     baseErr,
	}

	authService := MockAuthService{
		authenticateFn: func(ctx context.Context, token string) (string, error) {
			return "", authErr
		},
	}

	gateway := NewCloudStorageGateway(
		authService,
		MockMetadataService{},
		MockStorageService{},
	)

	err := gateway.UploadFile(context.Background(), []byte("test data"))

	// Verify error chain is intact
	if !errors.Is(err, baseErr) {
		t.Error("Expected to find base error in chain")
	}

	var authErrUnwrapped *AuthError
	if !errors.As(err, &authErrUnwrapped) {
		t.Error("Expected to unwrap AuthError")
	}

	if authErrUnwrapped.Message != "auth service unreachable" {
		t.Errorf("Expected message 'auth service unreachable', got: %s", authErrUnwrapped.Message)
	}
}

func TestContextCancellation_ErrorPropagation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	authService := MockAuthService{
		authenticateFn: func(ctx context.Context, token string) (string, error) {
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("auth operation cancelled: %w", ctx.Err())
			default:
				return "userID123", nil
			}
		},
	}

	gateway := NewCloudStorageGateway(
		authService,
		MockMetadataService{},
		MockStorageService{},
	)

	err := gateway.UploadFile(ctx, []byte("test data"))

	if err == nil {
		t.Fatal("Expected error due to context cancellation")
	}

	if !errors.Is(err, context.Canceled) {
		t.Error("Expected to detect context.Canceled in error chain")
	}
}

func TestContextDeadline_TimeoutDetection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	metadataService := MockMetadataService{
		fetchMetadataFn: func(ctx context.Context, userID string) (map[string]string, error) {
			select {
			case <-ctx.Done():
				return nil, &TimeoutError{
					Message: "metadata fetch timed out",
					Err:     ctx.Err(),
				}
			case <-time.After(100 * time.Millisecond):
				return map[string]string{"role": "admin"}, nil
			}
		},
	}

	gateway := NewCloudStorageGateway(
		MockAuthService{},
		metadataService,
		MockStorageService{},
	)

	err := gateway.UploadFile(ctx, []byte("test data"))

	if err == nil {
		t.Fatal("Expected timeout error")
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("Expected to detect context.DeadlineExceeded in error chain")
	}

	var timeoutErr *TimeoutError
	if errors.As(err, &timeoutErr) {
		if !timeoutErr.Timeout() {
			t.Error("Expected Timeout() method to return true")
		}
	}
}

func TestErrorWrapping_PreservesOriginalError(t *testing.T) {
	originalErr := errors.New("original low-level error")

	storageErr := &StorageError{
		Message: "storage layer error",
		Err:     originalErr,
	}

	wrappedErr := fmt.Errorf("high-level operation failed: %w", storageErr)

	// Verify we can still find the original error
	if !errors.Is(wrappedErr, originalErr) {
		t.Error("Expected to find original error in wrapped error chain")
	}

	// Verify unwrapping works
	if unwrapped := errors.Unwrap(wrappedErr); unwrapped != storageErr {
		t.Error("Expected first unwrap to return StorageError")
	}
}

// Benchmark tests
func BenchmarkUploadFile_Success(b *testing.B) {
	gateway := NewCloudStorageGateway(
		MockAuthService{},
		MockMetadataService{},
		MockStorageService{},
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = gateway.UploadFile(context.Background(), []byte("test data"))
	}
}

func BenchmarkErrorWrapping(b *testing.B) {
	baseErr := errors.New("base error")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := fmt.Errorf("layer1: %w", baseErr)
		err = fmt.Errorf("layer2: %w", err)
		err = fmt.Errorf("layer3: %w", err)
		_ = err
	}
}
