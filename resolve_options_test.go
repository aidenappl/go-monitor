package monitor

import "testing"

func TestResolveEmitOptions_DefaultsToInfo(t *testing.T) {
	if got := ResolveEmitOptions().Level; got != LevelInfo {
		t.Errorf("Level = %q, want %q", got, LevelInfo)
	}
}

func TestResolveEmitOptions_AppliesWithLevel(t *testing.T) {
	tests := []struct {
		name  string
		level string
	}{
		{"debug", LevelDebug},
		{"info", LevelInfo},
		{"warn", LevelWarn},
		{"error", LevelError},
		{"fatal", LevelFatal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveEmitOptions(WithLevel(tt.level)).Level; got != tt.level {
				t.Errorf("Level = %q, want %q", got, tt.level)
			}
		})
	}
}

func TestResolveEmitOptions_LastOptionWins(t *testing.T) {
	got := ResolveEmitOptions(WithLevel(LevelWarn), WithLevel(LevelError)).Level
	if got != LevelError {
		t.Errorf("Level = %q, want %q (last option should win)", got, LevelError)
	}
}
