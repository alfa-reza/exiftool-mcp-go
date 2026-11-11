package meta

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkEnumerate measures frozen-snapshot enumeration cost, which
// the PRD requires to be measured before choosing scan defaults.
func BenchmarkEnumerate(b *testing.B) {
	root := b.TempDir()
	for i := 0; i < 2000; i++ {
		p := filepath.Join(root, fmt.Sprintf("f%04d.jpg", i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cands, errs, _ := Enumerate(context.Background(), []string{root}, true, nil)
		if len(errs) != 0 || len(cands) != 2000 {
			b.Fatalf("enumerate: %d candidates, %d errors", len(cands), len(errs))
		}
	}
}
