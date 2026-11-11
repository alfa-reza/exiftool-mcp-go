package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alfa-reza/exiftool-mcp-go/internal/config"
	"github.com/alfa-reza/exiftool-mcp-go/internal/meta"
	"github.com/alfa-reza/exiftool-mcp-go/internal/query"
	"github.com/alfa-reza/exiftool-mcp-go/internal/runner"
	"github.com/alfa-reza/exiftool-mcp-go/internal/store"
)

// Round-7 regression tests.

// smallQuotaApp builds an App whose result store quota is the
// given size, with the real ExifTool runner.
func smallQuotaApp(t *testing.T, quota int64) *App {
	t.Helper()
	cfg := config.Config{
		StoreDir: t.TempDir(), CallTimeout: 60 * time.Second, MaxConcurrency: 4,
		MaxResponseBytes: 1 << 20, DefaultPageSize: 100, MaxInlineBinary: 1 << 20,
		ResultTTL: time.Hour, StoreQuotaBytes: quota, ScanBatchFiles: 200, ScanBatchTime: 30 * time.Second,
	}
	rn := runner.New(cfg)
	st, err := store.New(cfg.StoreDir, cfg.ResultTTL, cfg.StoreQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	return &App{Cfg: cfg, Runner: rn, Reader: &meta.Reader{Runner: rn}, Store: st}
}

func writeTextValue(t *testing.T, dir, name, text string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// twoBigValuesJPEG writes a JPEG carrying two ~40 KB values.
func twoBigValuesJPEG(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "big.jpg")
	writeJPEG(t, path)
	big1 := writeTextValue(t, dir, "v1.txt", strings.Repeat("a", 40960))
	big2 := writeTextValue(t, dir, "v2.txt", strings.Repeat("b", 40960))
	cli(t, "-overwrite_original", "-Comment<="+big1, "-XMP-dc:Description<="+big2, path)
	return path
}

func TestReadValueRefEvictedDuringCallFails(t *testing.T) {
	requireExifTool(t)
	dir := t.TempDir()
	path := twoBigValuesJPEG(t, dir)
	// The quota fits ONE ~41 KB fragment, not two: writing the
	// second evicts the first. The read must fail with the
	// store-full contract instead of returning a successful
	// response whose first reference already reads "not found".
	app := smallQuotaApp(t, 45000)
	_, out, err := app.readMetadata(context.Background(), nil, ReadMetadataIn{Paths: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "failed" || len(out.Errors) == 0 || out.Errors[0].Code != "RESULT_STORE_FULL" {
		t.Fatalf("status=%s errors=%v, want failed RESULT_STORE_FULL", out.Status, out.Errors)
	}
}

func TestSearchValueRefEvictedDuringBatchFails(t *testing.T) {
	requireExifTool(t)
	dir := t.TempDir()
	// Two matched files, each carrying one ~40 KB value: the
	// batch externalizes the first file's value, then the second
	// file's fragment evicts it under the tight quota. Committing
	// or returning the first record would hand out a dead
	// reference, so the batch must fail instead.
	for _, name := range []string{"one.jpg", "two.jpg"} {
		p := filepath.Join(dir, name)
		writeJPEG(t, p)
		v := writeTextValue(t, t.TempDir(), "v.txt", strings.Repeat("a", 40960))
		cli(t, "-overwrite_original", "-Comment<="+v, p)
	}
	app := smallQuotaApp(t, 45000)
	_, out, err := app.searchMetadata(context.Background(), nil, SearchMetadataIn{
		Paths:      []string{dir},
		Query:      &query.Node{Type: "predicate", Tag: "Comment", Operator: "exists"},
		ReturnTags: []string{"Comment"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "failed" || len(out.Errors) == 0 || out.Errors[0].Code != "RESULT_STORE_FULL" {
		t.Fatalf("status=%s errors=%v, want failed RESULT_STORE_FULL", out.Status, out.Errors)
	}
}

func TestCompareExternalizesOversizedValues(t *testing.T) {
	requireExifTool(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jpg")
	b := filepath.Join(dir, "b.jpg")
	writeJPEG(t, a)
	writeJPEG(t, b)
	big := writeTextValue(t, dir, "big.txt", strings.Repeat("x", 100*1024))
	cli(t, "-overwrite_original", "-XMP-dc:Description=short", a)
	cli(t, "-overwrite_original", "-XMP-dc:Description<="+big, b)
	app, _ := newTestApp(t, t.TempDir())
	_, out, err := app.compareMetadata(context.Background(), nil, CompareMetadataIn{
		LeftPath: a, RightPath: b,
		MetadataFilter:  &MetadataFilter{Mode: "select", IncludeTags: []string{"XMP-dc:Description"}},
		ResponseOptions: &ResponseOptionsInput{MaxResponseBytes: 1024},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Changed) != 1 {
		t.Fatalf("changed = %d records (%+v), want 1", len(out.Changed), out.Changed)
	}
	rec := out.Changed[0]
	if rec.RightValue != nil || rec.RightValueRef == nil {
		t.Fatalf("oversized right value not externalized: value set=%v ref=%v", rec.RightValue != nil, rec.RightValueRef)
	}
	// The reference must resolve to the stored fragment.
	_, chunk, _, err := app.Store.Fragment(rec.RightValueRef.ResultID, 0, 8)
	if err != nil || string(chunk) != "xxxxxxxx" {
		t.Fatalf("fragment read: chunk=%q err=%v", chunk, err)
	}
	// And the response itself must respect the caller's budget
	// (before the fix it carried the whole ~100 KB value).
	raw, _ := json.Marshal(out)
	if len(raw) > 8192 {
		t.Fatalf("compare response = %d bytes with a 1024 budget", len(raw))
	}
}

func TestCompareValueRefEvictedDuringCallFails(t *testing.T) {
	requireExifTool(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jpg")
	b := filepath.Join(dir, "b.jpg")
	writeJPEG(t, a)
	writeJPEG(t, b)
	va := writeTextValue(t, dir, "va.txt", strings.Repeat("a", 40960))
	vb := writeTextValue(t, dir, "vb.txt", strings.Repeat("b", 40960))
	cli(t, "-overwrite_original", "-Comment<="+va, "-XMP-dc:Description<="+va, a)
	cli(t, "-overwrite_original", "-Comment<="+vb, "-XMP-dc:Description<="+vb, b)
	// Two changed records × two oversized sides = four fragments
	// against a quota that fits one: later writes evict earlier
	// references, so the comparison must fail store-full rather
	// than return records with dead references.
	app := smallQuotaApp(t, 45000)
	_, out, err := app.compareMetadata(context.Background(), nil, CompareMetadataIn{LeftPath: a, RightPath: b})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "failed" || len(out.Errors) == 0 || out.Errors[0].Code != "RESULT_STORE_FULL" {
		t.Fatalf("status=%s errors=%v, want failed RESULT_STORE_FULL", out.Status, out.Errors)
	}
}

// stubRunner is a runner.Runner returning canned results.
type stubRunner struct {
	res   runner.Result
	byArg func(args []string) (runner.Result, error)
}

func (s stubRunner) Run(_ context.Context, args ...string) (runner.Result, error) {
	if s.byArg != nil {
		return s.byArg(args)
	}
	return s.res, nil
}
func (s stubRunner) Path() string       { return "fake" }
func (s stubRunner) ConfigMode() string { return "standard" }

func stubApp(t *testing.T, r runner.Runner) *App {
	t.Helper()
	cfg := config.Config{
		StoreDir: t.TempDir(), CallTimeout: 5 * time.Second, MaxConcurrency: 2,
		MaxResponseBytes: 1 << 20, DefaultPageSize: 100, MaxInlineBinary: 1 << 20,
		ResultTTL: time.Hour, StoreQuotaBytes: 1 << 30, ScanBatchFiles: 200, ScanBatchTime: 30 * time.Second,
	}
	st, err := store.New(cfg.StoreDir, cfg.ResultTTL, cfg.StoreQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	return &App{Cfg: cfg, Runner: r, Reader: &meta.Reader{Runner: r}, Store: st}
}

func TestExtractBinaryNonzeroExitFails(t *testing.T) {
	// Partial stdout from a failed extraction is not the tag's
	// data: it must not come back as a complete extraction with
	// a size and checksum.
	app := stubApp(t, stubRunner{res: runner.Result{ExitCode: 1, Stdout: []byte("partial binary payload"), Stderr: "simulated error"}})
	_, out, err := app.extractBinary(context.Background(), nil, ExtractBinaryIn{Path: "/x/a.jpg", Tag: "PreviewImage"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "failed" || len(out.Errors) == 0 || out.Errors[0].Code != "EXTRACTION_FAILED" {
		t.Fatalf("status=%s errors=%v, want failed EXTRACTION_FAILED", out.Status, out.Errors)
	}
}

func TestExtractBinaryAllFormsRejected(t *testing.T) {
	app := stubApp(t, stubRunner{res: runner.Result{ExitCode: 0}})
	for _, tag := range []string{"EXIF:all", "all", "EXIF:All", "exif:ALL"} {
		_, out, err := app.extractBinary(context.Background(), nil, ExtractBinaryIn{Path: "/x/a.jpg", Tag: tag})
		if err != nil {
			t.Fatal(err)
		}
		if out.Status != "failed" || len(out.Errors) == 0 || out.Errors[0].Code != "INVALID_ARGUMENT" {
			t.Fatalf("tag %q: status=%s errors=%v, want failed INVALID_ARGUMENT", tag, out.Status, out.Errors)
		}
	}
}

func TestGetExiftoolInfoCatalogFailureStatus(t *testing.T) {
	r := stubRunner{byArg: func(args []string) (runner.Result, error) {
		for _, a := range args {
			if a == "-ver" {
				return runner.Result{Stdout: []byte("13.55\n")}, nil
			}
		}
		return runner.Result{}, errors.New("catalog unavailable")
	}}
	app := stubApp(t, r)
	res, out, err := app.getExiftoolInfo(context.Background(), nil, ExiftoolInfoIn{IncludeFormats: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "unavailable" || !res.IsError {
		t.Fatalf("status=%q isError=%v, want unavailable + IsError (was: ok + IsError)", out.Status, res.IsError)
	}
}

func TestGetResultPageArgumentErrors(t *testing.T) {
	app, _ := newTestApp(t, t.TempDir())
	_, out, err := app.getResultPage(context.Background(), nil, GetResultPageIn{Cursor: "!!bogus"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Errors) == 0 || out.Errors[0].Code != "INVALID_ARGUMENT" {
		t.Fatalf("missing result_id: errors=%v, want INVALID_ARGUMENT", out.Errors)
	}
	_, out, err = app.getResultPage(context.Background(), nil, GetResultPageIn{ResultID: "abc", Cursor: "!!bogus"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Errors) == 0 || out.Errors[0].Code != "INVALID_ARGUMENT" {
		t.Fatalf("malformed cursor: errors=%v, want INVALID_ARGUMENT (was EXTRACTION_FAILED)", out.Errors)
	}
}

func TestReadSingleRecordOverBudgetFails(t *testing.T) {
	requireExifTool(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "many.jpg")
	writeJPEG(t, path)
	// Many small tags: no single value is oversized, but the one
	// file's record is several KB — over a 1024-byte budget it
	// can never be paged, so the read must fail explicitly
	// rather than return a response over the caller's budget.
	cli(t, "-overwrite_original",
		"-EXIF:Make=Canon", "-EXIF:Model=ProbeCam", "-EXIF:Software=ProbeSoft 1.0",
		"-EXIF:Artist=Probe Artist", "-EXIF:Copyright=Probe Copyright 2026",
		"-EXIF:ImageDescription=A probe image description with some text",
		"-XMP-dc:Creator=Probe Creator", "-XMP-dc:Title=Probe Title",
		"-XMP-dc:Description=Another probe description text here",
		"-XMP-dc:Subject=probe subject", "-XMP-xmp:Label=Red", "-XMP-xmp:Rating=4",
		"-IPTC:Caption-Abstract=Probe caption abstract text", "-IPTC:Keywords=probe",
		"-IPTC:By-line=Probe Byline", "-IPTC:Credit=Probe Credit", "-IPTC:Source=Probe Source",
		"-IPTC:Headline=Probe Headline", "-IPTC:City=Jakarta", "-IPTC:Province-State=DKI",
		"-IPTC:Country-PrimaryLocationName=Indonesia",
		"-EXIF:ExposureTime=1/125", "-EXIF:FNumber=2.8", "-EXIF:ISO=200",
		"-EXIF:FocalLength=35", "-EXIF:DateTimeOriginal=2026:01:01 10:00:00",
		path)
	app, _ := newTestApp(t, t.TempDir())
	_, out, err := app.readMetadata(context.Background(), nil, ReadMetadataIn{
		Paths:           []string{path},
		ResponseOptions: &ResponseOptionsInput{MaxResponseBytes: 1024},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "failed" || len(out.Errors) == 0 || out.Errors[0].Code != "INVALID_ARGUMENT" {
		t.Fatalf("status=%s errors=%v, want failed INVALID_ARGUMENT", out.Status, out.Errors)
	}
}
