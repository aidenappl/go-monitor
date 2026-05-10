package monitor

import (
	"context"
	"errors"
	"testing"
)

func TestCaptureError(t *testing.T) {
	if err := Init(Config{Service: "test-errors", DisableStdout: true}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	t.Run("basic error capture", func(t *testing.T) {
		err := errors.New("something went wrong")
		// Should not panic
		CaptureError(context.Background(), err)
	})

	t.Run("nil error is no-op", func(t *testing.T) {
		// Should not panic
		CaptureError(context.Background(), nil)
	})

	t.Run("error with additional data", func(t *testing.T) {
		err := errors.New("database connection failed")
		CaptureError(context.Background(), err, map[string]any{
			"host":     "db.example.com",
			"port":     3306,
			"database": "mydb",
		})
	})

	t.Run("error with multiple data maps", func(t *testing.T) {
		err := errors.New("request failed")
		CaptureError(context.Background(), err,
			map[string]any{"method": "GET", "url": "/api/test"},
			map[string]any{"retry_count": 3},
		)
	})

	t.Run("error before init", func(t *testing.T) {
		globalConfig.Store(nil)
		err := errors.New("test error")
		// Should not panic
		CaptureError(context.Background(), err)
	})
}
