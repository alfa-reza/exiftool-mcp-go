package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alfa-reza/exiftool-mcp-go/internal/config"
	"github.com/alfa-reza/exiftool-mcp-go/internal/meta"
	"github.com/alfa-reza/exiftool-mcp-go/internal/query"
	"github.com/alfa-reza/exiftool-mcp-go/internal/runner"
	"github.com/alfa-reza/exiftool-mcp-go/internal/store"
)

// tinyJPEG is a valid 1x1 JPEG used as fixture ground truth.
const tinyJPEG = "/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAP//////////////////////////////////////////////////////////////////////////////////////2wBDAf//////////////////////////////////////////////////////////////////////////////////////wAARCAABAAEDASIAAhEBAxEB/8QAFAABAAAAAAAAAAAAAAAAAAAACf/EABQQAQAAAAAAAAAAAAAAAAAAAAD/xAAUAQEAAAAAAAAAAAAAAAAAAAAA/8QAFBEBAAAAAAAAAAAAAAAAAAAAAP/aAAwDAQACEQMRAD8AvwH/2Q=="

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(tinyJPEG)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func requireExifTool(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("exiftool")
	if err != nil {
		t.Skip("exiftool not installed; skipping integration test")
	}
	return p
}

func cli(t *testing.T, args ...string) {
	t.Helper()
	et := requireExifTool(t)
	cmd := exec.Command(et, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture exiftool %v: %v\n%s", args, err, out)
	}
}

func writeJPEG(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, jpegBytes(t), 0o644); err != nil {
		t.Fatal(err)
	}
}

type countingRunner struct {
	inner runner.Runner
	calls atomic.Int64
}

func (c *countingRunner) Run(ctx context.Context, args ...string) (runner.Result, error) {
	c.calls.Add(1)
	return c.inner.Run(ctx, args...)
}
func (c *countingRunner) Path() string       { return c.inner.Path() }
func (c *countingRunner) ConfigMode() string { return c.inner.ConfigMode() }

func newTestApp(t *testing.T, storeDir string) (*App, *countingRunner) {
	t.Helper()
	cfg := config.Config{
		StoreDir:         storeDir,
		CallTimeout:      60 * time.Second,
		MaxConcurrency:   4,
		MaxResponseBytes: 1 << 20,
		DefaultPageSize:  100,
		MaxInlineBinary:  1 << 20,
		ResultTTL:        time.Hour,
		StoreQuotaBytes:  1 << 30,
		ScanBatchFiles:   200,
		ScanBatchTime:    30 * time.Second,
	}
	cr := &countingRunner{inner: runner.New(cfg)}
	st, err := store.New(storeDir, cfg.ResultTTL, cfg.StoreQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Cfg: cfg, Runner: cr, Reader: &meta.Reader{Runner: cr}, Store: st}
	return app, cr
}

// setupPhotos builds a directory with three tagged JPEGs and one
// corrupt file, returning the directory and file paths.
func setupPhotos(t *testing.T) string {
	t.Helper()
	requireExifTool(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jpg")
	b := filepath.Join(dir, "b.jpg")
	c := filepath.Join(dir, "c.jpg")
	thumb := filepath.Join(dir, "thumb-src.jpg")
	for _, p := range []string{a, b, c, thumb} {
		writeJPEG(t, p)
	}
	cli(t, "-q", "-overwrite_original",
		"-EXIF:Make=Canon", "-EXIF:Model=EOS Test", "-EXIF:ISO=3200",
		"-EXIF:DateTimeOriginal=2023:05:01 10:00:00",
		"-EXIF:CreateDate=2023:05:01 10:00:00", "-XMP:CreateDate=2023:05:01 10:00:00",
		a)
	cli(t, "-q", "-overwrite_original", "-ThumbnailImage<="+thumb, a)
	cli(t, "-q", "-overwrite_original", "-EXIF:Make=Nikon", "-EXIF:ISO=100", b)
	cli(t, "-q", "-overwrite_original", "-EXIF:Make=Canon", "-EXIF:ISO=800", c)
	// A genuinely corrupt file: binary junk no parser accepts (a text
	// file would be read successfully by ExifTool as TXT).
	junk := make([]byte, 300)
	for i := range junk {
		junk[i] = byte((i*37 + 11) % 256)
	}
	if err := os.WriteFile(filepath.Join(dir, "corrupt.jpg"), junk, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(thumb); err != nil {
		t.Fatal(err)
	}
	return dir
}

func findEntry(fr meta.FileResult, name string) *meta.Entry {
	for i := range fr.Entries {
		if fr.Entries[i].Name == name {
			return &fr.Entries[i]
		}
	}
	return nil
}

func TestInfoIntegration(t *testing.T) {
	app, _ := newTestApp(t, t.TempDir())
	_, out, err := app.getExiftoolInfo(context.Background(), nil, ExiftoolInfoIn{Format: "jpg"})
	if err != nil {
		t.Fatal(err)
	}
	et := requireExifTool(t)
	verOut, _ := exec.Command(et, "-ver").Output()
	if out.Status != "ok" || out.Version != strings.TrimSpace(string(verOut)) {
		t.Fatalf("info mismatch: %+v (cli %s)", out, verOut)
	}
	if out.FormatResult != "readable" {
		t.Fatalf("jpg should be readable, got %q", out.FormatResult)
	}
	if out.ConfigMode != "standard" {
		t.Fatalf("expected standard config mode, got %q", out.ConfigMode)
	}
}

func TestReadMetadataIntegration(t *testing.T) {
	dir := setupPhotos(t)
	app, _ := newTestApp(t, t.TempDir())
	ctx := context.Background()
	aPath := filepath.Join(dir, "a.jpg")
	before, _ := os.ReadFile(aPath)
	hashBefore := sha256.Sum256(before)

	_, out, err := app.readMetadata(ctx, nil, ReadMetadataIn{Paths: []string{aPath}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "complete" || len(out.Items) != 1 {
		t.Fatalf("unexpected read out: %+v", out)
	}
	fr := out.Items[0]
	mk := findEntry(fr, "Make")
	if mk == nil || mk.Value != "Canon" || mk.Group != "EXIF:IFD0" {
		t.Fatalf("Make entry wrong: %+v", mk)
	}
	// Duplicate names across groups are separate entries.
	createDates := 0
	for _, e := range fr.Entries {
		if e.Name == "CreateDate" {
			createDates++
		}
	}
	if createDates < 2 {
		t.Fatalf("expected EXIF+XMP CreateDate entries, got %d", createDates)
	}
	after, _ := os.ReadFile(aPath)
	if sha256.Sum256(after) != hashBefore {
		t.Fatal("source file changed after read (read-only violation)")
	}

	// Select filter actually restricts the result.
	_, sel, err := app.readMetadata(ctx, nil, ReadMetadataIn{
		Paths:          []string{aPath},
		MetadataFilter: &MetadataFilter{Mode: "select", IncludeTags: []string{"EXIF:CreateDate"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Items) != 1 {
		t.Fatalf("select: expected 1 item, got %d", len(sel.Items))
	}
	for _, e := range sel.Items[0].Entries {
		if e.Name != "CreateDate" {
			t.Fatalf("select returned unexpected entry %s", e.Key)
		}
	}

	// Mixed batch: one good, one corrupt, one missing.
	_, mixed, err := app.readMetadata(ctx, nil, ReadMetadataIn{
		Paths: []string{aPath, filepath.Join(dir, "corrupt.jpg"), filepath.Join(dir, "missing.jpg")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mixed.Status != "partial" || mixed.ProcessedCount != 2 {
		t.Fatalf("mixed batch: status=%s processed=%d", mixed.Status, mixed.ProcessedCount)
	}
	statusByBase := map[string]string{}
	for _, it := range mixed.Items {
		statusByBase[filepath.Base(it.Path)] = it.Status
	}
	if statusByBase["a.jpg"] != "ok" || statusByBase["corrupt.jpg"] != "error" {
		t.Fatalf("mixed batch statuses: %v", statusByBase)
	}
	foundNotFound := false
	for _, e := range mixed.Errors {
		if e.Code == "PATH_NOT_FOUND" {
			foundNotFound = true
		}
	}
	if !foundNotFound {
		t.Fatalf("expected PATH_NOT_FOUND error, got %+v", mixed.Errors)
	}
}

func TestSearchResumeRestartFrozen(t *testing.T) {
	dir := setupPhotos(t)
	storeDir := t.TempDir()
	ctx := context.Background()
	q := &query.Node{Type: "predicate", Tag: "Make", Operator: "eq", Value: "Canon"}

	app1, _ := newTestApp(t, storeDir)
	_, out1, err := app1.searchMetadata(ctx, nil, SearchMetadataIn{
		Paths:       []string{dir},
		Query:       q,
		ScanOptions: &ScanOptionsInput{MaxFiles: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out1.ScanComplete || out1.ResumeToken == "" {
		t.Fatalf("first batch should be incomplete with a resume token: %+v", out1)
	}
	if out1.TotalCandidates != 4 || out1.ScannedCount != 2 || out1.MatchedCount != 1 {
		t.Fatalf("first batch stats wrong: %+v", out1)
	}
	// A file created after enumeration must not join the frozen scan.
	writeJPEG(t, filepath.Join(dir, "d.jpg"))
	cli(t, "-q", "-overwrite_original", "-EXIF:Make=Canon", filepath.Join(dir, "d.jpg"))

	// Simulate a server restart: brand-new App and Store on the same dir.
	app2, _ := newTestApp(t, storeDir)
	_, out2, err := app2.searchMetadata(ctx, nil, SearchMetadataIn{ResumeToken: out1.ResumeToken})
	if err != nil {
		t.Fatal(err)
	}
	if !out2.ScanComplete {
		t.Fatalf("second batch should complete the scan: %+v", out2)
	}
	if out2.TotalCandidates != 4 || out2.ScannedCount != 4 {
		t.Fatalf("frozen snapshot violated: %+v", out2)
	}
	if out2.MatchedCount != 2 || out2.NoMatchCount != 1 || out2.ErrorCount != 1 {
		t.Fatalf("final stats wrong: matched=%d nomatch=%d errors=%d",
			out2.MatchedCount, out2.NoMatchCount, out2.ErrorCount)
	}
	// Combining resume with a new query is rejected.
	_, bad, err := app2.searchMetadata(ctx, nil, SearchMetadataIn{ResumeToken: out1.ResumeToken, Query: q})
	if err != nil {
		t.Fatal(err)
	}
	if bad.Status != "failed" {
		t.Fatalf("resume+query should fail, got %+v", bad)
	}
	// Full history is readable through the result id.
	_, page, err := app2.getResultPage(ctx, nil, GetResultPageIn{ResultID: out2.ResultID, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 stored match records, got %d", len(page.Items))
	}
}

func TestSearchChangedAndIndeterminate(t *testing.T) {
	requireExifTool(t)
	dir := t.TempDir()
	f1 := filepath.Join(dir, "f1.jpg")
	f2 := filepath.Join(dir, "f2.jpg")
	writeJPEG(t, f1)
	writeJPEG(t, f2)
	cli(t, "-q", "-overwrite_original", "-EXIF:Make=Canon", f1)
	cli(t, "-q", "-overwrite_original", "-EXIF:Make=Canon", f2)
	storeDir := t.TempDir()
	app, _ := newTestApp(t, storeDir)
	ctx := context.Background()
	q := &query.Node{Type: "predicate", Tag: "Make", Operator: "eq", Value: "Canon"}
	_, out1, err := app.searchMetadata(ctx, nil, SearchMetadataIn{
		Paths: []string{dir}, Query: q, ScanOptions: &ScanOptionsInput{MaxFiles: 1},
	})
	if err != nil || out1.ScanComplete {
		t.Fatalf("batch1: %+v err=%v", out1, err)
	}
	// Modify f2 after enumeration.
	cli(t, "-q", "-overwrite_original", "-EXIF:ISO=200", f2)
	_, out2, err := app.searchMetadata(ctx, nil, SearchMetadataIn{ResumeToken: out1.ResumeToken})
	if err != nil {
		t.Fatal(err)
	}
	if !out2.ScanComplete || len(out2.Items) != 1 {
		t.Fatalf("batch2: %+v", out2)
	}
	if !out2.Items[0].ChangedSinceDiscovery {
		t.Fatalf("changed file was not flagged: %+v", out2.Items[0])
	}

	// Indeterminate: naive datetime vs aware bound, no time context.
	dir2 := setupPhotos(t)
	app2, _ := newTestApp(t, t.TempDir())
	dq := &query.Node{Type: "predicate", Tag: "DateTimeOriginal", Operator: "before",
		Value: "2024-01-01T00:00:00+07:00", ValueType: "datetime"}
	_, out3, err := app2.searchMetadata(ctx, nil, SearchMetadataIn{
		Paths: []string{filepath.Join(dir2, "a.jpg")}, Query: dq,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out3.IndeterminateCount != 1 || len(out3.Items) != 1 || out3.Items[0].Status != "indeterminate" {
		t.Fatalf("expected one indeterminate record: %+v", out3)
	}
}

func TestCompareIntegration(t *testing.T) {
	dir := setupPhotos(t)
	app, _ := newTestApp(t, t.TempDir())
	ctx := context.Background()
	a := filepath.Join(dir, "a.jpg")
	mod := filepath.Join(dir, "mod.jpg")
	data, _ := os.ReadFile(a)
	if err := os.WriteFile(mod, data, 0o644); err != nil {
		t.Fatal(err)
	}
	cli(t, "-q", "-overwrite_original", "-EXIF:ISO=6400", "-XMP:Rating=5", mod)
	_, out, err := app.compareMetadata(ctx, nil, CompareMetadataIn{LeftPath: a, RightPath: mod})
	if err != nil {
		t.Fatal(err)
	}
	var isoChanged, ratingAdded bool
	for _, ch := range out.Changed {
		if ch.Name == "ISO" {
			isoChanged = true
		}
	}
	for _, ch := range out.Added {
		if ch.Name == "Rating" {
			ratingAdded = true
		}
	}
	if !isoChanged || !ratingAdded {
		t.Fatalf("compare missed changes: changed=%+v added=%+v", out.Changed, out.Added)
	}
	// Comparing a file with a corrupt one must not fabricate a diff.
	_, bad, err := app.compareMetadata(ctx, nil, CompareMetadataIn{LeftPath: a, RightPath: filepath.Join(dir, "corrupt.jpg")})
	if err != nil {
		t.Fatal(err)
	}
	if bad.Status != "failed" || len(bad.Added)+len(bad.Removed)+len(bad.Changed) != 0 {
		t.Fatalf("expected failed compare with no diff, got %+v", bad)
	}
}

func TestDiscoverIntegration(t *testing.T) {
	dir := setupPhotos(t)
	app, _ := newTestApp(t, t.TempDir())
	ctx := context.Background()
	_, cat, err := app.discoverTags(ctx, nil, DiscoverTagsIn{Scope: "catalog", Query: "CreateDate"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range cat.Items {
		if it.Name == "CreateDate" && it.Source == "catalog" {
			found = true
		}
	}
	if !found || cat.TotalCount == 0 {
		t.Fatalf("catalog search failed: total=%d", cat.TotalCount)
	}
	_, obs, err := app.discoverTags(ctx, nil, DiscoverTagsIn{
		Scope: "present_in_files", Query: "Make", Paths: []string{dir}, Extensions: []string{"jpg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, it := range obs.Items {
		if it.Name == "Make" && it.Source == "observed" {
			if it.Count != 3 {
				t.Fatalf("Make should be observed in 3 files, got %d", it.Count)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("observed Make not found: %+v", obs.Items)
	}
}

func TestExtractBinaryIntegration(t *testing.T) {
	dir := setupPhotos(t)
	app, _ := newTestApp(t, t.TempDir())
	ctx := context.Background()
	a := filepath.Join(dir, "a.jpg")
	res, out, err := app.extractBinary(ctx, nil, ExtractBinaryIn{Path: a, Tag: "ThumbnailImage"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "complete" || out.Delivery != "image" {
		t.Fatalf("thumbnail delivery wrong: %+v", out)
	}
	if out.SHA256 != fmt.Sprintf("%x", sha256.Sum256(jpegBytes(t))) {
		t.Fatalf("thumbnail hash mismatch: %s", out.SHA256)
	}
	img, ok := res.Content[0].(*mcp.ImageContent)
	if !ok {
		t.Fatalf("first content is not image content: %T", res.Content[0])
	}
	sum := sha256.Sum256(img.Data)
	if sum != sha256.Sum256(jpegBytes(t)) {
		t.Fatal("image content bytes differ from fixture thumbnail")
	}
	// Absent binary tag: a structured failure, never an empty success.
	_, missing, err := app.extractBinary(ctx, nil, ExtractBinaryIn{Path: a, Tag: "ICC_Profile"})
	if err != nil {
		t.Fatal(err)
	}
	if missing.Status != "failed" || len(missing.Errors) == 0 || missing.Errors[0].Code != "BINARY_NOT_FOUND" {
		t.Fatalf("expected BINARY_NOT_FOUND, got %+v", missing)
	}
	// Wildcards are rejected.
	_, wild, err := app.extractBinary(ctx, nil, ExtractBinaryIn{Path: a, Tag: "EXIF:All"})
	if err != nil {
		t.Fatal(err)
	}
	if wild.Status != "failed" {
		t.Fatalf("group selector should be rejected, got %+v", wild)
	}
}

func TestPagingDoesNotInvokeExifTool(t *testing.T) {
	dir := setupPhotos(t)
	app, cr := newTestApp(t, t.TempDir())
	ctx := context.Background()
	_, out, err := app.readMetadata(ctx, nil, ReadMetadataIn{
		Paths:           []string{filepath.Join(dir, "a.jpg"), filepath.Join(dir, "b.jpg"), filepath.Join(dir, "c.jpg")},
		ResponseOptions: &ResponseOptionsInput{PageSize: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.HasMore || out.ResultID == "" {
		t.Fatalf("expected paged result: %+v", out)
	}
	callsBefore := cr.calls.Load()
	seen := map[string]bool{}
	seen[filepath.Base(out.Items[0].Path)] = true
	cursor := out.NextCursor
	for cursor != "" {
		_, page, err := app.getResultPage(ctx, nil, GetResultPageIn{ResultID: out.ResultID, Cursor: cursor, PageSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 {
			t.Fatalf("expected 1 record per page, got %d", len(page.Items))
		}
		var rec meta.FileResult
		if err := json.Unmarshal(page.Items[0], &rec); err != nil {
			t.Fatal(err)
		}
		if seen[filepath.Base(rec.Path)] {
			t.Fatalf("duplicate record across pages: %s", rec.Path)
		}
		seen[filepath.Base(rec.Path)] = true
		cursor = page.NextCursor
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 unique files across pages, got %v", seen)
	}
	if cr.calls.Load() != callsBefore {
		t.Fatal("get_result_page invoked exiftool")
	}
}
