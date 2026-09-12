//go:build linux || darwin

package monitor

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockDir takes an exclusive, non-blocking advisory lock on dir. Two processes
// appending to one spool would interleave torn writes and each drain the
// other's segments, so the second one is refused and falls back to memory.
//
// flock locks belong to the open file description, so the kernel releases it
// when the process dies — a crash never leaves a stale lock behind.
func lockDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "LOCK"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// diskFree reports the bytes available to an unprivileged user on the
// filesystem holding dir.
func diskFree(dir string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}
