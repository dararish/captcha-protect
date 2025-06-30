package filelock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileLock_BasicLocking(t *testing.T) {
	// Create a temporary file for testing
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")

	// Create the test file
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Test basic lock/unlock
	lock := New(testFile)

	// Lock should succeed
	if err := lock.Lock(); err != nil {
		t.Fatalf("Failed to acquire lock: %v", err)
	}

	// Verify lock file exists
	lockFile := testFile + ".lock"
	if _, err := os.Stat(lockFile); os.IsNotExist(err) {
		t.Fatalf("Lock file was not created")
	}

	// Unlock should succeed
	if err := lock.Unlock(); err != nil {
		t.Fatalf("Failed to release lock: %v", err)
	}

	// Verify lock file is removed
	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Fatalf("Lock file was not removed")
	}
}

func TestFileLock_TryLock(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")

	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	lock1 := New(testFile)
	lock2 := New(testFile)

	// First lock should succeed
	if err := lock1.TryLock(); err != nil {
		t.Fatalf("First TryLock failed: %v", err)
	}
	defer lock1.Unlock()

	// Second lock should fail
	if err := lock2.TryLock(); err == nil {
		t.Fatalf("Second TryLock should have failed")
	}
}

func TestFileLock_ConcurrentLocking(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")

	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	lock1 := New(testFile)
	lock2 := New(testFile)

	// First lock should succeed
	if err := lock1.Lock(); err != nil {
		t.Fatalf("First lock failed: %v", err)
	}

	// Start a goroutine that will try to acquire the second lock
	done := make(chan bool)
	go func() {
		// This should block until the first lock is released
		if err := lock2.Lock(); err != nil {
			t.Errorf("Second lock failed: %v", err)
		}
		lock2.Unlock()
		done <- true
	}()

	// Wait a bit to ensure the second lock is waiting
	time.Sleep(100 * time.Millisecond)

	// Release the first lock
	if err := lock1.Unlock(); err != nil {
		t.Fatalf("Failed to release first lock: %v", err)
	}

	// Wait for the second lock to complete
	select {
	case <-done:
		// Success
	case <-time.After(5 * time.Second):
		t.Fatalf("Second lock did not acquire within timeout")
	}
}

func TestFileLock_StaleLockHandling(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	lockFile := testFile + ".lock"

	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Create a stale lock file (old timestamp)
	if err := os.WriteFile(lockFile, []byte("12345"), 0644); err != nil {
		t.Fatalf("Failed to create stale lock file: %v", err)
	}

	// Make the lock file appear old
	oldTime := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(lockFile, oldTime, oldTime); err != nil {
		t.Fatalf("Failed to set old timestamp on lock file: %v", err)
	}

	lock := New(testFile)

	// Lock should succeed by removing the stale lock
	if err := lock.Lock(); err != nil {
		t.Fatalf("Failed to acquire lock with stale lock present: %v", err)
	}

	if err := lock.Unlock(); err != nil {
		t.Fatalf("Failed to release lock: %v", err)
	}
}
