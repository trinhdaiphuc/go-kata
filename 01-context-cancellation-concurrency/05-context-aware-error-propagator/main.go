package main

import (
	"context"
	"fmt"
	"time"
)

func main() {

}

type CloudStorageGateway struct {
	authService     AuthService
	metadataService MetadataService
	storageService  StorageService
}

func NewCloudStorageGateway(auth AuthService, metadata MetadataService, storage StorageService) *CloudStorageGateway {
	return &CloudStorageGateway{
		authService:     auth,
		metadataService: metadata,
		storageService:  storage,
	}
}

func (c CloudStorageGateway) UploadFile(ctx context.Context, data []byte) error {
	userID, err := c.authService.Authenticate(ctx, "some-token")
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	metadata, err := c.metadataService.FetchMetadata(ctx, userID)
	if err != nil {
		return fmt.Errorf("metadata fetch failed: %w", err)
	}

	err = c.storageService.StoreData(ctx, userID, metadata)
	if err != nil {
		return fmt.Errorf("data storage failed: %w", err)
	}

	return nil
}

// Service interfaces for dependency injection and testing
type AuthService interface {
	Authenticate(ctx context.Context, token string) (string, error)
}

type MetadataService interface {
	FetchMetadata(ctx context.Context, userID string) (map[string]string, error)
}

type StorageService interface {
	StoreData(ctx context.Context, userID string, data map[string]string) error
}

// Concrete implementations
type DefaultAuthService struct{}

func (a DefaultAuthService) Authenticate(ctx context.Context, token string) (string, error) {
	time.Sleep(50 * time.Millisecond)
	return "userID123", nil
}

type DefaultMetadataService struct{}

func (m DefaultMetadataService) FetchMetadata(ctx context.Context, userID string) (map[string]string, error) {
	time.Sleep(50 * time.Millisecond)
	return map[string]string{"role": "admin"}, nil
}

type DefaultStorageService struct{}

func (s DefaultStorageService) StoreData(ctx context.Context, userID string, data map[string]string) error {
	time.Sleep(50 * time.Millisecond)
	return nil
}

type AuthError struct {
	Message string
	Err     error
}

func (e AuthError) Error() string {
	return e.Message
}

func (e AuthError) Unwrap() error {
	return e.Err
}

type MetadataError struct {
	Message string
	Err     error
}

func (e MetadataError) Error() string {
	return e.Message
}

func (e MetadataError) Unwrap() error {
	return e.Err
}

type StorageError struct {
	Message string
	Err     error
}

func (e StorageError) Error() string {
	return e.Message
}

func (e StorageError) Unwrap() error {
	return e.Err
}
