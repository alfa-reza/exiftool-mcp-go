package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alfa-reza/exiftool-mcp-go/internal/config"
)

// writeFakeExifTool installs an executable shell script as the
// "exiftool" binary for a test.
func writeFakeExifTool(t *testing.T, body string) string {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh available")
	}
	p := filepath.Join(t.TempDir(), "exiftool")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunTimeoutIsAnErrorNotData(t *testing.T) {
	// A process killed by the call timeout surfaces as *exec.ExitError;
	// the runner must report the context error instead of returning
	// the partial output as an ordinary result.
	fake := writeFakeExifTool(t, "exec sleep 5\n")
	r := New(config.Config{ExifToolPath: fake, CallTimeout: 100 * time.Millisecond})
	_, err := r.Run(context.Background(), "-ver")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

func TestRunNonZeroExitIsData(t *testing.T) {
	fake := writeFakeExifTool(t, "echo hello\nexit 1\n")
	r := New(config.Config{ExifToolPath: fake, CallTimeout: 10 * time.Second})
	res, err := r.Run(context.Background(), "-ver")
	if err != nil {
		t.Fatalf("non-zero exit must not be a Go error: %v", err)
	}
	if res.ExitCode != 1 || string(res.Stdout) != "hello\n" {
		t.Fatalf("unexpected result: %+v", res)
	}
}
