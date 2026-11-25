//go:build !windows

package protocol

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// ErrLockHeld indicates another process already holds the single-instance lock file.
var ErrLockHeld = errors.New("instance lock already held")

// FileLock holds an exclusive advisory flock on a lock file.
type FileLock struct {
	file *os.File
	path string
}

// AcquireFileLock opens path and acquires a non-blocking exclusive flock.
func AcquireFileLock(path string) (*FileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w: %s", ErrLockHeld, path)
		}
		return nil, err
	}
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	return &FileLock{file: f, path: path}, nil
}

// Release unlocks and closes the lock file.
func (l *FileLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}

// StartParentWatchdog monitors parentPID every 2 seconds and invokes onGone if the parent exits.
func StartParentWatchdog(ctx context.Context, parentPID int, onGone func()) {
	if parentPID <= 1 {
		return
	}
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if os.Getppid() == 1 || !processAlive(parentPID) {
					onGone()
					return
				}
			}
		}
	}()
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
