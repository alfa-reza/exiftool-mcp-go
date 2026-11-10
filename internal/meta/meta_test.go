package meta

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alfa-reza/exiftool-mcp-go/internal/runner"
)

func TestValidateSelector(t *testing.T) {
	good := []string{"Make", "EXIF:Make", "EXIF:All", "XMP:CreateDate", "GPSLatitude", "IFD0:ISO",
		// Group families may contain dashes (XMP-dc, XMP-photoshop).
		"XMP-dc:Title", "XMP-photoshop:Headline",
		// A tag that shares its name with an ExifTool option stays
		// available in the qualified form, which is never an option.
		"System:FileName", "ExifTool:FileNumber"}
	for _, s := range good {
		if err := ValidateSelector(s); err != nil {
			t.Errorf("selector %q should be valid: %v", s, err)
		}
	}
	bad := []string{"", "-config", "--all", "Make=1", "TAG<file", "a b", "EXIF:", "EXIF:Make#", "*", "EXIF:*", "a/b", "../x",
		// Unqualified names the ExifTool application parses as options
		// (argument injection into the read-only boundary).
		"config", "stay_open", "restore_original", "delete_original",
		"overwrite_original", "overwrite_original_in_place", "tagsFromFile",
		"srcfile", "filename", "directory", "api", "if", "p", "ver", "j",
		"all", "Config", "STAY_OPEN"}
	for _, s := range bad {
		if err := ValidateSelector(s); err == nil {
			t.Errorf("selector %q should be rejected", s)
		}
	}
}

func TestSpecArgs(t *testing.T) {
	s := ReadSpec{IncludeTags: []string{"EXIF:Make"}, ExcludeTags: []string{"GPS:*"}}
	// GPS:* is not a valid selector; use a group selector instead.
	s.ExcludeTags = []string{"GPS:All"}
	args := strings.Join(s.Args(), " ")
	for _, want := range []string{"-j", "-G0:1", "-EXIF:Make", "--GPS:All"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
	s2 := ReadSpec{IncludeDuplicates: true, UnknownTags: 2, EmbeddedLevel: 2, NumericValues: true, PreserveStructures: true, FastScanLevel: 2}
	args2 := strings.Join(s2.Args(), " ")
	for _, want := range []string{"-a", "-G4", "-U", "-eee", "-G3", "-n", "-struct", "-fast2"} {
		if !strings.Contains(args2, want) {
			t.Errorf("args %q missing %q", args2, want)
		}
	}
}

func TestParseJSONArrayPreservesDuplicates(t *testing.T) {
	// Two identical keys in one object (ExifTool emits renamed keys in
	// JSON mode, but the parser must never silently drop pairs).
	data := []byte(`[{"SourceFile":"a.jpg","EXIF:Make":"Canon","EXIF:Make":"Canon2","Big":12345678901234567890}]`)
	objects, err := ParseJSONArray(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || len(objects[0]) != 4 {
		t.Fatalf("expected 4 pairs, got %+v", objects)
	}
	fr := FileResultFromPairs("a.jpg", objects[0])
	if len(fr.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(fr.Entries))
	}
}

func TestParseKey(t *testing.T) {
	g, n, d, _, _ := ParseKey("EXIF:IFD0:Make")
	if g != "EXIF:IFD0" || n != "Make" || d != "" {
		t.Fatalf("unexpected parse: %q %q %q", g, n, d)
	}
	g, n, d, _, _ = ParseKey("File:System:Main:FileName")
	if g != "File:System" || n != "FileName" || d != "Main" {
		t.Fatalf("unexpected Main parse: %q %q %q", g, n, d)
	}
	_, n, d, _, _ = ParseKey("Doc2:GPS:GPSLatitude")
	if n != "GPSLatitude" || d != "Doc2" {
		t.Fatalf("unexpected Doc parse: %q %q", n, d)
	}
	// Sub-documents carry dash-separated numbers ("Doc2-3").
	_, n, d, _, _ = ParseKey("Doc2-3:EXIF:Make")
	if n != "Make" || d != "Doc2-3" {
		t.Fatalf("unexpected sub-document parse: %q %q", n, d)
	}
	// Duplicate copies from -G4 carry a Copy segment in the group.
	g, n, _, _, cp := ParseKey("File:Copy1:Comment")
	if g != "File" || n != "Comment" || cp != "Copy1" {
		t.Fatalf("unexpected copy parse: %q %q %q", g, n, cp)
	}
}

func TestFileResultDiagnostics(t *testing.T) {
	pairs := []Pair{
		{Key: "SourceFile", Value: "x.jpg"},
		{Key: "ExifTool:Error", Value: "File format error"},
		{Key: "ExifTool:Warning", Value: "minor"},
		{Key: "EXIF:Make", Value: "Canon"},
	}
	fr := FileResultFromPairs("x.jpg", pairs)
	if fr.Status != "error" || fr.Error == "" {
		t.Fatalf("expected error status, got %+v", fr)
	}
	if len(fr.Warnings) != 1 || len(fr.Entries) != 1 {
		t.Fatalf("warnings/entries wrong: %+v", fr)
	}
}

type fakeRunner struct {
	stdout []byte
	stderr string
}

func (f *fakeRunner) Run(_ context.Context, _ ...string) (runner.Result, error) {
	return runner.Result{Stdout: f.stdout, Stderr: f.stderr, ExitCode: 1}, nil
}
func (f *fakeRunner) Path() string       { return "fake" }
func (f *fakeRunner) ConfigMode() string { return "standard" }

// With tag selection, ExifTool reports a failed file's error only on
// stderr ("Error: <msg> - <operand>"); the reader must correlate it.
func TestReadStderrErrorCorrelation(t *testing.T) {
	fr := &fakeRunner{
		stdout: []byte(`[{"SourceFile":"/x/bad.jpg"},{"SourceFile":"/x/good.jpg","EXIF:IFD0:Make":"Canon"}]`),
		stderr: "Error: File format error - /x/bad.jpg\n    2 image files read\n",
	}
	r := &Reader{Runner: fr}
	results, err := r.Read(context.Background(), []string{"/x/bad.jpg", "/x/good.jpg"},
		ReadSpec{IncludeTags: []string{"Make"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Status != "error" || results[0].Error != "File format error" {
		t.Fatalf("bad file not correlated: %+v", results[0])
	}
	if results[1].Status != "ok" || len(results[1].Entries) != 1 {
		t.Fatalf("good file wrong: %+v", results[1])
	}
}

func TestEnumeratePolicy(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p string) {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "a.jpg"))
	write(filepath.Join(root, "b.JPG"))
	write(filepath.Join(root, ".hidden.jpg"))
	write(filepath.Join(sub, "c.jpg"))
	// Symlinked directory must not be descended into (loop safety).
	loopDir := filepath.Join(root, "loop")
	if err := os.Symlink(root, loopDir); err != nil {
		t.Fatal(err)
	}
	// Non-regular file must be skipped without hanging.
	makeFIFO(t, filepath.Join(root, "pipe.jpg"))

	cands, errs := Enumerate([]string{root}, true, []string{"jpg"})
	if len(errs) != 0 {
		t.Fatalf("unexpected enum errors: %+v", errs)
	}
	if len(cands) != 4 { // a, b, hidden, sub/c — no loop dupes, no fifo
		var paths []string
		for _, c := range cands {
			paths = append(paths, c.Path)
		}
		t.Fatalf("expected 4 candidates, got %v", paths)
	}
	// Non-recursive: only direct files.
	cands2, _ := Enumerate([]string{root}, false, nil)
	if len(cands2) != 3 {
		t.Fatalf("expected 3 shallow candidates, got %d", len(cands2))
	}
	// Missing path is a structured error, not a panic.
	_, errs3 := Enumerate([]string{filepath.Join(root, "nope.jpg")}, false, nil)
	if len(errs3) != 1 || errs3[0].Code != "PATH_NOT_FOUND" {
		t.Fatalf("expected PATH_NOT_FOUND, got %+v", errs3)
	}
	// Extension filter is case-insensitive and dot-optional.
	cands4, _ := Enumerate([]string{root}, true, []string{".JPG"})
	if len(cands4) != 4 {
		t.Fatalf("expected 4 with .JPG filter, got %d", len(cands4))
	}
}
