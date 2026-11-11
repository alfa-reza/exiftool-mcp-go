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
		"all", "Config", "STAY_OPEN",
		// Parameterized option forms: an option name plus digits is
		// still an option (-fast2, -ee3, -g4), never a tag.
		"g4", "G3", "fast2", "ee3", "fast5", "g0"}
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
	for _, want := range []string{"-a", "-G0:1:3:4", "-U", "-ee2", "-n", "-struct", "-fast2"} {
		if !strings.Contains(args2, want) {
			t.Errorf("args %q missing %q", args2, want)
		}
	}
	// Group families always travel as ONE combined -G argument:
	// separate -G flags replace each other on real ExifTool.
	for _, tc := range []struct {
		spec ReadSpec
		want string
	}{
		{ReadSpec{}, "-G0:1"},
		{ReadSpec{EmbeddedLevel: 1}, "-G0:1:3"},
		{ReadSpec{IncludeDuplicates: true}, "-G0:1:4"},
		{ReadSpec{EmbeddedLevel: 2, IncludeDuplicates: true}, "-G0:1:3:4"},
		{ReadSpec{DocProvenance: true}, "-G0:1:3"},
	} {
		joined := strings.Join(tc.spec.Args(), " ")
		if !strings.Contains(joined, tc.want) || strings.Count(joined, "-G") != 1 {
			t.Errorf("spec %+v: args %q, want exactly one combined %q", tc.spec, joined, tc.want)
		}
	}
}

func TestParseJSONArrayRejectsTruncation(t *testing.T) {
	// Output cut after a complete object (the closing bracket never
	// arrived) must not parse as a successful read.
	if _, err := ParseJSONArray([]byte(`[{"SourceFile":"a.jpg","EXIF:Make":"Canon"}`)); err == nil {
		t.Fatal("truncated JSON array was accepted")
	}
	if _, err := ParseJSONArray([]byte(`[{"SourceFile":"a.jpg"},{"SourceFile":"b.jpg"}`)); err == nil {
		t.Fatal("JSON array missing its closing bracket was accepted")
	}
	objects, err := ParseJSONArray([]byte(`[{"SourceFile":"a.jpg"}]`))
	if err != nil || len(objects) != 1 {
		t.Fatalf("valid array rejected: %v (%d objects)", err, len(objects))
	}
}

func TestEmbeddedLevelArgsDocumentedForm(t *testing.T) {
	// The documented option form is -ee[NUM].
	for level, want := range map[int]string{1: "-ee", 2: "-ee2", 3: "-ee3"} {
		args := strings.Join(ReadSpec{EmbeddedLevel: level}.Args(), " ")
		if !strings.Contains(args, " "+want+" ") && !strings.HasSuffix(args, " "+want) && !strings.Contains(args, want) {
			t.Errorf("level %d: args %q missing %q", level, args, want)
		}
		if strings.Contains(args, "-eee") && want != "-ee3" {
			t.Errorf("level %d: args %q use the undocumented repeated-e form", level, args)
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

func TestReadCorrelatesAcrossSlashStyles(t *testing.T) {
	// On Windows, ExifTool echoes SourceFile and stderr operands with
	// forward slashes while callers pass backslash paths. Correlation
	// must not depend on the slash style, or every read on Windows is
	// misreported as "no output from exiftool for this file".
	fr := &fakeRunner{
		stdout: []byte(`[{"SourceFile":"C:/x/bad.jpg"},{"SourceFile":"C:/x/good.jpg","EXIF:IFD0:Make":"Canon"}]`),
		// Forward slashes AND a CRLF line ending, as on Windows.
		stderr: "Error: File format error - C:/x/bad.jpg\r\n",
	}
	r := &Reader{Runner: fr}
	results, err := r.Read(context.Background(), []string{`C:\x\bad.jpg`, `C:\x\good.jpg`},
		ReadSpec{IncludeTags: []string{"Make"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Status != "error" || results[0].Error != "File format error" {
		t.Fatalf("bad file not correlated across slash styles: %+v", results[0])
	}
	if results[1].Status != "ok" || len(results[1].Entries) != 1 {
		t.Fatalf("good file not correlated across slash styles: %+v", results[1])
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

	cands, errs, _ := Enumerate(context.Background(), []string{root}, true, []string{"jpg"})
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
	cands2, _, _ := Enumerate(context.Background(), []string{root}, false, nil)
	if len(cands2) != 3 {
		t.Fatalf("expected 3 shallow candidates, got %d", len(cands2))
	}
	// Missing path is a structured error, not a panic.
	_, errs3, _ := Enumerate(context.Background(), []string{filepath.Join(root, "nope.jpg")}, false, nil)
	if len(errs3) != 1 || errs3[0].Code != "PATH_NOT_FOUND" {
		t.Fatalf("expected PATH_NOT_FOUND, got %+v", errs3)
	}
	// Extension filter is case-insensitive and dot-optional.
	cands4, _, _ := Enumerate(context.Background(), []string{root}, true, []string{".JPG"})
	if len(cands4) != 4 {
		t.Fatalf("expected 4 with .JPG filter, got %d", len(cands4))
	}
}

func TestEnumerateSymlinkRootRecursive(t *testing.T) {
	// A symlink-to-directory as the walk root must yield the
	// directory's files: WalkDir Lstats its root, so without
	// resolving it the walk silently produced zero candidates.
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "a.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	cands, errs, _ := Enumerate(context.Background(), []string{link}, true, nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(cands) != 1 {
		t.Fatalf("symlink root should yield 1 candidate, got %d", len(cands))
	}
}

func TestEnumerateSymlinkFileRecursive(t *testing.T) {
	// A symlink to a regular file inside a walked tree is a
	// candidate, exactly as in the non-recursive listing.
	base := t.TempDir()
	real := filepath.Join(base, "real.jpg")
	if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(base, "link.jpg")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	cands, errs, _ := Enumerate(context.Background(), []string{base}, true, nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(cands) != 2 {
		t.Fatalf("recursive walk should find real + symlinked file, got %d candidates", len(cands))
	}
}

func TestEnumerateTraversalErrorReported(t *testing.T) {
	// An unreadable subtree must surface as an enumeration error;
	// silently skipping it makes a partial scan look complete.
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "open.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(base, "locked")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "hidden.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sub, 0o755)
	cands, errs, _ := Enumerate(context.Background(), []string{base}, true, nil)
	if len(errs) == 0 {
		t.Skip("directory permissions not enforced in this environment (likely running as root)")
	}
	found := false
	for _, e := range errs {
		if e.Path == sub && e.Code == "PERMISSION_DENIED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a PERMISSION_DENIED error for the locked subtree, got %v", errs)
	}
	if len(cands) != 1 {
		t.Fatalf("the readable file should still be a candidate, got %d", len(cands))
	}
}

func TestValidateSelectorRejectsWriteOptions(t *testing.T) {
	// Names that must never become argv: write-capable options and
	// their documented aliases, plus the alias class that slipped
	// through an earlier, incomplete denylist. If ExifTool is
	// upgraded and this test needs new names, extend the extraction
	// in options.go — do not weaken the test.
	bad := []string{
		// write-capable options and aliases
		"w", "W", "textOut", "tagOut", "o", "overwrite_original",
		"restore_original", "delete_original", "savePath", "saveFormat",
		"saveBin", "writeMode", "out", "diff",
		// file-reading / config options
		"config", "stay_open", "api", "common_args", "tagsFromFile",
		"addTagsFromFile", "srcFile", "geotag",
		// the alias class from the round-4 review
		"groupNames", "json", "binary", "printConv", "extractEmbedded",
		// parameterized forms of listed bases
		"htmldump0", "echo2", "fileOrder4", "sort2", "s3",
	}
	for _, name := range bad {
		if err := ValidateSelector(name); err == nil {
			t.Errorf("selector %q must be rejected (it is an ExifTool option)", name)
		}
	}
	// Real tag names keep working bare, and any collision-prone tag
	// remains reachable in qualified form.
	for _, ok := range []string{"SerialNumber", "Comment", "Make", "ISO", "EXIF:SerialNumber", "File:FileName"} {
		if err := ValidateSelector(ok); err != nil {
			t.Errorf("selector %q should be accepted: %v", ok, err)
		}
	}
}

func TestReaderStderrCorrelationWithDashPath(t *testing.T) {
	// A file name containing " - " must not break stderr error
	// correlation: splitting the diagnostic line at its last " - "
	// used to truncate the operand to "edited.jpg" and silently
	// drop the error, leaving the file "ok".
	f := "/tmp/photos/photo - edited.jpg"
	r := &Reader{Runner: &fakeRunner{
		stdout: []byte(`[{"SourceFile":"` + f + `"}]`),
		stderr: "Error: File format error - " + f + "\n",
	}}
	res, err := r.Read(context.Background(), []string{f}, ReadSpec{IncludeTags: []string{"Make"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Status != "error" || res[0].Error != "File format error" {
		t.Fatalf("dash-path error not correlated: %+v", res)
	}
}

func TestParseJSONArrayRejectsTrailingData(t *testing.T) {
	// Output after the closing bracket is not part of the JSON
	// document ExifTool produced; accepting it would bless
	// malformed output as a successful read.
	for _, data := range []string{
		`[{"SourceFile":"x"}] trailing garbage`,
		`[{"SourceFile":"x"}] {"more":1}`,
		`[{"SourceFile":"x"}] ]`,
	} {
		if _, err := ParseJSONArray([]byte(data)); err == nil {
			t.Fatalf("trailing data accepted: %q", data)
		}
	}
	if _, err := ParseJSONArray([]byte("[{\"SourceFile\":\"x\"}]\n")); err != nil {
		t.Fatalf("clean output with trailing newline rejected: %v", err)
	}
}

func TestReadUnexplainedNonzeroExitFails(t *testing.T) {
	// fakeRunner always exits 1. With valid JSON and no stderr
	// diagnostics to correlate, nothing explains the failure, so
	// the read must not report the file as a clean ok.
	fr := &fakeRunner{
		stdout: []byte(`[{"SourceFile":"/x/a.jpg","EXIF:IFD0:Make":"Canon"}]`),
		stderr: "",
	}
	r := &Reader{Runner: fr}
	if _, err := r.Read(context.Background(), []string{"/x/a.jpg"}, ReadSpec{}); err == nil {
		t.Fatal("unexplained exit 1 was reported as a successful read")
	}
	// The explained counterpart keeps its per-file results:
	// TestReadStderrErrorCorrelation covers a correlated error.
}
