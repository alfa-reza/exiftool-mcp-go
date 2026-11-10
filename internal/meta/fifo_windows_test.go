//go:build windows

package meta

import "testing"

// makeFIFO is a no-op on Windows, which has no FIFOs. The enumeration
// count expectations in TestEnumeratePolicy are identical with or
// without the pipe present: a FIFO is skipped either way.
func makeFIFO(t *testing.T, path string) {
	t.Helper()
}
