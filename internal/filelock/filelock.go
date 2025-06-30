package filelock

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// FileLock represents a file-based lock
type FileLock struct {
	lockFile string
	acquired bool
}

// New creates a new file lock for the given file path
func New(filePath string) *FileLock {
	lockFile := filePath + ".lock"
	return &FileLock{
		lockFile: lockFile,
		acquired: false,
	}
}

// Lock acquires an exclusive lock by creating a lock file
// It will retry for up to 30 seconds if the lock is already held
func (fl *FileLock) Lock() error {
	if fl.acquired {
		return fmt.Errorf("lock already acquired")
	}

	// Create directory if it doesn't exist
	dir := filepath.Dir(fl.lockFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create lock directory: %w", err)
	}

	// Try to acquire lock with retries
	maxRetries := 300 // 30 seconds with 100ms intervals
	for i := 0; i < maxRetries; i++ {
		// Try to create lock file exclusively
		file, err := os.OpenFile(fl.lockFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			// Successfully created lock file
			// Write process ID to lock file for debugging
			fmt.Fprintf(file, "%d\n", os.Getpid())
			file.Close()
			fl.acquired = true
			return nil
		}

		// Check if it's a permission error or other non-existence error
		if !os.IsExist(err) {
			return fmt.Errorf("failed to create lock file: %w", err)
		}

		// Lock file exists, check if it's stale
		if fl.isStale() {
			// Try to remove stale lock file
			if removeErr := os.Remove(fl.lockFile); removeErr == nil {
				continue // Try again
			}
		}

		// Wait before retrying
		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for lock on %s", fl.lockFile)
}

// Unlock releases the lock by removing the lock file
func (fl *FileLock) Unlock() error {
	if !fl.acquired {
		return nil // Nothing to unlock
	}

	err := os.Remove(fl.lockFile)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove lock file: %w", err)
	}

	fl.acquired = false
	return nil
}

// isStale checks if the lock file is stale (older than 5 minutes)
// This helps recover from situations where a process crashed without cleaning up
func (fl *FileLock) isStale() bool {
	info, err := os.Stat(fl.lockFile)
	if err != nil {
		return true // If we can't stat it, consider it stale
	}

	// Consider lock stale if it's older than 5 minutes
	return time.Since(info.ModTime()) > 5*time.Minute
}

// TryLock attempts to acquire the lock without blocking
func (fl *FileLock) TryLock() error {
	if fl.acquired {
		return fmt.Errorf("lock already acquired")
	}

	// Create directory if it doesn't exist
	dir := filepath.Dir(fl.lockFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create lock directory: %w", err)
	}

	// Try to create lock file exclusively (non-blocking)
	file, err := os.OpenFile(fl.lockFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("lock already held")
		}
		return fmt.Errorf("failed to create lock file: %w", err)
	}

	// Successfully created lock file
	fmt.Fprintf(file, "%d\n", os.Getpid())
	file.Close()
	fl.acquired = true
	return nil
}
