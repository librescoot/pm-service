package clockcontrol

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestAcquireLockProcessOwnership(t *testing.T) {
	if path := os.Getenv("CLOCKCONTROL_TEST_LOCK"); path != "" {
		lock, err := AcquireLock(path)
		if err != nil {
			os.Exit(3)
		}
		lock.Close()
		return
	}
	path := filepath.Join(t.TempDir(), "coordinator.lock")
	lock, err := AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	tryChild := func() error {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAcquireLockProcessOwnership$")
		cmd.Env = append(os.Environ(), "CLOCKCONTROL_TEST_LOCK="+path)
		return cmd.Run()
	}
	if err := tryChild(); err == nil {
		t.Fatal("second process acquired held lock")
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 3 {
		t.Fatalf("child failed unexpectedly: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tryChild(); err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
}

func TestAcquireLockRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	if err := os.Symlink(filepath.Join(dir, "target"), path); err != nil {
		t.Fatal(err)
	}
	if lock, err := AcquireLock(path); err == nil {
		lock.Close()
		t.Fatal("accepted symlink")
	}
}
