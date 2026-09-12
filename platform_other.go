//go:build !linux && !darwin

package monitor

import (
	"os"
	"path/filepath"
)

// lockDir cannot take an advisory lock on this platform. Running one process
// per spool directory is the caller's responsibility here.
func lockDir(dir string) (*os.File, error) {
	return os.OpenFile(filepath.Join(dir, "LOCK"), os.O_CREATE|os.O_RDWR, 0o600)
}

// diskFree is not measurable on this platform; the floor check is skipped.
func diskFree(string) (int64, bool) { return 0, false }
