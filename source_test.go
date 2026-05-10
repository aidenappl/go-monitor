package monitor

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSourceLocationCapture(t *testing.T) {
	if err := Init(Config{Service: "test-source", DisableStdout: true}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	t.Run("Emit captures source location", func(t *testing.T) {
		ctx := context.Background()
		event := newEvent(ctx, "test.source", map[string]any{"key": "value"}, "info")
		attachSourceLocation(&event, 1)

		data, ok := event.Data.(map[string]any)
		if !ok {
			t.Fatal("event.Data should be a map")
		}

		if _, ok := data["source_file"]; !ok {
			t.Error("data should contain source_file")
		}
		if _, ok := data["source_line"]; !ok {
			t.Error("data should contain source_line")
		}
		if _, ok := data["source_func"]; !ok {
			t.Error("data should contain source_func")
		}

		// source_file should be just the filename, not the full path
		sourceFile, _ := data["source_file"].(string)
		if sourceFile != "source_test.go" {
			t.Errorf("source_file = %v, want source_test.go", sourceFile)
		}
	})

	t.Run("source location with nil data", func(t *testing.T) {
		ctx := context.Background()
		event := newEvent(ctx, "test.nil-data", nil, "info")
		attachSourceLocation(&event, 1)

		data, ok := event.Data.(map[string]any)
		if !ok {
			t.Fatal("event.Data should be a map after source attach")
		}

		if _, ok := data["source_file"]; !ok {
			t.Error("data should contain source_file even with nil original data")
		}
	})

	t.Run("source location with non-map data", func(t *testing.T) {
		ctx := context.Background()
		event := newEvent(ctx, "test.string-data", "some string", "info")
		attachSourceLocation(&event, 1)

		data, ok := event.Data.(map[string]any)
		if !ok {
			t.Fatal("event.Data should be a map after source attach")
		}

		if data["_data"] != "some string" {
			t.Errorf("original data should be preserved as _data, got %v", data["_data"])
		}
		if _, ok := data["source_file"]; !ok {
			t.Error("data should contain source_file")
		}
	})

	t.Run("CaptureSource disabled", func(t *testing.T) {
		captureOff := false
		if err := Init(Config{Service: "test-source", DisableStdout: true, CaptureSource: &captureOff}); err != nil {
			t.Fatalf("Init() error = %v", err)
		}

		ctx := context.Background()
		// Create a test server to capture the emitted event
		event := newEvent(ctx, "test.no-source", map[string]any{"key": "value"}, "info")

		// When CaptureSource is false, Emit should not attach source
		cfg := globalConfig.Load()
		if captureSourceEnabled(cfg) {
			t.Error("captureSourceEnabled should return false")
		}

		// Verify the data doesn't have source fields
		jsonBytes, _ := json.Marshal(event)
		var decoded map[string]any
		json.Unmarshal(jsonBytes, &decoded)

		dataMap, _ := decoded["data"].(map[string]any)
		if _, ok := dataMap["source_file"]; ok {
			t.Error("source_file should not be present when CaptureSource is disabled")
		}
	})

	t.Run("CaptureSource default (nil = true)", func(t *testing.T) {
		if err := Init(Config{Service: "test-source", DisableStdout: true}); err != nil {
			t.Fatalf("Init() error = %v", err)
		}

		cfg := globalConfig.Load()
		if !captureSourceEnabled(cfg) {
			t.Error("captureSourceEnabled should return true by default")
		}
	})
}
