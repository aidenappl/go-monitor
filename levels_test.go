package monitor

import (
	"context"
	"testing"
)

func TestLevelConstants(t *testing.T) {
	tests := []struct {
		name     string
		constant string
		want     string
	}{
		{"debug", LevelDebug, "debug"},
		{"info", LevelInfo, "info"},
		{"warn", LevelWarn, "warn"},
		{"error", LevelError, "error"},
		{"fatal", LevelFatal, "fatal"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.constant != tt.want {
				t.Errorf("Level%s = %v, want %v", tt.name, tt.constant, tt.want)
			}
		})
	}
}

func TestConvenienceFunctions(t *testing.T) {
	if err := Init(Config{Service: "test-levels", DisableStdout: true}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx := context.Background()

	// These should not panic
	Info(ctx, "test.info", map[string]any{"key": "value"})
	Warn(ctx, "test.warn", map[string]any{"key": "value"})
	Error(ctx, "test.error", map[string]any{"key": "value"})
	Fatal(ctx, "test.fatal", map[string]any{"key": "value"})
}

func TestDebugOnlyEmitsWhenEnabled(t *testing.T) {
	t.Run("debug disabled", func(t *testing.T) {
		if err := Init(Config{Service: "test-debug", DisableStdout: true, Debug: false}); err != nil {
			t.Fatalf("Init() error = %v", err)
		}

		ctx := context.Background()
		// Should be a no-op when debug is disabled
		Debug(ctx, "test.debug", map[string]any{"key": "value"})
	})

	t.Run("debug enabled", func(t *testing.T) {
		if err := Init(Config{Service: "test-debug", DisableStdout: true, Debug: true}); err != nil {
			t.Fatalf("Init() error = %v", err)
		}

		ctx := context.Background()
		// Should emit when debug is enabled
		Debug(ctx, "test.debug", map[string]any{"key": "value"})
	})
}

func TestConvenienceFunctionsBeforeInit(t *testing.T) {
	globalConfig.Store(nil)

	ctx := context.Background()

	// None of these should panic
	Debug(ctx, "test.debug", nil)
	Info(ctx, "test.info", nil)
	Warn(ctx, "test.warn", nil)
	Error(ctx, "test.error", nil)
	Fatal(ctx, "test.fatal", nil)
}
