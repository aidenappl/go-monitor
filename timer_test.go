package monitor

import (
	"context"
	"testing"
	"time"
)

func TestTimer(t *testing.T) {
	if err := Init(Config{Service: "test-timer", DisableStdout: true}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	t.Run("basic timer", func(t *testing.T) {
		timer := StartTimer("test.operation")
		time.Sleep(10 * time.Millisecond)
		timer.End(context.Background())
	})

	t.Run("timer with data", func(t *testing.T) {
		timer := StartTimer("test.operation")
		timer.WithData("user_id", "123")
		timer.WithData("operation", "query")
		time.Sleep(5 * time.Millisecond)
		timer.End(context.Background())
	})

	t.Run("timer chaining", func(t *testing.T) {
		timer := StartTimer("test.chained").
			WithData("key1", "value1").
			WithData("key2", "value2")
		timer.End(context.Background())
	})

	t.Run("timer start time is accurate", func(t *testing.T) {
		before := time.Now()
		timer := StartTimer("test.timing")
		after := time.Now()

		if timer.start.Before(before) || timer.start.After(after) {
			t.Error("timer.start should be between before and after")
		}
	})
}
