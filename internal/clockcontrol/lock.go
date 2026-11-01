// Package clockcontrol contains the local clock coordinator's primitives.
package clockcontrol

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Lock provides exclusive process ownership. The containing directory must be
// owned by root and not writable by other users. The lock file must not be
// removed while a coordinator is running.
type Lock struct {
	file *os.File
}

// AcquireLock takes a non-blocking exclusive lock on path.
func AcquireLock(path string) (*Lock, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open coordinator lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat coordinator lock: %w", err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("coordinator lock is not a regular file")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("lock coordinator: %w", err)
	}
	return &Lock{file: file}, nil
}

// Close releases the lock without removing its inode.
func (l *Lock) Close() error {
	return l.file.Close()
}
