//go:build !windows

package meta

import (
	"syscall"
	"testing"
)

// makeFIFO creates a named pipe used to prove enumeration skips
// non-regular files without hanging.
func makeFIFO(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Logf("Mkfifo: %v (non-regular-file coverage reduced)", err)
	}
}
