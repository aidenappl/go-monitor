package monitor

import (
	"context"
	"sync"
	"time"
)

// Timer tracks duration for an operation and emits an event when ended.
type Timer struct {
	start time.Time
	name  string
	data  map[string]any
	mu    sync.Mutex
}

// StartTimer creates a new Timer that begins tracking immediately.
func StartTimer(name string) *Timer {
	return &Timer{
		start: time.Now(),
		name:  name,
		data:  make(map[string]any),
	}
}

// WithData adds a key-value pair to the timer's data map.
func (t *Timer) WithData(key string, value any) *Timer {
	t.mu.Lock()
	t.data[key] = value
	t.mu.Unlock()
	return t
}

// End stops the timer and emits an event with the accumulated data and duration_ms.
func (t *Timer) End(ctx context.Context) {
	duration := time.Since(t.start)

	t.mu.Lock()
	t.data["duration_ms"] = duration.Milliseconds()
	data := make(map[string]any, len(t.data))
	for k, v := range t.data {
		data[k] = v
	}
	t.mu.Unlock()

	emitWithCallerDepth(ctx, t.name, data, LevelInfo, 2)
}
