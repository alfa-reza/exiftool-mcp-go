package meta

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Round-7 regression tests.

func TestEnumerateHonorsContext(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A cancelled context stops enumeration with its error
	// instead of walking on unbounded.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Enumerate(ctx, []string{dir}, true, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled enumeration err = %v, want context.Canceled", err)
	}
	// A live context still enumerates normally.
	cands, _, err := Enumerate(context.Background(), []string{dir}, true, nil)
	if err != nil || len(cands) != 1 {
		t.Fatalf("live enumeration: cands=%d err=%v, want 1 candidate", len(cands), err)
	}
}
