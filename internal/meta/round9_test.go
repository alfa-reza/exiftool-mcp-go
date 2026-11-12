package meta

import (
	"context"
	"encoding/json"
	"testing"
)

// Round-9 regression tests: a nonzero ExifTool exit is explained
// only by actual per-file errors — never by a warning, and never
// while stderr holds error lines that name no file in the batch.

func zzStdout(t *testing.T) []byte {
	t.Helper()
	stdout, err := json.Marshal([]map[string]any{
		{"SourceFile": "a.jpg", "EXIF:Make": "Canon"},
		{"SourceFile": "b.jpg", "EXIF:Make": "Nikon"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return stdout
}

func TestGlobalErrorNotMaskedByWarning(t *testing.T) {
	fr := &fakeRunner{stdout: zzStdout(t), stderr: "Warning: minor issue - a.jpg\nError: global failure not tied to any file\n"}
	r := &Reader{Runner: fr}
	if _, err := r.Read(context.Background(), []string{"a.jpg", "b.jpg"}, ReadSpec{}); err == nil {
		t.Fatal("a global error line plus a per-file warning read as success; the global error was masked")
	}
}

func TestWarningAloneDoesNotExplainExit(t *testing.T) {
	fr := &fakeRunner{stdout: zzStdout(t), stderr: "Warning: minor issue - a.jpg\n"}
	r := &Reader{Runner: fr}
	if _, err := r.Read(context.Background(), []string{"a.jpg", "b.jpg"}, ReadSpec{}); err == nil {
		t.Fatal("exit 1 with only a warning read as success; a warning cannot explain an error exit")
	}
}

func TestPerFileErrorStillExplainsExit(t *testing.T) {
	stdout, err := json.Marshal([]map[string]any{
		{"SourceFile": "a.jpg", "EXIF:Make": "Canon"},
		{"SourceFile": "b.jpg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{stdout: stdout, stderr: "Error: Not a valid JPEG - b.jpg\n"}
	r := &Reader{Runner: fr}
	out, err := r.Read(context.Background(), []string{"a.jpg", "b.jpg"}, ReadSpec{})
	if err != nil {
		t.Fatalf("a fully correlated per-file error must keep per-file results: %v", err)
	}
	if len(out) != 2 || out[0].Status != "ok" || out[1].Status != "error" {
		t.Fatalf("statuses = %v, want [ok error]", []string{out[0].Status, out[1].Status})
	}
}
