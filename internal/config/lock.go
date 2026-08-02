package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// Lock stops a second agent from serving the same location.
//
// Two agents on one shard do not simply duplicate work: they fight. Each sees
// the other's container as one it did not create, removes it and creates its
// own, forever. The symptom — a container that appears and vanishes every few
// seconds — reads as a crash loop in the runtime rather than as two agents,
// which is a long way from the cause.
type Lock struct {
	file *os.File
	path string
}

// Acquire takes an exclusive lock for the given location. The lock is advisory
// and held by the open file descriptor, so it disappears with the process even
// if it is killed.
func Acquire(location string) (*Lock, error) {
	dir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate home directory: %w", err)
	}
	dir = filepath.Join(dir, ".location-agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}

	path := filepath.Join(dir, location+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := ""
		if raw, rerr := os.ReadFile(path); rerr == nil && len(raw) > 0 {
			holder = " (pid " + string(raw) + ")"
		}
		f.Close()
		return nil, fmt.Errorf("another location-agent is already serving %q%s", location, holder)
	}

	// Recorded for the error message above, not for the locking itself.
	_ = f.Truncate(0)
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return &Lock{file: f, path: path}, nil
}

func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	// The file is left behind on purpose: removing it races with another agent
	// that has already opened it and would hand the lock to two processes.
	return l.file.Close()
}
