//go:build windows

package protocol

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// ErrLockHeld indicates another process already holds the single-instance lock file.
var ErrLockHeld = errors.New("instance lock already held")

// FileLock holds a best-effort lock file handle on Windows.
type FileLock struct {
	file *os.File
	path string
}

// AcquireFileLock opens path as a lock marker on Windows.
func AcquireFileLock(path string) (*FileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &FileLock{file: f, path: path}, nil
}

// Release closes the lock file handle.
func (l *FileLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = l.file.Close()
	l.file = nil
}

// StartParentWatchdog is a no-op fallback on Windows.
func StartParentWatchdog(_ context.Context, _ int, _ func()) {}
