package t3

import (
	"path/filepath"
	"testing"
)

func TestLaunchLockDeniesConcurrentAndRecoversAfterInterruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launch.lock")
	first, err := launchLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := launchLock(path); err == nil {
		second.Close()
		t.Fatal("concurrent startup accepted")
	}
	// File close also occurs when a process exits or exec replaces it.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := launchLock(path)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}
